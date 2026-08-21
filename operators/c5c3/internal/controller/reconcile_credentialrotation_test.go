// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the CredentialRotation reconciler bootstrap
// idempotence, the re-mint nudge, password-hash-change detection, unsupported
// targets, and the deferred scheduled-rotation fields.
package controller

import (
	"context"
	"testing"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// credentialRotation builds a CredentialRotation CR in the default namespace
// (same namespace as korcControlPlane) targeting the admin application credential.
func credentialRotation() *c5c3v1alpha1.CredentialRotation {
	return &c5c3v1alpha1.CredentialRotation{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rotate-admin",
			Namespace:  "default",
			Generation: 3,
		},
		Spec: c5c3v1alpha1.CredentialRotationSpec{
			Target: c5c3v1alpha1.RotationTargetAdminApplicationCredential,
		},
	}
}

// existingAC builds the owned admin ApplicationCredential CR with the given
// password-hash annotation already stamped (as reconcileKORC would have done).
func existingAC(cp *c5c3v1alpha1.ControlPlane, hash string) *orcv1alpha1.ApplicationCredential {
	return &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:        adminAppCredentialName(cp),
			Namespace:   childNamespace(cp),
			Annotations: map[string]string{adminPasswordHashAnnotation: hash},
		},
	}
}

// rotationReconcileResult runs the CredentialRotation reconciler against the
// given seeded objects and returns the reloaded CR plus the reconciler client.
func runRotationReconcile(
	t *testing.T, objs ...client.Object,
) (*c5c3v1alpha1.CredentialRotation, client.Client) {
	t.Helper()
	got, c, _ := runRotationReconcileAt(t,
		types.NamespacedName{Namespace: "default", Name: "rotate-admin"}, objs...)
	return got, c
}

// runRotationReconcileAt is runRotationReconcile for a CR at an arbitrary key,
// and additionally returns the reconcile result so a requeue can be asserted.
func runRotationReconcileAt(
	t *testing.T, key types.NamespacedName, objs ...client.Object,
) (*c5c3v1alpha1.CredentialRotation, client.Client, ctrl.Result) {
	t.Helper()
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&c5c3v1alpha1.CredentialRotation{}).
		Build()

	r := &CredentialRotationReconciler{Client: c, Scheme: s}
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())

	got := &c5c3v1alpha1.CredentialRotation{}
	g.Expect(c.Get(context.Background(), key, got)).To(Succeed())
	return got, c, result
}

// rotationReadyCondition returns the Ready condition of the CR (or nil).
func rotationReadyCondition(cr *c5c3v1alpha1.CredentialRotation) *metav1.Condition {
	return conditions.GetCondition(cr.Status.Conditions, conditionTypeRotationReady)
}

// --- Bootstrap ---

func TestRotation_BootstrapNoOpWhenACExists(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcControlPlane()
	cr := credentialRotation()
	cr.Spec.Bootstrap = true
	ac := existingAC(cp, testPasswordHash())

	got, _ := runRotationReconcile(t, cp, cr, ac, adminPasswordSecret())

	cond := rotationReadyCondition(got)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BootstrapComplete"))
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(3)))
}

func TestRotation_BootstrapWaitsWhenACAbsent(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcControlPlane()
	cr := credentialRotation()
	cr.Spec.Bootstrap = true

	got, _ := runRotationReconcile(t, cp, cr, adminPasswordSecret())

	cond := rotationReadyCondition(got)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForBootstrap"))
}

// --- ReMint nudge ---

func TestRotation_ReMintClearsHashAnnotation(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcControlPlane()
	cr := credentialRotation()
	cr.Spec.ReMint = true
	// Annotation matches the current password so only ReMint=true drives the nudge.
	ac := existingAC(cp, testPasswordHash())

	got, c := runRotationReconcile(t, cp, cr, ac, adminPasswordSecret())

	cond := rotationReadyCondition(got)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("RotationTriggered"))

	reloaded := getAC(t, c, cp)
	g.Expect(reloaded.Annotations[adminPasswordHashAnnotation]).To(BeEmpty(),
		"reMint must clear the password-hash annotation to nudge a re-mint")
}

