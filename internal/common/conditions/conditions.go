// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package conditions

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetCondition upserts condition into conditions, delegating to
// meta.SetStatusCondition which enforces the Kubernetes API conventions
// (non-empty Reason, non-negative ObservedGeneration).
func SetCondition(conditions *[]metav1.Condition, condition metav1.Condition) {
	meta.SetStatusCondition(conditions, condition)
}

// GetCondition returns the condition with the given type, or nil if absent.
func GetCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(conditions, conditionType)
}

// IsReady returns true if a "Ready" condition exists with status True.
func IsReady(conditions []metav1.Condition) bool {
	c := meta.FindStatusCondition(conditions, "Ready")
	return c != nil && c.Status == metav1.ConditionTrue
}

// AllTrue returns true if all specified condition types exist and have status True.
// Returns true for an empty types list (vacuous truth).
func AllTrue(conditions []metav1.Condition, types ...string) bool {
	for _, ct := range types {
		c := meta.FindStatusCondition(conditions, ct)
		if c == nil || c.Status != metav1.ConditionTrue {
			return false
		}
	}
	return true
}
