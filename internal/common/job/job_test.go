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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

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
		PodSpecHashAnnotation: PodSpecHash(&desired.Spec.Template),
	}
	now := metav1.Now()
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	return j
}

// testFailedJobWithHash returns a permanently failed Job that carries the
// pod-spec-hash annotation matching the testJob() PodSpec, simulating a Job
// created via RunJob that then exhausted its backoffLimit. As with
// testCompletedJobWithHash, API-server defaults may be present on the stored
// spec, but the hash annotation refers to the original desired spec — so a
// failed Job whose spec is unchanged stays failed (#460).
func testFailedJobWithHash() *batchv1.Job {
	desired := testJob()
	j := testJob()
	// Simulate API-server defaults on the stored spec.
	for i := range j.Spec.Template.Spec.Containers {
		j.Spec.Template.Spec.Containers[i].ImagePullPolicy = corev1.PullAlways
		j.Spec.Template.Spec.Containers[i].TerminationMessagePath = corev1.TerminationMessagePathDefault
		j.Spec.Template.Spec.Containers[i].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	j.Annotations = map[string]string{
		PodSpecHashAnnotation: PodSpecHash(&desired.Spec.Template),
	}
	j.Status.Failed = 3
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
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
	// annotation), with API-server defaults on the stored spec.
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

	// A permanently failed Job whose stored re-run key still matches the
	// desired template: the spec has not been fixed, so it stays failed and
	// returns ErrJobFailed rather than re-running forever (#460).
	job := testFailedJobWithHash()

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

// TestRunJob_existingFailed_specChanged verifies the #460 fix: a permanently
// failed Job is deleted and re-created when the desired pod template changes
// (here a new container image), instead of returning ErrJobFailed forever. This
// is the failed-then-fixed path shared by the default-hash callers (db-sync,
// schema-check, expand/migrate/contract, policy validation).
func TestRunJob_existingFailed_specChanged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Failed Job whose stored hash matches the OLD template.
	oldJob := testFailedJobWithHash()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, oldJob).
		WithStatusSubresource(oldJob).
		Build()

	// New desired Job with a different container image (the fixed spec).
	newJob := testJob()
	newJob.Spec.Template.Spec.Containers[0].Image = "busybox:v2"

	ready, err := RunJob(context.Background(), c, s, owner, newJob)
	g.Expect(err).NotTo(HaveOccurred(), "a failed Job must be re-run when the spec is fixed, not return ErrJobFailed")
	g.Expect(ready).To(BeFalse(), "should recreate the failed Job when the spec changes")

	// Verify the failed Job was replaced with one carrying the new image and hash.
	fetched := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-job", Namespace: "default"}, fetched)).To(Succeed())
	g.Expect(fetched.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox:v2"))
	g.Expect(fetched.OwnerReferences).To(HaveLen(1))
	g.Expect(fetched.Annotations[PodSpecHashAnnotation]).To(Equal(PodSpecHash(&newJob.Spec.Template)),
		"replacement Job should carry the hash of the new template")
}

// TestRunJob_existingFailed_noHashAnnotation verifies that a permanently failed
// Job without a hash annotation (e.g. one that failed before the operator was
// upgraded to a hash-aware version) is treated as stale and re-created rather
// than wedged on ErrJobFailed (#460). Mirrors the completed-Job counterpart.
func TestRunJob_existingFailed_noHashAnnotation(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	oldJob := testJob()
	oldJob.Status.Failed = 3
	oldJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
	}
	// No hash annotation on the old Job.

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, oldJob).
		WithStatusSubresource(oldJob).
		Build()

	ready, err := RunJob(context.Background(), c, s, owner, testJob())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "should recreate the failed Job when the hash annotation is missing")

	fetched := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-job", Namespace: "default"}, fetched)).To(Succeed())
	g.Expect(fetched.Annotations[PodSpecHashAnnotation]).NotTo(BeEmpty())
}

func TestRunJob_existingComplete_specChanged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Simulate a completed Job from a previous operator version. The hash
	// annotation matches the old spec.
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
	// matter because comparison is hash-based.
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

// completedJobWithRerunKey returns a completed Job stamped with an explicit
// re-run key in the PodSpecHashAnnotation, simulating a Job created via
// RunJobWithRerunKey.
func completedJobWithRerunKey(key string) *batchv1.Job {
	j := testJob()
	j.UID = "existing-uid"
	j.Annotations = map[string]string{PodSpecHashAnnotation: key}
	now := metav1.Now()
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	return j
}

