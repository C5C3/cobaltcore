// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the OVNChassis reconcile entry point, the remote-children
// teardown path, and the watch mappers.
package controller

import (
	"context"
	"testing"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// readyCentral returns the OVNCentral fixture as a chassis needs to find it:
// both addresses published, the client Secret named, and its own rollout landed
// on the image it resolves. Without those four values the central step polls,
// and no chassis test past the gate would reach the steps it is about.
func readyCentral() *ovnv1alpha1.OVNCentral {
	central := publishEndpoints(testOVNCentral())
	central.Status.ClientSecretName = testOVNCentralName + "-client"
	central.Status.InstalledImage = effectiveImage(central.Spec.Image).Reference()
	return central
}

// TestReconcile_NotFoundChassisIsIgnored covers the pass that observes a CR
// after it left etcd. It must not fail the workqueue. Unlike the OVNCentral
// path it drops no metric series: the operator's per-CR collectors count backup
// runs, which only an OVNCentral takes.
func TestReconcile_NotFoundChassisIsIgnored(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newTestOVNChassisReconciler(t) // no OVNChassis seeded

	res, err := r.Reconcile(context.Background(), ovnChassisRequest)

	g.Expect(err).NotTo(HaveOccurred(), "a deleted CR is not an error")
	g.Expect(res.IsZero()).To(BeTrue())
}

// TestReconcileChassis_TargetClusterUnavailableGatesBeforeFinalizer pins the
// failure surface of an unresolvable spec.targetClusterRef: the CR reports
// CentralReady=False with the shared reason, the pass requeues instead of
// erroring, and the CR is left with neither a finalizer nor a single child
// object, because the resolution runs ahead of the finalizer add.
func TestReconcileChassis_TargetClusterUnavailableGatesBeforeFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nowhere"}
	r := newTestOVNChassisReconciler(t, cr, readyCentral())
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ovnChassisRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"an unregistered target cluster is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getOVNChassis(t, r.Client, testOVNChassisName)
	cond := ovnChassisCondition(got, conditionTypeCentralReady)
	g.Expect(cond).NotTo(BeNil(), "the first gate condition must carry the failure")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	g.Expect(got.Finalizers).To(BeEmpty(),
		"a CR whose target never resolves must not be pinned by a finalizer")

	// Nothing was projected: the pass returned before the central gate could
	// resolve, and the management cluster is the only one a failing resolver
	// could have handed back.
	var daemonSets appsv1.DaemonSetList
	g.Expect(r.List(ctx, &daemonSets)).To(Succeed())
	g.Expect(daemonSets.Items).To(BeEmpty())
	var configMaps corev1.ConfigMapList
	g.Expect(r.List(ctx, &configMaps)).To(Succeed())
	g.Expect(configMaps.Items).To(BeEmpty())
}

// TestReconcileChassis_InstallsRemoteChildrenFinalizerForATargetCluster
// verifies that a CR naming a target cluster is pinned before anything is
// projected onto it, so a deletion issued between this pass and the next still
// funnels through the sweep.
func TestReconcileChassis_InstallsRemoteChildrenFinalizerForATargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	r := newTestOVNChassisReconciler(t, cr)
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{
		Client: ovnChassisFakeClientBuilder(t).Build(),
	})

	res, err := r.Reconcile(context.Background(), ovnChassisRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"the pass installing the finalizer must requeue before any sub-reconciler runs")
	g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
}

// TestReconcileChassis_LocalCRNeverCarriesTheRemoteChildrenFinalizer pins the
// other half. A CR that keeps its children on the management cluster has
// nothing for the sweep to do, and a finalizer it does not need is one more
// thing that can block its deletion.
func TestReconcileChassis_LocalCRNeverCarriesTheRemoteChildrenFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis() // no spec.targetClusterRef: children stay local
	r := newTestOVNChassisReconciler(t, cr)

	_, err := r.Reconcile(context.Background(), ovnChassisRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).To(BeEmpty(),
		"a local CR must stay free of the remote-children finalizer")
}

