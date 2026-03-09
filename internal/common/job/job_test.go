// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package job

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Feature: CC-0005

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       "test-uid",
		},
	}
}

func testJob() *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-job",
			Namespace: "default",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox:latest", Command: []string{"echo", "hello"}},
					},
				},
			},
		},
	}
}

// testCompletedJobWithHash returns a completed Job that carries the
// pod-spec-hash annotation matching the testJob() PodSpec, simulating a Job
// that was created via RunJob. API-server defaults may be present on the
// stored spec, but the hash annotation refers to the original desired spec
// (CC-0005).
func testCompletedJobWithHash() *batchv1.Job {
	desired := testJob()
	j := testJob()
	// Simulate API-server defaults on the stored spec.
	for i := range j.Spec.Template.Spec.Containers {
		j.Spec.Template.Spec.Containers[i].ImagePullPolicy = corev1.PullAlways
		j.Spec.Template.Spec.Containers[i].TerminationMessagePath = corev1.TerminationMessagePathDefault
		j.Spec.Template.Spec.Containers[i].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	j.Annotations = map[string]string{
		PodSpecHashAnnotation: PodSpecHash(&desired.Spec.Template.Spec),
	}
	now := metav1.Now()
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	return j
}

func testCronJob() *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cronjob",
			Namespace: "default",
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 * * 0",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers: []corev1.Container{
								{Name: "worker", Image: "busybox:latest", Command: []string{"echo", "hello"}},
							},
						},
					},
				},
			},
		},
	}
}

// --- RunJob ---

func TestRunJob_creates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ready, err := RunJob(context.Background(), c, s, owner, testJob())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created job should not be complete")

	created := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-job", Namespace: "default"}, created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.Annotations[PodSpecHashAnnotation]).NotTo(BeEmpty(), "created Job should carry pod-spec hash annotation")
}

func TestRunJob_existingIncomplete(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	job := testJob()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, job).
		WithStatusSubresource(job).
		Build()

	ready, err := RunJob(context.Background(), c, s, owner, testJob())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestRunJob_existingComplete(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Simulate a completed Job that was created via RunJob (carries hash
	// annotation), with API-server defaults on the stored spec (CC-0005).
	job := testCompletedJobWithHash()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, job).
		WithStatusSubresource(job).
		Build()

	ready, err := RunJob(context.Background(), c, s, owner, testJob())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestRunJob_existingFailed(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	job := testJob()
	job.Status.Failed = 3
	job.Status.Conditions = []batchv1.JobCondition{
		{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, job).
		WithStatusSubresource(job).
		Build()

	ready, err := RunJob(context.Background(), c, s, owner, testJob())
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, ErrJobFailed)).To(BeTrue(), "error should wrap ErrJobFailed sentinel")
	g.Expect(err.Error()).To(ContainSubstring("default/test-job"))
	g.Expect(ready).To(BeFalse())
}

func TestRunJob_existingComplete_specChanged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Simulate a completed Job from a previous operator version. The hash
	// annotation matches the old spec (CC-0005).
	oldJob := testCompletedJobWithHash()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, oldJob).
		WithStatusSubresource(oldJob).
		Build()

	// New desired Job with a different container image (operator upgrade).
	newJob := testJob()
	newJob.Spec.Template.Spec.Containers[0].Image = "busybox:v2"

	ready, err := RunJob(context.Background(), c, s, owner, newJob)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "should recreate Job when spec changes after completion")

	// Verify the old Job was replaced with one carrying the new image.
	fetched := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-job", Namespace: "default"}, fetched)).To(Succeed())
	g.Expect(fetched.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox:v2"))
	g.Expect(fetched.OwnerReferences).To(HaveLen(1))
	g.Expect(fetched.Annotations[PodSpecHashAnnotation]).NotTo(BeEmpty(), "replacement Job should carry hash annotation")
}

func TestRunJob_existingComplete_specUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Completed Job with the same spec (hash matches desired) should
	// return (true, nil). API-server defaults on the stored spec do not
	// matter because comparison is hash-based (CC-0005).
	existing := testCompletedJobWithHash()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	ready, err := RunJob(context.Background(), c, s, owner, testJob())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue(), "should return true when completed Job has unchanged spec")
}

