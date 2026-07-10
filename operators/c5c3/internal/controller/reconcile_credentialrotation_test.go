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

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
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
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&c5c3v1alpha1.CredentialRotation{}).
		Build()

	r := &CredentialRotationReconciler{Client: c, Scheme: s}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "rotate-admin"},
	})
	g.Expect(err).NotTo(HaveOccurred())

	got := &c5c3v1alpha1.CredentialRotation{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "rotate-admin"}, got)).To(Succeed())
	return got, c
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

// cpWithServiceAccount returns the admin-mode ControlPlane with one declared
// service account "nova".
func cpWithServiceAccount() *c5c3v1alpha1.ControlPlane {
	cp := korcControlPlane()
	cp.Spec.KORC.ServiceAccounts = []c5c3v1alpha1.ServiceAccountSpec{{
		Name:    "nova",
		Project: c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service"},
	}}
	return cp
}

// saRotationCR builds a CredentialRotation targeting the "nova" service account's
// password.
func saRotationCR() *c5c3v1alpha1.CredentialRotation {
	cr := credentialRotation()
	cr.Spec.Target = c5c3v1alpha1.RotationTargetServiceAccountPassword
	cr.Spec.ServiceAccount = "nova"
	return cr
}

// managedSAUser builds the managed User CR reconcileServiceAccounts owns, with the
// given generation annotation.
func managedSAUser(cp *c5c3v1alpha1.ControlPlane, gen string) *orcv1alpha1.User {
	sa := cp.Spec.KORC.ServiceAccounts[0]
	return &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceAccountUserRef(cp, sa),
			Namespace:   childNamespace(cp),
			Annotations: map[string]string{serviceAccountPasswordGenerationAnnotation: gen},
		},
	}
}

func TestRotation_ServiceAccountUnknown(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cpWithServiceAccount()
	cr := saRotationCR()
	cr.Spec.ServiceAccount = "glance" // not declared

	got, _ := runRotationReconcile(t, cp, cr)
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("UnknownServiceAccount"))
}

func TestRotation_ServiceAccountBootstrapNoOpWhenUserExists(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cpWithServiceAccount()
	cr := saRotationCR()
	cr.Spec.Bootstrap = true

	got, _ := runRotationReconcile(t, cp, cr, managedSAUser(cp, "1"))
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BootstrapComplete"))
}

func TestRotation_ServiceAccountBootstrapWaitsWhenUserAbsent(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cpWithServiceAccount()
	cr := saRotationCR()
	cr.Spec.Bootstrap = true

	got, _ := runRotationReconcile(t, cp, cr)
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForBootstrap"))
}

func TestRotation_ServiceAccountReMintClearsGenerationAnnotation(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cpWithServiceAccount()
	cr := saRotationCR()
	cr.Spec.ReMint = true
	user := managedSAUser(cp, "1")

	got, c := runRotationReconcile(t, cp, cr, user)
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("RotationTriggered"))
	g.Expect(got.Status.LastTriggeredGeneration).To(Equal(got.Generation))

	reloaded := &orcv1alpha1.User{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: serviceAccountUserRef(cp, cp.Spec.KORC.ServiceAccounts[0]), Namespace: childNamespace(cp),
	}, reloaded)).To(Succeed())
	g.Expect(reloaded.Annotations[serviceAccountPasswordGenerationAnnotation]).To(BeEmpty(),
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
	cp := cpWithServiceAccount()
	cr := saRotationCR()
	cr.Spec.ReMint = true
	// The reMint already fired for this spec generation (latched).
	cr.Status.LastTriggeredGeneration = cr.Generation
	// The generation annotation was re-stamped after the earlier rotation nudge.
	user := managedSAUser(cp, "2")

	got, c := runRotationReconcile(t, cp, cr, user)
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NoRotationNeeded"),
		"a latched reMint must not re-fire on a subsequent pass of the same generation")

	reloaded := &orcv1alpha1.User{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: serviceAccountUserRef(cp, cp.Spec.KORC.ServiceAccounts[0]), Namespace: childNamespace(cp),
	}, reloaded)).To(Succeed())
	g.Expect(reloaded.Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("2"),
		"a latched reMint must leave the re-stamped generation annotation untouched (no second nudge)")
}

// TestRotation_ServiceAccountWaitsWhenUserAbsent covers the non-bootstrap
// WaitingForServiceAccount branch: a rotation requested before the ControlPlane
// reconciler has provisioned the managed User must defer (Ready=False) rather than
// clear an annotation on a User that does not exist yet.
func TestRotation_ServiceAccountWaitsWhenUserAbsent(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cpWithServiceAccount()
	cr := saRotationCR()
	cr.Spec.ReMint = true // non-bootstrap rotation, but the User is not yet provisioned

	got, _ := runRotationReconcile(t, cp, cr) // no managed User seeded
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForServiceAccount"))
}

func TestRotation_ServiceAccountNoRotationWithoutReMint(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cpWithServiceAccount()
	cr := saRotationCR() // no reMint
	user := managedSAUser(cp, "1")

	got, c := runRotationReconcile(t, cp, cr, user)
	cond := rotationReadyCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NoRotationNeeded"))

	reloaded := &orcv1alpha1.User{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: serviceAccountUserRef(cp, cp.Spec.KORC.ServiceAccounts[0]), Namespace: childNamespace(cp),
	}, reloaded)).To(Succeed())
	g.Expect(reloaded.Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("1"),
		"without reMint the generation annotation must be untouched")
}
