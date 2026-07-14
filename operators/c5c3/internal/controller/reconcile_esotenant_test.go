// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the per-ControlPlane ESO-tenant-store sub-reconciler
// reconcileESOTenantStore and its pure builders/helpers. The tests cover default
// provisioning (nil secretStoreRef) and its not-ready gate, the Ready pass, the
// provisioning-error path, the explicit-ref override that provisions nothing, and
// the effectiveControlPlaneStoreRef resolution table.
package controller

import (
	"context"
	"errors"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// readyTenantStoreFor returns the operator-provisioned per-tenant SecretStore in
// cp's child namespace with a Ready status, so a store-gated sub-reconciler under
// test passes the ESOTenantStore gate without an ESO controller — the default
// store a nil-ref ControlPlane resolves to. The provider is bare so
// openBaoConnection falls back to the documented defaults.
func readyTenantStoreFor(cp *c5c3v1alpha1.ControlPlane) *esov1.SecretStore {
	return readyTenantSecretStore(esoTenantStoreName, childNamespace(cp), "", "")
}

// getTenantStore fetches the operator-provisioned tenant SecretStore.
func getTenantStore(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esov1.SecretStore, error) {
	t.Helper()
	store := &esov1.SecretStore{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: esoTenantStoreName}, store)
	return store, err
}

// esoTenantCondition returns the ESOTenantStoreReady condition off the CR.
func esoTenantCondition(cp *c5c3v1alpha1.ControlPlane) *metav1.Condition {
	return conditions.GetCondition(cp.Status.Conditions, conditionTypeESOTenantStoreReady)
}

