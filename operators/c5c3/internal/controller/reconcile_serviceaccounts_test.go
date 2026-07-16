// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the declarative service-account sub-reconciler
// reconcileServiceAccounts (spec.korc.serviceAccounts).
package controller

import (
	"context"
	"regexp"
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

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// saControlPlane returns a managed ControlPlane with AdminCredentialReady already
// satisfied and one declared service account, so reconcileServiceAccounts runs
// past its gates. Tests mutate the single declared account as needed.
func saControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := korcControlPlane()
	setAdminCredentialReady(cp)
	cp.Spec.KORC.ServiceAccounts = []c5c3v1alpha1.ServiceAccountSpec{{
		Name:    "nova",
		Project: c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service"},
	}}
	return cp
}

// runServiceAccounts runs reconcileServiceAccounts against cp with the given
// seeded objects (plus a ready ClusterSecretStore) and returns the resulting
// ServiceAccountsReady condition and the client.
func runServiceAccounts(t *testing.T, cp *c5c3v1alpha1.ControlPlane, objs ...client.Object) (*metav1.Condition, client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	all := append([]client.Object{cp, readyClusterSecretStore(), readyTenantStoreFor(cp)}, objs...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(all...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	_, err := r.reconcileServiceAccounts(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	return conditions.GetCondition(cp.Status.Conditions, conditionTypeServiceAccountsReady), c
}

// ownedByCP hand-builds a controller OwnerReference to cp so metav1.IsControlledBy
// recognises a seeded child as swept by the prune sweep.
func ownedByCP(cp *c5c3v1alpha1.ControlPlane) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion:         c5c3v1alpha1.GroupVersion.String(),
		Kind:               "ControlPlane",
		Name:               cp.Name,
		UID:                cp.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
}

func getUserByName(t *testing.T, c client.Client, name, ns string) (*orcv1alpha1.User, bool) {
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

// saUserProjectAppliedV1 seeds the managed User (Available, generation-1 password
// applied), the referenced Project import (Available), and the generation-1
// password Secret, so a reconcile reaches the role-projection step for sa.
func saUserProjectAppliedV1(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec) []client.Object {
	ns := childNamespace(cp)
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:            serviceAccountUserRef(cp, sa),
			Namespace:       ns,
			Annotations:     map[string]string{serviceAccountPasswordGenerationAnnotation: "1"},
			OwnerReferences: ownedByCP(cp),
		},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyManaged,
			Resource: &orcv1alpha1.UserResourceSpec{
				PasswordRef: ptr.To(orcv1alpha1.KubernetesNameRef(serviceAccountPasswordSecretName(cp, sa, 1))),
			},
		},
		Status: orcv1alpha1.UserStatus{
			Conditions: availableImportConditions(),
			ID:         ptr.To("sa-user-id"),
			Resource:   &orcv1alpha1.UserResourceStatus{AppliedPasswordRef: serviceAccountPasswordSecretName(cp, sa, 1)},
		},
	}
	project := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountProjectRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.ProjectStatus{Conditions: availableImportConditions(), ID: ptr.To("sa-project-id")},
	}
	pw := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountPasswordSecretName(cp, sa, 1), Namespace: ns, OwnerReferences: ownedByCP(cp)},
		Data:       map[string][]byte{serviceAccountPasswordKey: []byte("current-pw")},
	}
	return []client.Object{user, project, pw}
}

func TestReconcileServiceAccounts_EmptyListReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	setAdminCredentialReady(cp)

	cond, _ := runServiceAccounts(t, cp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonNoServiceAccountsDeclared))
}

func TestReconcileServiceAccounts_WaitingForAdminCredential(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	// Drop the gate condition.
	cp.Status.Conditions = nil

	cond, _ := runServiceAccounts(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccountAdmin))
}

func TestReconcileServiceAccounts_SecretStoreNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	s := korcTestScheme(t)
	// No ClusterSecretStore seeded => IsClusterSecretStoreReady reports not ready.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	_, err := r.reconcileServiceAccounts(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeServiceAccountsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountStoreNotReady))
}

func TestReconcileServiceAccounts_ProbeAbsentCreatesManagedUserAndReferencedProject(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)
	// Seed the user probe reporting the user does NOT exist yet (pending-external).
	probe := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountUserProbeRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.UserStatus{Conditions: pendingImportConditions(0)},
	}

	cond, c := runServiceAccounts(t, cp, probe)

	// The probe is dropped once it reports absent.
	_, probeExists := getUserByName(t, c, serviceAccountUserProbeRef(cp, sa), ns)
	g.Expect(probeExists).To(BeFalse(), "the resolved probe must be deleted")

	// The managed User is created with generation-1 password and annotation.
	user, ok := getUserByName(t, c, serviceAccountUserRef(cp, sa), ns)
	g.Expect(ok).To(BeTrue())
	g.Expect(user.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(user.Spec.Resource).NotTo(BeNil())
	g.Expect(user.Spec.Resource.PasswordRef).NotTo(BeNil())
	g.Expect(string(*user.Spec.Resource.PasswordRef)).To(Equal(serviceAccountPasswordSecretName(cp, sa, 1)))
	g.Expect(user.Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("1"))

	pw := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountPasswordSecretName(cp, sa, 1), Namespace: ns}, pw)).To(Succeed())
	g.Expect(pw.Data[serviceAccountPasswordKey]).NotTo(BeEmpty())

	// The referenced project is an unmanaged import.
	proj := &orcv1alpha1.Project{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountProjectRef(cp, sa), Namespace: ns}, proj)).To(Succeed())
	g.Expect(proj.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(proj.Spec.Import).NotTo(BeNil())

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
}

