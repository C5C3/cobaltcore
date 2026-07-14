// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Keystone sub-reconciler.
package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// keystoneTestScheme registers c5c3, client-go, and keystone types.
func keystoneTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := keystonev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding keystone scheme: %v", err)
	}
	return s
}

// keystoneControlPlane builds a ControlPlane with managed infrastructure and an
// InfrastructureReady=True condition already set (gate passed).
func keystoneControlPlane() *c5c3v1alpha1.ControlPlane {
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
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				},
			},
		},
	}
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeInfrastructureReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "InfrastructureReady",
		Message:            "ready",
	})
	return cp
}

func getProjectedKeystone(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane) *keystonev1alpha1.Keystone {
	t.Helper()
	k := &keystonev1alpha1.Keystone{}
	key := types.NamespacedName{Name: keystoneName(cp), Namespace: childNamespace(cp)}
	if err := c.Get(context.Background(), key, k); err != nil {
		t.Fatalf("getting projected Keystone %s: %v", key, err)
	}
	return k
}

func TestReconcileKeystone_ImageTagFromRelease(t *testing.T) {
	// Two rows prove the tag is DERIVED from openStackRelease (a different release
	// yields a different tag), rather than coincidentally matching a single literal
	// (PR1).
	for _, tt := range []struct {
		release string
		wantTag string
	}{
		{release: "2025.2", wantTag: "2025.2"},
		{release: "2026.1", wantTag: "2026.1"},
	} {
		t.Run(tt.release, func(t *testing.T) {
			g := NewGomegaWithT(t)

			s := keystoneTestScheme(t)
			cp := keystoneControlPlane()
			cp.Spec.OpenStackRelease = tt.release
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
			r := &ControlPlaneReconciler{Client: c, Scheme: s}

			_, err := r.reconcileKeystone(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())

			k := getProjectedKeystone(t, c, cp)
			g.Expect(k.Spec.Image.Repository).To(Equal(defaultKeystoneRepository))
			g.Expect(k.Spec.Image.Tag).To(Equal(tt.wantTag),
				"image tag must derive from openStackRelease (%s)", tt.release)

			// The federation proxy default is projected alongside the
			// service image (release-independent, so no derived tag).
			g.Expect(k.Spec.Federation).NotTo(BeNil())
			g.Expect(k.Spec.Federation.ProxyImage).NotTo(BeNil())
			g.Expect(k.Spec.Federation.ProxyImage.Repository).To(Equal(defaultFederationProxyRepository))
			g.Expect(k.Spec.Federation.ProxyImage.Tag).To(Equal("latest"))

			// Owner reference set to the ControlPlane.
			g.Expect(k.OwnerReferences).To(HaveLen(1))
			g.Expect(k.OwnerReferences[0].Name).To(Equal("cp"))
			g.Expect(k.OwnerReferences[0].Kind).To(Equal("ControlPlane"))
		})
	}
}

// TestReconcileKeystone_NotManagedWhenServiceUnset verifies that a ControlPlane
// with spec.services.keystone unset projects no Keystone child and reports
// KeystoneReady as not-managed (staged adoption / externally-managed Keystone).
func TestReconcileKeystone_NotManagedWhenServiceUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone = nil
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	// No Keystone child is created.
	k := &keystonev1alpha1.Keystone{}
	key := types.NamespacedName{Name: keystoneName(cp), Namespace: childNamespace(cp)}
	g.Expect(apierrors.IsNotFound(c.Get(context.Background(), key, k))).To(BeTrue(),
		"no Keystone child must be projected when services.keystone is unset")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKeystoneReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("KeystoneNotManaged"))
}

