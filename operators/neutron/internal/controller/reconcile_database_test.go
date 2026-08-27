// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/job"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	neutronmetrics "github.com/c5c3/cobaltcore/operators/neutron/internal/metrics"
)

// dbConfigMapName stands in for what reconcileConfig hands the database step.
const dbConfigMapName = "neutron-config-abc"

// neutronJobKey addresses one of this CR's migration Jobs by its phase suffix.
func neutronJobKey(neutron *neutronv1alpha1.Neutron, suffix string) client.ObjectKey {
	return client.ObjectKey{Namespace: neutron.Namespace, Name: neutron.Name + "-" + suffix}
}

// getJob reads a migration Job the database step created, failing the test when
// it is absent.
func getJob(t *testing.T, r *NeutronReconciler, neutron *neutronv1alpha1.Neutron, suffix string) *batchv1.Job {
	t.Helper()
	var j batchv1.Job
	if err := r.Get(context.Background(), neutronJobKey(neutron, suffix), &j); err != nil {
		t.Fatalf("reading the %s Job: %v", suffix, err)
	}
	return &j
}

// expectNoJob asserts the named phase Job was never created.
func expectNoJob(t *testing.T, r *NeutronReconciler, neutron *neutronv1alpha1.Neutron, suffix string) {
	t.Helper()
	var j batchv1.Job
	err := r.Get(context.Background(), neutronJobKey(neutron, suffix), &j)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no %s Job, got err=%v", suffix, err)
	}
}

// completedSyncJob returns the db-sync Job the database step builds, marked
// complete and carrying the UID the terminal-metric dedupe keys on. The fake
// client assigns no UID of its own, so a Job the step created itself would be
// counted afresh on every pass and the dedupe would look like it worked.
func completedSyncJob(neutron *neutronv1alpha1.Neutron, configMapName, uid string) *batchv1.Job {
	desired := database.SyncJob(neutronJobSetParams(neutron, configMapName))
	completed := desired.DeepCopy()
	completed.UID = types.UID(uid)
	completed.Annotations = map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template)}
	now := metav1.Now()
	completed.Status.Succeeded = 1
	completed.Status.CompletionTime = &now
	completed.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	return completed
}

// upgradingNeutron returns a Neutron mid-release-bump: 2025.2 is installed, the
// spec requests 2026.1 in both the release and the image tag (the operator's
// bump-in-lockstep contract), and the installed image records the release the
// schema was actually migrated by.
func upgradingNeutron() *neutronv1alpha1.Neutron {
	neutron := validNeutron()
	neutron.Spec.OpenStackRelease = "2026.1"
	neutron.Spec.Image.Tag = "2026.1"
	neutron.Status.InstalledRelease = "2025.2"
	neutron.Status.InstalledImage = "ghcr.io/c5c3/neutron:2025.2"
	return neutron
}

// TestNeutronMaxUserConnections pins the connection-cap arithmetic. The cap is
// what the operator asks mariadb-operator for, and a value below the real
// concurrency does not degrade: the last processes to start fail their pool with
// MySQL error 1226 and crash-loop.
func TestNeutronMaxUserConnections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*neutronv1alpha1.Neutron)
		want    int32
		because string
	}{
		{
			name: "default topology",
			want: 18,
			because: "3 API pods plus one surge run 2 uWSGI processes each, the two worker " +
				"Deployments run 3 replicas plus a surge each, and two Jobs may overlap",
		},
		{
			name: "autoscaling raises the pod ceiling",
			mutate: func(n *neutronv1alpha1.Neutron) {
				n.Spec.Autoscaling = &neutronv1alpha1.AutoscalingSpec{MaxReplicas: 5}
			},
			want:    22,
			because: "an HPA owns the replica count, so the cap is sized for its ceiling rather than for spec.deployment.replicas",
		},
		{
			name: "uWSGI concurrency multiplies the API floor",
			mutate: func(n *neutronv1alpha1.Neutron) {
				n.Spec.APIServer = &neutronv1alpha1.APIServerSpec{
					UWSGI: &commonv1.UWSGISpec{Processes: 4, Threads: 2},
				}
			},
			want:    42,
			because: "every worker thread holds a pooled connection once its app has loaded",
		},
		{
			name:    "worker replicas count twice",
			mutate:  func(n *neutronv1alpha1.Neutron) { n.Spec.Workers.Deployment.Replicas = 1 },
			want:    14,
			because: "the periodic workers and the ovn-maintenance worker each run the configured replica count",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			neutron := validNeutron()
			if tc.mutate != nil {
				tc.mutate(neutron)
			}
			g.Expect(neutronMaxUserConnections(neutron)).To(Equal(tc.want), tc.because)
		})
	}
}