// TestReconcileServiceAccounts_ConvergedAccountReportsReadyStatus drives the
// fully-converged pass: K-ORC has the user Available with the current password
// applied, the project import resolved, the PushSecret synced, and the
// materialized consumer Secret carries the current password. The condition must
// flip True/ServiceAccountsProvisioned AND status.serviceAccounts[].ready must
// report true in the same pass — the e2e suite reads both back-to-back, so a
// condition that flips True while the per-account ready flag stays false is the
// exact regression this guards against.
func TestReconcileServiceAccounts_ConvergedAccountReportsReadyStatus(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	// A declared role must be Available too for the account to converge: its
	// readiness is folded into the per-account gate.
	cp.Spec.KORC.ServiceAccounts[0].Roles = []string{"member"}
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	roleImport := &orcv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountRoleImportRef(cp, "member"), Namespace: ns},
		Status:     orcv1alpha1.RoleStatus{Conditions: availableImportConditions(), ID: ptr.To("member-role-id")},
	}
	roleAssignment := &orcv1alpha1.RoleAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountRoleAssignmentRef(cp, sa, "member"), Namespace: ns},
		Status:     orcv1alpha1.RoleAssignmentStatus{Conditions: availableImportConditions()},
	}

	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:            serviceAccountUserRef(cp, sa),
			Namespace:       ns,
			Annotations:     map[string]string{serviceAccountPasswordGenerationAnnotation: "1"},
			OwnerReferences: ownedByCP(cp),
		},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyManaged,
			Resource: &orcv1alpha1.UserResourceSpec{
				PasswordRef: ptr.To(orcv1alpha1.KubernetesNameRef(serviceAccountPasswordSecretName(cp, sa, 1))),
			},
		},
		Status: orcv1alpha1.UserStatus{
			Conditions: availableImportConditions(),
			ID:         ptr.To("sa-user-id"),
			Resource:   &orcv1alpha1.UserResourceStatus{AppliedPasswordRef: serviceAccountPasswordSecretName(cp, sa, 1)},
		},
	}
	project := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountProjectRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.ProjectStatus{Conditions: availableImportConditions(), ID: ptr.To("sa-project-id")},
	}
	pw := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountPasswordSecretName(cp, sa, 1), Namespace: ns, OwnerReferences: ownedByCP(cp)},
		Data:       map[string][]byte{serviceAccountPasswordKey: []byte("current-pw")},
	}
	push := serviceAccountPushSecret(cp, sa)
	push.OwnerReferences = ownedByCP(cp)
	push.Status.Conditions = []esov1alpha1.PushSecretStatusCondition{
		{Type: esov1alpha1.PushSecretReady, Status: corev1.ConditionTrue},
	}
	push.Status.SyncedResourceVersion = testPushSyncedRV
	materialized := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountCredentialsSecretName(cp, sa), Namespace: ns},
		Data:       map[string][]byte{serviceAccountPasswordKey: []byte("current-pw")},
	}

	cond, _ := runServiceAccounts(t, cp, user, project, pw, push, materialized, roleImport, roleAssignment)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountsProvisioned))
	g.Expect(cp.Status.ServiceAccounts).To(HaveLen(1))
	got := cp.Status.ServiceAccounts[0]
	g.Expect(got.Ready).To(BeTrue(),
		"status.serviceAccounts[].ready must be true in the same pass the condition reports ServiceAccountsProvisioned")
	g.Expect(got.UserID).To(Equal("sa-user-id"))
	g.Expect(got.ProjectID).To(Equal("sa-project-id"))
	g.Expect(got.PasswordGeneration).To(Equal(int64(1)))
	g.Expect(got.SecretName).To(Equal(serviceAccountCredentialsSecretName(cp, sa)))
}

func TestReconcileServiceAccounts_CollisionFailsLoudly(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)
	// Seed the user probe RESOLVED — a user of that name already exists.
	probe := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountUserProbeRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.UserStatus{Conditions: availableImportConditions()},
	}

	cond, c := runServiceAccounts(t, cp, probe)

	// The managed User must NOT be created — the operator fails loudly.
	_, ok := getUserByName(t, c, serviceAccountUserRef(cp, sa), ns)
	g.Expect(ok).To(BeFalse())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring("adopt=true"))
	g.Expect(cond.Message).To(ContainSubstring("nova"))
}

