// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Horizon sub-reconciler.
package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// horizonTestScheme registers c5c3, client-go, and horizon types.
func horizonTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := horizonv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding horizon scheme: %v", err)
	}
	return s
}

// horizonControlPlane builds a ControlPlane with services.horizon set and a
// KeystoneReady=True condition already set (gate passed).
func horizonControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "default",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
					Database:   "keystone",
					SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
				},
				Cache: commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-memcached"},
					Backend:    "dogpile.cache.pymemcache",
					Replicas:   3,
				},
			},
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:  &c5c3v1alpha1.ServiceHorizonSpec{},
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				},
			},
		},
	}
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "KeystoneReady",
		Message:            "ready",
	})
	return cp
}

func newHorizonTestReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := horizonTestScheme(t)
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	cb = cb.WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &horizonv1alpha1.Horizon{})
	return &ControlPlaneReconciler{Client: cb.Build(), Scheme: s}
}

func getProjectedHorizon(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane) *horizonv1alpha1.Horizon {
	t.Helper()
	h := &horizonv1alpha1.Horizon{}
	key := types.NamespacedName{Name: horizonName(cp), Namespace: childNamespace(cp)}
	if err := c.Get(context.Background(), key, h); err != nil {
		t.Fatalf("getting projected Horizon %s: %v", key, err)
	}
	return h
}

func TestReconcileHorizon_ImageTagFromRelease(t *testing.T) {
	// Two rows prove the tag is DERIVED from openStackRelease (a different
	// release yields a different tag), rather than coincidentally matching a
	// single literal.
	for _, tt := range []struct {
		release string
		wantTag string
	}{
		{release: "2025.2", wantTag: "2025.2"},
		{release: "2026.1", wantTag: "2026.1"},
	} {
		t.Run(tt.release, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := horizonControlPlane()
			cp.Spec.OpenStackRelease = tt.release
			r := newHorizonTestReconciler(t, cp)

			_, err := r.reconcileHorizon(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())

			h := getProjectedHorizon(t, r.Client, cp)
			g.Expect(h.Spec.Image.Repository).To(Equal("ghcr.io/c5c3/horizon"))
			g.Expect(h.Spec.Image.Tag).To(Equal(tt.wantTag))
		})
	}
}

func TestReconcileHorizon_ImageOverrideWins(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	cp.Spec.Services.Horizon.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/mirror/horizon",
		Tag:        "custom",
	}
	r := newHorizonTestReconciler(t, cp)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.Image.Repository).To(Equal("registry.example.com/mirror/horizon"))
	g.Expect(h.Spec.Image.Tag).To(Equal("custom"))
}

func TestReconcileHorizon_NotManagedWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	cp.Spec.Services.Horizon = nil
	r := newHorizonTestReconciler(t, cp)

	res, err := r.reconcileHorizon(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeHorizonReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HorizonNotManaged"))
	// No child was created.
	var list horizonv1alpha1.HorizonList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileHorizon_NilInfrastructureDoesNotPanic exercises the defensive
// nil-Infrastructure guard. An External-mode ControlPlane omits
// spec.infrastructure, so the dashboard cache projection has nothing to
// DeepCopy. The fixture carries KeystoneReady=True, so without a local guard the
// projection's cp.Spec.Infrastructure.Cache deref would panic once the
// KeystoneReady gate is passed. The guard must instead requeue.
func TestReconcileHorizon_NilInfrastructureDoesNotPanic(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	cp.Spec.Infrastructure = nil
	r := newHorizonTestReconciler(t, cp)

	res, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	// No Horizon child is projected against the absent infrastructure.
	var list horizonv1alpha1.HorizonList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

func TestReconcileHorizon_UnsetPreservesChildByDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	r := newHorizonTestReconciler(t, cp)

	// Project the child first, then unset the block WITHOUT the opt-in
	// annotation: the child must be preserved.
	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	cp.Spec.Services.Horizon = nil

	_, err = r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h).NotTo(BeNil(), "child must be preserved without the opt-in annotation")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeHorizonReady)
	g.Expect(cond.Reason).To(Equal("HorizonNotManaged"))
	g.Expect(cond.Message).To(ContainSubstring(horizonDeletionAllowedAnnotation))
}

