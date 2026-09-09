// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/job"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// testConfigMapName stands in for the rendered config ConfigMap the config step
// hands the database step.
const testConfigMapName = "cinder-config-abc12345"

// upgradingCinder returns a Cinder mid-upgrade: the installed release is 2025.2,
// the spec requests 2026.1 (both the OpenStack release and the image tag, per the
// operator's bump-in-lockstep contract), and the given phase is active.
func upgradingCinder(phase commonv1.UpgradePhase) *cinderv1alpha1.Cinder {
	cinder := validCinder()
	cinder.Spec.OpenStackRelease = "2026.1"
	cinder.Spec.Image.Tag = "2026.1"
	cinder.Status.InstalledRelease = "2025.2"
	cinder.Status.TargetRelease = "2026.1"
	cinder.Status.UpgradePhase = phase
	return cinder
}

// completedJob marks a copy of desired complete, with the pod-spec hash the
// runner compares against and a stable UID for the terminal-state dedupe.
func completedJob(desired *batchv1.Job, uid string) *batchv1.Job {
	now := metav1.Now()
	j := desired.DeepCopy()
	j.UID = types.UID(uid)
	j.Annotations = map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template)}
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	return j
}

// TestCinderMaxUserConnections pins the connection-cap arithmetic. The cap is
// what the operator asks mariadb-operator for, and a process that finds it
// exhausted does not degrade: it fails its pool with MySQL error 1226 and
// crash-loops, so every term is spelled out here.
func TestCinderMaxUserConnections(t *testing.T) {
	cases := []struct {
		name              string
		mutate            func(*cinderv1alpha1.Cinder)
		volumeDeployments int32
		want              int32
		because           string
	}{
		{
			name: "the default topology",
			want: 22,
			because: "4 surge-inclusive API pods × 2 processes × 1 thread × 2 connections, " +
				"plus 2 each for the scheduler and the backup service, plus 2 for the Jobs",
		},
		{
			name:              "one cinder-volume per attached backend",
			volumeDeployments: 2,
			want:              26,
			because:           "each volume Deployment adds its own pair of connections",
		},
		{
			name:    "an HPA owns the replica count",
			mutate:  func(c *cinderv1alpha1.Cinder) { c.Spec.Autoscaling = &cinderv1alpha1.AutoscalingSpec{MaxReplicas: 10} },
			want:    50,
			because: "the cap has to cover the ceiling the HPA may scale to, not today's replica count",
		},
		{
			name:    "raised scheduler replicas",
			mutate:  func(c *cinderv1alpha1.Cinder) { c.Spec.Scheduler.Deployment.Replicas = 3 },
			want:    26,
			because: "schedulers are peers, and each one holds its own pair",
		},
		{
			name: "a wider uWSGI topology",
			mutate: func(c *cinderv1alpha1.Cinder) {
				c.Spec.API.UWSGI = &cinderv1alpha1.UWSGISpec{Processes: 4, Threads: 2}
			},
			want:    70,
			because: "the API floor scales with processes × threads, which is where the cap is spent",
		},
		{
			name:    "a CR that bypassed the defaulting webhook",
			mutate:  func(c *cinderv1alpha1.Cinder) { c.Spec.API.Deployment.Replicas = 0 },
			want:    22,
			because: "an unset replica count resolves to the shared default, never to zero pods",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cinder := validCinder()
			if tc.mutate != nil {
				tc.mutate(cinder)
			}
			g.Expect(cinderMaxUserConnections(cinder, tc.volumeDeployments)).To(Equal(tc.want), tc.because)
		})
	}
}

// TestReconcileDatabase_NoConfigWaits covers the pass before the config step has
// rendered anything: the migration Jobs mount that ConfigMap as their whole
// config directory, so an empty name must wait instead of creating a Job the API
// server refuses.
func TestReconcileDatabase_NoConfigWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder)

	res, err := r.reconcileDatabase(context.Background(), r.Client, cinder, "", 0)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := cinderCondition(cinder, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDatabaseWaitingForConfig))

	var jobs batchv1.JobList
	g.Expect(r.List(context.Background(), &jobs)).To(Succeed())
	g.Expect(jobs.Items).To(BeEmpty(), "no Job may be built against an empty config mount")
}

