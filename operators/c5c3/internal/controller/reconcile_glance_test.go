// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Glance sub-reconciler.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/forge/internal/common/conditions"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
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
	if err := orcv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding K-ORC scheme: %v", err)
	}
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets scheme: %v", err)
	}
	if err := esgenv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets generators scheme: %v", err)
	}
	return s
}

// glanceControlPlane builds a ControlPlane with services.glance set and a
// KeystoneReady=True condition — the one gate reconcileGlance reads off the
// ControlPlane itself. The other gate is the projected KeystoneService child,
// which newGlanceTestReconciler seeds Ready (see withReadyGlanceRegistration).
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

// readyGlanceRegistration builds the KeystoneService child the Glance projection
// gates on, converged: account provisioned, catalog registered, aggregate Ready.
// A child in a dedicated namespace carries the ownership labels, so the
// projection re-applies it instead of refusing to adopt a same-named foreign CR.
func readyGlanceRegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	ks := glanceRegistration(cp, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonKeystoneServiceAccountProvisioned,
		Message: "account provisioned",
	})
	conditions.SetCondition(&ks.Status.Conditions, metav1.Condition{
		Type:    conditionTypeKeystoneServiceCatalogReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonKeystoneServiceCatalogRegistered,
		Message: "catalog registered",
	})
	conditions.SetCondition(&ks.Status.Conditions, metav1.Condition{
		Type:    conditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: "All sub-conditions are ready",
	})
	return ks
}

// glanceRegistration builds the KeystoneService child at the projected
// name/namespace carrying the given conditions, for the tests that drive the gate
// and the readiness fold from a child that has not converged.
func glanceRegistration(cp *c5c3v1alpha1.ControlPlane, conds ...metav1.Condition) *c5c3v1alpha1.KeystoneService {
	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: glanceName(cp), Namespace: cp.GlanceNamespace()},
	}
	if ks.Namespace != cp.Namespace {
		stampControlPlaneChildLabels(ks, cp)
	}
	for _, cond := range conds {
		conditions.SetCondition(&ks.Status.Conditions, cond)
	}
	return ks
}

// readyGlanceDBCredES builds a Ready Glance DB-credential ExternalSecret at the
// derived name/namespace (Dynamic default shape), so WaitForExternalSecret reports
// Ready and the projection clears its dynamic readiness gate. Mirrors
// readyDBCredES on the Keystone side.
func readyGlanceDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := notReadyGlanceDBCredES(cp)
	es.Status = esov1.ExternalSecretStatus{
		Conditions: []esov1.ExternalSecretStatusCondition{
			{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
		},
	}
	return es
}

// materialisedGlanceDBCredSecret builds the Secret an ESO sync of the
// generator-backed ExternalSecret would materialise: an ENGINE-ISSUED username
// (the OpenBao mysql-database-plugin prefix) plus its password. The Dynamic gate
// checks the username, not just the ExternalSecret's Ready condition, so a Secret
// carrying a static seed's username reads as "not yet issued".
func materialisedGlanceDBCredSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      glanceDBCredentialSecretName(cp),
			Namespace: cp.GlanceNamespace(),
		},
		Data: map[string][]byte{
			"username": []byte(engineIssuedUsernamePrefix + "kubernetes-glance-abc123-1750000000"),
			"password": []byte("engine-issued-password"),
		},
	}
}

// withReadyGlanceDBCred seeds a Ready Glance DB-credential ExternalSecret AND the
// engine-issued Secret an ESO sync of it would materialise, for the ControlPlane
// in objs, unless an ExternalSecret was already seeded explicitly.
//
// The Dynamic-default projection gates the child on both — the ExternalSecret
// having synced and the Secret behind it carrying an engine-issued username — and
// a fake client never runs ESO, so without this every projection test would stall
// on the gate and assert against a child that was deliberately not projected.
// Seeding both here is the fake-client stand-in for "ESO has materialised the
// engine-issued credential"; the tests that exercise the gate itself seed their
// own ExternalSecret (and, where the gate under test is the username one, their
// own Secret), which is kept.
func withReadyGlanceDBCred(objs []client.Object) []client.Object {
	var cp *c5c3v1alpha1.ControlPlane
	for _, o := range objs {
		if c, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			cp = c
			break
		}
	}
	// Only the Dynamic path has a readiness gate; a Static ControlPlane projects a
	// KV-backed ExternalSecret of a different shape and must be left to build it.
	if cp == nil || !glanceDBCredentialsDynamicEnabled(cp) {
		return objs
	}
	name, ns := glanceDBCredentialSecretName(cp), cp.GlanceNamespace()
	for _, o := range objs {
		if _, ok := o.(*esov1.ExternalSecret); ok && o.GetName() == name && o.GetNamespace() == ns {
			return objs
		}
	}
	return append(objs, readyGlanceDBCredES(cp), materialisedGlanceDBCredSecret(cp))
}

// withReadyGlanceRegistration seeds the converged KeystoneService child the
// projection gates on, unless the test seeded one of its own — which is what the
// gate and readiness-fold tests do.
//
// A fake client runs no KeystoneService controller, so without this every
// projection test would hold at the registration gate and assert against a Glance
// that was deliberately not projected.
func withReadyGlanceRegistration(objs []client.Object) []client.Object {
	var cp *c5c3v1alpha1.ControlPlane
	for _, o := range objs {
		if c, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			cp = c
			break
		}
	}
	if cp == nil || cp.Spec.Services.Glance == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*c5c3v1alpha1.KeystoneService); ok {
			return objs
		}
	}
	return append(objs, readyGlanceRegistration(cp))
}

// withGlanceTenantStore seeds the per-tenant SecretStore in the image service's
// namespace when that service is PLACED, unless the test seeded one of its own.
// The credential mirror a placed service gets is gated on that store, so without
// it every placement test would hold at SecretStoreNotReady.
//
// The store lands on the local client because newGlanceTestReconciler wires no
// resolver, which resolves every namespace to the management cluster. The tests
// that exercise the two-cluster legs build their reconciler themselves.
func withGlanceTenantStore(objs []client.Object) []client.Object {
	var cp *c5c3v1alpha1.ControlPlane
	for _, o := range objs {
		if c, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			cp = c
			break
		}
	}
	if cp == nil || targetClusterRefForNamespace(cp, cp.GlanceNamespace()) == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*esov1.SecretStore); ok {
			return objs
		}
	}
	return append(objs, readyTenantSecretStore(esoTenantStoreName, cp.GlanceNamespace(), "", ""))
}

func newGlanceTestReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := glanceTestScheme(t)
	seeded := withGlanceTenantStore(withReadyGlanceRegistration(withReadyGlanceDBCred(objs)))
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &glancev1alpha1.Glance{},
			&c5c3v1alpha1.KeystoneService{})
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

// notReadyGlanceDBCredES builds the Glance DB-credential ExternalSecret with NO
// Ready condition, so WaitForExternalSecret reports not-Ready and the Dynamic
// readiness gate engages. Seeding it explicitly is what keeps
// withReadyGlanceDBCred from substituting a Ready one.
func notReadyGlanceDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := dbCredentialGeneratorExternalSecret(glanceDBCredentialTarget(cp))
	// Stamped as this ControlPlane's child so the cross-namespace projection path
	// re-applies it instead of refusing to adopt a same-named foreign object.
	stampControlPlaneChildLabels(es, cp)
	return es
}

