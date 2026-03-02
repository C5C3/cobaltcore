package job

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// IsJobComplete returns true if the given Job has a "Complete" condition with
// status "True". This indicates the Job has successfully finished all of its
// work. (CC-0005, REQ-006)
func IsJobComplete(j *batchv1.Job) bool {
	if j == nil {
		return false
	}
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