// TestCheckImageReleaseMismatch covers the decoupled-field contract between
// spec.openStackRelease and spec.image: they drive different things, and a
// disagreement would promote an installed-release marker for a release the pods
// do not run.
func TestCheckImageReleaseMismatch(t *testing.T) {
	cases := []struct {
		name    string
		tag     string
		release string
		blocked bool
		because string
	}{
		{name: "in lockstep", tag: "2026.1", release: "2026.1", blocked: false},
		{
			name: "a patched image build", tag: "2026.1-p1", release: "2026.1", blocked: false,
			because: "the patch suffix names a rebuild of the same release",
		},
		{
			name: "a digest-pinned image", tag: "", release: "2026.1", blocked: false,
			because: "a digest carries no release string, so the explicit declaration stands",
		},
		{
			name: "an unparseable tag", tag: "latest", release: "2026.1", blocked: false,
			because: "nothing comparable, so release tracking is left to spec.openStackRelease",
		},
		{
			name: "a lagging image", tag: "2025.2", release: "2026.1", blocked: true,
			because: "the migration Jobs would run the old cinder-manage against the new schema",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cinder := validCinder()
			cinder.Spec.Image.Tag = tc.tag
			cinder.Spec.OpenStackRelease = tc.release

			res, blocked := checkImageReleaseMismatch(cinder)

			g.Expect(blocked).To(Equal(tc.blocked), tc.because)
			if !tc.blocked {
				g.Expect(res.IsZero()).To(BeTrue())
				g.Expect(cinderCondition(cinder, "DatabaseReady")).To(BeNil())
				return
			}
			g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
			cond := cinderCondition(cinder, "DatabaseReady")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonImageReleaseMismatch))
			g.Expect(cond.Message).To(ContainSubstring("bump spec.image in lockstep"))
		})
	}
}

// TestReconcileDatabase_SyncJobCommandAndEnv pins the steady-state Job: the
// cinder-manage command, the config mount every migration reads its database
// coordinates from, and the env overrides that keep the credentials out of the
// rendered file.
func TestReconcileDatabase_SyncJobCommandAndEnv(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder)

	_, err := r.reconcileDatabase(context.Background(), r.Client, cinder, testConfigMapName, 0)
	g.Expect(err).NotTo(HaveOccurred())

	var syncJob batchv1.Job
	key := client.ObjectKey{Namespace: testNamespace, Name: "cinder-db-sync"}
	g.Expect(r.Get(context.Background(), key, &syncJob)).To(Succeed())

	container := syncJob.Spec.Template.Spec.Containers[0]
	g.Expect(container.Command).To(Equal([]string{
		"cinder-manage", "--config-dir", "/etc/cinder/cinder.conf.d", "db", "sync",
	}))
	g.Expect(container.Image).To(Equal(cinder.Spec.Image.Reference()))

	var mountPaths []string
	for _, mount := range container.VolumeMounts {
		mountPaths = append(mountPaths, mount.MountPath)
	}
	g.Expect(mountPaths).To(ContainElement(cinderConfigDir))

	envNames := make([]string, 0, len(container.Env))
	for _, env := range container.Env {
		envNames = append(envNames, env.Name)
	}
	g.Expect(envNames).To(ContainElements(
		database.ConnectionEnvVarName,
		"OS_DEFAULT__TRANSPORT_URL",
		"OS_KEYSTONE_AUTHTOKEN__PASSWORD",
		"OS_SERVICE_USER__PASSWORD",
	))
}

