//go:build !integration

package deployment_test

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c5c3/forge/internal/common/deployment"
)

func TestIsDeploymentReady(t *testing.T) {
	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		want       bool
	}{
		{
			name:       "nil deployment returns false",
			deployment: nil,
			want:       false,
		},
		{
			name: "no conditions returns false",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Status:     appsv1.DeploymentStatus{},
			},
			want: false,
		},
		{
			name: "Available=True returns true",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Status: appsv1.DeploymentStatus{
					Conditions: []appsv1.DeploymentCondition{
						{
							Type:   appsv1.DeploymentAvailable,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			want: true,
		},
		{
			name: "Available=False returns false",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Status: appsv1.DeploymentStatus{
					Conditions: []appsv1.DeploymentCondition{
						{
							Type:   appsv1.DeploymentAvailable,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			want: false,
		},
		{
			name: "Available=Unknown returns false",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Status: appsv1.DeploymentStatus{
					Conditions: []appsv1.DeploymentCondition{
						{
							Type:   appsv1.DeploymentAvailable,
							Status: corev1.ConditionUnknown,
						},
					},
				},
			},
			want: false,
		},
		{
			name: "other conditions only returns false",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Status: appsv1.DeploymentStatus{
					Conditions: []appsv1.DeploymentCondition{
						{
							Type:   appsv1.DeploymentProgressing,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			want: false,
		},
		{
			name: "multiple conditions with Available=True returns true",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Status: appsv1.DeploymentStatus{
					Conditions: []appsv1.DeploymentCondition{
						{
							Type:   appsv1.DeploymentProgressing,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   appsv1.DeploymentAvailable,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			want: true,
		},
		{
			name: "multiple conditions with Available=False returns false",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Status: appsv1.DeploymentStatus{
					Conditions: []appsv1.DeploymentCondition{
						{
							Type:   appsv1.DeploymentProgressing,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   appsv1.DeploymentAvailable,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deployment.IsDeploymentReady(tt.deployment)
			if got != tt.want {
				t.Errorf("IsDeploymentReady() = %v, want %v", got, tt.want)
			}
		})
	}
}