func TestReconcileServiceAccounts_AdoptSkipsProbe(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Adopt = true
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	_, c := runServiceAccounts(t, cp)

	// adopt=true skips the probe and creates the managed User directly.
	_, probeExists := getUserByName(t, c, serviceAccountUserProbeRef(cp, sa), ns)
	g.Expect(probeExists).To(BeFalse(), "no probe is created when adopt=true")
	user, ok := getUserByName(t, c, serviceAccountUserRef(cp, sa), ns)
	g.Expect(ok).To(BeTrue())
	g.Expect(user.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
}

func TestReconcileServiceAccounts_RotationFlipsPasswordRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	// A managed User at generation 1 whose generation annotation was CLEARED by the
	// CredentialRotation reconciler — the rotation nudge.
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceAccountUserRef(cp, sa),
			Namespace:   ns,
			Annotations: map[string]string{serviceAccountPasswordGenerationAnnotation: ""},
		},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyManaged,
			Resource: &orcv1alpha1.UserResourceSpec{
				PasswordRef: ptr.To(orcv1alpha1.KubernetesNameRef(serviceAccountPasswordSecretName(cp, sa, 1))),
			},
		},
		Status: orcv1alpha1.UserStatus{
			Conditions: availableImportConditions(),
			Resource:   &orcv1alpha1.UserResourceStatus{AppliedPasswordRef: serviceAccountPasswordSecretName(cp, sa, 1)},
		},
	}
	pwV1 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountPasswordSecretName(cp, sa, 1), Namespace: ns, OwnerReferences: ownedByCP(cp)},
		Data:       map[string][]byte{serviceAccountPasswordKey: []byte("old-pw")},
	}

	_, c := runServiceAccounts(t, cp, user, pwV1)

	// The passwordRef flips to generation 2 and the annotation is restamped.
	updated, ok := getUserByName(t, c, serviceAccountUserRef(cp, sa), ns)
	g.Expect(ok).To(BeTrue())
	g.Expect(string(*updated.Spec.Resource.PasswordRef)).To(Equal(serviceAccountPasswordSecretName(cp, sa, 2)))
	g.Expect(updated.Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("2"))

	pwV2 := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountPasswordSecretName(cp, sa, 2), Namespace: ns}, pwV2)).To(Succeed())
	g.Expect(pwV2.Data[serviceAccountPasswordKey]).NotTo(BeEmpty())

	// The old generation is NOT deleted until K-ORC applies the new one.
	pwV1After := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountPasswordSecretName(cp, sa, 1), Namespace: ns}, pwV1After)).To(Succeed())

	g.Expect(cp.Status.ServiceAccounts).To(HaveLen(1))
	g.Expect(cp.Status.ServiceAccounts[0].PasswordGeneration).To(Equal(int64(2)))
	g.Expect(cp.Status.ServiceAccounts[0].LastPasswordRotation).NotTo(BeNil())
}

// TestReconcileServiceAccounts_RotationPrunesSupersededPasswordOnceApplied covers
// the applied==true branch of ensureServiceAccountUser: once K-ORC confirms the
// current generation is applied, the superseded generation's password Secret is
// garbage-collected while the current one survives (the sibling
// TestReconcileServiceAccounts_RotationFlipsPasswordRef only proves the old Secret
// is NOT deleted while the new one is still pending).
func TestReconcileServiceAccounts_RotationPrunesSupersededPasswordOnceApplied(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	// The rotation has completed: the managed User is at generation 2, K-ORC has
	// APPLIED v2, and both the superseded v1 and the current v2 Secret still exist.
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceAccountUserRef(cp, sa),
			Namespace:   ns,
			Annotations: map[string]string{serviceAccountPasswordGenerationAnnotation: "2"},
		},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyManaged,
			Resource: &orcv1alpha1.UserResourceSpec{
				PasswordRef: ptr.To(orcv1alpha1.KubernetesNameRef(serviceAccountPasswordSecretName(cp, sa, 2))),
			},
		},
		Status: orcv1alpha1.UserStatus{
			Conditions: availableImportConditions(),
			Resource:   &orcv1alpha1.UserResourceStatus{AppliedPasswordRef: serviceAccountPasswordSecretName(cp, sa, 2)},
		},
	}
	pwV1 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountPasswordSecretName(cp, sa, 1), Namespace: ns, OwnerReferences: ownedByCP(cp)},
		Data:       map[string][]byte{serviceAccountPasswordKey: []byte("old-pw")},
	}
	pwV2 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountPasswordSecretName(cp, sa, 2), Namespace: ns, OwnerReferences: ownedByCP(cp)},
		Data:       map[string][]byte{serviceAccountPasswordKey: []byte("new-pw")},
	}

	_, c := runServiceAccounts(t, cp, user, pwV1, pwV2)

	// The superseded v1 Secret is deleted once K-ORC applied v2.
	v1 := &corev1.Secret{}
	err := c.Get(context.Background(), types.NamespacedName{Name: serviceAccountPasswordSecretName(cp, sa, 1), Namespace: ns}, v1)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the superseded v1 password Secret must be pruned once v2 is applied")

	// The current v2 Secret survives — it is the one the managed User references.
	v2 := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountPasswordSecretName(cp, sa, 2), Namespace: ns}, v2)).To(Succeed())
}

