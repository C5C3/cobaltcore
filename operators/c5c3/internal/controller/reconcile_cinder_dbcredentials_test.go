// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Cinder DB-credentials concern
// (reconcile_cinder_dbcredentials.go): the target the service-agnostic builders
// in reconcile_dbcredentials.go consume, the two OpenBao handles it carries, the
// per-service databaseCredentialsMode override plus the Static opt-out that decide
// the mode, and the objects reconcileCinder projects from them. The handle tests
// run against a bare ControlPlane; the reconcile-driven ones reuse
// cinderControlPlane / newCinderTestReconciler from reconcile_cinder_test.go.
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
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// getCinderVDS fetches the projected Cinder VaultDynamicSecret generator at its
// derived name/namespace.
func getCinderVDS(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esgenv1alpha1.VaultDynamicSecret, error) {
	t.Helper()
	vds := &esgenv1alpha1.VaultDynamicSecret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: cp.CinderNamespace(), Name: cinderDBCredentialSecretName(cp)}, vds)
	return vds, err
}

// getCinderDBCredES fetches the projected Cinder DB-credential ExternalSecret at
// its derived name/namespace.
func getCinderDBCredES(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esov1.ExternalSecret, error) {
	t.Helper()
	es := &esov1.ExternalSecret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: cp.CinderNamespace(), Name: cinderDBCredentialSecretName(cp)}, es)
	return es, err
}

// cinderLeftoverClientCert builds the Cinder mTLS client Certificate at its derived
// name/namespace, as a prior Dynamic deployment left it.
func cinderLeftoverClientCert(cp *c5c3v1alpha1.ControlPlane) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(cinderDBCredentialClientCertName(cp))
	cert.SetNamespace(cp.CinderNamespace())
	return cert
}

// cinderDBCredentialsControlPlane builds a ControlPlane on the managed SHARED
// database with a Cinder service block, the shape the Dynamic default applies to.
// The backend list is required on the block; the DB-credential concern reads it
// nowhere.
func cinderDBCredentialsControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default", Generation: 1},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
					Database:   "keystone",
					SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
				},
			},
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
				Cinder: &c5c3v1alpha1.ServiceCinderSpec{
					Backends: []c5c3v1alpha1.CinderBackendEntry{{
						Name: "nfs1",
						Type: "NFS",
						NFS:  &c5c3v1alpha1.NFSShareSpec{Server: "nfs.example.com", Path: "/exports/cinder"},
					}},
				},
			},
		},
	}
}

