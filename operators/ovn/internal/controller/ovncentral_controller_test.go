// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the OVNCentral reconcile entry point, the remote-children
// teardown path, and the field-index plumbing.
package controller

import (
	"context"
	"testing"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	ovnmetrics "github.com/c5c3/cobaltcore/operators/ovn/internal/metrics"
)

// unresolvableResolver is a ClusterResolver that never knows any cluster. It
// returns the upstream sentinel so the test asserts the message an operator
// actually reads on the CR, not a locally invented string.
type unresolvableResolver struct{}

func (unresolvableResolver) GetCluster(_ context.Context, _ mcruntime.ClusterName) (cluster.Cluster, error) {
	return nil, mcruntime.ErrClusterNotFound
}

// backupSeriesCount counts the ovn_operator_backup_total series carrying this
// CR's labels on the process-wide registry the operator publishes on.
func backupSeriesCount(t *testing.T, name, namespace string) int {
	t.Helper()
	g := NewGomegaWithT(t)

	families, err := ctrlmetrics.Registry.Gather()
	g.Expect(err).NotTo(HaveOccurred())

	count := 0
	for _, fam := range families {
		if fam.GetName() != "ovn_operator_backup_total" {
			continue
		}
		for _, metric := range fam.GetMetric() {
			labels := map[string]string{}
			for _, l := range metric.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["ovncentral"] == name && labels["namespace"] == namespace {
				count++
			}
		}
	}
	return count
}

// TestReconcile_NotFoundCRIsIgnored covers the pass that observes a CR after it
// left etcd. It must not fail the workqueue, and it is the only pass a CR
// keeping its children on the management cluster ever reaches after deletion,
// so it is where the per-CR metric series have to be dropped, or a deleted CR
// keeps a time series for the operator's lifetime.
func TestReconcile_NotFoundCRIsIgnored(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(ovnmetrics.Register()).To(Succeed())
	t.Cleanup(func() { ovnmetrics.DeleteForOVNCentral(testOVNCentralName, testNamespace) })

	ovnmetrics.RecordBackup(testOVNCentralName, testNamespace, "succeeded", time.Second)
	g.Expect(backupSeriesCount(t, testOVNCentralName, testNamespace)).To(BeNumerically(">", 0),
		"the fixture has to leave a series behind for the cleanup to be observable")

	r := newTestOVNCentralReconciler(t) // no OVNCentral seeded

	res, err := r.Reconcile(context.Background(), ovnCentralRequest)

	g.Expect(err).NotTo(HaveOccurred(), "a deleted CR is not an error")
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(backupSeriesCount(t, testOVNCentralName, testNamespace)).To(Equal(0),
		"a CR that left etcd must not keep a time series alive")
}

// TestReconcile_TargetClusterUnavailableGatesBeforeFinalizer pins the failure
// surface of an unresolvable spec.targetClusterRef: the CR reports
// TLSReady=False with the shared reason, the pass requeues instead of erroring,
// and the CR is left with neither a finalizer nor a single child object,
// because the resolution runs ahead of the finalizer add.
func TestReconcile_TargetClusterUnavailableGatesBeforeFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nowhere"}
	r := newTestOVNCentralReconciler(t, cr)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ovnCentralRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"an unregistered target cluster is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getOVNCentral(t, r.Client, testOVNCentralName)
	cond := ovnCentralCondition(got, conditionTypeTLSReady)
	g.Expect(cond).NotTo(BeNil(), "the first gate condition must carry the failure")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	g.Expect(got.Finalizers).To(BeEmpty(),
		"a CR whose target never resolves must not be pinned by a finalizer")

	// Nothing was projected: the pass returned before the TLS gate could write,
	// and the management cluster is the only one a failing resolver could have
	// handed back.
	var certificates certmanagerv1.CertificateList
	g.Expect(r.List(ctx, &certificates)).To(Succeed())
	g.Expect(certificates.Items).To(BeEmpty())
	var statefulSets appsv1.StatefulSetList
	g.Expect(r.List(ctx, &statefulSets)).To(Succeed())
	g.Expect(statefulSets.Items).To(BeEmpty())
}

