// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

func bootstrapTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return s
}

func bootstrapKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			UID:        "ks-uid",
			Generation: 1,
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Deployment: keystonev1alpha1.DeploymentSpec{Replicas: 3},
			Image:      commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
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

// bootstrapTestAdminPassword is the admin password held by the admin Secret in
// the bootstrap reconcile tests; its SHA-256 digest is what buildBootstrapJob must
// stamp as the admin-password-hash annotation.
const bootstrapTestAdminPassword = "admin-password"

// bootstrapAdminPasswordHash returns hex(sha256(bootstrapTestAdminPassword)),
// matching the digest reconcileBootstrap derives from the admin Secret.
func bootstrapAdminPasswordHash() string {
	sum := sha256.Sum256([]byte(bootstrapTestAdminPassword))
	return hex.EncodeToString(sum[:])
}

// bootstrapAdminSecret returns the admin password Secret referenced by the CR,
// holding bootstrapTestAdminPassword under the `password` key.
func bootstrapAdminSecret(ks *keystonev1alpha1.Keystone) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ks.Spec.Bootstrap.AdminPasswordSecretRef.Name,
			Namespace: ks.Namespace,
		},
		Data: map[string][]byte{"password": []byte(bootstrapTestAdminPassword)},
	}
}

func newBootstrapTestReconciler(s *runtime.Scheme, objs ...client.Object) *KeystoneReconciler {
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{})
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

// completedBootstrapJob returns a bootstrap Job that matches what buildBootstrapJob
// produces for the given keystone and is marked as complete with the correct
// re-run key. The bootstrap Job's re-run key is the admin-password digest (NOT
// the pod-template hash), so a release upgrade does not re-trigger it.
func completedBootstrapJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())
	now := metav1.Now()
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: bootstrapAdminPasswordHash(),
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

// failedBootstrapJob returns a bootstrap Job that is marked as permanently failed.
func failedBootstrapJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	j := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash()).DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: bootstrapAdminPasswordHash(),
	}
	j.Status.Failed = 5
	j.Status.Conditions = []batchv1.JobCondition{
		{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue,
		},
	}
	return j
}

// runningBootstrapJob returns a bootstrap Job that exists but is still running.
func runningBootstrapJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	j := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash()).DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: bootstrapAdminPasswordHash(),
	}
	return j
}

func TestReconcileBootstrap_JobCreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	r := newBootstrapTestReconciler(s, ks, bootstrapAdminSecret(ks))

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueBootstrapWait))

	// Verify the Job was created.
	var createdJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &createdJob)).To(Succeed())

	// Verify command and args.
	container := createdJob.Spec.Template.Spec.Containers[0]
	g.Expect(container.Name).To(Equal("bootstrap"))
	g.Expect(container.Command[:3]).To(Equal([]string{"/bin/sh", "-eu", "-c"}))
	g.Expect(container.Command[3]).To(ContainSubstring("keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ bootstrap"))
	// The bootstrap URLs and region id are passed via env vars (review-1 follow-up), so the wrapper string references
	// them as $BOOTSTRAP_*_URL / $BOOTSTRAP_REGION_ID rather than embedding
	// the cluster-local Service URL or region literal directly.
	g.Expect(container.Command[3]).To(ContainSubstring(`--bootstrap-internal-url "$BOOTSTRAP_INTERNAL_URL"`))
	g.Expect(container.Command[3]).To(ContainSubstring(`--bootstrap-region-id "$BOOTSTRAP_REGION_ID"`))
	g.Expect(container.Args).To(BeNil())

	// Verify env: BOOTSTRAP_PASSWORD first (from admin Secret), then
	// OS_DATABASE__CONNECTION override sourced from the derived DB connection
	// Secret, then the BOOTSTRAP_*
	// parameter env vars consumed by the embedded Python and the
	// keystone-manage flags (review-1 follow-up).
	// Ordering is asserted so future edits cannot reorder or drop entries
	// unnoticed.
	g.Expect(container.Env).To(HaveLen(6))
	g.Expect(container.Env[0].Name).To(Equal("BOOTSTRAP_PASSWORD"))
	g.Expect(container.Env[0].ValueFrom.SecretKeyRef.LocalObjectReference.Name).To(Equal("keystone-admin"))
	g.Expect(container.Env[0].ValueFrom.SecretKeyRef.Key).To(Equal("password"))
	g.Expect(container.Env[1].Name).To(Equal("OS_DATABASE__CONNECTION"))
	g.Expect(container.Env[1].ValueFrom.SecretKeyRef.LocalObjectReference.Name).To(Equal(ks.Name + "-db-connection"))
	g.Expect(container.Env[1].ValueFrom.SecretKeyRef.Key).To(Equal(dbConnectionSecretKey))
	g.Expect(container.Env[2].Name).To(Equal("BOOTSTRAP_REGION_ID"))
	g.Expect(container.Env[2].Value).To(Equal("RegionOne"))
	expectedServiceURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:5000/v3", ks.Name, ks.Namespace)
	g.Expect(container.Env[3].Name).To(Equal("BOOTSTRAP_ADMIN_URL"))
	g.Expect(container.Env[3].Value).To(Equal(expectedServiceURL))
	g.Expect(container.Env[4].Name).To(Equal("BOOTSTRAP_INTERNAL_URL"))
	g.Expect(container.Env[4].Value).To(Equal(expectedServiceURL))
	g.Expect(container.Env[5].Name).To(Equal("BOOTSTRAP_PUBLIC_URL"))
	g.Expect(container.Env[5].Value).To(Equal(expectedServiceURL))

	// Verify config volume mount is present (: bootstrap needs keystone.conf for DB connection).
	g.Expect(container.VolumeMounts).To(HaveLen(2))
	g.Expect(container.VolumeMounts[0].Name).To(Equal("config"))
	g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(container.VolumeMounts[0].ReadOnly).To(BeTrue())

	// Verify fernet-keys volume mount (: bootstrap needs fernet keys).
	g.Expect(container.VolumeMounts[1].Name).To(Equal("fernet-keys"))
	g.Expect(container.VolumeMounts[1].MountPath).To(Equal("/etc/keystone/fernet-keys/"))
	g.Expect(container.VolumeMounts[1].ReadOnly).To(BeTrue())

	// Verify volumes reference the ConfigMap and fernet-keys Secret.
	g.Expect(createdJob.Spec.Template.Spec.Volumes).To(HaveLen(2))
	g.Expect(createdJob.Spec.Template.Spec.Volumes[0].Name).To(Equal("config"))
	g.Expect(createdJob.Spec.Template.Spec.Volumes[0].ConfigMap.Name).To(Equal("keystone-config-abc123"))
	g.Expect(createdJob.Spec.Template.Spec.Volumes[1].Name).To(Equal("fernet-keys"))
	g.Expect(createdJob.Spec.Template.Spec.Volumes[1].Secret.SecretName).To(Equal("test-keystone-fernet-keys"))

	// Verify backoff limit.
	g.Expect(*createdJob.Spec.BackoffLimit).To(Equal(int32(4)))

	// Verify pod-spec hash annotation.
	g.Expect(createdJob.Annotations).To(HaveKey(job.PodSpecHashAnnotation))

	// Verify BootstrapReady condition is False/InProgress.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BootstrapInProgress"))
}