// TestCinderWorkloadEnv_KeystoneFree covers the deployment without an identity
// service: the two password overrides have no section to override, so they are
// not rendered and no Secret reference is left pointing at nothing.
func TestCinderWorkloadEnv_KeystoneFree(t *testing.T) {
	g := NewGomegaWithT(t)

	env := cinderWorkloadEnv(keystoneFreeCinder())

	names := make([]string, 0, len(env))
	for _, e := range env {
		names = append(names, e.Name)
	}
	g.Expect(names).To(ConsistOf(database.ConnectionEnvVarName, "OS_DEFAULT__TRANSPORT_URL"))
}

// TestCinderJobSetParams_DatabaseTLS covers the encrypted database connection:
// the DSN names ssl_ca/ssl_cert/ssl_key paths under the mount point, so without
// the projected keypair cinder-manage cannot open them and every migration
// fails.
func TestCinderJobSetParams_DatabaseTLS(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
	}

	params := cinderJobSetParams(cinder, testConfigMapName)

	g.Expect(params.ExtraVolumes).To(HaveLen(1))
	g.Expect(params.ExtraVolumes[0].Name).To(Equal(dbTLSVolumeName))
	g.Expect(params.ExtraVolumeMounts).To(HaveLen(1))
	g.Expect(params.ExtraVolumeMounts[0].MountPath).To(Equal(dbTLSMountPath))

	// A disabled block projects nothing, so a Cinder that keeps the certificate
	// references while turning verification off does not mount them.
	cinder.Spec.Database.TLS.Mode = "disabled"
	g.Expect(cinderJobSetParams(cinder, testConfigMapName).ExtraVolumes).To(BeEmpty())
}

// TestBuildPhaseJob covers the four phases of the upgrade walk. Cinder has no
// expand/contract verbs of its own: the expand phase re-runs the idempotent
// sync, the migrate phase runs the readiness check, and the contract phase runs
// the online data migrations the rolling update before it made safe.
func TestBuildPhaseJob(t *testing.T) {
	cases := []struct {
		phase   commonv1.UpgradePhase
		name    string
		command []string
	}{
		{
			phase:   commonv1.UpgradePhaseExpanding,
			name:    "cinder-db-expand",
			command: []string{"cinder-manage", "--config-dir", "/etc/cinder/cinder.conf.d", "db", "sync"},
		},
		{
			phase:   commonv1.UpgradePhaseMigrating,
			name:    "cinder-db-migrate",
			command: []string{"/bin/sh", "-eu", "-c", upgradeCheckScript},
		},
		{
			phase: commonv1.UpgradePhaseContracting,
			name:  "cinder-db-contract",
			command: []string{
				"cinder-manage", "--config-dir", "/etc/cinder/cinder.conf.d", "db", "online_data_migrations",
			},
		},
	}
	cinder := upgradingCinder(commonv1.UpgradePhaseExpanding)
	r := newCinderTestReconciler(cinder)
	build := r.upgradeFlowParams(context.Background(), r.Client, cinder, testConfigMapName).BuildPhaseJob

	for _, tc := range cases {
		t.Run(string(tc.phase), func(t *testing.T) {
			g := NewGomegaWithT(t)
			phaseJob := build(tc.phase)

			g.Expect(phaseJob).NotTo(BeNil())
			g.Expect(phaseJob.Name).To(Equal(tc.name))
			g.Expect(*phaseJob.Spec.BackoffLimit).To(Equal(upgradePhaseJobBackoffLimit))

			container := phaseJob.Spec.Template.Spec.Containers[0]
			g.Expect(container.Command).To(Equal(tc.command))
			g.Expect(container.Image).To(Equal(cinder.Spec.Image.Reference()),
				"every phase runs the target release's image")
			g.Expect(container.Env).To(Equal(cinderWorkloadEnv(cinder)))

			var mountPaths []string
			for _, mount := range container.VolumeMounts {
				mountPaths = append(mountPaths, mount.MountPath)
			}
			g.Expect(mountPaths).To(ContainElement(cinderConfigDir))
		})
	}

	t.Run("no Job outside the three migration phases", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(build(commonv1.UpgradePhaseRollingUpdate)).To(BeNil(),
			"the Deployment rollout drives the rolling update, not a Job")
		g.Expect(build("")).To(BeNil())
	})
}

