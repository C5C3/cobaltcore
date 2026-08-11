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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
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
