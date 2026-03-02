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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/job"
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
	return testutil.CreateTestNamespace(t, ctx, k8sClient, "test-job-")
}

// ---------------------------------------------------------------------------
// RunJob
// ---------------------------------------------------------------------------

func TestRunJob_Creates(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run-job",
			Namespace: ns.Name,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox:latest", Command: []string{"echo", "hello"}},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := job.RunJob(ctx, k8sClient, j); err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}

	// Verify the Job exists.
	got := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-run-job", Namespace: ns.Name}, got); err != nil {
		t.Fatalf("failed to get Job after creation: %v", err)
	}
	if got.Name != "test-run-job" {
		t.Fatalf("expected job name=test-run-job, got %s", got.Name)
	}
}

func TestRunJob_Idempotent(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run-job-idem",
			Namespace: ns.Name,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox:latest", Command: []string{"echo", "hello"}},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := job.RunJob(ctx, k8sClient, j); err != nil {
		t.Fatalf("first RunJob returned error: %v", err)
	}

	// Second call with the same Job must succeed (AlreadyExists is handled).
	j2 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run-job-idem",
			Namespace: ns.Name,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox:latest", Command: []string{"echo", "hello"}},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := job.RunJob(ctx, k8sClient, j2); err != nil {
		t.Fatalf("second RunJob returned error: %v", err)
	}
}

func TestRunJob_OwnerReferences(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run-job-owner",
			Namespace: ns.Name,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox:latest", Command: []string{"echo", "hello"}},
					},
					RestartPolicy: corev1.RestartPolicyNever,
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

	if err := job.RunJob(ctx, k8sClient, j, ownerRef); err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}

	// Verify the Job has the owner reference.
	got := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-run-job-owner", Namespace: ns.Name}, got); err != nil {
		t.Fatalf("failed to get Job: %v", err)
	}

	refs := got.GetOwnerReferences()
	if len(refs) == 0 {
		t.Fatal("expected at least one ownerReference on Job, got none")
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
// EnsureCronJob
// ---------------------------------------------------------------------------

func TestEnsureCronJob_Creates(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cronjob",
			Namespace: ns.Name,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "cron", Image: "busybox:latest", Command: []string{"echo", "tick"}},
							},
							RestartPolicy: corev1.RestartPolicyOnFailure,
						},
					},
				},
			},
		},
	}

	if err := job.EnsureCronJob(ctx, k8sClient, cj, "test-manager"); err != nil {
		t.Fatalf("EnsureCronJob returned error: %v", err)
	}

	// Verify the CronJob exists.
	got := &batchv1.CronJob{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-cronjob", Namespace: ns.Name}, got); err != nil {
		t.Fatalf("failed to get CronJob after creation: %v", err)
	}
	if got.Spec.Schedule != "*/5 * * * *" {
		t.Fatalf("expected schedule=*/5 * * * *, got %s", got.Spec.Schedule)
	}
}

func TestEnsureCronJob_Idempotent(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-cronjob",
			Namespace: ns.Name,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "cron", Image: "busybox:latest", Command: []string{"echo", "tick"}},
							},
							RestartPolicy: corev1.RestartPolicyOnFailure,
						},
					},
				},
			},
		},
	}

	if err := job.EnsureCronJob(ctx, k8sClient, cj, "test-manager"); err != nil {
		t.Fatalf("first EnsureCronJob returned error: %v", err)
	}

	// Second call with the same spec must succeed (SSA is idempotent).
	cj2 := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-cronjob",
			Namespace: ns.Name,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "cron", Image: "busybox:latest", Command: []string{"echo", "tick"}},
							},
							RestartPolicy: corev1.RestartPolicyOnFailure,
						},
					},
				},
			},
		},
	}

	if err := job.EnsureCronJob(ctx, k8sClient, cj2, "test-manager"); err != nil {
		t.Fatalf("second EnsureCronJob returned error: %v", err)
	}
}

func TestEnsureCronJob_OwnerReferences(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owned-cronjob",
			Namespace: ns.Name,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "cron", Image: "busybox:latest", Command: []string{"echo", "tick"}},
							},
							RestartPolicy: corev1.RestartPolicyOnFailure,
						},
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

	if err := job.EnsureCronJob(ctx, k8sClient, cj, "test-manager", ownerRef); err != nil {
		t.Fatalf("EnsureCronJob returned error: %v", err)
	}

	// Verify the CronJob has the owner reference.
	got := &batchv1.CronJob{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "owned-cronjob", Namespace: ns.Name}, got); err != nil {
		t.Fatalf("failed to get CronJob: %v", err)
	}

	refs := got.GetOwnerReferences()
	if len(refs) == 0 {
		t.Fatal("expected at least one ownerReference on CronJob, got none")
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
