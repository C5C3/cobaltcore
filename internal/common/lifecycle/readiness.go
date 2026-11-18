// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
)

// CurrentTrue reports whether this condition affirms the desired generation.
func CurrentTrue(conds []metav1.Condition, name string, generation int64) bool {
	c := conditions.GetCondition(conds, name)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == generation
}

// SetReady aggregates only observations of this generation. Report the first
// unmet gate in pipeline order so a waiting pass explains what it still needs.
func SetReady(conds *[]metav1.Condition, generation int64, required []string, passErr error) {
	ready := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		Reason:             "AllReady",
		Message:            "All sub-conditions are ready",
	}
	for _, name := range required {
		c := conditions.GetCondition(*conds, name)
		switch {
		case c == nil:
			ready.Message = name + " has not been reported"
		case c.ObservedGeneration != generation:
			ready.Message = fmt.Sprintf("%s last observed generation %d", name, c.ObservedGeneration)
		case c.Status != metav1.ConditionTrue:
			ready.Message = name + ": " + c.Reason
		default:
			continue
		}
		ready.Status = metav1.ConditionFalse
		ready.Reason = "NotAllReady"
		break
	}
	if passErr != nil && ready.Status == metav1.ConditionTrue {
		ready.Status = metav1.ConditionFalse
		ready.Reason = "ReconcileError"
		ready.Message = "The reconciliation pass failed"
	}
	conditions.SetCondition(conds, ready)
}

// StepFailed supplies a condition for a failed step that did not provide a
// more specific current failure. Keep raw client errors in the reconcile log.
func StepFailed(conds *[]metav1.Condition, generation int64, name, condition string, err error) {
	if err == nil {
		return
	}
	c := conditions.GetCondition(*conds, condition)
	if c != nil && c.ObservedGeneration == generation && c.Status == metav1.ConditionFalse && c.Reason != "ReconcileError" {
		return
	}
	conditions.SetCondition(conds, metav1.Condition{
		Type:               condition,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
		Reason:             "ReconcileError",
		Message:            name + " reconciliation failed",
	})
}
