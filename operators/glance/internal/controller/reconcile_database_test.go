// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/job"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

// dbTestScheme adds the MariaDB API to the shared test scheme so the managed
// provisioning path can construct MariaDB CRs.
func dbTestScheme() *runtime.Scheme {
	s := testScheme()
	_ = mariadbv1alpha1.AddToScheme(s)
	return s
}

// newDBTestReconciler builds a GlanceReconciler over a fake client seeded with
// objs, using the MariaDB-aware scheme.
func newDBTestReconciler(s *runtime.Scheme, objs ...client.Object) *GlanceReconciler {
	return &GlanceReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
			WithStatusSubresource(&glancev1alpha1.Glance{}, &glancev1alpha1.GlanceBackend{}).Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(50),
	}
}

// managedGlance returns a Glance in managed database mode (ClusterRef set).
func managedGlance() *glancev1alpha1.Glance {
	glance := testGlance()
	glance.Spec.Database = commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "glance",
		SecretRef:  commonv1.SecretRefSpec{Name: "glance-db"},
	}
	return glance
}

// upgradingGlance returns a Glance mid-upgrade: the installed release is 2025.2,
// the spec requests 2026.1 (both the OpenStack release and the image tag, per the
// operator's bump-in-lockstep contract), and the given phase is active.
func upgradingGlance(phase commonv1.UpgradePhase) *glancev1alpha1.Glance {
	glance := testGlance()
	glance.Spec.OpenStackRelease = "2026.1"
	glance.Spec.Image.Tag = "2026.1"
	glance.Status.InstalledRelease = "2025.2"
	glance.Status.TargetRelease = "2026.1"
	glance.Status.UpgradePhase = phase
	return glance
}

// glanceUpgradeJob builds the expand-migrate-contract phase Job the operator's
// upgradeFlowParams.BuildPhaseJob produces for the given db verb ("expand",
// "migrate", "contract"), so seeded Jobs share the pod-spec hash the runner
// compares against.
func glanceUpgradeJob(glance *glancev1alpha1.Glance, configMapName, verb string) *batchv1.Job {
	cmd := []string{"glance-manage", "--config-dir", glanceConfigDir, "db", verb}
	return database.BuildJob(glanceJobSetParams(glance, configMapName), glance.Spec.Image.Reference(), "db-"+verb, cmd, 4)
}

// completedGlanceUpgradeJob returns a phase Job marked complete with the matching
// pod-spec hash and a stable UID for terminal-metric dedupe.
func completedGlanceUpgradeJob(glance *glancev1alpha1.Glance, configMapName, verb string) *batchv1.Job {
	desired := glanceUpgradeJob(glance, configMapName, verb)
	now := metav1.Now()
	j := desired.DeepCopy()
	j.UID = types.UID(glance.Name + "-db-" + verb + "-complete-uid")
	j.Annotations = map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template)}
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	return j
}

// failedGlanceUpgradeJob returns a permanently-failed phase Job with the matching
// pod-spec hash and a stable UID for terminal-metric dedupe.
func failedGlanceUpgradeJob(glance *glancev1alpha1.Glance, configMapName, verb string) *batchv1.Job {
	desired := glanceUpgradeJob(glance, configMapName, verb)
	j := desired.DeepCopy()
	j.UID = types.UID(glance.Name + "-db-" + verb + "-failed-uid")
	j.Annotations = map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template)}
	j.Status.Failed = 5
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
	}
	return j
}

// readyMariaDBCluster returns the MariaDB cluster managedGlance references,
// reporting Ready so the provisioning flow passes its cluster gate.
func readyMariaDBCluster() *mariadbv1alpha1.MariaDB {
	cluster := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: "default"},
	}
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Running",
	})
	return cluster
}

// readyGlanceDatabase returns the Database CR the provisioning flow applies for
// the shared fixture, reporting Ready so the flow reaches the User step.
func readyGlanceDatabase() *mariadbv1alpha1.Database {
	db := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "test-glance", Namespace: "default"},
	}
	meta.SetStatusCondition(&db.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created",
	})
	return db
}

// failingUserApplyReconciler builds a reconciler whose server-side apply of the
// shared fixture's User CR fails with boom, so the wrapping of the error can be
// asserted. The provisioning flow writes through Apply, not Create.
func failingUserApplyReconciler(boom error, objs ...client.Object) *GlanceReconciler {
	s := dbTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&glancev1alpha1.Glance{}, &glancev1alpha1.GlanceBackend{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				if co, ok := obj.(client.Object); ok &&
					co.GetObjectKind().GroupVersionKind().Kind == "User" && co.GetName() == "test-glance" {
					return boom
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).
		Build()
	return &GlanceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(50)}
}

// atRelease moves glance to the given OpenStack release, bumping the image tag
// in lockstep as the operator's decoupled-field contract requires.
func atRelease(glance *glancev1alpha1.Glance, openStackRelease string) {
	glance.Spec.OpenStackRelease = openStackRelease
	glance.Spec.Image.Tag = openStackRelease
}