// TestReconcileGlance_DynamicCredentialNotReady_DefersProjection is the gate that
// keeps the Dynamic default from failing OPEN. The engine role behind the
// generator is provisioned by a MANUAL onboarding step
// (setup-database-tenant.sh), while the operator rolls out on its own, so a
// ControlPlane can reach here with no role to mint against. Until the credential
// materialises no Glance child may be projected at all — projecting one would
// point Glance at a credential that never lands.
func TestReconcileGlance_DynamicCredentialNotReady_DefersProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp, notReadyGlanceDBCredES(cp))

	res, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0), "must requeue while the DB credential has not landed")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForGlanceDBCredential"))
	g.Expect(cond.Message).To(ContainSubstring(glanceDBDynamicCredsPathFor(cp)),
		"the condition must name the engine path an operator has to onboard")

	var list glancev1alpha1.GlanceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no Glance child may be projected before the credential lands")
}

// TestReconcileGlance_DynamicCredentialNotReady_LeavesExistingChildStatic is the
// migration half of the same gate: a Static->Dynamic flip must not move a RUNNING
// Glance onto credentialsMode Dynamic before the engine-issued credential exists.
// Flipping early points the child at whatever the materialised Secret happens to
// hold — on a migrated deployment, the retired static seed's stale username — and
// stops the glance-operator asserting the static User/Grant that was working.
func TestReconcileGlance_DynamicCredentialNotReady_LeavesExistingChildStatic(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	r := newGlanceTestReconciler(t, cp, notReadyGlanceDBCredES(cp))

	// Static deployment: the child is projected and runs on the static credential.
	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic))

	// Flip to Dynamic while the generator-backed ExternalSecret has not synced.
	cp.Spec.Services.Glance.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
	res, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0))

	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic),
			"the running child must stay Static until the engine-issued credential lands")
}

// staleStaticGlanceDBCredSecret builds the Secret a MIGRATED cluster is left with
// right after the Static->Dynamic flip: materialised by the last STATIC sync, so
// it still carries the retired bootstrap's username=glance seed. That name is a
// syntactically valid username, so a gate that only checks for a non-empty
// username would wave it through — but no MySQL user was ever created under it
// (the static login is the Glance CR name).
func staleStaticGlanceDBCredSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	secret := materialisedGlanceDBCredSecret(cp)
	secret.Data["username"] = []byte("glance")
	return secret
}

// TestReconcileGlance_DynamicCredentialStaleStaticUsername_LeavesExistingChildStatic
// is the regression guard for the failure the ExternalSecret-only gate let
// through. A Static->Dynamic flip create-or-updates the ExternalSecret IN PLACE,
// so on a migrated cluster it keeps reporting Ready from its last Static sync
// while the Secret behind it still holds the retired static seed. Flipping the
// child on that Ready alone stops the glance-operator asserting the static
// User/Grant Glance was serving on and points it at a login that never existed —
// an outage behind GlanceReady=True. The operator must hold the flip until the
// Secret carries an engine-issued username.
func TestReconcileGlance_DynamicCredentialStaleStaticUsername_LeavesExistingChildStatic(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	// A Ready ExternalSecret over a Secret still holding the static seed: exactly
	// what a cluster upgraded from the Static path presents to the reconciler.
	r := newGlanceTestReconciler(t, cp, readyGlanceDBCredES(cp), staleStaticGlanceDBCredSecret(cp))

	// Static deployment: the child is projected and runs on the static credential.
	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic))

	// Flip to Dynamic. The ExternalSecret is Ready, but only from the Static sync.
	cp.Spec.Services.Glance.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
	res, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0), "must requeue while the credential is not engine-issued")

	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic),
			"a Ready ExternalSecret over a stale static username must not flip the running child to Dynamic")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForGlanceDBCredential"))
	g.Expect(cond.Message).To(ContainSubstring(`"glance"`),
		"the condition must name the non-engine-issued username it found")
	g.Expect(cond.Message).To(ContainSubstring(glanceDBCredentialSecretName(cp)),
		"the condition must name the Secret an operator has to delete")
}

// TestReconcileGlance_DynamicCredentialSecretAbsent_DefersProjection covers the
// window the migration guide's `kubectl delete secret` step opens: the
// ExternalSecret is Ready but its target Secret is gone until ESO re-materialises
// it from the generator. An absent Secret is "not issued yet", not an error — no
// child may be projected against it.
func TestReconcileGlance_DynamicCredentialSecretAbsent_DefersProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	// A Ready ExternalSecret with no materialised Secret behind it.
	r := newGlanceTestReconciler(t, cp, readyGlanceDBCredES(cp))

	res, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "an absent Secret is an expected state, not a client failure")
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForGlanceDBCredential"))

	var list glancev1alpha1.GlanceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no Glance child may be projected before the credential is materialised")
}

// TestReconcileGlance_UnsetTearsDownDynamicGeneratorWithoutOptIn covers the
// preserve-by-default branch: dropping spec.services.glance keeps the child
// (an accidental block drop must not remove a running service) but must NOT keep
// the credential minter. A retained VaultDynamicSecret mints a fresh MySQL user
// with ALL PRIVILEGES every refresh interval, forever, for a service the operator
// was told it no longer manages — with no consumer, no revocation, and a
// GlanceReady=True condition that surfaces none of it.
func TestReconcileGlance_UnsetTearsDownDynamicGeneratorWithoutOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: cp.GlanceNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).To(Succeed(), "the generator was projected alongside the child")

	// No opt-in annotation: the child is preserved.
	cp.Spec.Services.Glance = nil
	_, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedGlance(t, r.Client, cp)).NotTo(BeNil(), "the child must still be preserved")

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: cp.GlanceNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(),
		"the credential minter must be torn down even though the child is preserved")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialServiceAccountName, Namespace: cp.GlanceNamespace(),
	}, &corev1.ServiceAccount{})).NotTo(Succeed(), "the generator's ServiceAccount must be torn down too")
	orphanCert := &unstructured.Unstructured{}
	orphanCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialClientCertName(cp), Namespace: cp.GlanceNamespace(),
	}, orphanCert)).NotTo(Succeed(), "the generator's mTLS client Certificate must be torn down too")
}

// TestReconcileGlance_UnsetDeletesChildWithOptIn verifies the opt-in deletion
// sweep removes the child AND every DB-credential object — the (generator-backed)
// ExternalSecret plus the Dynamic-mode VaultDynamicSecret, Certificate, and
// ServiceAccount — and, the ownership guard, never touches a hand-created Glance
// child the ControlPlane does not own.
func TestReconcileGlance_UnsetDeletesChildWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	// The Dynamic-default DB-credential objects were projected alongside the child.
	var es esov1.ExternalSecret
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: cp.GlanceNamespace(),
	}, &es)).To(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: cp.GlanceNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).To(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialServiceAccountName, Namespace: cp.GlanceNamespace(),
	}, &corev1.ServiceAccount{})).To(Succeed())
	glanceCert := &unstructured.Unstructured{}
	glanceCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialClientCertName(cp), Namespace: cp.GlanceNamespace(),
	}, glanceCert)).To(Succeed())

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
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: cp.GlanceNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(), "the VaultDynamicSecret generator must be swept too")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialServiceAccountName, Namespace: cp.GlanceNamespace(),
	}, &corev1.ServiceAccount{})).NotTo(Succeed(), "the generator's ServiceAccount must be swept too")
	sweptCert := &unstructured.Unstructured{}
	sweptCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: glanceDBCredentialClientCertName(cp), Namespace: cp.GlanceNamespace(),
	}, sweptCert)).NotTo(Succeed(), "the mTLS client Certificate must be swept too")
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

// TestReconcileGlance_GatedOnRegistrationAccountNotReady pins the registration
// gate: while the child's AccountReady is False no Glance is projected, the
// child's own reason and message are relayed, and a Glance projected by an
// earlier pass is left running on the credentials it already has.
func TestReconcileGlance_GatedOnRegistrationAccountNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	ks := glanceRegistration(cp, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionFalse,
		Reason:  reasonServiceAccountCollision,
		Message: `user "glance" already exists in Keystone`,
	})
	existing := &glancev1alpha1.Glance{
		ObjectMeta: metav1.ObjectMeta{Name: glanceName(cp), Namespace: cp.GlanceNamespace()},
		Spec:       glancev1alpha1.GlanceSpec{Region: "RegionPrevious"},
	}
	r := newGlanceTestReconciler(t, cp, ks, existing)

	res, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring(`user "glance" already exists in Keystone`))

	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.Region).To(Equal("RegionPrevious"),
		"the gate must write no Glance at all, leaving a previously projected one untouched")
}

