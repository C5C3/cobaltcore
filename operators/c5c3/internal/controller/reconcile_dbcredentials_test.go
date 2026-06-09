// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the per-ControlPlane DB-credential sub-reconciler (CC-0116,
// REQ-001, REQ-003, REQ-004, REQ-005): the dbCredentialRemoteKeyFor /
// dbCredentialSecretName scoping helpers, the dbCredentialExternalSecret
// builder, and reconcileDBCredentials.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// legacyFlatDBKey is the shared, pre-CC-0116 flat OpenBao path every Keystone
// drew its DB password from. The per-ControlPlane helpers MUST never collapse
// back to it.
const legacyFlatDBKey = "openstack/keystone/db"

// dbCredsManagedControlPlane builds a managed-mode ControlPlane (database
// ClusterRef set) using the documented Quick Start identity so the derived
// names match the acceptance criteria: ControlPlane "controlplane" in namespace
// "openstack" yields Secret/ExternalSecret "controlplane-keystone-db-credentials"
// and remote key "openstack/keystone/openstack/controlplane/db".
func dbCredsManagedControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "controlplane",
			Namespace:  "openstack",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Infrastructure: c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
					Database:   "keystone",
					SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
				},
			},
		},
	}
}

// dbCredsBrownfieldControlPlane builds a brownfield ControlPlane (database Host
// set, ClusterRef nil): the operator provisions no database and so must not
// manufacture a DB credential.
func dbCredsBrownfieldControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := dbCredsManagedControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "keystone-db", Key: "password"},
	}
	return cp
}

// readyDBCredentialES builds the per-ControlPlane DB-credential ExternalSecret
// (via the production builder) carrying a Ready=True status, modelling ESO
// having synced the materialised Secret.
func readyDBCredentialES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := dbCredentialExternalSecret(cp)
	es.Status = esov1.ExternalSecretStatus{
		Conditions: []esov1.ExternalSecretStatusCondition{
			{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
		},
	}
	return es
}

// getDBCredentialES fetches the DB-credential ExternalSecret the reconciler is
// expected to own; the bool reports whether it exists.
func getDBCredentialES(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane) (*esov1.ExternalSecret, bool) {
	t.Helper()
	es := &esov1.ExternalSecret{}
	key := types.NamespacedName{Name: dbCredentialSecretName(cp), Namespace: childNamespace(cp)}
	err := c.Get(context.Background(), key, es)
	if err != nil {
		return nil, false
	}
	return es, true
}

// --- 1.2: scoping helpers (REQ-003) ---

// TestDBCredentialRemoteKeyFor_EmbedsNamespaceAndName locks in the per-CR OpenBao
// path (CC-0116, REQ-003): the RemoteKey is scoped by both the ControlPlane's
// Namespace and Name so two ControlPlanes never clobber each other's DB
// credential on the cluster-global OpenBao backend, and neither reuses the legacy
// flat path.
func TestDBCredentialRemoteKeyFor_EmbedsNamespaceAndName(t *testing.T) {
	g := NewWithT(t)

	cases := []struct {
		name      string
		namespace string
		cpName    string
		want      string
	}{
		{"default control plane", "openstack", "controlplane", "openstack/keystone/openstack/controlplane/db"},
		{"same name, different namespace", "tenant-b", "controlplane", "openstack/keystone/tenant-b/controlplane/db"},
		{"different name, same namespace (a)", "openstack", "cp-a", "openstack/keystone/openstack/cp-a/db"},
		{"different name, same namespace (b)", "openstack", "cp-b", "openstack/keystone/openstack/cp-b/db"},
	}

	keys := make([]string, 0, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cp := &c5c3v1alpha1.ControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: tc.cpName, Namespace: tc.namespace},
			}
			got := dbCredentialRemoteKeyFor(cp)
			g.Expect(got).To(Equal(tc.want))
			g.Expect(got).NotTo(Equal(legacyFlatDBKey),
				"the per-ControlPlane key must not collapse back to the legacy flat path")
		})
		keys = append(keys, tc.want)
	}

	// Same name in different namespaces must still diverge (the namespace segment
	// disambiguates), as must different names in one namespace.
	g.Expect(keys[0]).NotTo(Equal(keys[1]),
		"same-name ControlPlanes in different namespaces must not share a RemoteKey")
	g.Expect(keys[2]).NotTo(Equal(keys[3]),
		"different-name ControlPlanes in one namespace must not share a RemoteKey")
}