// TestGlanceReleaseUsesUWSGI pins the launch-mode boundary on a bare release
// string: uWSGI from 2026.1 onward, the eventlet glance-api server below it and
// for a release that does not parse. The connection-cap sizing asks it about
// the installed and target releases, which can differ from the spec release
// mid-upgrade.
func TestGlanceReleaseUsesUWSGI(t *testing.T) {
	cases := []struct {
		release string
		want    bool
	}{
		{release: "2026.1", want: true},
		{release: "2026.2", want: true},
		{release: "2027.1", want: true},
		{release: "2025.2", want: false},
		{release: "", want: false},
		{release: "garbage", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.release, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(glanceReleaseUsesUWSGI(tc.release)).To(Equal(tc.want))
		})
	}
}

// TestGlanceMaxUserConnections pins the connection-cap arithmetic in both launch
// modes and across a mixed fleet. The cap is what the operator asks
// mariadb-operator for, and a value below the real concurrency does not
// degrade: the process that opens the connection past it gets MySQL error 1226
// and the upload, import or snapshot it serves answers 500.
func TestGlanceMaxUserConnections(t *testing.T) {
	apiServer := func(uwsgi *glancev1alpha1.UWSGISpec, workers *int32) *glancev1alpha1.APIServerSpec {
		return &glancev1alpha1.APIServerSpec{UWSGI: uwsgi, Workers: workers}
	}
	cases := []struct {
		name    string
		mutate  func(*glancev1alpha1.Glance)
		want    int32
		because string
	}{
		// uWSGI (2026.1): perPod = processes × (threads + 5).
		{
			name:    "uwsgi default topology",
			want:    50,
			because: "3 API pods plus one surge run 2 processes of 1 thread plus the import pool, and two Jobs may overlap",
		},
		{
			name: "uwsgi autoscaling raises the pod ceiling",
			mutate: func(gl *glancev1alpha1.Glance) {
				gl.Spec.Autoscaling = &glancev1alpha1.AutoscalingSpec{MaxReplicas: 5}
			},
			want:    74,
			because: "an HPA owns the replica count, so the cap is sized for its ceiling",
		},
		{
			name: "uwsgi processes multiply threads plus the import pool",
			mutate: func(gl *glancev1alpha1.Glance) {
				gl.Spec.APIServer = apiServer(&glancev1alpha1.UWSGISpec{Processes: 4, Threads: 2}, nil)
			},
			want:    114,
			because: "(3+1)*4*(2+5)+2",
		},
		{
			name:    "uwsgi single replica",
			mutate:  func(gl *glancev1alpha1.Glance) { gl.Spec.Deployment.Replicas = 1 },
			want:    26,
			because: "the surge pod doubles a single-replica fleet during a rollout",
		},
		// Eventlet (2025.2): perPod = workers × 5.
		{
			name:    "eventlet default topology",
			mutate:  func(gl *glancev1alpha1.Glance) { atRelease(gl, "2025.2") },
			want:    42,
			because: "2 pinned eventlet workers hold 5 connections each",
		},
		{
			name: "eventlet workers multiply the pool size",
			mutate: func(gl *glancev1alpha1.Glance) {
				atRelease(gl, "2025.2")
				gl.Spec.APIServer = apiServer(nil, ptr.To(int32(4)))
			},
			want:    82,
			because: "(3+1)*4*5+2",
		},
		{
			name: "eventlet raised replica count",
			mutate: func(gl *glancev1alpha1.Glance) {
				atRelease(gl, "2025.2")
				gl.Spec.Deployment.Replicas = 5
			},
			want:    62,
			because: "(5+1)*2*5+2",
		},
		{
			name: "eventlet autoscaling raises the pod ceiling",
			mutate: func(gl *glancev1alpha1.Glance) {
				atRelease(gl, "2025.2")
				gl.Spec.Autoscaling = &glancev1alpha1.AutoscalingSpec{MaxReplicas: 5}
			},
			want:    62,
			because: "an HPA owns the replica count, so the cap is sized for its ceiling",
		},
		// Each mode ignores the other mode's knob.
		{
			name: "eventlet ignores the inert uwsgi block",
			mutate: func(gl *glancev1alpha1.Glance) {
				atRelease(gl, "2025.2")
				gl.Spec.APIServer = apiServer(&glancev1alpha1.UWSGISpec{Processes: 8}, nil)
			},
			want:    42,
			because: "the eventlet server runs [DEFAULT] workers, not uWSGI processes",
		},
		{
			name: "uwsgi ignores the inert workers field",
			mutate: func(gl *glancev1alpha1.Glance) {
				gl.Spec.APIServer = apiServer(nil, ptr.To(int32(8)))
			},
			want:    50,
			because: "uWSGI runs its own processes and ignores [DEFAULT] workers",
		},
		// Mixed fleets during an upgrade or its abort.
		{
			name: "installed eventlet fleet outweighs the uwsgi target",
			mutate: func(gl *glancev1alpha1.Glance) {
				gl.Spec.APIServer = apiServer(nil, ptr.To(int32(8)))
				gl.Status.InstalledRelease = "2025.2"
				gl.Status.TargetRelease = "2026.1"
			},
			want:    162,
			because: "the old eventlet pods of 8 workers hold 40 each until the RollingUpdate phase replaces them",
		},
		{
			name: "mixed fleet at defaults sizes for uwsgi",
			mutate: func(gl *glancev1alpha1.Glance) {
				gl.Status.InstalledRelease = "2025.2"
				gl.Status.TargetRelease = "2026.1"
			},
			want:    50,
			because: "a default uWSGI pod (12) holds more than a default eventlet pod (10)",
		},
		{
			name: "uwsgi target outweighs the eventlet spec",
			mutate: func(gl *glancev1alpha1.Glance) {
				atRelease(gl, "2025.2")
				gl.Spec.APIServer = apiServer(&glancev1alpha1.UWSGISpec{Processes: 4}, nil)
				gl.Status.InstalledRelease = "2025.2"
				gl.Status.TargetRelease = "2026.1"
			},
			want:    98,
			because: "an abort leaves uWSGI pods of 4 processes (24 each) beside the eventlet ones",
		},
		// Zero inputs fall back to the defaults.
		{
			name:    "zero replicas fall back to the default",
			mutate:  func(gl *glancev1alpha1.Glance) { gl.Spec.Deployment.Replicas = 0 },
			want:    50,
			because: "an unset replica count is the default of 3, never a fleet of zero",
		},
		{
			name: "zero workers fall back to the default",
			mutate: func(gl *glancev1alpha1.Glance) {
				atRelease(gl, "2025.2")
				gl.Spec.APIServer = apiServer(nil, ptr.To(int32(0)))
			},
			want:    42,
			because: "a worker count below 1 sizes as DefaultEventletWorkers",
		},
		{
			name: "zero uwsgi counts fall back to the defaults",
			mutate: func(gl *glancev1alpha1.Glance) {
				gl.Spec.APIServer = apiServer(&glancev1alpha1.UWSGISpec{Processes: 0, Threads: 0}, nil)
			},
			want:    50,
			because: "the command renders 2 processes and 1 thread for non-positive counts, and the cap follows it",
		},
		// Absent inputs fall back to the defaults.
		{
			name:    "uwsgi nil apiServer",
			mutate:  func(gl *glancev1alpha1.Glance) { gl.Spec.APIServer = nil },
			want:    50,
			because: "an absent apiServer block runs the default 2 processes of 1 thread",
		},
		{
			name: "eventlet nil apiServer",
			mutate: func(gl *glancev1alpha1.Glance) {
				atRelease(gl, "2025.2")
				gl.Spec.APIServer = nil
			},
			want:    42,
			because: "an absent apiServer block runs DefaultEventletWorkers",
		},
		{
			name: "empty status releases size from the spec release alone",
			mutate: func(gl *glancev1alpha1.Glance) {
				gl.Spec.APIServer = apiServer(nil, ptr.To(int32(8)))
				gl.Status.InstalledRelease = ""
				gl.Status.TargetRelease = ""
			},
			want:    50,
			because: "a fresh install has no old pods, so the inert workers count must not size the cap",
		},
		// Releases that do not parse.
		{
			name:    "unparseable installed release",
			mutate:  func(gl *glancev1alpha1.Glance) { gl.Status.InstalledRelease = "garbage" },
			want:    50,
			because: "garbage sizes as eventlet (10 per pod), which loses to uWSGI's 12",
		},
		{
			name:    "empty spec release sizes as eventlet",
			mutate:  func(gl *glancev1alpha1.Glance) { gl.Spec.OpenStackRelease = "" },
			want:    42,
			because: "an empty release launches the eventlet server, so the cap never collapses to the Job headroom",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			glance := testGlance()
			if tc.mutate != nil {
				tc.mutate(glance)
			}
			g.Expect(glanceMaxUserConnections(glance)).To(Equal(tc.want), tc.because)
		})
	}
}