// TestReconcileKeystone_NilInfrastructureDoesNotPanic exercises the defensive
// nil-Infrastructure guard on a NON-External ControlPlane — the webhook-bypass
// shape, since the validating webhook requires spec.infrastructure outside
// External mode. The fixture keeps InfrastructureReady=True, so without a local
// guard the projection's cp.Spec.Infrastructure derefs would panic once the gate
// is passed. The guard must instead requeue and project no child.
func TestReconcileKeystone_NilInfrastructureDoesNotPanic(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Infrastructure = nil
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	// No Keystone child is projected against the absent infrastructure.
	k := &keystonev1alpha1.Keystone{}
	key := types.NamespacedName{Name: keystoneName(cp), Namespace: childNamespace(cp)}
	g.Expect(apierrors.IsNotFound(c.Get(context.Background(), key, k))).To(BeTrue(),
		"no Keystone child must be projected when spec.infrastructure is nil")
}

// TestReconcileKeystone_PreservesChildOnFlipToNil verifies that flipping
// spec.services.keystone from set to nil PRESERVES the previously-projected
// Keystone child by default (no opt-in annotation). Deleting it would cascade to
// the child's irreplaceable credential-keys Secret, so an accidental unset must
// be fail-safe rather than fail-destructive.
func TestReconcileKeystone_PreservesChildOnFlipToNil(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// First pass projects the Keystone child.
	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	getProjectedKeystone(t, c, cp)

	// Flip services.keystone to nil and reconcile again, WITHOUT the opt-in
	// annotation.
	cp.Spec.Services.Keystone = nil
	_, err = r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	// The Keystone child is preserved (its credential/fernet keys are safe).
	getProjectedKeystone(t, c, cp)

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKeystoneReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("KeystoneNotManaged"))
	g.Expect(cond.Message).To(ContainSubstring("preserved"),
		"the not-managed message must tell the operator the child was preserved")
}

// TestReconcileKeystone_DeletesChildOnFlipToNilWithOptIn verifies that with the
// explicit keystoneDeletionAllowedAnnotation opt-in, flipping
// spec.services.keystone to nil deletes the previously-projected Keystone child.
func TestReconcileKeystone_DeletesChildOnFlipToNilWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// First pass projects the Keystone child.
	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	getProjectedKeystone(t, c, cp)

	// Flip services.keystone to nil AND opt in to deletion, then reconcile again.
	cp.Spec.Services.Keystone = nil
	cp.Annotations = map[string]string{keystoneDeletionAllowedAnnotation: "true"}
	_, err = r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	// The opted-in Keystone child is deleted.
	k := &keystonev1alpha1.Keystone{}
	key := types.NamespacedName{Name: keystoneName(cp), Namespace: childNamespace(cp)}
	g.Expect(apierrors.IsNotFound(c.Get(context.Background(), key, k))).To(BeTrue(),
		"opted-in Keystone child must be deleted on services.keystone set→nil flip")
}

func TestReconcileKeystone_ImageOverrideWins(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone.Image = &commonv1.ImageSpec{
		Repository: "registry.internal/keystone",
		Tag:        "custom-tag",
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Image.Repository).To(Equal("registry.internal/keystone"))
	g.Expect(k.Spec.Image.Tag).To(Equal("custom-tag"), "explicit image override must win over release-derived default")
}

func TestReconcileKeystone_ClusterRefsDerivedFromInfrastructure(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Database.ClusterRef).NotTo(BeNil())
	g.Expect(k.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(k.Spec.Database.Database).To(Equal("keystone"))
	g.Expect(k.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(k.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	// Admin password derived from the operator-projected per-CP Secret in managed mode.
	g.Expect(k.Spec.Bootstrap.AdminPasswordSecretRef.Name).To(Equal(adminPasswordSecretName(cp)))
	g.Expect(k.Spec.Bootstrap.Region).To(Equal("RegionOne"))
}

func TestReconcileKeystone_PolicyMerge(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.GlobalPolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{
			"identity:create_user": "role:admin",
			"identity:list_users":  "role:admin",
		},
	}
	cp.Spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{
			"identity:list_users": "role:reader", // overrides global
			"identity:get_user":   "role:reader", // new key
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.PolicyOverrides).NotTo(BeNil())
	g.Expect(k.Spec.PolicyOverrides.Rules).To(Equal(map[string]string{
		"identity:create_user": "role:admin",
		"identity:list_users":  "role:reader",
		"identity:get_user":    "role:reader",
	}), "per-service overrides must win, global rules merged in")
}

func TestReconcileKeystone_ScheduleConversionWeekly(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone.RotationInterval = &metav1.Duration{Duration: 168 * time.Hour}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Fernet.RotationSchedule).To(Equal("0 0 * * 0"), "168h must map to weekly cron")
	g.Expect(k.Spec.CredentialKeys.RotationSchedule).To(Equal("0 0 * * 0"))
}

