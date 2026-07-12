// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the per-ControlPlane admin-password sub-reconciler
// reconcileAdminPassword and its
// pure builder/helpers. The slice is scoped to the ExternalSecret that
// materialises a per-ControlPlane admin password from OpenBao; the projected
// secretRef override (effectiveAdminPasswordSecretRef) is wired into
// consumers in a later level and is NOT exercised here.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// adminPwManagedControlPlane builds a managed-mode ControlPlane (Database.ClusterRef
// set) — the mode in which reconcileAdminPassword projects the per-CP admin-password
// ExternalSecret.
func adminPwManagedControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "controlplane",
			Namespace:  "openstack",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
					Database:   "keystone",
				},
			},
		},
	}
}

// adminPwBrownfieldControlPlane builds a brownfield-mode ControlPlane: the user
// supplies their own DB connection (Host set, ClusterRef nil) and admin-password
// Secret, so the operator must NOT project an ExternalSecret (scenario: brownfield early-exit).
func adminPwBrownfieldControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := adminPwManagedControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:     "db.example.com",
		Database: "keystone",
	}
	cp.Spec.KORC.AdminCredential.PasswordSecretRef = commonv1.SecretRefSpec{
		Name: "user-admin-secret",
		Key:  "password",
	}
	return cp
}

// getAdminPwES fetches the projected admin-password ExternalSecret at its derived
// name/namespace.
func getAdminPwES(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esov1.ExternalSecret, error) {
	t.Helper()
	es := &esov1.ExternalSecret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: adminPasswordSecretName(cp)}, es)
	return es, err
}

// readyAdminPwES builds a Ready admin-password ExternalSecret at the derived
// name/namespace, mirroring readyDBCredES so WaitForExternalSecret reports
// Ready without an ESO controller in the fake client.
func readyAdminPwES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := adminPasswordExternalSecret(cp)
	es.Status = esov1.ExternalSecretStatus{
		Conditions: []esov1.ExternalSecretStatusCondition{
			{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
		},
	}
	return es
}

// TestReconcileAdminPassword_Managed_CreatesExternalSecret a managed CP
// drives reconcileAdminPassword to project the per-CP admin-password ExternalSecret
// with the OpenBao-backed ClusterSecretStore ref, Owner creation policy, the single
// password Data entry, and a ControlPlane controller owner reference.
func TestReconcileAdminPassword_Managed_CreatesExternalSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := adminPwManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore(), readyTenantStoreFor(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// No Ready status on the freshly-created ES, so the call requeues with
	// AdminPasswordReady=False — that is expected here; we assert the ES shape.
	if _, err := r.reconcileAdminPassword(context.Background(), cp); err != nil {
		t.Fatalf("reconcileAdminPassword: %v", err)
	}

	es, err := getAdminPwES(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the admin-password ExternalSecret")

	// A nil secretStoreRef now defaults to the operator-provisioned per-tenant
	// namespaced store, not the shared cluster store.
	g.Expect(es.Spec.SecretStoreRef.Kind).To(Equal("SecretStore"))
	g.Expect(es.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))
	g.Expect(es.Spec.Target.Name).To(Equal(adminPasswordSecretName(cp)))
	g.Expect(es.Spec.Target.CreationPolicy).To(Equal(esov1.CreatePolicyOwner))

	remoteKey := adminPasswordRemoteKeyFor(cp)
	g.Expect(es.Spec.Data).To(HaveLen(1))
	g.Expect(es.Spec.Data[0].SecretKey).To(Equal("password"))
	g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(remoteKey))
	g.Expect(es.Spec.Data[0].RemoteRef.Property).To(Equal("password"))

	owner := metav1.GetControllerOf(es)
	g.Expect(owner).NotTo(BeNil(), "admin-password ExternalSecret must be controller-owned by the ControlPlane")
	g.Expect(owner.Kind).To(Equal("ControlPlane"))
	g.Expect(owner.Name).To(Equal(cp.Name))
	g.Expect(owner.Controller).NotTo(BeNil())
	g.Expect(*owner.Controller).To(BeTrue())
}

