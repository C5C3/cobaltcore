// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"net/http"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi/computeapitest"
)

var testPoolKey = types.NamespacedName{Namespace: testNamespace, Name: testPoolName}

// controlPlaneObjects are the objects a pool of validNova finds once the
// control plane is up: the Nova, its service-user Secret and its contract.
func controlPlaneObjects() []client.Object {
	return []client.Object{readyNovaForCompute(), novaServiceUserSecret("pw"), computeContractSecret("pw")}
}

// reconcilePool runs one Reconcile of the fixture pool.
func reconcilePool(t *testing.T, r *NovaComputeReconciler) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: testPoolKey})
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	return result
}

func getPool(t *testing.T, r *NovaComputeReconciler) *novav1alpha1.NovaCompute {
	t.Helper()
	cr := &novav1alpha1.NovaCompute{}
	NewGomegaWithT(t).Expect(r.Get(context.Background(), testPoolKey, cr)).To(Succeed())
	return cr
}

func TestNovaComputePipelineSteps_OrderAndNames(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &NovaComputeReconciler{}

	var names []string
	for _, step := range r.pipelineSteps(nil, validNovaCompute(), &novaComputePass{}) {
		names = append(names, step.Name)
	}

	g.Expect(names).To(Equal([]string{"NovaRef", "Nodes", "PoolConfig", "DaemonSet", "Aggregates", "Services"}))
}

func TestNovaComputeReconcile_AddsTheDrainFinalizerOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newNovaComputeTestReconciler(computeapitest.New(), validNovaCompute())

	result := reconcilePool(t, r)

	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueNextPass))
	cr := getPool(t, r)
	g.Expect(cr.Finalizers).To(Equal([]string{novaComputeDrainFinalizer}),
		"a local pool's children are collected from their owner references")

	reconcilePool(t, r)
	g.Expect(getPool(t, r).Finalizers).To(Equal([]string{novaComputeDrainFinalizer}))
}

func TestNovaComputeReconcile_PlacedPoolAddsBothFinalizers(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "compute-a"}
	r := newNovaComputeTestReconciler(computeapitest.New(), cr)
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{Client: novaFakeClientBuilder().Build()})

	g.Expect(reconcilePool(t, r).RequeueAfter).To(Equal(commonreconcile.RequeueNextPass))
	g.Expect(reconcilePool(t, r).RequeueAfter).To(Equal(commonreconcile.RequeueNextPass))

	g.Expect(getPool(t, r).Finalizers).To(ConsistOf(novaComputeDrainFinalizer, commonmulticluster.RemoteChildrenFinalizer))
}

func TestNovaComputeReconcile_UnresolvableTargetAddsNoFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nowhere"}
	r := newNovaComputeTestReconciler(computeapitest.New(), cr)
	r.Resolver = unresolvableResolver{}

	result := reconcilePool(t, r)

	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	got := getPool(t, r)
	g.Expect(got.Finalizers).To(BeEmpty())
	g.Expect(novaComputeCondition(got, conditionTypeNovaReady).Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
}

// TestNovaComputeReconcile_ReachesReady runs the whole pipeline against a Nova
// that has a service registered for the pool's node.
func TestNovaComputeReconcile_ReachesReady(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddService(testNodeName, "enabled", "up")
	cr := validNovaCompute()
	cr.Finalizers = []string{novaComputeDrainFinalizer}
	r := newNovaComputeTestReconciler(api, append(controlPlaneObjects(), cr, selectedNode(testNodeName))...)

	result := reconcilePool(t, r)

	g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling), "the node was Pending when the pass began")
	got := getPool(t, r)
	ready := novaComputeCondition(got, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue), "conditions: %+v", got.Status.Conditions)
	g.Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
	g.Expect(got.Status.Nodes).To(ConsistOf(HaveField("Phase", novav1alpha1.NovaComputeNodeActive)))
	g.Expect(got.Status.InstalledImage).To(Equal("ghcr.io/c5c3/nova-compute:2025.2"))
}

