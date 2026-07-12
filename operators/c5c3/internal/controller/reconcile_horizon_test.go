// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Horizon sub-reconciler.
package controller

import (
	"context"
	"fmt"
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
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// horizonTestScheme registers c5c3, client-go, horizon, and keystone types.
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
	// reconcileHorizon lists the KeystoneIdentityBackend CRs attached to the
	// Keystone child to project the websso choices and domain dropdown.
	if err := keystonev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding keystone scheme: %v", err)
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

// horizonBackend builds a KeystoneIdentityBackend attached to the Keystone
// child of the horizonControlPlane fixture ("cp-keystone").
func horizonBackend(name string, typ keystonev1alpha1.IdentityBackendType, domain string, ready bool) *keystonev1alpha1.KeystoneIdentityBackend {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	return &keystonev1alpha1.KeystoneIdentityBackend{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: keystonev1alpha1.KeystoneIdentityBackendSpec{
			KeystoneRef: keystonev1alpha1.KeystoneRefSpec{Name: "cp-keystone"},
			Domain:      keystonev1alpha1.DomainSpec{Name: domain},
			Type:        typ,
		},
		Status: keystonev1alpha1.KeystoneIdentityBackendStatus{
			Conditions: []metav1.Condition{{Type: "Ready", Status: status, Reason: "Test"}},
		},
	}
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

// TestReconcileHorizon_ProjectsSecretStoreRef verifies the ControlPlane's
// spec.secretStoreRef is projected onto the Horizon child.
func TestReconcileHorizon_ProjectsSecretStoreRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	r := newHorizonTestReconciler(t, cp)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.SecretStoreRef).NotTo(BeNil(), "the ControlPlane store ref must be projected onto the Horizon child")
	g.Expect(h.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(h.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))
}

// TestReconcileHorizon_ClearsSecretStoreRefWhenUnset verifies clearing the
// ControlPlane store ref reverts the Horizon child to the default (nil).
func TestReconcileHorizon_ClearsSecretStoreRefWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	r := newHorizonTestReconciler(t, cp)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.SecretStoreRef).To(BeNil(),
		"a ControlPlane without a store ref must leave the Horizon child on the default (nil)")
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

// publishSSOEndpoints gives cp the two browser-facing endpoints a WebSSO
// hand-off needs: an externally reachable dashboard (whose origin Keystone will
// trust) and an externally reachable Keystone (which the browser is redirected
// to). Without both, horizonWebSSO projects nothing.
func publishSSOEndpoints(cp *c5c3v1alpha1.ControlPlane) {
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		PublicEndpoint: "https://keystone.127-0-0-1.nip.io/v3",
	}
	cp.Spec.Services.Horizon.PublicEndpoint = "https://horizon.127-0-0-1.nip.io"
}

// TestReconcileHorizon_WebSSOProjectedFromReadyFederationBackends is the core
// of the federation-entry projection: one choice per Ready OIDC backend, the
// credentials fallback leading, a matching idpMapping, and the BROWSER-facing
// Keystone URL (not the cluster-local endpoint).
func TestReconcileHorizon_WebSSOProjectedFromReadyFederationBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	publishSSOEndpoints(cp)
	kc := horizonBackend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	r := newHorizonTestReconciler(t, cp, kc)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.WebSSO).NotTo(BeNil())
	g.Expect(h.Spec.WebSSO.Enabled).To(BeTrue())
	g.Expect(h.Spec.WebSSO.Choices).To(Equal([]horizonv1alpha1.WebSSOChoice{
		{ID: horizonv1alpha1.DefaultWebSSOLocalChoiceID, Label: horizonv1alpha1.DefaultWebSSOLocalChoiceLabel},
		{ID: "keycloak_openid", Label: "keycloak"},
	}))
	g.Expect(h.Spec.WebSSO.IDPMapping).To(Equal(map[string]horizonv1alpha1.WebSSOIDPTarget{
		"keycloak_openid": {IdentityProvider: "keycloak", Protocol: "openid"},
	}))
	g.Expect(h.Spec.WebSSO.InitialChoice).To(Equal(horizonv1alpha1.DefaultWebSSOLocalChoiceID))
	// The browser follows this redirect, so it must be the external endpoint —
	// never the cluster-local Service URL projected into keystoneEndpoint.
	g.Expect(h.Spec.WebSSO.KeystoneURL).To(Equal("https://keystone.127-0-0-1.nip.io/v3"))
	g.Expect(h.Spec.KeystoneEndpoint).NotTo(Equal(h.Spec.WebSSO.KeystoneURL))
}