// TestRotation_ReMintFullCycleReStampsHash drives the COMPLETE re-mint mutation
// cycle, not just the nudge half (TE7). It (1) runs the CredentialRotation
// reconciler to clear the password-hash annotation (the nudge), then runs two
// reconcileKORC passes against the SAME client to prove the nudge is consumed:
// (2a) the cleared annotation drives reconcileKORC to DELETE the AC (the re-mint
// trigger), and (2b) the next pass recreates it stamped with the current hash.
// Asserting all three steps guards against a regression where the nudge fires but
// the delete+recreate never happens (or vice versa).
func TestRotation_ReMintFullCycleReStampsHash(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	cr := credentialRotation()
	cr.Spec.ReMint = true
	ac := existingAC(cp, testPasswordHash())

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cp, cr, ac, adminPasswordSecret()).
		WithStatusSubresource(&c5c3v1alpha1.CredentialRotation{}).
		Build()

	// --- Half 1: the rotation reconciler clears the annotation (the nudge). ---
	rotator := &CredentialRotationReconciler{Client: c, Scheme: s}
	_, err := rotator.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "rotate-admin"},
	})
	g.Expect(err).NotTo(HaveOccurred())

	nudged := getAC(t, c, cp)
	g.Expect(nudged.Annotations[adminPasswordHashAnnotation]).To(BeEmpty(),
		"reMint must clear the password-hash annotation to nudge a re-mint")

	// --- Half 2a: reconcileKORC consumes the nudge and DELETES the AC to re-mint. ---
	cpr := &ControlPlaneReconciler{Client: c, Scheme: s}
	_, err = cpr.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	deleted := &orcv1alpha1.ApplicationCredential{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialName(cp), Namespace: childNamespace(cp),
	}, deleted)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"the cleared annotation must drive reconcileKORC to delete the AC for a re-mint")

	// --- Half 2b: the next pass recreates the AC stamped with the current hash. ---
	_, err = cpr.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	reminted := getAC(t, c, cp)
	g.Expect(reminted.Annotations).To(HaveKeyWithValue(adminPasswordHashAnnotation, testPasswordHash()),
		"reconcileKORC must recreate the AC and stamp the current password hash (re-mint)")
}

// TestRotation_ReMintLatchedToGeneration proves the reMint one-shot latch: a
// `reMint: true` left in the spec must nudge exactly once per spec generation, not
// on every cache resync or operator restart (the indefinite re-rotation loop this
// fixes). It runs the reconciler twice against the SAME client with the SAME
// generation: pass 1 nudges (clears the annotation, records
// lastTriggeredGeneration), then the annotation is re-stamped to simulate
// reconcileKORC completing the re-mint, and pass 2 — with reMint STILL true — must
// observe the latch and report NoRotationNeeded WITHOUT re-clearing the annotation.
func TestRotation_ReMintLatchedToGeneration(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	cr := credentialRotation()
	cr.Spec.ReMint = true
	ac := existingAC(cp, testPasswordHash())

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cp, cr, ac, adminPasswordSecret()).
		WithStatusSubresource(&c5c3v1alpha1.CredentialRotation{}).
		Build()

	rotator := &CredentialRotationReconciler{Client: c, Scheme: s}
	crKey := types.NamespacedName{Namespace: "default", Name: "rotate-admin"}

	// --- Pass 1: reMint fires once, clearing the annotation and latching. ---
	_, err := rotator.Reconcile(context.Background(), ctrl.Request{NamespacedName: crKey})
	g.Expect(err).NotTo(HaveOccurred())

	got := &c5c3v1alpha1.CredentialRotation{}
	g.Expect(c.Get(context.Background(), crKey, got)).To(Succeed())
	g.Expect(rotationReadyCondition(got).Reason).To(Equal("RotationTriggered"))
	g.Expect(got.Status.LastTriggeredGeneration).To(Equal(int64(3)),
		"an explicit reMint must latch on the current spec generation")
	g.Expect(getAC(t, c, cp).Annotations[adminPasswordHashAnnotation]).To(BeEmpty())

	// Simulate reconcileKORC consuming the nudge and re-stamping the fresh hash.
	reminted := getAC(t, c, cp)
	reminted.Annotations[adminPasswordHashAnnotation] = testPasswordHash()
	g.Expect(c.Update(context.Background(), reminted)).To(Succeed())

	// --- Pass 2: reMint is STILL true but the generation is unchanged. ---
	_, err = rotator.Reconcile(context.Background(), ctrl.Request{NamespacedName: crKey})
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(c.Get(context.Background(), crKey, got)).To(Succeed())
	g.Expect(rotationReadyCondition(got).Reason).To(Equal("NoRotationNeeded"),
		"a latched reMint must not re-fire on a subsequent pass of the same generation")
	g.Expect(getAC(t, c, cp).Annotations[adminPasswordHashAnnotation]).To(Equal(testPasswordHash()),
		"the latched reMint must leave the re-stamped annotation untouched (no second nudge)")
}

