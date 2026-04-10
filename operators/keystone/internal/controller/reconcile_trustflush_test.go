// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0057

func trustFlushTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return s
}

func trustFlushTestKeystone() *keystonev1alpha1.Keystone {
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

func newTrustFlushTestReconciler(s *runtime.Scheme, objs ...client.Object) *KeystoneReconciler {
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{})
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

// --- Path 1: trust flush enabled — create CronJob (REQ-001) ---

func TestReconcileTrustFlush_TrustFlushSet_CreatesCronJob(t *testing.T) {
	g := NewGomegaWithT(t)
	s := trustFlushTestScheme()
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}
	r := newTrustFlushTestReconciler(s, ks)

	result, err := r.reconcileTrustFlush(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	var cronJob batchv1.CronJob
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-trust-flush", Namespace: "default",
	}, &cronJob)).To(Succeed())

	g.Expect(cronJob.OwnerReferences).To(HaveLen(1))
	g.Expect(cronJob.OwnerReferences[0].Name).To(Equal("test-keystone"))

	// Verify TrustFlushReady condition is set with reason TrustFlushReady (CC-0057, REQ-009).
	cond := meta.FindStatusCondition(ks.Status.Conditions, "TrustFlushReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("TrustFlushReady"))
}

func TestReconcileTrustFlush_TrustFlushSet_CronJobUpdated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := trustFlushTestScheme()
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}
	r := newTrustFlushTestReconciler(s, ks)
	ctx := context.Background()

	// First reconcile creates CronJob.
	_, err := r.reconcileTrustFlush(ctx, ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// Change schedule and re-reconcile.
	ks.Spec.TrustFlush.Schedule = "*/30 * * * *"
	_, err = r.reconcileTrustFlush(ctx, ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "test-keystone-trust-flush", Namespace: "default",
	}, &cronJob)).To(Succeed())

	g.Expect(cronJob.Spec.Schedule).To(Equal("*/30 * * * *"))
}

func TestReconcileTrustFlush_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := trustFlushTestScheme()

	// Test ObservedGeneration for the trust-flush-enabled path.
	ks := trustFlushTestKeystone()
	ks.Generation = 7
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}
	r := newTrustFlushTestReconciler(s, ks)

	_, err := r.reconcileTrustFlush(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "TrustFlushReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))

	// Test ObservedGeneration for the not-required path.
	ks2 := trustFlushTestKeystone()
	ks2.Generation = 12
	r2 := newTrustFlushTestReconciler(s, ks2)

	_, err = r2.reconcileTrustFlush(context.Background(), ks2, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, "TrustFlushReady")
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(12)))
}

// --- Path 2: trust flush disabled — delete CronJob (REQ-002) ---

func TestReconcileTrustFlush_TrustFlushNil_NoExistingCronJob_SetsTrustFlushNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	s := trustFlushTestScheme()
	ks := trustFlushTestKeystone()
	// trustFlush is nil by default
	r := newTrustFlushTestReconciler(s, ks)

	result, err := r.reconcileTrustFlush(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "TrustFlushReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("TrustFlushNotRequired"))
}

func TestReconcileTrustFlush_TrustFlushNil_ExistingCronJob_DeletesCronJob(t *testing.T) {
	g := NewGomegaWithT(t)
	s := trustFlushTestScheme()
	ks := trustFlushTestKeystone()

	// Pre-create a CronJob as if trust flush was previously enabled.
	existingCronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-trust-flush",
			Namespace: "default",
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: defaultPodTemplate(),
				},
			},
		},
	}
	r := newTrustFlushTestReconciler(s, ks, existingCronJob)
	ctx := context.Background()

	// Verify CronJob exists before reconcile.
	var cronJob batchv1.CronJob
	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "test-keystone-trust-flush", Namespace: "default",
	}, &cronJob)).To(Succeed())

	// reconcileTrustFlush with nil trustFlush should delete the CronJob.
	result, err := r.reconcileTrustFlush(ctx, ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// Verify CronJob was deleted.
	err = r.Get(ctx, types.NamespacedName{
		Name: "test-keystone-trust-flush", Namespace: "default",
	}, &cronJob)
	g.Expect(err).To(HaveOccurred())
	g.Expect(client.IgnoreNotFound(err)).To(Succeed())

	// Verify TrustFlushReady condition is set with reason TrustFlushNotRequired.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "TrustFlushReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("TrustFlushNotRequired"))
}

// defaultPodTemplate returns a minimal PodTemplateSpec for test CronJobs.
func defaultPodTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyOnFailure,
			Containers: []corev1.Container{{
				Name:    "trust-flush",
				Image:   "ghcr.io/c5c3/keystone:2025.2",
				Command: []string{"keystone-manage", "trust_flush"},
			}},
		},
	}
}

// --- Path 3: error scenarios ---