// TestReconcileDatabase_WaitsWithoutARenderedConfig covers the gate: the
// migration Job mounts the config ConfigMap as its whole /etc/neutron, so an
// empty name would render a volume the API server rejects on every pass.
func TestReconcileDatabase_WaitsWithoutARenderedConfig(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron)

	res, err := r.reconcileDatabase(context.Background(), r.Client, neutron, "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := neutronCondition(neutron, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDatabaseWaitingForConfig))
	expectNoJob(t, r, neutron, "db-sync")
}

// TestReconcileDatabase_SyncJobCommandEnvAndMounts pins what the db-sync Job
// runs and what it can reach: both config files, both credential overrides, and
// the OVN client identity the [ovn] section of the mounted ml2_conf.ini names.
func TestReconcileDatabase_SyncJobCommandEnvAndMounts(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron)

	_, err := r.reconcileDatabase(context.Background(), r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	syncJob := getJob(t, r, neutron, "db-sync")
	container := syncJob.Spec.Template.Spec.Containers[0]
	g.Expect(container.Command).To(Equal([]string{
		"neutron-db-manage",
		"--config-file", "/etc/neutron/neutron.conf",
		"--config-file", "/etc/neutron/ml2_conf.ini",
		"upgrade", "head",
	}))

	envNames := make(map[string]string, len(container.Env))
	for _, env := range container.Env {
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			envNames[env.Name] = env.ValueFrom.SecretKeyRef.Name
		}
	}
	g.Expect(envNames).To(HaveKeyWithValue(database.ConnectionEnvVarName, database.ConnectionSecretName(neutron.Name)))
	g.Expect(envNames).To(HaveKeyWithValue(messaging.TransportURLEnvVarName, messaging.TransportURLSecretName(neutron.Name)))

	g.Expect(container.VolumeMounts).To(ContainElement(corev1.VolumeMount{
		Name: ovnTLSVolumeName, MountPath: ovnTLSMountPath, ReadOnly: true,
	}))
	var volumeNames []string
	for _, vol := range syncJob.Spec.Template.Spec.Volumes {
		volumeNames = append(volumeNames, vol.Name)
	}
	g.Expect(volumeNames).To(ConsistOf(configVolumeName, ovnTLSVolumeName),
		"a Job without database TLS mounts the config and the OVN identity, nothing else")

	// The Job carries no schema-check step: neutron-db-manage upgrade head is
	// idempotent, so a read-only second Job would assert nothing new.
	expectNoJob(t, r, neutron, "schema-check")
}

// TestReconcileDatabase_SyncJobProjectsDBTLSKeypair covers the conditional
// mount: the DSN in the derived Secret names ssl_ca/ssl_cert/ssl_key paths under
// dbTLSMountPath, so without the keypair every migration fails on an unopenable
// file.
func TestReconcileDatabase_SyncJobProjectsDBTLSKeypair(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "neutron-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "neutron-db-client"},
	}
	r := newNeutronTestReconciler(neutron)

	_, err := r.reconcileDatabase(context.Background(), r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	syncJob := getJob(t, r, neutron, "db-sync")
	tlsVol, tlsMount := neutronDBTLSVolumeAndMount(neutron)
	g.Expect(syncJob.Spec.Template.Spec.Volumes).To(ContainElement(tlsVol))
	g.Expect(syncJob.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(tlsMount))
}

