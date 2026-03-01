package conditions

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetCondition inserts or updates a condition by type. When the status is unchanged,
// LastTransitionTime is preserved. (CC-0004, REQ-003)
func SetCondition(conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string) {
	if conditions == nil {
		return
	}

	now := metav1.Now()

	for i := range *conditions {
		if (*conditions)[i].Type == conditionType {
			if (*conditions)[i].Status != status {
				(*conditions)[i].LastTransitionTime = now
			}
			(*conditions)[i].Status = status
			(*conditions)[i].Reason = reason
			(*conditions)[i].Message = message
			return
		}
	}

	*conditions = append(*conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// GetCondition returns the condition with the given type, or nil if not found.
func GetCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// IsReady returns true if a "Ready" condition exists with status True.
func IsReady(conditions []metav1.Condition) bool {
	c := GetCondition(conditions, "Ready")
	return c != nil && c.Status == metav1.ConditionTrue
}

// AllTrue returns true if every named condition type has status True.
// Returns true (vacuously) when no types are provided.
func AllTrue(conditions []metav1.Condition, types ...string) bool {
	for _, t := range types {
		c := GetCondition(conditions, t)
		if c == nil || c.Status != metav1.ConditionTrue {
			return false
		}
	}
	return true
}