// TestNovaComputeReconcile_EmptySelectionStillRunsTheLaterSteps pins that
// NoMatchingNodes is not a wait: the DaemonSet is applied and Nova is polled.
func TestNovaComputeReconcile_EmptySelectionStillRunsTheLaterSteps(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := validNovaCompute()
	cr.Finalizers = []string{novaComputeDrainFinalizer}
	r := newNovaComputeTestReconciler(api, append(controlPlaneObjects(), cr)...)

	reconcilePool(t, r)

	got := getPool(t, r)
	g.Expect(novaComputeCondition(got, conditionTypeNodesReady).Reason).To(Equal(conditionReasonNoMatchingNodes))
	g.Expect(novaComputeCondition(got, conditionTypeDaemonSetReady)).NotTo(BeNil())
	g.Expect(r.Get(context.Background(), novaComputeDaemonSetKey, &appsv1.DaemonSet{})).To(Succeed(),
		"the selector term keeps the DaemonSet applied while no node matches")
	g.Expect(novaComputeCondition(got, conditionTypeServicesReady)).NotTo(BeNil())
	g.Expect(api.CallsTo(http.MethodGet, "/v2.1/os-services")).To(HaveLen(1))
}

// TestNovaComputeReconcile_ProgressingDaemonSetStillDrains pins that a rollout
// one node never finishes, the NotReady hypervisor being taken out of the pool,
// does not hold up the drain: the Aggregates and Services steps still run.
func TestNovaComputeReconcile_ProgressingDaemonSetStillDrains(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddService(testNodeName, "enabled", "up")
	api.SetServers(testNodeName, 2)
	cr := validNovaCompute()
	cr.Finalizers = []string{novaComputeDrainFinalizer}
	cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeActive)}
	relabelled := poolNode(testNodeName, map[string]string{zoneLabel: testZone})
	c := novaFakeClientBuilder(append(controlPlaneObjects(), cr, relabelled)...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if ds, ok := obj.(*appsv1.DaemonSet); ok {
				// The node's pod never gets ready.
				ds.Generation = 1
				ds.Status = appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 1, UpdatedNumberScheduled: 1}
			}
			return nil
		},
	}).Build()
	r := &NovaComputeReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100), HTTPClient: api}

	reconcilePool(t, r)

	got := getPool(t, r)
	g.Expect(novaComputeCondition(got, conditionTypeDaemonSetReady).Reason).To(Equal(conditionReasonDaemonSetProgressing))
	g.Expect(novaComputeCondition(got, conditionTypeAggregatesReady)).NotTo(BeNil())
	g.Expect(api.Services()[0].Status).To(Equal("disabled"))
	g.Expect(got.Status.Nodes).To(ConsistOf(And(
		HaveField("Phase", novav1alpha1.NovaComputeNodeDraining),
		HaveField("Instances", ptr.To(int32(2))),
	)))
}

// deletingPool is the fixture pool, terminating, with the given finalizers.
func deletingPool(finalizers ...string) *novav1alpha1.NovaCompute {
	cr := validNovaCompute()
	cr.DeletionTimestamp = ptr.To(metav1.Now())
	cr.Finalizers = finalizers
	return cr
}

func TestNovaComputeTeardown_NovaGoneReleasesAtOnce(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := deletingPool(novaComputeDrainFinalizer, commonmulticluster.RemoteChildrenFinalizer)
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "compute-a"}
	cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeActive)}
	r := newNovaComputeTestReconciler(api, cr)
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{Client: novaFakeClientBuilder().Build()})

	reconcilePool(t, r)

	err := r.Get(context.Background(), testPoolKey, &novav1alpha1.NovaCompute{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "both finalizers are released, got %v", err)
	g.Expect(api.Calls()).To(BeEmpty())
}

func TestNovaComputeTeardown_InstancesHoldTheFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddService(testNodeName, "enabled", "up")
	api.SetServers(testNodeName, 2)
	cr := deletingPool(novaComputeDrainFinalizer)
	cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeActive)}
	r := newNovaComputeTestReconciler(api, append(controlPlaneObjects(), cr, selectedNode(testNodeName))...)

	result := reconcilePool(t, r)

	g.Expect(result.RequeueAfter).To(BeNumerically(">", 0))
	got := getPool(t, r)
	g.Expect(got.Finalizers).To(ContainElement(novaComputeDrainFinalizer))
	g.Expect(got.Status.Nodes).To(ConsistOf(And(
		HaveField("Phase", novav1alpha1.NovaComputeNodeDraining),
		HaveField("Instances", ptr.To(int32(2))),
	)))
	g.Expect(api.Services()[0].Status).To(Equal("disabled"))
}

