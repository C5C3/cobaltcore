// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the per-ControlPlane DB-credentials sub-reconciler
// reconcileDBCredentials and its
// pure builder/helpers. The slice is scoped to the ExternalSecret that
// materialises a per-ControlPlane service DB credential from OpenBao; the
// projected secretRef override is a later level and is NOT exercised
// here.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// dbCredManagedControlPlane builds a managed-mode ControlPlane (Database.ClusterRef
// set) — the mode in which reconcileDBCredentials projects the per-CP DB-credential
// ExternalSecret.
func dbCredManagedControlPlane() *c5c3v1alpha1.ControlPlane {
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

// dbCredBrownfieldControlPlane builds a brownfield-mode ControlPlane: the user
// supplies their own DB connection (Host set, ClusterRef nil), so the operator
// must NOT project an ExternalSecret (scenario: brownfield early-exit).
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

// openBaoClusterStoreName aliases the shared ClusterSecretStore name
// (secrets.OpenBaoClusterStoreName) for the ClusterSecretStore fixtures in
// this package's tests.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

// readyClusterSecretStore returns the OpenBao-backed ClusterSecretStore with a
// Ready status condition so IsClusterSecretStoreReady reports the store ready
// without an ESO controller in the fake client. Seed it whenever a managed-mode
// sub-reconciler must pass its ClusterSecretStore gate (#476).
func readyClusterSecretStore() *esov1.ClusterSecretStore {
	return &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// readyDBCredES builds a Ready DB-credential ExternalSecret at the derived
// name/namespace (Dynamic default shape), so WaitForExternalSecret reports Ready
// without an ESO controller in the fake client.
func readyDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := dbCredentialGeneratorExternalSecret(cp)
	es.Status = esov1.ExternalSecretStatus{
		Conditions: []esov1.ExternalSecretStatusCondition{
			{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
		},
	}
	return es
}

// getVDS fetches the projected VaultDynamicSecret generator.
func getVDS(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esgenv1alpha1.VaultDynamicSecret, error) {
	t.Helper()
	vds := &esgenv1alpha1.VaultDynamicSecret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: dbCredentialSecretName(cp)}, vds)
	return vds, err
}