// TestReconcileDatabase_SizesTheUserConnectionCap verifies that the User CR the
// provisioning flow creates carries the cap sized for the launch mode of the
// CR's release. Left unset, the mariadb-operator CRD default of 10 applies,
// which the default fleet exceeds under load in either mode.
func TestReconcileDatabase_SizesTheUserConnectionCap(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*glancev1alpha1.Glance)
		want   int32
	}{
		{name: "uwsgi at 2026.1", want: 50},
		{
			name:   "eventlet at 2025.2",
			mutate: func(gl *glancev1alpha1.Glance) { atRelease(gl, "2025.2") },
			want:   42,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			glance := managedGlance()
			if tc.mutate != nil {
				tc.mutate(glance)
			}
			r := newDBTestReconciler(dbTestScheme(), glance, readyMariaDBCluster(), readyGlanceDatabase())

			_, err := r.reconcileDatabase(context.Background(), r.Client, glance, "test-glance-config-abc")
			g.Expect(err).NotTo(HaveOccurred())

			user := &mariadbv1alpha1.User{}
			g.Expect(r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-glance"}, user)).To(Succeed())
			g.Expect(user.Spec.MaxUserConnections).To(Equal(tc.want))
		})
	}
}

// TestReconcileDatabase_UserApplyErrorPropagates verifies that a failed apply of
// the sized User CR surfaces as a reconcile error wrapping the cause, and that
// no migration Job runs against a user whose cap was never written.
func TestReconcileDatabase_UserApplyErrorPropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := managedGlance()
	boom := errors.New("boom")
	r := failingUserApplyReconciler(boom, glance, readyMariaDBCluster(), readyGlanceDatabase())

	_, err := r.reconcileDatabase(context.Background(), r.Client, glance, "test-glance-config-abc")

	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, boom)).To(BeTrue(), "the apply error must stay in the chain")
	g.Expect(err.Error()).To(ContainSubstring("ensuring database user"))

	var syncJob batchv1.Job
	getErr := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-glance-db-sync"}, &syncJob)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "no migration Job may run on this path")
}