func TestRotation_PasswordHashChangeTriggersNudge(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcControlPlane()
	cr := credentialRotation()
	// Stamp a stale hash so the current password hash differs.
	ac := existingAC(cp, "stale-hash-value")

	got, c := runRotationReconcile(t, cp, cr, ac, adminPasswordSecret())

	cond := rotationReadyCondition(got)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("RotationTriggered"))

	reloaded := getAC(t, c, cp)
	g.Expect(reloaded.Annotations[adminPasswordHashAnnotation]).To(BeEmpty(),
		"a password-hash change must clear the annotation to nudge a re-mint")
}

// TestRotation_ExternalPasswordHashChangeTriggersNudge proves the nudge path works
// unchanged against an external Keystone: effectiveAdminPasswordSecretRef resolves
// to the USER-supplied Secret, so rotating it out-of-band clears the stamped hash
// and the next reconcileKORC pass re-mints against the external endpoint. This is
// the only supported rotation path — the operator never writes to the external
// installation.
func TestRotation_ExternalPasswordHashChangeTriggersNudge(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcExternalControlPlane()
	cr := credentialRotation()
	ac := existingAC(cp, "stale-hash-value")

	got, c := runRotationReconcile(t, cp, cr, ac, adminPasswordSecret())

	cond := rotationReadyCondition(got)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("RotationTriggered"))

	reloaded := getAC(t, c, cp)
	g.Expect(reloaded.Annotations[adminPasswordHashAnnotation]).To(BeEmpty(),
		"rotating the user-supplied admin password must nudge a re-mint in External mode too")
}

// TestRotation_ExternalMissingAdminPasswordSecretIsNotARotation covers the error
// path: with the user's Secret absent the rotator cannot derive a hash, so it must
// not clear the annotation — a cleared annotation would nudge a re-mint that
// revokes the working credential and cannot mint a replacement.
func TestRotation_ExternalMissingAdminPasswordSecretIsNotARotation(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcExternalControlPlane()
	cr := credentialRotation()
	ac := existingAC(cp, testPasswordHash())

	// No adminPasswordSecret() is seeded.
	got, c := runRotationReconcile(t, cp, cr, ac)

	cond := rotationReadyCondition(got)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))

	reloaded := getAC(t, c, cp)
	g.Expect(reloaded.Annotations[adminPasswordHashAnnotation]).To(Equal(testPasswordHash()),
		"an unreadable admin password must never clear the stamped hash")
}