// failedJobWithRerunKey returns a permanently failed Job stamped with an
// explicit re-run key in the PodSpecHashAnnotation, simulating a Job created via
// RunJobWithRerunKey that then exhausted its backoffLimit (#460).
func failedJobWithRerunKey(key string) *batchv1.Job {
	j := testJob()
	j.UID = "existing-uid"
	j.Annotations = map[string]string{PodSpecHashAnnotation: key}
	j.Status.Failed = 3
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
	}
	return j
}

// TestRunJobWithRerunKey_keyUnchanged_imageChanged locks the behavior:
// with an explicit re-run key, a completed Job is NOT re-run on a pod-template
// change (here a new container image) as long as the key is unchanged. The
// keystone bootstrap Job relies on this so a release upgrade does not re-run
// keystone-manage bootstrap against the already-migrated admin user.
func TestRunJobWithRerunKey_keyUnchanged_imageChanged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := completedJobWithRerunKey("rerun-key-1")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	// Desired Job has a DIFFERENT image but the SAME re-run key.
	newJob := testJob()
	newJob.Spec.Template.Spec.Containers[0].Image = "busybox:v2"

	ready, err := RunJobWithRerunKey(context.Background(), c, s, owner, newJob, "rerun-key-1")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue(), "unchanged re-run key must not re-run the completed Job despite an image change")

	// The original Job must be retained unchanged (same UID, original image).
	fetched := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-job", Namespace: "default"}, fetched)).To(Succeed())
	g.Expect(fetched.UID).To(Equal(existing.UID), "completed Job must be retained, not recreated")
	g.Expect(fetched.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox:latest"))
}

// TestRunJobWithRerunKey_keyChanged verifies that a completed Job IS re-run when
// the supplied re-run key changes, and the replacement carries the new key.
func TestRunJobWithRerunKey_keyChanged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := completedJobWithRerunKey("rerun-key-1")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	ready, err := RunJobWithRerunKey(context.Background(), c, s, owner, testJob(), "rerun-key-2")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "a changed re-run key must re-run the completed Job")

	fetched := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-job", Namespace: "default"}, fetched)).To(Succeed())
	g.Expect(fetched.Annotations[PodSpecHashAnnotation]).To(Equal("rerun-key-2"), "replacement Job must carry the new re-run key")
}

// TestRunJobWithRerunKey_failed_keyChanged verifies the #460 fix for the
// explicit-key caller (the keystone bootstrap Job): a permanently failed Job is
// re-run when the supplied re-run key changes — e.g. a rotated admin password —
// and the replacement carries the new key.
func TestRunJobWithRerunKey_failed_keyChanged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := failedJobWithRerunKey("rerun-key-1")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	ready, err := RunJobWithRerunKey(context.Background(), c, s, owner, testJob(), "rerun-key-2")
	g.Expect(err).NotTo(HaveOccurred(), "a changed re-run key must re-run the failed Job, not return ErrJobFailed")
	g.Expect(ready).To(BeFalse(), "should recreate the failed Job when the re-run key changes")

	fetched := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-job", Namespace: "default"}, fetched)).To(Succeed())
	g.Expect(fetched.Annotations[PodSpecHashAnnotation]).To(Equal("rerun-key-2"), "replacement Job must carry the new re-run key")
}

// TestRunJobWithRerunKey_failed_keyUnchanged_imageChanged locks the
// behavior for a *failed* Job: with an explicit re-run key, a failed Job is NOT
// re-run on a pod-template change (here a new container image) as long as the
// key is unchanged — it returns ErrJobFailed. The keystone bootstrap Job relies
// on this so a release-upgrade image change does not re-run keystone-manage
// bootstrap against the already-migrated admin user after a transient failure
// (#460).
func TestRunJobWithRerunKey_failed_keyUnchanged_imageChanged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := failedJobWithRerunKey("rerun-key-1")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	// Desired Job has a DIFFERENT image but the SAME re-run key.
	newJob := testJob()
	newJob.Spec.Template.Spec.Containers[0].Image = "busybox:v2"

	ready, err := RunJobWithRerunKey(context.Background(), c, s, owner, newJob, "rerun-key-1")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, ErrJobFailed)).To(BeTrue(), "unchanged re-run key must keep a failed Job failed despite an image change")
	g.Expect(ready).To(BeFalse())

	// The original failed Job must be retained unchanged (same UID, original image).
	fetched := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-job", Namespace: "default"}, fetched)).To(Succeed())
	g.Expect(fetched.UID).To(Equal(existing.UID), "failed Job must be retained, not recreated")
	g.Expect(fetched.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox:latest"))
}

// TestRunJob_existingComplete_noHashAnnotation verifies that a completed Job
// without a hash annotation (e.g. created before the hash mechanism was
// introduced) is treated as stale and recreated.
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
	// the replacement Create returns AlreadyExists.
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