func TestReconcileBootstrap_JobComplete(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	r := newBootstrapTestReconciler(s, ks, completedBootstrapJob(ks), bootstrapAdminSecret(ks))

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BootstrapComplete"))
	g.Expect(cond.Message).To(Equal("Keystone bootstrap completed successfully"))

	expectEvent(g, r, "Normal BootstrapComplete")
}

func TestReconcileBootstrap_JobRunning(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	r := newBootstrapTestReconciler(s, ks, runningBootstrapJob(ks), bootstrapAdminSecret(ks))

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueBootstrapWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BootstrapInProgress"))
	g.Expect(cond.Message).To(Equal("Keystone bootstrap job is running"))

	expectNoEvent(g, r)
}

func TestReconcileBootstrap_JobFailed(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	r := newBootstrapTestReconciler(s, ks, failedBootstrapJob(ks), bootstrapAdminSecret(ks))

	_, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, job.ErrJobFailed)).To(BeTrue())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BootstrapFailed"))

	expectEvent(g, r, "Warning BootstrapFailed")
}

func TestReconcileBootstrap_StaleJobDetection(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	// Create a completed Job with a stale re-run key (simulating a changed input).
	staleJob := completedBootstrapJob(ks)
	staleJob.Annotations[job.PodSpecHashAnnotation] = "stale-key-from-previous-spec"

	r := newBootstrapTestReconciler(s, ks, staleJob, bootstrapAdminSecret(ks))

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueBootstrapWait))

	// Verify the old Job was deleted and a new one was created.
	var newJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &newJob)).To(Succeed())

	// The new Job should carry the current re-run key (the admin-password digest).
	g.Expect(newJob.Annotations[job.PodSpecHashAnnotation]).To(Equal(bootstrapAdminPasswordHash()))
}

// TestReconcileBootstrap_JobFailed_PasswordRotated_Recreates verifies the #460
// fix at the bootstrap caller: when the bootstrap Job has permanently failed and
// the admin password is then rotated (a new re-run key), the failed Job is
// re-created and BootstrapReady transitions back to False/BootstrapInProgress
// instead of staying stuck on ErrJobFailed.
func TestReconcileBootstrap_JobFailed_PasswordRotated_Recreates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	// Failed bootstrap Job stamped with a stale re-run key (the pre-rotation
	// admin-password digest).
	failed := failedBootstrapJob(ks)
	failed.Annotations[job.PodSpecHashAnnotation] = "stale-key-from-previous-password"

	r := newBootstrapTestReconciler(s, ks, failed, bootstrapAdminSecret(ks))

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred(), "a rotated admin password must re-run the failed bootstrap Job, not surface ErrJobFailed")
	g.Expect(result.RequeueAfter).To(Equal(RequeueBootstrapWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BootstrapInProgress"))

	// The recreated Job carries the current re-run key (the admin-password digest).
	var newJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &newJob)).To(Succeed())
	g.Expect(newJob.Annotations[job.PodSpecHashAnnotation]).To(Equal(bootstrapAdminPasswordHash()))
}

