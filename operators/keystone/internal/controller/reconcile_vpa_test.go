// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
)

func vpaTestScheme() *runtime.Scheme {
	s := hpaTestScheme()
	_ = vpav1.AddToScheme(s)
	return s
}

func vpaTargetFake(s *runtime.Scheme, servesVPA bool, objs ...client.Object) client.Client {
	builder := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithTypeConverters(managedfields.NewDeducedTypeConverter())
	if servesVPA {
		return mctestutil.TargetFake(builder, deployment.VPAGVK)
	}
	return mctestutil.TargetFake(builder)
}

func withVerticalAutoscaling(ks *keystonev1alpha1.Keystone) *keystonev1alpha1.Keystone {
	ks.Spec.Deployment.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	return ks
}

// The API Deployment is Keystone's only long-running workload, so it is the
// only target, fed by spec.deployment and named like the Deployment.
func TestKeystoneVPATargets(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hpaTestKeystone()

	g.Expect(keystoneVPATargets(ks)).To(Equal([]deployment.VPATarget{
		{Kind: "Deployment", Name: "test-keystone"},
	}), "a CR without the block lists the target without a spec")

	withVerticalAutoscaling(ks)
	targets := keystoneVPATargets(ks)
	g.Expect(targets).To(HaveLen(1))
	g.Expect(targets[0].Spec).To(BeIdenticalTo(ks.Spec.Deployment.VerticalAutoscaling))
}

func TestReconcileVPA_OptedInCreatesTheVPA(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaTestScheme()
	ks := withVerticalAutoscaling(hpaTestKeystone())
	r := newHPATestReconciler(s, ks)
	r.vpaAvailable = true

	_, err := r.reconcileVPA(context.Background(), r.Client, ks)
	g.Expect(err).NotTo(HaveOccurred())

	var vpa vpav1.VerticalPodAutoscaler
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: "test-keystone", Namespace: "default"}, &vpa)).To(Succeed())
	g.Expect(vpa.Spec.TargetRef.Kind).To(Equal("Deployment"))
	g.Expect(vpa.Labels).To(Equal(commonLabels(ks)))
	g.Expect(metav1.IsControlledBy(&vpa, ks)).To(BeTrue())
	cond := meta.FindStatusCondition(ks.Status.Conditions, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPAReady))
}

// On a management cluster without the VPA the latch is false: the opt-in is
// reported, the pass succeeds, and no VPA is attempted.
func TestReconcileVPA_LatchFalse_SetsNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaTestScheme()
	ks := withVerticalAutoscaling(hpaTestKeystone())
	r := newHPATestReconciler(s, ks)
	r.vpaAvailable = false

	result, err := r.reconcileVPA(context.Background(), r.Client, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPANotInstalled))
	g.Expect(cond.Message).To(ContainSubstring("opted-in workloads: test-keystone"))
	var list vpav1.VerticalPodAutoscalerList
	g.Expect(r.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// The management cluster has no VPA and the target cluster has, so only the
// target's own answer can produce the VPA.
func TestReconcileVPA_RemoteChildrenServeTheKindDespiteTheLatch(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaTestScheme()
	ks := withVerticalAutoscaling(hpaTestKeystone())
	r := newHPATestReconciler(s, ks)
	r.vpaAvailable = false
	target := vpaTargetFake(s, true)

	_, err := r.reconcileVPA(context.Background(), mctestutil.RemoteChildren(t, r.Client, target), ks)
	g.Expect(err).NotTo(HaveOccurred())

	var vpa vpav1.VerticalPodAutoscaler
	g.Expect(target.Get(context.Background(), types.NamespacedName{Name: "test-keystone", Namespace: "default"}, &vpa)).
		To(Succeed(), "the VPA belongs on the cluster the children are written to")
	g.Expect(vpa.OwnerReferences).To(BeEmpty())
	g.Expect(vpa.Labels).To(HaveKeyWithValue(commonmulticluster.OwnerKindLabel, "Keystone"))
}

// The latch says the management cluster serves the kind and the target does
// not; the message names the target cluster, which a restart does not fix.
func TestReconcileVPA_RemoteChildrenWithoutTheKindNameTheTargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaTestScheme()
	ks := withVerticalAutoscaling(hpaTestKeystone())
	r := newHPATestReconciler(s, ks)
	r.vpaAvailable = true

	_, err := r.reconcileVPA(context.Background(), mctestutil.RemoteChildren(t, r.Client, vpaTargetFake(s, false)), ks)
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPANotInstalled))
	g.Expect(cond.Message).To(ContainSubstring("target cluster does not serve"))
	g.Expect(cond.Message).NotTo(ContainSubstring("restart"))
}

// A probe that fails establishes nothing: the pass fails and the CR says why.
func TestReconcileVPA_RemoteProbeFailureSurfacesCapabilityProbeFailed(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaTestScheme()
	ks := withVerticalAutoscaling(hpaTestKeystone())
	r := newHPATestReconciler(s, ks)

	_, err := r.reconcileVPA(context.Background(), mctestutil.UnprobeableChildren(r.Client), ks)
	g.Expect(err).To(MatchError(ContainSubstring("probing the target cluster for the VerticalPodAutoscaler kind")))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.CapabilityProbeFailed))
}