// TestReconcileDatabase_FailedJobIsAHardError covers the failure path: a
// permanently failed db-sync surfaces as DBSyncFailed plus a returned error, so
// the controller backs off instead of reporting a schema it never migrated.
func TestReconcileDatabase_FailedJobIsAHardError(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron)

	// First pass creates the Job; the fake client then marks it permanently failed.
	_, err := r.reconcileDatabase(context.Background(), r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	failed := getJob(t, r, neutron, "db-sync")
	failed.Status.Failed = 5
	failed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	g.Expect(r.Status().Update(context.Background(), failed)).To(Succeed())

	_, err = r.reconcileDatabase(context.Background(), r.Client, neutron, dbConfigMapName)

	g.Expect(err).To(HaveOccurred())
	cond := neutronCondition(neutron, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncFailed))
	g.Expect(neutron.Status.InstalledRelease).To(BeEmpty(),
		"a failed migration must not promote the installed-release marker")
}

// TestReconcileDatabase_ImageReleaseMismatchBlocks pins the decoupled-field
// contract: the migration Jobs run spec.image while release tracking keys on
// spec.openStackRelease, so a tag-pinned image naming another release would
// promote installedRelease to a release the pods never migrated to. It requeues
// rather than erroring — the fields disagree, which an edit fixes, not a retry.
func TestReconcileDatabase_ImageReleaseMismatchBlocks(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.OpenStackRelease = "2026.1"
	neutron.Spec.Image.Tag = "2025.2"
	neutron.Status.InstalledRelease = "2025.2"
	r := newNeutronTestReconciler(neutron)

	res, err := r.reconcileDatabase(context.Background(), r.Client, neutron, dbConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(neutron.Status.UpgradePhase).To(BeEmpty())
	g.Expect(neutron.Status.TargetRelease).To(BeEmpty())
	cond := neutronCondition(neutron, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonImageReleaseMismatch))
	expectNoJob(t, r, neutron, "db-sync")
	expectNoJob(t, r, neutron, "db-expand")
}

// TestReconcileDatabase_RejectedTransitions covers the release gate. Every case
// must refuse before a Job exists: once a phase Job has run, the schema has
// already moved.
func TestReconcileDatabase_RejectedTransitions(t *testing.T) {
	cases := []struct {
		name string
		// mutate turns the shared fixture into the rejected transition.
		mutate func(*neutronv1alpha1.Neutron)
		reason string
		// wantCause is the substring the returned error must carry, so the message
		// names the offending value rather than swallowing it.
		wantCause string
	}{
		{
			name: "downgrade",
			mutate: func(n *neutronv1alpha1.Neutron) {
				// Release and image tag name 2025.2 in lockstep, so the mismatch guard
				// passes and the downgrade is what the gate rejects.
				n.Spec.OpenStackRelease = "2025.2"
				n.Spec.Image.Tag = "2025.2"
				n.Status.InstalledRelease = "2026.1"
			},
			reason:    database.ReasonDowngradeNotSupported,
			wantCause: "downgrade",
		},
		{
			name: "non-sequential jump",
			mutate: func(n *neutronv1alpha1.Neutron) {
				n.Spec.OpenStackRelease = "2026.1"
				n.Spec.Image.Tag = "2026.1"
				n.Status.InstalledRelease = "2025.1" // skips 2025.2
			},
			reason:    database.ReasonUpgradePathInvalid,
			wantCause: "sequential",
		},
		{
			name: "unparseable installed release",
			mutate: func(n *neutronv1alpha1.Neutron) {
				n.Status.InstalledRelease = "not-a-release"
			},
			reason:    database.ReasonVersionParseError,
			wantCause: `invalid release format "not-a-release"`,
		},
		{
			name: "release bump leaving the image untouched",
			mutate: func(n *neutronv1alpha1.Neutron) {
				// Digest-pinned: nothing in the pod template changes, so the shared flow
				// would short-circuit on the completed Job and promote off a run of the
				// previous release's binary.
				n.Spec.Image.Tag = ""
				n.Spec.Image.Digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
				n.Spec.OpenStackRelease = "2026.1"
				n.Status.InstalledRelease = "2025.2"
				n.Status.InstalledImage = n.Spec.Image.Reference()
			},
			reason:    conditionReasonImageReleaseMismatch,
			wantCause: "leaves spec.image unchanged",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			neutron := validNeutron()
			tc.mutate(neutron)
			r := newNeutronTestReconciler(neutron)

			_, err := r.reconcileDatabase(context.Background(), r.Client, neutron, dbConfigMapName)

			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantCause))
			g.Expect(neutron.Status.TargetRelease).To(BeEmpty())
			g.Expect(neutron.Status.UpgradePhase).To(BeEmpty())
			cond := neutronCondition(neutron, "DatabaseReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.reason))
			recorder, ok := r.Recorder.(*record.FakeRecorder)
			g.Expect(ok).To(BeTrue())
			g.Expect(collectEvents(recorder)).To(ContainElement(ContainSubstring("Warning " + tc.reason)))
			expectNoJob(t, r, neutron, "db-sync")
			expectNoJob(t, r, neutron, "db-expand")
		})
	}
}