// TestReconcileGlance_GatedOnRegistrationWithoutConditions covers the child that
// exists but has not been reconciled yet: the gate holds on a waiting message
// rather than reading a missing condition as ready.
func TestReconcileGlance_GatedOnRegistrationWithoutConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp, glanceRegistration(cp))

	res, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(conditionTypeKeystoneServiceAccountReady))

	var list glancev1alpha1.GlanceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the registration gate must block projection")
}

// TestReconcileGlance_RegistrationNotFoundAfterEnsureHolds covers the read-back
// that misses: a child the API server has not made readable yet is a wait, not an
// error.
func TestReconcileGlance_RegistrationNotFoundAfterEnsureHolds(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	s := glanceTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withReadyGlanceDBCred([]client.Object{cp})...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &glancev1alpha1.Glance{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneService); ok {
					return apierrors.NewNotFound(
						schema.GroupResource{Group: "c5c3.io", Resource: "keystoneservices"}, key.Name)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "a child that is not readable yet is not a failure")
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))

	var list glancev1alpha1.GlanceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileGlance_RegistrationReadFailureSurfaces covers the other half: a
// read that fails for any reason OTHER than absence is an error, wrapped with what
// it was reading.
func TestReconcileGlance_RegistrationReadFailureSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	s := glanceTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withReadyGlanceDBCred([]client.Object{cp})...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &glancev1alpha1.Glance{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneService); ok {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("reading the glance KeystoneService child:"))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
}

// TestReconcileGlance_NeverAdoptsForeignRegistration proves the registration
// write is refused rather than allowed to overwrite a same-named KeystoneService
// in a namespace the ControlPlane does not own: the refusal surfaces on
// GlanceReady and the foreign CR keeps its spec.
func TestReconcileGlance_NeverAdoptsForeignRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "images", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	foreign := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: glanceName(cp), Namespace: "images"},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "someone-else"},
		},
	}
	r := newGlanceTestReconciler(t, cp, foreign)

	_, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign registration must be refused")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
	g.Expect(cond.Message).To(ContainSubstring("refusing to adopt pre-existing"))

	var live c5c3v1alpha1.KeystoneService
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: glanceName(cp), Namespace: "images",
	}, &live)).To(Succeed())
	g.Expect(live.Spec.ControlPlaneRef.Name).To(Equal("someone-else"),
		"a foreign registration must never be overwritten")
	g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel))
}

// TestBuiltinRegistrationForeignFields_NamesAnUndeclaredEndpointInterface covers
// the listType=map arm of the guard. It is a unit test rather than a pass through
// reconcileGlance because the fake client carries no schema and replaces the
// endpoints array wholesale, where a real apiserver merges it by listMapKey and
// leaves an interface another field manager added standing.
func TestBuiltinRegistrationForeignFields_NamesAnUndeclaredEndpointInterface(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	projected := desiredGlanceRegistration(cp).Spec

	child := desiredGlanceRegistration(cp)
	child.Spec.Catalog.Endpoints = append(child.Spec.Catalog.Endpoints,
		c5c3v1alpha1.KeystoneServiceEndpointSpec{
			Interface: c5c3v1alpha1.ExternalEndpointTypeAdmin,
			URL:       "https://attacker.example.com",
		})

	g.Expect(builtinRegistrationForeignFields(&projected, child)).
		To(ConsistOf("spec.catalog.endpoints[interface=admin]"))

	// The interfaces the projection DOES declare are asserted in the apply body, so
	// force-ownership reclaims them and the guard must stay silent on them.
	untouched := desiredGlanceRegistration(cp)
	untouched.Spec.Catalog.Endpoints[0].URL = "https://attacker.example.com"
	g.Expect(builtinRegistrationForeignFields(&projected, untouched)).To(BeEmpty())
}

// TestReconcileGlance_ReclaimsARegistrationCarryingForeignFields covers the fields
// the apply cannot take back. adopt is a bool with omitempty and rotation a nil
// pointer, so the projection asserts neither and ForceOwnership has nothing to
// force; endpoints is a listType=map, where an apply reconciles only the keys it
// carries. An actor holding patch on keystoneservices could otherwise publish an
// admin endpoint in the catalog, or flip adopt to take over a pre-existing
// Keystone user — and the registration's own controller, which this ControlPlane
// halting does not touch, would act on it. Field ownership constrains apply
// requests only, so the projection resets them with an ordinary Update instead of
// waiting for somebody to read a condition — and the pass records what it reset on
// both the event and the service's condition.
//
// The endpoints arm is covered by
// TestReclaimBuiltinRegistrationFields_ResetsTheEndpointList instead: the fake
// client carries no schema and replaces the endpoints array wholesale, so a pass
// through reconcileGlance never sees the undeclared row a real apiserver leaves
// standing (the same reason
// TestBuiltinRegistrationForeignFields_NamesAnUndeclaredEndpointInterface is a
// unit test).
func TestReconcileGlance_ReclaimsARegistrationCarryingForeignFields(t *testing.T) {
	for name, tamper := range map[string]struct {
		apply  func(*c5c3v1alpha1.KeystoneService)
		field  string
		assert func(g *WithT, live *c5c3v1alpha1.KeystoneService)
	}{
		"catalog adopt": {
			apply: func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Catalog.Adopt = true },
			field: "spec.catalog.adopt",
			assert: func(g *WithT, live *c5c3v1alpha1.KeystoneService) {
				g.Expect(live.Spec.Catalog.Adopt).To(BeFalse())
			},
		},
		"account adopt": {
			apply: func(ks *c5c3v1alpha1.KeystoneService) { ks.Spec.Account.Adopt = true },
			field: "spec.account.adopt",
			assert: func(g *WithT, live *c5c3v1alpha1.KeystoneService) {
				g.Expect(live.Spec.Account.Adopt).To(BeFalse())
			},
		},
		"account rotation": {
			apply: func(ks *c5c3v1alpha1.KeystoneService) {
				ks.Spec.Account.Rotation = &c5c3v1alpha1.ServiceAccountRotationSpec{
					Mode: c5c3v1alpha1.ServiceAccountRotationModeManual,
				}
			},
			field: "spec.account.rotation",
			assert: func(g *WithT, live *c5c3v1alpha1.KeystoneService) {
				g.Expect(live.Spec.Account.Rotation).To(BeNil())
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()

			// The live child is the one this ControlPlane projects, with one foreign
			// field written on top of it by another field manager.
			tampered := readyGlanceRegistration(cp)
			tampered.Spec = desiredGlanceRegistration(cp).Spec
			tamper.apply(tampered)
			r := newGlanceTestReconciler(t, cp, tampered)
			rec := record.NewFakeRecorder(10)
			r.Recorder = rec

			res, err := r.reconcileGlance(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred(), "a tampered child is remediated, not a failed reconcile")
			g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

			events := strings.Join(drainEvents(rec), "\n")
			g.Expect(events).To(ContainSubstring("ServiceRegistrationFieldsReclaimed"))
			g.Expect(events).To(ContainSubstring(tamper.field))

			// The event ages out of etcd on the cluster's TTL. Without the condition a
			// tampering remediated overnight would leave the ControlPlane reporting
			// Ready=True with no durable trace of it anywhere in the API.
			cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationFieldsReclaimed))
			g.Expect(cond.Message).To(ContainSubstring(tamper.field))

			var live c5c3v1alpha1.KeystoneService
			g.Expect(r.Get(context.Background(), types.NamespacedName{
				Name: glanceName(cp), Namespace: cp.GlanceNamespace(),
			}, &live)).To(Succeed())
			tamper.assert(g, &live)
			g.Expect(builtinRegistrationForeignFields(&desiredGlanceRegistration(cp).Spec, &live)).To(BeEmpty())

			err = r.Get(context.Background(), types.NamespacedName{
				Name: glanceName(cp), Namespace: cp.GlanceNamespace(),
			}, &glancev1alpha1.Glance{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"the pass that reclaims stops there; the reclaimed child is picked up on the next one")
		})
	}
}