// TestReconcileESOTenantStore_ProvisionsObjects a managed CP with no explicit
// secretStoreRef drives reconcileESOTenantStore to provision the eso-tenant-auth
// ServiceAccount, the eso-tenant-client-tls Certificate, and the
// openbao-tenant-store SecretStore — all owner-referenced to the ControlPlane —
// with the OpenBao connection copied from the shared cluster store. While the
// store is not Ready the sub-reconciler requeues with ESOTenantStoreReady=False.
func TestReconcileESOTenantStore_ProvisionsObjects(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	// A custom shared-store provider so we can assert openBaoConnection is sourced
	// from the SHARED store, never the tenant store this reconciler builds.
	sharedStore := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName},
		Spec: esov1.SecretStoreSpec{Provider: &esov1.SecretStoreProvider{Vault: &esov1.VaultProvider{
			Server: "https://openbao.example.svc:8200",
			Auth:   &esov1.VaultAuth{Kubernetes: &esov1.VaultKubernetesAuth{Path: "kubernetes/management"}},
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, sharedStore).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(), "provisioning must not error")
	g.Expect(res.RequeueAfter).To(Equal(esoTenantStoreRequeueAfter), "must requeue while the store is not Ready")

	// ServiceAccount.
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: esoTenantServiceAccountName}, sa)).To(Succeed())
	g.Expect(metav1.GetControllerOf(sa)).NotTo(BeNil(), "SA must be owner-referenced to the ControlPlane")

	// Certificate (unstructured cert-manager GVK).
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: esoTenantClientCertName}, cert)).To(Succeed())
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	g.Expect(issuer).To(Equal(openBaoCAIssuerName))
	usages, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "usages")
	g.Expect(usages).To(ContainElement("client auth"))
	// secretName is the store↔cert linchpin: cert-manager writes the keypair (plus
	// ca.crt) into this Secret, and the store's CertSecretRef/KeySecretRef/CAProvider
	// all read esoTenantClientCertName below — a divergent secretName would leave the
	// store authenticating against a Secret cert-manager never populates.
	secretName, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	g.Expect(secretName).To(Equal(esoTenantClientCertName))
	g.Expect(metav1.GetControllerOf(cert)).NotTo(BeNil(), "Certificate must be owner-referenced")

	// SecretStore: vault provider authenticating as the eso-tenant role/SA, with
	// the server/mount copied from the SHARED store.
	store, err := getTenantStore(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the tenant SecretStore")
	g.Expect(store.Spec.Provider).NotTo(BeNil())
	g.Expect(store.Spec.Provider.Vault).NotTo(BeNil())
	g.Expect(store.Spec.Provider.Vault.Server).To(Equal("https://openbao.example.svc:8200"),
		"server must be sourced from the shared cluster store, not the tenant store")
	g.Expect(store.Spec.Provider.Vault.Path).NotTo(BeNil())
	g.Expect(*store.Spec.Provider.Vault.Path).To(Equal(esoTenantKVMountPath))
	g.Expect(store.Spec.Provider.Vault.Version).To(Equal(esov1.VaultKVStoreV2),
		"version must be set explicitly — no omitempty, so \"\" fails the CRD enum")
	g.Expect(store.Spec.Provider.Vault.Auth.Kubernetes.Path).To(Equal("kubernetes/management"))
	g.Expect(store.Spec.Provider.Vault.Auth.Kubernetes.Role).To(Equal(esoTenantVaultRole))
	g.Expect(store.Spec.Provider.Vault.Auth.Kubernetes.ServiceAccountRef.Name).To(Equal(esoTenantServiceAccountName))
	g.Expect(store.Spec.Provider.Vault.CAProvider.Name).To(Equal(esoTenantClientCertName))
	g.Expect(store.Spec.Provider.Vault.CAProvider.Key).To(Equal("ca.crt"))
	g.Expect(store.Spec.Provider.Vault.ClientTLS.CertSecretRef.Name).To(Equal(esoTenantClientCertName))
	g.Expect(store.Spec.Provider.Vault.ClientTLS.KeySecretRef.Name).To(Equal(esoTenantClientCertName))
	g.Expect(metav1.GetControllerOf(store)).NotTo(BeNil(), "SecretStore must be owner-referenced")

	// Condition: not Ready while the store has no Ready status.
	cond := esoTenantCondition(cp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
	g.Expect(cond.Message).To(ContainSubstring(esoTenantStoreName))
}

// TestReconcileESOTenantStore_ReadyWhenStoreReady when the tenant SecretStore
// already reports Ready, the sub-reconciler flips ESOTenantStoreReady=True and
// stops requeuing.
func TestReconcileESOTenantStore_ReadyWhenStoreReady(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	// Seed a Ready tenant SecretStore in the child namespace; the operator's
	// Server-Side Apply re-asserts the spec without clobbering the Ready status.
	readyStore := readyTenantSecretStore(esoTenantStoreName, childNamespace(cp), "", "")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore(), readyStore).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a Ready store must not requeue")

	cond := esoTenantCondition(cp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("ESOTenantStoreReady"))
}

// TestReconcileESOTenantStore_StoreRefOverridden an explicit spec.secretStoreRef
// opts out of the operator-provisioned store: nothing is provisioned and
// ESOTenantStoreReady is True with reason StoreRefOverridden.
func TestReconcileESOTenantStore_StoreRefOverridden(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindCluster,
		Name: "my-own-store",
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "the override path must not requeue")

	// No per-tenant objects were provisioned.
	_, err = getTenantStore(t, r, cp)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no tenant SecretStore must be provisioned under an explicit ref")
	sa := &corev1.ServiceAccount{}
	err = r.Get(context.Background(),
		types.NamespacedName{Namespace: childNamespace(cp), Name: esoTenantServiceAccountName}, sa)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no tenant ServiceAccount must be provisioned under an explicit ref")

	cond := esoTenantCondition(cp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("StoreRefOverridden"))
	g.Expect(cond.Message).To(ContainSubstring("my-own-store"))
}

