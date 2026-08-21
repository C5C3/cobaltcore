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

	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

func barbicanAutoscalingSpec(minR, maxR int32) *barbicanv1alpha1.AutoscalingSpec {
	return &barbicanv1alpha1.AutoscalingSpec{
		MinReplicas:          ptr.To(minR),
		MaxReplicas:          maxR,
		TargetCPUUtilization: ptr.To(int32(80)),
	}
}

func TestReconcileHPA_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	stale := &autoscalingv2.HorizontalPodAutoscaler{}
	stale.Name = testBarbicanName
	stale.Namespace = testNamespace
	r := newBarbicanTestReconciler(barbican, stale)

	res, err := r.reconcileHPA(context.Background(), r.Client, barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := barbicanCondition(barbican, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPANotRequired"))

	var gone autoscalingv2.HorizontalPodAutoscaler
	err = r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testBarbicanName}, &gone)
	g.Expect(err).To(HaveOccurred(), "stale HPA must be deleted when autoscaling is disabled")
}

func TestReconcileHPA_EnabledCreatesHPA(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.Autoscaling = barbicanAutoscalingSpec(2, 5)
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileHPA(context.Background(), r.Client, barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	var hpa autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testBarbicanName}, &hpa)).To(Succeed())
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(testBarbicanName))
	g.Expect(hpa.Spec.MinReplicas).To(HaveValue(Equal(int32(2))))
	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(5)))

	cond := barbicanCondition(barbican, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPAReady"))

	// The Deployment leaves replicas unset while the HPA owns the count, so the
	// two controllers do not fight over it every reconcile.
	g.Expect(buildBarbicanDeployment(barbican, validProjection(), deploymentConfigSecretName, "", "").Spec.Replicas).To(BeNil())
}
