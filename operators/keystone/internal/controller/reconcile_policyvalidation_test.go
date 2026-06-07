// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/job"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0058

// --- Test Helpers ---

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
			Generation: 3,
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 1,
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
		},
	}
}

// policyValidationKeystoneWithPolicy returns a test Keystone CR with
// PolicyOverrides set (CC-0058).
func policyValidationKeystoneWithPolicy() *keystonev1alpha1.Keystone {
	ks := policyValidationKeystone()
	ks.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{
			"identity:get_project": "role:admin or role:member",
		},
	}
	return ks
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

// completedPolicyValidationJob returns a validation Job that matches what
// buildPolicyValidationJob produces and is marked as complete with the correct
// pod-spec hash (CC-0058).
func completedPolicyValidationJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	j := buildPolicyValidationJob(ks, "keystone-config-abc123")
	now := metav1.Now()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&j.Spec.Template),
	}
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	return j
}

// failedPolicyValidationJob returns a validation Job that is marked as
// permanently failed (CC-0058).
func failedPolicyValidationJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	j := buildPolicyValidationJob(ks, "keystone-config-abc123")
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&j.Spec.Template),
	}
	j.Status.Failed = 3
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
	}
	return j
}

// runningPolicyValidationJob returns a validation Job that exists but is still
// running (no completion or failure conditions) (CC-0058).
func runningPolicyValidationJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	j := buildPolicyValidationJob(ks, "keystone-config-abc123")
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&j.Spec.Template),
	}
	return j
}

// --- Path 1: policyOverrides nil — skip validation (REQ-003) ---

// TestReconcilePolicyValidation_NoPolicyOverrides_SkipsValidation verifies that
// when policyOverrides is nil, no Job is created and PolicyValidReady=True with
// reason PolicyValidationNotRequired (CC-0058, REQ-003).
func TestReconcilePolicyValidation_NoPolicyOverrides_SkipsValidation(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystone()
	ks.Spec.PolicyOverrides = nil

	s := policyValidationTestScheme()
	r := newPolicyValidationTestReconciler(s, ks)

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil(), "PolicyValidReady condition should be set")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonPolicyValidationNotRequired))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

// TestReconcilePolicyValidation_PolicyRemoved_JobCleanedUp verifies that when
// policyOverrides is nil and a validation Job exists from a previous reconcile,
// the Job is deleted and PolicyValidReady=True/PolicyValidationNotRequired (CC-0058, REQ-003).
func TestReconcilePolicyValidation_PolicyRemoved_JobCleanedUp(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone() // nil policyOverrides

	// Pre-create a validation Job as if policyOverrides was previously set.
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-policy-validation",
			Namespace: "default",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "validator",
						Image: "ghcr.io/c5c3/keystone:2025.2",
					}},
				},
			},
		},
	}
	r := newPolicyValidationTestReconciler(s, ks, existingJob)
	ctx := context.Background()

	// Verify Job exists before reconcile.
	var j batchv1.Job
	g.Expect(r.Client.Get(ctx, client.ObjectKey{
		Name: "test-keystone-policy-validation", Namespace: "default",
	}, &j)).To(Succeed())

	// Reconcile with nil policyOverrides should delete the Job.
	result, err := r.reconcilePolicyValidation(ctx, ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Verify Job was deleted.
	err = r.Get(ctx, client.ObjectKey{
		Name: "test-keystone-policy-validation", Namespace: "default",
	}, &j)
	g.Expect(err).To(HaveOccurred())
	g.Expect(client.IgnoreNotFound(err)).To(Succeed())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonPolicyValidationNotRequired))
}

// TestReconcilePolicyValidation_PolicyRemoved_NoJob_NoError verifies that when
// policyOverrides is nil and no validation Job exists, no error occurs and
// PolicyValidReady=True/PolicyValidationNotRequired (CC-0058, REQ-003).
func TestReconcilePolicyValidation_PolicyRemoved_NoJob_NoError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystone() // nil policyOverrides, no Job in cluster

	r := newPolicyValidationTestReconciler(s, ks)

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonPolicyValidationNotRequired))
	g.Expect(cond.Message).To(Equal("No policy overrides configured"))
}

// --- Path 2: policyOverrides set — run validation Job (REQ-001) ---

