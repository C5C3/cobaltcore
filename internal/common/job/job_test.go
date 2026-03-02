package job

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestIsJobComplete(t *testing.T) {
	tests := []struct {
		name string
		job  *batchv1.Job
		want bool
	}{
		{
			name: "complete job",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
			},
			want: true,
		},
		{
			name: "no conditions",
			job:  &batchv1.Job{},
			want: false,
		},
		{
			name: "Complete=False",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
					},
				},
			},
			want: false,
		},
		{
			name: "only SuccessCriteriaMet",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
					},
				},
			},
			want: false,
		},
		{
			name: "failed job",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
					},
				},
			},
			want: false,
		},
		{
			name: "nil job",
			job:  nil,
			want: false,
		},
		{
			name: "multiple conditions including Complete=True",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
			},
			want: true,
		},
		{
			name: "Complete=Unknown",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionUnknown},
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsJobComplete(tt.job)
			if got != tt.want {
				t.Errorf("IsJobComplete() = %v, want %v", got, tt.want)
			}
		})
	}
}
