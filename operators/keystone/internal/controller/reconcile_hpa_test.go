// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
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

func hpaTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = autoscalingv2.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return s
}

func hpaTestKeystone() *keystonev1alpha1.Keystone {
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

func newHPATestReconciler(s *runtime.Scheme, objs ...client.Object) *KeystoneReconciler {
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{})
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

func int32Ptr(v int32) *int32 { return &v }

// TestSubResourceName_ReturnsCRNameWithoutSuffix verifies that the
// subResourceName helper returns the bare CR name with no "-api" suffix
// The helper is the single source of truth for sub-resource
// names; flipping it here propagates through every builder (Deployment, HPA,
// Service, PDB, NetworkPolicy, HTTPRoute).
func TestSubResourceName_ReturnsCRNameWithoutSuffix(t *testing.T) {
	g := NewGomegaWithT(t)

	// Plain CR name → returned verbatim.
	ks := &keystonev1alpha1.Keystone{ObjectMeta: metav1.ObjectMeta{Name: "test-keystone"}}
	g.Expect(subResourceName(ks)).To(Equal("test-keystone"),
		"subResourceName must return CR name with no '-api' suffix")

	// CR name that itself contains '-api' → suffix is NOT stripped from the
	// CR name, only the helper's own suffix is dropped. This protects fixtures
	// like the e2e-chaos `keystone-chaos-api` CR from regressing to
	// `keystone-chaos-api-api` or being incorrectly truncated.
	chaosKS := &keystonev1alpha1.Keystone{ObjectMeta: metav1.ObjectMeta{Name: "keystone-chaos-api"}}
	g.Expect(subResourceName(chaosKS)).To(Equal("keystone-chaos-api"),
		"subResourceName must preserve CR names that contain '-api'")
}

// TestSubResourceName_EmptyCRName verifies that an empty CR name yields an
// empty result rather than a synthetic "-api" artefact.
// Empty-name input is the responsibility of CRD validation, not this helper.
func TestSubResourceName_EmptyCRName(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := &keystonev1alpha1.Keystone{ObjectMeta: metav1.ObjectMeta{Name: ""}}
	g.Expect(subResourceName(ks)).To(Equal(""),
		"subResourceName must return empty string for empty CR name")
}

// --- Path 1: autoscaling enabled — create HPA ---

func TestReconcileHPA_AutoscalingSet_CreatesHPA(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hpaTestScheme()
	ks := hpaTestKeystone()
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          10,
		TargetCPUUtilization: int32Ptr(80),
	}
	r := newHPATestReconciler(s, ks)

	result, err := r.reconcileHPA(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	var hpa autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &hpa)).To(Succeed())

	g.Expect(hpa.OwnerReferences).To(HaveLen(1))
	g.Expect(hpa.OwnerReferences[0].Name).To(Equal("test-keystone"))

	// Verify HPAReady condition is set with reason HPAReady.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPAReady"))
}

func TestReconcileHPA_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hpaTestScheme()

	// Test ObservedGeneration for the autoscaling-enabled path.
	ks := hpaTestKeystone()
	ks.Generation = 7
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          10,
		TargetCPUUtilization: int32Ptr(80),
	}
	r := newHPATestReconciler(s, ks)

	_, err := r.reconcileHPA(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))

	// Test ObservedGeneration for the not-required path.
	ks2 := hpaTestKeystone()
	ks2.Generation = 12
	r2 := newHPATestReconciler(s, ks2)

	_, err = r2.reconcileHPA(context.Background(), ks2)
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, "HPAReady")
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(12)))
}

func TestReconcileHPA_AutoscalingEnabled_HPAUpdated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hpaTestScheme()
	ks := hpaTestKeystone()
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          10,
		TargetCPUUtilization: int32Ptr(80),
	}
	r := newHPATestReconciler(s, ks)
	ctx := context.Background()

	// First reconcile creates HPA.
	_, err := r.reconcileHPA(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	// Change max replicas and re-reconcile.
	ks.Spec.Autoscaling.MaxReplicas = 20
	_, err = r.reconcileHPA(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	var hpa autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &hpa)).To(Succeed())

	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(20)))
}

// --- Path 2: autoscaling disabled — delete HPA ---