// --- PodSpecHash template coverage ---

// TestPodSpecHash_TemplateAnnotationParticipates verifies that two pod
// templates with byte-identical PodSpecs but differing pod-template
// annotations hash to different digests. This is the property relies
// on: a content-derived pod-template annotation must change the stored
// forge.c5c3.io/pod-spec-hash so RunJob re-runs a completed Job even when the
// PodSpec itself is unchanged.
func TestPodSpecHash_TemplateAnnotationParticipates(t *testing.T) {
	g := NewGomegaWithT(t)

	base := testJob().Spec.Template
	annotated := testJob().Spec.Template
	annotated.Annotations = map[string]string{"forge.c5c3.io/trigger": "rotated"}

	g.Expect(PodSpecHash(&base)).To(Equal(PodSpecHash(&base)), "hash must be deterministic")
	g.Expect(PodSpecHash(&annotated)).NotTo(Equal(PodSpecHash(&base)),
		"a pod-template annotation must change the hash even when the PodSpec is identical")
}

// TestRunJob_RecreatesOnTemplateAnnotationChange verifies that a completed Job
// is deleted-and-recreated when the only difference in the desired template is
// a pod-template annotation — the PodSpec is byte-identical. This
// mirrors the bootstrap password-rotation case, where the password is injected
// by secretKeyRef and only a pod-template annotation carries the change signal.
func TestRunJob_RecreatesOnTemplateAnnotationChange(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Completed Job whose stored hash was computed from a template WITHOUT the
	// trigger annotation.
	oldJob := testJob()
	oldJob.Annotations = map[string]string{
		PodSpecHashAnnotation: PodSpecHash(&oldJob.Spec.Template),
	}
	now := metav1.Now()
	oldJob.Status.Succeeded = 1
	oldJob.Status.CompletionTime = &now
	oldJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, oldJob).
		WithStatusSubresource(oldJob).
		Build()

	// Desired Job differs only by a pod-template annotation; the PodSpec is
	// byte-identical.
	newJob := testJob()
	newJob.Spec.Template.Annotations = map[string]string{"forge.c5c3.io/trigger": "rotated"}

	ready, err := RunJob(context.Background(), c, s, owner, newJob)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "should recreate when only the pod-template annotation changed")

	fetched := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-job", Namespace: "default"}, fetched)).To(Succeed())
	g.Expect(fetched.Annotations[PodSpecHashAnnotation]).To(Equal(PodSpecHash(&newJob.Spec.Template)),
		"replacement Job should carry the hash of the new template")
	g.Expect(fetched.Annotations[PodSpecHashAnnotation]).NotTo(Equal(oldJob.Annotations[PodSpecHashAnnotation]),
		"new hash must differ from the old template's hash")
	g.Expect(fetched.Spec.Template.Annotations).To(HaveKeyWithValue("forge.c5c3.io/trigger", "rotated"))
}

// TestRunJob_NoRecreateWhenTemplateUnchanged verifies that a completed Job
// whose stored hash already matches the desired template — including its
// pod-template annotations — is retained without a delete-and-recreate
// This is the no-churn invariant for an unchanged credential.
func TestRunJob_NoRecreateWhenTemplateUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	desired := testJob()
	desired.Spec.Template.Annotations = map[string]string{"forge.c5c3.io/trigger": "v1"}

	// Completed Job whose stored hash already matches the desired template.
	existing := desired.DeepCopy()
	existing.Annotations = map[string]string{
		PodSpecHashAnnotation: PodSpecHash(&desired.Spec.Template),
	}
	now := metav1.Now()
	existing.Status.Succeeded = 1
	existing.Status.CompletionTime = &now
	existing.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	ready, err := RunJob(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue(), "should retain the completed Job when the template (incl. annotations) is unchanged")
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

// --- isJobComplete ---

func TestIsJobComplete_true(t *testing.T) {
	g := NewGomegaWithT(t)
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
	g.Expect(isJobComplete(job)).To(BeTrue())
}

func TestIsJobComplete_false_noConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	job := &batchv1.Job{}
	g.Expect(isJobComplete(job)).To(BeFalse())
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
	g.Expect(isJobComplete(job)).To(BeFalse())
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
	g.Expect(isJobComplete(job)).To(BeFalse())
}

// --- isJobFailed ---

func TestIsJobFailed_true(t *testing.T) {
	g := NewGomegaWithT(t)
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}
	g.Expect(isJobFailed(job)).To(BeTrue())
}

func TestIsJobFailed_false_noConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	job := &batchv1.Job{}
	g.Expect(isJobFailed(job)).To(BeFalse())
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
	g.Expect(isJobFailed(job)).To(BeFalse())
}
