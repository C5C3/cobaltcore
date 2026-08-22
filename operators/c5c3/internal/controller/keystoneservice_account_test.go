// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneService account block: the collision gates, the managed
// user and its password generations, the role assignments, and the OpenBao
// round-trip behind the consumer Secret.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// ksAccountReconciler builds the reconciler and its client for an account-block
// test, seeding the CR, its ControlPlane and the ready tenant store the delivery
// leg gates on.
func ksAccountReconciler(
	t *testing.T, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane, objs ...client.Object,
) (*KeystoneServiceReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	s := korcTestScheme(t)
	all := append([]client.Object{cp, ks, readyTenantStoreFor(cp)}, objs...)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(all...).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}).
		Build()
	recorder := record.NewFakeRecorder(20)
	return &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: recorder}, c, recorder
}

// runKSAccount drives ensureAccount once and returns the resulting AccountReady
// condition. The conditions are written onto ks in memory, exactly as the
// reconcile loop leaves them before its single status persist.
func runKSAccount(
	t *testing.T, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane, objs ...client.Object,
) (*metav1.Condition, client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)
	r, c, _ := ksAccountReconciler(t, ks, cp, objs...)
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)
	_, err := r.ensureAccount(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())
	return ksAccountCondition(ks), c
}

// ksWithAccount returns a CR declaring the minimal account block.
func ksWithAccount() *c5c3v1alpha1.KeystoneService {
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()
	return ks
}

func ksGetUser(t *testing.T, c client.Client, name, ns string) (*orcv1alpha1.User, bool) {
	t.Helper()
	u := &orcv1alpha1.User{}
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, u)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("getting User %q: %v", name, err)
	}
	return u, true
}

// --- the project handle ---

func TestKSAccount_ReferencedProjectIsAnUnmanagedImport(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount() // project.create defaults to false

	_, c := runKSAccount(t, ks, cp)

	project := &orcv1alpha1.Project{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceProjectRef(ks), Namespace: ks.Namespace}, project)).To(Succeed())
	g.Expect(project.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
		"a referenced project must never be created or deleted by the operator")
	g.Expect(project.Spec.Import).NotTo(BeNil())
	g.Expect(string(*project.Spec.Import.Filter.Name)).To(Equal("service"))
	g.Expect(project.Spec.Resource).To(BeNil())
}

func TestKSAccount_ManagedProjectCollisionFailsLoudly(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Project.Create = true

	// The probe resolved: a project of that name already exists in Keystone.
	probe := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceProjectProbeRef(ks), Namespace: ks.Namespace},
		Status:     orcv1alpha1.ProjectStatus{Conditions: availableImportConditions()},
	}

	cond, c := runKSAccount(t, ks, cp, probe)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring("account.project.create=false"))
	g.Expect(cond.Message).To(ContainSubstring("service"))

	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceProjectRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.Project{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no managed Project may be created over a collision")
}

func TestKSAccount_ManagedProjectProbePending(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Project.Create = true

	// No probe seeded: the apply creates it and it has resolved neither way.
	cond, c := runKSAccount(t, ks, cp)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonProbingForCollision))
	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceProjectRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.Project{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "nothing is created while the probe is unresolved")
}

func TestKSAccount_ManagedProjectCreatedOnceTheProbeReportsAbsent(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Project.Create = true

	probe := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceProjectProbeRef(ks), Namespace: ks.Namespace},
		Status:     orcv1alpha1.ProjectStatus{Conditions: pendingImportConditions(0)},
	}

	_, c := runKSAccount(t, ks, cp, probe)

	project := &orcv1alpha1.Project{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceProjectRef(ks), Namespace: ks.Namespace}, project)).To(Succeed())
	g.Expect(project.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	// The resolved probe is dropped so it stops polling Keystone.
	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceProjectProbeRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.Project{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

// --- the user collision gate ---

func TestKSAccount_UserCollisionFailsLoudly(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	probe := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceUserProbeRef(ks), Namespace: ks.Namespace},
		Status:     orcv1alpha1.UserStatus{Conditions: availableImportConditions()},
	}

	cond, c := runKSAccount(t, ks, cp, probe)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring("account.adopt=true"))
	g.Expect(cond.Message).To(ContainSubstring(ksTestName))

	_, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeFalse(), "the managed User must not be created over a collision")
}

