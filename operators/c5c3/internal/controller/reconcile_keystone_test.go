// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Keystone sub-reconciler (CC-0110, REQ-009).
package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
			Infrastructure: c5c3v1alpha1.InfrastructureSpec{
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
	// (CC-0110, PR1).
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

			// Owner reference set to the ControlPlane.
			g.Expect(k.OwnerReferences).To(HaveLen(1))
			g.Expect(k.OwnerReferences[0].Name).To(Equal("cp"))
			g.Expect(k.OwnerReferences[0].Kind).To(Equal("ControlPlane"))
		})
	}
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
	cp.Spec.Global = &commonv1.PolicySpec{
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
	g.Expect(err).NotTo(HaveOccurred(), "an invalid interval must not crash the reconciler")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKeystoneReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("InvalidRotationInterval"))

	// No Keystone CR must be created when the interval is invalid.
	k := &keystonev1alpha1.Keystone{}
	err = c.Get(context.Background(), types.NamespacedName{
		Name: keystoneName(cp), Namespace: childNamespace(cp),
	}, k)
	g.Expect(err).To(HaveOccurred(), "no Keystone CR should be created for an invalid rotation interval")
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
	g.Expect(k.Spec.Replicas).To(Equal(int32(5)))
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

func TestReconcileKeystone_ManagedOverridesDBSecretRef(t *testing.T) {
	// In MANAGED mode (Database.ClusterRef != nil) the projected Keystone CR's
	// database.secretRef must point at the operator-owned per-ControlPlane
	// DB-credential Secret, not the cp-level default. The override must not
	// mutate cp.Spec (CC-0116, REQ-003).
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
	// (CC-0116, REQ-003).
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
	// mutate cp.Spec (CC-0117, REQ-005).
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
	// (CC-0117, REQ-005).
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