// TestNovaComputeTeardown_LastPoolRemovesTenantFilterTests pins that the
// teardown of the last pool of a Nova runs the aggregate cleanup even with no
// node left to drain, and then releases the CR.
func TestNovaComputeTeardown_LastPoolRemovesTenantFilterTests(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate(tenantFilterTestsAggregate, "", nil, map[string]string{aggregateMarkerKey: testPoolMarker})
	cr := deletingPool(novaComputeDrainFinalizer)
	r := newNovaComputeTestReconciler(api, append(controlPlaneObjects(), cr)...)

	reconcilePool(t, r)

	_, ok := api.Aggregate(tenantFilterTestsAggregate)
	g.Expect(ok).To(BeFalse())
	g.Expect(apierrors.IsNotFound(r.Get(context.Background(), testPoolKey, &novav1alpha1.NovaCompute{}))).To(BeTrue())
}

// TestNovaComputeTeardown_ReleasingTheLastNodeCleansItsAggregates pins the
// pass that releases a deleting pool's last node: its Aggregates step ran while
// the host still sat in the aggregates, and deleting the service empties them.
// The finalizer stays for one more pass, which deletes them.
func TestNovaComputeTeardown_ReleasingTheLastNodeCleansItsAggregates(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddService(testNodeName, "disabled", "up")
	marker := map[string]string{aggregateMarkerKey: testPoolMarker}
	api.AddAggregate(testZone, testZone, []string{testNodeName}, marker)
	api.AddAggregate(tenantFilterTestsAggregate, "", []string{testNodeName}, marker)
	cr := deletingPool(novaComputeDrainFinalizer)
	cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeReleasing)}
	r := newNovaComputeTestReconciler(api, append(controlPlaneObjects(), cr)...)

	reconcilePool(t, r)
	g.Expect(api.Services()).To(BeEmpty())
	g.Expect(getPool(t, r).Finalizers).To(ContainElement(novaComputeDrainFinalizer),
		"the pass that released the last node keeps the finalizer")

	reconcilePool(t, r)
	g.Expect(api.Aggregates()).To(BeEmpty())
	g.Expect(apierrors.IsNotFound(r.Get(context.Background(), testPoolKey, &novav1alpha1.NovaCompute{}))).To(BeTrue())
}

func TestNovaComputeTeardown_UnreachableTargetHolds(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := deletingPool(novaComputeDrainFinalizer, commonmulticluster.RemoteChildrenFinalizer)
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "compute-a"}
	r := newNovaComputeTestReconciler(computeapitest.New(), cr)
	r.Resolver = unresolvableResolver{}

	result := reconcilePool(t, r)

	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	got := getPool(t, r)
	g.Expect(got.Finalizers).To(HaveLen(2))
	g.Expect(novaComputeCondition(got, conditionTypeNovaReady).Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
}

// mirrorSecret is the compute-contract Secret in the pool's namespace,
// labelled as the ControlPlane's mirror when mirrored is set.
func mirrorSecret(mirrored bool) *corev1.Secret {
	secret := computeContractSecret("pw")
	if mirrored {
		secret.Labels = map[string]string{novav1alpha1.ComputeConfigMirrorLabel: "true"}
	}
	return secret
}