// TestReconcile_InstallsRemoteChildrenFinalizerForATargetCluster verifies that a
// CR naming a target cluster is pinned before anything is projected onto it, so
// a deletion issued between this pass and the next still funnels through the
// sweep.
func TestReconcile_InstallsRemoteChildrenFinalizerForATargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	r := newTestOVNCentralReconciler(t, cr)
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{
		Client: ovnCentralFakeClientBuilder(t).Build(),
	})

	res, err := r.Reconcile(context.Background(), ovnCentralRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"the pass installing the finalizer must requeue before any sub-reconciler runs")
	g.Expect(getOVNCentral(t, r.Client, testOVNCentralName).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
}

// TestReconcile_LocalCRNeverCarriesTheRemoteChildrenFinalizer pins the other
// half. A CR that keeps its children on the management cluster has nothing for
// the sweep to do, and a finalizer it does not need is one more thing that can
// block its deletion.
func TestReconcile_LocalCRNeverCarriesTheRemoteChildrenFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral() // no spec.targetClusterRef: children stay local
	r := newTestOVNCentralReconciler(t, cr)

	_, err := r.Reconcile(context.Background(), ovnCentralRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getOVNCentral(t, r.Client, testOVNCentralName).Finalizers).To(BeEmpty(),
		"a local CR must stay free of the remote-children finalizer")
}

// TestReconcile_TerminatingCR_UnresolvableTargetReleasesFinalizer is the guard
// against a CR that can never be deleted. Deregistering a target cluster is a
// documented operation, and a CR that already projected onto it carries the
// finalizer by then. While the resolution ran ahead of the deletion branch,
// every pass short-circuited on "cluster not found" before the sweep, the
// finalizer was never released, and the CR stayed Terminating until someone
// stripped it by hand.
func TestReconcile_TerminatingCR_UnresolvableTargetReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	// The window this process has to sit out starts on its own first failure to
	// resolve, so it has to be compressed for the second pass to reach past it.
	abandonAfter := commonmulticluster.AbandonAfter
	t.Cleanup(func() { commonmulticluster.AbandonAfter = abandonAfter })
	commonmulticluster.AbandonAfter = time.Millisecond

	cr := testOVNCentral()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "deregistered"}
	cr.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}
	// Terminating for far longer than the window, which by itself must not be
	// enough: a CR blocked in cleanup for minutes is ordinary, and giving up on
	// it the moment the operator comes back would strand its children.
	deletedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	cr.DeletionTimestamp = &deletedAt
	r := newTestOVNCentralReconciler(t, cr)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ovnCentralRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"the first pass this process fails to resolve on starts the window, it does not end it")
	g.Expect(getOVNCentral(t, r.Client, testOVNCentralName).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
	time.Sleep(10 * commonmulticluster.AbandonAfter)

	res, err = r.Reconcile(ctx, ovnCentralRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"a target cluster that is gone must not fail the deletion pass")
	g.Expect(res.IsZero()).To(BeTrue(), "the deletion resolves once the window is out")

	// With the finalizer released, the fake client garbage-collects the CR.
	var gone ovnv1alpha1.OVNCentral
	err = r.Get(ctx, ovnCentralRequest.NamespacedName, &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer must be released even without a reachable target cluster")

	// The abandoned children are announced rather than silently dropped.
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
}

// TestReconcile_TerminatingCR_TargetNotEngagedYetKeepsFinalizer pins the other
// half of that contract. Cluster engagement is asynchronous, so a registered
// cluster does not resolve either while the provider is still syncing after an
// operator restart. A CR deleted in that window must requeue rather than release
// its finalizer: abandoning here would leave its databases running on a cluster
// that is perfectly reachable, with no CR left to retry from.
func TestReconcile_TerminatingCR_TargetNotEngagedYetKeepsFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "not-engaged-yet"}
	cr.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}
	r := newTestOVNCentralReconciler(t, cr)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	// Deleted just now, so the whole abandon window is still ahead.
	g.Expect(r.Delete(ctx, cr)).To(Succeed())

	res, err := r.Reconcile(ctx, ovnCentralRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getOVNCentral(t, r.Client, testOVNCentralName)
	g.Expect(got.Finalizers).To(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
		"a target that may still be engaging must not cost the CR its finalizer")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		NotTo(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))

	// The hold lasts minutes, so it has to be readable off the CR: without it, a
	// namespace stuck Terminating looks like a wedged finalizer and can only be
	// told apart by correlating operator logs across replicas.
	cond := ovnCentralCondition(got, conditionTypeTLSReady)
	g.Expect(cond).NotTo(BeNil(), "the deliberate hold must be visible on the CR")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("not-engaged-yet"))
}