func TestReconcileKeystone_InvalidRotationIntervalSetsFalse(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone.RotationInterval = &metav1.Duration{Duration: 5 * time.Hour}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	// The sub-reconciler now RETURNS the error so the Reconcile chain stops here
	// (the chain guard keys off err != nil) and the manager requeues with
	// backoff, rather than returning a zero Result that lets the chain continue
	// past this failed sub-reconciler (#476).
	g.Expect(err).To(HaveOccurred(), "an invalid rotation interval must surface as an error")
	g.Expect(err.Error()).To(ContainSubstring("rotation interval"))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKeystoneReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("InvalidRotationInterval"))

	// No Keystone CR must be created when the interval is invalid.
	k := &keystonev1alpha1.Keystone{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Name: keystoneName(cp), Namespace: childNamespace(cp),
	}, k)
	g.Expect(getErr).To(HaveOccurred(), "no Keystone CR should be created for an invalid rotation interval")
}

func TestReconcileKeystone_ReplicasPassthrough(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	replicas := int32(5)
	cp.Spec.Services.Keystone.Replicas = &replicas
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Deployment.Replicas).To(Equal(int32(5)))
}

func TestReconcileKeystone_InfraGatingNoKeystoneCreated(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	// Remove the InfrastructureReady condition so the gate blocks projection.
	cp.Status.Conditions = nil
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(keystoneInfraGateRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKeystoneReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForInfrastructure"))

	// No Keystone CR may be created while infrastructure is not ready.
	k := &keystonev1alpha1.Keystone{}
	err = c.Get(context.Background(), types.NamespacedName{
		Name: keystoneName(cp), Namespace: childNamespace(cp),
	}, k)
	g.Expect(err).To(HaveOccurred(), "no Keystone CR may exist while InfrastructureReady is absent")
}

func TestReconcileKeystone_GatewayProjection(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{
			Name:        "openstack-gw",
			Namespace:   "openstack",
			SectionName: "https",
		},
		Hostname:    "keystone.127-0-0-1.nip.io",
		Path:        "/",
		Annotations: map[string]string{"foo": "bar"},
	}
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.127-0-0-1.nip.io:8443/v3"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Gateway).NotTo(BeNil(), "a shared gateway must be projected onto the Keystone CR")
	g.Expect(k.Spec.Gateway.ParentRef.Name).To(Equal("openstack-gw"))
	g.Expect(k.Spec.Gateway.ParentRef.Namespace).To(Equal("openstack"))
	g.Expect(k.Spec.Gateway.ParentRef.SectionName).To(Equal("https"))
	g.Expect(k.Spec.Gateway.Hostname).To(Equal("keystone.127-0-0-1.nip.io"))
	g.Expect(k.Spec.Gateway.Path).To(Equal("/"))
	g.Expect(k.Spec.Gateway.Annotations).To(HaveKeyWithValue("foo", "bar"))
	// Explicit publicEndpoint (carrying the kind :8443 host port) wins verbatim.
	g.Expect(k.Spec.Bootstrap.PublicEndpoint).To(Equal("https://keystone.127-0-0-1.nip.io:8443/v3"))
}

