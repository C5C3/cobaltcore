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

	"github.com/c5c3/forge/internal/common/conditions"
)

func TestReconcileHPA_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	stale := &autoscalingv2.HorizontalPodAutoscaler{}
	stale.Name = "test-horizon"
	stale.Namespace = "default"
	r := newTestReconciler(testScheme(), h, stale)

	res, err := r.reconcileHPA(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(h.Status.Conditions, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPANotRequired"))

	var gone autoscalingv2.HorizontalPodAutoscaler
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-horizon"}, &gone)
	g.Expect(err).To(HaveOccurred(), "stale HPA must be deleted when autoscaling is disabled")
}

func TestReconcileHPA_EnabledCreatesHPA(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Autoscaling = autoscalingSpecWithCPU(2, 5)
	r := newTestReconciler(testScheme(), h)

	res, err := r.reconcileHPA(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	var hpa autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-horizon"}, &hpa)).To(Succeed())
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal("test-horizon"))
	g.Expect(hpa.Spec.MinReplicas).To(HaveValue(Equal(int32(2))))
	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(5)))

	cond := conditions.GetCondition(h.Status.Conditions, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPAReady"))
}