func TestReconcileHorizon_UnsetDeletesChildWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	r := newHorizonTestReconciler(t, cp)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cp.Spec.Services.Horizon = nil
	cp.Annotations = map[string]string{horizonDeletionAllowedAnnotation: "true"}

	_, err = r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list horizonv1alpha1.HorizonList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "opt-in annotation must delete the orphaned child")
}

func TestReconcileHorizon_GatedOnKeystoneReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	// Flip the gate to False.
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		Reason:             "WaitingForKeystone",
		Message:            "not ready",
	})
	r := newHorizonTestReconciler(t, cp)

	res, err := r.reconcileHorizon(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(keystoneInfraGateRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeHorizonReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForKeystone"))

	// The gate blocked projection: no child exists.
	var list horizonv1alpha1.HorizonList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

func TestReconcileHorizon_CacheDeepCopiedFromInfrastructure(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	r := newHorizonTestReconciler(t, cp)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(h.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	// DeepCopy: the child's pointer must not alias the ControlPlane's spec.
	g.Expect(h.Spec.Cache.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Cache.ClusterRef))
	// The shared CacheSpec.Backend is overloaded: infrastructure.cache.backend
	// holds the oslo.cache dogpile path Keystone consumes, but the dashboard
	// renders spec.cache.backend verbatim as the Django CACHES backend. The
	// projection must translate it to an importable Django class, never carry
	// the dogpile path through (which makes Django 500 on the login page the
	// probes hit).
	g.Expect(h.Spec.Cache.Backend).To(Equal(horizonv1alpha1.DefaultCacheBackend))
	g.Expect(h.Spec.Cache.Backend).NotTo(Equal(cp.Spec.Infrastructure.Cache.Backend))
}

func TestReconcileHorizon_KeystoneEndpointDerivation(t *testing.T) {
	// The dashboard's Django backend connects to spec.keystoneEndpoint
	// server-side, so the projection must always use the cluster-local
	// convention URL. External exposure (gateway hostname, explicit
	// publicEndpoint) must NOT leak into it: those URLs may only resolve
	// outside the cluster (a kind port-mapping, an external LB) and would
	// break every dashboard login.
	tests := []struct {
		name   string
		mutate func(cp *c5c3v1alpha1.ControlPlane)
		want   string
	}{
		{
			name:   "convention URL by default",
			mutate: func(*c5c3v1alpha1.ControlPlane) {},
			want:   "http://cp-keystone.default.svc:5000/v3",
		},
		{
			name: "gateway hostname does not leak in",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
					ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
					Hostname:  "keystone.example.com",
				}
			},
			want: "http://cp-keystone.default.svc:5000/v3",
		},
		{
			name: "explicit publicEndpoint does not leak in",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
					ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
					Hostname:  "keystone.example.com",
				}
				cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com:8443/v3"
			},
			want: "http://cp-keystone.default.svc:5000/v3",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := horizonControlPlane()
			tc.mutate(cp)
			r := newHorizonTestReconciler(t, cp)

			_, err := r.reconcileHorizon(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())

			h := getProjectedHorizon(t, r.Client, cp)
			g.Expect(h.Spec.KeystoneEndpoint).To(Equal(tc.want))
		})
	}
}