// TestReclaimBuiltinRegistrationFields_ResetsTheEndpointList covers the arm the
// pass-level test cannot reach: an endpoint row keyed on an interface the
// projection does not declare is PUBLISHED in the Keystone catalog by the
// registration's own controller, so admin-scoped clients resolving it send their
// token wherever it points. The reclaim replaces the list rather than merging it.
func TestReclaimBuiltinRegistrationFields_ResetsTheEndpointList(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := glanceControlPlane()

	tampered := desiredGlanceRegistration(cp)
	tampered.Spec.Catalog.Endpoints = append(tampered.Spec.Catalog.Endpoints,
		c5c3v1alpha1.KeystoneServiceEndpointSpec{
			Interface: c5c3v1alpha1.ExternalEndpointTypeAdmin,
			URL:       "https://attacker.example.com",
		})
	s := glanceTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(tampered).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	projected := desiredGlanceRegistration(cp).Spec
	g.Expect(r.reclaimBuiltinRegistrationFields(ctx, &projected, tampered)).To(Succeed())

	var live c5c3v1alpha1.KeystoneService
	g.Expect(c.Get(ctx, types.NamespacedName{
		Name: glanceName(cp), Namespace: cp.GlanceNamespace(),
	}, &live)).To(Succeed())
	g.Expect(live.Spec.Catalog.Endpoints).To(Equal(projected.Catalog.Endpoints))
	g.Expect(builtinRegistrationForeignFields(&projected, &live)).To(BeEmpty())
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

// TestReconcileGlance_ExtraConfigMerge proves the projected child's
// spec.extraConfig is the key-by-key merge of globalExtraConfig and the
// per-service block: the per-service value wins on an overlapping key, a
// global-only key in the same section survives, and a global-only section is
// carried over.
func TestReconcileGlance_ExtraConfigMerge(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{
		"database": {
			"connection_recycle_time": "280",
			"max_pool_size":           "5",
		},
		"DEFAULT": {"debug": "true"},
	}
	cp.Spec.Services.Glance.ExtraConfig = map[string]map[string]string{
		"database": {"connection_recycle_time": "600"}, // overrides global
	}
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.ExtraConfig).To(Equal(map[string]map[string]string{
		"database": {
			"connection_recycle_time": "600", // per-service wins
			"max_pool_size":           "5",   // global-only key in the same section
		},
		"DEFAULT": {"debug": "true"}, // global-only section
	}), "per-service extraConfig must win, global keys/sections merged in")
}

// TestReconcileGlance_ExtraConfigClearedProjectsNil proves the field is assigned
// unconditionally: clearing both extraConfig blocks reverts the child to an
// absent spec.extraConfig rather than leaving the previously-projected value
// pinned.
func TestReconcileGlance_ExtraConfigClearedProjectsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"DEFAULT": {"debug": "true"}}
	cp.Spec.Services.Glance.ExtraConfig = map[string]map[string]string{
		"database": {"connection_recycle_time": "600"},
	}
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.ExtraConfig).NotTo(BeEmpty())

	cp.Spec.GlobalExtraConfig = nil
	cp.Spec.Services.Glance.ExtraConfig = nil
	_, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.ExtraConfig).To(BeNil(),
		"clearing both extraConfig blocks must revert the child")
}

// TestReconcileGlance_DatabaseManagedProjection verifies the managed-mode DB
// wiring: the logical schema is always "glance", the secretRef points at the
// operator-owned DB-credential Secret, and credentialsMode is Dynamic by default
// (the managed-shared default now that glance has its own engine role).
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
	g.Expect(gl.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic),
		"a managed shared glance database defaults to Dynamic (engine-issued) credentials")

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

// TestReconcileGlance_ServiceUserFromRegistration verifies the Keystone service
// user names the account the registration child declares — not the inline
// spec.korc.serviceAccounts entry, whose project this fixture deliberately spells
// differently — and reads its password from the registration's consumer Secret.
func TestReconcileGlance_ServiceUserFromRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.ServiceUser.Username).To(Equal("glance"))
	g.Expect(gl.Spec.ServiceUser.ProjectName).To(Equal("service-glance"))
	// Both domains resolve to the ControlPlane's effective admin domain, which is
	// what the registration resolves its own unset domainName to.
	g.Expect(gl.Spec.ServiceUser.UserDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(gl.Spec.ServiceUser.ProjectDomainName).To(Equal(adminDomainName(cp)))
	// The password comes from the Secret the registration delivers.
	g.Expect(gl.Spec.ServiceUser.SecretRef.Name).To(Equal("cp-glance-credentials"))
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

// TestReconcileGlance_ImportFilteringProjected proves every list of the
// web-download URI filter reaches the child unchanged, and that the child does
// not alias the ControlPlane spec. (A nil source projecting to nil is pinned by
// TestReconcileGlance_ImportFilteringClearedProjectsNil.)
func TestReconcileGlance_ImportFilteringProjected(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.ImportFiltering = &glancev1alpha1.ImportFilteringSpec{
		AllowedSchemes:    []string{"http", "https"},
		DisallowedSchemes: nil,
		AllowedHosts:      []string{"mirror.example.com"},
		DisallowedHosts:   nil,
		AllowedPorts:      []int32{80, 443},
		DisallowedPorts:   nil,
	}
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.ImportFiltering).To(Equal(&glancev1alpha1.ImportFilteringSpec{
		AllowedSchemes: []string{"http", "https"},
		AllowedHosts:   []string{"mirror.example.com"},
		AllowedPorts:   []int32{80, 443},
	}))

	// DeepCopy: the child's pointer must not alias the ControlPlane's spec, or a
	// later mutation there would reach through into the projected object.
	g.Expect(gl.Spec.ImportFiltering).NotTo(BeIdenticalTo(cp.Spec.Services.Glance.ImportFiltering))
}

// TestReconcileGlance_ImportFilteringClearedProjectsNil proves the field is
// assigned unconditionally: clearing services.glance.importFiltering removes the
// block from the child so the Glance operator's restrictive defaults apply
// again, rather than leaving the previously-projected policy pinned.
func TestReconcileGlance_ImportFilteringClearedProjectsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.ImportFiltering = &glancev1alpha1.ImportFilteringSpec{
		AllowedHosts: []string{"mirror.example.com"},
	}
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.ImportFiltering).NotTo(BeNil())

	cp.Spec.Services.Glance.ImportFiltering = nil
	_, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.ImportFiltering).To(BeNil(),
		"clearing the ControlPlane filter must revert the child to the operator defaults")
}

// TestReconcileGlance_StagingProjected proves the scratch-space bound reaches
// the child unchanged, and that the child does not alias the ControlPlane spec.
// (A nil source projecting to nil is pinned by
// TestReconcileGlance_StagingClearedProjectsNil.)
func TestReconcileGlance_StagingProjected(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Staging = &glancev1alpha1.StagingSpec{
		SizeLimit: ptr.To(resource.MustParse("50Gi")),
	}
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Staging).NotTo(BeNil())
	g.Expect(gl.Spec.Staging.SizeLimit).NotTo(BeNil())
	g.Expect(gl.Spec.Staging.SizeLimit.Cmp(resource.MustParse("50Gi"))).To(Equal(0))

	// DeepCopy: the child's pointer must not alias the ControlPlane's spec, or a
	// later mutation there would reach through into the projected object.
	g.Expect(gl.Spec.Staging).NotTo(BeIdenticalTo(cp.Spec.Services.Glance.Staging))
}