// TestReconcileServiceAccounts_SupersededPruneBoundedToExistingSecrets is the
// regression guard for the bounded superseded-password sweep: on a steady-state
// pass over a long-lived account whose superseded generations were pruned long ago,
// the reconciler must NOT issue a DELETE per already-gone generation. The blind
// v1..v(gen-1) loop this replaced fired one NotFound DELETE per past generation on
// every reconcile, growing unbounded with the account's rotation history.
func TestReconcileServiceAccounts_SupersededPruneBoundedToExistingSecrets(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	// An account rotated to generation 5 whose superseded v1..v4 Secrets were pruned
	// generations ago: only the current v5 Secret remains.
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceAccountUserRef(cp, sa),
			Namespace:   ns,
			Annotations: map[string]string{serviceAccountPasswordGenerationAnnotation: "5"},
		},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyManaged,
			Resource: &orcv1alpha1.UserResourceSpec{
				PasswordRef: ptr.To(orcv1alpha1.KubernetesNameRef(serviceAccountPasswordSecretName(cp, sa, 5))),
			},
		},
		Status: orcv1alpha1.UserStatus{
			Conditions: availableImportConditions(),
			Resource:   &orcv1alpha1.UserResourceStatus{AppliedPasswordRef: serviceAccountPasswordSecretName(cp, sa, 5)},
		},
	}
	pwV5 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountPasswordSecretName(cp, sa, 5), Namespace: ns, OwnerReferences: ownedByCP(cp)},
		Data:       map[string][]byte{serviceAccountPasswordKey: []byte("current-pw")},
	}

	var secretDeletes int
	s := korcTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, readyClusterSecretStore(), readyTenantStoreFor(cp), user, pwV5).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					secretDeletes++
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	_, err := r.reconcileServiceAccounts(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(secretDeletes).To(Equal(0),
		"a steady-state reconcile must not issue DELETE calls for already-pruned password generations")
}

// TestReconcileServiceAccounts_CustomDomainCreatesUnmanagedImport covers the
// non-admin branch of ensureServiceAccountDomain: when an account's effective
// domain differs from the admin domain, a per-account unmanaged Domain import is
// created (a CR-only handle; the external domain is referenced, never created or
// deleted) and the teardown sweep names it so it is cleaned up on delete.
func TestReconcileServiceAccounts_CustomDomainCreatesUnmanagedImport(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].DomainName = "custom-service-domain"
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	g.Expect(serviceAccountDomainRef(cp, sa)).NotTo(Equal(adminDomainRef(cp)),
		"a non-admin domain must resolve to a distinct per-account Domain import")

	_, c := runServiceAccounts(t, cp)

	domain := &orcv1alpha1.Domain{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountDomainRef(cp, sa), Namespace: ns}, domain)).To(Succeed())
	g.Expect(domain.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(domain.Spec.Import).NotTo(BeNil())

	// The teardown sweep names the distinct per-account domain so it is torn down.
	names := make([]string, 0)
	for _, child := range orcChildObjects(cp) {
		names = append(names, child.name)
	}
	g.Expect(names).To(ContainElement(serviceAccountDomainRef(cp, sa)))
}

func TestReconcileServiceAccounts_ManagedProjectProbeAbsentCreatesManagedProject(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Project = c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service", Create: true}
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)
	// Both probes report absent so the managed Project and User are created.
	projectProbe := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountProjectProbeRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.ProjectStatus{Conditions: pendingImportConditions(0)},
	}
	userProbe := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountUserProbeRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.UserStatus{Conditions: pendingImportConditions(0)},
	}

	_, c := runServiceAccounts(t, cp, projectProbe, userProbe)

	proj := &orcv1alpha1.Project{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountProjectRef(cp, sa), Namespace: ns}, proj)).To(Succeed())
	g.Expect(proj.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(proj.Spec.Resource).NotTo(BeNil())
	g.Expect(string(*proj.Spec.Resource.Name)).To(Equal("service"))
}

func TestReconcileServiceAccounts_ManagedProjectCollisionFailsLoudly(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Project = c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service", Create: true}
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)
	// The project probe RESOLVES — a project of that name already exists.
	projectProbe := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountProjectProbeRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.ProjectStatus{Conditions: availableImportConditions()},
	}

	cond, c := runServiceAccounts(t, cp, projectProbe)

	// The managed Project must NOT be created.
	managed := &orcv1alpha1.Project{}
	err := c.Get(context.Background(), types.NamespacedName{Name: serviceAccountProjectRef(cp, sa), Namespace: ns}, managed)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring("project.create=false"))
}

func TestReconcileServiceAccounts_PrunesUndeclaredChild(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	setAdminCredentialReady(cp)
	// No declared accounts, but a leftover managed User the operator owns.
	ns := childNamespace(cp)
	leftover := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:            serviceAccountChildPrefix(cp) + "user-old",
			Namespace:       ns,
			OwnerReferences: ownedByCP(cp),
		},
		Spec: orcv1alpha1.UserSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
	}

	cond, c := runServiceAccounts(t, cp, leftover)

	_, ok := getUserByName(t, c, serviceAccountChildPrefix(cp)+"user-old", ns)
	g.Expect(ok).To(BeFalse(), "the undeclared owned User must be pruned")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
}

