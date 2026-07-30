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
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

func placementAutoscalingSpec(minR, maxR int32) *placementv1alpha1.AutoscalingSpec {
	return &placementv1alpha1.AutoscalingSpec{
		MinReplicas:          ptr.To(minR),
		MaxReplicas:          maxR,
		TargetCPUUtilization: ptr.To(int32(80)),
	}
}

func TestReconcileHPA_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	stale := &autoscalingv2.HorizontalPodAutoscaler{}
	stale.Name = "test-placement"
	stale.Namespace = "default"
	r := newPlacementTestReconciler(placement, stale)

	res, err := r.reconcileHPA(context.Background(), placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(placement.Status.Conditions, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPANotRequired"))

	var gone autoscalingv2.HorizontalPodAutoscaler
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-placement"}, &gone)
	g.Expect(err).To(HaveOccurred(), "stale HPA must be deleted when autoscaling is disabled")
}

func TestReconcileHPA_EnabledCreatesHPA(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.Autoscaling = placementAutoscalingSpec(2, 5)
	r := newPlacementTestReconciler(placement)

	res, err := r.reconcileHPA(context.Background(), placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	var hpa autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-placement"}, &hpa)).To(Succeed())
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal("test-placement"))
	g.Expect(hpa.Spec.MinReplicas).To(HaveValue(Equal(int32(2))))
	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(5)))

	cond := conditions.GetCondition(placement.Status.Conditions, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPAReady"))

	// The Deployment leaves replicas unset while the HPA owns the count, so the
	// two controllers do not fight over it every reconcile.
	g.Expect(buildPlacementDeployment(placement, "test-placement-config", "", "").Spec.Replicas).To(BeNil())
}