// TestReconcilePolicyValidation_PolicySet_JobCreated verifies that when
// policyOverrides is set and no Job exists, a validation Job is created and
// PolicyValidReady=False/PolicyValidationInProgress with requeue (CC-0058, REQ-001).
func TestReconcilePolicyValidation_PolicySet_JobCreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystoneWithPolicy()

	r := newPolicyValidationTestReconciler(s, ks)

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueValidationWait))

	// Verify the Job was created.
	var createdJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-policy-validation",
		Namespace: "default",
	}, &createdJob)).To(Succeed())

	// Verify pod-spec hash annotation.
	g.Expect(createdJob.Annotations).To(HaveKey(job.PodSpecHashAnnotation))

	// Verify PolicyValidReady condition is False/InProgress.
	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonPolicyValidationInProgress))
}

// --- Job lifecycle paths (REQ-002) ---

// TestReconcilePolicyValidation_JobRunning_Requeues verifies that when the
// validation Job exists but is still running, result has
// RequeueAfter=RequeueValidationWait and PolicyValidReady=False/PolicyValidationInProgress
// (CC-0058, REQ-002).
func TestReconcilePolicyValidation_JobRunning_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystoneWithPolicy()

	r := newPolicyValidationTestReconciler(s, ks, runningPolicyValidationJob(ks))

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueValidationWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonPolicyValidationInProgress))
	g.Expect(cond.Message).To(Equal("Policy validation job is running"))
}

// TestReconcilePolicyValidation_JobComplete_ConditionTrue verifies that when
// the validation Job completes successfully, PolicyValidReady=True/PolicyValidationPassed
// with ObservedGeneration matching CR generation (CC-0058, REQ-002).
func TestReconcilePolicyValidation_JobComplete_ConditionTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystoneWithPolicy()

	r := newPolicyValidationTestReconciler(s, ks, completedPolicyValidationJob(ks))

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonPolicyValidationPassed))
	g.Expect(cond.Message).To(Equal("Policy validation completed successfully"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

// TestReconcilePolicyValidation_JobFailed_ConditionFalse verifies that when the
// validation Job fails, PolicyValidReady=False/PolicyValidationFailed and error
// is returned wrapping job.ErrJobFailed (CC-0058, REQ-002).
func TestReconcilePolicyValidation_JobFailed_ConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystoneWithPolicy()

	r := newPolicyValidationTestReconciler(s, ks, failedPolicyValidationJob(ks))

	_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, job.ErrJobFailed)).To(BeTrue())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonPolicyValidationFailed))
}

// --- Descriptive error extraction (REQ-006) ---

// TestReconcilePolicyValidation_JobFailed_DescriptiveErrorMessage verifies that
// when the validation Job fails, the PolicyValidReady condition message includes
// the specific error output from the Pod's termination message, not a generic
// failure message (CC-0058, REQ-006).
func TestReconcilePolicyValidation_JobFailed_DescriptiveErrorMessage(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystoneWithPolicy()

	failedJob := failedPolicyValidationJob(ks)

	// Create a Pod associated with the failed Job via the job-name label.
	// The termination message is populated by the kubelet from stderr when
	// terminationMessagePolicy=FallbackToLogsOnError is set on the container.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-policy-validation-abc12",
			Namespace: "default",
			Labels:    map[string]string{"job-name": "test-keystone-policy-validation"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "validator",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Message:  "oslopolicy-validator: error: Unknown action name 'identity:get_nonexistent'",
						ExitCode: 1,
					},
				},
			}},
		},
	}

	r := newPolicyValidationTestReconciler(s, ks, failedJob, pod)

	_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, job.ErrJobFailed)).To(BeTrue())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonPolicyValidationFailed))
	g.Expect(cond.Message).To(HavePrefix("Policy validation failed: "))
	g.Expect(cond.Message).To(ContainSubstring("Unknown action name 'identity:get_nonexistent'"))
}