// TestReconcileGlance_StagingClearedProjectsNil proves the field is assigned
// unconditionally: clearing services.glance.staging removes the block from the
// child so the Glance operator's default size limit applies again, rather than
// leaving the previously-projected bound pinned.
func TestReconcileGlance_StagingClearedProjectsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Staging = &glancev1alpha1.StagingSpec{
		SizeLimit: ptr.To(resource.MustParse("50Gi")),
	}
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.Staging).NotTo(BeNil())

	cp.Spec.Services.Glance.Staging = nil
	_, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.Staging).To(BeNil(),
		"clearing the ControlPlane bound must revert the child to the operator default")
}

// TestReconcileGlance_ImageCacheProjected proves the cache block reaches the
// child unchanged — both the size bound and the maintenance cadence — and that
// the child does not alias the ControlPlane spec. (A nil source projecting to
// nil is pinned by TestReconcileGlance_ImageCacheClearedProjectsNil.)
func TestReconcileGlance_ImageCacheProjected(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.ImageCache = &glancev1alpha1.ImageCacheSpec{
		SizeLimit:           ptr.To(resource.MustParse("256Mi")),
		MaintenanceInterval: &metav1.Duration{Duration: 7 * time.Minute},
	}
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.ImageCache).NotTo(BeNil())
	g.Expect(gl.Spec.ImageCache.SizeLimit).NotTo(BeNil())
	// Compare the rendered quantities, not the structs: DeepCopy drops the
	// Quantity's cached string form, so struct equality fails on equal values.
	g.Expect(gl.Spec.ImageCache.SizeLimit.String()).To(Equal("256Mi"))
	g.Expect(gl.Spec.ImageCache.MaintenanceInterval).NotTo(BeNil())
	g.Expect(gl.Spec.ImageCache.MaintenanceInterval.Duration).To(Equal(7 * time.Minute))

	// DeepCopy: the child's pointer must not alias the ControlPlane's spec, or a
	// later mutation there would reach through into the projected object.
	g.Expect(gl.Spec.ImageCache).NotTo(BeIdenticalTo(cp.Spec.Services.Glance.ImageCache))
}

// TestReconcileGlance_ImageCacheClearedProjectsNil proves the field is assigned
// unconditionally: clearing services.glance.imageCache removes the block from
// the child, which disables the cache on the next rollout, rather than leaving
// the previously-projected budget pinned and the cache silently on.
func TestReconcileGlance_ImageCacheClearedProjectsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.ImageCache = &glancev1alpha1.ImageCacheSpec{
		SizeLimit: ptr.To(resource.MustParse("256Mi")),
	}
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.ImageCache).NotTo(BeNil())

	cp.Spec.Services.Glance.ImageCache = nil
	_, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.ImageCache).To(BeNil(),
		"clearing the ControlPlane cache must disable it on the child again")
}

// TestReconcileGlance_ImportPluginsProjected proves every sub-block of the
// plugin selection reaches the child unchanged, and that the child does not alias
// the ControlPlane spec. (A nil source projecting to nil is pinned by
// TestReconcileGlance_ImportPluginsClearedProjectsNil.)
func TestReconcileGlance_ImportPluginsProjected(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.ImportPlugins = &glancev1alpha1.ImportPluginsSpec{
		Decompression: &glancev1alpha1.ImportDecompressionSpec{},
		Conversion:    &glancev1alpha1.ImportConversionSpec{OutputFormat: "qcow2"},
		InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
			Properties:      map[string]string{"hw_disk_bus": "scsi"},
			IgnoreUserRoles: []string{"admin", "reader"},
		},
	}
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.ImportPlugins).To(Equal(&glancev1alpha1.ImportPluginsSpec{
		Decompression: &glancev1alpha1.ImportDecompressionSpec{},
		Conversion:    &glancev1alpha1.ImportConversionSpec{OutputFormat: "qcow2"},
		InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
			Properties:      map[string]string{"hw_disk_bus": "scsi"},
			IgnoreUserRoles: []string{"admin", "reader"},
		},
	}))

	// DeepCopy: the child's pointer must not alias the ControlPlane's spec, or a
	// later mutation there would reach through into the projected object.
	g.Expect(gl.Spec.ImportPlugins).NotTo(BeIdenticalTo(cp.Spec.Services.Glance.ImportPlugins))
}

// TestReconcileGlance_ImportPluginsClearedProjectsNil proves the field is
// assigned unconditionally: clearing services.glance.importPlugins removes the
// block from the child, so the Glance operator's defaults apply again and the
// next rollout runs no plugin, rather than leaving the previously-projected
// selection pinned.
func TestReconcileGlance_ImportPluginsClearedProjectsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.ImportPlugins = &glancev1alpha1.ImportPluginsSpec{
		Decompression: &glancev1alpha1.ImportDecompressionSpec{},
	}
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.ImportPlugins).NotTo(BeNil())

	cp.Spec.Services.Glance.ImportPlugins = nil
	_, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.ImportPlugins).To(BeNil(),
		"clearing the ControlPlane selection must run no import plugin on the child again")
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

// TestReconcileGlance_DBCredentialExternalSecretShape verifies the Static opt-out
// projects a KV-backed DB-credential ExternalSecret that reads the
// per-ControlPlane KV path through the resolved store and materializes the
// operator-owned Secret. The opt-out is set explicitly now that the managed-shared
// default is Dynamic.
func TestReconcileGlance_DBCredentialExternalSecretShape(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var es esov1.ExternalSecret
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: cp.GlanceNamespace(),
	}, &es)).To(Succeed())

	g.Expect(es.Spec.Target.Name).To(Equal(glanceDBCredentialSecretName(cp)))
	g.Expect(es.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))
	g.Expect(es.Spec.DataFrom).To(BeEmpty(), "the Static ExternalSecret must not use a generator")
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

// --- the projected KeystoneService registration ---

func getProjectedRegistration(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	t.Helper()
	ks := &c5c3v1alpha1.KeystoneService{}
	key := types.NamespacedName{Name: glanceName(cp), Namespace: cp.GlanceNamespace()}
	if err := c.Get(context.Background(), key, ks); err != nil {
		t.Fatalf("getting projected KeystoneService %s: %v", key, err)
	}
	return ks
}