// TestReconcileDatabase_InstalledMarkersPromotedOnSuccess covers the epilogue: a
// completed db-sync promotes the release marker, clears the in-flight target,
// and records the image that migrated the schema, which is what lets the release
// gate tell a real bump from a spec-only edit.
func TestReconcileDatabase_InstalledMarkersPromotedOnSuccess(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron)

	_, err := r.reconcileDatabase(context.Background(), r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(simulators.SimulateJobComplete(context.Background(), r.Client, neutronJobKey(neutron, "db-sync"))).To(Succeed())

	res, err := r.reconcileDatabase(context.Background(), r.Client, neutron, dbConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(neutron.Status.InstalledRelease).To(Equal("2026.1"))
	g.Expect(neutron.Status.TargetRelease).To(BeEmpty())
	g.Expect(neutron.Status.InstalledImage).To(Equal("ghcr.io/c5c3/neutron:2026.1"))
	cond := neutronCondition(neutron, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))
}

// TestReconcileDatabase_DBSyncMetricEmittedOncePerUID pins the dedupe: a
// completed Job stays completed, so every later pass observes the same terminal
// state and the counter must not track the number of passes.
func TestReconcileDatabase_DBSyncMetricEmittedOncePerUID(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(neutronmetrics.Register()).To(Succeed())

	neutron := validNeutron()
	neutron.Name = "db-sync-metric"
	t.Cleanup(func() { neutronmetrics.DeleteForNeutron(neutron.Name, neutron.Namespace) })
	r := newNeutronTestReconciler(neutron, completedSyncJob(neutron, dbConfigMapName, "db-sync-metric-uid"))

	// The counter carries the terminal result, the duration histogram does not.
	counterLabels := map[string]string{
		"neutron": neutron.Name, "namespace": neutron.Namespace, "result": "succeeded",
	}
	durationLabels := map[string]string{"neutron": neutron.Name, "namespace": neutron.Namespace}
	for range 3 {
		_, err := r.reconcileDatabase(context.Background(), r.Client, neutron, dbConfigMapName)
		g.Expect(err).NotTo(HaveOccurred())
	}

	g.Expect(neutron.Annotations).To(HaveKey(dbJobUIDAnnotationKey("db-sync")),
		"the terminal metric must stamp the dedupe annotation so the run is counted once")
	g.Expect(counterValue(t, "neutron_operator_db_sync_total", counterLabels)).To(Equal(1.0))
	g.Expect(histogramSampleCount(t, "neutron_operator_db_sync_duration_seconds", durationLabels)).To(Equal(uint64(1)))
}

