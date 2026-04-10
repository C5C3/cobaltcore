// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/job"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0058

func policyValidationTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return s
}

func policyValidationKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			UID:        "ks-uid",
			Generation: 1,
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db-credentials"},
			},
			Cache: commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
			PolicyOverrides: &commonv1.PolicySpec{
				Rules: map[string]string{"identity:get_user": "role:admin"},
			},
		},
	}
}

func newPolicyValidationTestReconciler(s *runtime.Scheme, objs ...client.Object) *KeystoneReconciler {
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{})
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

// completedPolicyValidationJob returns a policy validation Job that matches what
// buildPolicyValidationJob produces and is marked as complete with the correct
// pod-spec hash (CC-0058).
func completedPolicyValidationJob(ks *keystonev1alpha1.Keystone, configMapName string) *batchv1.Job {
	desired := buildPolicyValidationJob(ks, configMapName)
	now := metav1.Now()
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{
		{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
		},
	}
	return j
}

// failedPolicyValidationJob returns a policy validation Job that is marked as
// permanently failed (CC-0058).
func failedPolicyValidationJob(ks *keystonev1alpha1.Keystone, configMapName string) *batchv1.Job {
	desired := buildPolicyValidationJob(ks, configMapName)
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	j.Status.Failed = 3
	j.Status.Conditions = []batchv1.JobCondition{
		{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue,
		},
	}
	return j
}

// runningPolicyValidationJob returns a policy validation Job that exists but is
// still running (CC-0058).
func runningPolicyValidationJob(ks *keystonev1alpha1.Keystone, configMapName string) *batchv1.Job {
	desired := buildPolicyValidationJob(ks, configMapName)
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	return j
}

// --- Build function tests (CC-0058, REQ-007) ---

func TestBuildPolicyValidationJob_ImageMatchesDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystone()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "validator")
	g.Expect(container).NotTo(BeNil())
	expectedImage := fmt.Sprintf("%s:%s", ks.Spec.Image.Repository, ks.Spec.Image.Tag)
	g.Expect(container.Image).To(Equal(expectedImage))
}

func TestBuildPolicyValidationJob_SecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystone()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "validator")
	expectRestrictedSecurityContext(g, container)
}

func TestBuildPolicyValidationJob_ConfigMapMount(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystone()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "validator")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.VolumeMounts).To(HaveLen(1))
	g.Expect(container.VolumeMounts[0].Name).To(Equal("config"))
	g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(container.VolumeMounts[0].ReadOnly).To(BeTrue())

	// Verify the volume references the ConfigMap by exact name.
	g.Expect(j.Spec.Template.Spec.Volumes).To(HaveLen(1))
	g.Expect(j.Spec.Template.Spec.Volumes[0].Name).To(Equal("config"))
	g.Expect(j.Spec.Template.Spec.Volumes[0].ConfigMap.Name).To(Equal("keystone-config-abc123"))
}

func TestBuildPolicyValidationJob_BackoffAndTTL(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystone()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	g.Expect(j.Spec.BackoffLimit).NotTo(BeNil())
	g.Expect(*j.Spec.BackoffLimit).To(Equal(int32(2)))
	g.Expect(j.Spec.TTLSecondsAfterFinished).NotTo(BeNil())
	g.Expect(*j.Spec.TTLSecondsAfterFinished).To(Equal(int32(300)))
	g.Expect(j.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
}

func TestBuildPolicyValidationJob_Command(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystone()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "validator")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Command).To(Equal([]string{
		"oslopolicy-validator",
		"--namespace", "keystone",
		"--config-file", "/etc/keystone/keystone.conf.d/keystone.conf",
	}))
}

func TestBuildPolicyValidationJob_TerminationMessagePolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystone()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "validator")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.TerminationMessagePolicy).To(Equal(corev1.TerminationMessageFallbackToLogsOnError))
}

// --- Reconciler lifecycle tests (CC-0058, REQ-001, REQ-002, REQ-003, REQ-005, REQ-009) ---