// TestCinderDBDynamicKeys_FollowTheCinderNamespace pins the re-keying of the two
// OpenBao handles onto the CINDER service namespace: the engine role name, the
// dynamic creds path, and the static KV path all track it, so cinder's engine
// plumbing is keystone-independent. The role name MUST stay in sync with
// setup-database-tenant.sh. It also pins every field the service-agnostic
// builders read off the target, including the fixed ServiceAccount name the
// cinder-db auth role binds in setup-auth.sh.
func TestCinderDBDynamicKeys_FollowTheCinderNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := cinderDBCredentialsControlPlane() // no namespace assignment; cinder shares "default"
	g.Expect(cinderDBDynamicRoleFor(cp)).To(Equal("cinder-default"),
		"an unassigned Cinder keeps the ControlPlane-namespace-derived role name")
	g.Expect(cinderDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/cinder-default"))
	g.Expect(cinderDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/cinder/default/cp/db"))

	colocated := cinderDBCredentialTarget(cp)
	g.Expect(colocated.namespace).To(Equal("default"),
		"a co-located Cinder keeps its credential material in the ControlPlane's namespace")
	g.Expect(colocated.credsPath).To(Equal("database/mariadb/creds/cinder-default"))
	g.Expect(colocated.kvPath).To(Equal("openstack/cinder/default/cp/db"))

	// A services.cinder.namespace assignment moves the whole concern with the
	// service: both OpenBao handles re-key onto it and every object is built in
	// the namespace the generator's policy grants.
	cp.Spec.Services.Cinder.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "block", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	g.Expect(cinderDBDynamicRoleFor(cp)).To(Equal("cinder-block"))
	g.Expect(cinderDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/cinder-block"))
	g.Expect(cinderDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/cinder/block/cp/db"))

	// The names the cinder-db role binds in setup-auth.sh, and the two
	// per-ControlPlane object names tracking the projected Cinder child. Both names
	// are ControlPlane-derived, so the move leaves them untouched.
	g.Expect(cinderDBCredentialServiceAccountName).To(Equal("cinder-db-creds"))
	g.Expect(cinderDBDynamicVaultRole).To(Equal("cinder-db"))
	g.Expect(cinderDBCredentialSecretName(cp)).To(Equal("cp-cinder-db-credentials"))
	g.Expect(cinderDBCredentialClientCertName(cp)).To(Equal("cp-cinder-db-openbao-client"))

	target := cinderDBCredentialTarget(cp)
	g.Expect(target.qualifier).To(Equal("Cinder"))
	g.Expect(target.prefix()).To(Equal("Cinder "), "the target must name Cinder in the messages it emits")
	g.Expect(target.namespace).To(Equal("block"), "every dynamic object lands beside the Cinder child")
	g.Expect(target.secretName).To(Equal("cp-cinder-db-credentials"))
	g.Expect(target.certName).To(Equal("cp-cinder-db-openbao-client"))
	g.Expect(target.saName).To(Equal("cinder-db-creds"),
		"the generator's SA keeps the fixed name the cinder-db role binds in any namespace")
	g.Expect(target.vaultRole).To(Equal("cinder-db"))
	g.Expect(target.credsPath).To(Equal("database/mariadb/creds/cinder-block"))
	g.Expect(target.kvPath).To(Equal("openstack/cinder/block/cp/db"))
	g.Expect(target.storeRef).To(Equal(effectiveControlPlaneStoreRef(cp)))
}

// TestCinderDBCredentialsDynamicEnabled_DedicatedIsStaticEvenWhenModeBypassed is
// the fail-safe twin: the validating webhook rejects a Dynamic override / mode on
// a dedicated cinder database, but a webhook-bypassed CR must still fall closed
// onto Static rather than projecting a generator that could never sync (no engine
// role exists for a dedicated instance). The other closed branches are pinned
// beside it: an explicit Static override, the shared block's own Static opt-out,
// a brownfield shared database with no cluster to issue against, and a CR whose
// database does not resolve at all.
func TestCinderDBCredentialsDynamicEnabled_DedicatedIsStaticEvenWhenModeBypassed(t *testing.T) {
	g := NewGomegaWithT(t)

	dedicated := cinderDBCredentialsControlPlane()
	dedicated.Spec.Services.Cinder.DedicatedBackingServices = &c5c3v1alpha1.CinderDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-cinder-db"},
			CredentialsMode: commonv1.CredentialsModeDynamic,
			Database:        "cinder",
			SecretRef:       commonv1.SecretRefSpec{Name: "cinder-db"},
		},
	}
	g.Expect(cinderDBCredentialsDynamicEnabled(dedicated)).To(BeFalse(),
		"a dedicated cinder database is never Dynamic, even with the mode written directly into the spec")

	static := cinderDBCredentialsControlPlane()
	static.Spec.Services.Cinder.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	g.Expect(cinderDBCredentialsDynamicEnabled(static)).To(BeFalse(),
		"a per-service Static override opts Cinder out of the engine-issued credential")

	sharedStatic := cinderDBCredentialsControlPlane()
	sharedStatic.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeStatic
	g.Expect(cinderDBCredentialsDynamicEnabled(sharedStatic)).To(BeFalse(),
		"with no per-service override Cinder inherits the shared Static opt-out")

	brownfield := cinderDBCredentialsControlPlane()
	brownfield.Spec.Infrastructure.Database.ClusterRef = nil
	g.Expect(cinderDBCredentialsDynamicEnabled(brownfield)).To(BeFalse(),
		"an unmanaged shared database has no engine role to issue credentials from")

	unresolvable := cinderDBCredentialsControlPlane()
	unresolvable.Spec.Infrastructure = nil
	unresolvable.Spec.Services.Cinder = nil
	g.Expect(cinderDBCredentialsDynamicEnabled(unresolvable)).To(BeFalse(),
		"with no cinder block and no infrastructure nothing resolves, so the predicate falls closed")

	shared := cinderDBCredentialsControlPlane()
	g.Expect(cinderDBCredentialsDynamicEnabled(shared)).To(BeTrue(),
		"a managed shared database with no per-service override inherits the ControlPlane-wide Dynamic default")
}

// TestReconcileCinder_DynamicDefaultProjectsEngineObjects verifies a managed shared
// cinder database (default Dynamic) projects the generator-backed ExternalSecret (no
// static Data), the VaultDynamicSecret with role cinder-db and the per-tenant creds
// path, the cinder-db-creds ServiceAccount, and the mTLS client Certificate named
// <cinder-name>-db-openbao-client.
func TestReconcileCinder_DynamicDefaultProjectsEngineObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	g.Expect(cinderDBCredentialsDynamicEnabled(cp)).To(BeTrue(),
		"a managed shared cinder database defaults to Dynamic")

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// ExternalSecret: generator-backed, no static KV Data, no SecretStoreRef.
	es, err := getCinderDBCredES(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the Cinder DB-credential ExternalSecret")
	g.Expect(es.Spec.Data).To(BeEmpty(), "the Dynamic ExternalSecret must carry no static Data refs")
	g.Expect(es.Spec.SecretStoreRef.Name).To(BeEmpty(),
		"a generator-backed ExternalSecret must not reference a SecretStore")
	g.Expect(es.Spec.DataFrom).To(HaveLen(1))
	g.Expect(es.Spec.DataFrom[0].SourceRef).NotTo(BeNil())
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef).NotTo(BeNil())
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Kind).To(Equal("VaultDynamicSecret"))
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Name).To(Equal(cinderDBCredentialSecretName(cp)))

	// VaultDynamicSecret: role cinder-db, per-tenant creds path, same-namespace refs.
	vds, err := getCinderVDS(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the Cinder VaultDynamicSecret generator")
	g.Expect(vds.Spec.Path).To(Equal("database/mariadb/creds/cinder-default"))
	g.Expect(vds.Spec.Method).To(Equal("GET"))
	g.Expect(vds.Spec.Provider).NotTo(BeNil())
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.Role).To(Equal(cinderDBDynamicVaultRole))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.Role).To(Equal("cinder-db"))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.ServiceAccountRef.Name).To(Equal(cinderDBCredentialServiceAccountName))
	g.Expect(vds.Spec.Provider.CAProvider.Name).To(Equal(cinderDBCredentialClientCertName(cp)))
	g.Expect(vds.Spec.Provider.ClientTLS.CertSecretRef.Name).To(Equal(cinderDBCredentialClientCertName(cp)))
	g.Expect(vds.Spec.Provider.ClientTLS.KeySecretRef.Name).To(Equal(cinderDBCredentialClientCertName(cp)))

	// ServiceAccount cinder-db-creds, the name the cinder-db auth role binds.
	g.Expect(cinderDBCredentialServiceAccountName).To(Equal("cinder-db-creds"))
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: cp.CinderNamespace(), Name: cinderDBCredentialServiceAccountName,
	}, sa)).To(Succeed())

	// Certificate <cinder-name>-db-openbao-client with the CA issuer.
	g.Expect(cinderDBCredentialClientCertName(cp)).To(Equal("cp-cinder-db-openbao-client"))
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: cp.CinderNamespace(), Name: cinderDBCredentialClientCertName(cp),
	}, cert)).To(Succeed())
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	g.Expect(issuer).To(Equal(openBaoCAIssuerName))

	// The projected child carries Dynamic.
	cn := getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic))
}