// TestReconcileKeystone_ProjectsSecretStoreRef verifies the ControlPlane's
// spec.secretStoreRef is projected onto the Keystone child.
func TestReconcileKeystone_ProjectsSecretStoreRef(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.SecretStoreRef).NotTo(BeNil(), "the ControlPlane store ref must be projected onto the Keystone child")
	g.Expect(k.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(k.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))
}

// TestReconcileKeystone_DefaultsSecretStoreRefToTenantStore verifies that a
// ControlPlane without an explicit store ref projects the operator-provisioned
// per-tenant namespaced store onto the Keystone child, so the child reaches
// OpenBao as the tenant identity rather than the shared cluster store.
func TestReconcileKeystone_DefaultsSecretStoreRefToTenantStore(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.SecretStoreRef).NotTo(BeNil(),
		"a nil ControlPlane store ref must project the per-tenant store, not nil")
	g.Expect(k.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(k.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))
}

func TestReconcileKeystone_GatewayNilStaysInCluster(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Gateway).To(BeNil(), "no gateway must be projected when the shared gateway is unset (in-cluster only is the default)")
	g.Expect(k.Spec.Bootstrap.PublicEndpoint).To(BeEmpty(), "no public endpoint must be advertised when neither gateway nor publicEndpoint is set")
}

func TestReconcileKeystone_PublicEndpointDerivedFromHostname(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.example.com",
	}
	// publicEndpoint intentionally left empty → derived from the gateway hostname.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Bootstrap.PublicEndpoint).To(Equal("https://keystone.example.com/v3"),
		"publicEndpoint must derive from the gateway hostname (default-443 form) when not set explicitly")
}

func TestReconcileKeystone_MirrorsChildReady(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()

	// Pre-create a Ready Keystone child so create-or-update finds it and the
	// sub-reconciler mirrors KeystoneReady=True.
	existing := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneName(cp),
			Namespace: childNamespace(cp),
		},
		Status: keystonev1alpha1.KeystoneStatus{
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				Message:            "ready",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, existing).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKeystoneReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// TestReconcileKeystone_InvalidRejectionSurfacesDistinctReason is the regression
// guard for #466: when the projected spec collides with a now-immutable Keystone
// db/bootstrap field (e.g. a spec.region edit that landed on the ControlPlane
// before its own immutability webhook existed, diverging it from the already-
// frozen Keystone child), the Keystone API server rejects the UPDATE with an
// Invalid (422) error and the loop re-attempts it on every requeue with no
// self-heal. The sub-reconciler must surface a distinct, actionable KeystoneReady
// reason rather than the generic KeystoneError so the wedge is diagnosable.
func TestReconcileKeystone_InvalidRejectionSurfacesDistinctReason(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()

	// A pre-existing Keystone child whose region differs from the ControlPlane's,
	// so the SSA apply is rejected by the interceptor, standing in for the CEL
	// immutability transition rule.
	existing := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneName(cp),
			Namespace: childNamespace(cp),
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Bootstrap: keystonev1alpha1.BootstrapSpec{Region: "OldRegion"},
		},
	}

	invalidErr := apierrors.NewInvalid(
		schema.GroupKind{Group: keystonev1alpha1.GroupVersion.Group, Kind: "Keystone"},
		existing.Name,
		field.ErrorList{field.Invalid(
			field.NewPath("spec", "bootstrap", "region"), cp.Spec.Region, "bootstrap.region is immutable",
		)},
	)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				return invalidErr
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).To(HaveOccurred(), "the Invalid rejection must propagate so the manager requeues with backoff")
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKeystoneReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("KeystoneProjectionRejected"),
		"an Invalid (422) rejection must surface a distinct, actionable reason, not the generic KeystoneError")
	g.Expect(cond.Message).To(ContainSubstring("immutable"),
		"the underlying immutability rejection must be carried into the condition message")
}