// TestReconcileDatabase_NoUserOutsideStaticManaged verifies that the operator
// sizes no User where it owns none: a brownfield database is not the
// operator's to provision, and in Dynamic credentials mode the OpenBao engine
// issues the users.
func TestReconcileDatabase_NoUserOutsideStaticManaged(t *testing.T) {
	cases := []struct {
		name   string
		glance func() *glancev1alpha1.Glance
	}{
		{name: "brownfield", glance: testGlance},
		{
			name: "dynamic credentials",
			glance: func() *glancev1alpha1.Glance {
				gl := managedGlance()
				gl.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
				return gl
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			glance := tc.glance()
			r := newDBTestReconciler(dbTestScheme(), glance, readyMariaDBCluster(), readyGlanceDatabase())

			_, err := r.reconcileDatabase(context.Background(), r.Client, glance, "test-glance-config-abc")
			g.Expect(err).NotTo(HaveOccurred())

			users := &mariadbv1alpha1.UserList{}
			g.Expect(r.List(context.Background(), users, client.InNamespace("default"))).To(Succeed())
			g.Expect(users.Items).To(BeEmpty())
		})
	}
}

func TestReconcileDatabase_ProvisionGatesOnClusterReady(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := managedGlance()
	// The referenced MariaDB cluster does not exist yet, so provisioning gates.
	r := newDBTestReconciler(dbTestScheme(), glance)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, "test-glance-config-abc")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonClusterNotReady))
}

func TestReconcileDatabase_NoConfigWaitsForBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance() // brownfield, OpenStackRelease 2026.1
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	// TargetRelease is owned by the shared upgrade flow now (set on initiate,
	// cleared on completion/abort); the steady-state path no longer stamps it.
	g.Expect(glance.Status.TargetRelease).To(BeEmpty())
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDatabaseWaitingForBackends))
}

func TestReconcileDatabase_DowngradeRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	// Both the release and the image tag name 2025.2 (bump-in-lockstep contract)
	// so the image/release-mismatch guard passes and the downgrade path is what is
	// exercised.
	glance.Spec.OpenStackRelease = "2025.2"
	glance.Spec.Image.Tag = "2025.2"
	glance.Status.InstalledRelease = "2026.1"
	r := newGlanceTestReconciler(glance)

	_, err := r.reconcileDatabase(context.Background(), r.Client, glance, "test-glance-config-abc")

	// A downgrade now surfaces through the shared vocabulary as a returned error
	// (controller backoff), exactly like keystone; the rejection precedes target
	// stamping so TargetRelease stays empty.
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("downgrade"))
	g.Expect(glance.Status.TargetRelease).To(BeEmpty())
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDowngradeNotSupported))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Warning DowngradeNotSupported")))
}

func TestReconcileDatabase_NonSequentialJumpRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.OpenStackRelease = "2026.1"
	glance.Status.InstalledRelease = "2025.1" // skips 2025.2
	r := newGlanceTestReconciler(glance)

	_, err := r.reconcileDatabase(context.Background(), r.Client, glance, "test-glance-config-abc")

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("sequential"))
	g.Expect(glance.Status.TargetRelease).To(BeEmpty())
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(database.ReasonUpgradePathInvalid))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Warning UpgradePathInvalid")))
}

func TestReconcileDatabase_PatchOnlyAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.OpenStackRelease = "2026.1-p1" // patch of the installed release
	glance.Status.InstalledRelease = "2026.1"
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, "test-glance-config-abc")

	g.Expect(err).NotTo(HaveOccurred())
	// Accepted: a patch-only change stays on the steady-state path (a fresh
	// db-sync Job is in progress) rather than entering the upgrade flow.
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(glance.Status.UpgradePhase).To(BeEmpty())
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress))
}

func TestReconcileDatabase_SequentialUpgradeAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.OpenStackRelease = "2026.1"
	glance.Status.InstalledRelease = "2025.2"
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, "test-glance-config-abc")

	g.Expect(err).NotTo(HaveOccurred())
	// A sequential upgrade initiates the shared flow: TargetRelease and the
	// Expanding phase are stamped and the reconcile requeues immediately.
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}))
	g.Expect(glance.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseExpanding))
	g.Expect(glance.Status.TargetRelease).To(Equal("2026.1"))
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonExpandInProgress))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Normal UpgradeInitiated")))
}