// TestReconcileHorizon_WebSSOOmitsNotReadyBackend is the issue's contract:
// websso entries appear only for Ready backends, so the login page never
// offers an SSO button that dead-ends.
func TestReconcileHorizon_WebSSOOmitsNotReadyBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	publishSSOEndpoints(cp)
	ready := horizonBackend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	pending := horizonBackend("pending-idp", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", false)
	r := newHorizonTestReconciler(t, cp, ready, pending)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.WebSSO).NotTo(BeNil())
	ids := make([]string, 0, len(h.Spec.WebSSO.Choices))
	for _, c := range h.Spec.WebSSO.Choices {
		ids = append(ids, c.ID)
	}
	g.Expect(ids).To(ContainElement("keycloak_openid"))
	g.Expect(ids).NotTo(ContainElement("pending-idp_openid"))
	g.Expect(h.Spec.WebSSO.IDPMapping).NotTo(HaveKey("pending-idp_openid"))
}

// TestReconcileHorizon_NoBackendsLeavesWebSSONil covers the overwhelmingly
// common ControlPlane: no federation, no multi-domain, no rendered settings.
func TestReconcileHorizon_NoBackendsLeavesWebSSONil(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	r := newHorizonTestReconciler(t, cp)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.WebSSO).To(BeNil())
	g.Expect(h.Spec.MultiDomain).To(BeNil())
}

