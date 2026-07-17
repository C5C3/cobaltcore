// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Glance sub-reconciler.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
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
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// glanceTestScheme registers c5c3, client-go, glance, and external-secrets types
// (the projection ensures a DB-credential ExternalSecret).
func glanceTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := glancev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding glance scheme: %v", err)
	}
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets scheme: %v", err)
	}
	return s
}

// glanceControlPlane builds a ControlPlane with services.glance set, a
// KeystoneReady=True condition, the auto-injected glance service account, and a
// Ready per-account status for it — i.e. every gate reconcileGlance checks is
// already passed.
func glanceControlPlane() *c5c3v1alpha1.ControlPlane {
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
				Glance: &c5c3v1alpha1.ServiceGlanceSpec{
					Backends: []c5c3v1alpha1.GlanceBackendEntry{{
						Name:      "primary",
						Type:      "S3",
						IsDefault: true,
						S3: &c5c3v1alpha1.GlanceBackendS3Spec{
							Endpoint:             "https://s3.example.com",
							Bucket:               "images",
							CredentialsSecretRef: c5c3v1alpha1.SecretNameRef{Name: "glance-s3-creds"},
						},
					}},
				},
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				},
				ServiceAccounts: []c5c3v1alpha1.ServiceAccountSpec{{
					Name:    "glance",
					Project: c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service", Create: true},
					Roles:   []string{"service"},
				}},
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
	cp.Status.ServiceAccounts = []c5c3v1alpha1.ServiceAccountStatus{{Name: "glance", Ready: true}}
	return cp
}

func newGlanceTestReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := glanceTestScheme(t)
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &glancev1alpha1.Glance{})
	return &ControlPlaneReconciler{Client: cb.Build(), Scheme: s}
}

func getProjectedGlance(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane) *glancev1alpha1.Glance {
	t.Helper()
	gl := &glancev1alpha1.Glance{}
	key := types.NamespacedName{Name: glanceName(cp), Namespace: cp.GlanceNamespace()}
	if err := c.Get(context.Background(), key, gl); err != nil {
		t.Fatalf("getting projected Glance %s: %v", key, err)
	}
	return gl
}

// --- gates ---

func TestReconcileGlance_NotManagedWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance = nil
	r := newGlanceTestReconciler(t, cp)

	res, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("GlanceNotManaged"))

	var list glancev1alpha1.GlanceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

func TestReconcileGlance_UnsetPreservesChildByDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cp.Spec.Services.Glance = nil
	_, err = r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl).NotTo(BeNil(), "child must be preserved without the opt-in annotation")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Reason).To(Equal("GlanceNotManaged"))
	g.Expect(cond.Message).To(ContainSubstring(glanceDeletionAllowedAnnotation))
}

// TestReconcileGlance_UnsetDeletesChildWithOptIn verifies the opt-in deletion
// sweep removes the child AND the DB-credential ExternalSecret, and — the
// ownership guard — never touches a hand-created Glance child the ControlPlane
// does not own.
func TestReconcileGlance_UnsetDeletesChildWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	// The DB-credential ExternalSecret was projected alongside the child.
	var es esov1.ExternalSecret
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: cp.GlanceNamespace(),
	}, &es)).To(Succeed())

	cp.Spec.Services.Glance = nil
	cp.Annotations = map[string]string{glanceDeletionAllowedAnnotation: "true"}

	_, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list glancev1alpha1.GlanceList
	g.Expect(r.Client.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "opt-in annotation must delete the owned child")

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: cp.GlanceNamespace(),
	}, &esov1.ExternalSecret{})).NotTo(Succeed(), "the DB-credential ExternalSecret must be swept too")
}