func TestReconcileTrustFlush_EnsureError_Propagated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := trustFlushTestScheme()
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*batchv1.CronJob); ok {
					return fmt.Errorf("simulated CronJob creation error")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileTrustFlush(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ensuring trust flush CronJob"))
	g.Expect(err.Error()).To(ContainSubstring("simulated CronJob creation error"))
}

func TestReconcileTrustFlush_DeleteError_Propagated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := trustFlushTestScheme()
	ks := trustFlushTestKeystone()
	// trustFlush is nil — triggers delete path.

	existingCronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-trust-flush",
			Namespace: "default",
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: defaultPodTemplate(),
				},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, existingCronJob).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*batchv1.CronJob); ok {
					return fmt.Errorf("simulated CronJob deletion error")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileTrustFlush(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("deleting trust flush CronJob"))
	g.Expect(err.Error()).To(ContainSubstring("simulated CronJob deletion error"))
}

// --- trustFlushCronJob builder unit tests ---

func TestTrustFlushCronJob_Schedule(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "*/15 * * * *",
	}

	cronJob := trustFlushCronJob(ks, "test-keystone-config-abc123")

	g.Expect(cronJob.Spec.Schedule).To(Equal("*/15 * * * *"))
}

func TestTrustFlushCronJob_Suspend(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
		Suspend:  true,
	}

	cronJob := trustFlushCronJob(ks, "test-keystone-config-abc123")

	g.Expect(cronJob.Spec.Suspend).NotTo(BeNil())
	g.Expect(*cronJob.Spec.Suspend).To(BeTrue())

	// Verify unsuspended case.
	ks.Spec.TrustFlush.Suspend = false
	cronJob2 := trustFlushCronJob(ks, "test-keystone-config-abc123")
	g.Expect(cronJob2.Spec.Suspend).NotTo(BeNil())
	g.Expect(*cronJob2.Spec.Suspend).To(BeFalse())
}

func TestTrustFlushCronJob_ArgsIncluded(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
		Args:     []string{"--date", "2026-01-01"},
	}

	cronJob := trustFlushCronJob(ks, "test-keystone-config-abc123")

	container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	g.Expect(container.Command).To(Equal([]string{
		"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "trust_flush",
		"--date", "2026-01-01",
	}))
}

func TestTrustFlushCronJob_NoArgs(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}

	cronJob := trustFlushCronJob(ks, "test-keystone-config-abc123")

	container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	g.Expect(container.Command).To(Equal([]string{
		"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "trust_flush",
	}))
}

func TestTrustFlushCronJob_Labels(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}

	cronJob := trustFlushCronJob(ks, "test-keystone-config-abc123")

	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))

	// Pod template labels should match.
	g.Expect(cronJob.Spec.JobTemplate.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(cronJob.Spec.JobTemplate.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
}

func TestTrustFlushCronJob_Image(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := trustFlushTestKeystone()
	ks.Spec.Image = commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"}
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}

	cronJob := trustFlushCronJob(ks, "test-keystone-config-abc123")

	container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/keystone:2025.2"))
}

func TestTrustFlushCronJob_Volumes(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}

	cronJob := trustFlushCronJob(ks, "test-keystone-config-abc123")

	volumes := cronJob.Spec.JobTemplate.Spec.Template.Spec.Volumes
	g.Expect(volumes).To(HaveLen(3))

	// Config volume from ConfigMap.
	g.Expect(volumes[0].Name).To(Equal("config"))
	g.Expect(volumes[0].ConfigMap.Name).To(Equal("test-keystone-config-abc123"))

	// Fernet keys volume from Secret.
	g.Expect(volumes[1].Name).To(Equal("fernet-keys"))
	g.Expect(volumes[1].Secret.SecretName).To(Equal("test-keystone-fernet-keys"))

	// Credential keys volume from Secret.
	g.Expect(volumes[2].Name).To(Equal("credential-keys"))
	g.Expect(volumes[2].Secret.SecretName).To(Equal("test-keystone-credential-keys"))
}

func TestTrustFlushCronJob_VolumeMounts(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}

	cronJob := trustFlushCronJob(ks, "test-keystone-config-abc123")

	mounts := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].VolumeMounts
	g.Expect(mounts).To(HaveLen(3))

	g.Expect(mounts[0].Name).To(Equal("config"))
	g.Expect(mounts[0].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(mounts[0].ReadOnly).To(BeTrue())

	g.Expect(mounts[1].Name).To(Equal("fernet-keys"))
	g.Expect(mounts[1].MountPath).To(Equal("/etc/keystone/fernet-keys"))
	g.Expect(mounts[1].ReadOnly).To(BeTrue())

	g.Expect(mounts[2].Name).To(Equal("credential-keys"))
	g.Expect(mounts[2].MountPath).To(Equal("/etc/keystone/credential-keys"))
	g.Expect(mounts[2].ReadOnly).To(BeTrue())
}

func TestTrustFlushCronJob_SecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}

	cronJob := trustFlushCronJob(ks, "test-keystone-config-abc123")

	container := findContainerByName(cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers, "trust-flush")
	expectRestrictedSecurityContext(g, container)
}

func TestTrustFlushCronJob_RestartPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}

	cronJob := trustFlushCronJob(ks, "test-keystone-config-abc123")

	g.Expect(cronJob.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))
}

func TestTrustFlushCronJob_Name(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := trustFlushTestKeystone()
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: "0 * * * *",
	}

	cronJob := trustFlushCronJob(ks, "test-keystone-config-abc123")

	g.Expect(cronJob.Name).To(Equal("test-keystone-trust-flush"))
	g.Expect(cronJob.Namespace).To(Equal("default"))
}
