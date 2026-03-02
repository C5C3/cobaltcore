//go:build integration

package job_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/job"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var (
	k8sClient  client.Client
	testScheme *runtime.Scheme
)

const testNamespace = "test-job"

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c
	testScheme = c.Scheme()

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

// newOwner creates a ConfigMap to act as the owner for owner-reference tests.
func newOwner(t *testing.T, ctx context.Context, name string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("failed to create owner ConfigMap: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatalf("failed to get owner ConfigMap: %v", err)
	}
	return cm
}

func testJob(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox", Command: []string{"echo", "hello"}},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
}

func testCronJob(name string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "cron", Image: "busybox", Command: []string{"echo", "cron"}},
							},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// RunJob tests
// ---------------------------------------------------------------------------

func TestRunJob_Creates(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(t, ctx, "owner-runjob-creates")

	j := testJob("runjob-creates")
	name, err := job.RunJob(ctx, k8sClient, owner, testScheme, j)
	if err != nil {
		t.Fatalf("RunJob() returned error: %v", err)
	}
	if name != "runjob-creates" {
		t.Fatalf("RunJob() returned name %q, want %q", name, "runjob-creates")
	}

	fetched := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "runjob-creates", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get created Job: %v", err)
	}
	if len(fetched.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("expected at least one container in the created Job")
	}
	if fetched.Spec.Template.Spec.Containers[0].Image != "busybox" {
		t.Errorf("expected container image %q, got %q", "busybox", fetched.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestRunJob_Idempotent(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(t, ctx, "owner-runjob-idempotent")

	j1 := testJob("runjob-idempotent")
	if _, err := job.RunJob(ctx, k8sClient, owner, testScheme, j1); err != nil {
		t.Fatalf("first RunJob() returned error: %v", err)
	}

	j2 := testJob("runjob-idempotent")
	name, err := job.RunJob(ctx, k8sClient, owner, testScheme, j2)
	if err != nil {
		t.Fatalf("second RunJob() returned error: %v", err)
	}
	if name != "runjob-idempotent" {
		t.Fatalf("second RunJob() returned name %q, want %q", name, "runjob-idempotent")
	}

	// Verify spec fields are unchanged after idempotent call.
	fetched := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "runjob-idempotent", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get Job after idempotent call: %v", err)
	}
	if fetched.Spec.Template.Spec.Containers[0].Image != "busybox" {
		t.Fatalf("Job image changed after idempotent call: expected %q, got %q", "busybox", fetched.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestRunJob_OwnerRef(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(t, ctx, "owner-runjob-ownerref")

	j := testJob("runjob-ownerref")
	if _, err := job.RunJob(ctx, k8sClient, owner, testScheme, j); err != nil {
		t.Fatalf("RunJob() returned error: %v", err)
	}

	fetched := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "runjob-ownerref", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get Job: %v", err)
	}

	refs := fetched.GetOwnerReferences()
	found := false
	for _, ref := range refs {
		if ref.UID == owner.UID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected owner reference with UID %s, got refs: %v", owner.UID, refs)
	}
}

// ---------------------------------------------------------------------------
// EnsureCronJob tests
// ---------------------------------------------------------------------------

func TestEnsureCronJob_Creates(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(t, ctx, "owner-cronjob-creates")

	cj := testCronJob("cronjob-creates")
	name, err := job.EnsureCronJob(ctx, k8sClient, owner, testScheme, cj)
	if err != nil {
		t.Fatalf("EnsureCronJob() returned error: %v", err)
	}
	if name != "cronjob-creates" {
		t.Fatalf("EnsureCronJob() returned name %q, want %q", name, "cronjob-creates")
	}

	fetched := &batchv1.CronJob{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "cronjob-creates", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get created CronJob: %v", err)
	}
	if fetched.Spec.Schedule != "0 * * * *" {
		t.Errorf("expected schedule %q, got %q", "0 * * * *", fetched.Spec.Schedule)
	}
}

func TestEnsureCronJob_Idempotent(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(t, ctx, "owner-cronjob-idempotent")

	cj1 := testCronJob("cronjob-idempotent")
	if _, err := job.EnsureCronJob(ctx, k8sClient, owner, testScheme, cj1); err != nil {
		t.Fatalf("first EnsureCronJob() returned error: %v", err)
	}

	cj2 := testCronJob("cronjob-idempotent")
	name, err := job.EnsureCronJob(ctx, k8sClient, owner, testScheme, cj2)
	if err != nil {
		t.Fatalf("second EnsureCronJob() returned error: %v", err)
	}
	if name != "cronjob-idempotent" {
		t.Fatalf("second EnsureCronJob() returned name %q, want %q", name, "cronjob-idempotent")
	}

	// Verify spec fields are unchanged after idempotent call.
	fetchedCJ := &batchv1.CronJob{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "cronjob-idempotent", Namespace: testNamespace}, fetchedCJ); err != nil {
		t.Fatalf("failed to get CronJob after idempotent call: %v", err)
	}
	if fetchedCJ.Spec.Schedule != "0 * * * *" {
		t.Fatalf("CronJob schedule changed after idempotent call: expected %q, got %q", "0 * * * *", fetchedCJ.Spec.Schedule)
	}
}

func TestEnsureCronJob_OwnerRef(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(t, ctx, "owner-cronjob-ownerref")

	cj := testCronJob("cronjob-ownerref")
	if _, err := job.EnsureCronJob(ctx, k8sClient, owner, testScheme, cj); err != nil {
		t.Fatalf("EnsureCronJob() returned error: %v", err)
	}

	fetched := &batchv1.CronJob{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "cronjob-ownerref", Namespace: testNamespace}, fetched); err != nil {
		t.Fatalf("failed to get CronJob: %v", err)
	}

	refs := fetched.GetOwnerReferences()
	found := false
	for _, ref := range refs {
		if ref.UID == owner.UID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected owner reference with UID %s, got refs: %v", owner.UID, refs)
	}
}