func TestRotation_HashMatchIsNoOp(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcControlPlane()
	cr := credentialRotation()
	// Annotation matches the current password hash; no ReMint -> no rotation.
	ac := existingAC(cp, testPasswordHash())

	got, c := runRotationReconcile(t, cp, cr, ac, adminPasswordSecret())

	cond := rotationReadyCondition(got)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NoRotationNeeded"))

	reloaded := getAC(t, c, cp)
	g.Expect(reloaded.Annotations[adminPasswordHashAnnotation]).To(Equal(testPasswordHash()),
		"a hash match must leave the annotation untouched")
}

// --- Unsupported target ---

func TestRotation_UnsupportedTargetReadyFalse(t *testing.T) {
	g := NewGomegaWithT(t)

	cr := credentialRotation()
	cr.Spec.Target = c5c3v1alpha1.RotationTarget("somethingElse")

	got, _ := runRotationReconcile(t, cr)

	cond := rotationReadyCondition(got)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("UnsupportedTarget"))
}

// --- ControlPlane lookup ---

func TestRotation_NoControlPlaneReadyFalse(t *testing.T) {
	g := NewGomegaWithT(t)

	cr := credentialRotation()
	cr.Spec.ReMint = true

	got, _ := runRotationReconcile(t, cr)

	cond := rotationReadyCondition(got)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NoControlPlane"))
}

// TestResolveControlPlane_AmbiguousIsDefenseInDepth verifies that when two
// ControlPlanes coexist in a namespace (a state the ControlPlane validating webhook now prevents on CREATE), the CredentialRotation
// reconciler fails safe with Ready=False reason "AmbiguousControlPlane" rather
// than silently picking one. This branch is defense-in-depth for CRs that
// predate the webhook guard or callers that bypass it.
func TestResolveControlPlane_AmbiguousIsDefenseInDepth(t *testing.T) {
	g := NewGomegaWithT(t)

	cp1 := korcControlPlane()
	cp2 := korcControlPlane()
	cp2.Name = cp1.Name + "-second" // same namespace, distinct name => ambiguous
	cp2.UID = types.UID("cp-uid-second")

	cr := credentialRotation()
	cr.Spec.ReMint = true

	got, _ := runRotationReconcile(t, cp1, cp2, cr)

	cond := rotationReadyCondition(got)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("AmbiguousControlPlane"))
}

// --- Scheduled fields accepted but loop not run ---

func TestRotation_ScheduledFieldsAcceptedNoError(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcControlPlane()
	cr := credentialRotation()
	cr.Spec.IntervalDays = ptr.To(int32(30))
	cr.Spec.PreRotationDays = ptr.To(int32(7))
	cr.Spec.GracePeriodDays = ptr.To(int32(3))
	// Hash matches so the one-shot decision is a no-op; the scheduled fields must
	// not cause an error and must not run any loop.
	ac := existingAC(cp, testPasswordHash())

	got, _ := runRotationReconcile(t, cp, cr, ac, adminPasswordSecret())

	cond := rotationReadyCondition(got)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"scheduled fields must be accepted without error; one-shot semantics still apply")
	g.Expect(cond.Reason).To(Equal("NoRotationNeeded"))
}

// --- Service-account password rotation target ---

// saKeystoneService returns the account-only KeystoneService "nova" the
// service-account rotation tests target. It sits in the same namespace as the
// CredentialRotation and references the default/cp ControlPlane fixture.
func saKeystoneService() *c5c3v1alpha1.KeystoneService {
	return &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: "nova", Namespace: "default"},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp"},
			Account: &c5c3v1alpha1.KeystoneServiceAccountSpec{
				Project: c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service"},
			},
		},
	}
}

// saRotationCR builds a CredentialRotation targeting the password of the "nova"
// KeystoneService's account.
func saRotationCR() *c5c3v1alpha1.CredentialRotation {
	cr := credentialRotation()
	cr.Spec.Target = c5c3v1alpha1.RotationTargetServiceAccountPassword
	cr.Spec.KeystoneService = "nova"
	return cr
}