// TestReconcileChassis_TerminatingCR_UnresolvableTargetReleasesFinalizer is the
// guard against a CR that can never be deleted. Deregistering a target cluster
// is a documented operation, and a CR that already projected onto it carries the
// finalizer by then; every pass short-circuiting on "cluster not found" ahead of
// the sweep would leave it Terminating until someone stripped the finalizer by
// hand.
func TestReconcileChassis_TerminatingCR_UnresolvableTargetReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	// The window this process has to sit out starts on its own first failure to
	// resolve, so it has to be compressed for the second pass to reach past it.
	abandonAfter := commonmulticluster.AbandonAfter
	t.Cleanup(func() { commonmulticluster.AbandonAfter = abandonAfter })
	commonmulticluster.AbandonAfter = time.Millisecond

	cr := testOVNChassis()
	// The name is this test's own: the abandon window is tracked per cluster
	// name in a process-global map, and the OVNCentral test that compresses it
	// too would otherwise have started this one's window already.
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "deregistered-chassis"}
	cr.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}
	// Terminating for far longer than the window, which by itself must not be
	// enough: a CR blocked in cleanup for minutes is ordinary, and giving up on
	// it the moment the operator comes back would strand its children.
	deletedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	cr.DeletionTimestamp = &deletedAt
	r := newTestOVNChassisReconciler(t, cr)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ovnChassisRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"the first pass this process fails to resolve on starts the window, it does not end it")
	g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
	time.Sleep(10 * commonmulticluster.AbandonAfter)

	res, err = r.Reconcile(ctx, ovnChassisRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"a target cluster that is gone must not fail the deletion pass")
	g.Expect(res.IsZero()).To(BeTrue(), "the deletion resolves once the window is out")

	// With the finalizer released, the fake client garbage-collects the CR.
	var gone ovnv1alpha1.OVNChassis
	err = r.Get(ctx, ovnChassisRequest.NamespacedName, &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer must be released even without a reachable target cluster")

	// The abandoned children are announced rather than silently dropped.
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
}

// TestReconcileChassis_TerminatingCR_TargetNotEngagedYetKeepsFinalizer pins the
// other half of that contract. Cluster engagement is asynchronous, so a
// registered cluster does not resolve either while the provider is still syncing
// after an operator restart. A CR deleted in that window must requeue rather
// than release its finalizer: abandoning here would leave the DaemonSets running
// on a cluster that is perfectly reachable, with no CR left to retry from.
func TestReconcileChassis_TerminatingCR_TargetNotEngagedYetKeepsFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "not-engaged-yet"}
	cr.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}
	r := newTestOVNChassisReconciler(t, cr)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	// Deleted just now, so the whole abandon window is still ahead.
	g.Expect(r.Delete(ctx, cr)).To(Succeed())

	res, err := r.Reconcile(ctx, ovnChassisRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getOVNChassis(t, r.Client, testOVNChassisName)
	g.Expect(got.Finalizers).To(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
		"a target that may still be engaging must not cost the CR its finalizer")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		NotTo(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))

	// The hold lasts minutes, so it has to be readable off the CR: without it, a
	// namespace stuck Terminating looks like a wedged finalizer and can only be
	// told apart by correlating operator logs across replicas.
	cond := ovnChassisCondition(got, conditionTypeCentralReady)
	g.Expect(cond).NotTo(BeNil(), "the deliberate hold must be visible on the CR")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("not-engaged-yet"))
}

// TestReconcileChassis_FullPassWithCentralReadyReachesReadyWithNoNodes walks the
// whole pipeline of a local CR twice: once against a cluster no node of which
// the selector matches, and once against a node that does match. The first pass
// projects both DaemonSets and reports the empty selection rather than an error,
// the second one turns Ready True and records the node it rendered.
func TestReconcileChassis_FullPassWithCentralReadyReachesReadyWithNoNodes(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	cr.Generation = 4
	r := newTestOVNChassisReconciler(t, cr, readyCentral())

	res, err := r.Reconcile(ctx, ovnChassisRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(),
		"an empty node selection is a steady state, not something to poll")

	got := getOVNChassis(t, r.Client, testOVNChassisName)
	g.Expect(ovnChassisCondition(got, conditionTypeCentralReady).Status).To(Equal(metav1.ConditionTrue))
	nodesReady := ovnChassisCondition(got, conditionTypeNodesReady)
	g.Expect(nodesReady.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(nodesReady.Reason).To(Equal(conditionReasonNoMatchingNodes))
	g.Expect(got.Status.Nodes).To(BeEmpty())
	g.Expect(ovnChassisCondition(got, "Ready").Status).To(Equal(metav1.ConditionFalse),
		"a chassis nothing runs on is not ready")
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(4)),
		"a status write stamps the generation it was decided from")

	// The steps behind the node gate ran anyway: the pods mount the ConfigMap the
	// node step writes, so both DaemonSets exist before a node is labelled.
	g.Expect(r.Get(ctx, chassisOVSDaemonSet, &appsv1.DaemonSet{})).To(Succeed())
	g.Expect(r.Get(ctx, chassisControllerDaemonSet, &appsv1.DaemonSet{})).To(Succeed())

	// Label a node into the selection and let both rollouts land on it.
	g.Expect(r.Create(ctx, chassisNode(testNodeA, selectedLabels()))).To(Succeed())
	g.Expect(simulators.MarkDaemonSetReady(ctx, r.Client, chassisOVSDaemonSet)).To(Succeed())
	g.Expect(simulators.MarkDaemonSetReady(ctx, r.Client, chassisControllerDaemonSet)).To(Succeed())

	res, err = r.Reconcile(ctx, ovnChassisRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a converged chassis is not polled")

	got = getOVNChassis(t, r.Client, testOVNChassisName)
	g.Expect(ovnChassisCondition(got, "Ready").Status).To(Equal(metav1.ConditionTrue))
	g.Expect(got.Status.Nodes).To(HaveLen(1))
	g.Expect(got.Status.Nodes[0].Name).To(Equal(testNodeA))
	g.Expect(got.Status.Nodes[0].SystemID).To(MatchRegexp(uuidPattern))
}