func TestReconcileHorizon_SecretKeyRefDefaultAndOverride(t *testing.T) {
	g := NewGomegaWithT(t)

	// Default: the kind-infrastructure shim Secret.
	cp := horizonControlPlane()
	r := newHorizonTestReconciler(t, cp)
	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.SecretKeyRef.Name).To(Equal("horizon-secret-key"))
	g.Expect(h.Spec.SecretKeyRef.Key).To(Equal("secret-key"))

	// Override wins.
	cp2 := horizonControlPlane()
	cp2.Name = "cp2"
	cp2.Spec.Services.Horizon.SecretKeyRef = &commonv1.SecretRefSpec{Name: "tenant-key", Key: "custom"}
	r2 := newHorizonTestReconciler(t, cp2)
	_, err = r2.reconcileHorizon(context.Background(), cp2)
	g.Expect(err).NotTo(HaveOccurred())
	h2 := getProjectedHorizon(t, r2.Client, cp2)
	g.Expect(h2.Spec.SecretKeyRef.Name).To(Equal("tenant-key"))
	g.Expect(h2.Spec.SecretKeyRef.Key).To(Equal("custom"))
}

func TestReconcileHorizon_ReplicasPassthrough(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	cp.Spec.Services.Horizon.Replicas = ptr.To(int32(5))
	r := newHorizonTestReconciler(t, cp)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.Deployment.Replicas).To(Equal(int32(5)))
}

func TestReconcileHorizon_ReplicasRevertsToDefaultWhenCleared(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	cp.Spec.Services.Horizon.Replicas = ptr.To(int32(8))
	r := newHorizonTestReconciler(t, cp)
	ctx := context.Background()

	// First projection pins the child at the override.
	_, err := r.reconcileHorizon(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.Deployment.Replicas).To(Equal(int32(8)))

	// Clearing the override must revert the child to the operator default,
	// not leave the previously-projected value pinned on the fetched child.
	cp.Spec.Services.Horizon.Replicas = nil
	_, err = r.reconcileHorizon(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	h = getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas))
}

func TestReconcileHorizon_MirrorsChildReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	r := newHorizonTestReconciler(t, cp)
	ctx := context.Background()

	// First pass: child created but not ready → WaitingForHorizon + requeue.
	res, err := r.reconcileHorizon(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeHorizonReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForHorizon"))

	// Mark the child Ready and reconcile again → HorizonReady=True.
	h := getProjectedHorizon(t, r.Client, cp)
	conditions.SetCondition(&h.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: h.Generation,
		Reason:             "AllReady",
		Message:            "ready",
	})
	g.Expect(r.Client.Status().Update(ctx, h)).To(Succeed())

	res, err = r.reconcileHorizon(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond = conditions.GetCondition(cp.Status.Conditions, conditionTypeHorizonReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HorizonReady"))
}

func TestReconcileHorizon_SetsControllerOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	r := newHorizonTestReconciler(t, cp)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(metav1.IsControlledBy(h, cp)).To(BeTrue(),
		"the projected Horizon must carry the ControlPlane controller owner reference")
}

func TestSetServicesStatus_TwoServices(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type: conditionTypeHorizonReady, Status: metav1.ConditionTrue,
		ObservedGeneration: 1, Reason: "HorizonReady", Message: "ready",
	})

	setServicesStatus(cp)

	g.Expect(cp.Status.Services).To(HaveLen(2))
	g.Expect(cp.Status.Services[0].Name).To(Equal("keystone"))
	g.Expect(cp.Status.Services[0].Ready).To(BeTrue())
	g.Expect(cp.Status.Services[1].Name).To(Equal("horizon"))
	g.Expect(cp.Status.Services[1].Ready).To(BeTrue())

	// Horizon degrading flips only its own entry.
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type: conditionTypeHorizonReady, Status: metav1.ConditionFalse,
		ObservedGeneration: 1, Reason: "WaitingForHorizon", Message: "not ready",
	})
	setServicesStatus(cp)
	g.Expect(cp.Status.Services[0].Ready).To(BeTrue())
	g.Expect(cp.Status.Services[1].Ready).To(BeFalse())
}

func TestSetServicesStatus_HorizonOnlyWhenConfigured(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	cp.Spec.Services.Horizon = nil

	setServicesStatus(cp)

	g.Expect(cp.Status.Services).To(HaveLen(1))
	g.Expect(cp.Status.Services[0].Name).To(Equal("keystone"))
}