// TestReconcileAdminPassword_NotReady_SetsConditionFalseAndRequeues while the projected ExternalSecret has not synced, the sub-reconciler
// requeues and reports AdminPasswordReady=False with the waiting reason.
func TestReconcileAdminPassword_NotReady_SetsConditionFalseAndRequeues(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := adminPwManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore(), readyTenantStoreFor(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileAdminPassword(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(adminPasswordRequeueAfter),
		"must requeue with adminPasswordRequeueAfter while admin password ES is not Ready")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeAdminPasswordReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForAdminPasswordSecret"))
}

// TestReconcileAdminPassword_Ready_SetsConditionTrue once the
// projected ExternalSecret reports Ready, AdminPasswordReady flips True and the
// sub-reconciler stops requeuing.
func TestReconcileAdminPassword_Ready_SetsConditionTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := adminPwManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore(), readyTenantStoreFor(cp), readyAdminPwES(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileAdminPassword(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}), "ready admin password must not requeue")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeAdminPasswordReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("AdminPasswordReady"))
}

// TestReconcileAdminPassword_Brownfield_NoExternalSecret_ReadyTrue a
// brownfield CP supplies its own admin password, so the operator projects NO
// ExternalSecret and reports AdminPasswordReady=True immediately with no requeue,
// leaving the user-declared PasswordSecretRef untouched.
func TestReconcileAdminPassword_Brownfield_NoExternalSecret_ReadyTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := adminPwBrownfieldControlPlane()
	userRef := cp.Spec.KORC.AdminCredential.PasswordSecretRef
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileAdminPassword(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}), "brownfield must not requeue")

	_, getErr := getAdminPwES(t, r, cp)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"brownfield must NOT project an admin-password ExternalSecret")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeAdminPasswordReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

	g.Expect(cp.Spec.KORC.AdminCredential.PasswordSecretRef).To(Equal(userRef),
		"brownfield must not mutate the user-declared admin PasswordSecretRef")
}

// TestReconcileAdminPassword_StoreNotReady_SetsConditionFalse (#476): when the
// OpenBao-backed ClusterSecretStore is not Ready (here: absent), the managed-mode
// sub-reconciler flips AdminPasswordReady=False with reason SecretStoreNotReady
// and requeues, instead of leaving a stale Ready=True between resyncs. No
// ExternalSecret is projected while the store is unreachable.
func TestReconcileAdminPassword_StoreNotReady_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := adminPwManagedControlPlane()
	// No ClusterSecretStore seeded => IsClusterSecretStoreReady reports not ready.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileAdminPassword(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(adminPasswordRequeueAfter),
		"must requeue while the store is not ready")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeAdminPasswordReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))

	_, getErr := getAdminPwES(t, r, cp)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"no admin-password ExternalSecret may be created while the store is not ready")
}

// TestAdminPasswordRemoteKeyFor_And_SecretName_DistinctPerControlPlane the OpenBao remote key is scoped by both Namespace and keystoneName,
// and the secret name is derived from keystoneName, so two ControlPlanes never
// resolve to the same OpenBao path or materialised Secret — and neither the name
// nor the key ever collides with the legacy static "keystone-admin" identifier.
func TestAdminPasswordRemoteKeyFor_And_SecretName_DistinctPerControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)

	cpFor := func(name, ns string) *c5c3v1alpha1.ControlPlane {
		return &c5c3v1alpha1.ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		}
	}

	// (a) Same Name in distinct namespaces → distinct remote keys.
	g.Expect(adminPasswordRemoteKeyFor(cpFor("cp", "ns-a"))).
		To(Equal("bootstrap/ns-a/cp-keystone/admin"))
	g.Expect(adminPasswordRemoteKeyFor(cpFor("cp", "ns-b"))).
		To(Equal("bootstrap/ns-b/cp-keystone/admin"))
	g.Expect(adminPasswordRemoteKeyFor(cpFor("cp", "ns-a"))).
		NotTo(Equal(adminPasswordRemoteKeyFor(cpFor("cp", "ns-b"))))

	// (b) Distinct Names in the same namespace → distinct secret names.
	g.Expect(adminPasswordSecretName(cpFor("cp-a", "openstack"))).
		To(Equal("cp-a-keystone-admin-credentials"))
	g.Expect(adminPasswordSecretName(cpFor("cp-b", "openstack"))).
		To(Equal("cp-b-keystone-admin-credentials"))
	g.Expect(adminPasswordSecretName(cpFor("cp-a", "openstack"))).
		NotTo(Equal(adminPasswordSecretName(cpFor("cp-b", "openstack"))))

	// (c) The canonical controlplane/openstack pair resolves to the documented
	// keystone-name-scoped remote key and secret name.
	canonical := cpFor("controlplane", "openstack")
	g.Expect(adminPasswordRemoteKeyFor(canonical)).
		To(Equal("bootstrap/openstack/controlplane-keystone/admin"))
	g.Expect(adminPasswordSecretName(canonical)).
		To(Equal("controlplane-keystone-admin-credentials"))

	// (d) Neither the derived name nor the remote key ever equals the legacy
	// static "keystone-admin" identifier the static ExternalSecrets used.
	for _, cp := range []*c5c3v1alpha1.ControlPlane{
		cpFor("cp", "ns-a"), cpFor("cp", "ns-b"),
		cpFor("cp-a", "openstack"), cpFor("cp-b", "openstack"),
		canonical,
	} {
		g.Expect(adminPasswordSecretName(cp)).NotTo(Equal("keystone-admin"))
		g.Expect(adminPasswordRemoteKeyFor(cp)).NotTo(Equal("keystone-admin"))
	}
}