// managedSAUser builds the managed User CR the KeystoneService reconciler
// projects for ks's account, with the given generation annotation. It lives in
// cp's namespace, never in the registration's, and carries the ownership labels
// stampKeystoneServiceChildLabels puts on every projected child — the only
// ownership marker a cross-namespace child can have.
func managedSAUser(ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane, gen string) *orcv1alpha1.User {
	return &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        keystoneServiceUserRef(ks),
			Namespace:   keystoneServiceChildNamespace(cp),
			Labels:      keystoneServiceChildLabels(ks),
			Annotations: map[string]string{serviceAccountPasswordGenerationAnnotation: gen},
		},
	}
}

// getSAUser reloads the managed User at the coordinates the reconciler nudges.
func getSAUser(t *testing.T, c client.Client, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane) *orcv1alpha1.User {
	t.Helper()
	g := NewGomegaWithT(t)
	user := &orcv1alpha1.User{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: keystoneServiceUserRef(ks), Namespace: keystoneServiceChildNamespace(cp),
	}, user)).To(Succeed())
	return user
}

// TestRotation_KeystoneServiceNotFound covers the dangling reference: the CR
// names a KeystoneService that does not exist, so there is nothing to rotate and
// the reconciler defers rather than erroring.
func TestRotation_KeystoneServiceNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := saRotationCR()
	cr.Spec.KeystoneService = "glance" // no such KeystoneService

	got, _, result := runRotationReconcileAt(t,
		types.NamespacedName{Namespace: "default", Name: "rotate-admin"},
		korcControlPlane(), saKeystoneService(), cr)
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("KeystoneServiceNotFound"))
	g.Expect(cond.Message).To(ContainSubstring("default/glance"))
	g.Expect(result.RequeueAfter).To(Equal(credentialRotationRequeueAfter))
}

// TestRotation_KeystoneServiceNoAccountDeclared covers a catalog-only
// registration: it declares no account, so there is no password to rotate. The
// account block can still be added by a later spec edit, so the reconciler waits.
func TestRotation_KeystoneServiceNoAccountDeclared(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := saKeystoneService()
	ks.Spec.Account = nil
	ks.Spec.Catalog = &c5c3v1alpha1.KeystoneServiceCatalogSpec{ServiceType: "compute"}

	got, _, result := runRotationReconcileAt(t,
		types.NamespacedName{Namespace: "default", Name: "rotate-admin"},
		korcControlPlane(), ks, saRotationCR())
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NoAccountDeclared"))
	g.Expect(result.RequeueAfter).To(Equal(credentialRotationRequeueAfter))
}

// TestRotation_KeystoneServiceControlPlaneRefDangling covers the second hop of
// the resolution chain: the KeystoneService exists and declares an account, but
// its controlPlaneRef names a ControlPlane that does not exist, so the child
// coordinates cannot be derived.
func TestRotation_KeystoneServiceControlPlaneRefDangling(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := saKeystoneService()
	ks.Spec.ControlPlaneRef.Name = "absent-cp"

	got, _, result := runRotationReconcileAt(t,
		types.NamespacedName{Namespace: "default", Name: "rotate-admin"},
		korcControlPlane(), ks, saRotationCR())
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ControlPlaneNotFound"))
	g.Expect(result.RequeueAfter).To(Equal(credentialRotationRequeueAfter))
}

// foreignNamespaceRotation builds the cross-namespace fixture set the two
// foreign-namespace tests share: an admitted registration in "tenant" (which
// holds no ControlPlane of its own) against the default/cp plane, plus the
// CredentialRotation beside it. The caller narrows the plane's consent.
func foreignNamespaceRotation() (*c5c3v1alpha1.ControlPlane, *c5c3v1alpha1.KeystoneService, *c5c3v1alpha1.CredentialRotation) {
	cp := ksControlPlane() // default/cp; namespace "tenant" holds no ControlPlane
	cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{
		AllowedNamespaces: []string{"tenant"},
	}
	ks := saKeystoneService()
	ks.Namespace = "tenant"
	ks.Spec.ControlPlaneRef = c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "default"}
	cr := saRotationCR()
	cr.Namespace = "tenant"
	cr.Spec.ReMint = true
	return cp, ks, cr
}