func TestReconcileHPA_AutoscalingNil_NoExistingHPA_SetsHPANotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hpaTestScheme()
	ks := hpaTestKeystone()
	// autoscaling is nil by default
	r := newHPATestReconciler(s, ks)

	result, err := r.reconcileHPA(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPANotRequired"))
}

func TestReconcileHPA_AutoscalingNil_ExistingHPA_DeletesHPA(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hpaTestScheme()
	ks := hpaTestKeystone()

	// Pre-create an HPA as if autoscaling was previously enabled.
	existingHPA := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone",
			Namespace: "default",
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "test-keystone",
			},
			MaxReplicas: 10,
		},
	}
	r := newHPATestReconciler(s, ks, existingHPA)
	ctx := context.Background()

	// Verify HPA exists before reconcile.
	var hpa autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &hpa)).To(Succeed())

	// reconcileHPA with nil autoscaling should delete the HPA.
	result, err := r.reconcileHPA(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// Verify HPA was deleted.
	err = r.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &hpa)
	g.Expect(err).To(HaveOccurred())
	g.Expect(client.IgnoreNotFound(err)).To(Succeed())

	// Verify HPAReady condition is set with reason HPANotRequired.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPANotRequired"))
}

// --- Path 3: error scenarios ---

func TestReconcileHPA_EnsureError_Propagated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hpaTestScheme()
	ks := hpaTestKeystone()
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          10,
		TargetCPUUtilization: int32Ptr(80),
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler); ok {
					return fmt.Errorf("simulated HPA creation error")
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

	_, err := r.reconcileHPA(context.Background(), ks)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ensuring HorizontalPodAutoscaler"))
	g.Expect(err.Error()).To(ContainSubstring("simulated HPA creation error"))
}

func TestReconcileHPA_DeleteError_Propagated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hpaTestScheme()
	ks := hpaTestKeystone()
	// autoscaling is nil — triggers delete path.

	existingHPA := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone",
			Namespace: "default",
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "test-keystone",
			},
			MaxReplicas: 10,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, existingHPA).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler); ok {
					return fmt.Errorf("simulated HPA deletion error")
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

	_, err := r.reconcileHPA(context.Background(), ks)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("deleting HorizontalPodAutoscaler"))
	g.Expect(err.Error()).To(ContainSubstring("simulated HPA deletion error"))
}

// --- buildKeystoneHPA unit tests ---

func TestBuildKeystoneHPA_ScaleTargetRef(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hpaTestKeystone()
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          10,
		TargetCPUUtilization: int32Ptr(80),
	}

	hpa := buildKeystoneHPA(ks)

	g.Expect(hpa.Name).To(Equal("test-keystone"))
	g.Expect(hpa.Namespace).To(Equal("default"))
	g.Expect(hpa.Spec.ScaleTargetRef.APIVersion).To(Equal("apps/v1"))
	g.Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal("test-keystone"))
}

func TestBuildKeystoneHPA_Labels(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hpaTestKeystone()
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          10,
		TargetCPUUtilization: int32Ptr(80),
	}

	hpa := buildKeystoneHPA(ks)

	g.Expect(hpa.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(hpa.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(hpa.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))
}

func TestBuildKeystoneHPA_MinReplicasDefaultsToSpecReplicas(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hpaTestKeystone()
	ks.Spec.Replicas = 3
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          10,
		TargetCPUUtilization: int32Ptr(80),
		// MinReplicas is nil — should default to spec.replicas.
	}

	hpa := buildKeystoneHPA(ks)

	g.Expect(hpa.Spec.MinReplicas).NotTo(BeNil())
	g.Expect(*hpa.Spec.MinReplicas).To(Equal(int32(3)))
}

func TestBuildKeystoneHPA_ExplicitMinReplicas(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hpaTestKeystone()
	ks.Spec.Replicas = 3
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MinReplicas:          int32Ptr(2),
		MaxReplicas:          10,
		TargetCPUUtilization: int32Ptr(80),
	}

	hpa := buildKeystoneHPA(ks)

	g.Expect(hpa.Spec.MinReplicas).NotTo(BeNil())
	g.Expect(*hpa.Spec.MinReplicas).To(Equal(int32(2)))
}

func TestBuildKeystoneHPA_MaxReplicas(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hpaTestKeystone()
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          15,
		TargetCPUUtilization: int32Ptr(80),
	}

	hpa := buildKeystoneHPA(ks)

	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(15)))
}

