//go:build !integration

package job_test

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c5c3/forge/internal/common/job"
)

func TestIsJobComplete(t *testing.T) {
	tests := []struct {
		name string
		job  *batchv1.Job
		want bool
	}{
		{
			name: "nil job returns false",
			job:  nil,
			want: false,
		},
		{
			name: "no conditions returns false",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job"},
				Status:     batchv1.JobStatus{},
			},
			want: false,
		},
		{
			name: "Complete=True returns true",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
			},
			want: true,
		},
		{
			name: "Complete=False returns false",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
					},
				},
			},
			want: false,
		},
		{
			name: "Complete=Unknown returns false",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionUnknown},
					},
				},
			},
			want: false,
		},
		{
			name: "Failed=True only returns false",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
					},
				},
			},
			want: false,
		},
		{
			name: "multiple conditions including Complete=True returns true",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionFalse},
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
			},
			want: true,
		},
		{
			name: "multiple conditions including Complete=False returns false",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionFalse},
						{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
					},
				},
			},
			want: false,
		},
		{
			name: "SuccessCriteriaMet=True but no Complete returns false",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := job.IsJobComplete(tt.job)
			if got != tt.want {
				t.Errorf("IsJobComplete() = %v, want %v", got, tt.want)
			}
		})
	}
}