// TestReconcileHorizon_RetainsWebSSOWhileBackendUnhealthy separates "backend
// detached" from "backend not healthy right now". A backend's aggregate Ready
// can drop on a failed observation while the Keystone-side federation objects
// it provisioned are untouched — the SSO button keeps working. Rebuilding the
// block from that view would re-render local_settings.py, roll the dashboard,
// and roll it back on recovery, twice over, for a login page that was never
// broken.
func TestReconcileHorizon_RetainsWebSSOWhileBackendUnhealthy(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	publishSSOEndpoints(cp)
	kc := horizonBackend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	ldap := horizonBackend("openldap", keystonev1alpha1.IdentityBackendTypeLDAP, "planetexpress", true)
	r := newHorizonTestReconciler(t, cp, kc, ldap)
	ctx := context.Background()

	_, err := r.reconcileHorizon(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	projected := getProjectedHorizon(t, r.Client, cp)
	g.Expect(projected.Spec.WebSSO).NotTo(BeNil())
	g.Expect(projected.Spec.MultiDomain).NotTo(BeNil())

	// Both backends go unhealthy — a Keystone restart, an identity-API blip.
	for _, name := range []string{"keycloak", "openldap"} {
		b := &keystonev1alpha1.KeystoneIdentityBackend{}
		g.Expect(r.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, b)).To(Succeed())
		b.Status.Conditions[0].Status = metav1.ConditionFalse
		// The backend type carries no fake status subresource, so a plain Update
		// persists the demoted condition.
		g.Expect(r.Update(ctx, b)).To(Succeed())
	}

	_, err = r.reconcileHorizon(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	retained := getProjectedHorizon(t, r.Client, cp)
	g.Expect(retained.Spec.WebSSO).To(Equal(projected.Spec.WebSSO),
		"an unhealthy backend must not strip the login page's SSO choices")
	g.Expect(retained.Spec.MultiDomain).To(Equal(projected.Spec.MultiDomain),
		"an unhealthy backend must not strip the login page's domain field")
}

// TestReconcileHorizon_ClearsWebSSOOnBackendDetach is the other half of the
// distinction above: removing the last backend is an authoritative statement
// that the dashboard must stop offering the choice, so the block is cleared and
// the login page reverts to local credentials.
func TestReconcileHorizon_ClearsWebSSOOnBackendDetach(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	publishSSOEndpoints(cp)
	kc := horizonBackend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	ldap := horizonBackend("openldap", keystonev1alpha1.IdentityBackendTypeLDAP, "planetexpress", true)
	r := newHorizonTestReconciler(t, cp, kc, ldap)
	ctx := context.Background()

	_, err := r.reconcileHorizon(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedHorizon(t, r.Client, cp).Spec.WebSSO).NotTo(BeNil())

	g.Expect(r.Delete(ctx, kc)).To(Succeed())
	g.Expect(r.Delete(ctx, ldap)).To(Succeed())

	_, err = r.reconcileHorizon(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	cleared := getProjectedHorizon(t, r.Client, cp)
	g.Expect(cleared.Spec.WebSSO).To(BeNil())
	g.Expect(cleared.Spec.MultiDomain).To(BeNil())
}

// TestReconcileHorizon_ClearsWebSSOWhenEndpointsAreRemoved guards against the
// retention above swallowing a real spec change: dropping the dashboard's
// publicEndpoint removes the origin Keystone trusts, so the SSO button would
// dead-end after the user has authenticated. It must disappear even though the
// backend is still attached.
func TestReconcileHorizon_ClearsWebSSOWhenEndpointsAreRemoved(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	publishSSOEndpoints(cp)
	kc := horizonBackend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	r := newHorizonTestReconciler(t, cp, kc)
	ctx := context.Background()

	_, err := r.reconcileHorizon(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedHorizon(t, r.Client, cp).Spec.WebSSO).NotTo(BeNil())

	cp.Spec.Services.Horizon.PublicEndpoint = ""

	_, err = r.reconcileHorizon(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedHorizon(t, r.Client, cp).Spec.WebSSO).To(BeNil())
}

// TestReconcileHorizon_MultiDomainProjectedFromDomainBackends covers the
// LDAP-domain login path: the form gains a domain field, and the default domain
// stays the one assumed for users who supply none.
func TestReconcileHorizon_MultiDomainProjectedFromDomainBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	ldap := horizonBackend("openldap", keystonev1alpha1.IdentityBackendTypeLDAP, "planetexpress", true)
	r := newHorizonTestReconciler(t, cp, ldap)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.MultiDomain).NotTo(BeNil())
	g.Expect(h.Spec.MultiDomain.Enabled).To(BeTrue())
	g.Expect(h.Spec.MultiDomain.DefaultDomain).To(Equal(horizonv1alpha1.DefaultMultiDomainDefaultDomain))
	// An LDAP-only ControlPlane gets a domain field but no SSO button.
	g.Expect(h.Spec.WebSSO).To(BeNil())
}

// TestReconcileHorizon_MultiDomainKeepsTheDomainFieldFreeText is the regression
// guard for the lockout an operator never asked for: pinning domainChoices to
// the LDAP-backed domains makes Django reject every other domain, so a single
// LDAP attach would take away the login of every user in a SQL-backed domain
// the operator cannot enumerate.
func TestReconcileHorizon_MultiDomainKeepsTheDomainFieldFreeText(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	ldap := horizonBackend("openldap", keystonev1alpha1.IdentityBackendTypeLDAP, "planetexpress", true)
	pending := horizonBackend("ldap-pending", keystonev1alpha1.IdentityBackendTypeLDAP, "futurama", false)
	r := newHorizonTestReconciler(t, cp, ldap, pending)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.MultiDomain.DomainDropdown).To(BeFalse(),
		"a fixed dropdown would reject every domain the operator does not back with LDAP")
	g.Expect(h.Spec.MultiDomain.DomainChoices).To(BeEmpty())
}

