// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

// Feature: CC-0005

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// helper to build a scheme with all core types.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return s
}

// helper to build a fake client.
func testClient(scheme *runtime.Scheme, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// helper to create an owner ConfigMap with UID and GVK set.
func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       types.UID("owner-uid-1234"),
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
	}
}

func int32Ptr(i int32) *int32 { return &i }

// ---------------------------------------------------------------------------
// TestIsDeploymentReady – table-driven
// ---------------------------------------------------------------------------

func TestIsDeploymentReady(t *testing.T) {
	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		want       bool
	}{
		{
			name: "all replicas available",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
				},
				Status: appsv1.DeploymentStatus{
					AvailableReplicas: 3,
					ReadyReplicas:     3,
					UpdatedReplicas:   3,
				},
			},
			want: true,
		},
		{
			name: "zero available replicas",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
				},
				Status: appsv1.DeploymentStatus{
					AvailableReplicas: 0,
					ReadyReplicas:     0,
					UpdatedReplicas:   0,
				},
			},
			want: false,
		},
		{
			name: "partial replicas",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
				},
				Status: appsv1.DeploymentStatus{
					AvailableReplicas: 2,
					ReadyReplicas:     2,
					UpdatedReplicas:   3,
				},
			},
			want: false,
		},
		{
			name: "nil replicas spec with 1 available – defaults to 1",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: nil,
				},
				Status: appsv1.DeploymentStatus{
					AvailableReplicas: 1,
					ReadyReplicas:     1,
					UpdatedReplicas:   1,
				},
			},
			want: true,
		},
		{
			name: "nil replicas spec with 0 available – defaults to 1",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: nil,
				},
				Status: appsv1.DeploymentStatus{
					AvailableReplicas: 0,
					ReadyReplicas:     0,
					UpdatedReplicas:   0,
				},
			},
			want: false,
		},
		{
			name: "updated replicas less than desired",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
				},
				Status: appsv1.DeploymentStatus{
					AvailableReplicas: 3,
					ReadyReplicas:     3,
					UpdatedReplicas:   2,
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(IsDeploymentReady(tt.deployment)).To(Equal(tt.want))
		})
	}
}

// ---------------------------------------------------------------------------
// EnsureDeployment tests
// ---------------------------------------------------------------------------

func TestEnsureDeployment_createsDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	_, err := EnsureDeployment(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the Deployment was created.
	created := &appsv1.Deployment{}
	err = c.Get(ctx, client.ObjectKeyFromObject(desired), created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:latest"))
}

func TestEnsureDeployment_updatesDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	ctx := context.Background()
	owner := testOwner()

	existing := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx:1.0"},
					},
				},
			},
		},
	}
	c := testClient(scheme, existing)

	updated := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(2),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx:2.0"},
					},
				},
			},
		},
	}

	_, err := EnsureDeployment(ctx, c, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the Deployment was updated.
	result := &appsv1.Deployment{}
	err = c.Get(ctx, client.ObjectKeyFromObject(updated), result)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:2.0"))
	g.Expect(*result.Spec.Replicas).To(Equal(int32(2)))
}

func TestEnsureDeployment_setsOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	_, err := EnsureDeployment(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	created := &appsv1.Deployment{}
	err = c.Get(ctx, client.ObjectKeyFromObject(desired), created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(types.UID("owner-uid-1234")))
}

func TestEnsureDeployment_returnsFalse_whenNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(3),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	// Fake client won't set status, so the deployment will have 0 replicas ready.
	ready, err := EnsureDeployment(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

// ---------------------------------------------------------------------------
// EnsureService tests
// ---------------------------------------------------------------------------

func TestEnsureService_createsService(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}

	err := EnsureService(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	created := &corev1.Service{}
	err = c.Get(ctx, client.ObjectKeyFromObject(desired), created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.Spec.Ports).To(HaveLen(1))
	g.Expect(created.Spec.Ports[0].Port).To(Equal(int32(80)))
	g.Expect(created.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
}

func TestEnsureService_updatesService(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	ctx := context.Background()
	owner := testOwner()

	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}
	c := testClient(scheme, existing)

	updated := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "default",
			Labels:    map[string]string{"app": "test-v2"},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "test-v2"},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP},
			},
		},
	}

	err := EnsureService(ctx, c, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	result := &corev1.Service{}
	err = c.Get(ctx, client.ObjectKeyFromObject(updated), result)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Spec.Ports[0].Port).To(Equal(int32(8080)))
	g.Expect(result.Spec.Selector["app"]).To(Equal("test-v2"))
	g.Expect(result.Labels["app"]).To(Equal("test-v2"))
}

func TestEnsureService_setsOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}

	err := EnsureService(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	created := &corev1.Service{}
	err = c.Get(ctx, client.ObjectKeyFromObject(desired), created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(types.UID("owner-uid-1234")))
}

func TestEnsureService_preservesClusterIP(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	ctx := context.Background()
	owner := testOwner()

	// Simulate an existing service with a ClusterIP assigned by the API server.
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type:       corev1.ServiceTypeClusterIP,
			ClusterIP:  "10.96.0.100",
			ClusterIPs: []string{"10.96.0.100"},
			Selector:   map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}
	c := testClient(scheme, existing)

	// Desired service does NOT set ClusterIP (callers never should).
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP},
			},
		},
	}

	err := EnsureService(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	result := &corev1.Service{}
	err = c.Get(ctx, client.ObjectKeyFromObject(desired), result)
	g.Expect(err).NotTo(HaveOccurred())

	// ClusterIP must be preserved.
	g.Expect(result.Spec.ClusterIP).To(Equal("10.96.0.100"))
	g.Expect(result.Spec.ClusterIPs).To(Equal([]string{"10.96.0.100"}))
	// Port should be updated.
	g.Expect(result.Spec.Ports[0].Port).To(Equal(int32(8080)))
}