func TestReapComputeConfigMirror(t *testing.T) {
	ctx := context.Background()
	contractKey := types.NamespacedName{Namespace: testNamespace, Name: testContract}

	t.Run("the last pool on the cluster deletes a labelled mirror", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := deletingPool(novaComputeDrainFinalizer)
		r := newNovaComputeTestReconciler(nil, cr, mirrorSecret(true))

		g.Expect(r.reapComputeConfigMirror(ctx, r.Client, cr)).To(Succeed())

		g.Expect(apierrors.IsNotFound(r.Get(ctx, contractKey, &corev1.Secret{}))).To(BeTrue())
		g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ConsistOf(ContainSubstring("Normal ComputeConfigMirrorReaped")))
	})

	t.Run("an unlabelled Secret is kept", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := deletingPool(novaComputeDrainFinalizer)
		r := newNovaComputeTestReconciler(nil, cr, mirrorSecret(false))

		g.Expect(r.reapComputeConfigMirror(ctx, r.Client, cr)).To(Succeed())

		g.Expect(r.Get(ctx, contractKey, &corev1.Secret{})).To(Succeed())
	})

	t.Run("another pool on the same cluster keeps it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := deletingPool(novaComputeDrainFinalizer)
		sibling := rivalPool("pool-b", 0, testPoolLabel, "b")
		r := newNovaComputeTestReconciler(nil, cr, sibling, mirrorSecret(true))

		g.Expect(r.reapComputeConfigMirror(ctx, r.Client, cr)).To(Succeed())

		g.Expect(r.Get(ctx, contractKey, &corev1.Secret{})).To(Succeed())
	})

	t.Run("a pool on another cluster or a deleting one does not keep it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := deletingPool(novaComputeDrainFinalizer)
		elsewhere := rivalPool("pool-b", 0, testPoolLabel, "b")
		elsewhere.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "compute-b"}
		leaving := rivalPool("pool-c", 0, testPoolLabel, "c")
		leaving.DeletionTimestamp = ptr.To(metav1.Now())
		leaving.Finalizers = []string{novaComputeDrainFinalizer}
		r := newNovaComputeTestReconciler(nil, cr, elsewhere, leaving, mirrorSecret(true))

		g.Expect(r.reapComputeConfigMirror(ctx, r.Client, cr)).To(Succeed())

		g.Expect(apierrors.IsNotFound(r.Get(ctx, contractKey, &corev1.Secret{}))).To(BeTrue())
	})

	t.Run("a deleting pool that still holds a node keeps it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := deletingPool(novaComputeDrainFinalizer)
		draining := rivalPool("pool-b", 0, testPoolLabel, "b")
		draining.DeletionTimestamp = ptr.To(metav1.Now())
		draining.Finalizers = []string{novaComputeDrainFinalizer}
		draining.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry("node-b", novav1alpha1.NovaComputeNodeDraining)}
		r := newNovaComputeTestReconciler(nil, cr, draining, mirrorSecret(true))

		g.Expect(r.reapComputeConfigMirror(ctx, r.Client, cr)).To(Succeed())

		g.Expect(r.Get(ctx, contractKey, &corev1.Secret{})).To(Succeed(),
			"its draining pod mounts the Secret, and its teardown reads it")
	})

	t.Run("a missing Secret is success", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := deletingPool(novaComputeDrainFinalizer)
		r := newNovaComputeTestReconciler(nil, cr)

		g.Expect(r.reapComputeConfigMirror(ctx, r.Client, cr)).To(Succeed())
	})

	t.Run("the teardown runs it after the Nova is gone", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := deletingPool(novaComputeDrainFinalizer)
		r := newNovaComputeTestReconciler(computeapitest.New(), cr, mirrorSecret(true))

		reconcilePool(t, r)

		g.Expect(apierrors.IsNotFound(r.Get(ctx, contractKey, &corev1.Secret{}))).To(BeTrue())
		g.Expect(controllerutil.ContainsFinalizer(cr, novaComputeDrainFinalizer)).To(BeTrue(), "the in-memory fixture is untouched")
	})
}

// TestNovaComputeTeardownSteps pins the reduced teardown pipeline: a deleting
// pool that holds no node writes no child, so a pass from a stale copy of the
// CR cannot recreate what the sweep removed.
func TestNovaComputeTeardownSteps(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &NovaComputeReconciler{}
	names := func(cr *novav1alpha1.NovaCompute) []string {
		var out []string
		for _, step := range r.teardownSteps(nil, cr, &novaComputePass{}) {
			out = append(out, step.Name)
		}
		return out
	}

	holding := deletingPool(novaComputeDrainFinalizer)
	holding.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeDraining)}
	g.Expect(names(holding)).To(Equal([]string{"NovaRef", "Nodes", "PoolConfig", "DaemonSet", "Aggregates", "Services"}))
	g.Expect(names(deletingPool(novaComputeDrainFinalizer))).To(Equal([]string{"NovaRef", "Aggregates"}))
}