func TestKSAccount_UserProbePending(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	cond, c := runKSAccount(t, ks, cp)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonProbingForCollision))
	_, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeFalse())
}

func TestKSAccount_AdoptSkipsTheProbeEntirely(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Adopt = true

	_, c := runKSAccount(t, ks, cp)

	_, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeTrue(), "adopt takes over the pre-existing account without probing")
	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceUserProbeRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.User{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no probe is created under adopt")
}

func TestKSAccount_ExistingManagedUserSkipsTheProbe(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	_, c := runKSAccount(t, ks, cp, ksConvergedAccount(ks, cp)...)

	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceUserProbeRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.User{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a User we already own settles the verdict")
}

// --- effective identity values ---

func TestKSAccount_UserNameAndDomainDefaults(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Adopt = true // reach the managed User in one pass

	_, c := runKSAccount(t, ks, cp)

	user, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeTrue())
	g.Expect(string(*user.Spec.Resource.Name)).To(Equal(ksTestName),
		"an omitted userName falls back to the CR's own name")
	g.Expect(string(*user.Spec.Resource.DomainRef)).To(Equal(adminDomainRef(cp)),
		"an omitted domain reuses the ControlPlane's admin Domain import")

	// The admin domain import belongs to the ControlPlane; the registration must
	// not project one of its own for it.
	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceChildPrefix(ks) + "domain", Namespace: ks.Namespace}, &orcv1alpha1.Domain{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

// TestKSAccount_AdminIdentityIsRefused pins the one identity a registration may
// never resolve to. adopt=true short-circuits the collision probe, so the refusal
// has to sit ahead of it — otherwise the documented remediation for every other
// collision doubles as the switch that hands over the cloud admin account.
func TestKSAccount_AdminIdentityIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.UserName = adminUserName(cp)
	ks.Spec.Account.DomainName = adminDomainName(cp)
	ks.Spec.Account.Adopt = true

	cond, c := runKSAccount(t, ks, cp)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring("admin identity"))

	_, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeFalse(), "no managed User may be created over the admin identity")
	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceProjectRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.Project{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the refusal lands before any child is projected")
}

// TestKSAccount_AdminIdentityRefusalNamesAWorkingRemedy pins the remediation the
// refusal prescribes. account.userName and account.domainName are both immutable,
// so the message must not send the operator at an apply the CRD and the webhook
// reject — under Flux or Argo that rejection fails the whole kustomization.
func TestKSAccount_AdminIdentityRefusalNamesAWorkingRemedy(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.UserName = adminUserName(cp)
	ks.Spec.Account.DomainName = adminDomainName(cp)

	cond, _ := runKSAccount(t, ks, cp)

	g.Expect(cond.Message).To(ContainSubstring("immutable"))
	g.Expect(cond.Message).To(ContainSubstring("delete this KeystoneService and re-create it"))
	g.Expect(cond.Message).NotTo(ContainSubstring("rename account.userName"))
}

// ksLiveManagedUser is the managed K-ORC User a registration owns once
// ensureKeystoneServiceUser has run: the live child whose K-ORC finalizer deletes
// the Keystone user when the CR goes away.
func ksLiveManagedUser(ks *c5c3v1alpha1.KeystoneService) *orcv1alpha1.User {
	return &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:              keystoneServiceUserRef(ks),
			Namespace:         ks.Namespace,
			CreationTimestamp: metav1.Now(),
			OwnerReferences:   ownedByKS(ks),
		},
		Spec: orcv1alpha1.UserSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
	}
}