// TestRotation_ForeignNamespaceKeystoneServiceRotates proves the service-account
// target does NOT use the admin path's same-namespace ControlPlane lookup: the
// CredentialRotation and the KeystoneService live in a namespace that holds no
// ControlPlane at all, and — with the plane admitting that namespace — the
// rotation still fires against the User in the referenced plane's namespace.
func TestRotation_ForeignNamespaceKeystoneServiceRotates(t *testing.T) {
	g := NewGomegaWithT(t)
	cp, ks, cr := foreignNamespaceRotation()

	key := types.NamespacedName{Namespace: "tenant", Name: cr.Name}
	got, c, _ := runRotationReconcileAt(t, key, cp, ks, cr, managedSAUser(ks, cp, "1"))
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("RotationTriggered"))

	g.Expect(getSAUser(t, c, ks, cp).Annotations[serviceAccountPasswordGenerationAnnotation]).To(BeEmpty(),
		"a registration in a ControlPlane-less namespace must still nudge the User in the plane's namespace")
}

// TestRotation_ForeignNamespaceNotAllowedIsRefused pins the registration-consent
// gate on the rotation path. Consent is the escalation control that keeps
// namespace write access from becoming cloud admin, and de-listing a namespace
// FREEZES its registrations rather than deleting their children — so the User
// survives in the plane's namespace with nothing left to act on a nudge. Without
// the gate this path would write into a namespace whose plane has withdrawn
// consent and report Ready=True while nothing rotates.
func TestRotation_ForeignNamespaceNotAllowedIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)
	cp, ks, cr := foreignNamespaceRotation()
	cp.Spec.KORC.ServiceRegistrations = nil // consent withdrawn

	key := types.NamespacedName{Namespace: "tenant", Name: cr.Name}
	got, c, result := runRotationReconcileAt(t, key, cp, ks, cr, managedSAUser(ks, cp, "1"))
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceNamespaceNotAllowed))
	g.Expect(cond.Message).To(ContainSubstring(`"tenant"`))
	g.Expect(result.RequeueAfter).To(Equal(credentialRotationRequeueAfter))

	g.Expect(getSAUser(t, c, ks, cp).Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("1"),
		"a registration the plane does not admit must not reach the User in the plane's namespace")
	g.Expect(got.Status.LastTriggeredGeneration).To(BeZero(),
		"a refused rotation must not latch the generation, so admitting the namespace later does not skip it")
}

// TestRotation_ServiceAccountWaitsForAdminCredential pins the third gate the
// rotation path repeats from the KeystoneService reconciler: K-ORC cannot reach
// Keystone before the plane's admin credential is minted, so an unready plane
// defers the registration while its User is still there to be nudged. Without the
// gate the rotation would clear the annotation, latch the generation and report
// Ready=True while nothing rotates until an operator repairs the plane — the
// exact reporting failure the namespace gate above prevents.
func TestRotation_ServiceAccountWaitsForAdminCredential(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane() // AdminCredentialReady deliberately unset
	ks := saKeystoneService()
	cr := saRotationCR()
	cr.Spec.ReMint = true

	got, c, result := runRotationReconcileAt(t,
		types.NamespacedName{Namespace: "default", Name: cr.Name},
		cp, ks, cr, managedSAUser(ks, cp, "1"))
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccountAdmin))
	g.Expect(result.RequeueAfter).To(Equal(credentialRotationRequeueAfter))

	g.Expect(getSAUser(t, c, ks, cp).Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("1"),
		"a plane whose KeystoneService reconciler cannot reach Keystone must not have its User nudged")
	g.Expect(got.Status.LastTriggeredGeneration).To(BeZero(),
		"a deferred rotation must not latch the generation, so it still fires once the plane recovers")
}