// TestReconcileDatabase_InstalledReleasePromotedOnSuccess covers the steady-state
// completion: the marker moves to spec.openStackRelease and the terminal metric
// stamps its per-phase dedupe annotation.
func TestReconcileDatabase_InstalledReleasePromotedOnSuccess(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder() // InstalledRelease empty (fresh), OpenStackRelease 2026.1
	completed := completedJob(database.SyncJob(cinderJobSetParams(cinder, testConfigMapName)), "sync-job-uid")
	r := newCinderTestReconciler(cinder, completed)

	res, err := r.reconcileDatabase(context.Background(), r.Client, cinder, testConfigMapName, 0)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(cinder.Status.InstalledRelease).To(Equal("2026.1"))
	cond := cinderCondition(cinder, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))
	g.Expect(cinder.Annotations).To(HaveKey(dbJobUIDAnnotationKey("db-sync")))
}

// TestReconcileDatabase_UpgradeWalk covers the release bump: the initiation off
// the steady-state path, and the migrate phase completing into the rolling
// update the contract phase waits for.
func TestReconcileDatabase_UpgradeWalk(t *testing.T) {
	t.Run("a release bump initiates the upgrade", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := validCinder()
		cinder.Status.InstalledRelease = "2025.2"
		r := newCinderTestReconciler(cinder)

		_, err := r.reconcileDatabase(context.Background(), r.Client, cinder, testConfigMapName, 0)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cinder.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseExpanding))
		g.Expect(cinder.Status.TargetRelease).To(Equal("2026.1"))
	})

	t.Run("the migrate phase completes into the rolling update", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := upgradingCinder(commonv1.UpgradePhaseMigrating)
		params := cinderJobSetParams(cinder, testConfigMapName)
		desired := database.BuildJob(params, cinder.Spec.Image.Reference(), upgradeMigrateJobSuffix,
			[]string{"/bin/sh", "-eu", "-c", upgradeCheckScript}, upgradePhaseJobBackoffLimit)
		r := newCinderTestReconciler(cinder, completedJob(desired, "migrate-job-uid"))

		res, err := r.reconcileDatabase(context.Background(), r.Client, cinder, testConfigMapName, 0)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}))
		g.Expect(cinder.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseRollingUpdate))
		// Both terminal reporters ran: the shared metric and the check report.
		g.Expect(cinder.Annotations).To(HaveKey(dbJobUIDAnnotationKey(upgradeMigrateJobSuffix)))
		g.Expect(cinder.Annotations).To(HaveKey(dbJobUIDAnnotationKey(upgradeMigrateJobSuffix + "-check")))
	})

	// Mid-upgrade the image and the release must stay in lockstep too: a lone
	// image edit would dispatch phase Jobs built from the wrong binary.
	t.Run("a mid-upgrade image drift blocks", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := upgradingCinder(commonv1.UpgradePhaseExpanding)
		cinder.Spec.Image.Tag = "2025.2"
		r := newCinderTestReconciler(cinder)

		res, err := r.reconcileDatabase(context.Background(), r.Client, cinder, testConfigMapName, 0)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
		g.Expect(cinderCondition(cinder, "DatabaseReady").Reason).To(Equal(conditionReasonImageReleaseMismatch))
		g.Expect(cinder.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseExpanding),
			"the phase is frozen until the image is bumped in lockstep")
	})
}

// --- the cinder-status upgrade check report ---

// terminatedCheckPod returns the migrate Job's pod with the given termination
// message on its only container.
func terminatedCheckPod(jobName, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-pod",
			Namespace: testNamespace,
			Labels:    map[string]string{"batch.kubernetes.io/job-name": jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: upgradeMigrateJobSuffix,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Message: message},
				},
			}},
		},
	}
}