// TestReconcileDatabase_ImageReleaseMismatchBlocks verifies that a tag-pinned
// image whose tag names a different OpenStack release than spec.openStackRelease
// blocks the database pass instead of initiating a no-op upgrade that would
// falsely promote the installed-release marker. Regression guard for the
// decoupled spec.image / spec.openStackRelease contract: the migration Jobs and
// the Deployment run spec.image, but release tracking and the launch mode key on
// spec.openStackRelease, so the two must name the same release.
func TestReconcileDatabase_ImageReleaseMismatchBlocks(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	// The operator bumped the release but left the image tag on the old release.
	glance.Spec.OpenStackRelease = "2026.1"
	glance.Spec.Image.Tag = "2025.2"
	glance.Status.InstalledRelease = "2025.2"
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, "test-glance-config-abc")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	// The upgrade flow is never entered: no phase is stamped, no target is
	// recorded, and the installed-release marker stays at the installed release.
	g.Expect(glance.Status.UpgradePhase).To(BeEmpty())
	g.Expect(glance.Status.TargetRelease).To(BeEmpty())
	g.Expect(glance.Status.InstalledRelease).To(Equal("2025.2"))
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonImageReleaseMismatch))

	// No phase Job (nor a db-sync Job) is created while the fields disagree.
	var expandJob batchv1.Job
	getErr := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-glance-db-expand"}, &expandJob)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue())
}

// TestReconcileDatabase_ImagePatchBuildAccepted verifies that a patched image
// build (tag 2026.1-p1) still matches the base release 2026.1 and passes the
// mismatch guard: the patch suffix is ignored, so the reconcile advances to the
// steady-state db-sync path rather than blocking.
func TestReconcileDatabase_ImagePatchBuildAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance() // OpenStackRelease 2026.1, InstalledRelease empty (fresh)
	glance.Spec.Image.Tag = "2026.1-p1"
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, "test-glance-config-abc")

	g.Expect(err).NotTo(HaveOccurred())
	// Not blocked: the fresh install runs the steady-state db-sync (a Job is in
	// progress), and the DatabaseReady reason is the sync reason, not the mismatch.
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(glance.Status.UpgradePhase).To(BeEmpty())
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress))
}

func TestReconcileDatabase_SyncJobCommandAndEnv(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	r := newGlanceTestReconciler(glance)

	_, err := r.reconcileDatabase(context.Background(), r.Client, glance, "test-glance-config-abc")
	g.Expect(err).NotTo(HaveOccurred())

	var syncJob batchv1.Job
	key := client.ObjectKey{Namespace: "default", Name: "test-glance-db-sync"}
	g.Expect(r.Get(context.Background(), key, &syncJob)).To(Succeed())

	container := syncJob.Spec.Template.Spec.Containers[0]
	g.Expect(container.Command).To(Equal([]string{
		"/bin/sh", "-eu", "-c",
		"glance-manage --config-dir /etc/glance/glance-api.conf.d/ db sync && " +
			"glance-manage --config-dir /etc/glance/glance-api.conf.d/ db load_metadefs " +
			"--path /var/lib/openstack/etc/glance/metadefs",
	}))

	var connEnv *corev1.EnvVar
	for i := range container.Env {
		if container.Env[i].Name == database.ConnectionEnvVarName {
			connEnv = &container.Env[i]
		}
	}
	g.Expect(connEnv).NotTo(BeNil(), "the db-sync Job overrides [database].connection via env")
	g.Expect(connEnv.ValueFrom.SecretKeyRef.Name).To(Equal(database.ConnectionSecretName(glance.Name)))
}

func TestReconcileDatabase_InstalledReleasePromotedOnSuccess(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance() // InstalledRelease empty (fresh), OpenStackRelease 2026.1
	configMapName := "test-glance-config-abc"

	// Seed a completed db-sync Job matching the desired pod-spec hash so
	// ReconcileSyncJobs observes it as done and promotes InstalledRelease.
	desired := database.SyncJob(glanceJobSetParams(glance, configMapName))
	now := metav1.Now()
	completed := desired.DeepCopy()
	completed.UID = "sync-job-uid"
	completed.Annotations = map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template)}
	completed.Status.Succeeded = 1
	completed.Status.CompletionTime = &now
	completed.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	r := newGlanceTestReconciler(glance, completed)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(glance.Status.InstalledRelease).To(Equal("2026.1"),
		"InstalledRelease is promoted to spec.openStackRelease on db-sync success")
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))

	// The db-sync terminal metric dedupe annotation is stamped on the CR so the
	// metric emits at most once per Job UID.
	g.Expect(glance.Annotations).To(HaveKey(dbJobUIDAnnotationKey("db-sync")))
}

// --- Upgrade phase walk ---