// TestReconcileChassis_CentralNotFoundGatesThePipeline pins the first gate at
// the entry point rather than at the step list. A chassis applied before its
// OVNCentral has nothing to dial and no certificate to present, so the pass has
// to stop before projecting a DaemonSet whose pods would crash-loop against a
// missing Secret.
func TestReconcileChassis_CentralNotFoundGatesThePipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	r := newTestOVNChassisReconciler(t, testOVNChassis()) // no OVNCentral seeded

	res, err := r.Reconcile(ctx, ovnChassisRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"an OVNCentral that has not been applied yet is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getOVNChassis(t, r.Client, testOVNChassisName)
	cond := ovnChassisCondition(got, conditionTypeCentralReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCentralNotFound))
	g.Expect(ovnChassisCondition(got, "Ready").Status).To(Equal(metav1.ConditionFalse))

	// The step after the gate never ran, so nothing was projected.
	var daemonSets appsv1.DaemonSetList
	g.Expect(r.List(ctx, &daemonSets)).To(Succeed())
	g.Expect(daemonSets.Items).To(BeEmpty())
	g.Expect(ovnChassisCondition(got, conditionTypeNodesReady)).To(BeNil())
}

// TestChassisPipelineSteps_OrderAndNames pins the pipeline: five named steps in
// dependency order and no parallel group, because ovn-controller registers
// itself in the local database Open vSwitch owns.
func TestChassisPipelineSteps_OrderAndNames(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &OVNChassisReconciler{}

	steps := r.pipelineSteps(nil, testOVNChassis(), nil)

	var named []string
	for _, step := range steps {
		named = append(named, step.Name)
	}
	g.Expect(named).To(Equal([]string{"Central", "Nodes", "OVS", "Controller", "Maintenance"}))
}

func TestSetChassisReadyCondition_TrueOnlyWhenAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()

	// Every sub-condition True → aggregate Ready True.
	for _, ct := range chassisSubConditionTypes {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:   ct,
			Status: metav1.ConditionTrue,
			Reason: "OK",
		})
	}
	setChassisReadyCondition(cr)
	ready := ovnChassisCondition(cr, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

	// Flip one sub-condition False → aggregate Ready flips False.
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:   conditionTypeMaintenanceReady,
		Status: metav1.ConditionFalse,
		Reason: "Degraded",
	})
	setChassisReadyCondition(cr)
	g.Expect(ovnChassisCondition(cr, "Ready").Status).To(Equal(metav1.ConditionFalse))
}

// --- watch mappers ----------------------------------------------------------

// indexedChassisClient builds a client with the OVNChassis field index the
// central-to-chassis mapper resolves through, registered from the same extractor
// the manager registers, so the test exercises the production lookup rather than
// a hand-filtered list.
func indexedChassisClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	return ovnChassisFakeClientBuilder(t, objs...).
		WithIndex(&ovnv1alpha1.OVNChassis{}, OVNChassisCentralRefIndexKey, ovnChassisCentralRefExtractor).
		Build()
}