// TestReconcileESOTenantStore_ProvisioningError when provisioning the per-tenant
// objects fails (here the ServiceAccount Server-Side Apply errors), the
// sub-reconciler surfaces the error and reports ESOTenantStoreReady=False with
// reason ProvisioningError — so a failed SA/Certificate/SecretStore apply is
// diagnosable from the CR status rather than silently swallowed.
func TestReconcileESOTenantStore_ProvisioningError(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	// Fail the ServiceAccount apply (the first object ensureESOTenantStoreObjects
	// writes via Server-Side Apply) so ensureESOTenantStoreObjects returns an error.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyClusterSecretStore()).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				return errors.New("apply refused")
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).To(HaveOccurred(), "a failed provisioning apply must surface as an error for backoff")
	g.Expect(res.IsZero()).To(BeTrue(), "the error path returns an empty Result")

	cond := esoTenantCondition(cp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ProvisioningError"))
	g.Expect(cond.ObservedGeneration).To(Equal(cp.Generation),
		"ProvisioningError must stamp ObservedGeneration for staleness detection")
}

// TestEffectiveControlPlaneStoreRef the nil default is the namespaced per-tenant
// store, an explicit ref wins, and an explicit ref with an empty Kind normalises
// to ClusterSecretStore (the webhook-bypass safety net).
func TestEffectiveControlPlaneStoreRef(t *testing.T) {
	g := NewGomegaWithT(t)

	// Default (nil) → per-tenant namespaced store.
	def := effectiveControlPlaneStoreRef(&c5c3v1alpha1.ControlPlane{})
	g.Expect(def.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(def.Name).To(Equal(esoTenantStoreName))

	// Explicit override wins.
	cp := &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
		SecretStoreRef: &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindCluster, Name: "shared"},
	}}
	g.Expect(effectiveControlPlaneStoreRef(cp).Name).To(Equal("shared"))
	g.Expect(effectiveControlPlaneStoreRef(cp).Kind).To(Equal(commonv1.SecretStoreKindCluster))

	// Empty-kind explicit ref normalises to the cluster kind.
	cpEmptyKind := &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
		SecretStoreRef: &commonv1.SecretStoreRefSpec{Name: "no-kind"},
	}}
	g.Expect(effectiveControlPlaneStoreRef(cpEmptyKind).Kind).To(Equal(commonv1.SecretStoreKindCluster))
}

// --- per-service namespaces (issue #646) ---

// TestReconcileESOTenantStore_ProvisionsAStorePerNamespace pins the secret-
// distribution mechanism: an ESO SecretStore and the Secrets it materialises are
// namespace-local, so a store in the ControlPlane's namespace cannot deliver
// anything into a service namespace. Each namespace hosting a service therefore
// gets its own tenant store trio — and the one in a service namespace carries the
// ownership labels rather than an owner reference.
func TestReconcileESOTenantStore_ProvisionsAStorePerNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "identity",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	for _, ns := range []string{"openstack", "identity"} {
		store := &esov1.SecretStore{}
		g.Expect(r.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: esoTenantStoreName}, store)).To(Succeed(),
			"every namespace hosting a service needs its own tenant store")

		sa := &corev1.ServiceAccount{}
		g.Expect(r.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: esoTenantServiceAccountName}, sa)).To(Succeed())

		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certificateGVK)
		g.Expect(r.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: esoTenantClientCertName}, cert)).To(Succeed())
		commonName, _, _ := unstructured.NestedString(cert.Object, "spec", "commonName")
		g.Expect(commonName).To(Equal(esoTenantClientCertName+"."+ns+".svc"),
			"each store's client cert must identify its own namespace")
	}

	// Ownership: owner reference at home, labels abroad.
	home := &esov1.SecretStore{}
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: "openstack", Name: esoTenantStoreName}, home)).To(Succeed())
	g.Expect(metav1.GetControllerOf(home)).NotTo(BeNil())

	abroad := &esov1.SecretStore{}
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: "identity", Name: esoTenantStoreName}, abroad)).To(Succeed())
	g.Expect(abroad.OwnerReferences).To(BeEmpty())
	g.Expect(abroad.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "controlplane"))
	g.Expect(abroad.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "openstack"))
}

