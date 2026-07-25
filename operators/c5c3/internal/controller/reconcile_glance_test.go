// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Glance sub-reconciler.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func newGlanceTestReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := glanceTestScheme(t)
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(withReadyGlanceDBCred(objs)...).
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