// chassisFor builds an OVNChassis attached to the named OVNCentral.
func chassisFor(name, namespace, central string) *ovnv1alpha1.OVNChassis {
	cr := testOVNChassis()
	cr.Name = name
	cr.Namespace = namespace
	cr.Spec.CentralRef = ovnv1alpha1.OVNCentralRef{Name: central}
	return cr
}

// The mapper has to select exactly the chassis of the OVNCentral that changed:
// a namespace holds the chassis of every control plane deployed into it, and a
// second namespace holds a control plane of its own, whose chassis this event
// says nothing about.
func TestCentralToChassisMapperUsesTheIndex(t *testing.T) {
	g := NewGomegaWithT(t)

	c := indexedChassisClient(t,
		chassisFor("edge", testNamespace, testOVNCentralName),
		chassisFor("compute", testNamespace, testOVNCentralName),
		chassisFor("other-central", testNamespace, "other"),
		chassisFor("elsewhere", "openstack-two", testOVNCentralName),
	)

	requests := centralToChassisMapper(c)(context.Background(), testOVNCentral())

	g.Expect(requests).To(ConsistOf(
		reconcile.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: "edge"}},
		reconcile.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: "compute"}},
	))
}

// An object of another kind maps to nothing rather than to whatever chassis
// happen to name an OVNCentral of the same name. The leg watches OVNCentral
// alone, so this is the guard against a future caller wiring the mapper onto a
// second kind.
func TestCentralToChassisMapper_IgnoresNonCentralObjects(t *testing.T) {
	g := NewGomegaWithT(t)

	c := indexedChassisClient(t, chassisFor("edge", testNamespace, testOVNCentralName))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      testOVNCentralName,
		Namespace: testNamespace,
	}}

	g.Expect(centralToChassisMapper(c)(context.Background(), secret)).To(BeNil())
}

// A list the mapper cannot answer maps to nothing rather than to a partial fan
// out: handler.MapFunc has no error return, and the periodic requeue is the
// fallback.
func TestCentralToChassisMapper_ListErrorMapsToNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	c := ovnChassisFakeClientBuilder(t).
		WithIndex(&ovnv1alpha1.OVNChassis{}, OVNChassisCentralRefIndexKey, ovnChassisCentralRefExtractor).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewServiceUnavailable("cache not started")
			},
		}).Build()

	g.Expect(centralToChassisMapper(c)(context.Background(), testOVNCentral())).To(BeEmpty())
}

// A Node event reaches every chassis, across namespaces: the label that changed
// may have added the node to one CR's selection and taken it out of another's,
// and there is no index that resolves a node to the CRs it concerns.
func TestNodeToChassisMapperEnqueuesEveryChassis(t *testing.T) {
	g := NewGomegaWithT(t)

	c := ovnChassisFakeClientBuilder(t,
		chassisFor("edge", testNamespace, testOVNCentralName),
		chassisFor("elsewhere", "openstack-two", "other"),
	).Build()

	requests := nodeToChassisMapper(c)(context.Background(), chassisNode(testNodeA, selectedLabels()))

	g.Expect(requests).To(ConsistOf(
		reconcile.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: "edge"}},
		reconcile.Request{NamespacedName: client.ObjectKey{Namespace: "openstack-two", Name: "elsewhere"}},
	))

	// A list that fails maps to nothing, per the handler.MapFunc contract.
	broken := ovnChassisFakeClientBuilder(t).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewServiceUnavailable("cache not started")
			},
		}).Build()
	g.Expect(nodeToChassisMapper(broken)(context.Background(), chassisNode(testNodeA, nil))).To(BeEmpty())
}

// --- remote children --------------------------------------------------------

// terminatingRemoteChassis returns an OVNChassis that names a target cluster and
// is being deleted, carrying the remote-children finalizer plus a foreign one so
// the CR survives the release and the test can read back which finalizers the
// pass dropped.
func terminatingRemoteChassis(t *testing.T) (*OVNChassisReconciler, *ovnv1alpha1.OVNChassis) {
	t.Helper()
	g := NewGomegaWithT(t)

	cr := testOVNChassis()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	cr.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer, "foreign.example.com/keep-alive"}
	deletedAt := metav1.NewTime(time.Now())
	cr.DeletionTimestamp = &deletedAt

	r := newTestOVNChassisReconciler(t, cr)
	var terminating ovnv1alpha1.OVNChassis
	g.Expect(r.Get(context.Background(), ovnChassisRequest.NamespacedName, &terminating)).To(Succeed())
	return r, &terminating
}