// TestReconcileGlance_ProjectsTheRegistration pins the registration's content:
// the image catalog entry with both endpoint rows, the service account in its own
// per-service project, and the explicit controlPlaneRef a child in a dedicated
// namespace needs to resolve the ControlPlane at all.
func TestReconcileGlance_ProjectsTheRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedRegistration(t, r.Client, cp)
	g.Expect(ks.Name).To(Equal("cp-glance"))
	g.Expect(ks.Namespace).To(Equal("default"))
	g.Expect(ks.Spec.ControlPlaneRef.Name).To(Equal("cp"))
	g.Expect(ks.Spec.ControlPlaneRef.Namespace).To(Equal("default"),
		"the namespace is explicit so a child in a dedicated namespace resolves the right ControlPlane")

	g.Expect(ks.Spec.Catalog).NotTo(BeNil())
	g.Expect(ks.Spec.Catalog.ServiceType).To(Equal("image"))
	g.Expect(ks.Spec.Catalog.ServiceName).To(Equal("glance"))
	g.Expect(ks.Spec.Catalog.Adopt).To(BeFalse(), "a colliding catalog row must fail loud, never be adopted")
	g.Expect(ks.Spec.Catalog.Endpoints).To(HaveLen(2))
	g.Expect(ks.Spec.Catalog.Endpoints[0].Interface).To(Equal(c5c3v1alpha1.ExternalEndpointTypeInternal))
	g.Expect(ks.Spec.Catalog.Endpoints[0].URL).To(Equal(glanceEndpointURL(cp)))
	g.Expect(ks.Spec.Catalog.Endpoints[1].Interface).To(Equal(c5c3v1alpha1.ExternalEndpointTypePublic))
	g.Expect(ks.Spec.Catalog.Endpoints[1].URL).To(Equal(glanceCatalogURL(cp)))

	g.Expect(ks.Spec.Account).NotTo(BeNil())
	g.Expect(ks.Spec.Account.UserName).To(Equal("glance"))
	g.Expect(ks.Spec.Account.DomainName).To(BeEmpty(),
		"an unset domain lets the registration resolve the ControlPlane's admin domain")
	g.Expect(ks.Spec.Account.Adopt).To(BeFalse(), "a colliding user must fail loud, never be taken over")
	g.Expect(ks.Spec.Account.Project.Name).To(Equal("service-glance"))
	g.Expect(ks.Spec.Account.Project.Create).To(BeTrue())
	g.Expect(ks.Spec.Account.Roles).To(Equal([]string{"service"}))

	g.Expect(metav1.IsControlledBy(ks, cp)).To(BeTrue(),
		"a co-located registration carries the ControlPlane controller owner reference")
}

// TestReconcileGlance_PlacedRegistrationEndpointsFollowThePlacement covers the
// internal row of a placed service: the in-cluster Service URL resolves nowhere
// outside its cluster, so the placed entry advertises the public URL on both
// interfaces.
func TestReconcileGlance_PlacedRegistrationEndpointsFollowThePlacement(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedGlanceControlPlane("remote-a")
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedRegistration(t, r.Client, cp)
	g.Expect(ks.Spec.Catalog.Endpoints[0].URL).To(Equal("https://glance.example.com"))
	g.Expect(ks.Spec.Catalog.Endpoints[1].URL).To(Equal("https://glance.example.com"))
}

// TestReconcileGlance_CrossNamespaceRegistrationIsLabelledNotOwned verifies the
// ownership substitute for a registration in a namespace of its own: the two
// ownership labels and no owner reference.
func TestReconcileGlance_CrossNamespaceRegistrationIsLabelledNotOwned(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "images", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedRegistration(t, r.Client, cp)
	g.Expect(ks.Namespace).To(Equal("images"))
	g.Expect(ks.OwnerReferences).To(BeEmpty(), "a cross-namespace child cannot carry an owner reference")
	g.Expect(ks.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(ks.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
	g.Expect(ks.Spec.ControlPlaneRef.Namespace).To(Equal("default"))
}

// TestReconcileGlance_ReadyFoldsInTheRegistration proves GlanceReady is the
// conjunction of both children: a Ready Glance whose registration collided on the
// catalog row keeps GlanceReady False, naming the failing child condition.
func TestReconcileGlance_ReadyFoldsInTheRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	ks := glanceRegistration(cp,
		metav1.Condition{
			Type:    conditionTypeKeystoneServiceAccountReady,
			Status:  metav1.ConditionTrue,
			Reason:  reasonKeystoneServiceAccountProvisioned,
			Message: "account provisioned",
		},
		metav1.Condition{
			Type:    conditionTypeKeystoneServiceCatalogReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonKeystoneServiceCatalogCollision,
			Message: `a service row of type "image" named "glance" already exists`,
		},
		metav1.Condition{
			Type:    conditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  "NotAllReady",
			Message: "One or more sub-conditions are not ready",
		},
	)
	r := newGlanceTestReconciler(t, cp, ks)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// The Glance child itself reaches Ready.
	gl := getProjectedGlance(t, r.Client, cp)
	conditions.SetCondition(&gl.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gl.Generation,
		Reason:             "AllReady",
		Message:            "ready",
	})
	g.Expect(r.Client.Status().Update(ctx, gl)).To(Succeed())

	res, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
		"a Glance nothing can discover through the catalog is not ready")
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogCollision),
		"the failing sub-condition's reason is relayed, not the aggregate's")
	g.Expect(cond.Message).To(ContainSubstring(conditionTypeKeystoneServiceCatalogReady))
	g.Expect(cond.Message).To(ContainSubstring("cp-glance"))
}

// TestReconcileGlance_UnsetDeletesRegistrationWithOptIn verifies the opt-in
// teardown removes the registration too — which is what unregisters Glance from
// the catalog and the identity plane.
func TestReconcileGlance_UnsetDeletesRegistrationWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	getProjectedRegistration(t, r.Client, cp)

	cp.Spec.Services.Glance = nil
	cp.Annotations = map[string]string{glanceDeletionAllowedAnnotation: "true"}
	_, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the opt-in annotation must delete the owned registration")
}

// TestReconcileGlance_UnsetPreservesForeignRegistration is the ownership guard on
// that sweep: a same-named KeystoneService the ControlPlane does not own survives.
func TestReconcileGlance_UnsetPreservesForeignRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance = nil
	cp.Annotations = map[string]string{glanceDeletionAllowedAnnotation: "true"}

	foreign := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: glanceName(cp), Namespace: cp.GlanceNamespace()},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "someone-else"},
		},
	}
	r := newGlanceTestReconciler(t, cp, foreign)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: glanceName(cp), Namespace: cp.GlanceNamespace(),
	}, &c5c3v1alpha1.KeystoneService{})).To(Succeed(),
		"a KeystoneService we do not own must never be deleted")
}

// TestReconcileGlance_UnsetPreservesRegistrationByDefault pins the preserve
// default: without the opt-in annotation a previously projected registration
// stays, so an accidental block drop never unregisters a running service.
func TestReconcileGlance_UnsetPreservesRegistrationByDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cp.Spec.Services.Glance = nil
	_, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	getProjectedRegistration(t, r.Client, cp)
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("GlanceNotManaged"))
}

// TestReconcileGlance_NilBlockProjectsNoRegistration covers the staged-adoption
// path: a ControlPlane that manages no image service registers none either.
func TestReconcileGlance_NilBlockProjectsNoRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance = nil
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestGlanceEndpointURL pins the in-cluster Glance API endpoint convention the
// later catalog package registers against: http://{name}.{ns}.svc:9292.
func TestGlanceEndpointURL(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	g.Expect(glanceEndpointURL(cp)).To(Equal("http://cp-glance.default.svc:9292"))
}

// --- GlanceBackend projection and prune (Commit B) ---

func getProjectedGlanceBackend(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane, entryName string) *glancev1alpha1.GlanceBackend {
	t.Helper()
	b := &glancev1alpha1.GlanceBackend{}
	key := types.NamespacedName{Name: glanceBackendName(cp, entryName), Namespace: cp.GlanceNamespace()}
	if err := c.Get(context.Background(), key, b); err != nil {
		t.Fatalf("getting projected GlanceBackend %s: %v", key, err)
	}
	return b
}