// TestReconcileCinder_StaticOptOutProjectsKVAndTearsDownDynamic verifies that both
// opt-out routes, the shared credentialsMode: Static and the per-service
// services.cinder.databaseCredentialsMode: Static, project the KV-backed
// ExternalSecret, tear down any leftover generator objects, and stamp the child
// Static.
func TestReconcileCinder_StaticOptOutProjectsKVAndTearsDownDynamic(t *testing.T) {
	for _, tt := range []struct {
		name  string
		apply func(cp *c5c3v1alpha1.ControlPlane)
	}{
		{
			name: "shared credentialsMode Static",
			apply: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeStatic
			},
		},
		{
			name: "per-service databaseCredentialsMode Static",
			apply: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Cinder.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := cinderControlPlane()
			tt.apply(cp)
			g.Expect(cinderDBCredentialsDynamicEnabled(cp)).To(BeFalse())

			// Pre-seed the leftover generator, SA and mTLS client Certificate from a
			// prior Dynamic deployment, each carrying the ownership a live projection
			// stamps: the teardown is gated on it.
			s := cinderTestScheme(t)
			target := cinderDBCredentialTarget(cp)
			leftovers := []client.Object{
				dbCredentialVaultDynamicSecret(target, openBaoDefaultServer, openBaoDefaultKubernetesMount),
				dbCredentialServiceAccount(target),
				cinderLeftoverClientCert(cp),
			}
			for _, obj := range leftovers {
				g.Expect(claimChildOwnership(localWriter(), cp, obj, s)).To(Succeed())
			}
			r := newCinderTestReconciler(t, append([]client.Object{cp}, leftovers...)...)
			ctx := context.Background()

			_, err := r.reconcileCinder(ctx, cp)
			g.Expect(err).NotTo(HaveOccurred())

			es, err := getCinderDBCredES(t, r, cp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(es.Spec.DataFrom).To(BeEmpty(), "the Static opt-out must project the KV ExternalSecret")
			g.Expect(es.Spec.Data).To(HaveLen(2))
			g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(cinderDBCredentialRemoteKeyFor(cp)))

			_, vdsErr := getCinderVDS(t, r, cp)
			g.Expect(apierrors.IsNotFound(vdsErr)).To(BeTrue(),
				"the Static opt-out must delete the leftover VaultDynamicSecret")

			saErr := r.Get(ctx, types.NamespacedName{
				Namespace: cp.CinderNamespace(), Name: cinderDBCredentialServiceAccountName,
			}, &corev1.ServiceAccount{})
			g.Expect(apierrors.IsNotFound(saErr)).To(BeTrue(),
				"the Static opt-out must delete the generator's ServiceAccount")

			sweptCert := &unstructured.Unstructured{}
			sweptCert.SetGroupVersionKind(certificateGVK)
			certErr := r.Get(ctx, types.NamespacedName{
				Namespace: cp.CinderNamespace(), Name: cinderDBCredentialClientCertName(cp),
			}, sweptCert)
			g.Expect(apierrors.IsNotFound(certErr)).To(BeTrue(),
				"the Static opt-out must delete the leftover mTLS client Certificate")

			cn := getProjectedCinder(t, r.Client, cp)
			g.Expect(cn.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))
		})
	}
}

