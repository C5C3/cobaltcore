// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the declarative service-account sub-reconciler
// reconcileServiceAccounts (spec.korc.serviceAccounts).
package controller

import (
	"context"
	"testing"

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
	sa := cp.Spec.KORC.ServiceAccounts[0]
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

	cond, _ := runServiceAccounts(t, cp, user, project, pw, push, materialized)

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
