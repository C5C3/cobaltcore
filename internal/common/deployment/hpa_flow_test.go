// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
)

func TestReconcileHPA_disabledDeletes(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	existing := testHPA()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()
	var conds []metav1.Condition

	res, err := ReconcileHPA(context.Background(), c, s, testOwner(), HPAFlowParams{
		Enabled: false, Name: "test-hpa", Namespace: "default",
		Conditions: &conds, Generation: 4, ConditionType: "HPAReady",
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	err = c.Get(context.Background(), client.ObjectKey{Name: "test-hpa", Namespace: "default"}, &autoscalingv2.HorizontalPodAutoscaler{})
	g.Expect(err).To(HaveOccurred())
	cond := conditions.GetCondition(conds, "HPAReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonHPANotRequired))
	g.Expect(cond.Message).To(Equal("Autoscaling is not configured"))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(4)))
}

func TestReconcileHPA_enabledEnsures(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(testOwner()).Build()
	var conds []metav1.Condition

	res, err := ReconcileHPA(context.Background(), c, s, testOwner(), HPAFlowParams{
		Enabled: true, Desired: testHPA(), Name: "test-hpa", Namespace: "default",
		Conditions: &conds, Generation: 4, ConditionType: "HPAReady",
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	created := &autoscalingv2.HorizontalPodAutoscaler{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-hpa", Namespace: "default"}, created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	cond := conditions.GetCondition(conds, "HPAReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonHPAReady))
	g.Expect(cond.Message).To(Equal("HorizontalPodAutoscaler is configured"))
}