// TestReconcilePolicyValidation_JobFailed_FallbackMessage verifies that when
// the validation Job fails but no Pod termination message is available, the
// condition message falls back to referencing the Job name and namespace for
// manual log inspection (CC-0058, REQ-006).
func TestReconcilePolicyValidation_JobFailed_FallbackMessage(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystoneWithPolicy()

	failedJob := failedPolicyValidationJob(ks)
	// No Pod created — simulates termination message unavailable.

	r := newPolicyValidationTestReconciler(s, ks, failedJob)

	_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonPolicyValidationFailed))
	g.Expect(cond.Message).To(ContainSubstring("kubectl logs"))
	g.Expect(cond.Message).To(ContainSubstring("test-keystone-policy-validation"))
	g.Expect(cond.Message).To(ContainSubstring("default"))
}

// --- Stale Job detection (REQ-005) ---

// TestReconcilePolicyValidation_StaleJob_DeletedAndRecreated verifies that when
// a completed Job has a different pod-spec hash (e.g., ConfigMap name changed),
// the old Job is deleted and a new one is created (CC-0058, REQ-005).
func TestReconcilePolicyValidation_StaleJob_DeletedAndRecreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystoneWithPolicy()

	// Create a completed Job with a stale hash (simulating a spec change).
	staleJob := completedPolicyValidationJob(ks)
	staleJob.Annotations[job.PodSpecHashAnnotation] = "stale-hash-from-previous-spec"

	r := newPolicyValidationTestReconciler(s, ks, staleJob)

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueValidationWait))

	// Verify the old Job was deleted and a new one was created.
	var newJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-policy-validation",
		Namespace: "default",
	}, &newJob)).To(Succeed())

	// The new Job should have the correct hash.
	desired := buildPolicyValidationJob(ks, "keystone-config-abc123")
	expectedHash := job.PodSpecHash(&desired.Spec.Template)
	g.Expect(newJob.Annotations[job.PodSpecHashAnnotation]).To(Equal(expectedHash))
}

// --- buildPolicyValidationJob specification tests (REQ-007) ---

// TestBuildPolicyValidationJob_ImageMatchesDeployment verifies that the
// validation Job container image is {spec.image.repository}:{spec.image.tag},
// matching the API Deployment (CC-0058, REQ-007).
func TestBuildPolicyValidationJob_ImageMatchesDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystoneWithPolicy()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "validator")
	g.Expect(container).NotTo(BeNil(), "validator container must exist")
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/keystone:2025.2"))
}

// TestBuildPolicyValidationJob_SecurityContext verifies that the validator
// container SecurityContext satisfies PSS Restricted profile (CC-0058, REQ-007).
func TestBuildPolicyValidationJob_SecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystoneWithPolicy()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "validator")
	expectRestrictedSecurityContext(g, container)
}

// TestBuildPolicyValidationJob_ConfigMapMount verifies that the ConfigMap is
// mounted at /etc/keystone/keystone.conf.d/ with readOnly=true and the volume
// references the ConfigMap by exact name (CC-0058, REQ-007).
func TestBuildPolicyValidationJob_ConfigMapMount(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystoneWithPolicy()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "validator")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.VolumeMounts).To(HaveLen(1))
	g.Expect(container.VolumeMounts[0].Name).To(Equal("config"))
	g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(container.VolumeMounts[0].ReadOnly).To(BeTrue())

	// Verify volume references the ConfigMap by exact name.
	g.Expect(j.Spec.Template.Spec.Volumes).To(HaveLen(1))
	g.Expect(j.Spec.Template.Spec.Volumes[0].Name).To(Equal("config"))
	g.Expect(j.Spec.Template.Spec.Volumes[0].ConfigMap.Name).To(Equal("keystone-config-abc123"))
}

// TestBuildPolicyValidationJob_BackoffAndTTL verifies backoffLimit=2,
// restartPolicy=Never, and that ttlSecondsAfterFinished is unset so the
// completed Job lingers as the RunJob state record (CC-0113, #415).
func TestBuildPolicyValidationJob_BackoffAndTTL(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystoneWithPolicy()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	g.Expect(j.Spec.BackoffLimit).NotTo(BeNil())
	g.Expect(*j.Spec.BackoffLimit).To(Equal(int32(2)))

	g.Expect(j.Spec.TTLSecondsAfterFinished).To(BeNil())

	g.Expect(j.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
}

// TestBuildPolicyValidationJob_Command verifies that the container command runs
// oslopolicy-validator with --namespace keystone and --config-dir pointing to
// the ConfigMap mount. The validator auto-discovers keystone.conf in the dir
// and reads the [oslo_policy] policy_file setting to locate policy.yaml
// (CC-0058, REQ-007).
func TestBuildPolicyValidationJob_Command(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystoneWithPolicy()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "validator")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Command).To(Equal([]string{
		"oslopolicy-validator",
		"--namespace", "keystone",
		"--config-dir", "/etc/keystone/keystone.conf.d/",
	}))
}