// TestReconcileDatabase_UpgradeWalk drives a 2025.2 → 2026.1 bump through every
// phase the shared flow runs, asserting the command each phase Job carries. The
// migrate phase is the neutron-specific one: neutron has no data-migration
// command, so the phase runs neutron-db-manage current, which prints the
// revision each branch sits at and exits 0.
//
// The RollingUpdate → Contracting flip is the deployment step's, so the walk
// stops at RollingUpdate, asserts no contract Job exists yet, and enters the
// contract phase the way that step would.
func TestReconcileDatabase_UpgradeWalk(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	neutron := upgradingNeutron()
	r := newNeutronTestReconciler(neutron)

	// Pass 1: the bump is detected and the flow is initiated.
	res, err := r.reconcileDatabase(ctx, r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}))
	g.Expect(neutron.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseExpanding))
	g.Expect(neutron.Status.TargetRelease).To(Equal("2026.1"))

	// Pass 2: the expand Job is created and the phase waits on it.
	res, err = r.reconcileDatabase(ctx, r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueUpgradeWait))
	g.Expect(getJob(t, r, neutron, "db-expand").Spec.Template.Spec.Containers[0].Command).To(Equal([]string{
		"neutron-db-manage",
		"--config-file", "/etc/neutron/neutron.conf",
		"--config-file", "/etc/neutron/ml2_conf.ini",
		"upgrade", "--expand",
	}))
	g.Expect(neutronCondition(neutron, "DatabaseReady").Reason).To(Equal(database.ReasonExpandInProgress))

	// Pass 3: the completed expand Job advances the phase.
	g.Expect(simulators.SimulateJobComplete(ctx, r.Client, neutronJobKey(neutron, "db-expand"))).To(Succeed())
	_, err = r.reconcileDatabase(ctx, r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(neutron.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseMigrating))

	// Pass 4: the migrate Job reports the revision rather than migrating data.
	_, err = r.reconcileDatabase(ctx, r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getJob(t, r, neutron, "db-migrate").Spec.Template.Spec.Containers[0].Command).To(Equal([]string{
		"neutron-db-manage",
		"--config-file", "/etc/neutron/neutron.conf",
		"--config-file", "/etc/neutron/ml2_conf.ini",
		"current",
	}))

	// Pass 5: the completed migrate Job hands over to the Deployment rollout.
	g.Expect(simulators.SimulateJobComplete(ctx, r.Client, neutronJobKey(neutron, "db-migrate"))).To(Succeed())
	_, err = r.reconcileDatabase(ctx, r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(neutron.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseRollingUpdate))
	g.Expect(neutronCondition(neutron, "DatabaseReady").Reason).To(Equal(database.ReasonUpgradeRollingUpdate))

	// Pass 6: the schema is not contracted while the old image may still serve.
	_, err = r.reconcileDatabase(ctx, r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	expectNoJob(t, r, neutron, "db-contract")
	g.Expect(neutron.Status.InstalledRelease).To(Equal("2025.2"))

	// The deployment step flips the phase once the rollout has drained the old
	// image (covered by the deployment test); from there the contract Job runs.
	neutron.Status.UpgradePhase = commonv1.UpgradePhaseContracting
	_, err = r.reconcileDatabase(ctx, r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getJob(t, r, neutron, "db-contract").Spec.Template.Spec.Containers[0].Command).To(Equal([]string{
		"neutron-db-manage",
		"--config-file", "/etc/neutron/neutron.conf",
		"--config-file", "/etc/neutron/ml2_conf.ini",
		"upgrade", "--contract",
	}))

	// The completed contract Job promotes the release and clears the upgrade.
	g.Expect(simulators.SimulateJobComplete(ctx, r.Client, neutronJobKey(neutron, "db-contract"))).To(Succeed())
	res, err = r.reconcileDatabase(ctx, r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(neutron.Status.InstalledRelease).To(Equal("2026.1"))
	g.Expect(neutron.Status.TargetRelease).To(BeEmpty())
	g.Expect(neutron.Status.UpgradePhase).To(BeEmpty())
	cond := neutronCondition(neutron, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))
}

// TestReconcileDatabase_UpgradePhaseFailureIsAHardError covers the phase-failure
// path: a permanently failed expand Job stops the walk with the shared reason
// and a Warning event, so the migrate phase never runs against a half-expanded
// schema.
func TestReconcileDatabase_UpgradePhaseFailureIsAHardError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	neutron := upgradingNeutron()
	neutron.Status.TargetRelease = "2026.1"
	neutron.Status.UpgradePhase = commonv1.UpgradePhaseExpanding
	r := newNeutronTestReconciler(neutron)

	_, err := r.reconcileDatabase(ctx, r.Client, neutron, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	failed := getJob(t, r, neutron, "db-expand")
	failed.Status.Failed = 5
	failed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	g.Expect(r.Status().Update(ctx, failed)).To(Succeed())

	_, err = r.reconcileDatabase(ctx, r.Client, neutron, dbConfigMapName)

	g.Expect(err).To(HaveOccurred())
	cond := neutronCondition(neutron, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonExpandFailed))
	g.Expect(neutron.Status.InstalledRelease).To(Equal("2025.2"))
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())
	g.Expect(collectEvents(recorder)).To(ContainElement(ContainSubstring("Warning ExpandFailed")))
}