// TestReconcileGlance_UnsetPreservesForeignChild proves the deletion sweep is
// ownership-checked: a Glance child of the same name that the ControlPlane does
// NOT own (no owner reference, no ownership labels) survives an opt-in teardown.
func TestReconcileGlance_UnsetPreservesForeignChild(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance = nil
	cp.Annotations = map[string]string{glanceDeletionAllowedAnnotation: "true"}

	foreign := &glancev1alpha1.Glance{
		ObjectMeta: metav1.ObjectMeta{Name: glanceName(cp), Namespace: cp.GlanceNamespace()},
	}
	r := newGlanceTestReconciler(t, cp, foreign)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: glanceName(cp), Namespace: cp.GlanceNamespace(),
	}, &glancev1alpha1.Glance{})).To(Succeed(), "a Glance child we do not own must never be deleted")
}

func TestReconcileGlance_GatedOnKeystoneReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		Reason:             "WaitingForKeystone",
		Message:            "not ready",
	})
	r := newGlanceTestReconciler(t, cp)

	res, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(keystoneInfraGateRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForKeystone"))

	var list glancev1alpha1.GlanceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

func TestReconcileGlance_GatedOnServiceAccountNotDeclared(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	// Drop the auto-injected glance service account (a webhook-bypassed CR).
	cp.Spec.KORC.ServiceAccounts = nil
	r := newGlanceTestReconciler(t, cp)

	res, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a missing SA declaration is not a transient wait — no requeue")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ServiceAccountNotDeclared"))
	g.Expect(cond.Message).To(ContainSubstring("spec.korc.serviceAccounts"))

	var list glancev1alpha1.GlanceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

func TestReconcileGlance_GatedOnServiceAccountNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Status.ServiceAccounts = []c5c3v1alpha1.ServiceAccountStatus{{Name: "glance", Ready: false}}
	r := newGlanceTestReconciler(t, cp)

	res, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForServiceAccount"))

	var list glancev1alpha1.GlanceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the SA gate must block projection")
}

// --- projected child fields ---

func TestReconcileGlance_ImageTagFromRelease(t *testing.T) {
	for _, tt := range []struct {
		release string
		wantTag string
	}{
		{release: "2025.2", wantTag: "2025.2"},
		{release: "2026.1", wantTag: "2026.1"},
	} {
		t.Run(tt.release, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()
			cp.Spec.OpenStackRelease = tt.release
			r := newGlanceTestReconciler(t, cp)

			_, err := r.reconcileGlance(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())

			gl := getProjectedGlance(t, r.Client, cp)
			g.Expect(gl.Spec.Image.Repository).To(Equal("ghcr.io/c5c3/glance"))
			g.Expect(gl.Spec.Image.Tag).To(Equal(tt.wantTag))
			// The launch-mode / release-tracking field tracks the same release.
			g.Expect(gl.Spec.OpenStackRelease).To(Equal(tt.release))
		})
	}
}

func TestReconcileGlance_ImageOverrideWins(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/mirror/glance",
		Tag:        "custom",
	}
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Image.Repository).To(Equal("registry.example.com/mirror/glance"))
	g.Expect(gl.Spec.Image.Tag).To(Equal("custom"))
}

// TestReconcileGlance_DatabaseManagedProjection verifies the managed-mode DB
// wiring: the logical schema is always "glance", the secretRef points at the
// operator-owned DB-credential Secret, and credentialsMode is forced Static
// (there is no engine role for the glance schema).
func TestReconcileGlance_DatabaseManagedProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Database.ClusterRef).NotTo(BeNil())
	g.Expect(gl.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(gl.Spec.Database.Database).To(Equal("glance"),
		"the logical schema must be glance, not the shared block's keystone")
	g.Expect(gl.Spec.Database.SecretRef.Name).To(Equal(glanceDBCredentialSecretName(cp)))
	g.Expect(gl.Spec.Database.SecretRef.Key).To(Equal("password"))
	g.Expect(gl.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic),
		"a managed glance database is Static — no engine role can mint its credentials")

	// DeepCopy: the projected ClusterRef must not alias the ControlPlane spec.
	g.Expect(gl.Spec.Database.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Database.ClusterRef))
}

