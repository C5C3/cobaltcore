package job

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// IsJobComplete returns true when the given Job has a Complete condition with
// status True. It is a pure function that inspects the in-memory object without
// making any API calls. (CC-0005 / REQ-006)
func IsJobComplete(job *batchv1.Job) bool {
	if job == nil {
		return false
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
