// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
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

// An HPA the API server refuses (a behavior that bypassed the webhook) must
// surface as a reconcile error and leave the condition slice as it was, so
// the last reported state stays visible until the next attempt.
func TestReconcileHPA_applyRejectedReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(testOwner()).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: "autoscaling", Kind: "HorizontalPodAutoscaler"}, "test-hpa",
					field.ErrorList{field.Invalid(
						field.NewPath("spec", "behavior", "scaleDown", "stabilizationWindowSeconds"),
						int32(3601), "must be less than or equal to 3600")},
				)
			},
		}).
		Build()
	conds := []metav1.Condition{{
		Type: "HPAReady", Status: metav1.ConditionTrue, Reason: ReasonHPAReady,
		Message: "HorizontalPodAutoscaler is configured", ObservedGeneration: 3,
	}}
	before := append([]metav1.Condition(nil), conds...)

	res, err := ReconcileHPA(context.Background(), c, s, testOwner(), HPAFlowParams{
		Enabled: true, Desired: testHPA(), Name: "test-hpa", Namespace: "default",
		Conditions: &conds, Generation: 4, ConditionType: "HPAReady",
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ensuring HorizontalPodAutoscaler:"))
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(conds).To(Equal(before))
}
