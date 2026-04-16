// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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

// Feature: CC-0013

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
		},
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
// produces for the given keystone and is marked as complete with the correct pod-spec hash.
func completedBootstrapJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys")
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

// failedBootstrapJob returns a bootstrap Job that is marked as permanently failed.
func failedBootstrapJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys")
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
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
	desired := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys")
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	return j
}

func TestReconcileBootstrap_JobCreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	r := newBootstrapTestReconciler(s, ks)

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
	expectedServiceURL := fmt.Sprintf("http://%s-api.%s.svc.cluster.local:5000/v3", ks.Name, ks.Namespace)
	g.Expect(container.Command[3]).To(ContainSubstring(expectedServiceURL))
	g.Expect(container.Command[3]).To(ContainSubstring("--bootstrap-region-id RegionOne"))
	g.Expect(container.Args).To(BeNil())

	// Verify env.
	g.Expect(container.Env).To(HaveLen(1))
	g.Expect(container.Env[0].Name).To(Equal("BOOTSTRAP_PASSWORD"))
	g.Expect(container.Env[0].ValueFrom.SecretKeyRef.LocalObjectReference.Name).To(Equal("keystone-admin"))
	g.Expect(container.Env[0].ValueFrom.SecretKeyRef.Key).To(Equal("password"))

	// Verify config volume mount is present (CC-0013: bootstrap needs keystone.conf for DB connection).
	g.Expect(container.VolumeMounts).To(HaveLen(2))
	g.Expect(container.VolumeMounts[0].Name).To(Equal("config"))
	g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(container.VolumeMounts[0].ReadOnly).To(BeTrue())

	// Verify fernet-keys volume mount (CC-0018: bootstrap needs fernet keys).
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

	r := newBootstrapTestReconciler(s, ks, completedBootstrapJob(ks))

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

	r := newBootstrapTestReconciler(s, ks, runningBootstrapJob(ks))

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

	r := newBootstrapTestReconciler(s, ks, failedBootstrapJob(ks))

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

	// Create a completed Job with a stale hash (simulating a spec change).
	staleJob := completedBootstrapJob(ks)
	staleJob.Annotations[job.PodSpecHashAnnotation] = "stale-hash-from-previous-spec"

	r := newBootstrapTestReconciler(s, ks, staleJob)

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueBootstrapWait))

	// Verify the old Job was deleted and a new one was created.
	var newJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &newJob)).To(Succeed())

	// The new Job should have the correct hash.
	desired := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys")
	expectedHash := job.PodSpecHash(&desired.Spec.Template.Spec)
	g.Expect(newJob.Annotations[job.PodSpecHashAnnotation]).To(Equal(expectedHash))
}

func TestReconcileBootstrap_JobFailed_ConditionMessage(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	r := newBootstrapTestReconciler(s, ks, failedBootstrapJob(ks))

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

	r := newBootstrapTestReconciler(s, ks, completedBootstrapJob(ks))

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
// Restricted profile fields set correctly (CC-0045: REQ-001, REQ-002, REQ-003, REQ-004).
func TestBuildBootstrapJob_SecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys")

	container := findContainerByName(job.Spec.Template.Spec.Containers, "bootstrap")
	expectRestrictedSecurityContext(g, container)
}

func TestReconcileBootstrap_PublicEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()
	ks.Spec.Bootstrap.PublicEndpoint = "https://keystone.example.com/v3"

	r := newBootstrapTestReconciler(s, ks)

	_, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var createdJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &createdJob)).To(Succeed())

	container := createdJob.Spec.Template.Spec.Containers[0]
	// Admin and internal URLs should use cluster-local service.
	internalURL := fmt.Sprintf("http://%s-api.%s.svc.cluster.local:5000/v3", ks.Name, ks.Namespace)
	g.Expect(container.Command[3]).To(ContainSubstring("--bootstrap-admin-url " + internalURL))
	g.Expect(container.Command[3]).To(ContainSubstring("--bootstrap-internal-url " + internalURL))
	// Public URL should use the explicit PublicEndpoint (CC-0013).
	g.Expect(container.Command[3]).To(ContainSubstring("--bootstrap-public-url https://keystone.example.com/v3"))
}

func TestReconcileBootstrap_JobSpec_TTLAndBackoff(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	r := newBootstrapTestReconciler(s, ks)

	_, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var createdJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &createdJob)).To(Succeed())

	// Verify TTLSecondsAfterFinished.
	g.Expect(createdJob.Spec.TTLSecondsAfterFinished).NotTo(BeNil())
	g.Expect(*createdJob.Spec.TTLSecondsAfterFinished).To(Equal(int32(300)))

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
// paths with distinct generation values (CC-0072, REQ-002, REQ-003).
func TestReconcileBootstrap_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()

	// Test ObservedGeneration for the BootstrapInProgress path (running job).
	ks := bootstrapKeystone()
	ks.Generation = 5

	r := newBootstrapTestReconciler(s, ks, runningBootstrapJob(ks))

	_, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(5)))

	// Test ObservedGeneration for the BootstrapComplete path (completed job).
	ks2 := bootstrapKeystone()
	ks2.Generation = 7

	r2 := newBootstrapTestReconciler(s, ks2, completedBootstrapJob(ks2))

	_, err = r2.reconcileBootstrap(context.Background(), ks2, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, "BootstrapReady")
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(7)))

	// Test ObservedGeneration for the BootstrapFailed path (failed job).
	ks3 := bootstrapKeystone()
	ks3.Generation = 12

	r3 := newBootstrapTestReconciler(s, ks3, failedBootstrapJob(ks3))

	_, err = r3.reconcileBootstrap(context.Background(), ks3, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())

	cond3 := meta.FindStatusCondition(ks3.Status.Conditions, "BootstrapReady")
	g.Expect(cond3).NotTo(BeNil())
	g.Expect(cond3.ObservedGeneration).To(Equal(int64(12)))
}

// TestBuildBootstrapJob_PriorityClassNameSet verifies that when spec.PriorityClassName
// is set, the bootstrap Job PodSpec includes the configured priority class (CC-0075).
func TestBuildBootstrapJob_PriorityClassNameSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()
	pcn := "system-cluster-critical"
	ks.Spec.PriorityClassName = &pcn

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys")

	g.Expect(job.Spec.Template.Spec.PriorityClassName).To(Equal("system-cluster-critical"))
}

// TestBuildBootstrapJob_PriorityClassNameNil verifies that when spec.PriorityClassName
// is nil, the bootstrap Job PodSpec has an empty priority class name (CC-0075).
func TestBuildBootstrapJob_PriorityClassNameNil(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := bootstrapKeystone()

	job := buildBootstrapJob(ks, "keystone-config-abc123", "test-keystone-fernet-keys")

	g.Expect(job.Spec.Template.Spec.PriorityClassName).To(BeEmpty())
}