// TestReconcile_FirstPassRunsTheTLSGateFirst pins the pipeline order at the
// entry point rather than at the step list: a cluster without cert-manager can
// issue nothing, so the first pass has to stop at the TLS gate and leave the
// databases unprojected. Every OVN connection is authenticated with those
// certificates, and a StatefulSet started without them comes up with nothing to
// present.
func TestReconcile_FirstPassRunsTheTLSGateFirst(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()
	cr.Generation = 3
	// cert-manager is absent, which is the one thing this reconciler cannot work
	// around; the shared constructor would report it installed.
	r := &OVNCentralReconciler{
		Client:   ovnCentralFakeClientBuilder(t, cr).Build(),
		Scheme:   newTestScheme(t),
		Recorder: record.NewFakeRecorder(50),
	}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ovnCentralRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"a missing CRD is a wait for an operator action, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getOVNCentral(t, r.Client, testOVNCentralName)
	cond := ovnCentralCondition(got, conditionTypeTLSReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCertManagerUnavailable))

	ready := ovnCentralCondition(got, "Ready")
	g.Expect(ready).NotTo(BeNil(), "every exit re-aggregates the Ready condition")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(3)),
		"a status write stamps the generation it was decided from")

	// The step after the gate never ran, so no database was projected.
	var statefulSets appsv1.StatefulSetList
	g.Expect(r.List(ctx, &statefulSets)).To(Succeed())
	g.Expect(statefulSets.Items).To(BeEmpty())
	g.Expect(ovnCentralCondition(got, conditionTypeNorthboundReady)).To(BeNil())
}

// TestPipelineSteps_OrderAndNames pins the sequential pipeline: the four named
// gates in dependency order, then the unnamed parallel group that
// self-instruments its own members.
func TestPipelineSteps_OrderAndNames(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &OVNCentralReconciler{}

	steps := r.pipelineSteps(nil, testOVNCentral())

	g.Expect(steps).To(HaveLen(5))
	var named []string
	for _, step := range steps[:4] {
		named = append(named, step.Name)
	}
	g.Expect(named).To(Equal([]string{"TLS", "Northbound", "Southbound", "Endpoints"}))
	g.Expect(steps[4].Name).To(BeEmpty(),
		"the parallel group must stay unnamed so RunPipeline does not instrument it twice")
}

// RunParallelGroup merges a member's conditions and its metadata back onto the
// primary CR and nothing else, so the two fields the group publishes into
// status have to be carried over by the step itself. Without it both writes are
// discarded with the copy the member ran on: an OVNChassis is never handed the
// relay address and never learns that the central finished its rollout, so it
// dials the Raft leader directly and upgrades ahead of the databases.
//
// The members are driven the way the group drives them — through the step's own
// Fn, on a DeepCopy — because running the sub-reconciler directly on the primary
// is exactly what hides the defect.
func TestParallelSteps_CarryPublishedStatusOntoThePrimary(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	primary := publishEndpoints(relayOVNCentral())
	r := newTestOVNCentralReconciler(t, primary)

	stepNamed := func(name string) commonreconcile.ParallelStep[*ovnv1alpha1.OVNCentral] {
		for _, step := range r.parallelSteps(r.Client, primary) {
			if step.Name == name {
				return step
			}
		}
		t.Fatalf("no parallel step named %s", name)
		return commonreconcile.ParallelStep[*ovnv1alpha1.OVNCentral]{}
	}

	// First pass: both members create their children, neither publishes yet.
	for _, name := range []string{"Northd", "Relay"} {
		_, err := stepNamed(name).Fn(ctx, primary.DeepCopy())
		g.Expect(err).NotTo(HaveOccurred(), name)
	}
	g.Expect(primary.Status.InstalledImage).To(BeEmpty())
	g.Expect(primary.Status.RelayAddress).To(BeEmpty())

	// The parts of the environment the fake client does not run.
	assignRelayClusterIP(ctx, t, r)
	g.Expect(simulators.SimulateDeploymentReady(ctx, r.Client, centralKey("ovn-northd"),
		commonv1.DefaultReplicas)).To(Succeed())
	g.Expect(simulators.SimulateDeploymentReady(ctx, r.Client, centralKey("ovn-sb-relay"), 2)).To(Succeed())

	for _, name := range []string{"Northd", "Relay"} {
		_, err := stepNamed(name).Fn(ctx, primary.DeepCopy())
		g.Expect(err).NotTo(HaveOccurred(), name)
	}

	g.Expect(primary.Status.InstalledImage).To(Equal(effectiveImage(nil).Reference()))
	g.Expect(primary.Status.RelayAddress).To(Equal("ssl:" + testRelayClusterIP + ":6642"))
}

func TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()

	// Every sub-condition True → aggregate Ready True.
	for _, ct := range centralSubConditionTypes {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:   ct,
			Status: metav1.ConditionTrue,
			Reason: "OK",
		})
	}
	setReadyCondition(cr)
	ready := ovnCentralCondition(cr, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

	// Flip one sub-condition False → aggregate Ready flips False.
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:   conditionTypeBackupReady,
		Status: metav1.ConditionFalse,
		Reason: "Degraded",
	})
	setReadyCondition(cr)
	g.Expect(ovnCentralCondition(cr, "Ready").Status).To(Equal(metav1.ConditionFalse))
}

// --- field index ------------------------------------------------------------

// recordingFieldIndexer is a client.FieldIndexer that records the keys it was
// asked to register, so the registration helper can be exercised without a
// running manager.
type recordingFieldIndexer struct {
	keys []string
}

func (r *recordingFieldIndexer) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	r.keys = append(r.keys, field)
	return nil
}

func TestRegisterOVNIndexes_RegistersCentralRefKey(t *testing.T) {
	g := NewGomegaWithT(t)

	idx := &recordingFieldIndexer{}
	g.Expect(registerOVNIndexes(context.Background(), idx)).To(Succeed())
	g.Expect(idx.keys).To(ConsistOf(OVNChassisCentralRefIndexKey))
}

func TestOVNChassisCentralRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	chassis := &ovnv1alpha1.OVNChassis{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: testNamespace},
		Spec:       ovnv1alpha1.OVNChassisSpec{CentralRef: ovnv1alpha1.OVNCentralRef{Name: testOVNCentralName}},
	}
	g.Expect(ovnChassisCentralRefExtractor(chassis)).To(ConsistOf(testOVNCentralName))

	// A chassis that reached the indexer without a reference indexes under
	// nothing rather than under the empty string, which would collect every such
	// CR under one key.
	chassis.Spec.CentralRef.Name = ""
	g.Expect(ovnChassisCentralRefExtractor(chassis)).To(BeNil())

	// The wrong object type yields nil rather than a panic.
	g.Expect(ovnChassisCentralRefExtractor(&corev1.ConfigMap{})).To(BeNil())
}

// --- remote children --------------------------------------------------------

// ownedRemoteChild stamps the ownership labels the projection wrote on a child
// it put on the target cluster, so the sweep selects it exactly as it would
// there.
func ownedRemoteChild(t *testing.T, owner, child client.Object) client.Object {
	t.Helper()

	labels, err := commonmulticluster.OwnerLabels(newTestScheme(t), owner)
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	child.SetLabels(labels)
	return child
}

// terminatingRemoteCentral returns an OVNCentral that names a target cluster and
// is being deleted, carrying the remote-children finalizer plus a foreign one so
// the CR survives the release and the test can read back which finalizers the
// pass dropped.
func terminatingRemoteCentral(t *testing.T) (*OVNCentralReconciler, *ovnv1alpha1.OVNCentral) {
	t.Helper()
	g := NewGomegaWithT(t)

	cr := testOVNCentral()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	cr.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer, "foreign.example.com/keep-alive"}
	deletedAt := metav1.NewTime(time.Now())
	cr.DeletionTimestamp = &deletedAt

	r := newTestOVNCentralReconciler(t, cr)
	var terminating ovnv1alpha1.OVNCentral
	g.Expect(r.Get(context.Background(), ovnCentralRequest.NamespacedName, &terminating)).To(Succeed())
	return r, &terminating
}