// TestReconcileServiceAccounts_ProjectsRoleImportAndAssignment covers the role
// projection: a declared role creates the unmanaged Role import and the managed
// RoleAssignment with the expected names, refs, policies, and credentials refs, and
// the account stays not-ready while the freshly-created assignment is still pending.
func TestReconcileServiceAccounts_ProjectsRoleImportAndAssignment(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Roles = []string{"member"}
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	cond, c := runServiceAccounts(t, cp, saUserProjectAppliedV1(cp, sa)...)

	roleImport := &orcv1alpha1.Role{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: serviceAccountRoleImportRef(cp, "member"), Namespace: ns}, roleImport)).To(Succeed())
	g.Expect(roleImport.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(roleImport.Spec.Import).NotTo(BeNil())
	g.Expect(roleImport.Spec.Import.Filter).NotTo(BeNil())
	g.Expect(roleImport.Spec.Import.Filter.Name).NotTo(BeNil())
	g.Expect(string(*roleImport.Spec.Import.Filter.Name)).To(Equal("member"))
	g.Expect(roleImport.Spec.Import.Filter.DomainRef).To(BeNil(),
		"a role import must carry no DomainRef (Keystone roles are global)")
	g.Expect(roleImport.Spec.CloudCredentialsRef.SecretName).To(Equal("k-orc-clouds-yaml"),
		"the Role import rides the spec clouds.yaml, like the Domain/Project imports")

	assignment := &orcv1alpha1.RoleAssignment{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: serviceAccountRoleAssignmentRef(cp, sa, "member"), Namespace: ns}, assignment)).To(Succeed())
	g.Expect(assignment.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(assignment.Spec.Resource).NotTo(BeNil())
	g.Expect(string(assignment.Spec.Resource.RoleRef)).To(Equal(serviceAccountRoleImportRef(cp, "member")))
	g.Expect(assignment.Spec.Resource.UserRef).NotTo(BeNil())
	g.Expect(string(*assignment.Spec.Resource.UserRef)).To(Equal(serviceAccountUserRef(cp, sa)))
	g.Expect(assignment.Spec.Resource.ProjectRef).NotTo(BeNil())
	g.Expect(string(*assignment.Spec.Resource.ProjectRef)).To(Equal(serviceAccountProjectRef(cp, sa)))
	g.Expect(assignment.Spec.CloudCredentialsRef.SecretName).To(Equal(adminPasswordCloudSecretName(cp)),
		"the managed RoleAssignment authenticates via the admin-password cloud so teardown survives the AC revoke")

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
	g.Expect(cp.Status.ServiceAccounts).To(HaveLen(1))
	g.Expect(cp.Status.ServiceAccounts[0].Ready).To(BeFalse(),
		"the account must stay not-ready while its RoleAssignment is pending")
}

// TestReconcileServiceAccounts_SharedRoleProjectsOneImport pins the Role import's
// keying: the import filters on the role name alone and rides the one credRef every
// account shares, so it carries nothing account-specific and accounts declaring the
// same role MUST resolve the SAME import. Keying it per account would mint
// byte-identical duplicates — K-ORC would poll one Keystone role lookup per account
// and wake the ControlPlane on every duplicate's status write. Their
// RoleAssignments stay per-account: those bind a specific user to a project.
func TestReconcileServiceAccounts_SharedRoleProjectsOneImport(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Roles = []string{"member"}
	cp.Spec.KORC.ServiceAccounts = append(cp.Spec.KORC.ServiceAccounts, c5c3v1alpha1.ServiceAccountSpec{
		Name:    "glance",
		Project: c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service"},
		Roles:   []string{"member"},
	})
	nova, glance := cp.Spec.KORC.ServiceAccounts[0], cp.Spec.KORC.ServiceAccounts[1]

	g.Expect(serviceAccountRoleImportRef(cp, "member")).To(Equal(serviceAccountRoleImportRef(cp, "member")),
		"the Role import name must not depend on the account")
	g.Expect(serviceAccountRoleAssignmentRef(cp, nova, "member")).
		NotTo(Equal(serviceAccountRoleAssignmentRef(cp, glance, "member")),
			"RoleAssignments bind a specific user/project, so they stay per-account")

	seeded := append(saUserProjectAppliedV1(cp, nova), saUserProjectAppliedV1(cp, glance)...)
	_, c := runServiceAccounts(t, cp, seeded...)

	roles := &orcv1alpha1.RoleList{}
	g.Expect(c.List(context.Background(), roles, client.InNamespace(childNamespace(cp)))).To(Succeed())
	g.Expect(roles.Items).To(HaveLen(1),
		"two accounts declaring the same role must project ONE shared Role import")
	g.Expect(roles.Items[0].Name).To(Equal(serviceAccountRoleImportRef(cp, "member")))

	assignments := &orcv1alpha1.RoleAssignmentList{}
	g.Expect(c.List(context.Background(), assignments, client.InNamespace(childNamespace(cp)))).To(Succeed())
	g.Expect(assignments.Items).To(HaveLen(2),
		"each account still gets its own RoleAssignment against the shared import")
	for i := range assignments.Items {
		g.Expect(string(assignments.Items[i].Spec.Resource.RoleRef)).To(Equal(serviceAccountRoleImportRef(cp, "member")))
	}

	// Both accounts keep the shared import in the prune keep-set, so dropping one
	// account cannot prune the role the other still declares.
	g.Expect(serviceAccountDeclaredChildNames(cp, cp.Spec.KORC.ServiceAccounts)).
		To(HaveKey(serviceAccountRoleImportRef(cp, "member")))
}

