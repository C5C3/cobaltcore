// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the per-ControlPlane DB-credentials sub-reconciler
// (CC-0116, REQ-001, REQ-002, REQ-008): reconcileDBCredentials and its
// pure builder/helpers. The slice is scoped to the ExternalSecret that
// materialises a per-ControlPlane service DB credential from OpenBao; the
// projected secretRef override (REQ-003) is a later level and is NOT exercised
// here.
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

// dbCredManagedControlPlane builds a managed-mode ControlPlane (Database.ClusterRef
// set) — the mode in which reconcileDBCredentials projects the per-CP DB-credential
// ExternalSecret (REQ-001).
func dbCredManagedControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "controlplane",
			Namespace:  "openstack",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			Infrastructure: c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
					Database:   "keystone",
				},
			},
		},
	}
}

// dbCredBrownfieldControlPlane builds a brownfield-mode ControlPlane: the user
// supplies their own DB connection (Host set, ClusterRef nil), so the operator
// must NOT project an ExternalSecret (REQ-001, scenario: brownfield early-exit).
func dbCredBrownfieldControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := dbCredManagedControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:     "db.example.com",
		Database: "keystone",
	}
	return cp
}

// getDBCredES fetches the projected DB-credential ExternalSecret at its derived
// name/namespace.
func getDBCredES(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esov1.ExternalSecret, error) {
	t.Helper()
	es := &esov1.ExternalSecret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: dbCredentialSecretName(cp)}, es)
	return es, err
}

// readyDBCredES builds a Ready DB-credential ExternalSecret at the derived
// name/namespace, mirroring readyCloudsYamlES so WaitForExternalSecret reports
// Ready without an ESO controller in the fake client.
func readyDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := dbCredentialExternalSecret(cp)
	es.Status = esov1.ExternalSecretStatus{
		Conditions: []esov1.ExternalSecretStatusCondition{
			{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
		},
	}
	return es
}

// TestReconcileDBCredentials_Managed_CreatesExternalSecret (REQ-001): a managed CP
// drives reconcileDBCredentials to project the per-CP DB-credential ExternalSecret
// with the OpenBao-backed ClusterSecretStore ref, Owner creation policy, the two
// username/password Data entries, and a ControlPlane controller owner reference.
func TestReconcileDBCredentials_Managed_CreatesExternalSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// No Ready status on the freshly-created ES, so the call requeues with
	// DBCredentialsReady=False — that is expected here; we assert the ES shape.
	if _, err := r.reconcileDBCredentials(context.Background(), cp); err != nil {
		t.Fatalf("reconcileDBCredentials: %v", err)
	}

	es, err := getDBCredES(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the DB-credential ExternalSecret")

	g.Expect(es.Spec.SecretStoreRef.Kind).To(Equal("ClusterSecretStore"))
	g.Expect(es.Spec.SecretStoreRef.Name).To(Equal(openBaoClusterStoreName))
	g.Expect(es.Spec.Target.Name).To(Equal(dbCredentialSecretName(cp)))
	g.Expect(es.Spec.Target.CreationPolicy).To(Equal(esov1.CreatePolicyOwner))

	remoteKey := dbCredentialRemoteKeyFor(cp)
	g.Expect(es.Spec.Data).To(HaveLen(2))
	g.Expect(es.Spec.Data[0].SecretKey).To(Equal("username"))
	g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(remoteKey))
	g.Expect(es.Spec.Data[0].RemoteRef.Property).To(Equal("username"))
	g.Expect(es.Spec.Data[1].SecretKey).To(Equal("password"))
	g.Expect(es.Spec.Data[1].RemoteRef.Key).To(Equal(remoteKey))
	g.Expect(es.Spec.Data[1].RemoteRef.Property).To(Equal("password"))

	owner := metav1.GetControllerOf(es)
	g.Expect(owner).NotTo(BeNil(), "DB-credential ExternalSecret must be controller-owned by the ControlPlane")
	g.Expect(owner.Kind).To(Equal("ControlPlane"))
	g.Expect(owner.Name).To(Equal(cp.Name))
	g.Expect(owner.Controller).NotTo(BeNil())
	g.Expect(*owner.Controller).To(BeTrue())
}

// TestReconcileDBCredentials_NotReady_SetsConditionFalseAndRequeues (REQ-001,
// REQ-008): while the projected ExternalSecret has not synced, the sub-reconciler
// requeues and reports DBCredentialsReady=False with the waiting reason.
func TestReconcileDBCredentials_NotReady_SetsConditionFalseAndRequeues(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeNumerically(">", 0), "must requeue while DB credential ES is not Ready")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDBCredentialSecret"))
}

// TestReconcileDBCredentials_Ready_SetsConditionTrue (REQ-001, REQ-008): once the
// projected ExternalSecret reports Ready, DBCredentialsReady flips True and the
// sub-reconciler stops requeuing.
func TestReconcileDBCredentials_Ready_SetsConditionTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyDBCredES(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}), "ready DB credential must not requeue")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("DBCredentialsReady"))
}

// TestReconcileDBCredentials_Brownfield_NoExternalSecret_ReadyTrue (REQ-001): a
// brownfield CP supplies its own DB credential, so the operator projects NO
// ExternalSecret and reports DBCredentialsReady=True immediately with no requeue.
func TestReconcileDBCredentials_Brownfield_NoExternalSecret_ReadyTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredBrownfieldControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}), "brownfield must not requeue")

	_, getErr := getDBCredES(t, r, cp)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"brownfield must NOT project a DB-credential ExternalSecret")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// TestDBCredentialRemoteKeyFor_And_SecretName_DistinctPerControlPlane (REQ-001):
// the OpenBao remote key is scoped by both Namespace and Name, and the secret name
// is derived from keystoneName, so two ControlPlanes never resolve to the same
// OpenBao path or materialised Secret.
func TestDBCredentialRemoteKeyFor_And_SecretName_DistinctPerControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)

	cpFor := func(name, ns string) *c5c3v1alpha1.ControlPlane {
		return &c5c3v1alpha1.ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		}
	}

	// (a) Same Name in distinct namespaces → distinct remote keys.
	g.Expect(dbCredentialRemoteKeyFor(cpFor("cp", "ns-a"))).
		To(Equal("openstack/keystone/ns-a/cp/db"))
	g.Expect(dbCredentialRemoteKeyFor(cpFor("cp", "ns-b"))).
		To(Equal("openstack/keystone/ns-b/cp/db"))
	g.Expect(dbCredentialRemoteKeyFor(cpFor("cp", "ns-a"))).
		NotTo(Equal(dbCredentialRemoteKeyFor(cpFor("cp", "ns-b"))))

	// (b) Distinct Names in the same namespace → distinct secret names.
	g.Expect(dbCredentialSecretName(cpFor("cp-a", "openstack"))).
		To(Equal("cp-a-keystone-db-credentials"))
	g.Expect(dbCredentialSecretName(cpFor("cp-b", "openstack"))).
		To(Equal("cp-b-keystone-db-credentials"))
	g.Expect(dbCredentialSecretName(cpFor("cp-a", "openstack"))).
		NotTo(Equal(dbCredentialSecretName(cpFor("cp-b", "openstack"))))

	// (c) The canonical controlplane/openstack pair resolves to the documented
	// remote key and secret name.
	canonical := cpFor("controlplane", "openstack")
	g.Expect(dbCredentialRemoteKeyFor(canonical)).
		To(Equal("openstack/keystone/openstack/controlplane/db"))
	g.Expect(dbCredentialSecretName(canonical)).
		To(Equal("controlplane-keystone-db-credentials"))
}