// TestReconcileESOTenantStore_GatesOnEveryNamespace verifies the readiness gate
// aggregates: a store still issuing its client cert in a SERVICE namespace holds
// the condition False, and the message names that namespace — otherwise the
// admin-password ExternalSecret would be projected through a store that cannot
// deliver it.
func TestReconcileESOTenantStore_GatesOnEveryNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"},
		},
	}

	// Ready at home, absent (hence not ready) in the service namespace.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, readyTenantSecretStore(esoTenantStoreName, "openstack", "", "")).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(esoTenantStoreRequeueAfter))

	cond := esoTenantCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
	g.Expect(cond.Message).To(ContainSubstring(`namespace "identity"`))
}

// TestReconcileESOTenantStore_RefusesForeignStoreInExternalNamespace a SecretStore
// that merely shares the operator's fixed name (openbao-tenant-store) in an
// External-lifecycle service namespace, but carries no ControlPlane ownership
// labels, must NOT be adopted: the operator never created it. Adopting it would
// overwrite its provider spec to point at our OpenBao and, via the labels the
// projection would stamp, make the teardown residue sweep DELETE it. The
// sub-reconciler fails loud (mirroring reconcileNamespaces' NamespaceNotOwned) and
// leaves the foreign object untouched.
func TestReconcileESOTenantStore_RefusesForeignStoreInExternalNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "shared-tenant",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
	}
	// A store somebody else provisioned in the shared namespace, pointing at their
	// own OpenBao and carrying no ControlPlane ownership labels.
	foreign := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      esoTenantStoreName,
			Namespace: "shared-tenant",
			UID:       types.UID("foreign-store-uid"),
			Labels:    map[string]string{"owner": "someone-else"},
		},
		Spec: esov1.SecretStoreSpec{Provider: &esov1.SecretStoreProvider{Vault: &esov1.VaultProvider{
			Server: "https://not-ours.example.svc:8200",
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).To(HaveOccurred(), "must refuse to adopt a foreign store in an unowned namespace")
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt"))

	// Untouched: the foreign store still points where it did and never acquired the
	// ownership labels that would make the teardown sweep delete it.
	after := &esov1.SecretStore{}
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: "shared-tenant", Name: esoTenantStoreName}, after)).To(Succeed())
	g.Expect(after.Spec.Provider.Vault.Server).To(Equal("https://not-ours.example.svc:8200"),
		"foreign store spec must not be overwritten")
	g.Expect(after.Labels).NotTo(HaveKey(controlPlaneNameLabel))
	g.Expect(after.Labels).NotTo(HaveKey(controlPlaneNamespaceLabel))
	g.Expect(isControlPlaneChild(after, cp)).To(BeFalse(),
		"foreign store must not become a ControlPlane child, so the residue sweep leaves it alone")
}

// TestReconcileESOTenantStore_RefusesForeignCertInExternalNamespace is the
// Certificate twin of the store case: the eso-tenant-client-tls Certificate stays
// read-modify-write via CreateOrUpdate, so its ownership guard lives in
// refuseForeignAdoption. A same-named foreign Certificate in an External namespace
// must not be reshaped or stamped with our labels.
func TestReconcileESOTenantStore_RefusesForeignCertInExternalNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	cp := dbCredManagedControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "shared-tenant",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
	}
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(certificateGVK)
	foreign.SetName(esoTenantClientCertName)
	foreign.SetNamespace("shared-tenant")
	foreign.SetUID(types.UID("foreign-cert-uid"))
	foreign.SetLabels(map[string]string{"owner": "someone-else"})
	g.Expect(unstructured.SetNestedField(foreign.Object, "not-ours-issuer", "spec", "issuerRef", "name")).To(Succeed())
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileESOTenantStore(context.Background(), cp)
	g.Expect(err).To(HaveOccurred(), "must refuse to adopt a foreign Certificate in an unowned namespace")
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt"))

	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: "shared-tenant", Name: esoTenantClientCertName}, after)).To(Succeed())
	issuer, _, _ := unstructured.NestedString(after.Object, "spec", "issuerRef", "name")
	g.Expect(issuer).To(Equal("not-ours-issuer"), "foreign Certificate spec must not be overwritten")
	g.Expect(after.GetLabels()).NotTo(HaveKey(controlPlaneNameLabel))
	g.Expect(isControlPlaneChild(after, cp)).To(BeFalse())
}