// TestDBCredentialSecretName_NoCollisionAcrossControlPlanes asserts that no pair
// of {remoteKey, secretName} collides across the same-name/different-namespace
// and different-name/same-namespace fixtures (CC-0116, REQ-003).
//
// DECISION: dbCredentialSecretName embeds only cp.Name (keystoneName(cp) +
// "-db-credentials"), NOT the namespace, per the authoritative formula in the
// issue/REQ-003. So two same-named ControlPlanes in different namespaces resolve
// to the same Secret *name* — but the Secret is namespace-scoped and lives in
// childNamespace(cp), so they remain distinct objects, and the OpenBao remote
// key (the actual shared-backend blast radius) always embeds both namespace and
// name. The non-collision guarantee is therefore over the {remoteKey, secretName}
// pair, which is what the per-tenant isolation contract requires.
func TestDBCredentialSecretName_NoCollisionAcrossControlPlanes(t *testing.T) {
	g := NewWithT(t)

	// The documented default identity resolves to the documented Secret name.
	g.Expect(dbCredentialSecretName(dbCredsManagedControlPlane())).
		To(Equal("controlplane-keystone-db-credentials"))

	fixtures := []struct {
		namespace string
		cpName    string
	}{
		{"openstack", "controlplane"},
		{"tenant-b", "controlplane"}, // same name, different namespace
		{"openstack", "cp-a"},        // different name, same namespace
		{"openstack", "cp-b"},
	}

	seen := make(map[string]string, len(fixtures))
	for _, f := range fixtures {
		cp := &c5c3v1alpha1.ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: f.cpName, Namespace: f.namespace},
		}
		pair := dbCredentialRemoteKeyFor(cp) + "|" + dbCredentialSecretName(cp)
		id := f.namespace + "/" + f.cpName
		other, dup := seen[pair]
		g.Expect(dup).To(BeFalse(),
			"{remoteKey, secretName} collision: %s and %s both resolve to %s", other, id, pair)
		seen[pair] = id
	}
	g.Expect(seen).To(HaveLen(len(fixtures)))
}

// --- 1.3: ExternalSecret builder (REQ-001) ---

// TestDBCredentialExternalSecret_BuildsScopedSpec asserts the built ExternalSecret
// mirrors the static keystone-db.yaml shape, scoped per ControlPlane (CC-0116,
// REQ-001): the OpenBao ClusterSecretStore, a 1h refresh, an Owner-created target
// Secret, and username/password data mapped from the per-CP remote key.
func TestDBCredentialExternalSecret_BuildsScopedSpec(t *testing.T) {
	g := NewWithT(t)

	cp := dbCredsManagedControlPlane()
	es := dbCredentialExternalSecret(cp)

	g.Expect(es.Name).To(Equal(dbCredentialSecretName(cp)))
	g.Expect(es.Name).To(Equal("controlplane-keystone-db-credentials"))
	g.Expect(es.Namespace).To(Equal(childNamespace(cp)))

	g.Expect(es.Spec.SecretStoreRef.Name).To(Equal(openBaoClusterStoreName))
	g.Expect(es.Spec.SecretStoreRef.Kind).To(Equal("ClusterSecretStore"))

	g.Expect(es.Spec.RefreshInterval).NotTo(BeNil())
	g.Expect(es.Spec.RefreshInterval.Duration).To(Equal(time.Hour))

	g.Expect(es.Spec.Target.Name).To(Equal(dbCredentialSecretName(cp)))
	g.Expect(es.Spec.Target.CreationPolicy).To(Equal(esov1.CreatePolicyOwner))

	wantKey := dbCredentialRemoteKeyFor(cp)
	g.Expect(es.Spec.Data).To(ConsistOf(
		esov1.ExternalSecretData{
			SecretKey: "username",
			RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: wantKey, Property: "username"},
		},
		esov1.ExternalSecretData{
			SecretKey: "password",
			RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: wantKey, Property: "password"},
		},
	))
}

// --- 1.4: reconcileDBCredentials (REQ-001, REQ-004, REQ-005) ---

