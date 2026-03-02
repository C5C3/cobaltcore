package deployment

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func ptr[T any](v T) *T { return &v }

func TestIsDeploymentReady(t *testing.T) {
	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		want       bool
	}{
		{
			name: "available deployment",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr(int32(3)),
				},
				Status: appsv1.DeploymentStatus{
					ReadyReplicas: 3,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
					},
				},
			},
			want: true,
		},
		{
			name: "no status",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr(int32(3)),
				},
			},
			want: false,
		},
		{
			name: "Available=False",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr(int32(3)),
				},
				Status: appsv1.DeploymentStatus{
					ReadyReplicas: 3,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse},
					},
				},
			},
			want: false,
		},
		{
			name: "readyReplicas less than desired",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr(int32(3)),
				},
				Status: appsv1.DeploymentStatus{
					ReadyReplicas: 1,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
					},
				},
			},
			want: false,
		},
		{
			name: "nil replicas",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{},
			},
			want: false,
		},
		{
			name:       "nil deployment",
			deployment: nil,
			want:       false,
		},
		{
			name: "no Available condition",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr(int32(3)),
				},
				Status: appsv1.DeploymentStatus{
					ReadyReplicas: 3,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue},
					},
				},
			},
			want: false,
		},
		{
			name: "zero replicas scaled down",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr(int32(0)),
				},
				Status: appsv1.DeploymentStatus{
					ReadyReplicas: 0,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsDeploymentReady(tt.deployment)
			if got != tt.want {
				t.Errorf("IsDeploymentReady() = %v, want %v", got, tt.want)
			}
		})
	}
}