// TestBuildPolicyValidationJob_TerminationMessagePolicy verifies that the
// container has terminationMessagePolicy=FallbackToLogsOnError set, enabling
// descriptive error extraction from failed pods (CC-0058, REQ-007).
func TestBuildPolicyValidationJob_TerminationMessagePolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := policyValidationKeystoneWithPolicy()

	j := buildPolicyValidationJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "validator")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.TerminationMessagePolicy).To(Equal(corev1.TerminationMessageFallbackToLogsOnError))
}

// --- Cross-cutting: ObservedGeneration (REQ-009) ---

// TestReconcilePolicyValidation_ConditionObservedGeneration verifies that all
// condition updates include ObservedGeneration matching the Keystone CR's
// current generation (CC-0058, REQ-009).
func TestReconcilePolicyValidation_ConditionObservedGeneration(t *testing.T) {
	s := policyValidationTestScheme()

	// Nil policyOverrides path.
	t.Run("nil_policy", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := policyValidationKeystone()
		ks.Generation = 5
		r := newPolicyValidationTestReconciler(s, ks)

		_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
		g.Expect(err).NotTo(HaveOccurred())
		cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.ObservedGeneration).To(Equal(int64(5)))
	})

	// Job created (in-progress) path.
	t.Run("job_created", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := policyValidationKeystoneWithPolicy()
		ks.Generation = 7
		r := newPolicyValidationTestReconciler(s, ks)

		_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
		g.Expect(err).NotTo(HaveOccurred())
		cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))
	})

	// Job complete path.
	t.Run("job_complete", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := policyValidationKeystoneWithPolicy()
		ks.Generation = 9
		r := newPolicyValidationTestReconciler(s, ks, completedPolicyValidationJob(ks))

		_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
		g.Expect(err).NotTo(HaveOccurred())
		cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.ObservedGeneration).To(Equal(int64(9)))
	})

	// Job failed path.
	t.Run("job_failed", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := policyValidationKeystoneWithPolicy()
		ks.Generation = 11
		r := newPolicyValidationTestReconciler(s, ks, failedPolicyValidationJob(ks))

		_, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
		g.Expect(err).To(HaveOccurred())
		cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.ObservedGeneration).To(Equal(int64(11)))
	})
}

// TestReconcilePolicyValidation_CompletedSameHash_NotRecreated verifies the
// CC-0113 steady state (#415, REQ-005): with policy overrides configured and a
// completed validation Job whose pod-spec hash matches the desired spec, the
// reconciler must NOT delete or recreate the Job. Since TTL was removed, the
// completed Job lingers as the RunJob pod-spec-hash state record and must carry
// no TTL. Recreation is detected via the retained Job's UID staying unchanged.
func TestReconcilePolicyValidation_CompletedSameHash_NotRecreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := policyValidationTestScheme()
	ks := policyValidationKeystoneWithPolicy()

	completed := completedPolicyValidationJob(ks)
	completed.UID = "policy-validation-job-uid"
	originalUID := completed.UID

	r := newPolicyValidationTestReconciler(s, ks, completed)

	result, err := r.reconcilePolicyValidation(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	// Steady state: no churn, no requeue.
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePolicyValidReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonPolicyValidationPassed))

	// The completed validation Job must still exist with the SAME UID,
	// proving it was neither deleted nor recreated.
	var retained batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-policy-validation",
		Namespace: "default",
	}, &retained)).To(Succeed())
	g.Expect(retained.UID).To(Equal(originalUID))

	// The lingering Job carries no TTL (CC-0113: TTL removed to stop the loop).
	g.Expect(retained.Spec.TTLSecondsAfterFinished).To(BeNil())
}