// TestReconcileCinder_DynamicObjectsLandInTheCinderNamespace verifies every dynamic
// object lands beside the Cinder child in a namespace of its own: the ServiceAccount
// whose token OpenBao authenticates, the mTLS client Certificate, the generator, and
// the ExternalSecret, carrying the ownership labels rather than an owner reference,
// and nothing is left in the ControlPlane's namespace.
func TestReconcileCinder_DynamicObjectsLandInTheCinderNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "block", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	es := &esov1.ExternalSecret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "block", Name: cinderDBCredentialSecretName(cp),
	}, es)).To(Succeed())
	g.Expect(es.OwnerReferences).To(BeEmpty(), "a cross-namespace object cannot carry an owner reference")
	g.Expect(es.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))

	vds := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "block", Name: cinderDBCredentialSecretName(cp),
	}, vds)).To(Succeed())
	g.Expect(vds.Spec.Path).To(Equal("database/mariadb/creds/cinder-block"),
		"the generator's per-tenant path follows the cinder namespace")

	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "block", Name: cinderDBCredentialServiceAccountName,
	}, sa)).To(Succeed(), "the generator's SA must authenticate from the namespace the policy grants")
	g.Expect(sa.Name).To(Equal("cinder-db-creds"))

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "block", Name: cinderDBCredentialClientCertName(cp),
	}, cert)).To(Succeed())

	// Nothing may be left in the ControlPlane's own namespace.
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: cinderDBCredentialSecretName(cp),
	}, &esov1.ExternalSecret{})).NotTo(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: cinderDBCredentialSecretName(cp),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed())
}