func TestReconcilePolicyValidation_NoPolicyOverrides_SkipsValidation(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()
	ks.Spec.PolicyOverrides = nil

	r := newPolicyValidationTestReconciler(s, ks)

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// Verify no Job was created.
	var j batchv1.Job
	err = r.Get(context.Background(), client.ObjectKey{
		Name:      fmt.Sprintf("%s-policy-validation", ks.Name),
		Namespace: ks.Namespace,
	}, &j)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	// Verify PolicyValidReady=True/NotRequired (CC-0058, REQ-003).
	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NotRequired"))
}

func TestReconcilePolicyValidation_PolicySet_JobCreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()

	r := newPolicyValidationTestReconciler(s, ks)

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueValidationWait))

	// Verify the Job was created (CC-0058, REQ-001).
	var createdJob batchv1.Job
	g.Expect(r.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-policy-validation",
		Namespace: "default",
	}, &createdJob)).To(Succeed())

	// Verify pod-spec hash annotation.
	g.Expect(createdJob.Annotations).To(HaveKey(job.PodSpecHashAnnotation))

	// Verify PolicyValidReady=False/PolicyValidationInProgress.
	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("PolicyValidationInProgress"))
}

func TestReconcilePolicyValidation_PolicyRemoved_JobCleanedUp(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()

	// Create a Job from when PolicyOverrides was set.
	existingJob := runningPolicyValidationJob(ks, "keystone-config-abc123")

	// Now remove PolicyOverrides (CC-0058, REQ-003).
	ks.Spec.PolicyOverrides = nil

	r := newPolicyValidationTestReconciler(s, ks, existingJob)

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// Verify the Job was deleted.
	var j batchv1.Job
	err = r.Get(context.Background(), client.ObjectKey{
		Name:      fmt.Sprintf("%s-policy-validation", ks.Name),
		Namespace: ks.Namespace,
	}, &j)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	// Verify PolicyValidReady=True/NotRequired.
	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NotRequired"))
}

func TestReconcilePolicyValidation_PolicyRemoved_NoJob_NoError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()
	ks.Spec.PolicyOverrides = nil

	r := newPolicyValidationTestReconciler(s, ks)

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NotRequired"))
}

func TestReconcilePolicyValidation_JobRunning_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()

	r := newPolicyValidationTestReconciler(s, ks, runningPolicyValidationJob(ks, "keystone-config-abc123"))

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueValidationWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("PolicyValidationInProgress"))
	g.Expect(cond.Message).To(Equal("Policy validation job is running"))
}

func TestReconcilePolicyValidation_JobComplete_ConditionTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()

	r := newPolicyValidationTestReconciler(s, ks, completedPolicyValidationJob(ks, "keystone-config-abc123"))

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("PolicyValidationPassed"))
	g.Expect(cond.Message).To(Equal("Policy validation completed successfully"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

func TestReconcilePolicyValidation_JobFailed_ConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()

	r := newPolicyValidationTestReconciler(s, ks, failedPolicyValidationJob(ks, "keystone-config-abc123"))

	_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, job.ErrJobFailed)).To(BeTrue())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("PolicyValidationFailed"))
}

func TestReconcilePolicyValidation_JobFailed_ReusesExistingMessage(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()

	// Pre-populate the condition as if a previous reconcile already extracted
	// the failure message. The reconciler should reuse this message instead of
	// calling extractJobFailureMessage again (CC-0058, comment #6).
	ks.Status.Conditions = []metav1.Condition{{
		Type:    conditionTypePolicyValidReady,
		Status:  metav1.ConditionFalse,
		Reason:  "PolicyValidationFailed",
		Message: "oslopolicy-checker: cached error from previous reconcile",
	}}

	// Failed Job with NO Pod — without the short-circuit, extractJobFailureMessage
	// would return the fallback "check logs" message instead of the cached one.
	failedJob := failedPolicyValidationJob(ks, "keystone-config-abc123")

	r := newPolicyValidationTestReconciler(s, ks, failedJob)

	_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, job.ErrJobFailed)).To(BeTrue())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Message).To(Equal("oslopolicy-checker: cached error from previous reconcile"))
}