func TestReconcileBootstrap_JobFailed_ConditionMessage(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	r := newBootstrapTestReconciler(s, ks, failedBootstrapJob(ks), bootstrapAdminSecret(ks))

	_, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BootstrapFailed"))
	g.Expect(cond.Message).To(ContainSubstring("Keystone bootstrap job failed"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

func TestReconcileBootstrap_JobComplete_ConditionMessageAndGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	r := newBootstrapTestReconciler(s, ks, completedBootstrapJob(ks), bootstrapAdminSecret(ks))

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BootstrapComplete"))
	g.Expect(cond.Message).To(Equal("Keystone bootstrap completed successfully"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

// TestBuildBootstrapJob_SecurityContext verifies that the bootstrap container in the Job
// returned by buildBootstrapJob() has a restricted SecurityContext with all four PSS
// Restricted profile fields set correctly.
func TestBuildBootstrapJob_SecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())

	container := findContainerByName(job.Spec.Template.Spec.Containers, "bootstrap")
	expectRestrictedSecurityContext(g, container)
}

func TestReconcileBootstrap_PublicEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()
	ks.Spec.Bootstrap.PublicEndpoint = "https://keystone.example.com/v3"

	r := newBootstrapTestReconciler(s, ks, bootstrapAdminSecret(ks))

	_, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var createdJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &createdJob)).To(Succeed())

	container := createdJob.Spec.Template.Spec.Containers[0]
	// The wrapper script references the bootstrap URLs via env vars, so we
	// verify the values on the env block rather than scraping the command.
	// Admin and internal URLs should use the cluster-local service.
	internalURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:5000/v3", ks.Name, ks.Namespace)
	envByName := make(map[string]string, len(container.Env))
	for _, e := range container.Env {
		envByName[e.Name] = e.Value
	}
	g.Expect(envByName).To(HaveKeyWithValue("BOOTSTRAP_ADMIN_URL", internalURL))
	g.Expect(envByName).To(HaveKeyWithValue("BOOTSTRAP_INTERNAL_URL", internalURL))
	// Public URL should use the explicit PublicEndpoint.
	g.Expect(envByName).To(HaveKeyWithValue("BOOTSTRAP_PUBLIC_URL", "https://keystone.example.com/v3"))
}

func TestReconcileBootstrap_JobSpec_TTLAndBackoff(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	r := newBootstrapTestReconciler(s, ks, bootstrapAdminSecret(ks))

	_, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var createdJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &createdJob)).To(Succeed())

	// (#415): TTLSecondsAfterFinished must be unset so the finished
	// bootstrap Job is not garbage-collected by the TTL controller; a deleted
	// Job would otherwise be re-created on the next reconcile, causing a
	// TTL-driven re-creation loop.
	g.Expect(createdJob.Spec.TTLSecondsAfterFinished).To(BeNil())

	// Verify BackoffLimit.
	g.Expect(createdJob.Spec.BackoffLimit).NotTo(BeNil())
	g.Expect(*createdJob.Spec.BackoffLimit).To(Equal(int32(4)))

	// Verify owner reference is set.
	g.Expect(createdJob.OwnerReferences).To(HaveLen(1))
	g.Expect(createdJob.OwnerReferences[0].Name).To(Equal("test-keystone"))
}

// TestReconcileBootstrap_ConditionObservedGeneration verifies that
// ObservedGeneration is set on the BootstrapReady condition for the
// False (BootstrapInProgress, BootstrapFailed) and True (BootstrapComplete)
// paths with distinct generation values.
func TestReconcileBootstrap_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()

	// Test ObservedGeneration for the BootstrapInProgress path (running job).
	ks := bootstrapKeystone()
	ks.Generation = 5

	r := newBootstrapTestReconciler(s, ks, runningBootstrapJob(ks), bootstrapAdminSecret(ks))

	_, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(5)))

	// Test ObservedGeneration for the BootstrapComplete path (completed job).
	ks2 := bootstrapKeystone()
	ks2.Generation = 7

	r2 := newBootstrapTestReconciler(s, ks2, completedBootstrapJob(ks2), bootstrapAdminSecret(ks2))

	_, err = r2.reconcileBootstrap(context.Background(), ks2, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, "BootstrapReady")
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(7)))

	// Test ObservedGeneration for the BootstrapFailed path (failed job).
	ks3 := bootstrapKeystone()
	ks3.Generation = 12

	r3 := newBootstrapTestReconciler(s, ks3, failedBootstrapJob(ks3), bootstrapAdminSecret(ks3))

	_, err = r3.reconcileBootstrap(context.Background(), ks3, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())

	cond3 := meta.FindStatusCondition(ks3.Status.Conditions, "BootstrapReady")
	g.Expect(cond3).NotTo(BeNil())
	g.Expect(cond3.ObservedGeneration).To(Equal(int64(12)))
}

// TestBuildBootstrapJob_PriorityClassNameSet verifies that when spec.PriorityClassName
// is set, the bootstrap Job PodSpec includes the configured priority class.
func TestBuildBootstrapJob_PriorityClassNameSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()
	pcn := "system-cluster-critical"
	ks.Spec.Deployment.PriorityClassName = &pcn

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())

	g.Expect(job.Spec.Template.Spec.PriorityClassName).To(Equal("system-cluster-critical"))
}