func TestBuildKeystoneHPA_CPUMetricOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hpaTestKeystone()
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          10,
		TargetCPUUtilization: int32Ptr(80),
	}

	hpa := buildKeystoneHPA(ks)

	g.Expect(hpa.Spec.Metrics).To(HaveLen(1))
	g.Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ResourceMetricSourceType))
	g.Expect(hpa.Spec.Metrics[0].Resource).NotTo(BeNil())
	g.Expect(hpa.Spec.Metrics[0].Resource.Name).To(Equal(corev1.ResourceCPU))
	g.Expect(hpa.Spec.Metrics[0].Resource.Target.Type).To(Equal(autoscalingv2.UtilizationMetricType))
	g.Expect(hpa.Spec.Metrics[0].Resource.Target.AverageUtilization).NotTo(BeNil())
	g.Expect(*hpa.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(int32(80)))
}

func TestBuildKeystoneHPA_MemoryMetricOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hpaTestKeystone()
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:             10,
		TargetMemoryUtilization: int32Ptr(70),
	}

	hpa := buildKeystoneHPA(ks)

	g.Expect(hpa.Spec.Metrics).To(HaveLen(1))
	g.Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ResourceMetricSourceType))
	g.Expect(hpa.Spec.Metrics[0].Resource).NotTo(BeNil())
	g.Expect(hpa.Spec.Metrics[0].Resource.Name).To(Equal(corev1.ResourceMemory))
	g.Expect(hpa.Spec.Metrics[0].Resource.Target.Type).To(Equal(autoscalingv2.UtilizationMetricType))
	g.Expect(hpa.Spec.Metrics[0].Resource.Target.AverageUtilization).NotTo(BeNil())
	g.Expect(*hpa.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(int32(70)))
}

func TestBuildKeystoneHPA_BothCPUAndMemoryMetrics(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hpaTestKeystone()
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:             10,
		TargetCPUUtilization:    int32Ptr(80),
		TargetMemoryUtilization: int32Ptr(70),
	}

	hpa := buildKeystoneHPA(ks)

	g.Expect(hpa.Spec.Metrics).To(HaveLen(2))

	// First metric should be CPU.
	g.Expect(hpa.Spec.Metrics[0].Resource.Name).To(Equal(corev1.ResourceCPU))
	g.Expect(*hpa.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(int32(80)))

	// Second metric should be memory.
	g.Expect(hpa.Spec.Metrics[1].Resource.Name).To(Equal(corev1.ResourceMemory))
	g.Expect(*hpa.Spec.Metrics[1].Resource.Target.AverageUtilization).To(Equal(int32(70)))
}

func TestBuildKeystoneHPA_MinReplicasDefaultIndependentOfPointer(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hpaTestKeystone()
	ks.Spec.Replicas = 5
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          10,
		TargetCPUUtilization: int32Ptr(80),
	}

	hpa := buildKeystoneHPA(ks)

	// Verify the defaulted minReplicas is independent — mutating spec.replicas
	// after build should not affect the HPA.
	g.Expect(*hpa.Spec.MinReplicas).To(Equal(int32(5)))
	ks.Spec.Replicas = 99
	g.Expect(*hpa.Spec.MinReplicas).To(Equal(int32(5)))
}

// TestBuildKeystoneHPA_NameMatchesCR pins both the HPA ObjectMeta.Name and its
// ScaleTargetRef.Name to the bare CR name. The HPA must scale the same
// Deployment the operator emits at `<cr-name>` — any drift would leave the HPA
// pointing at a non-existent target after the rename.
func TestBuildKeystoneHPA_NameMatchesCR(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hpaTestKeystone()
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          10,
		TargetCPUUtilization: int32Ptr(80),
	}

	hpa := buildKeystoneHPA(ks)

	g.Expect(hpa.Name).To(Equal(ks.Name),
		"HPA Name must equal the CR name")
	g.Expect(hpa.Name).NotTo(HaveSuffix("-api"),
		"HPA Name must not carry the legacy `-api` suffix")
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(ks.Name),
		"HPA ScaleTargetRef must point at the Deployment named after the CR")
	g.Expect(hpa.Spec.ScaleTargetRef.Name).NotTo(HaveSuffix("-api"),
		"HPA ScaleTargetRef Name must not carry the legacy `-api` suffix")
}