func TestReconcilePolicyValidation_StaleJob_DeletedAndRecreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()

	// Create a completed Job with a stale hash (simulating a ConfigMap name
	// change due to updated policy content) (CC-0058, REQ-005).
	staleJob := completedPolicyValidationJob(ks, "keystone-config-abc123")
	staleJob.Annotations[job.PodSpecHashAnnotation] = "stale-hash-from-previous-spec"

	r := newPolicyValidationTestReconciler(s, ks, staleJob)

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueValidationWait))

	// Verify a new Job was created with the correct hash.
	var newJob batchv1.Job
	g.Expect(r.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-policy-validation",
		Namespace: "default",
	}, &newJob)).To(Succeed())

	desired := buildPolicyValidationJob(ks, "keystone-config-abc123")
	expectedHash := job.PodSpecHash(&desired.Spec.Template.Spec)
	g.Expect(newJob.Annotations[job.PodSpecHashAnnotation]).To(Equal(expectedHash))
}

func TestReconcilePolicyValidation_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()
	ks.Generation = 5

	r := newPolicyValidationTestReconciler(s, ks)

	// Job creation path sets condition with ObservedGeneration (CC-0058, REQ-009).
	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueValidationWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(5)))
}

// --- Descriptive error extraction tests (CC-0058, REQ-006) ---

// policyValidationPodWithTerminationMessage returns a Pod labelled for the
// validation Job whose "validator" container terminated with the given message.
func policyValidationPodWithTerminationMessage(ks *keystonev1alpha1.Keystone, terminationMessage string) *corev1.Pod {
	jobName := fmt.Sprintf("%s-policy-validation", ks.Name)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-abc",
			Namespace: ks.Namespace,
			Labels:    map[string]string{"job-name": jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "validator",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1,
						Message:  terminationMessage,
					},
				},
			}},
		},
	}
}

func TestReconcilePolicyValidation_JobFailed_DescriptiveErrorMessage(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()

	failedJob := failedPolicyValidationJob(ks, "keystone-config-abc123")
	pod := policyValidationPodWithTerminationMessage(ks, "oslopolicy-checker: Unknown action 'identity:foo_bar'")

	r := newPolicyValidationTestReconciler(s, ks, failedJob, pod)

	_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, job.ErrJobFailed)).To(BeTrue())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("PolicyValidationFailed"))
	// REQ-006 Scenario 1: condition message must contain the actual error output.
	g.Expect(cond.Message).To(ContainSubstring("oslopolicy-checker: Unknown action 'identity:foo_bar'"))
}

func TestReconcilePolicyValidation_JobFailed_MessageTruncated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()

	// Build a termination message exceeding 1024 characters.
	longMessage := strings.Repeat("oslopolicy-checker error: malformed check string at line N; ", 30)
	g.Expect(len(longMessage)).To(BeNumerically(">", 1024))

	failedJob := failedPolicyValidationJob(ks, "keystone-config-abc123")
	pod := policyValidationPodWithTerminationMessage(ks, longMessage)

	r := newPolicyValidationTestReconciler(s, ks, failedJob, pod)

	_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	// REQ-006 Scenario 2: message must be truncated to at most 1024 characters.
	g.Expect(len(cond.Message)).To(BeNumerically("<=", 1024))
	// It should still contain the beginning of the actual error.
	g.Expect(cond.Message).To(ContainSubstring("oslopolicy-checker error"))
}

func TestReconcilePolicyValidation_JobFailed_FallbackMessage(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone()

	// Failed Job with NO Pod at all.
	failedJob := failedPolicyValidationJob(ks, "keystone-config-abc123")

	r := newPolicyValidationTestReconciler(s, ks, failedJob)

	_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, job.ErrJobFailed)).To(BeTrue())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("PolicyValidationFailed"))
	// REQ-006 Scenario 3: fallback message must include Job name and namespace.
	g.Expect(cond.Message).To(ContainSubstring("test-keystone-policy-validation"))
	g.Expect(cond.Message).To(ContainSubstring("default"))
	g.Expect(cond.Message).To(ContainSubstring("kubectl logs"))
}