func TestReconcileKeystone_ManagedOverridesDBSecretRef(t *testing.T) {
	// In MANAGED mode (Database.ClusterRef != nil) the projected Keystone CR's
	// database.secretRef must point at the operator-owned per-ControlPlane
	// DB-credential Secret, not the cp-level default. The override must not
	// mutate cp.Spec.
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Database.SecretRef.Name).To(Equal(dbCredentialSecretName(cp)),
		"managed mode must point the child at the per-ControlPlane DB-credential Secret")
	g.Expect(k.Spec.Database.SecretRef.Key).To(Equal("password"))
	// The rest of the database spec must still flow through (so the test can't
	// pass by clearing the whole struct).
	g.Expect(k.Spec.Database.ClusterRef).NotTo(BeNil())
	g.Expect(k.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	// The override must not mutate the source spec.
	g.Expect(cp.Spec.Infrastructure.Database.SecretRef.Name).To(Equal("keystone-db"),
		"the secretRef override must not mutate cp.Spec")
}

func TestReconcileKeystone_BrownfieldLeavesSuppliedSecretRef(t *testing.T) {
	// In BROWNFIELD mode (Database.ClusterRef == nil) the user owns the DB Secret
	// out-of-band, so the operator must leave the supplied secretRef untouched
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	// Keep the InfrastructureReady=True condition set by keystoneControlPlane so
	// the gate passes; only replace the database with a brownfield spec.
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "user-supplied-db-secret", Key: "pw"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Database.SecretRef.Name).To(Equal("user-supplied-db-secret"),
		"brownfield mode must leave the user-supplied secretRef untouched")
	g.Expect(k.Spec.Database.SecretRef.Key).To(Equal("pw"))
}

func TestReconcileKeystone_ManagedOverridesAdminPasswordSecretRef(t *testing.T) {
	// In MANAGED mode (Database.ClusterRef != nil) the projected Keystone CR's
	// bootstrap admin-password ref must point at the operator-owned per-ControlPlane
	// admin-password Secret, not the cp-level default. The override must not
	// mutate cp.Spec.
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Bootstrap.AdminPasswordSecretRef.Name).To(Equal(adminPasswordSecretName(cp)),
		"managed mode must point the child at the per-ControlPlane admin-password Secret")
	g.Expect(k.Spec.Bootstrap.AdminPasswordSecretRef.Key).To(Equal("password"))
	// The override must not mutate the source spec.
	g.Expect(cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name).To(Equal("keystone-admin"),
		"the admin-password ref override must not mutate cp.Spec")
}

func TestReconcileKeystone_BrownfieldLeavesSuppliedAdminPasswordRef(t *testing.T) {
	// In BROWNFIELD mode (Database.ClusterRef == nil) the user owns the admin-password
	// Secret out-of-band, so the operator must leave the supplied ref untouched
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	// Keep the InfrastructureReady=True condition set by keystoneControlPlane so
	// the gate passes; only replace the database with a brownfield spec.
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "user-supplied-db-secret", Key: "pw"},
	}
	cp.Spec.KORC.AdminCredential.PasswordSecretRef = commonv1.SecretRefSpec{Name: "user-supplied-admin", Key: "pw"}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Bootstrap.AdminPasswordSecretRef.Name).To(Equal("user-supplied-admin"),
		"brownfield mode must leave the user-supplied admin-password ref untouched")
	g.Expect(k.Spec.Bootstrap.AdminPasswordSecretRef.Key).To(Equal("pw"))
}

// keystoneExternalControlPlane builds an External-mode ControlPlane: no
// spec.infrastructure block, identity managed against a pre-existing Keystone.
func keystoneExternalControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := keystoneControlPlane()
	cp.Spec.Infrastructure = nil
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Mode:     c5c3v1alpha1.KeystoneModeExternal,
		External: &c5c3v1alpha1.ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
	}
	return cp
}