// TestReconcileDBCredentials_ManagedCreatesExternalSecret asserts that for a
// managed ControlPlane the sub-reconciler creates the owner-referenced,
// per-CP ExternalSecret, and — the ES not yet Ready — gates DBCredentialsReady on
// it (CC-0116, REQ-001, REQ-005).
func TestReconcileDBCredentials_ManagedCreatesExternalSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredsManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter),
		"a not-yet-Ready ExternalSecret must requeue on the DB-credentials cadence")

	es, ok := getDBCredentialES(t, c, cp)
	g.Expect(ok).To(BeTrue(), "the per-CP ExternalSecret must be created")
	g.Expect(es.Spec.SecretStoreRef.Name).To(Equal(openBaoClusterStoreName))
	g.Expect(es.Spec.Data).To(HaveLen(2))
	g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(dbCredentialRemoteKeyFor(cp)))

	// The ExternalSecret is controller-owned by the ControlPlane.
	g.Expect(es.OwnerReferences).To(HaveLen(1))
	owner := es.OwnerReferences[0]
	g.Expect(owner.Kind).To(Equal("ControlPlane"))
	g.Expect(owner.Name).To(Equal(cp.Name))
	g.Expect(owner.Controller).NotTo(BeNil())
	g.Expect(*owner.Controller).To(BeTrue())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDBCredentialSecret"))
}

// TestReconcileDBCredentials_IsIdempotent asserts two successive reconciles leave
// exactly one ExternalSecret with a stable spec and owner reference — no second
// object, no spec churn (CC-0116, REQ-001).
func TestReconcileDBCredentials_IsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredsManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	first, ok := getDBCredentialES(t, c, cp)
	g.Expect(ok).To(BeTrue())

	_, err = r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list esov1.ExternalSecretList
	g.Expect(c.List(context.Background(), &list, client.InNamespace(childNamespace(cp)))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1), "re-reconcile must not create a second ExternalSecret")

	second := &list.Items[0]
	g.Expect(second.Spec).To(Equal(first.Spec), "spec must be stable across reconciles")
	g.Expect(second.OwnerReferences).To(Equal(first.OwnerReferences))
}

// TestReconcileDBCredentials_BrownfieldNoExternalSecretReadyTrue asserts a
// brownfield ControlPlane creates NO ExternalSecret and reports
// DBCredentialsReady=True immediately (CC-0116, REQ-004).
func TestReconcileDBCredentials_BrownfieldNoExternalSecretReadyTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredsBrownfieldControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	_, ok := getDBCredentialES(t, c, cp)
	g.Expect(ok).To(BeFalse(), "brownfield must not create a DB-credential ExternalSecret")

	var list esov1.ExternalSecretList
	g.Expect(c.List(context.Background(), &list, client.InNamespace(childNamespace(cp)))).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BrownfieldDatabase"))
}

// TestReconcileDBCredentials_BrownfieldDoesNotRequeue asserts the brownfield path
// returns a zero ctrl.Result with nil error, so the reconcile proceeds to
// reconcileKeystone in the same pass (CC-0116, REQ-004).
func TestReconcileDBCredentials_BrownfieldDoesNotRequeue(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredsBrownfieldControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "brownfield must not requeue")
}

// TestReconcileDBCredentials_WaitsForExternalSecretReady asserts the readiness
// gate: a not-yet-synced ExternalSecret keeps DBCredentialsReady False with a
// requeue, and a Ready ExternalSecret flips it True with a zero result (CC-0116,
// REQ-005).
func TestReconcileDBCredentials_WaitsForExternalSecretReady(t *testing.T) {
	t.Run("not ready -> waiting + requeue", func(t *testing.T) {
		g := NewGomegaWithT(t)

		s := korcTestScheme(t)
		cp := dbCredsManagedControlPlane()
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		res, err := r.reconcileDBCredentials(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))

		cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal("WaitingForDBCredentialSecret"))
	})

	t.Run("ready -> DBCredentialsReady True", func(t *testing.T) {
		g := NewGomegaWithT(t)

		s := korcTestScheme(t)
		cp := dbCredsManagedControlPlane()
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(cp, readyDBCredentialES(cp)).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		res, err := r.reconcileDBCredentials(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue(), "a Ready ExternalSecret must not requeue")

		cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal("DBCredentialsReady"))
	})
}

// TestReconcileDBCredentials_EnsureErrorReported asserts that a failure to
// create/update the ExternalSecret surfaces DBCredentialsReady=False with reason
// ExternalSecretError and returns the error (CC-0116, REQ-005).
func TestReconcileDBCredentials_EnsureErrorReported(t *testing.T) {
	g := NewGomegaWithT(t)

	errInjected := errors.New("injected create failure")
	s := korcTestScheme(t)
	cp := dbCredsManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*esov1.ExternalSecret); ok {
					return errInjected
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, errInjected)).To(BeTrue(), "the underlying client error must be wrapped")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ExternalSecretError"))
}