// TestReconcileServiceAccounts_RoleAssignmentTerminalErrorFailsLoudly pins the
// terminal-error precedence: a terminal K-ORC failure on the managed RoleAssignment
// surfaces ServiceAccountsReady=False/ServiceAccountsFailed naming the account and role.
func TestReconcileServiceAccounts_RoleAssignmentTerminalErrorFailsLoudly(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Roles = []string{"member"}
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	roleImport := &orcv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountRoleImportRef(cp, "member"), Namespace: ns},
		Status:     orcv1alpha1.RoleStatus{Conditions: availableImportConditions()},
	}
	assignment := &orcv1alpha1.RoleAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountRoleAssignmentRef(cp, sa, "member"), Namespace: ns},
		Status:     orcv1alpha1.RoleAssignmentStatus{Conditions: terminalImportConditions("role assignment rejected")},
	}
	seed := append(saUserProjectAppliedV1(cp, sa), roleImport, assignment)

	cond, _ := runServiceAccounts(t, cp, seed...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountsFailed))
	g.Expect(cond.Message).To(ContainSubstring("nova"))
	g.Expect(cond.Message).To(ContainSubstring("member"))
}

// TestReconcileServiceAccounts_PrunesRemovedRoleChildren covers the prune sweep for
// role children: an owned Role import and RoleAssignment left over from a role the
// spec no longer declares are both deleted.
func TestReconcileServiceAccounts_PrunesRemovedRoleChildren(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane() // account "nova" declares NO roles
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	roleImport := &orcv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name: serviceAccountRoleImportRef(cp, "member"), Namespace: ns, OwnerReferences: ownedByCP(cp),
		},
		Spec: orcv1alpha1.RoleSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	assignment := &orcv1alpha1.RoleAssignment{
		ObjectMeta: metav1.ObjectMeta{
			Name: serviceAccountRoleAssignmentRef(cp, sa, "member"), Namespace: ns, OwnerReferences: ownedByCP(cp),
		},
		Spec: orcv1alpha1.RoleAssignmentSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
	}

	_, c := runServiceAccounts(t, cp, roleImport, assignment)

	ri := &orcv1alpha1.Role{}
	g.Expect(apierrors.IsNotFound(c.Get(context.Background(),
		types.NamespacedName{Name: serviceAccountRoleImportRef(cp, "member"), Namespace: ns}, ri))).To(BeTrue(),
		"the removed role's Role import must be pruned")
	ra := &orcv1alpha1.RoleAssignment{}
	g.Expect(apierrors.IsNotFound(c.Get(context.Background(),
		types.NamespacedName{Name: serviceAccountRoleAssignmentRef(cp, sa, "member"), Namespace: ns}, ra))).To(BeTrue(),
		"the removed role's RoleAssignment must be pruned")
}

// TestReconcileServiceAccounts_NoRoleAssignmentsDeferredEvent guards that the old
// one-shot deferral event is gone: a creation pass over an account WITH a declared
// role must not emit RoleAssignmentsDeferred (role projection is real now).
func TestReconcileServiceAccounts_NoRoleAssignmentsDeferredEvent(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Roles = []string{"member"}
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	// The user probe reports absent, so the managed User is created this pass
	// (st.created=true) — the old trigger for the deferral event.
	probe := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountUserProbeRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.UserStatus{Conditions: pendingImportConditions(0)},
	}

	s := korcTestScheme(t)
	rec := record.NewFakeRecorder(20)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, readyClusterSecretStore(), readyTenantStoreFor(cp), probe).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}
	_, err := r.reconcileServiceAccounts(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	_, ok := getUserByName(t, c, serviceAccountUserRef(cp, sa), ns)
	g.Expect(ok).To(BeTrue(), "the managed User must be created this pass")

	close(rec.Events)
	for ev := range rec.Events {
		g.Expect(ev).NotTo(ContainSubstring("RoleAssignmentsDeferred"),
			"role projection is real now; the deferral event must never fire")
	}
}

// TestServiceAccountRoleSlug covers the slug normalization and its case-sensitive
// collision resistance.
func TestServiceAccountRoleSlug(t *testing.T) {
	// The readable base plus an 8-hex suffix, at most 25 bytes; the base may be
	// empty for an all-non-alnum role, leaving just "-<hash>".
	shape := regexp.MustCompile(`^[a-z0-9-]{0,16}-[0-9a-f]{8}$`)

	cases := []struct {
		name       string
		role       string
		wantPrefix string
	}{
		{"mixed case lowercases", "Member", "member-"},
		{"unicode and spaces collapse to dashes", "Über Admin", "ber-admin-"},
		{"long base truncates to 16", "verylongrolenamethatexceeds", "verylongrolename-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			slug := serviceAccountRoleSlug(tc.role)
			g.Expect(len(slug)).To(BeNumerically("<=", 25))
			g.Expect(shape.MatchString(slug)).To(BeTrue(), "slug %q must be a name-safe segment", slug)
			g.Expect(slug).To(HavePrefix(tc.wantPrefix))
		})
	}

	g := NewGomegaWithT(t)
	// Two roles differing only by case must not collide: the hash is taken over the
	// ORIGINAL (case-sensitive) role string, so the suffixes differ.
	g.Expect(serviceAccountRoleSlug("Member")).NotTo(Equal(serviceAccountRoleSlug("member")),
		"case-only-different roles must hash to distinct slugs")
}