// TestReconcileKeystone_ExternalModeNoChildProjected asserts the External-mode
// short-circuit: KeystoneReady=True/ExternallyManaged, a message naming the
// external endpoint, no requeue, and provably no Keystone child.
func TestReconcileKeystone_ExternalModeNoChildProjected(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneExternalControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "the External short-circuit must not requeue")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKeystoneReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonExternallyManaged))
	g.Expect(cond.Message).To(ContainSubstring("https://keystone.example.com/v3"))

	k := &keystonev1alpha1.Keystone{}
	key := types.NamespacedName{Name: keystoneName(cp), Namespace: childNamespace(cp)}
	g.Expect(apierrors.IsNotFound(c.Get(context.Background(), key, k))).To(BeTrue(),
		"External mode must not project a Keystone child")
}

// TestReconcileKeystone_ExternalModeReasonDistinctFromNotManaged pins the
// vocabulary split behaviorally: "identity lives elsewhere" (External) and "there
// is no identity plane at all" (services.keystone unset) both report
// KeystoneReady=True, but an operator must be able to tell them apart from the
// reason alone.
func TestReconcileKeystone_ExternalModeReasonDistinctFromNotManaged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)

	external := keystoneExternalControlPlane()
	unmanaged := keystoneControlPlane()
	unmanaged.Name = "cp-unmanaged"
	unmanaged.Spec.Services.Keystone = nil

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(external, unmanaged).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), external)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.reconcileKeystone(context.Background(), unmanaged)
	g.Expect(err).NotTo(HaveOccurred())

	externalCond := conditions.GetCondition(external.Status.Conditions, conditionTypeKeystoneReady)
	unmanagedCond := conditions.GetCondition(unmanaged.Status.Conditions, conditionTypeKeystoneReady)
	g.Expect(externalCond.Reason).To(Equal(conditionReasonExternallyManaged))
	g.Expect(unmanagedCond.Reason).To(Equal("KeystoneNotManaged"))
	g.Expect(externalCond.Reason).NotTo(Equal(unmanagedCond.Reason))
}

// TestReconcileKeystone_ExternalModePreservesAnUnexpectedChild covers the
// destructive edge path. A Managed -> External flip is rejected at admission, so
// no child can exist here; if one does anyway (a webhook-bypassed CR), the
// External branch must NOT cascade-delete it — the child's credential/fernet keys
// are irreplaceable.
func TestReconcileKeystone_ExternalModePreservesAnUnexpectedChild(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneExternalControlPlane()
	child := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneName(cp), Namespace: childNamespace(cp)},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, child).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := &keystonev1alpha1.Keystone{}
	key := types.NamespacedName{Name: keystoneName(cp), Namespace: childNamespace(cp)}
	g.Expect(c.Get(context.Background(), key, k)).To(Succeed(),
		"an unexpected pre-existing Keystone child must be preserved, never deleted")
}

// TestReconcileKeystone_FederationProxyImageOverrideWins proves the override
// reaches the child: without it the suite would validate the sidecar published
// on main rather than the one under review.
func TestReconcileKeystone_FederationProxyImageOverrideWins(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone.FederationProxyImage = &commonv1.ImageSpec{
		Repository: "ghcr.io/c5c3/keystone-federation-proxy",
		Digest:     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Federation.ProxyImage.Digest).To(Equal(
		"sha256:1111111111111111111111111111111111111111111111111111111111111111",
	))
	g.Expect(k.Spec.Federation.ProxyImage.Tag).To(BeEmpty(),
		"an immutable digest pin must not also carry the mutable default tag")
}

// TestReconcileKeystone_ClearsFederationProxyImageOverride proves the field is
// assigned unconditionally: clearing the override must revert the child to the
// default rather than leave the previously-projected pin.
func TestReconcileKeystone_ClearsFederationProxyImageOverride(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone.FederationProxyImage = &commonv1.ImageSpec{
		Repository: "ghcr.io/c5c3/keystone-federation-proxy", Tag: "dev",
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedKeystone(t, c, cp).Spec.Federation.ProxyImage.Tag).To(Equal("dev"))

	cp.Spec.Services.Keystone.FederationProxyImage = nil
	_, err = r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedKeystone(t, c, cp).Spec.Federation.ProxyImage.Tag).To(Equal("latest"))
}

