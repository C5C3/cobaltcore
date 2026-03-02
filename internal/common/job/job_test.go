package job

import (
	"testing"

	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestIsJobComplete(t *testing.T) {
	tests := []struct {
		name     string
		job      *batchv1.Job
		expected bool
	}{
		{
			name:     "nil job returns false",
			job:      nil,
			expected: false,
		},
		{
			name: "Complete=True returns true",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
			},
			expected: true,
		},
		{
			name: "Complete=False returns false",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
					},
				},
			},
			expected: false,
		},
		{
			name: "no conditions returns false",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{},
			},
			expected: false,
		},
		{
			name: "only Failed=True returns false",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
					},
				},
			},
			expected: false,
		},
		{
			name: "Complete=True and SuccessCriteriaMet=True returns true",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
			},
			expected: true,
		},
		{
			name: "Complete=True and Failed=True returns true",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
			},
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(IsJobComplete(tc.job)).To(Equal(tc.expected))
		})
	}
}