// TestReconcileGlance_BackendFieldMapping verifies the CP-side backend entry is
// projected onto the GlanceBackend child field for field, with the CP-side
// endpoint mapped to the child's spec.s3.host and bucketURLFormat left unset so
// the child's own default applies.
func TestReconcileGlance_BackendFieldMapping(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane() // one backend "primary", default, no region/urlFormat
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	b := getProjectedGlanceBackend(t, r.Client, cp, "primary")
	g.Expect(b.Name).To(Equal("cp-glance-primary"))
	g.Expect(b.Spec.GlanceRef.Name).To(Equal("cp-glance"),
		"the backend must attach to this ControlPlane's Glance child")
	g.Expect(b.Spec.Type).To(Equal(glancev1alpha1.GlanceBackendTypeS3))
	g.Expect(b.Spec.S3).NotTo(BeNil())
	g.Expect(b.Spec.S3.Host).To(Equal("https://s3.example.com"),
		"the CP-side S3 endpoint maps to the child's spec.s3.host")
	g.Expect(b.Spec.S3.Bucket).To(Equal("images"))
	g.Expect(b.Spec.S3.CredentialsSecretRef.Name).To(Equal("glance-s3-creds"))
	g.Expect(b.Spec.IsDefault).To(BeTrue())
	g.Expect(b.Spec.S3.Region).To(BeEmpty())
	g.Expect(b.Spec.S3.BucketURLFormat).To(BeEmpty(),
		"an unset bucketURLFormat must be left unset so the GlanceBackend CRD default (path) applies")
	// The projected backend is owned so it is torn down with the ControlPlane.
	g.Expect(metav1.IsControlledBy(b, cp)).To(BeTrue())
}

// TestReconcileGlance_BackendOptionalFieldsProjectedWhenSet is the other half of
// the mapping: region and bucketURLFormat are carried through when the entry sets
// them.
func TestReconcileGlance_BackendOptionalFieldsProjectedWhenSet(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Backends[0].S3.Region = "us-east-1"
	cp.Spec.Services.Glance.Backends[0].S3.BucketURLFormat = "virtual"
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	b := getProjectedGlanceBackend(t, r.Client, cp, "primary")
	g.Expect(b.Spec.S3.Region).To(Equal("us-east-1"))
	g.Expect(b.Spec.S3.BucketURLFormat).To(Equal("virtual"))
}

// TestReconcileGlance_TwoBackendsTwoChildren verifies each declared entry
// projects its own GlanceBackend child.
func TestReconcileGlance_TwoBackendsTwoChildren(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Backends = append(cp.Spec.Services.Glance.Backends,
		c5c3v1alpha1.GlanceBackendEntry{
			Name: "secondary",
			Type: "S3",
			S3: &c5c3v1alpha1.GlanceBackendS3Spec{
				Endpoint:             "https://s3-2.example.com",
				Bucket:               "images-2",
				CredentialsSecretRef: c5c3v1alpha1.SecretNameRef{Name: "glance-s3-creds-2"},
			},
		})
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list glancev1alpha1.GlanceBackendList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	g.Expect(names).To(ConsistOf("cp-glance-primary", "cp-glance-secondary"))
}

// TestReconcileGlance_PruneRemovedBackend verifies removing an entry from the
// spec prunes exactly its projected child and leaves the others.
func TestReconcileGlance_PruneRemovedBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Backends = append(cp.Spec.Services.Glance.Backends,
		c5c3v1alpha1.GlanceBackendEntry{
			Name: "secondary",
			Type: "S3",
			S3: &c5c3v1alpha1.GlanceBackendS3Spec{
				Endpoint:             "https://s3-2.example.com",
				Bucket:               "images-2",
				CredentialsSecretRef: c5c3v1alpha1.SecretNameRef{Name: "glance-s3-creds-2"},
			},
		})
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	var before glancev1alpha1.GlanceBackendList
	g.Expect(r.Client.List(ctx, &before)).To(Succeed())
	g.Expect(before.Items).To(HaveLen(2))

	// Drop "secondary" from the spec.
	cp.Spec.Services.Glance.Backends = cp.Spec.Services.Glance.Backends[:1]
	_, err = r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var after glancev1alpha1.GlanceBackendList
	g.Expect(r.Client.List(ctx, &after)).To(Succeed())
	g.Expect(after.Items).To(HaveLen(1))
	g.Expect(after.Items[0].Name).To(Equal("cp-glance-primary"),
		"only the removed entry's child is pruned")
}

// TestReconcileGlance_NeverPrunesForeignBackend proves the prune is
// ownership-checked: a hand-created GlanceBackend that shares the projected name
// prefix but is NOT owned by the ControlPlane survives the sweep.
func TestReconcileGlance_NeverPrunesForeignBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()

	// A hand-created backend attached to the same Glance, name-prefix colliding,
	// but carrying neither our owner reference nor our labels.
	foreign := &glancev1alpha1.GlanceBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-glance-handmade", Namespace: cp.GlanceNamespace()},
		Spec: glancev1alpha1.GlanceBackendSpec{
			GlanceRef: glancev1alpha1.GlanceRefSpec{Name: "cp-glance"},
			Type:      glancev1alpha1.GlanceBackendTypeS3,
			S3: &glancev1alpha1.S3BackendSpec{
				Host:                 "https://byo.example.com",
				Bucket:               "byo",
				CredentialsSecretRef: glancev1alpha1.SecretNameRefSpec{Name: "byo-creds"},
			},
		},
	}
	r := newGlanceTestReconciler(t, cp, foreign)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: "cp-glance-handmade", Namespace: cp.GlanceNamespace(),
	}, &glancev1alpha1.GlanceBackend{})).To(Succeed(),
		"a hand-created GlanceBackend we do not own must never be pruned")
}

// TestReconcileGlance_NeverAdoptsForeignBackend proves the ensure refuses foreign
// adoption in a namespace the ControlPlane does not own: a pre-existing,
// unowned GlanceBackend at the projected name is neither overwritten nor adopted,
// and the projection surfaces the refusal.
func TestReconcileGlance_NeverAdoptsForeignBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "images", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}

	// A foreign object at the projected name in the (unowned) service namespace.
	foreign := &glancev1alpha1.GlanceBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-glance-primary", Namespace: "images"},
		Spec: glancev1alpha1.GlanceBackendSpec{
			GlanceRef: glancev1alpha1.GlanceRefSpec{Name: "someone-else"},
			Type:      glancev1alpha1.GlanceBackendTypeS3,
			S3: &glancev1alpha1.S3BackendSpec{
				Host:                 "https://foreign.example.com",
				Bucket:               "foreign",
				CredentialsSecretRef: glancev1alpha1.SecretNameRefSpec{Name: "foreign-creds"},
			},
		},
	}
	r := newGlanceTestReconciler(t, cp, foreign)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).To(HaveOccurred(), "adopting a foreign backend must be refused")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("GlanceBackendError"))

	// The foreign object's spec must be untouched.
	var live glancev1alpha1.GlanceBackend
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: "cp-glance-primary", Namespace: "images",
	}, &live)).To(Succeed())
	g.Expect(live.Spec.GlanceRef.Name).To(Equal("someone-else"),
		"a foreign backend must never be overwritten")
	g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel),
		"ownership must never be claimed over a backend we did not create")
}

// TestReconcileGlance_CrossNamespaceChildrenAreLabelledNotOwned verifies the
// ownership substitute for a Glance placed in a namespace of its own: the Glance
// child and its GlanceBackend children carry the ControlPlane's ownership labels
// and NO owner reference (Kubernetes forbids a cross-namespace one).
func TestReconcileGlance_CrossNamespaceChildrenAreLabelledNotOwned(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "images", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Namespace).To(Equal("images"))
	g.Expect(gl.OwnerReferences).To(BeEmpty(), "a cross-namespace child cannot carry an owner reference")
	g.Expect(gl.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(gl.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))

	b := getProjectedGlanceBackend(t, r.Client, cp, "primary")
	g.Expect(b.Namespace).To(Equal("images"))
	g.Expect(b.OwnerReferences).To(BeEmpty(), "a cross-namespace backend cannot carry an owner reference")
	g.Expect(b.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(b.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
	// It still attaches to the Glance child by name across the namespace boundary.
	g.Expect(b.Spec.GlanceRef.Name).To(Equal("cp-glance"))
}

// --- per-service target clusters ---

// placedGlanceControlPlane places the image service in a namespace of its own on
// a target cluster. Its database is brownfield, so the DB-credential leg — whose
// own placement is covered in reconcile_dbcredentials_test.go — projects nothing
// and the pass reaches the child projection over the local client alone.
func placedGlanceControlPlane(targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp := glanceControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
	}
	cp.Spec.Services.Glance.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      "images",
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Glance.PublicEndpoint = "https://glance.example.com"
	cp.Spec.Services.Glance.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: targetCluster}
	return cp
}