// TestReconcileKeystone_TrustedDashboardsProjectedFromHorizonPublicEndpoint
// covers the non-default-port case the override exists for: Keystone matches
// the origin verbatim, so the port must survive into trusted_dashboard.
func TestReconcileKeystone_TrustedDashboardsProjectedFromHorizonPublicEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{
		PublicEndpoint: "https://horizon.127-0-0-1.nip.io:8443",
		Gateway:        &commonv1.GatewaySpec{Hostname: "horizon.127-0-0-1.nip.io"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Federation.TrustedDashboards).To(Equal(
		[]string{"https://horizon.127-0-0-1.nip.io:8443/auth/websso/"},
	))
}

// TestReconcileKeystone_TrustedDashboardsNilWithoutHorizonBlock keeps a
// Keystone-only ControlPlane rendering no [federation] trusted_dashboard, and
// proves the field is cleared when the dashboard is removed.
func TestReconcileKeystone_TrustedDashboardsNilWithoutHorizonBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{
		Gateway: &commonv1.GatewaySpec{Hostname: "horizon.example.com"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedKeystone(t, c, cp).Spec.Federation.TrustedDashboards).To(
		Equal([]string{"https://horizon.example.com/auth/websso/"}),
	)

	cp.Spec.Services.Horizon = nil
	_, err = r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedKeystone(t, c, cp).Spec.Federation.TrustedDashboards).To(BeNil(),
		"removing the dashboard must clear the trusted origin")
}

// TestReconcileKeystone_DedicatedBackingServicesProjected verifies the Keystone
// child is pointed at the instances the service actually got: its DEDICATED
// database and cache rather than the ControlPlane-wide ones. The projected spec
// is what the keystone-operator derives its logical database, its MariaDB
// User/Grant CRs, and its NetworkPolicy egress rules from, so pointing it at the
// dedicated instances is what carries the isolation through the rest of the
// chain.
func TestReconcileKeystone_DedicatedBackingServicesProjected(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone.DedicatedBackingServices = &c5c3v1alpha1.KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-keystone-db"},
			CredentialsMode: commonv1.CredentialsModeStatic,
			Database:        "keystone",
			SecretRef:       commonv1.SecretRefSpec{Name: "seeded-db"},
		},
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
			Backend:    commonv1.DefaultCacheBackend,
			Replicas:   1,
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Database.ClusterRef).NotTo(BeNil())
	g.Expect(k.Spec.Database.ClusterRef.Name).To(Equal("cp-keystone-db"),
		"the child must point at the dedicated database, not the shared one")
	g.Expect(k.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(k.Spec.Cache.ClusterRef.Name).To(Equal("cp-keystone-cache"),
		"the child must point at the dedicated cache, not the shared one")
	// A dedicated MANAGED database still has the operator own the credential, so
	// the user-supplied secretRef is overridden onto the per-ControlPlane Secret —
	// and the effective mode is Static (no engine role exists for a dedicated
	// instance).
	g.Expect(k.Spec.Database.SecretRef.Name).To(Equal(dbCredentialSecretName(cp)))
	g.Expect(k.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))

	// The projection must not alias the ControlPlane spec: mutating the child's
	// clusterRef must leave the ControlPlane's dedicated declaration intact.
	k.Spec.Database.ClusterRef.Name = "mutated"
	g.Expect(cp.Spec.Services.Keystone.DedicatedBackingServices.Database.ClusterRef.Name).
		To(Equal("cp-keystone-db"), "the projection must DeepCopy the dedicated spec")
}