// TestKSAccount_AdminIdentityRefusalOnAnOwnedAccount covers the other way into the
// refusal: spec.korc.adminCredential is editable in place, so a ControlPlane edit
// can move the admin identity ONTO a user this registration already provisioned.
// Deleting the CR would then take the admin user out of Keystone with it, so the
// message has to point at the ControlPlane — and the pass must keep status.account
// naming the live resources the CR owns instead of blanking them.
func TestKSAccount_AdminIdentityRefusalOnAnOwnedAccount(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.UserName = adminUserName(cp)
	ks.Spec.Account.DomainName = adminDomainName(cp)
	ks.Status.Account = &c5c3v1alpha1.KeystoneServiceAccountStatus{
		SecretName: keystoneServiceCredentialsSecretName(ks),
		UserID:     "live-user-id",
		ProjectID:  "live-project-id",
	}

	cond, _ := runKSAccount(t, ks, cp, ksLiveManagedUser(ks))

	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring("spec.korc.adminCredential"))
	g.Expect(cond.Message).NotTo(ContainSubstring("delete this KeystoneService and re-create it"))

	g.Expect(ks.Status.Account).NotTo(BeNil())
	g.Expect(ks.Status.Account.UserID).To(Equal("live-user-id"),
		"a refusing pass must not hide which live Keystone user the CR owns")
	g.Expect(ks.Status.Account.ProjectID).To(Equal("live-project-id"))
}

// TestKSAccount_AdminIdentityRefusalReadsOwnershipOffTheLiveUser pins the signal
// the remedy branches on. The managed User is created a full pass before
// status.account.userID records its ID, and a refusing pass returns ahead of the
// step that would record it — so a CR that already owns the live user can sit at
// userID:"" forever. Reading the status projection instead of the child would
// prescribe deleting that CR, which takes the admin user out of Keystone with it.
func TestKSAccount_AdminIdentityRefusalReadsOwnershipOffTheLiveUser(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.UserName = adminUserName(cp)
	ks.Spec.Account.DomainName = adminDomainName(cp)
	// The child exists; the status projection has not caught up with it.
	g.Expect(ks.Status.Account).To(BeNil())

	cond, _ := runKSAccount(t, ks, cp, ksLiveManagedUser(ks))

	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring("spec.korc.adminCredential"))
	g.Expect(cond.Message).NotTo(ContainSubstring("delete this KeystoneService and re-create it"),
		"the CR owns the live user, so deleting it would delete the admin user from Keystone")
}

// TestKSAccount_AdminIdentityRefusalFoldsCase keeps the refusal in step with
// Keystone's own comparison: names live in a case-insensitive collation, so
// "Admin" in "default" is the very same identity as "admin" in "Default". An
// exact-bytes guard would let the case-variant spelling through — and with
// adopt=true there is no probe behind it to catch the takeover.
func TestKSAccount_AdminIdentityRefusalFoldsCase(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.UserName = strings.ToUpper(adminUserName(cp))
	ks.Spec.Account.DomainName = strings.ToLower(adminDomainName(cp))
	ks.Spec.Account.Adopt = true

	cond, c := runKSAccount(t, ks, cp)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	_, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeFalse(), "a case-variant spelling resolves to the admin identity too")
}

// TestKSAccount_AdminIdentityRefusalFoldsAccents keeps the refusal in step with
// the rest of Keystone's collation: its identity tables are *_general_ci, which
// folds accents as well as case, so "admín" resolves onto the very same row as
// "admin". strings.EqualFold folds only the case half, so an accented spelling
// would walk past the refusal — and with adopt=true there is no probe behind it.
func TestKSAccount_AdminIdentityRefusalFoldsAccents(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.UserName = "admín" // U+00ED, precomposed
	ks.Spec.Account.DomainName = "Défault"
	ks.Spec.Account.Adopt = true

	cond, c := runKSAccount(t, ks, cp)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	_, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeFalse(), "an accented spelling resolves to the admin identity too")
}