// TestReconcileDatabase_UpgradeExpand_CreatesJob verifies that the first pass of
// the Expanding phase creates the db-expand Job with the spec image, the
// glance-manage db expand command, and the backoffLimit-4 parity with keystone.
func TestReconcileDatabase_UpgradeExpand_CreatesJob(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := upgradingGlance(commonv1.UpgradePhaseExpanding)
	configMapName := "test-glance-config-abc"
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueUpgradeWait))

	var expandJob batchv1.Job
	key := client.ObjectKey{Namespace: "default", Name: "test-glance-db-expand"}
	g.Expect(r.Get(context.Background(), key, &expandJob)).To(Succeed())

	container := expandJob.Spec.Template.Spec.Containers[0]
	g.Expect(container.Image).To(Equal(glance.Spec.Image.Reference()))
	g.Expect(container.Command).To(Equal([]string{
		"glance-manage", "--config-dir", glanceConfigDir, "db", "expand",
	}))
	g.Expect(expandJob.Spec.BackoffLimit).NotTo(BeNil())
	g.Expect(*expandJob.Spec.BackoffLimit).To(Equal(int32(4)))

	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonExpandInProgress))
}

// TestReconcileDatabase_UpgradeExpandComplete_TransitionsToMigrating verifies the
// Expanding → Migrating transition once the seeded expand Job is complete.
func TestReconcileDatabase_UpgradeExpandComplete_TransitionsToMigrating(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := upgradingGlance(commonv1.UpgradePhaseExpanding)
	configMapName := "test-glance-config-abc"
	r := newGlanceTestReconciler(glance, completedGlanceUpgradeJob(glance, configMapName, "expand"))

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}))
	g.Expect(glance.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseMigrating))

	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonMigrateInProgress))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Normal ExpandComplete")))
}

// TestReconcileDatabase_UpgradeMigrateComplete_TransitionsToRollingUpdate verifies
// the Migrating → RollingUpdate transition once the seeded migrate Job is
// complete.
func TestReconcileDatabase_UpgradeMigrateComplete_TransitionsToRollingUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := upgradingGlance(commonv1.UpgradePhaseMigrating)
	configMapName := "test-glance-config-abc"
	r := newGlanceTestReconciler(glance, completedGlanceUpgradeJob(glance, configMapName, "migrate"))

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}))
	g.Expect(glance.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseRollingUpdate))

	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonUpgradeRollingUpdate))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Normal MigrateComplete")))
}

// TestReconcileDatabase_UpgradeContractComplete_CompletesUpgrade verifies the
// contract Job's completion promotes InstalledRelease, clears the upgrade state,
// and reports DatabaseReady=True.
func TestReconcileDatabase_UpgradeContractComplete_CompletesUpgrade(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := upgradingGlance(commonv1.UpgradePhaseContracting)
	configMapName := "test-glance-config-abc"
	r := newGlanceTestReconciler(glance, completedGlanceUpgradeJob(glance, configMapName, "contract"))

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	g.Expect(glance.Status.InstalledRelease).To(Equal("2026.1"))
	g.Expect(glance.Status.TargetRelease).To(BeEmpty())
	g.Expect(glance.Status.UpgradePhase).To(BeEmpty())

	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Normal UpgradeComplete")))
}

// TestReconcileDatabase_UpgradeExpandFailed_ReturnsError verifies a permanently
// failed expand Job surfaces as a reconcile error with ExpandFailed and a Warning
// event.
func TestReconcileDatabase_UpgradeExpandFailed_ReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := upgradingGlance(commonv1.UpgradePhaseExpanding)
	configMapName := "test-glance-config-abc"
	r := newGlanceTestReconciler(glance, failedGlanceUpgradeJob(glance, configMapName, "expand"))

	_, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
	g.Expect(err).To(HaveOccurred())

	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonExpandFailed))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Warning ExpandFailed")))
}

// TestReconcileDatabase_UpgradeAbort_RevertToInstalled verifies that reverting
// spec.openStackRelease (and the image tag) to the installed release aborts the
// upgrade: the phase Jobs are deleted, phase/target are cleared, an UpgradeAborted
// event fires, and a follow-up pass drops into the steady-state db-sync path.
func TestReconcileDatabase_UpgradeAbort_RevertToInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := upgradingGlance(commonv1.UpgradePhaseExpanding)
	// Revert both the release and the image tag to the installed release.
	glance.Spec.OpenStackRelease = "2025.2"
	glance.Spec.Image.Tag = "2025.2"
	configMapName := "test-glance-config-abc"

	// Seed all three phase Jobs so the abort has something to delete.
	r := newGlanceTestReconciler(
		glance,
		glanceUpgradeJob(glance, configMapName, "expand"),
		glanceUpgradeJob(glance, configMapName, "migrate"),
		glanceUpgradeJob(glance, configMapName, "contract"),
	)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}))

	g.Expect(glance.Status.UpgradePhase).To(BeEmpty())
	g.Expect(glance.Status.TargetRelease).To(BeEmpty())
	g.Expect(glance.Status.InstalledRelease).To(Equal("2025.2"))

	for _, suffix := range []string{"db-expand", "db-migrate", "db-contract"} {
		var jb batchv1.Job
		getErr := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-glance-" + suffix}, &jb)
		g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "%s Job must be absent after abort", suffix)
	}
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Normal UpgradeAborted")))

	// The next pass runs the steady-state sync: the db-sync Job is created and
	// DatabaseReady flips to DBSyncInProgress.
	res, err = r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	var syncJob batchv1.Job
	g.Expect(r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-glance-db-sync"}, &syncJob)).To(Succeed())
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress))
}