// TestReconcileGlance_ProjectsTheTargetClusterRef verifies the placement reaches
// the child verbatim — the glance-operator owns everything on the target, so the
// ref is the whole hand-over — and that an unplaced service projects no ref.
func TestReconcileGlance_ProjectsTheTargetClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedGlanceControlPlane("remote-a")
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedGlance(t, r.Client, cp).Spec.TargetClusterRef).
		To(Equal(&commonv1.TargetClusterRefSpec{Name: "remote-a"}))

	unplaced := glanceControlPlane()
	r2 := newGlanceTestReconciler(t, unplaced)
	_, err = r2.reconcileGlance(context.Background(), unplaced)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedGlance(t, r2.Client, unplaced).Spec.TargetClusterRef).To(BeNil(),
		"a service that names no cluster must project no ref at all")
}

// TestGlanceKeystoneEndpoint_FollowsThePlacement pins the endpoint policy: Glance
// validates tokens against Keystone itself, so it gets the in-cluster Service DNS
// name exactly while the two services share a cluster, and the public URL as soon
// as they do not — that name resolves nowhere else.
func TestGlanceKeystoneEndpoint_FollowsThePlacement(t *testing.T) {
	const (
		inCluster = "http://cp-keystone.identity.svc:5000/v3"
		public    = "https://keystone.example.com/v3"
	)
	for _, tc := range []struct {
		name             string
		glance, keystone *commonv1.TargetClusterRefSpec
		want             string
	}{
		{name: "both co-located", want: inCluster},
		{
			name:     "both on the same cluster",
			glance:   &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     inCluster,
		},
		{
			name:   "Glance placed, Keystone at home",
			glance: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:   public,
		},
		{
			name:     "Keystone placed, Glance at home",
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     public,
		},
		{
			name:     "different clusters",
			glance:   &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-b"},
			want:     public,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()
			cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"}
			cp.Spec.Services.Keystone.PublicEndpoint = public
			cp.Spec.Services.Keystone.TargetClusterRef = tc.keystone
			cp.Spec.Services.Glance.TargetClusterRef = tc.glance

			g.Expect(glanceKeystoneEndpoint(cp)).To(Equal(tc.want))
		})
	}
}

// --- the credential mirror of a placed service ---

// newPlacedGlanceReconciler wires a ControlPlane whose image service is placed on
// a target cluster: the CR and its registration live on the management cluster,
// the objects in onTarget on the other one.
func newPlacedGlanceReconciler(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, resolver *childrenResolver, onTarget ...client.Object,
) *ControlPlaneReconciler {
	t.Helper()
	s := glanceTestScheme(t)
	resolver.children = fake.NewClientBuilder().WithScheme(s).WithObjects(onTarget...).Build()
	local := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withReadyGlanceRegistration([]client.Object{cp})...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &glancev1alpha1.Glance{},
			&c5c3v1alpha1.KeystoneService{}).
		Build()
	return &ControlPlaneReconciler{Client: local, Scheme: s, Resolver: resolver}
}

// TestReconcileGlance_MirrorsRegistrationCredentialsToTheTarget covers the reason
// the mirror exists: the registration delivers its consumer Secret at home, and a
// Glance running on another cluster reads it there — from an ExternalSecret of the
// same name, over the same OpenBao path.
func TestReconcileGlance_MirrorsRegistrationCredentialsToTheTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedGlanceControlPlane("remote-a")
	resolver := &childrenResolver{}
	r := newPlacedGlanceReconciler(t, cp, resolver,
		readyTenantSecretStore(esoTenantStoreName, "images", "", ""))

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var mirror esov1.ExternalSecret
	g.Expect(resolver.children.Get(context.Background(), types.NamespacedName{
		Name: "cp-glance-credentials", Namespace: "images",
	}, &mirror)).To(Succeed())
	g.Expect(mirror.Spec.SecretStoreRef.Name).To(Equal(esoTenantStoreName))
	g.Expect(mirror.Spec.SecretStoreRef.Kind).To(Equal(string(commonv1.SecretStoreKindNamespaced)))
	g.Expect(mirror.Spec.Target.Name).To(Equal("cp-glance-credentials"))
	for _, d := range mirror.Spec.Data {
		g.Expect(d.RemoteRef.Key).To(Equal("openstack/keystone/images/cp-glance/service-accounts/credentials"))
	}
	// No owner reference crosses a cluster boundary, so the labels are the whole
	// of the mirror's identity — and what the teardown sweep selects on.
	g.Expect(mirror.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(mirror.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
	g.Expect(mirror.OwnerReferences).To(BeEmpty())
}

// TestReconcileGlance_NoMirrorForACoLocatedService is the other half: the
// registration's own delivery already lands in a co-located service's namespace,
// so no second ExternalSecret is written for it.
func TestReconcileGlance_NoMirrorForACoLocatedService(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)

	_, err := r.reconcileGlance(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: "cp-glance-credentials", Namespace: "default",
	}, &esov1.ExternalSecret{})).NotTo(Succeed(),
		"a co-located service must get no mirror at all")
}

// TestReconcileGlance_MirrorHoldsOnAnUnresolvableCluster covers the cluster that
// does not resolve: the resolver's own text reaches the condition, and nothing is
// projected.
func TestReconcileGlance_MirrorHoldsOnAnUnresolvableCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedGlanceControlPlane("remote-a")
	resolver := &childrenResolver{err: errors.New("cluster not found")}
	r := newPlacedGlanceReconciler(t, cp, resolver)

	res, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	var list glancev1alpha1.GlanceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileGlance_MirrorHoldsOnANotReadyTargetStore covers the store gate on
// the target cluster: an ExternalSecret written against a store that is not ready
// never syncs, so the projection waits and names the store, the namespace, and the
// cluster it is missing on.
func TestReconcileGlance_MirrorHoldsOnANotReadyTargetStore(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedGlanceControlPlane("remote-a")
	resolver := &childrenResolver{}
	// The store exists at home but not on the target, which is the cluster the
	// mirror is materialized on.
	r := newPlacedGlanceReconciler(t, cp, resolver)

	res, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountStoreNotReady))
	g.Expect(cond.Message).To(ContainSubstring(esoTenantStoreName))
	g.Expect(cond.Message).To(ContainSubstring(`namespace "images"`))
	g.Expect(cond.Message).To(ContainSubstring("target cluster"))

	g.Expect(resolver.children.Get(context.Background(), types.NamespacedName{
		Name: "cp-glance-credentials", Namespace: "images",
	}, &esov1.ExternalSecret{})).NotTo(Succeed(), "nothing may be written against a store that is not ready")
}

// TestReconcileGlance_MirrorStoreLookupFailurePropagates covers the store read
// that fails outright, as opposed to reporting not-ready: it is wrapped with what
// was being checked and returned, so the reconcile retries with backoff.
func TestReconcileGlance_MirrorStoreLookupFailurePropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedGlanceControlPlane("remote-a")
	s := glanceTestScheme(t)
	target := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*esov1.SecretStore); ok {
					return apierrors.NewInternalError(errors.New("the target apiserver is unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	local := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withReadyGlanceRegistration([]client.Object{cp})...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &glancev1alpha1.Glance{},
			&c5c3v1alpha1.KeystoneService{}).
		Build()
	r := &ControlPlaneReconciler{Client: local, Scheme: s, Resolver: &childrenResolver{children: target}}

	_, err := r.reconcileGlance(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`in namespace "images"`))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeGlanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
}