// TestReconcileGlance_DatabaseBrownfieldLeavesCredentialsModeUntouched covers the
// brownfield half: a database with no ClusterRef carries a user-supplied
// credential, so credentialsMode is untouched and no DB-credential ExternalSecret
// is projected.
func TestReconcileGlance_DatabaseBrownfieldLeavesCredentialsModeUntouched(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "brownfield-db"},
	}
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Database.ClusterRef).To(BeNil())
	g.Expect(gl.Spec.Database.Database).To(Equal("glance"),
		"the logical schema is always overridden to glance, even for a brownfield database")
	g.Expect(gl.Spec.Database.CredentialsMode).To(BeEmpty(),
		"a brownfield database must keep its credentialsMode untouched")
	g.Expect(gl.Spec.Database.SecretRef.Name).To(Equal("brownfield-db"),
		"a brownfield database keeps its user-supplied secretRef")

	// No DB-credential ExternalSecret is projected in brownfield mode.
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: cp.GlanceNamespace(),
	}, &esov1.ExternalSecret{})).NotTo(Succeed())
}

func TestReconcileGlance_CacheDeepCopiedFromInfrastructure(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(gl.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	// DeepCopy: the child's pointer must not alias the ControlPlane's spec.
	g.Expect(gl.Spec.Cache.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Cache.ClusterRef))
}

func TestReconcileGlance_KeystoneEndpointDerivation(t *testing.T) {
	// The Glance API validates tokens against spec.keystoneEndpoint server-side,
	// so the projection must always use the cluster-local convention URL; external
	// exposure (gateway hostname, explicit publicEndpoint) must not leak into it.
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.example.com",
	}
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com:8443/v3"
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.KeystoneEndpoint).To(Equal("http://cp-keystone.default.svc:5000/v3"),
		"the internal endpoint must be the cluster-local Service URL")
	g.Expect(gl.Spec.KeystonePublicEndpoint).To(Equal("https://keystone.example.com:8443/v3"),
		"the public endpoint carries the browser/client-facing URL")
}

func TestReconcileGlance_KeystonePublicEndpointEmptyWhenNotExposed(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane() // no gateway, no publicEndpoint on Keystone
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.KeystonePublicEndpoint).To(BeEmpty(),
		"an unexposed Keystone advertises no public endpoint; the child falls back to the internal one")
}

func TestReconcileGlance_RegionProjected(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Region = "RegionTwo"
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Region).To(Equal("RegionTwo"))
}

// TestReconcileGlance_ServiceUserFromServiceAccount verifies the Keystone service
// user is derived from the auto-injected glance service account entry.
func TestReconcileGlance_ServiceUserFromServiceAccount(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	sa := cp.Spec.KORC.ServiceAccounts[0]
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.ServiceUser.Username).To(Equal("glance"))
	g.Expect(gl.Spec.ServiceUser.ProjectName).To(Equal("service"))
	// Both domains resolve to the account's effective domain.
	g.Expect(gl.Spec.ServiceUser.UserDomainName).To(Equal(serviceAccountDomainName(cp, sa)))
	g.Expect(gl.Spec.ServiceUser.ProjectDomainName).To(Equal(serviceAccountDomainName(cp, sa)))
	g.Expect(gl.Spec.ServiceUser.UserDomainName).To(Equal(gl.Spec.ServiceUser.ProjectDomainName))
	// The password comes from the account's materialized consumer Secret.
	g.Expect(gl.Spec.ServiceUser.SecretRef.Name).To(Equal(serviceAccountCredentialsSecretName(cp, sa)))
	g.Expect(gl.Spec.ServiceUser.SecretRef.Name).To(Equal("cp-service-account-glance-credentials"))
	g.Expect(gl.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))
}