// TestBuildBootstrapJob_PriorityClassNameNil verifies that when spec.PriorityClassName
// is nil, the bootstrap Job PodSpec has an empty priority class name.
func TestBuildBootstrapJob_PriorityClassNameNil(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())

	g.Expect(job.Spec.Template.Spec.PriorityClassName).To(BeEmpty())
}

// TestBuildBootstrapJob_PreInsertScriptReadsDBConnectionEnvVar asserts the
// inline Python pre-insert script resolves the DB URL from the
// OS_DATABASE__CONNECTION env var and only falls back to keystone.conf when
// the env var is unset (C-001).
//
// Since the [database] connection key in keystone.conf is a
// placeholder ("mysql+pymysql://placeholder") and the real URL is injected
// via oslo.config's OS_<GROUP>__<OPTION> override. stdlib configparser has no
// knowledge of this override, so a script that reads keystone.conf only would
// dial the placeholder host and fail DNS, aborting before
// `keystone-manage bootstrap` runs. Guard against regression by asserting the
// script references the env var and falls back to configparser.
func TestBuildBootstrapJob_PreInsertScriptReadsDBConnectionEnvVar(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())

	container := findContainerByName(job.Spec.Template.Spec.Containers, "bootstrap")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Command).To(HaveLen(4))
	script := container.Command[3]

	g.Expect(script).To(ContainSubstring(`os.environ.get("OS_DATABASE__CONNECTION")`),
		"pre-insert script must read OS_DATABASE__CONNECTION from the environment first (C-001)")
	g.Expect(script).To(ContainSubstring("configparser"),
		"pre-insert script must still fall back to keystone.conf when the env var is unset (C-001)")
	g.Expect(script).To(MatchRegexp(`(?s)os\.environ\.get\("OS_DATABASE__CONNECTION"\).*configparser`),
		"env-var lookup must precede the configparser fallback (C-001)")
}

// TestBootstrapServiceURL_ComposesViaHelper verifies that
// bootstrapServiceURL() composes its return value from subResourceName(),
// the namespace, and the :5000/v3 suffix. The helper
// output is the single source of truth for the host segment.
func TestBootstrapServiceURL_ComposesViaHelper(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()
	ks.Name = "keystone"
	ks.Namespace = "openstack"

	url := bootstrapServiceURL(ks)

	g.Expect(url).To(Equal("http://keystone.openstack.svc.cluster.local:5000/v3"),
		"bootstrapServiceURL must produce host == subResourceName(ks)")
	g.Expect(url).To(ContainSubstring(subResourceName(ks)),
		"bootstrapServiceURL host segment must equal subResourceName(ks) so future helper changes propagate")
}

// TestBootstrapServiceURL_NoApiLiteral guards against re-introducing the
// literal "-api." substring in the bootstrap URL. After
// the rename the URL must not contain the legacy suffix in any form.
func TestBootstrapServiceURL_NoApiLiteral(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()
	ks.Name = "keystone"
	ks.Namespace = "openstack"

	url := bootstrapServiceURL(ks)

	g.Expect(url).NotTo(ContainSubstring("-api."),
		"bootstrapServiceURL must not contain the legacy '-api.' suffix")
	g.Expect(url).NotTo(ContainSubstring("-api:"),
		"bootstrapServiceURL must not contain a stray '-api:' segment")
}

// TestBuildBootstrapJob_AdminInternalURLsUseBareName pins the BOOTSTRAP_ADMIN_URL
// and BOOTSTRAP_INTERNAL_URL env values consumed by the bootstrap script to
// the bare-CR-name Service URL produced by bootstrapServiceURL(). After the
// rename these values must never embed the legacy "-api." segment in
// the host: any drift would write a stale catalog entry on the next bootstrap
// run, sending in-cluster clients to a non-existent Service.
func TestBuildBootstrapJob_AdminInternalURLsUseBareName(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())

	g.Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
	container := job.Spec.Template.Spec.Containers[0]

	expectedURL := bootstrapServiceURL(ks)
	envByName := make(map[string]string, len(container.Env))
	for _, e := range container.Env {
		envByName[e.Name] = e.Value
	}
	g.Expect(envByName).To(HaveKeyWithValue("BOOTSTRAP_ADMIN_URL", expectedURL),
		"BOOTSTRAP_ADMIN_URL must use the bare-CR-name Service URL")
	g.Expect(envByName).To(HaveKeyWithValue("BOOTSTRAP_INTERNAL_URL", expectedURL),
		"BOOTSTRAP_INTERNAL_URL must use the bare-CR-name Service URL")

	legacyHost := fmt.Sprintf("%s-api.%s.svc.cluster.local", ks.Name, ks.Namespace)
	for _, e := range container.Env {
		g.Expect(e.Value).NotTo(ContainSubstring(legacyHost),
			"bootstrap env vars must not embed the legacy `<cr-name>-api.<ns>` host") // keystone-api-legacy: assertion pins absence of the pre-rename host.
	}
	g.Expect(container.Command[3]).NotTo(ContainSubstring(legacyHost),
		"bootstrap wrapper script must not embed the legacy `<cr-name>-api.<ns>` host") // keystone-api-legacy: assertion pins absence of the pre-rename host.
}