// adminPwExternalControlPlane builds an External-mode ControlPlane: no
// spec.infrastructure block, and the admin password lives in a user-supplied
// Secret the operator only ever reads.
func adminPwExternalControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := adminPwManagedControlPlane()
	cp.Spec.Infrastructure = nil
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Mode:     c5c3v1alpha1.KeystoneModeExternal,
		External: &c5c3v1alpha1.ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
	}
	cp.Spec.KORC.AdminCredential.PasswordSecretRef = commonv1.SecretRefSpec{
		Name: "external-admin",
		Key:  "password",
	}
	return cp
}

// TestReconcileAdminPassword_ExternalMode_NoExternalSecret_ReadyTrue asserts the
// External-mode short-circuit: AdminPasswordReady=True/ExternallyManaged, a
// message naming both the user Secret and the external endpoint, and no
// projection. No ClusterSecretStore is seeded, so a still-consulted store gate
// would flip the condition to SecretStoreNotReady and fail this test.
func TestReconcileAdminPassword_ExternalMode_NoExternalSecret_ReadyTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := adminPwExternalControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileAdminPassword(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}), "the External short-circuit must not requeue")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeAdminPasswordReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonExternallyManaged))
	g.Expect(cond.Message).To(ContainSubstring(`"external-admin"`),
		"the message must name the user-supplied admin-password Secret")
	g.Expect(cond.Message).To(ContainSubstring("https://keystone.example.com/v3"))

	_, getErr := getAdminPwES(t, r, cp)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"External mode must NOT project an admin-password ExternalSecret")
}

// TestEffectiveAdminPasswordSecretRef_ExternalModeReturnsUserSuppliedRef pins that
// the External branch is keyed on the MODE discriminator, not on the database
// shape. The second case is webhook-impossible on purpose: an External-mode CR
// carrying a managed Database.ClusterRef. A Database.ClusterRef-keyed
// implementation would return the operator-owned Secret name there and lose the
// user's admin password — and with it the hash-driven re-mint input.
func TestEffectiveAdminPasswordSecretRef_ExternalModeReturnsUserSuppliedRef(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := adminPwExternalControlPlane()
	g.Expect(effectiveAdminPasswordSecretRef(cp)).
		To(Equal(commonv1.SecretRefSpec{Name: "external-admin", Key: "password"}))

	withManagedDB := adminPwExternalControlPlane()
	withManagedDB.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{
		Database: commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"}},
	}
	g.Expect(effectiveAdminPasswordSecretRef(withManagedDB)).
		To(Equal(commonv1.SecretRefSpec{Name: "external-admin", Key: "password"}),
			"the identity mode, not the database shape, must select the External branch")
}

// TestEffectiveAdminPasswordSecretRef_ManagedUnchanged guards the AC-2 regression
// contract: a managed CR still resolves to the operator-owned per-CP Secret.
func TestEffectiveAdminPasswordSecretRef_ManagedUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := adminPwManagedControlPlane()
	g.Expect(effectiveAdminPasswordSecretRef(cp)).
		To(Equal(commonv1.SecretRefSpec{Name: adminPasswordSecretName(cp), Key: "password"}))
}