// TestKSAccount_AdminIdentityRefusalFoldsDecomposedAccents covers the same gap
// spelled with a combining mark rather than a precomposed rune: the two are
// distinct byte sequences that Keystone stores as the one "admin" row.
func TestKSAccount_AdminIdentityRefusalFoldsDecomposedAccents(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.UserName = "admi\u0301n" // "i" + COMBINING ACUTE ACCENT
	ks.Spec.Account.DomainName = adminDomainName(cp)
	ks.Spec.Account.Adopt = true

	cond, c := runKSAccount(t, ks, cp)

	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	_, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeFalse())
}

// TestKSAccount_UnaccentedTenantNameIsAdmitted keeps the accent fold from
// swallowing names that are genuinely a different Keystone row: folding is only
// ever a reason to REFUSE, never a reason to widen what a registration may own.
func TestKSAccount_UnaccentedTenantNameIsAdmitted(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.UserName = "glancé"
	ks.Spec.Account.DomainName = adminDomainName(cp)
	ks.Spec.Account.Adopt = true

	_, c := runKSAccount(t, ks, cp)

	_, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeTrue(), "an accented name that is not the admin name is a registration like any other")
}

// TestKSAccount_AdminUserNameInAnotherDomainIsAdmitted keeps the refusal keyed on
// the (user, domain) PAIR: Keystone user names are unique per domain, so the same
// name in a domain of its own is a different user and no takeover at all.
func TestKSAccount_AdminUserNameInAnotherDomainIsAdmitted(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.UserName = adminUserName(cp)
	ks.Spec.Account.DomainName = "services"
	ks.Spec.Account.Adopt = true

	_, c := runKSAccount(t, ks, cp)

	_, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeTrue(), "the admin user NAME in a tenant domain is a different Keystone user")
}

func TestKSAccount_ExplicitUserNameAndDomainProjectTheirOwnImport(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Adopt = true
	ks.Spec.Account.UserName = "glance"
	ks.Spec.Account.DomainName = "services"

	_, c := runKSAccount(t, ks, cp)

	user, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeTrue())
	g.Expect(string(*user.Spec.Resource.Name)).To(Equal("glance"))

	domain := &orcv1alpha1.Domain{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceChildPrefix(ks) + "domain", Namespace: ks.Namespace}, domain)).To(Succeed())
	g.Expect(domain.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(string(*domain.Spec.Import.Filter.Name)).To(Equal("services"))
}

// --- the password generations ---

func TestKSAccount_PasswordIsGeneratedOnceAndPreserved(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Adopt = true

	r, c, _ := ksAccountReconciler(t, ks, cp)
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)
	_, err := r.ensureAccount(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())

	first := &corev1.Secret{}
	key := types.NamespacedName{Name: keystoneServicePasswordSecretName(ks, 1), Namespace: ks.Namespace}
	g.Expect(c.Get(context.Background(), key, first)).To(Succeed())
	g.Expect(first.Data[serviceAccountPasswordKey]).NotTo(BeEmpty())

	// A fresh create stamps generation 1, so a later pass can tell the steady
	// state from a rotation nudge rather than re-deriving it every time.
	created, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeTrue())
	g.Expect(created.Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("1"))
	g.Expect(string(*created.Spec.Resource.PasswordRef)).To(Equal(keystoneServicePasswordSecretName(ks, 1)))

	// A second pass must not mint a new value: K-ORC detects a rotation by the
	// passwordRef NAME changing, so an in-place edit would silently desynchronise
	// the Secret from the password Keystone actually holds.
	_, err = r.ensureAccount(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())
	second := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), key, second)).To(Succeed())
	g.Expect(second.Data[serviceAccountPasswordKey]).To(Equal(first.Data[serviceAccountPasswordKey]))
}

