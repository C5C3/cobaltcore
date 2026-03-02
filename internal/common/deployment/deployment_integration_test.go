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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/deployment"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var (
	k8sClient  client.Client
	testScheme *runtime.Scheme
)

const testNamespace = "test-deployment"

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c

	testScheme = runtime.NewScheme()
	if err := corev1.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "failed to add corev1 to scheme: %v\n", err)
		teardown()
		os.Exit(1)
	}
	if err := appsv1.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "failed to add appsv1 to scheme: %v\n", err)
		teardown()
		os.Exit(1)
	}

	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNamespace},
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

// newOwner creates a ConfigMap to use as an owner object for controller references.
func newOwner(ctx context.Context, t *testing.T, name string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("failed to create owner ConfigMap: %v", err)
	}
	// Re-fetch to populate UID and other server-set fields.
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatalf("failed to get owner ConfigMap: %v", err)
	}
	return cm
}

func testDeployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "busybox"},
					},
				},
			},
		},
	}
}

func testService(name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{
				{Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func TestEnsureDeployment_Creates(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-deploy-creates")

	dep := testDeployment("dep-creates")
	if err := deployment.EnsureDeployment(ctx, k8sClient, owner, testScheme, dep); err != nil {
		t.Fatalf("EnsureDeployment returned error: %v", err)
	}

	// Verify the Deployment exists in the cluster.
	got := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "dep-creates", Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get Deployment: %v", err)
	}

	if got.Spec.Template.Spec.Containers[0].Image != "busybox" {
		t.Fatalf("expected container image %q, got %q", "busybox", got.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestEnsureDeployment_Idempotent(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-deploy-idempotent")

	dep := testDeployment("dep-idempotent")
	if err := deployment.EnsureDeployment(ctx, k8sClient, owner, testScheme, dep); err != nil {
		t.Fatalf("first call to EnsureDeployment returned error: %v", err)
	}

	dep2 := testDeployment("dep-idempotent")
	if err := deployment.EnsureDeployment(ctx, k8sClient, owner, testScheme, dep2); err != nil {
		t.Fatalf("second call to EnsureDeployment returned error: %v", err)
	}
}

func TestEnsureDeployment_OwnerRef(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-deploy-ref")

	dep := testDeployment("dep-ownerref")
	if err := deployment.EnsureDeployment(ctx, k8sClient, owner, testScheme, dep); err != nil {
		t.Fatalf("EnsureDeployment returned error: %v", err)
	}

	got := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "dep-ownerref", Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get Deployment: %v", err)
	}

	ownerRefs := got.GetOwnerReferences()
	if len(ownerRefs) == 0 {
		t.Fatal("expected at least one owner reference, got none")
	}

	found := false
	for _, ref := range ownerRefs {
		if ref.UID == owner.UID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected owner reference with UID %s, not found in %v", owner.UID, ownerRefs)
	}
}

func TestEnsureService_Creates(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-svc-creates")

	svc := testService("svc-creates")
	if err := deployment.EnsureService(ctx, k8sClient, owner, testScheme, svc); err != nil {
		t.Fatalf("EnsureService returned error: %v", err)
	}

	// Verify the Service exists in the cluster.
	got := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "svc-creates", Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get Service: %v", err)
	}

	if len(got.Spec.Ports) != 1 || got.Spec.Ports[0].Port != 80 {
		t.Fatalf("expected port 80, got %v", got.Spec.Ports)
	}
}

func TestEnsureService_Idempotent(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-svc-idempotent")

	svc := testService("svc-idempotent")
	if err := deployment.EnsureService(ctx, k8sClient, owner, testScheme, svc); err != nil {
		t.Fatalf("first call to EnsureService returned error: %v", err)
	}

	svc2 := testService("svc-idempotent")
	if err := deployment.EnsureService(ctx, k8sClient, owner, testScheme, svc2); err != nil {
		t.Fatalf("second call to EnsureService returned error: %v", err)
	}
}

func TestEnsureService_OwnerRef(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-svc-ref")

	svc := testService("svc-ownerref")
	if err := deployment.EnsureService(ctx, k8sClient, owner, testScheme, svc); err != nil {
		t.Fatalf("EnsureService returned error: %v", err)
	}

	got := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "svc-ownerref", Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get Service: %v", err)
	}

	ownerRefs := got.GetOwnerReferences()
	if len(ownerRefs) == 0 {
		t.Fatal("expected at least one owner reference, got none")
	}

	found := false
	for _, ref := range ownerRefs {
		if ref.UID == owner.UID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected owner reference with UID %s, not found in %v", owner.UID, ownerRefs)
	}
}