// TestReconcileChassisDeleteRemoteChildren_SweepsEveryLabelledChild is the whole
// point of the finalizer: no garbage collection cascade crosses the cluster
// boundary, so the objects this CR projected have to be deleted by name from
// here, across every API group they live in. What the sweep leaves standing
// matters as much: a target cluster carries other people's objects, and an
// object nobody claimed or another CR claimed is not ours to remove.
func TestReconcileChassisDeleteRemoteChildren_SweepsEveryLabelledChild(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteChassis(t)

	daemonSet := ownedRemoteChild(t, terminating,
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: testOVNChassisName + "-ovs", Namespace: testNamespace}})
	configMap := ownedRemoteChild(t, terminating,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: testOVNChassisName + "-nodes", Namespace: testNamespace}})
	job := ownedRemoteChild(t, terminating,
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: testOVNChassisName + "-apply-node-a", Namespace: testNamespace}})
	unlabelled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-ca", Namespace: testNamespace}}
	foreign := ownedRemoteChild(t,
		&ovnv1alpha1.OVNChassis{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testNamespace}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "other-apply-node-a", Namespace: testNamespace}})
	target := ovnChassisFakeClientBuilder(t, daemonSet, configMap, job, unlabelled, foreign).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)

	g.Expect(err).NotTo(HaveOccurred())

	for _, child := range []client.Object{daemonSet, configMap, job} {
		err := target.Get(ctx, client.ObjectKeyFromObject(child), child.DeepCopyObject().(client.Object))
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s must be swept", child, child.GetName())
	}
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(unlabelled), &corev1.ConfigMap{})).To(Succeed(),
		"an object nobody claimed is nobody's child")
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(foreign), &batchv1.Job{})).To(Succeed(),
		"another OVNChassis's child must survive this one's teardown")

	updated := getOVNChassis(t, r.Client, testOVNChassisName)
	g.Expect(updated.Finalizers).To(ConsistOf("foreign.example.com/keep-alive"),
		"a completed sweep must release the remote-children finalizer and nothing else")
}

// TestReconcileChassisDeleteRemoteChildren_NilChildrenAbandonsAndReleases covers
// the deregistered target cluster. Its children cannot be reached, so holding
// the finalizer would only strand the CR in Terminating; the objects left
// running are announced rather than silently dropped. Any attempt to sweep
// through the nil client would fault, which is what proves nothing was deleted.
func TestReconcileChassisDeleteRemoteChildren_NilChildrenAbandonsAndReleases(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteChassis(t)

	err := r.reconcileDeleteRemoteChildren(context.Background(), nil, terminating)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
	g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).
		NotTo(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
			"an unreachable target cluster must not pin the CR forever")
}

// TestReconcileChassisDeleteRemoteChildren_WithoutFinalizerIsANoOp pins the
// guard a local CR relies on. It never carries the finalizer, its children are
// collected from their owner references, and a sweep running anyway would delete
// objects on the management cluster that the cascade already owns.
func TestReconcileChassisDeleteRemoteChildren_WithoutFinalizerIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr)

	child := ownedRemoteChild(t, cr,
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: testOVNChassisName + "-ovs", Namespace: testNamespace}})
	target := ovnChassisFakeClientBuilder(t, child).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(child), &appsv1.DaemonSet{})).To(Succeed(),
		"a CR without the finalizer must not sweep anything")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
}

// TestReconcileChassisDeleteRemoteChildren_SweepFailureKeepsFinalizer is the
// guard against a CR that leaves etcd while its DaemonSets keep running. A list
// the target cluster refuses says nothing about whether children exist, so the
// pass has to fail and sweep again on the next one.
func TestReconcileChassisDeleteRemoteChildren_SweepFailureKeepsFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteChassis(t)

	child := ownedRemoteChild(t, terminating,
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: testOVNChassisName + "-ovs", Namespace: testNamespace}})
	target := ovnChassisFakeClientBuilder(t, child).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if list.GetObjectKind().GroupVersionKind().Kind == "DaemonSetList" {
					return apierrors.NewForbidden(appsv1.Resource("daemonsets"), "", nil)
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)

	g.Expect(err).To(MatchError(ContainSubstring("listing remote DaemonSet children for teardown")))
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(child), &appsv1.DaemonSet{})).To(Succeed())
	g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
			"a failed sweep must keep the finalizer so the next pass retries")
}