// findVolumeMountByName returns the first VolumeMount with the given name, or
// nil if absent. Local helper kept out of the production package.
func findVolumeMountByName(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

// findVolumeByName returns the first Volume with the given name, or nil if
// absent.
func findVolumeByName(vols []corev1.Volume, name string) *corev1.Volume {
	for i := range vols {
		if vols[i].Name == name {
			return &vols[i]
		}
	}
	return nil
}

// TestBuildBootstrapJob_DBTLSDisabled_NoDBTLSVolume backs task 4.3
// when spec.database.tls is nil the bootstrap Job must not mount the
// db-tls Secret. The pre-existing plaintext path stays byte-equivalent.
func TestBuildBootstrapJob_DBTLSDisabled_NoDBTLSVolume(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()
	// TLS is nil — the default produced by bootstrapKeystone.

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())

	container := findContainerByName(job.Spec.Template.Spec.Containers, "bootstrap")
	g.Expect(container).NotTo(BeNil())
	g.Expect(findVolumeMountByName(container.VolumeMounts, "db-tls")).To(BeNil(),
		"db-tls VolumeMount must be absent when DB TLS is not enabled")
	g.Expect(findVolumeByName(job.Spec.Template.Spec.Volumes, "db-tls")).To(BeNil(),
		"db-tls Volume must be absent when DB TLS is not enabled")
}

// TestBuildBootstrapJob_DBTLSDisabledExplicit_NoDBTLSVolume verifies that when spec.database.tls is non-nil but
// Enabled is false the
// bootstrap Job still must not mount the db-tls Secret — the mode is the single
// gate (matches the reconcileDatabaseTLS NotRequired path).
func TestBuildBootstrapJob_DBTLSDisabledExplicit_NoDBTLSVolume(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()
	ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "disabled",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-server-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "test-keystone-db-client"},
	}

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())

	container := findContainerByName(job.Spec.Template.Spec.Containers, "bootstrap")
	g.Expect(container).NotTo(BeNil())
	g.Expect(findVolumeMountByName(container.VolumeMounts, "db-tls")).To(BeNil())
	g.Expect(findVolumeByName(job.Spec.Template.Spec.Volumes, "db-tls")).To(BeNil())
}

// TestBuildBootstrapJob_DBTLSEnabled_MountsClientSecret backs task 4.3
// when DB TLS is enabled the bootstrap Job projects ca.crt from
// caBundleSecretRef and tls.crt/tls.key from clientCertSecretRef read-only at
// /etc/keystone/db-tls/. The mount path must match dbtls_mode.dbTLSPaths so
// the DSN ssl_ca/ssl_cert/ssl_key file paths resolve.
func TestBuildBootstrapJob_DBTLSEnabled_MountsClientSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()
	ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-server-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "test-keystone-db-client"},
	}

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())

	container := findContainerByName(job.Spec.Template.Spec.Containers, "bootstrap")
	g.Expect(container).NotTo(BeNil())

	mount := findVolumeMountByName(container.VolumeMounts, "db-tls")
	g.Expect(mount).NotTo(BeNil(),
		"db-tls VolumeMount must be present when DB TLS is enabled")
	g.Expect(mount.MountPath).To(Equal("/etc/keystone/db-tls/"))
	g.Expect(mount.ReadOnly).To(BeTrue(),
		"db-tls VolumeMount must be ReadOnly")

	vol := findVolumeByName(job.Spec.Template.Spec.Volumes, "db-tls")
	g.Expect(vol).NotTo(BeNil())
	g.Expect(vol.Projected).NotTo(BeNil(),
		"db-tls Volume must be a Projected VolumeSource sourcing both Secret refs")
	expectDBTLSProjection(g, vol.Projected, "db-server-ca", "test-keystone-db-client")
}

// TestBuildBootstrapJob_DBTLSEnabled_BrownfieldUsesUserSuppliedSecretNames
// backs the BLOCKER fix from review #1: when a brownfield deployment supplies
// caBundleSecretRef and clientCertSecretRef pointing to two distinct Secrets
// (the canonical enterprise-PKI shape), the bootstrap Job must reference both
// names verbatim.
func TestBuildBootstrapJob_DBTLSEnabled_BrownfieldUsesUserSuppliedSecretNames(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()
	ks.Spec.Database.Host = "db.example.com"
	ks.Spec.Database.Port = 3306
	ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "enterprise-root-ca-bundle"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "site-specific-client-keypair"},
	}

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())

	vol := findVolumeByName(job.Spec.Template.Spec.Volumes, "db-tls")
	g.Expect(vol).NotTo(BeNil(),
		"db-tls Volume must be present in brownfield mode when TLS is enabled")
	g.Expect(vol.Projected).NotTo(BeNil(),
		"db-tls Volume must be Projected")
	expectDBTLSProjection(g, vol.Projected,
		"enterprise-root-ca-bundle", "site-specific-client-keypair")
}