func TestBuildServiceAccountCloudsYAML_UsesAccountIdentity(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()

	out := buildServiceAccountCloudsYAML(cp, "nova", "service", "Default", "s3cret")
	g.Expect(out).To(ContainSubstring("username: \"nova\""))
	g.Expect(out).To(ContainSubstring("password: \"s3cret\""))
	g.Expect(out).To(ContainSubstring("project_name: \"service\""))
	g.Expect(out).To(ContainSubstring("user_domain_name: \"Default\""))
	g.Expect(out).To(ContainSubstring("project_domain_name: \"Default\""))
}

// TestServiceAccountRemoteKeyFor_DefaultAndTargetNamespace pins the OpenBao path
// scoping: the default (no targetNamespace) key stays BIT-FOR-BIT the
// ControlPlane-namespace-scoped path, and a targetNamespace re-keys only the
// namespace segment (following the consumer, per the admin-password precedent).
func TestServiceAccountRemoteKeyFor_DefaultAndTargetNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()

	def := cp.Spec.KORC.ServiceAccounts[0]
	g.Expect(serviceAccountRemoteKeyFor(cp, def)).To(Equal("openstack/keystone/default/cp/service-accounts/nova"),
		"the default remote key must be bit-for-bit the ControlPlane-namespace-scoped path")

	// The target rides saTargetControlPlane, which DECLARES "identity" as the
	// dedicated keystone namespace: the remote key follows the delivery namespace,
	// and serviceAccountDeliveryNamespace only honours a target the ControlPlane
	// actually dedicates (see TestServiceAccountDeliveryNamespace_RejectsOutOfScopeTarget).
	targetedCP, targetNS := saTargetControlPlane()
	targeted := targetedCP.Spec.KORC.ServiceAccounts[0]
	g.Expect(serviceAccountRemoteKeyFor(targetedCP, targeted)).
		To(Equal("openstack/keystone/"+targetNS+"/cp/service-accounts/nova"),
			"a targetNamespace must move only the namespace segment of the remote key")
}

// TestServiceAccountDeliveryNamespace_RejectsOutOfScopeTarget pins the reconciler's
// own re-enforcement of the webhook's targetNamespace rule. The webhook constrains
// the target to the ControlPlane's own namespace or one of its dedicated service
// namespaces, but admission can be ABSENT rather than merely unavailable (the
// ValidatingWebhookConfiguration not yet registered during install, a GitOps/etcd
// restore replaying stored objects), which failurePolicy: Fail does not cover. An
// out-of-scope target must fall back to the ControlPlane's own namespace: the
// delivery Secrets carry the account's plaintext password and clouds.yaml, and the
// prune/teardown sweeps only walk controlPlaneNamespaces(cp), so a Secret planted
// outside it would survive the ControlPlane that minted it.
func TestServiceAccountDeliveryNamespace_RejectsOutOfScopeTarget(t *testing.T) {
	dedicated, dedicatedNS := saTargetControlPlane()
	own := saControlPlane()

	for _, tc := range []struct {
		name   string
		cp     *c5c3v1alpha1.ControlPlane
		target string
		want   string
	}{
		{"unset falls back to the own namespace", own, "", own.Namespace},
		{"the own namespace is in scope", own, own.Namespace, own.Namespace},
		{"a dedicated service namespace is in scope", dedicated, dedicatedNS, dedicatedNS},
		{"an out-of-scope target falls back", own, "kube-system", own.Namespace},
		{"a namespace dedicated to ANOTHER CP falls back", own, dedicatedNS, own.Namespace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			sa := tc.cp.Spec.KORC.ServiceAccounts[0]
			sa.TargetNamespace = tc.target
			g.Expect(serviceAccountDeliveryNamespace(tc.cp, sa)).To(Equal(tc.want))
		})
	}
}

// saTargetControlPlane returns saControlPlane's CP with its single account
// delivered into the dedicated "identity" service namespace (declared on the CR so
// the webhook contract holds), so the publish leg rides that namespace's tenant
// store.
func saTargetControlPlane() (*c5c3v1alpha1.ControlPlane, string) {
	const targetNS = "identity"
	cp := saControlPlane()
	cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      targetNS,
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.KORC.ServiceAccounts[0].TargetNamespace = targetNS
	return cp, targetNS
}

