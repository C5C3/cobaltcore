package deployment

import (
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func int32Ptr(i int32) *int32 { return &i }

func TestIsDeploymentReady(t *testing.T) {
	tests := []struct {
		name   string
		deploy *appsv1.Deployment
		want   bool
	}{
		{
			name:   "nil deployment returns false",
			deploy: nil,
			want:   false,
		},
		{
			name: "fully ready deployment (3/3 replicas)",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  1,
					Replicas:            3,
					UpdatedReplicas:     3,
					ReadyReplicas:       3,
					AvailableReplicas:   3,
					UnavailableReplicas: 0,
				},
			},
			want: true,
		},
		{
			name: "replicas spec nil defaults to 1, 1 ready",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: nil},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  1,
					Replicas:            1,
					UpdatedReplicas:     1,
					ReadyReplicas:       1,
					AvailableReplicas:   1,
					UnavailableReplicas: 0,
				},
			},
			want: true,
		},
		{
			name: "UpdatedReplicas less than desired (rolling update in progress)",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  2,
					Replicas:            3,
					UpdatedReplicas:     1,
					ReadyReplicas:       3,
					AvailableReplicas:   3,
					UnavailableReplicas: 0,
				},
			},
			want: false,
		},
		{
			name: "ReadyReplicas less than desired (pods not ready yet)",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  1,
					Replicas:            3,
					UpdatedReplicas:     3,
					ReadyReplicas:       2,
					AvailableReplicas:   3,
					UnavailableReplicas: 0,
				},
			},
			want: false,
		},
		{
			name: "AvailableReplicas less than desired",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  1,
					Replicas:            3,
					UpdatedReplicas:     3,
					ReadyReplicas:       3,
					AvailableReplicas:   2,
					UnavailableReplicas: 0,
				},
			},
			want: false,
		},
		{
			name: "UnavailableReplicas greater than zero",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  1,
					Replicas:            3,
					UpdatedReplicas:     3,
					ReadyReplicas:       3,
					AvailableReplicas:   3,
					UnavailableReplicas: 1,
				},
			},
			want: false,
		},
		{
			name: "ObservedGeneration less than Generation (stale status)",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 5},
				Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  4,
					Replicas:            3,
					UpdatedReplicas:     3,
					ReadyReplicas:       3,
					AvailableReplicas:   3,
					UnavailableReplicas: 0,
				},
			},
			want: false,
		},
		{
			name: "zero replicas desired, zero ready (scaled to zero)",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(0)},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  1,
					Replicas:            0,
					UpdatedReplicas:     0,
					ReadyReplicas:       0,
					AvailableReplicas:   0,
					UnavailableReplicas: 0,
				},
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(IsDeploymentReady(tc.deploy)).To(Equal(tc.want))
		})
	}
}
