// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/forge/internal/common/conditions"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

func TestReconcile_NotFoundIsNoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newTestReconciler(testScheme())

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
}

func TestReconcile_DeletingCRSkipsPipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	now := metav1.NewTime(time.Now())
	h.DeletionTimestamp = &now
	// A finalizer is required for the fake client to accept an object with a
	// DeletionTimestamp; the operator itself never installs one.
	h.Finalizers = []string{"test.c5c3.io/keep"}
	r := newTestReconciler(testScheme(), h)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-horizon"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// No pipeline step ran: no conditions were persisted.
	var got horizonv1alpha1.Horizon
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-horizon"}, &got)).To(Succeed())
	g.Expect(got.Status.Conditions).To(BeEmpty())
}

func TestReconcile_SecretsGateShortCircuitsAndPersistsStatus(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	// Store ready but the SECRET_KEY Secret is absent: the pipeline must stop
	// at Secrets, persist SecretsReady=False, and aggregate Ready=False.
	r := newTestReconciler(testScheme(), h, readyClusterSecretStore(openBaoClusterStoreName))

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-horizon"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	var got horizonv1alpha1.Horizon
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-horizon"}, &got)).To(Succeed())

	secretsCond := conditions.GetCondition(got.Status.Conditions, "SecretsReady")
	g.Expect(secretsCond).NotTo(BeNil())
	g.Expect(secretsCond.Status).To(Equal(metav1.ConditionFalse))

	readyCond := conditions.GetCondition(got.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))

	// The chain short-circuited: no downstream condition was set.
	g.Expect(conditions.GetCondition(got.Status.Conditions, "DeploymentReady")).To(BeNil())
}

// unresolvableResolver is a ClusterResolver that never knows any cluster. It
// returns the upstream sentinel so the test asserts the message an operator
// actually reads on the CR, not a locally invented string.
type unresolvableResolver struct{}

func (unresolvableResolver) GetCluster(_ context.Context, _ mcruntime.ClusterName) (cluster.Cluster, error) {
	return nil, mcruntime.ErrClusterNotFound
}

// TestReconcile_TargetClusterUnavailableGatesReconcile pins the failure surface
// of an unresolvable spec.targetClusterRef: the CR reports SecretsReady=False
// with the shared reason, the pass requeues instead of erroring, and not a
// single child object is projected. The secret gate is deliberately satisfied
// by the seeds, so a pass that skipped the resolution would have rendered the
// config ConfigMap and the dashboard Deployment.
func TestReconcile_TargetClusterUnavailableGatesReconcile(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nowhere"}
	r := newTestReconciler(testScheme(), h,
		readyClusterSecretStore(openBaoClusterStoreName),
		secretKeySecret("horizon-secret-key", "default", "secret-key", "django-key"))
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-horizon"},
	})

	g.Expect(err).NotTo(HaveOccurred(),
		"an unregistered target cluster is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	var got horizonv1alpha1.Horizon
	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: "default", Name: "test-horizon"}, &got)).To(Succeed())

	cond := conditions.GetCondition(got.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil(), "the first gate condition must carry the failure")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	// Nothing was projected: the pass returned before any sub-reconciler could
	// write, and the management cluster is the only one a failing resolver could
	// have handed back.
	var deployments appsv1.DeploymentList
	g.Expect(r.List(ctx, &deployments)).To(Succeed())
	g.Expect(deployments.Items).To(BeEmpty())
	var configMaps corev1.ConfigMapList
	g.Expect(r.List(ctx, &configMaps)).To(Succeed())
	g.Expect(configMaps.Items).To(BeEmpty())
	var secrets corev1.SecretList
	g.Expect(r.List(ctx, &secrets)).To(Succeed())
	g.Expect(secrets.Items).To(HaveLen(1), "only the seeded SECRET_KEY Secret may exist")
}

// TestUpdateStatus_SkipsWriteWhenUnchanged verifies the C3 gate: when the
// snapshot equals the status updateStatus computes, no Status().Update is
// issued. The reconciler's Status().Update is wired to always fail, so a
// skipped write is observable as a nil error return.
func TestUpdateStatus_SkipsWriteWhenUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)

	statusErr := fmt.Errorf("status update must not be called on an unchanged status")
	r, h := newUpdateStatusReconciler(t, testHorizon(), statusErr)

	// Bring h.Status into the exact state updateStatus would compute (Ready
	// aggregated + ObservedGeneration stamped), then snapshot it — a converged
	// steady-state pass.
	setReadyCondition(h)
	h.Status.ObservedGeneration = h.Generation
	snapshot := h.Status.DeepCopy()

	_, err := r.updateStatus(context.Background(), h, snapshot, ctrl.Result{}, nil)
	g.Expect(err).NotTo(HaveOccurred(),
		"an unchanged status must skip the write; the failing Status().Update proves it was not called")
}

// --- remote children -------------------------------------------------------

// horizonKey is the namespaced name of the CR every fixture in this file builds.
var horizonKey = types.NamespacedName{Namespace: "default", Name: "test-horizon"}