// TestReconcileDeleteRemoteChildren_SweepsEveryLabelledChild is the whole point
// of the finalizer: no garbage collection cascade crosses the cluster boundary,
// so the objects this CR projected have to be deleted by name from here, across
// every API group they live in. What the sweep leaves standing matters as much:
// a target cluster carries other people's objects, and an object nobody claimed
// or another CR claimed is not ours to remove.
func TestReconcileDeleteRemoteChildren_SweepsEveryLabelledChild(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteCentral(t)

	statefulSet := ownedRemoteChild(t, terminating,
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: testOVNCentralName + "-nb", Namespace: testNamespace}})
	pvc := ownedRemoteChild(t, terminating,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testOVNCentralName + "-backup", Namespace: testNamespace}})
	cronJob := ownedRemoteChild(t, terminating,
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: testOVNCentralName + "-backup", Namespace: testNamespace}})
	certificate := ownedRemoteChild(t, terminating,
		&certmanagerv1.Certificate{ObjectMeta: metav1.ObjectMeta{Name: testOVNCentralName + "-client", Namespace: testNamespace}})
	unlabelled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-ca", Namespace: testNamespace}}
	foreign := ownedRemoteChild(t,
		&ovnv1alpha1.OVNCentral{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testNamespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "other-nb-0", Namespace: testNamespace}})
	target := ovnCentralFakeClientBuilder(t, statefulSet, pvc, cronJob, certificate, unlabelled, foreign).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)

	g.Expect(err).NotTo(HaveOccurred())

	for _, child := range []client.Object{statefulSet, pvc, cronJob, certificate} {
		err := target.Get(ctx, client.ObjectKeyFromObject(child), child.DeepCopyObject().(client.Object))
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s must be swept", child, child.GetName())
	}
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(unlabelled), &corev1.ConfigMap{})).To(Succeed(),
		"an object nobody claimed is nobody's child")
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Service{})).To(Succeed(),
		"another OVNCentral's child must survive this one's teardown")

	updated := getOVNCentral(t, r.Client, testOVNCentralName)
	g.Expect(updated.Finalizers).To(ConsistOf("foreign.example.com/keep-alive"),
		"a completed sweep must release the remote-children finalizer and nothing else")
}

// TestReconcileDeleteRemoteChildren_NilChildrenAbandonsAndReleases covers the
// deregistered target cluster. Its children cannot be reached, so holding the
// finalizer would only strand the CR in Terminating; the objects left running
// are announced rather than silently dropped. Any attempt to sweep through the
// nil client would fault, which is what proves nothing was deleted.
func TestReconcileDeleteRemoteChildren_NilChildrenAbandonsAndReleases(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteCentral(t)

	err := r.reconcileDeleteRemoteChildren(context.Background(), nil, terminating)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
	g.Expect(getOVNCentral(t, r.Client, testOVNCentralName).Finalizers).
		NotTo(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
			"an unreachable target cluster must not pin the CR forever")
}

// TestReconcileDeleteRemoteChildren_WithoutFinalizerIsANoOp pins the guard a
// local CR relies on. It never carries the finalizer, its children are collected
// from their owner references, and a sweep running anyway would delete objects
// on the management cluster that the cascade already owns.
func TestReconcileDeleteRemoteChildren_WithoutFinalizerIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)

	child := ownedRemoteChild(t, cr,
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: testOVNCentralName + "-nb", Namespace: testNamespace}})
	target := ovnCentralFakeClientBuilder(t, child).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(child), &appsv1.StatefulSet{})).To(Succeed(),
		"a CR without the finalizer must not sweep anything")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
}

// TestReconcileDeleteRemoteChildren_SweepFailureKeepsFinalizer is the guard
// against a CR that leaves etcd while its databases keep running. A list the
// target cluster refuses says nothing about whether children exist, so the pass
// has to fail and sweep again on the next one.
func TestReconcileDeleteRemoteChildren_SweepFailureKeepsFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteCentral(t)

	child := ownedRemoteChild(t, terminating,
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: testOVNCentralName + "-nb", Namespace: testNamespace}})
	target := ovnCentralFakeClientBuilder(t, child).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if list.GetObjectKind().GroupVersionKind().Kind == "StatefulSetList" {
					return apierrors.NewForbidden(appsv1.Resource("statefulsets"), "", nil)
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)

	g.Expect(err).To(MatchError(ContainSubstring("listing remote StatefulSet children for teardown")))
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(child), &appsv1.StatefulSet{})).To(Succeed())
	g.Expect(getOVNCentral(t, r.Client, testOVNCentralName).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
			"a failed sweep must keep the finalizer so the next pass retries")
}