// TestReconcileDatabase_UpgradeTargetChanged_Blocks verifies that changing
// spec.openStackRelease to a third value mid-upgrade blocks with
// UpgradeTargetChanged and leaves the upgrade state untouched.
func TestReconcileDatabase_UpgradeTargetChanged_Blocks(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := upgradingGlance(commonv1.UpgradePhaseExpanding) // target 2026.1
	// Someone flips the release to a third value mid-upgrade.
	glance.Spec.OpenStackRelease = "2026.2"
	glance.Spec.Image.Tag = "2026.2"
	configMapName := "test-glance-config-abc"
	r := newGlanceTestReconciler(glance)

	_, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec release changed during active upgrade"))

	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonUpgradeTargetChanged))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Warning UpgradeTargetChanged")))

	// Upgrade state is untouched.
	g.Expect(glance.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseExpanding))
	g.Expect(glance.Status.TargetRelease).To(Equal("2026.1"))
	g.Expect(glance.Status.InstalledRelease).To(Equal("2025.2"))
}

// TestReconcileDatabase_MidUpgradeImageDriftBlocks verifies that editing
// spec.image.tag alone to an inconsistent release DURING an active upgrade blocks
// with ImageReleaseMismatch and dispatches no phase Job. The shared flow's
// target-changed guard only watches spec.openStackRelease, so without the
// mid-upgrade consistency check a lone image-tag drift would build phase Jobs from
// the wrong image and re-render the Deployment in a launch mode the image cannot
// run. Regression guard for the decoupled spec.image / spec.openStackRelease
// contract while a phase is in flight (keystone is immune: its release IS the tag).
func TestReconcileDatabase_MidUpgradeImageDriftBlocks(t *testing.T) {
	g := NewGomegaWithT(t)
	// Mid-upgrade to 2026.1 (openStackRelease and targetRelease agree), but the
	// image tag is drifted to a third release — an edit the shared flow's
	// target-changed guard (which keys on spec.openStackRelease) would not catch.
	glance := upgradingGlance(commonv1.UpgradePhaseExpanding)
	glance.Spec.Image.Tag = "2027.1"
	configMapName := "test-glance-config-abc"
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))

	// Blocked on the mismatch; the upgrade state is untouched and no phase Job runs.
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonImageReleaseMismatch))
	g.Expect(glance.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseExpanding))
	g.Expect(glance.Status.TargetRelease).To(Equal("2026.1"))

	var expandJob batchv1.Job
	getErr := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-glance-db-expand"}, &expandJob)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "no phase Job may be dispatched while the image drifts")
}

// TestReconcileDatabase_AbortReachableDuringImageDrift verifies the abort escape
// hatch stays reachable mid-upgrade even while spec.image.tag disagrees with the
// installed release: reverting spec.openStackRelease to the installed release must
// abort the upgrade (clearing the phase) rather than wedging on the mismatch guard.
func TestReconcileDatabase_AbortReachableDuringImageDrift(t *testing.T) {
	g := NewGomegaWithT(t)
	// Revert openStackRelease to the installed 2025.2 to abort, but the image tag
	// still lags on the aborted target — the mismatch guard must not block the abort.
	glance := upgradingGlance(commonv1.UpgradePhaseExpanding)
	glance.Spec.OpenStackRelease = "2025.2"
	glance.Spec.Image.Tag = "2026.1"
	configMapName := "test-glance-config-abc"
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}))

	// The upgrade was aborted, not blocked on the mismatch.
	g.Expect(glance.Status.UpgradePhase).To(BeEmpty())
	g.Expect(glance.Status.TargetRelease).To(BeEmpty())
	g.Expect(glance.Status.InstalledRelease).To(Equal("2025.2"))
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	if cond != nil {
		g.Expect(cond.Reason).NotTo(Equal(conditionReasonImageReleaseMismatch),
			"a revert-to-installed abort must not be blocked by the image mismatch guard")
	}
}

// TestUpgradePhaseFailureRecordsDBSyncMetric verifies that each
// expand-migrate-contract phase Job's terminal metric flows through
// recordDBJobTerminalState under its own suffix, stamping the per-phase
// dbJobUIDAnnotationKey dedupe annotation on the Glance CR while surfacing the
// failure as a reconcile error.
func TestUpgradePhaseFailureRecordsDBSyncMetric(t *testing.T) {
	cases := []struct {
		phase  commonv1.UpgradePhase
		verb   string
		suffix string
	}{
		{commonv1.UpgradePhaseExpanding, "expand", "db-expand"},
		{commonv1.UpgradePhaseMigrating, "migrate", "db-migrate"},
		{commonv1.UpgradePhaseContracting, "contract", "db-contract"},
	}
	for _, tc := range cases {
		t.Run(tc.suffix, func(t *testing.T) {
			g := NewGomegaWithT(t)
			glance := upgradingGlance(tc.phase)
			configMapName := "test-glance-config-abc"
			r := newGlanceTestReconciler(glance, failedGlanceUpgradeJob(glance, configMapName, tc.verb))

			_, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
			g.Expect(err).To(HaveOccurred(),
				"the failing upgrade-phase Job MUST surface as a reconcile error so callers retry")

			g.Expect(glance.Annotations).To(HaveKey(dbJobUIDAnnotationKey(tc.suffix)),
				"%s terminal metric MUST stamp the per-phase dedupe annotation on the CR", tc.suffix)
		})
	}
}