// TestReconcileHorizon_WebSSOOmittedWithoutBrowserFacingEndpoints is the
// regression guard for the dead SSO button: a Ready backend alone is not enough
// to complete a hand-off. Without a trusted dashboard origin, Keystone bounces
// the browser only AFTER the user has entered their corporate credentials;
// without a browser-facing Keystone URL the redirect targets cluster-local DNS.
func TestReconcileHorizon_WebSSOOmittedWithoutBrowserFacingEndpoints(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(cp *c5c3v1alpha1.ControlPlane)
		wantSSO bool
	}{
		{
			"neither endpoint published",
			func(*c5c3v1alpha1.ControlPlane) {},
			false,
		},
		{
			"dashboard unreachable, so Keystone would trust no origin",
			func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
					PublicEndpoint: "https://keystone.127-0-0-1.nip.io/v3",
				}
			},
			false,
		},
		{
			"Keystone unreachable, so the redirect would target cluster-local DNS",
			func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Horizon.PublicEndpoint = "https://horizon.127-0-0-1.nip.io"
			},
			false,
		},
		{"both published", publishSSOEndpoints, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := horizonControlPlane()
			tc.mutate(cp)
			kc := horizonBackend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
			r := newHorizonTestReconciler(t, cp, kc)

			_, err := r.reconcileHorizon(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())

			h := getProjectedHorizon(t, r.Client, cp)
			if tc.wantSSO {
				g.Expect(h.Spec.WebSSO).NotTo(BeNil())
				return
			}
			g.Expect(h.Spec.WebSSO).To(BeNil(),
				"an SSO button that can never complete is worse than no button")
		})
	}
}

// TestReconcileHorizon_WebSSOCapsFederatedChoices guards the projection against
// the wedge one backend too many would cause: websso.choices is bounded at 17
// items by the Horizon CRD, so an uncapped list would be rejected by the API
// server and block every later change to the dashboard.
func TestReconcileHorizon_WebSSOCapsFederatedChoices(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	publishSSOEndpoints(cp)

	objs := []client.Object{cp}
	for i := 0; i < maxProjectedFederationChoices+4; i++ {
		// Zero-padded so the sort order matches the creation order and the
		// dropped backends are the last four.
		objs = append(objs, horizonBackend(fmt.Sprintf("idp-%02d", i),
			keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true))
	}
	r := newHorizonTestReconciler(t, objs...)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.WebSSO.Choices).To(HaveLen(maxProjectedFederationChoices+1),
		"the local-credentials fallback plus the capped federated choices")
	g.Expect(h.Spec.WebSSO.IDPMapping).To(HaveLen(maxProjectedFederationChoices))
	g.Expect(h.Spec.WebSSO.Choices[0].ID).To(Equal(horizonv1alpha1.DefaultWebSSOLocalChoiceID))
	g.Expect(h.Spec.WebSSO.IDPMapping).To(HaveKey("idp-00_openid"))
	g.Expect(h.Spec.WebSSO.IDPMapping).NotTo(HaveKey("idp-19_openid"))
}

// TestReconcileHorizon_ClearsWebSSOWhenBackendsDetached proves the projection
// is assigned unconditionally: detaching the last backend must remove the SSO
// button rather than leave the previously-projected block pinned on the child.
func TestReconcileHorizon_ClearsWebSSOWhenBackendsDetached(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := horizonControlPlane()
	publishSSOEndpoints(cp)
	kc := horizonBackend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	ldap := horizonBackend("openldap", keystonev1alpha1.IdentityBackendTypeLDAP, "planetexpress", true)
	r := newHorizonTestReconciler(t, cp, kc, ldap)

	_, err := r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedHorizon(t, r.Client, cp).Spec.WebSSO).NotTo(BeNil())
	g.Expect(getProjectedHorizon(t, r.Client, cp).Spec.MultiDomain).NotTo(BeNil())

	g.Expect(r.Delete(context.Background(), kc)).To(Succeed())
	g.Expect(r.Delete(context.Background(), ldap)).To(Succeed())

	_, err = r.reconcileHorizon(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	h := getProjectedHorizon(t, r.Client, cp)
	g.Expect(h.Spec.WebSSO).To(BeNil(), "detaching the last OIDC backend must clear the websso block")
	g.Expect(h.Spec.MultiDomain).To(BeNil(), "detaching the last LDAP backend must clear the multiDomain block")
}