func TestKSAccount_RotationBumpsTheGenerationAndFlipsThePasswordRef(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	seeded := ksConvergedAccount(ks, cp)
	// The CredentialRotation nudge: the generation annotation is cleared.
	for _, obj := range seeded {
		if user, ok := obj.(*orcv1alpha1.User); ok {
			user.Annotations[serviceAccountPasswordGenerationAnnotation] = ""
		}
	}

	_, c := runKSAccount(t, ks, cp, seeded...)

	user, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeTrue())
	g.Expect(string(*user.Spec.Resource.PasswordRef)).To(Equal(keystoneServicePasswordSecretName(ks, 2)),
		"the rotation is a passwordRef name flip, not a content edit")
	g.Expect(user.Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("2"))

	fresh := &corev1.Secret{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServicePasswordSecretName(ks, 2), Namespace: ks.Namespace}, fresh)).To(Succeed())
	g.Expect(fresh.Data[serviceAccountPasswordKey]).NotTo(BeEmpty())

	g.Expect(ks.Status.Account).NotTo(BeNil())
	g.Expect(ks.Status.Account.PasswordGeneration).To(Equal(int64(2)))
	g.Expect(ks.Status.Account.LastPasswordRotation).NotTo(BeNil())
}

func TestKSAccount_UnobservedRotationNudgeSurvivesForTheNextPass(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	seeded := ksConvergedAccount(ks, cp)
	for _, obj := range seeded {
		if user, ok := obj.(*orcv1alpha1.User); ok {
			user.Annotations[serviceAccountPasswordGenerationAnnotation] = ""
		}
	}

	s := korcTestScheme(t)
	all := append([]client.Object{cp, ks, readyTenantStoreFor(cp)}, seeded...)
	// The rotation request lands BETWEEN the read that decides whether to rotate
	// and the create-or-update read that writes. The managed User is read three
	// times per pass: by the collision gate, by the rotation decision, and by
	// CreateOrUpdate. Faking the SECOND read is what makes this pass blind to the
	// nudge while the live object already carries it.
	userGets := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(all...).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := cl.Get(ctx, key, obj, opts...)
				if user, ok := obj.(*orcv1alpha1.User); ok && key.Name == keystoneServiceUserRef(ks) && err == nil {
					userGets++
					if userGets == 2 {
						user.Annotations[serviceAccountPasswordGenerationAnnotation] = "1"
					}
				}
				return err
			},
		}).Build()
	r := &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)
	_, err := r.ensureAccount(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())

	user, ok := ksGetUser(t, c, keystoneServiceUserRef(ks), ks.Namespace)
	g.Expect(ok).To(BeTrue())
	g.Expect(user.Annotations[serviceAccountPasswordGenerationAnnotation]).To(BeEmpty(),
		"an unobserved nudge must be left in place so the next pass rotates")
	g.Expect(string(*user.Spec.Resource.PasswordRef)).To(Equal(keystoneServicePasswordSecretName(ks, 1)),
		"and this pass must not rotate on a request it did not see")
}

func TestKSAccount_SupersededPasswordSecretsArePrunedOnlyOnceApplied(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	// Generation 2 is declared, but K-ORC still reports generation 1 applied.
	seeded := ksConvergedAccount(ks, cp)
	for _, obj := range seeded {
		if user, ok := obj.(*orcv1alpha1.User); ok {
			user.Spec.Resource.PasswordRef = ptr.To(orcv1alpha1.KubernetesNameRef(keystoneServicePasswordSecretName(ks, 2)))
			user.Annotations[serviceAccountPasswordGenerationAnnotation] = "2"
		}
	}
	v2 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServicePasswordSecretName(ks, 2),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
		Data: map[string][]byte{serviceAccountPasswordKey: []byte("newer-password")},
	}

	_, c := runKSAccount(t, ks, cp, append(seeded, v2)...)

	// v1 is still the password Keystone holds, so it must survive.
	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServicePasswordSecretName(ks, 1), Namespace: ks.Namespace}, &corev1.Secret{})
	g.Expect(err).NotTo(HaveOccurred(),
		"a superseded generation is dropped only after K-ORC confirms the new one is applied")
}