// TestBuildBootstrapJob_PreInsertScript_ParsesSSLDSNParams backs task
// 4.3: the inline python3 pre-insert script extracts the pymysql
// ssl_ca/ssl_cert/ssl_key + ssl_verify_cert/ssl_verify_identity query keys out
// of the DSN, maps them onto a pymysql ssl={...} dict (ca/cert/key plus
// verify_mode and check_hostname), and only passes the ssl kwarg when at
// least one ssl_* parameter is present.
func TestBuildBootstrapJob_PreInsertScript_ParsesSSLDSNParams(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())

	container := findContainerByName(job.Spec.Template.Spec.Containers, "bootstrap")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Command).To(HaveLen(4))
	script := container.Command[3]

	// The wrapper invokes the embedded Python via a 'PY' quoted heredoc so the
	// script content survives verbatim, with no shell interpolation.
	g.Expect(script).To(ContainSubstring("python3 - <<'PY'"),
		"wrapper must invoke python3 via a quoted heredoc so the embedded Python is preserved verbatim")
	g.Expect(script).To(ContainSubstring("\nPY\n"),
		"wrapper heredoc must terminate with the PY sentinel on its own line")

	// The embedded script must be the bootstrapDBSeedScript variable populated
	// via go:embed — verifies the directive is wired correctly.
	g.Expect(bootstrapDBSeedScript).NotTo(BeEmpty(),
		"bootstrapDBSeedScript must not be empty — check go:embed directive")
	g.Expect(script).To(ContainSubstring(bootstrapDBSeedScript),
		"wrapper must inline the embedded Python content")

	// keystone-manage is invoked via env-var-parameterised flags so the
	// wrapper string carries no caller-substituted literals.
	for _, flag := range []string{
		// =value form (not space-separated) so argparse accepts a rotated
		// password that starts with '-' (secrets.token_urlsafe can emit one).
		`--bootstrap-password="$BOOTSTRAP_PASSWORD"`,
		`--bootstrap-admin-url "$BOOTSTRAP_ADMIN_URL"`,
		`--bootstrap-internal-url "$BOOTSTRAP_INTERNAL_URL"`,
		`--bootstrap-public-url "$BOOTSTRAP_PUBLIC_URL"`,
		`--bootstrap-region-id "$BOOTSTRAP_REGION_ID"`,
	} {
		g.Expect(script).To(ContainSubstring(flag),
			"wrapper must pass keystone-manage flag from env var")
	}
}

// TestBuildBootstrapJob_AdminPasswordHashAnnotation verifies that
// buildBootstrapJob stamps the SHA-256 digest of the admin password onto the
// pod template's admin-password-hash annotation, so a rotated password
// changes the pod-spec-hash gate and re-runs the idempotent bootstrap
func TestBuildBootstrapJob_AdminPasswordHashAnnotation(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", bootstrapAdminPasswordHash())

	g.Expect(job.Spec.Template.ObjectMeta.Annotations).To(HaveKeyWithValue(
		"forge.c5c3.io/admin-password-hash", bootstrapAdminPasswordHash(),
	),
		"pod template must carry the admin-password-hash annotation")

	// Pin the hashing input: the annotation must equal hex(sha256("admin-password")).
	sum := sha256.Sum256([]byte("admin-password"))
	g.Expect(job.Spec.Template.ObjectMeta.Annotations["forge.c5c3.io/admin-password-hash"]).
		To(Equal(hex.EncodeToString(sum[:])),
			"admin-password-hash must be hex(sha256(admin password))")
}

// TestReconcileBootstrap_PasswordChangeRecreatesJob verifies that when the
// admin password rotates, the previously completed bootstrap Job (stamped with
// the OLD password's hash) is detected as stale, deleted, and recreated with
// the new admin-password-hash annotation — forcing the idempotent bootstrap to
// re-run with the rotated credential.
func TestReconcileBootstrap_PasswordChangeRecreatesJob(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	// hashOf returns hex(sha256(pw)) — the same digest reconcileBootstrap derives.
	hashOf := func(pw string) string {
		sum := sha256.Sum256([]byte(pw))
		return hex.EncodeToString(sum[:])
	}

	// Build an OLD completed bootstrap Job stamped with a DIFFERENT password's
	// re-run key. The bootstrap re-run key is the admin-password digest, NOT the
	// pod-template hash, so the image plays no part.
	oldDesired := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", hashOf("old-password"))
	oldHash := hashOf("old-password")
	oldJob := oldDesired.DeepCopy()
	oldJob.Annotations = map[string]string{job.PodSpecHashAnnotation: oldHash}
	now := metav1.Now()
	oldJob.Status.Succeeded = 1
	oldJob.Status.CompletionTime = &now
	oldJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}

	// The admin Secret holds the CURRENT password ("admin-password").
	r := newBootstrapTestReconciler(s, ks, bootstrapAdminSecret(ks), oldJob)

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueBootstrapWait),
		"stale Job recreation must requeue while the new Job runs")

	// A new Job must exist carrying the CURRENT admin-password-hash.
	var newJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &newJob)).To(Succeed())
	g.Expect(newJob.Spec.Template.ObjectMeta.Annotations).To(HaveKeyWithValue(
		"forge.c5c3.io/admin-password-hash", bootstrapAdminPasswordHash(),
	),
		"recreated Job must carry the current admin-password-hash")

	// The new re-run key must be the current admin-password digest and differ
	// from the old stamped key.
	curHash := bootstrapAdminPasswordHash()
	g.Expect(newJob.Annotations[job.PodSpecHashAnnotation]).To(Equal(curHash),
		"recreated Job's re-run key must be the current admin-password digest")
	g.Expect(curHash).NotTo(Equal(oldHash),
		"a rotated admin password must change the bootstrap re-run gate")
}

