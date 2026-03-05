// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package deployment

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// Feature: CC-0005

func ptr[T any](v T) *T { return &v }

// createNamespace creates a unique namespace in the cluster for test isolation.
func createNamespace(ctx context.Context, g Gomega, c client.Client, name string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	return ns
}

// createOwner creates a ConfigMap via the API server so it gets a real UID assigned.
func createOwner(ctx context.Context, g Gomega, c client.Client, namespace string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: namespace,
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())
	return cm
}

// ---------------------------------------------------------------------------
// TestIntegration_EnsureDeployment_CreatesInCluster
// ---------------------------------------------------------------------------

func TestIntegration_EnsureDeployment_CreatesInCluster(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-deploy-create")
	owner := createOwner(ctx, g, c, ns.Name)

	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "test"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
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

	ready, err := EnsureDeployment(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "should not be ready since no pods are running in envtest")

	// Verify the Deployment exists in the cluster.
	created := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-deploy", Namespace: ns.Name}, created)).To(Succeed())

	// Verify spec correctness.
	g.Expect(created.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:latest"))
	g.Expect(*created.Spec.Replicas).To(Equal(int32(1)))

	// Verify owner reference is set.
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(owner.UID))
}

// ---------------------------------------------------------------------------
// TestIntegration_EnsureDeployment_UpdatesExisting
// ---------------------------------------------------------------------------

func TestIntegration_EnsureDeployment_UpdatesExisting(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-deploy-update")
	owner := createOwner(ctx, g, c, ns.Name)

	// Create initial Deployment with nginx:1.24.
	initial := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "test"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx:1.24"},
					},
				},
			},
		},
	}

	_, err := EnsureDeployment(ctx, c, owner, initial)
	g.Expect(err).NotTo(HaveOccurred())

	// Update the Deployment to nginx:1.25.
	updated := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "test"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx:1.25"},
					},
				},
			},
		},
	}

	_, err = EnsureDeployment(ctx, c, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the Deployment was updated with the new image.
	result := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-deploy", Namespace: ns.Name}, result)).To(Succeed())
	g.Expect(result.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.25"))
}

// ---------------------------------------------------------------------------
// TestIntegration_EnsureService_CreatesInCluster
// ---------------------------------------------------------------------------

func TestIntegration_EnsureService_CreatesInCluster(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-svc-create")
	owner := createOwner(ctx, g, c, ns.Name)

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					TargetPort: intstr.FromInt32(8080),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	err := EnsureService(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the Service exists in the cluster.
	created := &corev1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-svc", Namespace: ns.Name}, created)).To(Succeed())

	// Verify correct ports, selector, and type.
	g.Expect(created.Spec.Ports).To(HaveLen(1))
	g.Expect(created.Spec.Ports[0].Port).To(Equal(int32(80)))
	g.Expect(created.Spec.Ports[0].TargetPort).To(Equal(intstr.FromInt32(8080)))
	g.Expect(created.Spec.Selector).To(HaveKeyWithValue("app", "test"))
	g.Expect(created.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))

	// Verify owner reference.
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(owner.UID))
}

// ---------------------------------------------------------------------------
// TestIntegration_EnsureService_UpdatesExisting
// ---------------------------------------------------------------------------

func TestIntegration_EnsureService_UpdatesExisting(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-svc-update")
	owner := createOwner(ctx, g, c, ns.Name)

	// Create initial Service with port 80.
	initial := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Port:     80,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}

	g.Expect(EnsureService(ctx, c, owner, initial)).To(Succeed())

	// Update the Service to port 8080.
	updated := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Port:     8080,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}

	g.Expect(EnsureService(ctx, c, owner, updated)).To(Succeed())

	// Verify the Service was updated with the new port.
	result := &corev1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-svc", Namespace: ns.Name}, result)).To(Succeed())
	g.Expect(result.Spec.Ports[0].Port).To(Equal(int32(8080)))
}

// ---------------------------------------------------------------------------
// TestIntegration_EnsureService_PreservesClusterIP
// ---------------------------------------------------------------------------

func TestIntegration_EnsureService_PreservesClusterIP(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-svc-clusterip")
	owner := createOwner(ctx, g, c, ns.Name)

	// Create a service so the API server assigns a ClusterIP.
	initial := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Port:     80,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}

	g.Expect(EnsureService(ctx, c, owner, initial)).To(Succeed())

	// Read back the service to capture the ClusterIP assigned by the API server.
	existing := &corev1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-svc", Namespace: ns.Name}, existing)).To(Succeed())
	originalClusterIP := existing.Spec.ClusterIP
	g.Expect(originalClusterIP).NotTo(BeEmpty(), "API server should assign a ClusterIP")

	// Call EnsureService again with different ports but no ClusterIP set.
	modified := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Port:     9090,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}

	g.Expect(EnsureService(ctx, c, owner, modified)).To(Succeed())

	// Verify the ClusterIP was preserved.
	result := &corev1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-svc", Namespace: ns.Name}, result)).To(Succeed())
	g.Expect(result.Spec.ClusterIP).To(Equal(originalClusterIP), "ClusterIP must be preserved across updates")
	// Port should be updated.
	g.Expect(result.Spec.Ports[0].Port).To(Equal(int32(9090)))
}

// ---------------------------------------------------------------------------
// TestIntegration_IsDeploymentReady_RealCluster
// ---------------------------------------------------------------------------

func TestIntegration_IsDeploymentReady_RealCluster(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-deploy-ready")

	// Create a Deployment directly via the API.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "test"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
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
	g.Expect(c.Create(ctx, deploy)).To(Succeed())

	// Re-read the deployment from the cluster to get its full status.
	fetched := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-deploy", Namespace: ns.Name}, fetched)).To(Succeed())

	// In envtest there is no kubelet, so no pods become ready.
	// IsDeploymentReady must return false.
	g.Expect(IsDeploymentReady(fetched)).To(BeFalse(),
		"should return false because envtest has no kubelet to run pods")
}
