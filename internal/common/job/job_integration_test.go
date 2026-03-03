// A Job is launched when work must run its course,
// one-shot and finite, driven by its source.
// Idempotent it stands — submit it twice,
// the cluster answers once, exact and nice.
//
// A CronJob ticks on schedules yet to come,
// its template waiting for the hour's drum.
// Updates shift the rhythm, change the beat,
// while owner refs ensure a cleanup neat.
//
// With envtest's stage we prove each batch command,
// that jobs complete as contracts understand.
// So test by test the workload holds its ground;
// in integration's forge, the truth is found.

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
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var k8sClient client.Client

const testNamespace = "test-job"

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
// RunJob tests
// ---------------------------------------------------------------------------

func TestRunJob_CreatesNewJob(t *testing.T) {
	ctx := context.Background()
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run-create", Namespace: testNamespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:    []corev1.Container{{Name: "worker", Image: "busybox", Command: []string{"echo", "hello"}}},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := job.RunJob(ctx, k8sClient, j); err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}

	got := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-run-create", Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get Job: %v", err)
	}
	if got.Name != "test-run-create" {
		t.Fatalf("expected Job name=%q, got %q", "test-run-create", got.Name)
	}
}

func TestRunJob_Idempotent(t *testing.T) {
	ctx := context.Background()
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run-idempotent", Namespace: testNamespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:    []corev1.Container{{Name: "worker", Image: "busybox", Command: []string{"echo", "hello"}}},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := job.RunJob(ctx, k8sClient, j); err != nil {
		t.Fatalf("first RunJob call returned error: %v", err)
	}

	// Second call with the same job should succeed (AlreadyExists is swallowed).
	j2 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run-idempotent", Namespace: testNamespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:    []corev1.Container{{Name: "worker", Image: "busybox", Command: []string{"echo", "hello"}}},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
	if err := job.RunJob(ctx, k8sClient, j2); err != nil {
		t.Fatalf("second RunJob call returned error: %v", err)
	}
}

func TestRunJob_SetsOwnerReferences(t *testing.T) {
	ctx := context.Background()
	ownerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deployment",
		UID:        "test-uid-12345",
	}
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-run-ownerrefs",
			Namespace:       testNamespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:    []corev1.Container{{Name: "worker", Image: "busybox", Command: []string{"echo", "hello"}}},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := job.RunJob(ctx, k8sClient, j); err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}

	got := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-run-ownerrefs", Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get Job: %v", err)
	}

	refs := got.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(refs))
	}
	if refs[0].Name != "my-deployment" {
		t.Fatalf("expected owner reference name=%q, got %q", "my-deployment", refs[0].Name)
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
// EnsureCronJob tests
// ---------------------------------------------------------------------------

func TestEnsureCronJob_CreatesNewCronJob(t *testing.T) {
	ctx := context.Background()
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cronjob-create", Namespace: testNamespace},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 * * 0",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "worker", Image: "busybox", Command: []string{"echo", "hello"}}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}

	if err := job.EnsureCronJob(ctx, k8sClient, cj); err != nil {
		t.Fatalf("EnsureCronJob returned error: %v", err)
	}

	got := &batchv1.CronJob{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-cronjob-create", Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get CronJob: %v", err)
	}
	if got.Spec.Schedule != "0 0 * * 0" {
		t.Fatalf("expected schedule=%q, got %q", "0 0 * * 0", got.Spec.Schedule)
	}
}

func TestEnsureCronJob_UpdatesExistingCronJob(t *testing.T) {
	ctx := context.Background()
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cronjob-update", Namespace: testNamespace},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 * * 0",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "worker", Image: "busybox", Command: []string{"echo", "hello"}}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}

	if err := job.EnsureCronJob(ctx, k8sClient, cj); err != nil {
		t.Fatalf("first EnsureCronJob call returned error: %v", err)
	}

	// Update the schedule and call again.
	updated := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cronjob-update", Namespace: testNamespace},
		Spec: batchv1.CronJobSpec{
			Schedule: "30 2 * * 1",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "worker", Image: "busybox", Command: []string{"echo", "hello"}}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}

	if err := job.EnsureCronJob(ctx, k8sClient, updated); err != nil {
		t.Fatalf("second EnsureCronJob call returned error: %v", err)
	}

	got := &batchv1.CronJob{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-cronjob-update", Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get CronJob: %v", err)
	}
	if got.Spec.Schedule != "30 2 * * 1" {
		t.Fatalf("expected updated schedule=%q, got %q", "30 2 * * 1", got.Spec.Schedule)
	}
}

func TestEnsureCronJob_SetsOwnerReferences(t *testing.T) {
	ctx := context.Background()
	ownerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deployment",
		UID:        "test-uid-67890",
	}
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-cronjob-ownerrefs",
			Namespace:       testNamespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 * * 0",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "worker", Image: "busybox", Command: []string{"echo", "hello"}}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}

	if err := job.EnsureCronJob(ctx, k8sClient, cj); err != nil {
		t.Fatalf("EnsureCronJob returned error: %v", err)
	}

	got := &batchv1.CronJob{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-cronjob-ownerrefs", Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get CronJob: %v", err)
	}

	refs := got.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(refs))
	}
	if refs[0].Name != "my-deployment" {
		t.Fatalf("expected owner reference name=%q, got %q", "my-deployment", refs[0].Name)
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