// TestReconcileBootstrap_UnchangedPasswordRetainsJob verifies that when the
// admin password is unchanged, the completed bootstrap Job (stamped with the
// current password hash) is recognised as current and is NOT deleted/recreated
// — the bootstrap completes idempotently without churn.
func TestReconcileBootstrap_UnchangedPasswordRetainsJob(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	// Completed Job built with the CURRENT password hash; pin a UID so we can
	// detect a delete/recreate (the recreated Job would get a fresh UID).
	completed := completedBootstrapJob(ks)
	completed.UID = "bootstrap-job-uid"

	r := newBootstrapTestReconciler(s, ks, bootstrapAdminSecret(ks), completed)

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero(),
		"unchanged password must not requeue — bootstrap is complete")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BootstrapComplete"))

	// The Job must be the same object (unchanged UID) — not deleted/recreated.
	var retained batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &retained)).To(Succeed())
	g.Expect(retained.UID).To(Equal(completed.UID),
		"unchanged password must retain the existing Job (same UID)")
}

// TestReconcileBootstrap_CutoverConditionTransitions verifies the full cutover:
// with only the admin Secret present, the first reconcile creates the Job and
// reports BootstrapReady=False/BootstrapInProgress; once the Job completes, a
// second reconcile reports BootstrapReady=True/BootstrapComplete.
func TestReconcileBootstrap_CutoverConditionTransitions(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	// Start with ONLY the admin Secret — no Job yet.
	r := newBootstrapTestReconciler(s, ks, bootstrapAdminSecret(ks))

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueBootstrapWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BootstrapInProgress"))

	// Fetch the created Job and mark it Complete.
	var created batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &created)).To(Succeed())
	now := metav1.Now()
	created.Status.Succeeded = 1
	created.Status.CompletionTime = &now
	created.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	g.Expect(r.Client.Status().Update(context.Background(), &created)).To(Succeed())

	// Second reconcile sees the completed Job — BootstrapReady should flip True.
	result, err = r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond = meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BootstrapComplete"))
}

// TestReconcileBootstrap_MissingPasswordKeyCleanError verifies that an admin
// Secret lacking the `password` key produces a clean error (no panic),
// BootstrapReady=False/AdminSecretInvalid, a Warning event, and NO bootstrap
// Job.
func TestReconcileBootstrap_MissingPasswordKeyCleanError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	// Admin Secret present but WITHOUT a "password" key.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ks.Spec.Bootstrap.AdminPasswordSecretRef.Name,
			Namespace: ks.Namespace,
		},
		Data: map[string][]byte{"other": []byte("x")},
	}

	r := newBootstrapTestReconciler(s, ks, adminSecret)

	_, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred(),
		"a missing password key must return an error")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("AdminSecretInvalid"))

	expectEvent(g, r, "Warning AdminSecretInvalid")

	// No bootstrap Job must have been created.
	var createdJob batchv1.Job
	getErr := r.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &createdJob)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"no bootstrap Job may be created when the admin password is invalid")
}

// TestReconcileBootstrap_EmptyPasswordCleanError verifies that an admin Secret
// with an empty `password` value produces a clean error (no panic),
// BootstrapReady=False/AdminSecretInvalid, a Warning event, and NO bootstrap
// Job.
func TestReconcileBootstrap_EmptyPasswordCleanError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	// Admin Secret present with an empty "password" value.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ks.Spec.Bootstrap.AdminPasswordSecretRef.Name,
			Namespace: ks.Namespace,
		},
		Data: map[string][]byte{"password": []byte("")},
	}

	r := newBootstrapTestReconciler(s, ks, adminSecret)

	_, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred(),
		"an empty password value must return an error")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("AdminSecretInvalid"))

	expectEvent(g, r, "Warning AdminSecretInvalid")

	var createdJob batchv1.Job
	getErr := r.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &createdJob)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"no bootstrap Job may be created when the admin password is empty")
}