// TestReconcileServiceAccounts_TargetNamespaceDeliversWithLabels drives a converged
// account whose targetNamespace is a dedicated service namespace: the source
// Secret, PushSecret, and consumer ExternalSecret land in that namespace carrying
// the ownership LABELS (no owner reference — Kubernetes forbids a cross-namespace
// one), the PushSecret's remote key is re-scoped to the target namespace, and the
// converged pass records status.serviceAccounts[].secretNamespace as the target.
func TestReconcileServiceAccounts_TargetNamespaceDeliversWithLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	cp, targetNS := saTargetControlPlane()
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	// The publish leg lands in the target namespace; seed its already-synced
	// PushSecret (label-owned so ensureUnownedOrOwned adopts it) and the
	// materialized consumer Secret, plus a Ready tenant store in that namespace.
	push := serviceAccountPushSecret(cp, sa)
	stampControlPlaneChildLabels(push, cp)
	push.Status.Conditions = []esov1alpha1.PushSecretStatusCondition{
		{Type: esov1alpha1.PushSecretReady, Status: corev1.ConditionTrue},
	}
	push.Status.SyncedResourceVersion = testPushSyncedRV
	materialized := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountCredentialsSecretName(cp, sa), Namespace: targetNS},
		Data:       map[string][]byte{serviceAccountPasswordKey: []byte("current-pw")},
	}
	targetStore := readyTenantSecretStore(esoTenantStoreName, targetNS, "", "")

	seed := append(saUserProjectAppliedV1(cp, sa), push, materialized, targetStore)
	cond, c := runServiceAccounts(t, cp, seed...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountsProvisioned))

	// The source Secret is created in the target namespace, label-owned, no owner ref.
	src := &corev1.Secret{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: serviceAccountSourceSecretName(cp, sa), Namespace: targetNS}, src)).To(Succeed(),
		"the source Secret must be assembled in the target namespace")
	g.Expect(src.OwnerReferences).To(BeEmpty(), "a cross-namespace child carries no owner reference")
	g.Expect(src.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, cp.Name))
	g.Expect(src.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, cp.Namespace))

	// The PushSecret lives in the target namespace with the re-scoped remote key.
	gotPush := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: serviceAccountPushSecretName(cp, sa), Namespace: targetNS}, gotPush)).To(Succeed())
	g.Expect(gotPush.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, cp.Name))
	g.Expect(gotPush.Spec.Data[0].Match.RemoteRef.RemoteKey).
		To(Equal("openstack/keystone/identity/cp/service-accounts/nova"))
	// No PushSecret is created in the ControlPlane's own namespace.
	homeErr := c.Get(context.Background(),
		types.NamespacedName{Name: serviceAccountPushSecretName(cp, sa), Namespace: ns}, &esov1alpha1.PushSecret{})
	g.Expect(apierrors.IsNotFound(homeErr)).To(BeTrue(),
		"no delivery object may land in the ControlPlane's own namespace")

	// The consumer ExternalSecret lives in the target namespace, label-owned.
	es := &esov1.ExternalSecret{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: serviceAccountCredentialsSecretName(cp, sa), Namespace: targetNS}, es)).To(Succeed())
	g.Expect(es.OwnerReferences).To(BeEmpty())
	g.Expect(es.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, cp.Name))
	g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal("openstack/keystone/identity/cp/service-accounts/nova"))

	g.Expect(cp.Status.ServiceAccounts).To(HaveLen(1))
	got := cp.Status.ServiceAccounts[0]
	g.Expect(got.Ready).To(BeTrue())
	g.Expect(got.SecretName).To(Equal(serviceAccountCredentialsSecretName(cp, sa)))
	g.Expect(got.SecretNamespace).To(Equal(targetNS),
		"status must report the target namespace the credentials Secret lives in")
}

// TestReconcileServiceAccounts_StoreGateNamesTargetNamespace pins the per-namespace
// store gate: a NOT-ready tenant store in the target namespace blocks the account
// and names that namespace, even when the ControlPlane's own tenant store is Ready.
func TestReconcileServiceAccounts_StoreGateNamesTargetNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp, targetNS := saTargetControlPlane()

	// runServiceAccounts seeds the child-namespace store Ready; seed the target
	// namespace's store NOT ready.
	notReady := readyTenantSecretStore(esoTenantStoreName, targetNS, "", "")
	notReady.Status.Conditions = nil

	cond, _ := runServiceAccounts(t, cp, notReady)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountStoreNotReady))
	g.Expect(cond.Message).To(ContainSubstring(targetNS),
		"the store gate must name the delivery namespace whose store is not ready")
}

// TestReconcileServiceAccounts_PrunesDeliveryObjectsInTargetNamespace pins the
// widened prune sweep: removing an account reaps its delivery objects wherever they
// landed, including a dedicated service namespace. It seeds label-owned delivery
// objects in the target namespace and reconciles with the entry removed.
func TestReconcileServiceAccounts_PrunesDeliveryObjectsInTargetNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp, targetNS := saTargetControlPlane()
	sa := cp.Spec.KORC.ServiceAccounts[0]

	// Label-owned leftovers from a since-removed account in the target namespace.
	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountSourceSecretName(cp, sa), Namespace: targetNS},
	}
	stampControlPlaneChildLabels(src, cp)
	push := serviceAccountPushSecret(cp, sa)
	stampControlPlaneChildLabels(push, cp)
	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountCredentialsSecretName(cp, sa), Namespace: targetNS},
	}
	stampControlPlaneChildLabels(es, cp)

	// Reconcile with the entry REMOVED (but the namespace assignment kept, so the
	// target namespace is still in the ControlPlane's occupied set).
	cp.Spec.KORC.ServiceAccounts = nil
	_, c := runServiceAccounts(t, cp,
		readyTenantSecretStore(esoTenantStoreName, targetNS, "", ""), src, push, es)

	for _, obj := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: src.Name, Namespace: targetNS}},
		&esov1alpha1.PushSecret{ObjectMeta: metav1.ObjectMeta{Name: push.Name, Namespace: targetNS}},
		&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{Name: es.Name, Namespace: targetNS}},
	} {
		err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"undeclared delivery object %T in the target namespace must be pruned", obj)
	}
}
