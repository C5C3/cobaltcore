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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/deployment"
	"github.com/c5c3/forge/internal/common/testutil"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var k8sClient client.Client

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c
	code := m.Run()
	teardown()
	os.Exit(code)
}

func createTestNamespace(t *testing.T, ctx context.Context) *corev1.Namespace {
	return testutil.CreateTestNamespace(t, ctx, k8sClient, "test-deploy-")
}

func int32Ptr(i int32) *int32 { return &i }

// ---------------------------------------------------------------------------
// EnsureDeployment
// ---------------------------------------------------------------------------

func TestEnsureDeployment_Creates(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: ns.Name,
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

	if err := deployment.EnsureDeployment(ctx, k8sClient, deploy, "test-manager"); err != nil {
		t.Fatalf("EnsureDeployment returned error: %v", err)
	}

	// Verify the Deployment exists.
	got := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-deploy", Namespace: ns.Name}, got); err != nil {
		t.Fatalf("failed to get Deployment after creation: %v", err)
	}
	if *got.Spec.Replicas != 1 {
		t.Fatalf("expected 1 replica, got %d", *got.Spec.Replicas)
	}
}

func TestEnsureDeployment_Idempotent(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-deploy",
			Namespace: ns.Name,
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

	if err := deployment.EnsureDeployment(ctx, k8sClient, deploy, "test-manager"); err != nil {
		t.Fatalf("first EnsureDeployment returned error: %v", err)
	}

	// Second call with the same spec must succeed (SSA is idempotent).
	deploy2 := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-deploy",
			Namespace: ns.Name,
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

	if err := deployment.EnsureDeployment(ctx, k8sClient, deploy2, "test-manager"); err != nil {
		t.Fatalf("second EnsureDeployment returned error: %v", err)
	}
}

func TestEnsureDeployment_OwnerReferences(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owned-deploy",
			Namespace: ns.Name,
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

	ownerRef := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "owner-cm",
		UID:        "fake-uid-12345",
	}

	if err := deployment.EnsureDeployment(ctx, k8sClient, deploy, "test-manager", ownerRef); err != nil {
		t.Fatalf("EnsureDeployment returned error: %v", err)
	}

	// Verify the Deployment has the owner reference.
	got := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "owned-deploy", Namespace: ns.Name}, got); err != nil {
		t.Fatalf("failed to get Deployment: %v", err)
	}

	refs := got.GetOwnerReferences()
	if len(refs) == 0 {
		t.Fatal("expected at least one ownerReference on Deployment, got none")
	}
	found := false
	for _, ref := range refs {
		if ref.Name == "owner-cm" && ref.Kind == "ConfigMap" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ownerReference with Name=owner-cm Kind=ConfigMap, got: %v", refs)
	}
}

// ---------------------------------------------------------------------------
// EnsureService
// ---------------------------------------------------------------------------

func TestEnsureService_Creates(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: ns.Name,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}

	if err := deployment.EnsureService(ctx, k8sClient, svc, "test-manager"); err != nil {
		t.Fatalf("EnsureService returned error: %v", err)
	}

	// Verify the Service exists.
	got := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-svc", Namespace: ns.Name}, got); err != nil {
		t.Fatalf("failed to get Service after creation: %v", err)
	}
	if len(got.Spec.Ports) != 1 || got.Spec.Ports[0].Port != 80 {
		t.Fatalf("unexpected service ports: %v", got.Spec.Ports)
	}
}

func TestEnsureService_Idempotent(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-svc",
			Namespace: ns.Name,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}

	if err := deployment.EnsureService(ctx, k8sClient, svc, "test-manager"); err != nil {
		t.Fatalf("first EnsureService returned error: %v", err)
	}

	// Second call with the same spec must succeed (SSA is idempotent).
	svc2 := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-svc",
			Namespace: ns.Name,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}

	if err := deployment.EnsureService(ctx, k8sClient, svc2, "test-manager"); err != nil {
		t.Fatalf("second EnsureService returned error: %v", err)
	}
}

func TestEnsureService_OwnerReferences(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owned-svc",
			Namespace: ns.Name,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}

	ownerRef := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "owner-cm",
		UID:        "fake-uid-12345",
	}

	if err := deployment.EnsureService(ctx, k8sClient, svc, "test-manager", ownerRef); err != nil {
		t.Fatalf("EnsureService returned error: %v", err)
	}

	// Verify the Service has the owner reference.
	got := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "owned-svc", Namespace: ns.Name}, got); err != nil {
		t.Fatalf("failed to get Service: %v", err)
	}

	refs := got.GetOwnerReferences()
	if len(refs) == 0 {
		t.Fatal("expected at least one ownerReference on Service, got none")
	}
	found := false
	for _, ref := range refs {
		if ref.Name == "owner-cm" && ref.Kind == "ConfigMap" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ownerReference with Name=owner-cm Kind=ConfigMap, got: %v", refs)
	}
}