// --- roles ---

func TestKSAccount_RolesProjectImportsAndAssignments(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Roles = []string{"member", "reader"}

	cond, c := runKSAccount(t, ks, cp, ksConvergedAccount(ks, cp)...)

	for _, role := range ks.Spec.Account.Roles {
		roleObj := &orcv1alpha1.Role{}
		g.Expect(c.Get(context.Background(),
			types.NamespacedName{Name: keystoneServiceRoleImportRef(ks, role), Namespace: ks.Namespace}, roleObj)).To(Succeed())
		g.Expect(roleObj.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
			"Keystone roles are global; the operator references them and never creates one")

		assignment := &orcv1alpha1.RoleAssignment{}
		g.Expect(c.Get(context.Background(),
			types.NamespacedName{Name: keystoneServiceRoleAssignmentRef(ks, role), Namespace: ks.Namespace}, assignment)).To(Succeed())
		g.Expect(assignment.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
		g.Expect(string(assignment.Spec.Resource.RoleRef)).To(Equal(keystoneServiceRoleImportRef(ks, role)))
		g.Expect(string(*assignment.Spec.Resource.UserRef)).To(Equal(keystoneServiceUserRef(ks)))
		g.Expect(string(*assignment.Spec.Resource.ProjectRef)).To(Equal(keystoneServiceProjectRef(ks)))
	}

	// The account is not provisioned until the roles it declares are bound.
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
}

func TestKSAccount_NoRolesProjectsNoRoleChildren(t *testing.T) {
	for _, tc := range []struct {
		name  string
		roles []string
	}{
		{name: "nil", roles: nil},
		{name: "empty", roles: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			cp := ksControlPlane()
			ks := ksWithAccount()
			ks.Spec.Account.Roles = tc.roles

			cond, c := runKSAccount(t, ks, cp, ksConvergedAccount(ks, cp)...)

			var roles orcv1alpha1.RoleList
			g.Expect(c.List(context.Background(), &roles, client.InNamespace(ks.Namespace))).To(Succeed())
			g.Expect(roles.Items).To(BeEmpty())
			var assignments orcv1alpha1.RoleAssignmentList
			g.Expect(c.List(context.Background(), &assignments, client.InNamespace(ks.Namespace))).To(Succeed())
			g.Expect(assignments.Items).To(BeEmpty())

			// An account without roles is still fully provisioned.
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceAccountProvisioned))
		})
	}
}

func TestKSAccount_StalledRoleImportHintsAtAMissingRole(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Roles = []string{"typo-role"}

	// The import has been waiting to be "created externally" past the grace window,
	// which for a role that is supposed to already exist means it does not.
	stalled := &orcv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceRoleImportRef(ks, "typo-role"), Namespace: ks.Namespace},
		Status:     orcv1alpha1.RoleStatus{Conditions: pendingImportConditions(2 * externalImportStallGrace)},
	}

	cond, _ := runKSAccount(t, ks, cp, append(ksConvergedAccount(ks, cp), stalled)...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
	g.Expect(cond.Message).To(ContainSubstring("may not exist in Keystone"))
	g.Expect(cond.Message).To(ContainSubstring("typo-role"))
}

func TestKSAccount_TerminalRoleErrorFailsLoudly(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Roles = []string{"member"}

	broken := &orcv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceRoleImportRef(ks, "member"), Namespace: ks.Namespace},
		Status:     orcv1alpha1.RoleStatus{Conditions: terminalImportConditions("filter matched more than one role")},
	}

	cond, _ := runKSAccount(t, ks, cp, append(ksConvergedAccount(ks, cp), broken)...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountsFailed))
	g.Expect(cond.Message).To(ContainSubstring("member"))
}

// --- the publish leg ---