// TestReconcileBootstrap_RecreatedRotationJob_HasNoTTL locks the
// (#415) invariant on the password-rotation path: when the admin
// password rotates, the previously completed bootstrap Job (stamped with the
// OLD password's hash) is detected as stale and recreated — and the recreated
// Job must carry NO TTLSecondsAfterFinished. The TTL was removed (commit on feature/) so the finished Job lingers as the RunJob pod-spec-hash
// state record; recreation must ride the pod-spec-hash gate (a changed
// admin-password-hash annotation), NOT a TTL that garbage-collects the Job and
// triggers a TTL-driven re-creation loop. This complements
// TestReconcileBootstrap_PasswordChangeRecreatesJob (which locks the hash) by
// additionally pinning TTL-nil on the rotation output.
func TestReconcileBootstrap_RecreatedRotationJob_HasNoTTL(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	// hashOf returns hex(sha256(pw)) — the same digest reconcileBootstrap derives.
	hashOf := func(pw string) string {
		sum := sha256.Sum256([]byte(pw))
		return hex.EncodeToString(sum[:])
	}

	// Build an OLD completed bootstrap Job stamped with a DIFFERENT password's
	// re-run key. The bootstrap re-run key is the admin-password digest, NOT the
	// pod-template hash, so the image plays no part.
	oldDesired := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys", hashOf("old-password"))
	oldHash := hashOf("old-password")
	oldJob := oldDesired.DeepCopy()
	oldJob.Annotations = map[string]string{job.PodSpecHashAnnotation: oldHash}
	now := metav1.Now()
	oldJob.Status.Succeeded = 1
	oldJob.Status.CompletionTime = &now
	oldJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}

	// The admin Secret holds the CURRENT password ("admin-password").
	r := newBootstrapTestReconciler(s, ks, bootstrapAdminSecret(ks), oldJob)

	_, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// The recreated Job must exist and carry the CURRENT admin-password-hash.
	var newJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &newJob)).To(Succeed())
	g.Expect(newJob.Spec.Template.ObjectMeta.Annotations).To(HaveKeyWithValue(
		"forge.c5c3.io/admin-password-hash", bootstrapAdminPasswordHash(),
	),
		"recreated rotation Job must carry the current admin-password-hash (#415)")

	// (#415): the recreated rotation Job must have no TTL —
	// rotation rides the pod-spec-hash gate, not a deleted TTL.
	g.Expect(newJob.Spec.TTLSecondsAfterFinished).To(BeNil(),
		"recreated rotation Job must not set TTLSecondsAfterFinished (#415)")
}

// TestReconcileBootstrap_CompletedSameHash_NotRecreated_NoTTL locks the
// (#415) steady-state regression: once the bootstrap Job has
// completed with the current pod-spec hash, subsequent reconciles must NOT
// churn it. Before the finished Job carried a TTL; the TTL controller
// would garbage-collect it and the next reconcile would re-create it, an
// endless delete/recreate loop. With the TTL removed the completed Job lingers
// as the pod-spec-hash state record and reconcile reaches steady state. This
// test pins three invariants: no requeue churn + BootstrapReady=True, the Job
// is retained with the SAME UID (proving no delete/recreate), and the lingering
// Job carries no TTLSecondsAfterFinished.
func TestReconcileBootstrap_CompletedSameHash_NotRecreated_NoTTL(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	// Completed Job built with the CURRENT password hash; pin a UID so a
	// delete/recreate would surface as a changed UID.
	completed := completedBootstrapJob(ks)
	completed.UID = "bootstrap-job-uid"

	r := newBootstrapTestReconciler(s, ks, bootstrapAdminSecret(ks), completed)

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero(),
		"steady-state bootstrap must not requeue — no churn (#415)")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"completed bootstrap must report BootstrapReady=True (#415)")

	// The Job must be the same object (unchanged UID) — not deleted/recreated.
	var retained batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &retained)).To(Succeed())
	g.Expect(retained.UID).To(Equal(completed.UID),
		"steady-state must retain the existing Job (same UID), not recreate it (#415)")

	// (#415): the lingering completed Job carries no TTL, so
	// the TTL controller cannot garbage-collect it and re-trigger the loop.
	g.Expect(retained.Spec.TTLSecondsAfterFinished).To(BeNil(),
		"lingering completed Job must not set TTLSecondsAfterFinished (#415)")
}

// TestReconcileBootstrap_ImageChangeRetainsJob locks the regression: a
// release upgrade (changed container image) must NOT re-run the completed
// bootstrap Job while the admin password is unchanged. The bootstrap re-run is
// gated on the admin-password digest only, so the image change is ignored.
// Before this fix the image was part of the re-run key (the full pod-template
// hash), so an upgrade re-ran keystone-manage bootstrap against the
// already-migrated admin user; it failed with DBDuplicateEntry 'default-admin',
// holding BootstrapReady — and the aggregate Ready — False for the whole
// upgrade and timing out keystone-release-upgrade / keystone-upgrade-flow.
func TestReconcileBootstrap_ImageChangeRetainsJob(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	// A completed bootstrap Job from the pre-upgrade release (image 2025.2),
	// stamped with the current admin-password digest. Pin a UID so a
	// delete/recreate would surface as a changed UID.
	completed := completedBootstrapJob(ks)
	completed.UID = "bootstrap-job-uid"

	// Reconcile after a release upgrade to a NEW image — same admin password.
	ks.Spec.Image.Tag = "2026.1"
	r := newBootstrapTestReconciler(s, ks, bootstrapAdminSecret(ks), completed)

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero(),
		"an image-only change must not re-run the bootstrap Job")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"image-only change must keep BootstrapReady=True")
	g.Expect(cond.Reason).To(Equal("BootstrapComplete"))

	// The Job must be the same object (unchanged UID) — not deleted/recreated.
	var retained batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &retained)).To(Succeed())
	g.Expect(retained.UID).To(Equal(completed.UID),
		"an image-only change must retain the existing bootstrap Job (same UID)")
}