// terminalMigrateJob returns a completed migrate Job with the given UID, which
// is what the per-Job dedupe keys the report on.
func terminalMigrateJob(uid string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testCinderName + "-" + upgradeMigrateJobSuffix,
			Namespace: testNamespace,
			UID:       types.UID(uid),
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
		},
	}
}

// TestReportUpgradeCheck covers what the operator makes of the exit code the
// migrate Job deliberately swallows: a clean run is Normal, warnings and a
// failed check are Warnings carrying the message, and a pod whose message cannot
// be read says so rather than leaving a silent gap.
func TestReportUpgradeCheck(t *testing.T) {
	cases := []struct {
		name      string
		message   string
		eventType string
		reason    string
		expect    string
	}{
		{
			name: "a clean check", message: "cinder-status upgrade check exit 0",
			eventType: corev1.EventTypeNormal, reason: "UpgradeCheckCompleted",
			expect: "cinder-status upgrade check exit 0",
		},
		{
			name: "warnings", message: "cinder-status upgrade check exit 1",
			eventType: corev1.EventTypeWarning, reason: "UpgradeCheckWarnings",
			expect: "cinder-status upgrade check exit 1",
		},
		{
			name: "a failed check", message: "cinder-status upgrade check exit 2",
			eventType: corev1.EventTypeWarning, reason: "UpgradeCheckWarnings",
			expect: "cinder-status upgrade check exit 2",
		},
		{
			name: "a pod that wrote no message", message: "",
			eventType: corev1.EventTypeNormal, reason: "UpgradeCheckCompleted",
			expect: "exit code unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cinder := validCinder()
			observed := terminalMigrateJob("migrate-uid-" + tc.name)
			r := newCinderTestReconciler(cinder, terminatedCheckPod(observed.Name, tc.message))

			r.reportUpgradeCheck(context.Background(), r.Client, cinder, observed)

			events := collectEvents(r.Recorder.(*record.FakeRecorder))
			g.Expect(events).To(ConsistOf(ContainSubstring(tc.eventType + " " + tc.reason)))
			g.Expect(events[0]).To(ContainSubstring(tc.expect))
		})
	}

	t.Run("no pod at all", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := validCinder()
		r := newCinderTestReconciler(cinder)

		r.reportUpgradeCheck(context.Background(), r.Client, cinder, terminalMigrateJob("migrate-uid-nopod"))

		g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ConsistOf(And(
			ContainSubstring("Normal UpgradeCheckCompleted"),
			ContainSubstring("exit code unavailable"),
		)))
	})

	// The message is diagnostic, so a broken pod read reports it as unavailable
	// rather than failing a reconcile that is otherwise done.
	t.Run("a failing pod List is reported, not returned", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := validCinder()
		c := cinderFakeClientBuilder(cinder).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.PodList); ok {
						return apierrors.NewInternalError(errors.New("etcd is unavailable"))
					}
					return cl.List(ctx, list, opts...)
				},
			}).Build()
		r := &CinderReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

		r.reportUpgradeCheck(context.Background(), r.Client, cinder, terminalMigrateJob("migrate-uid-listerr"))

		g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ConsistOf(And(
			ContainSubstring("Normal UpgradeCheckCompleted"),
			ContainSubstring("exit code unavailable"),
		)))
	})

	// The dedupe is what keeps a requeue loop from re-reporting the same Job.
	t.Run("one event per Job UID", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := validCinder()
		observed := terminalMigrateJob("migrate-uid-dedupe")
		r := newCinderTestReconciler(cinder, terminatedCheckPod(observed.Name, "cinder-status upgrade check exit 0"))

		r.reportUpgradeCheck(context.Background(), r.Client, cinder, observed)
		r.reportUpgradeCheck(context.Background(), r.Client, cinder, observed)

		g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(HaveLen(1))
	})
}