// ownedRemoteChild stamps the ownership labels the projection wrote on a child
// it put on the target cluster, so the sweep selects it exactly as it would
// there.
func ownedRemoteChild(t *testing.T, owner, child client.Object) client.Object {
	t.Helper()
	labels, err := commonmulticluster.OwnerLabels(testScheme(), owner)
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	child.SetLabels(labels)
	return child
}

// terminatingRemoteHorizon returns a Horizon that names a target cluster and is
// being deleted, carrying the remote-children finalizer plus a foreign one so
// the CR survives the release and the test can read back which finalizers the
// pass dropped.
func terminatingRemoteHorizon(t *testing.T) (*HorizonReconciler, *horizonv1alpha1.Horizon) {
	t.Helper()
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	h.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer, "foreign.example.com/keep-alive"}
	deletedAt := metav1.NewTime(time.Now())
	h.DeletionTimestamp = &deletedAt

	r := newTestReconciler(testScheme(), h)
	var terminating horizonv1alpha1.Horizon
	g.Expect(r.Get(context.Background(), horizonKey, &terminating)).To(Succeed())
	return r, &terminating
}

// getHorizon reads the CR back from the reconciler's client.
func getHorizon(t *testing.T, r *HorizonReconciler) *horizonv1alpha1.Horizon {
	t.Helper()
	var got horizonv1alpha1.Horizon
	NewGomegaWithT(t).Expect(r.Get(context.Background(), horizonKey, &got)).To(Succeed())
	return &got
}

// TestReconcileDeleteRemoteChildren_SweepsEveryProjectedKind is the whole point
// of the finalizer: no garbage collection cascade crosses the cluster boundary,
// so the dashboard projection has to be deleted by name from here, across every
// API group it lives in. What the sweep leaves standing matters as much: a
// target cluster carries other people's objects, and an object nobody claimed or
// another CR claimed is not ours to remove.
func TestReconcileDeleteRemoteChildren_SweepsEveryProjectedKind(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteHorizon(t)

	projected := []client.Object{
		ownedRemoteChild(t, terminating,
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-horizon", Namespace: "default"}}),
		ownedRemoteChild(t, terminating,
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-horizon", Namespace: "default"}}),
		ownedRemoteChild(t, terminating,
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "test-horizon-config", Namespace: "default"}}),
		ownedRemoteChild(t, terminating,
			&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "test-horizon", Namespace: "default"}}),
		ownedRemoteChild(t, terminating,
			&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "test-horizon", Namespace: "default"}}),
		ownedRemoteChild(t, terminating,
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "test-horizon", Namespace: "default"}}),
		ownedRemoteChild(t, terminating,
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "test-horizon", Namespace: "default"}}),
	}
	g.Expect(projected).To(HaveLen(len(HorizonRemoteChildKinds)),
		"every kind the sweep covers needs a child here, or the list grew untested")

	unlabelled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-ca", Namespace: "default"}}
	foreign := ownedRemoteChild(t,
		&horizonv1alpha1.Horizon{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "other-horizon", Namespace: "default"}})
	target := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(append([]client.Object{unlabelled, foreign}, projected...)...).Build()

	ctx := context.Background()
	g.Expect(r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)).To(Succeed())

	for _, child := range projected {
		err := target.Get(ctx, client.ObjectKeyFromObject(child), child.DeepCopyObject().(client.Object))
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s must be swept", child, child.GetName())
	}
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(unlabelled), &corev1.ConfigMap{})).To(Succeed(),
		"an object nobody claimed is nobody's child")
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(foreign), &appsv1.Deployment{})).To(Succeed(),
		"another Horizon's child must survive this one's teardown")

	g.Expect(getHorizon(t, r).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"),
		"a completed sweep must release the remote-children finalizer and nothing else")
}

// TestReconcileDeleteRemoteChildren_SweepFailureKeepsFinalizer is the guard
// against a CR that leaves etcd while its children keep running. A list the
// target cluster refuses says nothing about whether children exist, so the pass
// has to fail and sweep again on the next one.
func TestReconcileDeleteRemoteChildren_SweepFailureKeepsFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteHorizon(t)

	child := ownedRemoteChild(t, terminating,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-horizon", Namespace: "default"}})
	target := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(child).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if list.GetObjectKind().GroupVersionKind().Kind == "DeploymentList" {
					return apierrors.NewForbidden(appsv1.Resource("deployments"), "", nil)
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)

	g.Expect(err).To(MatchError(ContainSubstring("listing remote Deployment children for teardown")))
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(child), &appsv1.Deployment{})).To(Succeed())
	g.Expect(getHorizon(t, r).Finalizers).To(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
		"a failed sweep must keep the finalizer so the next pass retries")
}

