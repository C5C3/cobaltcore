// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/c5c3/forge/internal/common/conditions"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

func glanceAutoscalingSpec(minR, maxR int32) *glancev1alpha1.AutoscalingSpec {
	return &glancev1alpha1.AutoscalingSpec{
		MinReplicas:          ptr.To(minR),
		MaxReplicas:          maxR,
		TargetCPUUtilization: ptr.To(int32(80)),
	}
}

func TestReconcileHPA_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	stale := &autoscalingv2.HorizontalPodAutoscaler{}
	stale.Name = "test-glance"
	stale.Namespace = "default"
	r := newGlanceTestReconciler(glance, stale)

	res, err := r.reconcileHPA(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(glance.Status.Conditions, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPANotRequired"))

	var gone autoscalingv2.HorizontalPodAutoscaler
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"}, &gone)
	g.Expect(err).To(HaveOccurred(), "stale HPA must be deleted when autoscaling is disabled")
}

func TestReconcileHPA_EnabledCreatesHPA(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Autoscaling = glanceAutoscalingSpec(2, 5)
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileHPA(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	var hpa autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"}, &hpa)).To(Succeed())
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal("test-glance"))
	g.Expect(hpa.Spec.MinReplicas).To(HaveValue(Equal(int32(2))))
	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(5)))

	cond := conditions.GetCondition(glance.Status.Conditions, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPAReady"))
}