func TestKSAccount_PublishAssemblesTheDocumentedContract(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	cond, c := runKSAccount(t, ks, cp, ksConvergedAccount(ks, cp)...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceAccountProvisioned))

	source := &corev1.Secret{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceSourceSecretName(ks), Namespace: ks.Namespace}, source)).To(Succeed())
	for _, key := range []string{
		"password", "username", "project_name", "user_domain_name",
		"project_domain_name", "auth_url", "region_name", appCredCloudsYAMLKey,
	} {
		g.Expect(source.Data).To(HaveKey(key))
	}
	g.Expect(string(source.Data["username"])).To(Equal(ksTestName))
	g.Expect(string(source.Data["project_name"])).To(Equal("service"))
	g.Expect(string(source.Data[appCredCloudsYAMLKey])).To(ContainSubstring("auth_url"))

	push := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServicePushSecretName(ks), Namespace: ks.Namespace}, push)).To(Succeed())
	g.Expect(push.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal(keystoneServiceRemoteKeyFor(ks)))
	g.Expect(push.Spec.DeletionPolicy).To(Equal(esov1alpha1.PushSecretDeletionPolicyDelete))
	g.Expect(push.Annotations).To(HaveKey(keystoneServicePushContentHashAnnotation))

	es := &esov1.ExternalSecret{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCredentialsSecretName(ks), Namespace: ks.Namespace}, es)).To(Succeed())
	g.Expect(es.Spec.Target.CreationPolicy).To(Equal(esov1.CreatePolicyOwner))
	g.Expect(es.Spec.Data).To(HaveLen(2))
	for _, d := range es.Spec.Data {
		g.Expect(d.RemoteRef.Key).To(Equal(keystoneServiceRemoteKeyFor(ks)))
	}

	g.Expect(ks.Status.Account.SecretName).To(Equal(keystoneServiceCredentialsSecretName(ks)))
	g.Expect(ks.Status.Account.UserID).To(Equal("ks-user-id"))
	g.Expect(ks.Status.Account.ProjectID).To(Equal("ks-project-id"))
	g.Expect(ks.Status.Account.PasswordGeneration).To(Equal(int64(1)))
}

func TestKSAccount_WaitsWhileTheMaterializedSecretIsAbsent(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	seeded := ksConvergedAccount(ks, cp)
	kept := seeded[:0]
	for _, obj := range seeded {
		if obj.GetName() == keystoneServiceCredentialsSecretName(ks) {
			continue // ESO has not materialized the consumer Secret yet
		}
		kept = append(kept, obj)
	}

	cond, _ := runKSAccount(t, ks, cp, kept...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
	g.Expect(cond.Message).To(ContainSubstring("OpenBao round-trip"))
}

func TestKSAccount_WaitsWhileTheMaterializedPasswordIsStale(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	seeded := ksConvergedAccount(ks, cp)
	for _, obj := range seeded {
		if secret, ok := obj.(*corev1.Secret); ok && secret.Name == keystoneServiceCredentialsSecretName(ks) {
			secret.Data[serviceAccountPasswordKey] = []byte("a-rotated-away-password")
		}
	}

	cond, _ := runKSAccount(t, ks, cp, seeded...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
		"a materialized password from a superseded generation must never read ready")
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
}

func TestKSAccount_WaitsWhileThePushSecretHasNotSynced(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	seeded := ksConvergedAccount(ks, cp)
	for _, obj := range seeded {
		if push, ok := obj.(*esov1alpha1.PushSecret); ok {
			push.Status.Conditions = nil
		}
	}

	cond, _ := runKSAccount(t, ks, cp, seeded...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
}

func TestKSAccount_MissingPasswordSecretAtPublishIsAWaitNotAnError(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	// The generation Secret is gone — deleted out of band between the pass that
	// created it and this one. Publishing must defer, not fail: the next pass
	// re-creates it.
	r, _, _ := ksAccountReconciler(t, ks, cp)

	published, err := r.publishKeystoneServiceAccount(context.Background(), ks, cp, "glance", "service", "Default", 7)

	g.Expect(err).NotTo(HaveOccurred(), "a missing generation Secret is a bounded wait, not an error")
	g.Expect(published).To(BeFalse())
}

func TestKSAccount_EmptyPasswordAtPublishIsAWaitNotAnError(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	// The Secret exists but its key is still empty: the normal transient of a
	// two-step create-then-populate flow.
	empty := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServicePasswordSecretName(ks, 7),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
		Data: map[string][]byte{serviceAccountPasswordKey: {}},
	}
	r, _, _ := ksAccountReconciler(t, ks, cp, empty)

	published, err := r.publishKeystoneServiceAccount(context.Background(), ks, cp, "glance", "service", "Default", 7)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(published).To(BeFalse())
}

// --- failure classification ---

func TestKSAccount_TerminalUserErrorFailsLoudly(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Adopt = true

	broken := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceUserRef(ks),
			Namespace:       ks.Namespace,
			Annotations:     map[string]string{serviceAccountPasswordGenerationAnnotation: "1"},
			OwnerReferences: ownedByKS(ks),
		},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyManaged,
			Resource: &orcv1alpha1.UserResourceSpec{
				PasswordRef: ptr.To(orcv1alpha1.KubernetesNameRef(keystoneServicePasswordSecretName(ks, 1))),
			},
		},
		Status: orcv1alpha1.UserStatus{Conditions: terminalImportConditions("invalid domain reference")},
	}

	cond, _ := runKSAccount(t, ks, cp, broken)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountsFailed))
	g.Expect(cond.Message).To(ContainSubstring("invalid domain reference"))
}