// TestRotation_SettledServiceAccountIgnoresAdminCredentialDip pins the scope of
// the gate above: it guards the NUDGE, not the read-only branches before it.
// AdminCredentialReady is not rare — it dips on an ESO/OpenBao blip, a
// not-yet-Ready clouds.yaml ExternalSecret or an admin re-mint — and a rotation
// that already fired has nothing pending for the dip to hold up. Gating the whole
// path would flip every completed CredentialRotation to Ready=False with a message
// claiming a nudge is waiting, and turn a terminal result into a 10s poll for the
// duration of the outage.
func TestRotation_SettledServiceAccountIgnoresAdminCredentialDip(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane() // AdminCredentialReady deliberately unset
	ks := saKeystoneService()
	cr := saRotationCR()
	cr.Spec.ReMint = true
	cr.Status.LastTriggeredGeneration = cr.Generation // the one-shot already fired

	got, c, result := runRotationReconcileAt(t,
		types.NamespacedName{Namespace: "default", Name: cr.Name},
		cp, ks, cr, managedSAUser(ks, cp, "2"))
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"a completed rotation must not report the plane's credential outage as its own failure")
	g.Expect(cond.Reason).To(Equal("NoRotationNeeded"))
	g.Expect(result.RequeueAfter).To(BeZero(),
		"a settled one-shot stays terminal; the dip must not turn it into a 10s poll")

	g.Expect(getSAUser(t, c, ks, cp).Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("2"),
		"the settled branch writes nothing either way")
}

// TestRotation_ServiceAccountBootstrapIgnoresAdminCredentialDip is the bootstrap
// half of the same scope: a bootstrap CredentialRotation never nudges — it only
// reports whether the KeystoneService reconciler has projected the User yet — so
// an unready plane has no write to defer and must not be reported as a failure of
// the bootstrap.
func TestRotation_ServiceAccountBootstrapIgnoresAdminCredentialDip(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane() // AdminCredentialReady deliberately unset
	ks := saKeystoneService()
	cr := saRotationCR()
	cr.Spec.Bootstrap = true

	got, _, result := runRotationReconcileAt(t,
		types.NamespacedName{Namespace: "default", Name: cr.Name},
		cp, ks, cr, managedSAUser(ks, cp, "1"))
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BootstrapComplete"))
	g.Expect(result.RequeueAfter).To(BeZero())
}

// TestRotation_ForeignServiceAccountRefused pins the ownership check. The User
// lives outside the registration's namespace, so it carries no owner reference
// and the derived name is the only thing tying it to the KeystoneService — a User
// whose prefix digest collides with another registration's, or one nothing has
// claimed, would otherwise be rotated on this CR's behalf.
func TestRotation_ForeignServiceAccountRefused(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ksControlPlane()
	ks := saKeystoneService()
	cr := saRotationCR()
	cr.Spec.ReMint = true

	// Same coordinates, but the ownership labels name a different registration.
	stranger := managedSAUser(ks, cp, "1")
	stranger.Labels = map[string]string{
		keystoneServiceNameLabel:      "nova",
		keystoneServiceNamespaceLabel: "somebody-else",
	}

	got, c := runRotationReconcile(t, cp, ks, cr, stranger)
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ForeignServiceAccount"))

	g.Expect(getSAUser(t, c, ks, cp).Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("1"),
		"a User this registration did not create must not be nudged")
}

// TestRotation_MissingKeystoneServiceIsActionable covers the CR that predates the
// keystoneService field: it decodes with an empty name, which MinLength and the
// CEL rule make unreachable on create. Looking that up would report a dangling
// reference to a KeystoneService with no name and requeue forever, so the
// reconciler names the real cause and stops.
func TestRotation_MissingKeystoneServiceIsActionable(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := saRotationCR()
	cr.Spec.KeystoneService = "" // as a stored CR written against spec.serviceAccount decodes

	got, _, result := runRotationReconcileAt(t,
		types.NamespacedName{Namespace: "default", Name: "rotate-admin"},
		korcControlPlane(), saKeystoneService(), cr)
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("MissingKeystoneService"))
	g.Expect(cond.Message).To(ContainSubstring("spec.serviceAccount"))
	g.Expect(result.RequeueAfter).To(BeZero(),
		"only a re-created CR can resolve this, so requeueing every 10s would just repeat the message")
}

