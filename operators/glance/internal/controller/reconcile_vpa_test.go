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
	"k8s.io/apimachinery/pkg/types"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// The API Deployment is the only long-running workload of a Glance, so
// it is the only target, fed by spec.deployment and named like the Deployment.
func TestGlanceVPATargets(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testGlance()

	g.Expect(glanceVPATargets(cr)).To(Equal([]deployment.VPATarget{
		{Kind: "Deployment", Name: cr.Name},
	}), "a CR without the block lists the target without a spec")

	cr.Spec.Deployment.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	targets := glanceVPATargets(cr)
	g.Expect(targets).To(HaveLen(1))
	g.Expect(targets[0].Spec).To(BeIdenticalTo(cr.Spec.Deployment.VerticalAutoscaling))
}

func TestReconcileVPA_OptedInCreatesTheVPA(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testGlance()
	cr.Spec.Deployment.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newGlanceTestReconciler(cr)
	r.vpaAvailable = true

	_, err := r.reconcileVPA(context.Background(), r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var vpa vpav1.VerticalPodAutoscaler
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}, &vpa)).To(Succeed())
	g.Expect(vpa.Spec.TargetRef.Kind).To(Equal("Deployment"))
	g.Expect(vpa.Spec.TargetRef.Name).To(Equal(cr.Name))
	g.Expect(vpa.Labels).To(Equal(commonLabels(cr)))
	g.Expect(metav1.IsControlledBy(&vpa, cr)).To(BeTrue())
	cond := meta.FindStatusCondition(cr.Status.Conditions, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPAReady))
}

// On a management cluster without the VPA the latch is false: the opt-in is
// reported, the pass succeeds, and no VPA is attempted.
func TestReconcileVPA_LatchFalse_SetsNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testGlance()
	cr.Spec.Deployment.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newGlanceTestReconciler(cr)

	result, err := r.reconcileVPA(context.Background(), r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	cond := meta.FindStatusCondition(cr.Status.Conditions, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPANotInstalled))
	g.Expect(cond.Message).To(ContainSubstring("opted-in workloads: " + cr.Name))
	var list vpav1.VerticalPodAutoscalerList
	g.Expect(r.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}