// TestRunJob_existingComplete_noHashAnnotation verifies that a completed Job
// without a hash annotation (e.g. created before the hash mechanism was
// introduced) is treated as stale and recreated (CC-0005).
func TestRunJob_existingComplete_noHashAnnotation(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	now := metav1.Now()
	oldJob := testJob()
	oldJob.Status.Succeeded = 1
	oldJob.Status.CompletionTime = &now
	oldJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	// No hash annotation on the old Job.

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, oldJob).
		WithStatusSubresource(oldJob).
		Build()

	ready, err := RunJob(context.Background(), c, s, owner, testJob())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "should recreate Job when hash annotation is missing")

	// Verify the replacement Job carries the hash annotation.
	fetched := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-job", Namespace: "default"}, fetched)).To(Succeed())
	g.Expect(fetched.Annotations[PodSpecHashAnnotation]).NotTo(BeEmpty())
}

func TestRunJob_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ctx := context.Background()

	_, err := RunJob(ctx, c, s, owner, testJob())
	g.Expect(err).NotTo(HaveOccurred())

	// Second call should find existing job, not error.
	ready, err := RunJob(ctx, c, s, owner, testJob())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestRunJob_existingComplete_specChanged_alreadyExists(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Simulate a completed Job whose delete is pending (finalizer) so
	// the replacement Create returns AlreadyExists (CC-0005).
	oldJob := testCompletedJobWithHash()

	createCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, oldJob).
		WithStatusSubresource(oldJob).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*batchv1.Job); ok {
					createCalls++
					return apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, obj.GetName())
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	newJob := testJob()
	newJob.Spec.Template.Spec.Containers[0].Image = "busybox:v2"

	ready, err := RunJob(context.Background(), c, s, owner, newJob)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "should return false when old Job is still terminating")
	g.Expect(createCalls).To(Equal(1))
}

// --- EnsureCronJob ---

func TestEnsureCronJob_creates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	err := EnsureCronJob(context.Background(), c, s, owner, testCronJob())
	g.Expect(err).NotTo(HaveOccurred())

	created := &batchv1.CronJob{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-cronjob", Namespace: "default"}, created)).To(Succeed())
	g.Expect(created.Spec.Schedule).To(Equal("0 0 * * 0"))
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsureCronJob_updates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	existing := testCronJob()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	updated := testCronJob()
	updated.Spec.Schedule = "0 12 * * *"

	err := EnsureCronJob(context.Background(), c, s, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &batchv1.CronJob{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Spec.Schedule).To(Equal("0 12 * * *"))
}

func TestEnsureCronJob_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ctx := context.Background()
	g.Expect(EnsureCronJob(ctx, c, s, owner, testCronJob())).To(Succeed())
	g.Expect(EnsureCronJob(ctx, c, s, owner, testCronJob())).To(Succeed())

	list := &batchv1.CronJobList{}
	g.Expect(c.List(ctx, list, client.InNamespace("default"))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}

// --- IsJobComplete ---

func TestIsJobComplete_true(t *testing.T) {
	g := NewGomegaWithT(t)
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
	g.Expect(IsJobComplete(job)).To(BeTrue())
}

func TestIsJobComplete_false_noConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	job := &batchv1.Job{}
	g.Expect(IsJobComplete(job)).To(BeFalse())
}

func TestIsJobComplete_false_failed(t *testing.T) {
	g := NewGomegaWithT(t)
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}
	g.Expect(IsJobComplete(job)).To(BeFalse())
}

func TestIsJobComplete_false_completeNotTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
			},
		},
	}
	g.Expect(IsJobComplete(job)).To(BeFalse())
}

// --- IsJobFailed ---

func TestIsJobFailed_true(t *testing.T) {
	g := NewGomegaWithT(t)
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}
	g.Expect(IsJobFailed(job)).To(BeTrue())
}

func TestIsJobFailed_false_noConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	job := &batchv1.Job{}
	g.Expect(IsJobFailed(job)).To(BeFalse())
}

func TestIsJobFailed_false_failedNotTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionFalse},
			},
		},
	}
	g.Expect(IsJobFailed(job)).To(BeFalse())
}