// TestReconcileDBCredentials_Managed_ProjectsDynamicObjects a managed CP (default
// Dynamic mode) drives reconcileDBCredentials to project the generator-backed
// ExternalSecret (DataFrom.GeneratorRef, no static Data), the VaultDynamicSecret
// generator, the ServiceAccount, and the mTLS client Certificate — all
// owner-referenced to the ControlPlane.
func TestReconcileDBCredentials_Managed_ProjectsDynamicObjects(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore(), readyTenantStoreFor(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// No Ready status on the freshly-created ES, so the call requeues with
	// DBCredentialsReady=False — that is expected here; we assert the shapes.
	if _, err := r.reconcileDBCredentials(context.Background(), cp); err != nil {
		t.Fatalf("reconcileDBCredentials: %v", err)
	}

	// ExternalSecret: generator-backed, no static KV Data.
	es, err := getDBCredES(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the DB-credential ExternalSecret")
	g.Expect(es.Spec.Data).To(BeEmpty(), "Dynamic ExternalSecret must carry no static Data refs")
	g.Expect(es.Spec.SecretStoreRef.Name).To(BeEmpty(), "generator-backed ExternalSecret must not reference a SecretStore")
	g.Expect(es.Spec.DataFrom).To(HaveLen(1))
	g.Expect(es.Spec.DataFrom[0].SourceRef).NotTo(BeNil())
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef).NotTo(BeNil())
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Kind).To(Equal("VaultDynamicSecret"))
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Name).To(Equal(dbCredentialSecretName(cp)))
	g.Expect(metav1.GetControllerOf(es)).NotTo(BeNil())

	// VaultDynamicSecret: reads the per-tenant creds path, authenticates via the
	// per-CP SA and mTLS client cert (all same-namespace refs).
	vds, err := getVDS(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the VaultDynamicSecret generator")
	g.Expect(vds.Spec.Path).To(Equal(dbDynamicCredsPathFor(cp)))
	g.Expect(vds.Spec.Method).To(Equal("GET"))
	g.Expect(vds.Spec.Provider).NotTo(BeNil())
	g.Expect(vds.Spec.Provider.Server).To(Equal(openBaoDefaultServer))
	g.Expect(vds.Spec.Provider.Version).To(Equal(esov1.VaultKVStoreV2),
		"version must be set explicitly — no omitempty, so \"\" fails the CRD enum")
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.Path).To(Equal(openBaoDefaultKubernetesMount))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.Role).To(Equal(dbDynamicVaultRole))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.ServiceAccountRef.Name).To(Equal(dbCredentialServiceAccountName))
	g.Expect(vds.Spec.Provider.CAProvider.Name).To(Equal(dbCredentialClientCertName(cp)))
	g.Expect(vds.Spec.Provider.CAProvider.Key).To(Equal("ca.crt"))
	g.Expect(vds.Spec.Provider.ClientTLS.CertSecretRef.Name).To(Equal(dbCredentialClientCertName(cp)))
	g.Expect(vds.Spec.Provider.ClientTLS.KeySecretRef.Name).To(Equal(dbCredentialClientCertName(cp)))
	g.Expect(metav1.GetControllerOf(vds)).NotTo(BeNil())

	// ServiceAccount exists and is owner-referenced.
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: dbCredentialServiceAccountName}, sa)).To(Succeed())
	g.Expect(metav1.GetControllerOf(sa)).NotTo(BeNil())

	// Certificate (unstructured) exists with the client-auth usage.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: dbCredentialClientCertName(cp)}, cert)).To(Succeed())
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	g.Expect(issuer).To(Equal(openBaoCAIssuerName))
	g.Expect(metav1.GetControllerOf(cert)).NotTo(BeNil())
}

// TestReconcileDBCredentials_Static_ProjectsKVExternalSecret verifies the Static
// opt-out projects the stage-(a) KV-backed ExternalSecret (username/password Data
// from the per-CP KV path) and projects no VaultDynamicSecret generator.
// readyTenantSecretStore builds a Ready namespaced SecretStore in the given
// namespace, optionally carrying a Vault provider with a custom server and
// kubernetes-auth mount so openBaoConnection can read them.
func readyTenantSecretStore(name, namespace, server, mount string) *esov1.SecretStore {
	store := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
			},
		},
	}
	if server != "" || mount != "" {
		store.Spec.Provider = &esov1.SecretStoreProvider{
			Vault: &esov1.VaultProvider{
				Server: server,
				Auth: &esov1.VaultAuth{
					Kubernetes: &esov1.VaultKubernetesAuth{Path: mount},
				},
			},
		}
	}
	return store
}

// TestOpenBaoConnection_ReadsFromNamespacedStore verifies openBaoConnection
// resolves the ControlPlane's selected namespaced SecretStore (in the child
// namespace) and copies its Vault server/mount, rather than the cluster store.
func TestOpenBaoConnection_ReadsFromNamespacedStore(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	store := readyTenantSecretStore("openbao-tenant-store", childNamespace(cp),
		"https://openbao.tenant.svc:8200", "kubernetes/tenant")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, store).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	server, mount := r.openBaoConnection(context.Background(), cp, effectiveControlPlaneStoreRef(cp))
	g.Expect(server).To(Equal("https://openbao.tenant.svc:8200"))
	g.Expect(mount).To(Equal("kubernetes/tenant"))
}

// TestOpenBaoConnection_FallsBackToDefaultsWhenStoreMissing verifies the
// documented fallbacks when the selected namespaced store is absent.
func TestOpenBaoConnection_FallsBackToDefaultsWhenStoreMissing(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	server, mount := r.openBaoConnection(context.Background(), cp, effectiveControlPlaneStoreRef(cp))
	g.Expect(server).To(Equal(openBaoDefaultServer))
	g.Expect(mount).To(Equal(openBaoDefaultKubernetesMount))
}