func TestReconcileGlance_ProjectsSecretStoreRef(t *testing.T) {
	g := NewGomegaWithT(t)

	// Explicit override wins.
	cp := glanceControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	r := newGlanceTestReconciler(t, cp)
	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.SecretStoreRef).NotTo(BeNil())
	g.Expect(gl.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))

	// A nil ControlPlane store ref defaults to the per-tenant store, not nil.
	cp2 := glanceControlPlane()
	cp2.Name = "cp2"
	r2 := newGlanceTestReconciler(t, cp2)
	_, err = r2.reconcileGlance(context.Background(), cp2)
	g.Expect(err).NotTo(HaveOccurred())
	gl2 := getProjectedGlance(t, r2.Client, cp2)
	g.Expect(gl2.Spec.SecretStoreRef).NotTo(BeNil(),
		"a nil ControlPlane store ref must project the per-tenant store, not nil")
	g.Expect(gl2.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(gl2.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))
}

func TestReconcileGlance_GatewayNilClears(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "glance.example.com",
	}
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Gateway).NotTo(BeNil())
	g.Expect(gl.Spec.Gateway.Hostname).To(Equal("glance.example.com"))

	// Clearing the gateway reverts the child rather than pinning the old value.
	cp.Spec.Services.Glance.Gateway = nil
	_, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	gl = getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Gateway).To(BeNil(), "clearing the gateway must tear the HTTPRoute down")
}

func TestReconcileGlance_ReplicasDefaultAndOverride(t *testing.T) {
	g := NewGomegaWithT(t)

	// Default.
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)
	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas))

	// Override wins, and clearing it reverts to the default (assigned
	// unconditionally, not left pinned).
	cp.Spec.Services.Glance.Replicas = ptr.To(int32(5))
	_, err = r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	gl = getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Deployment.Replicas).To(Equal(int32(5)))

	cp.Spec.Services.Glance.Replicas = nil
	_, err = r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	gl = getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas),
		"clearing the override must revert the child to the operator default")
}

// TestReconcileGlance_DoesNotSetAPIServer pins the child-side defaults contract:
// the projection must leave spec.apiServer unset so the release-conditional
// glance defaults stay authoritative.
func TestReconcileGlance_DoesNotSetAPIServer(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.APIServer).To(BeNil())
}

// TestReconcileGlance_DBCredentialExternalSecretShape verifies the projected
// DB-credential ExternalSecret reads the per-ControlPlane KV path through the
// resolved store and materializes the operator-owned Secret.
func TestReconcileGlance_DBCredentialExternalSecretShape(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var es esov1.ExternalSecret
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: cp.GlanceNamespace(),
	}, &es)).To(Succeed())

	g.Expect(es.Spec.Target.Name).To(Equal(glanceDBCredentialSecretName(cp)))
	g.Expect(es.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))
	g.Expect(es.Spec.Data).To(HaveLen(2))
	wantKey := glanceDBCredentialRemoteKeyFor(cp)
	g.Expect(wantKey).To(Equal("openstack/glance/default/cp/db"))
	for _, d := range es.Spec.Data {
		g.Expect(d.RemoteRef.Key).To(Equal(wantKey))
		g.Expect(d.RemoteRef.Property).To(Equal(d.SecretKey))
	}
}

// TestReconcileGlance_MirrorsChildReady exercises the readiness mirror: a fresh
// child is not ready (WaitingForGlance + requeue), a Ready child flips GlanceReady
// True.
func TestReconcileGlance_MirrorsChildReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	res, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForGlance"))

	gl := getProjectedGlance(t, r.Client, cp)
	conditions.SetCondition(&gl.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gl.Generation,
		Reason:             "AllReady",
		Message:            "ready",
	})
	g.Expect(r.Client.Status().Update(ctx, gl)).To(Succeed())

	res, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond = conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("GlanceReady"))
}

func TestReconcileGlance_SetsControllerOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(metav1.IsControlledBy(gl, cp)).To(BeTrue(),
		"the projected Glance must carry the ControlPlane controller owner reference")
}

// TestGlanceEndpointURL pins the in-cluster Glance API endpoint convention the
// later catalog package registers against: http://{name}.{ns}.svc:9292.
func TestGlanceEndpointURL(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	g.Expect(glanceEndpointURL(cp)).To(Equal("http://cp-glance.default.svc:9292"))
}
