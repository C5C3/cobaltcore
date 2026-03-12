// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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
	desired := buildBootstrapJob(ks, "keystone-config-abc123")
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
	desired := buildBootstrapJob(ks, "keystone-config-abc123")
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
	desired := buildBootstrapJob(ks, "keystone-config-abc123")
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
	g.Expect(result.RequeueAfter).To(Equal(60 * time.Second))

	// Verify the Job was created.
	var createdJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &createdJob)).To(Succeed())

	// Verify command and args.
	container := createdJob.Spec.Template.Spec.Containers[0]
	g.Expect(container.Name).To(Equal("bootstrap"))
	g.Expect(container.Command).To(Equal([]string{"keystone-manage", "bootstrap"}))
	g.Expect(container.Args).To(Equal([]string{
		"--bootstrap-password", "$(BOOTSTRAP_PASSWORD)",
		"--bootstrap-admin-url", fmt.Sprintf("http://%s-api.%s.svc.cluster.local:5000/v3", ks.Name, ks.Namespace),
		"--bootstrap-internal-url", fmt.Sprintf("http://%s-api.%s.svc.cluster.local:5000/v3", ks.Name, ks.Namespace),
		"--bootstrap-public-url", fmt.Sprintf("http://%s-api.%s.svc.cluster.local:5000/v3", ks.Name, ks.Namespace),
		"--bootstrap-region-id", "RegionOne",
	}))

	// Verify env.
	g.Expect(container.Env).To(HaveLen(1))
	g.Expect(container.Env[0].Name).To(Equal("BOOTSTRAP_PASSWORD"))
	g.Expect(container.Env[0].ValueFrom.SecretKeyRef.LocalObjectReference.Name).To(Equal("keystone-admin"))
	g.Expect(container.Env[0].ValueFrom.SecretKeyRef.Key).To(Equal("password"))

	// Verify config volume mount is present (CC-0013: bootstrap needs keystone.conf for DB connection).
	g.Expect(container.VolumeMounts).To(HaveLen(1))
	g.Expect(container.VolumeMounts[0].Name).To(Equal("config"))
	g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(container.VolumeMounts[0].ReadOnly).To(BeTrue())

	// Verify config volume references the ConfigMap.
	g.Expect(createdJob.Spec.Template.Spec.Volumes).To(HaveLen(1))
	g.Expect(createdJob.Spec.Template.Spec.Volumes[0].Name).To(Equal("config"))
	g.Expect(createdJob.Spec.Template.Spec.Volumes[0].ConfigMap.Name).To(Equal("keystone-config-abc123"))

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
}

func TestReconcileBootstrap_JobRunning(t *testing.T) {
	g := NewGomegaWithT(t)
	s := bootstrapTestScheme()
	ks := bootstrapKeystone()

	r := newBootstrapTestReconciler(s, ks, runningBootstrapJob(ks))

	result, err := r.reconcileBootstrap(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(60 * time.Second))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "BootstrapReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BootstrapInProgress"))
	g.Expect(cond.Message).To(Equal("Keystone bootstrap job is running"))
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
	g.Expect(result.RequeueAfter).To(Equal(60 * time.Second))

	// Verify the old Job was deleted and a new one was created.
	var newJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-bootstrap",
		Namespace: "default",
	}, &newJob)).To(Succeed())

	// The new Job should have the correct hash.
	desired := buildBootstrapJob(ks, "keystone-config-abc123")
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
	g.Expect(container.Args).To(ContainElements(
		"--bootstrap-admin-url", internalURL,
		"--bootstrap-internal-url", internalURL,
	))
	// Public URL should use the explicit PublicEndpoint (CC-0013).
	g.Expect(container.Args).To(ContainElements(
		"--bootstrap-public-url", "https://keystone.example.com/v3",
	))
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