func TestReconcileDBCredentials_Static_ProjectsKVExternalSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeStatic
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore(), readyTenantStoreFor(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	if _, err := r.reconcileDBCredentials(context.Background(), cp); err != nil {
		t.Fatalf("reconcileDBCredentials: %v", err)
	}

	es, err := getDBCredES(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred())
	// A nil secretStoreRef now defaults to the operator-provisioned per-tenant
	// namespaced store, not the shared cluster store.
	g.Expect(es.Spec.SecretStoreRef.Kind).To(Equal("SecretStore"))
	g.Expect(es.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))
	g.Expect(es.Spec.DataFrom).To(BeEmpty(), "Static ExternalSecret must not use a generator")
	g.Expect(es.Spec.Data).To(HaveLen(2))
	g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(dbCredentialRemoteKeyFor(cp)))

	// No VaultDynamicSecret generator in Static mode.
	_, vdsErr := getVDS(t, r, cp)
	g.Expect(apierrors.IsNotFound(vdsErr)).To(BeTrue(), "Static mode must not project a VaultDynamicSecret")
}

// TestReconcileDBCredentials_StaticAfterDynamic_TearsDownGenerator verifies a
// Dynamic→Static flip deletes the previously-projected VaultDynamicSecret so no
// live generator is orphaned.
func TestReconcileDBCredentials_StaticAfterDynamic_TearsDownGenerator(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeStatic
	// Pre-seed a leftover VaultDynamicSecret from a prior Dynamic deployment.
	leftover := dbCredentialVaultDynamicSecret(cp, openBaoDefaultServer, openBaoDefaultKubernetesMount)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore(), readyTenantStoreFor(cp), leftover).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	if _, err := r.reconcileDBCredentials(context.Background(), cp); err != nil {
		t.Fatalf("reconcileDBCredentials: %v", err)
	}

	_, vdsErr := getVDS(t, r, cp)
	g.Expect(apierrors.IsNotFound(vdsErr)).To(BeTrue(), "Static flip must delete the leftover VaultDynamicSecret")
}

// TestReconcileDBCredentials_NotReady_SetsConditionFalseAndRequeues while the projected ExternalSecret has not synced, the sub-reconciler
// requeues and reports DBCredentialsReady=False with the waiting reason.
func TestReconcileDBCredentials_NotReady_SetsConditionFalseAndRequeues(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore(), readyTenantStoreFor(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeNumerically(">", 0), "must requeue while DB credential ES is not Ready")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDBCredentialSecret"))
}

// TestReconcileDBCredentials_Ready_SetsConditionTrue once the
// projected ExternalSecret reports Ready, DBCredentialsReady flips True and the
// sub-reconciler stops requeuing.
func TestReconcileDBCredentials_Ready_SetsConditionTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore(), readyTenantStoreFor(cp), readyDBCredES(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}), "ready DB credential must not requeue")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("DBCredentialsReady"))
}

// TestReconcileDBCredentials_Brownfield_NoExternalSecret_ReadyTrue a
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

// TestReconcileDBCredentials_StoreNotReady_SetsConditionFalse (#476): when the
// OpenBao-backed ClusterSecretStore is not Ready (here: absent), the managed-mode
// sub-reconciler flips DBCredentialsReady=False with reason SecretStoreNotReady
// and requeues, instead of leaving a stale Ready=True between resyncs. No
// ExternalSecret is projected while the store is unreachable.
func TestReconcileDBCredentials_StoreNotReady_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	// No ClusterSecretStore seeded => IsClusterSecretStoreReady reports not ready.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeNumerically(">", 0), "must requeue while the store is not ready")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))

	// No ExternalSecret may be projected while the store is unreachable.
	_, getErr := getDBCredES(t, r, cp)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"no DB-credential ExternalSecret may be created while the store is not ready")
}

// TestDBCredentialRemoteKeyFor_And_SecretName_DistinctPerControlPlane
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

