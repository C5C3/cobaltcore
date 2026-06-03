// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the ControlPlane controller skeleton (CC-0110, REQ-007).
package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// controllerTestScheme returns a runtime.Scheme with the c5c3 ControlPlane type
// registered alongside the core client-go types.
func controllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	return s
}

// trueCondition returns a metav1.Condition of the given type with status True.
func trueCondition(condType string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            "ready",
		LastTransitionTime: metav1.Now(),
	}
}

func TestAggregateReady_AllTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	conds := make([]metav1.Condition, 0, len(subConditionTypes))
	for _, ct := range subConditionTypes {
		conds = append(conds, trueCondition(ct))
	}

	g.Expect(conditions.AllTrue(conds, subConditionTypes...)).To(BeTrue(),
		"aggregate Ready must be true when all sub-conditions are True")
}

func TestAggregateReady_MissingCondition(t *testing.T) {
	g := NewGomegaWithT(t)

	// Drop the last sub-condition so one is missing entirely.
	conds := make([]metav1.Condition, 0, len(subConditionTypes)-1)
	for _, ct := range subConditionTypes[:len(subConditionTypes)-1] {
		conds = append(conds, trueCondition(ct))
	}

	g.Expect(conditions.AllTrue(conds, subConditionTypes...)).To(BeFalse(),
		"aggregate Ready must be false when a sub-condition is missing")
}

func TestAggregateReady_OneFalse(t *testing.T) {
	g := NewGomegaWithT(t)

	conds := make([]metav1.Condition, 0, len(subConditionTypes))
	for i, ct := range subConditionTypes {
		c := trueCondition(ct)
		if i == 0 {
			c.Status = metav1.ConditionFalse
		}
		conds = append(conds, c)
	}

	g.Expect(conditions.AllTrue(conds, subConditionTypes...)).To(BeFalse(),
		"aggregate Ready must be false when any sub-condition is False")
}

func TestReconcile_NotFound_EarlyReturn(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()

	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "absent"},
	})

	g.Expect(err).NotTo(HaveOccurred(),
		"Reconcile on a missing ControlPlane must not error")
	g.Expect(res).To(Equal(ctrl.Result{}),
		"Reconcile on a missing ControlPlane must return a zero result")
}