// TestReconcileDatabase_UpgradeInProgress_ShortCircuitsBeforeDeployment verifies
// that an in-progress expand Job returns a non-zero RequeueUpgradeWait so the
// pipeline short-circuits before the deployment step; a live Deployment carrying
// the old pod template is left untouched by the database pass.
func TestReconcileDatabase_UpgradeInProgress_ShortCircuitsBeforeDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := upgradingGlance(commonv1.UpgradePhaseExpanding)
	configMapName := "test-glance-config-abc"

	// In-progress expand Job (exists, no terminal condition).
	inProgress := glanceUpgradeJob(glance, configMapName, "expand")
	inProgress.Annotations = map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&inProgress.Spec.Template)}

	// A live Deployment still running the old release's pod template.
	oldGlance := testGlance()
	oldGlance.Spec.OpenStackRelease = "2025.2"
	oldGlance.Spec.Image.Tag = "2025.2"
	oldDeploy := buildGlanceDeployment(oldGlance, testArtifacts(), "", "")

	r := newGlanceTestReconciler(glance, inProgress, oldDeploy)

	res, err := r.reconcileDatabase(context.Background(), r.Client, glance, configMapName)
	g.Expect(err).NotTo(HaveOccurred())
	// Non-zero requeue: the pipeline stops here and never rolls the Deployment.
	g.Expect(res.RequeueAfter).To(Equal(RequeueUpgradeWait))

	// The Deployment's old pod template is unchanged by the database pass.
	var fetched appsv1.Deployment
	g.Expect(r.Get(context.Background(), client.ObjectKeyFromObject(oldDeploy), &fetched)).To(Succeed())
	g.Expect(fetched.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/c5c3/glance:2025.2"))
}

// TestGlanceJobs_PodSettings pins the pod settings of the db-sync Job, the upgrade phases and the db-purge CronJob: unset, every
// container renders the Job resources and no priority class or placement; the
// API Deployment's priority class and node selector carry over; spec.jobs
// overrides both.
func TestGlanceJobs_PodSettings(t *testing.T) {
	podSpecs := func(o *glancev1alpha1.Glance) map[string]corev1.PodSpec {
		return map[string]corev1.PodSpec{
			"db-sync":   database.SyncJob(glanceJobSetParams(o, dbPurgeConfigMapName)).Spec.Template.Spec,
			"db-expand": database.BuildJob(glanceJobSetParams(o, dbPurgeConfigMapName), o.Spec.Image.Reference(), "db-expand", nil, 4).Spec.Template.Spec,
			"db-purge":  dbPurgeCronJob(o, dbPurgeConfigMapName).Spec.JobTemplate.Spec.Template.Spec,
		}
	}
	defaults := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("368Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("368Mi")},
	}

	for _, tc := range []struct {
		name         string
		mutate       func(o *glancev1alpha1.Glance)
		wantPriority string
		wantSelector map[string]string
	}{
		{name: "defaults", mutate: func(*glancev1alpha1.Glance) {}},
		{
			name: "API Deployment fallback",
			mutate: func(o *glancev1alpha1.Glance) {
				o.Spec.Deployment.PriorityClassName = ptr.To("high")
				o.Spec.Deployment.NodeSelector = map[string]string{"a": "b"}
			},
			wantPriority: "high",
			wantSelector: map[string]string{"a": "b"},
		},
		{
			name: "spec.jobs override",
			mutate: func(o *glancev1alpha1.Glance) {
				o.Spec.Deployment.PriorityClassName = ptr.To("high")
				o.Spec.Deployment.NodeSelector = map[string]string{"a": "b"}
				o.Spec.Jobs = &commonv1.JobSpec{
					JobBaseSpec:       commonv1.JobBaseSpec{PriorityClassName: ptr.To("low")},
					NodePlacementSpec: commonv1.NodePlacementSpec{NodeSelector: map[string]string{"pool": "jobs"}},
				}
			},
			wantPriority: "low",
			wantSelector: map[string]string{"pool": "jobs"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			o := managedGlance()
			tc.mutate(o)

			for name, spec := range podSpecs(o) {
				g.Expect(spec.PriorityClassName).To(Equal(tc.wantPriority), name)
				g.Expect(spec.NodeSelector).To(Equal(tc.wantSelector), name)
				for _, c := range append(spec.InitContainers, spec.Containers...) {
					g.Expect(c.Resources).To(Equal(defaults), "%s/%s", name, c.Name)
				}
			}
		})
	}
}