// TestDBDynamicRoleFor_DistinctPerControlPlane backs AC 4 at the unit level: two
// ControlPlanes resolve to distinct per-tenant OpenBao roles and creds paths, so
// one tenant's revoke cannot affect another. The role-name derivation MUST stay
// in sync with setup-database-tenant.sh.
func TestDBDynamicRoleFor_DistinctPerControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)

	cpFor := func(name, ns string) *c5c3v1alpha1.ControlPlane {
		return &c5c3v1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	}

	a := cpFor("cp", "tenant-a")
	b := cpFor("cp", "tenant-b")
	g.Expect(dbDynamicRoleFor(a)).To(Equal("keystone-tenant-a"))
	g.Expect(dbDynamicRoleFor(b)).To(Equal("keystone-tenant-b"))
	g.Expect(dbDynamicRoleFor(a)).NotTo(Equal(dbDynamicRoleFor(b)))

	g.Expect(dbDynamicCredsPathFor(a)).To(Equal("database/mariadb/creds/keystone-tenant-a"))
	g.Expect(dbDynamicCredsPathFor(b)).To(Equal("database/mariadb/creds/keystone-tenant-b"))
	g.Expect(dbDynamicCredsPathFor(a)).NotTo(Equal(dbDynamicCredsPathFor(b)))

	// Regression: keying on the namespace alone is collision-free. The former
	// hyphen-joined <namespace>-<name> derivation flattened distinct ControlPlanes
	// to the same role — ns=a-b/name=c and ns=a/name=b-c both produced
	// keystone-a-b-c, so onboarding the second silently overwrote the first
	// tenant's connection config and role. Namespace-only keying keeps them
	// distinct (namespaces are cluster-unique).
	collideX := cpFor("c", "a-b")
	collideY := cpFor("b-c", "a")
	g.Expect(dbDynamicRoleFor(collideX)).NotTo(Equal(dbDynamicRoleFor(collideY)))
	g.Expect(dbDynamicCredsPathFor(collideX)).NotTo(Equal(dbDynamicCredsPathFor(collideY)))
}

// dbCredExternalControlPlane builds an External-mode ControlPlane: no
// spec.infrastructure block at all, so there is no database — managed OR
// brownfield — to issue credentials for.
func dbCredExternalControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := dbCredManagedControlPlane()
	cp.Spec.Infrastructure = nil
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Mode:     c5c3v1alpha1.KeystoneModeExternal,
		External: &c5c3v1alpha1.ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
	}
	return cp
}

// TestReconcileDBCredentials_ExternalMode_NoProjection_ReadyTrue asserts the
// External-mode short-circuit reports ExternallyManaged and projects nothing.
// No ClusterSecretStore is seeded into the fake client: were the store gate still
// consulted, reconcileDBCredentials would flip the condition to
// SecretStoreNotReady and requeue, so the assertions below double as proof that
// External mode never touches OpenBao/ESO.
func TestReconcileDBCredentials_ExternalMode_NoProjection_ReadyTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredExternalControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	result, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}), "the External short-circuit must not requeue")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonExternallyManaged))
	g.Expect(cond.Message).To(ContainSubstring("https://keystone.example.com/v3"))

	_, getErr := getDBCredES(t, r, cp)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"External mode must NOT project a DB-credential ExternalSecret")
	_, vdsErr := getVDS(t, r, cp)
	g.Expect(apierrors.IsNotFound(vdsErr)).To(BeTrue(),
		"External mode must NOT project a VaultDynamicSecret generator")
}

// TestReconcileDBCredentials_BrownfieldStillReportsBrownfieldReason pins that the
// External short-circuit did not swallow the brownfield one: a brownfield
// *database* under a managed Keystone keeps its own documented reason, which is a
// different operator situation from "no database at all".
func TestReconcileDBCredentials_BrownfieldStillReportsBrownfieldReason(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredBrownfieldControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("BrownfieldUserSuppliedCredential"))
	g.Expect(cond.Reason).NotTo(Equal(conditionReasonExternallyManaged),
		"a brownfield database is not an externally-managed identity plane")
}