// TestReconcileKeystone_DedicatedBrownfieldLeavesSuppliedSecretRef covers the
// brownfield half of the dedicated split: a service pointed at an externally
// operated database of its own keeps the user-supplied credential Secret, exactly
// as a brownfield SHARED database does — the operator owns no credential it did
// not provision.
func TestReconcileKeystone_DedicatedBrownfieldLeavesSuppliedSecretRef(t *testing.T) {
	g := NewGomegaWithT(t)

	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone.DedicatedBackingServices = &c5c3v1alpha1.KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			Host:      "keystone-db.example.com",
			Port:      3306,
			Database:  "keystone",
			SecretRef: commonv1.SecretRefSpec{Name: "user-supplied-db-creds"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	k := getProjectedKeystone(t, c, cp)
	g.Expect(k.Spec.Database.ClusterRef).To(BeNil())
	g.Expect(k.Spec.Database.Host).To(Equal("keystone-db.example.com"))
	g.Expect(k.Spec.Database.SecretRef.Name).To(Equal("user-supplied-db-creds"),
		"a brownfield dedicated database must keep the user-supplied credential Secret")
	// Only the DATABASE was taken dedicated: the cache stays shared.
	g.Expect(k.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(k.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
}

// --- per-service namespaces (issue #646) ---

// TestReconcileKeystone_ProjectsIntoTheAssignedNamespace verifies the Keystone
// child is placed in the namespace services.keystone.namespace assigns, carries
// the ownership labels (no owner reference is possible across namespaces), and
// that nothing is projected into the ControlPlane's own namespace.
func TestReconcileKeystone_ProjectsIntoTheAssignedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      "identity",
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &keystonev1alpha1.Keystone{}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKeystone(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var keystone keystonev1alpha1.Keystone
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "cp-keystone", Namespace: "identity",
	}, &keystone)).To(Succeed())
	g.Expect(keystone.OwnerReferences).To(BeEmpty(),
		"a cross-namespace child cannot carry an owner reference")
	g.Expect(keystone.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(keystone.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))

	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "cp-keystone", Namespace: "default",
	}, &keystonev1alpha1.Keystone{})).NotTo(Succeed(),
		"nothing may be left behind in the ControlPlane's namespace")
}

// TestDeleteOrphanedKeystone_CrossNamespace verifies the orphan cleanup follows
// the child into its namespace and honours the LABEL ownership test there — a
// same-named Keystone the ControlPlane does not own must survive.
func TestDeleteOrphanedKeystone_CrossNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := keystoneTestScheme(t)
	cp := keystoneControlPlane()
	cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      "identity",
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}

	ours := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-keystone", Namespace: "identity"},
	}
	stampControlPlaneChildLabels(ours, cp)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ours).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	g.Expect(r.deleteOrphanedKeystone(context.Background(), cp)).To(Succeed())
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "cp-keystone", Namespace: "identity",
	}, &keystonev1alpha1.Keystone{})).NotTo(Succeed())

	// An unlabelled Keystone of the same name is not ours and must survive.
	foreign := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-keystone", Namespace: "identity"},
	}
	c2 := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
	r2 := &ControlPlaneReconciler{Client: c2, Scheme: s}
	g.Expect(r2.deleteOrphanedKeystone(context.Background(), cp)).To(Succeed())
	g.Expect(c2.Get(context.Background(), types.NamespacedName{
		Name: "cp-keystone", Namespace: "identity",
	}, &keystonev1alpha1.Keystone{})).To(Succeed(), "a Keystone we do not own must never be deleted")
}

// TestKeystoneEndpointURL_FollowsTheServiceNamespace pins the cross-namespace
// service-discovery mechanism: the namespace-qualified Service DNS is what lets a
// service in one namespace still reach the identity service in another.
func TestKeystoneEndpointURL_FollowsTheServiceNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := keystoneControlPlane()
	g.Expect(keystoneEndpointURL(cp)).To(Equal("http://cp-keystone.default.svc:5000/v3"),
		"an unassigned Keystone resolves in the ControlPlane's namespace, as before")

	cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"}
	g.Expect(keystoneEndpointURL(cp)).To(Equal("http://cp-keystone.identity.svc:5000/v3"))
}
