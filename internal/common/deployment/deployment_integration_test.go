//go:build integration

package deployment_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/deployment"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var k8sClient client.Client

const testNamespace = "test-deployment"

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c

	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create test namespace: %v\n", err)
		teardown()
		os.Exit(1)
	}

	code := m.Run()
	teardown()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

func newTestDeployment(name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				"app": name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test",
							Image: "nginx:latest",
						},
					},
				},
			},
		},
	}
}

func newTestService(name string, port int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				"app": name,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": name,
			},
			Ports: []corev1.ServicePort{
				{
					Port:       port,
					TargetPort: intstr.FromInt32(port),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// EnsureDeployment tests
// ---------------------------------------------------------------------------

func TestEnsureDeployment_CreatesNewDeployment(t *testing.T) {
	ctx := context.Background()
	dep := newTestDeployment("test-create-deploy", int32(3))

	err := deployment.EnsureDeployment(ctx, k8sClient, dep)
	if err != nil {
		t.Fatalf("EnsureDeployment returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, dep)
	})

	fetched := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-create-deploy", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get Deployment: %v", err)
	}
	if *fetched.Spec.Replicas != int32(3) {
		t.Fatalf("expected replicas=3, got %d", *fetched.Spec.Replicas)
	}
	if fetched.Labels["app"] != "test-create-deploy" {
		t.Fatalf("expected label app=%q, got %q", "test-create-deploy", fetched.Labels["app"])
	}
}

func TestEnsureDeployment_UpdatesExistingDeployment(t *testing.T) {
	ctx := context.Background()
	dep := newTestDeployment("test-update-deploy", int32(3))

	err := deployment.EnsureDeployment(ctx, k8sClient, dep)
	if err != nil {
		t.Fatalf("first EnsureDeployment returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, dep)
	})

	// Modify replicas and call again.
	updated := newTestDeployment("test-update-deploy", int32(5))
	err = deployment.EnsureDeployment(ctx, k8sClient, updated)
	if err != nil {
		t.Fatalf("second EnsureDeployment returned error: %v", err)
	}

	fetched := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-update-deploy", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get Deployment: %v", err)
	}
	if *fetched.Spec.Replicas != int32(5) {
		t.Fatalf("expected replicas=5 after update, got %d", *fetched.Spec.Replicas)
	}
}

func TestEnsureDeployment_SetsOwnerReferences(t *testing.T) {
	ctx := context.Background()
	dep := newTestDeployment("test-ownerref-deploy", int32(3))
	dep.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Name:       "my-parent",
			UID:        "test-uid-12345",
		},
	}

	err := deployment.EnsureDeployment(ctx, k8sClient, dep)
	if err != nil {
		t.Fatalf("EnsureDeployment returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, dep)
	})

	fetched := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-ownerref-deploy", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get Deployment: %v", err)
	}

	refs := fetched.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(refs))
	}
	if refs[0].Name != "my-parent" {
		t.Fatalf("expected owner reference name=%q, got %q", "my-parent", refs[0].Name)
	}
	if refs[0].Kind != "Deployment" {
		t.Fatalf("expected owner reference kind=%q, got %q", "Deployment", refs[0].Kind)
	}
	if refs[0].APIVersion != "apps/v1" {
		t.Fatalf("expected owner reference apiVersion=%q, got %q", "apps/v1", refs[0].APIVersion)
	}
	if string(refs[0].UID) != "test-uid-12345" {
		t.Fatalf("expected owner reference uid=%q, got %q", "test-uid-12345", refs[0].UID)
	}
}

// ---------------------------------------------------------------------------
// EnsureService tests
// ---------------------------------------------------------------------------

func TestEnsureService_CreatesNewService(t *testing.T) {
	ctx := context.Background()
	svc := newTestService("test-create-svc", int32(8080))

	err := deployment.EnsureService(ctx, k8sClient, svc)
	if err != nil {
		t.Fatalf("EnsureService returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, svc)
	})

	fetched := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-create-svc", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get Service: %v", err)
	}
	if len(fetched.Spec.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(fetched.Spec.Ports))
	}
	if fetched.Spec.Ports[0].Port != int32(8080) {
		t.Fatalf("expected port=8080, got %d", fetched.Spec.Ports[0].Port)
	}
	if fetched.Labels["app"] != "test-create-svc" {
		t.Fatalf("expected label app=%q, got %q", "test-create-svc", fetched.Labels["app"])
	}
}