func TestKSAccount_ExternalModeClassifiesTheKORCMessage(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	cp.Spec.Services.Keystone.Mode = c5c3v1alpha1.KeystoneModeExternal
	cp.Spec.Services.Keystone.External = &c5c3v1alpha1.ExternalKeystoneSpec{
		AuthURL: "https://keystone.example.com/v3",
	}
	ks := ksWithAccount()
	ks.Spec.Account.Adopt = true

	// The project import cannot resolve because the credentials are rejected.
	project := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceProjectRef(ks), Namespace: ks.Namespace},
		Status: orcv1alpha1.ProjectStatus{
			Conditions: transientEntryConditions("Authentication failed: 401 Unauthorized"),
		},
	}

	cond, _ := runKSAccount(t, ks, cp, project)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonAuthenticationFailed),
		"the classified cause must beat the generic wait reason")
	g.Expect(cond.Message).To(ContainSubstring("https://keystone.example.com/v3"))
}

func TestKSAccount_KubernetesErrorIsReportedAndReturned(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()

	ks.Spec.Account.Adopt = true // reach the managed user in one pass

	s := korcTestScheme(t)
	boom := errors.New("secrets is forbidden")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ks, readyTenantStoreFor(cp)).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if secret, ok := obj.(*corev1.Secret); ok && secret.Name == keystoneServicePasswordSecretName(ks, 1) {
					return boom
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)

	_, err := r.ensureAccount(context.Background(), ks, cp, credRef, managedCredRef)

	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, boom)).To(BeTrue())
	cond := ksAccountCondition(ks)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountError))
}

// --- deferred scheduled rotation ---

func TestKSAccount_ScheduledRotationIsDeferredWithAnEvent(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithAccount()
	ks.Spec.Account.Adopt = true
	ks.Spec.Account.Rotation = &c5c3v1alpha1.ServiceAccountRotationSpec{
		Mode: c5c3v1alpha1.ServiceAccountRotationModeScheduled,
	}

	r, _, recorder := ksAccountReconciler(t, ks, cp)
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)
	_, err := r.ensureAccount(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())

	var event string
	select {
	case event = <-recorder.Events:
	default:
	}
	g.Expect(event).To(ContainSubstring("ScheduledRotationDeferred"),
		"the deferral must be visible; a silently ignored rotation policy reads as a working one")
}