// TestReconcile_TerminatingCR_UnresolvableTargetWaitsThenAbandons walks both
// halves of the deletion resolution. Cluster engagement is asynchronous, so a
// registered cluster does not resolve either while the provider is still syncing
// after an operator restart: the first pass has to wait and say so on the CR
// rather than give up on children that are perfectly reachable. Once the window
// is out the finalizer is released anyway, or a deregistered target cluster —
// a documented operation — would strand the CR in Terminating forever.
func TestReconcile_TerminatingCR_UnresolvableTargetWaitsThenAbandons(t *testing.T) {
	g := NewGomegaWithT(t)
	// The window this process has to sit out starts on its own first failure to
	// resolve, so it has to be compressed for the second pass to reach past it.
	abandonAfter := commonmulticluster.AbandonAfter
	t.Cleanup(func() { commonmulticluster.AbandonAfter = abandonAfter })
	commonmulticluster.AbandonAfter = time.Millisecond

	h := testHorizon()
	h.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "deregistered"}
	// A foreign finalizer keeps the CR in etcd so the test can read back which
	// finalizers the pass released.
	h.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer, "foreign.example.com/keep-alive"}
	// Terminating for far longer than the window, which by itself must not be
	// enough: giving up on a CR the moment the operator comes back would strand
	// its children.
	deletedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	h.DeletionTimestamp = &deletedAt
	// A child of this CR on the management cluster: an abandoning pass must
	// delete nothing anywhere, and the local client is the only one it holds.
	survivor := ownedRemoteChild(t, h,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-horizon", Namespace: "default"}})
	r := newTestReconciler(testScheme(), h, survivor)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: horizonKey}
	res, err := r.Reconcile(ctx, req)

	g.Expect(err).NotTo(HaveOccurred(), "a target cluster that is gone must not fail the deletion pass")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"the first pass this process fails to resolve on starts the window, it does not end it")
	waiting := getHorizon(t, r)
	g.Expect(waiting.Finalizers).To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
	cond := conditions.GetCondition(waiting.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil(), "a terminating CR waiting on its target cluster must say so on itself")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("before abandoning its children"))

	time.Sleep(10 * commonmulticluster.AbandonAfter)
	res, err = r.Reconcile(ctx, req)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "the deletion resolves once the window is out")
	g.Expect(getHorizon(t, r).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"),
		"an unreachable target cluster must not pin the CR forever")
	// The abandoned projection is announced rather than silently dropped.
	g.Expect(r.Recorder.(*record.FakeRecorder).Events).To(Receive(ContainSubstring("Warning RemoteChildrenAbandoned")))
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(survivor), &appsv1.Deployment{})).To(Succeed(),
		"an abandoning pass has no client to sweep with and must delete nothing")
}

// TestReconcile_InstallsRemoteChildrenFinalizerForATargetCluster verifies that a
// CR naming a target cluster is pinned before anything is projected onto it, so
// a deletion issued between this pass and the next still funnels through the
// sweep.
func TestReconcile_InstallsRemoteChildrenFinalizerForATargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	// A nil resolver keeps the children local, which is all this pass needs: the
	// finalizer decision reads spec.targetClusterRef, not the resolved client.
	h.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	r := newTestReconciler(testScheme(), h)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: horizonKey})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{Requeue: true}),
		"the pass installing the finalizer must requeue before any sub-reconciler runs")
	g.Expect(getHorizon(t, r).Finalizers).To(ConsistOf(commonmulticluster.RemoteChildrenFinalizer))
}

// TestReconcile_LocalCRDeletesInstantlyWithoutAFinalizer pins the other half.
// A CR that keeps its children on the management cluster has nothing for the
// sweep to do, and a finalizer it does not need is one more thing that can block
// its deletion — so it carries none and leaves etcd the moment it is deleted.
func TestReconcile_LocalCRDeletesInstantlyWithoutAFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon() // no spec.targetClusterRef: children stay local
	child := ownedRemoteChild(t, h,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-horizon", Namespace: "default"}})
	r := newTestReconciler(testScheme(), h, child)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: horizonKey}

	_, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getHorizon(t, r).Finalizers).To(BeEmpty(), "a local Horizon must stay finalizer-free")

	g.Expect(r.Delete(ctx, getHorizon(t, r))).To(Succeed())
	var gone horizonv1alpha1.Horizon
	g.Expect(apierrors.IsNotFound(r.Get(ctx, horizonKey, &gone))).To(BeTrue(),
		"nothing holds the CR, so the delete takes it out of etcd right away")

	res, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// The fake client runs no garbage collector, so the child standing here says
	// only one thing: the operator itself deleted nothing.
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(child), &appsv1.Deployment{})).To(Succeed(),
		"a local CR's children belong to the garbage collection cascade, not to a sweep")
}

func TestHorizonSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	h := testHorizon()
	g.Expect(horizonSecretNameExtractor(h)).To(Equal([]string{"horizon-secret-key"}))

	// Empty reference yields no index entries rather than an empty string.
	h.Spec.SecretKeyRef.Name = ""
	g.Expect(horizonSecretNameExtractor(h)).To(BeNil())

	// Wrong type is tolerated (nil, not panic).
	g.Expect(horizonSecretNameExtractor(&corev1.Secret{})).To(BeNil())
}