func TestEnsureService_UpdatesExistingService(t *testing.T) {
	ctx := context.Background()
	svc := newTestService("test-update-svc", int32(8080))

	err := deployment.EnsureService(ctx, k8sClient, svc)
	if err != nil {
		t.Fatalf("first EnsureService returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, svc)
	})

	// Modify port and call again.
	updated := newTestService("test-update-svc", int32(9090))
	err = deployment.EnsureService(ctx, k8sClient, updated)
	if err != nil {
		t.Fatalf("second EnsureService returned error: %v", err)
	}

	fetched := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-update-svc", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get Service: %v", err)
	}
	if fetched.Spec.Ports[0].Port != int32(9090) {
		t.Fatalf("expected port=9090 after update, got %d", fetched.Spec.Ports[0].Port)
	}
}

func TestEnsureService_SetsOwnerReferences(t *testing.T) {
	ctx := context.Background()
	svc := newTestService("test-ownerref-svc", int32(8080))
	svc.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Name:       "my-parent",
			UID:        "test-uid-67890",
		},
	}

	err := deployment.EnsureService(ctx, k8sClient, svc)
	if err != nil {
		t.Fatalf("EnsureService returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, svc)
	})

	fetched := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-ownerref-svc", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get Service: %v", err)
	}

	refs := fetched.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(refs))
	}
	if refs[0].Name != "my-parent" {
		t.Fatalf("expected owner reference name=%q, got %q", "my-parent", refs[0].Name)
	}
	if refs[0].Kind != "Deployment" {
		t.Fatalf("expected owner reference kind=%q, got %q", "Deployment", refs[0].Kind)
	}
	if refs[0].APIVersion != "apps/v1" {
		t.Fatalf("expected owner reference apiVersion=%q, got %q", "apps/v1", refs[0].APIVersion)
	}
	if string(refs[0].UID) != "test-uid-67890" {
		t.Fatalf("expected owner reference uid=%q, got %q", "test-uid-67890", refs[0].UID)
	}
}

func TestEnsureService_PreservesClusterIPWhenEmpty(t *testing.T) {
	ctx := context.Background()
	svc := newTestService("test-clusterip-svc", int32(8080))

	err := deployment.EnsureService(ctx, k8sClient, svc)
	if err != nil {
		t.Fatalf("first EnsureService returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, svc)
	})

	// Fetch the assigned ClusterIP.
	fetched := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-clusterip-svc", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get Service: %v", err)
	}
	originalClusterIP := fetched.Spec.ClusterIP
	if originalClusterIP == "" {
		t.Fatal("expected Kubernetes to assign a ClusterIP, got empty string")
	}

	// Update with empty ClusterIP — EnsureService should preserve the original.
	updated := newTestService("test-clusterip-svc", int32(9090))
	updated.Spec.ClusterIP = ""
	err = deployment.EnsureService(ctx, k8sClient, updated)
	if err != nil {
		t.Fatalf("second EnsureService returned error: %v", err)
	}

	refetched := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-clusterip-svc", Namespace: testNamespace}, refetched); err != nil {
		t.Fatalf("failed to get Service after update: %v", err)
	}
	if refetched.Spec.ClusterIP != originalClusterIP {
		t.Fatalf("expected ClusterIP to be preserved as %q, got %q", originalClusterIP, refetched.Spec.ClusterIP)
	}
}

func TestEnsureService_RejectsChangedClusterIP(t *testing.T) {
	ctx := context.Background()
	svc := newTestService("test-clusterip-reject-svc", int32(8080))

	err := deployment.EnsureService(ctx, k8sClient, svc)
	if err != nil {
		t.Fatalf("first EnsureService returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, svc)
	})

	// Attempt to update with a different ClusterIP — must fail. (CC-0005)
	updated := newTestService("test-clusterip-reject-svc", int32(9090))
	updated.Spec.ClusterIP = "10.96.99.99"
	err = deployment.EnsureService(ctx, k8sClient, updated)
	if err == nil {
		t.Fatal("expected error when ClusterIP differs from existing, got nil")
	}
}