func TestRotation_ServiceAccountBootstrapNoOpWhenUserExists(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ksControlPlane()
	ks := saKeystoneService()
	cr := saRotationCR()
	cr.Spec.Bootstrap = true

	got, _ := runRotationReconcile(t, cp, ks, cr, managedSAUser(ks, cp, "1"))
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BootstrapComplete"))
}

func TestRotation_ServiceAccountBootstrapWaitsWhenUserAbsent(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := saRotationCR()
	cr.Spec.Bootstrap = true

	got, _ := runRotationReconcile(t, ksControlPlane(), saKeystoneService(), cr)
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForBootstrap"))
}

func TestRotation_ServiceAccountReMintClearsGenerationAnnotation(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ksControlPlane()
	ks := saKeystoneService()
	cr := saRotationCR()
	cr.Spec.ReMint = true

	got, c := runRotationReconcile(t, cp, ks, cr, managedSAUser(ks, cp, "1"))
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("RotationTriggered"))
	g.Expect(got.Status.LastTriggeredGeneration).To(Equal(got.Generation))

	g.Expect(getSAUser(t, c, ks, cp).Annotations[serviceAccountPasswordGenerationAnnotation]).To(BeEmpty(),
		"reMint must clear the generation annotation to nudge a rotation")
}

// TestRotation_ServiceAccountReMintLatchedToGeneration proves the service-account
// reMint one-shot latch (the sibling of TestRotation_ReMintLatchedToGeneration for
// the admin path): a `reMint: true` left in the spec must nudge once per spec
// generation, not on every resync. With lastTriggeredGeneration already equal to
// the current generation, a subsequent pass must report NoRotationNeeded and leave
// the re-stamped generation annotation untouched — a regression that dropped the
// `LastTriggeredGeneration == cr.Generation` term would re-clear it every requeue.
func TestRotation_ServiceAccountReMintLatchedToGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ksControlPlane()
	ks := saKeystoneService()
	cr := saRotationCR()
	cr.Spec.ReMint = true
	// The reMint already fired for this spec generation (latched).
	cr.Status.LastTriggeredGeneration = cr.Generation
	// The generation annotation was re-stamped after the earlier rotation nudge.
	user := managedSAUser(ks, cp, "2")

	got, c := runRotationReconcile(t, cp, ks, cr, user)
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NoRotationNeeded"),
		"a latched reMint must not re-fire on a subsequent pass of the same generation")

	g.Expect(getSAUser(t, c, ks, cp).Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("2"),
		"a latched reMint must leave the re-stamped generation annotation untouched (no second nudge)")
}

// TestRotation_ServiceAccountWaitsWhenUserAbsent covers the non-bootstrap
// WaitingForServiceAccount branch: a rotation requested before the KeystoneService
// reconciler has projected the managed User must defer (Ready=False) rather than
// clear an annotation on a User that does not exist yet.
func TestRotation_ServiceAccountWaitsWhenUserAbsent(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := saRotationCR()
	cr.Spec.ReMint = true // non-bootstrap rotation, but the User is not yet projected

	// No managed User seeded.
	got, _ := runRotationReconcile(t, ksControlPlane(), saKeystoneService(), cr)
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForServiceAccount"))
}

func TestRotation_ServiceAccountNoRotationWithoutReMint(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := ksControlPlane()
	ks := saKeystoneService()
	cr := saRotationCR() // no reMint

	got, c := runRotationReconcile(t, cp, ks, cr, managedSAUser(ks, cp, "1"))
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NoRotationNeeded"))

	g.Expect(getSAUser(t, c, ks, cp).Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("1"),
		"without reMint the generation annotation must be untouched")
}
