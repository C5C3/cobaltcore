// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Placement DB-credentials concern
// (reconcile_placement_dbcredentials.go): the dynamic engine objects mirror the
// Keystone ones, placement-scoped, and the per-service databaseCredentialsMode
// override plus the Static opt-out decide the projection. The fixtures reuse
// placementControlPlane / newPlacementTestReconciler from reconcile_placement_test.go.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// getPlacementVDS fetches the projected Placement VaultDynamicSecret generator at
// its derived name/namespace.
func getPlacementVDS(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esgenv1alpha1.VaultDynamicSecret, error) {
	t.Helper()
	vds := &esgenv1alpha1.VaultDynamicSecret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: cp.PlacementNamespace(), Name: placementDBCredentialSecretName(cp)}, vds)
	return vds, err
}

// getPlacementDBCredES fetches the projected Placement DB-credential
// ExternalSecret at its derived name/namespace.
func getPlacementDBCredES(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esov1.ExternalSecret, error) {
	t.Helper()
	es := &esov1.ExternalSecret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: cp.PlacementNamespace(), Name: placementDBCredentialSecretName(cp)}, es)
	return es, err
}

// placementLeftoverClientCert builds the Placement mTLS client Certificate at its
// derived name/namespace, as a prior Dynamic deployment left it.
func placementLeftoverClientCert(cp *c5c3v1alpha1.ControlPlane) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(placementDBCredentialClientCertName(cp))
	cert.SetNamespace(cp.PlacementNamespace())
	return cert
}

// TestPlacementDBDynamicKeys_FollowThePlacementNamespace pins the re-keying of the
// two OpenBao handles onto the PLACEMENT service namespace: the engine role name,
// the dynamic creds path, and the static KV path all track it, so placement's
// engine plumbing is keystone-independent. The role name MUST stay in sync with
// setup-database-tenant.sh.
func TestPlacementDBDynamicKeys_FollowThePlacementNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := placementControlPlane() // no namespace assignment; placement shares "default"
	g.Expect(placementDBDynamicRoleFor(cp)).To(Equal("placement-default"),
		"an unassigned Placement keeps the ControlPlane-namespace-derived role name")
	g.Expect(placementDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/placement-default"))
	g.Expect(placementDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/placement/default/cp/db"))

	cp.Spec.Services.Placement.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "compute"}
	g.Expect(placementDBDynamicRoleFor(cp)).To(Equal("placement-compute"))
	g.Expect(placementDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/placement-compute"))
	g.Expect(placementDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/placement/compute/cp/db"))
}

// TestReconcilePlacement_DynamicDefaultProjectsEngineObjects verifies a managed
// shared placement database (default Dynamic) projects the generator-backed
// ExternalSecret (no static Data), the VaultDynamicSecret with role placement-db
// and the per-tenant creds path, the placement-db-creds ServiceAccount, and the
// mTLS client Certificate named <placement-name>-db-openbao-client.
func TestReconcilePlacement_DynamicDefaultProjectsEngineObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	r := newPlacementTestReconciler(t, cp)
	ctx := context.Background()

	g.Expect(placementDBCredentialsDynamicEnabled(cp)).To(BeTrue(),
		"a managed shared placement database defaults to Dynamic")

	_, err := r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// ExternalSecret: generator-backed, no static KV Data, no SecretStoreRef.
	es, err := getPlacementDBCredES(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the Placement DB-credential ExternalSecret")
	g.Expect(es.Spec.Data).To(BeEmpty(), "the Dynamic ExternalSecret must carry no static Data refs")
	g.Expect(es.Spec.SecretStoreRef.Name).To(BeEmpty(),
		"a generator-backed ExternalSecret must not reference a SecretStore")
	g.Expect(es.Spec.DataFrom).To(HaveLen(1))
	g.Expect(es.Spec.DataFrom[0].SourceRef).NotTo(BeNil())
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef).NotTo(BeNil())
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Kind).To(Equal("VaultDynamicSecret"))
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Name).To(Equal(placementDBCredentialSecretName(cp)))

	// VaultDynamicSecret: role placement-db, per-tenant creds path, same-namespace refs.
	vds, err := getPlacementVDS(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the Placement VaultDynamicSecret generator")
	g.Expect(vds.Spec.Path).To(Equal("database/mariadb/creds/placement-default"))
	g.Expect(vds.Spec.Method).To(Equal("GET"))
	g.Expect(vds.Spec.Provider).NotTo(BeNil())
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.Role).To(Equal(placementDBDynamicVaultRole))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.Role).To(Equal("placement-db"))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.ServiceAccountRef.Name).To(Equal(placementDBCredentialServiceAccountName))
	g.Expect(vds.Spec.Provider.CAProvider.Name).To(Equal(placementDBCredentialClientCertName(cp)))
	g.Expect(vds.Spec.Provider.ClientTLS.CertSecretRef.Name).To(Equal(placementDBCredentialClientCertName(cp)))
	g.Expect(vds.Spec.Provider.ClientTLS.KeySecretRef.Name).To(Equal(placementDBCredentialClientCertName(cp)))

	// ServiceAccount placement-db-creds — the name the placement-db auth role binds.
	g.Expect(placementDBCredentialServiceAccountName).To(Equal("placement-db-creds"))
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: cp.PlacementNamespace(), Name: placementDBCredentialServiceAccountName,
	}, sa)).To(Succeed())

	// Certificate <placement-name>-db-openbao-client with the CA issuer.
	g.Expect(placementDBCredentialClientCertName(cp)).To(Equal("cp-placement-db-openbao-client"))
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: cp.PlacementNamespace(), Name: placementDBCredentialClientCertName(cp),
	}, cert)).To(Succeed())
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	g.Expect(issuer).To(Equal(openBaoCAIssuerName))

	// The projected child carries Dynamic.
	pl := getProjectedPlacement(t, r.Client, cp)
	g.Expect(pl.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic))
}

// TestReconcilePlacement_StaticOptOutProjectsKVAndTearsDownDynamic verifies that
// both opt-out routes — the shared credentialsMode: Static and the per-service
// services.placement.databaseCredentialsMode: Static — project the KV-backed
// ExternalSecret, tear down any leftover generator objects, and stamp the child
// Static.
func TestReconcilePlacement_StaticOptOutProjectsKVAndTearsDownDynamic(t *testing.T) {
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
				cp.Spec.Services.Placement.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := placementControlPlane()
			tt.apply(cp)
			g.Expect(placementDBCredentialsDynamicEnabled(cp)).To(BeFalse())

			// Pre-seed the leftover generator, SA and mTLS client Certificate from a
			// prior Dynamic deployment, each carrying the ownership a live projection
			// stamps — the teardown is gated on it.
			s := placementTestScheme(t)
			target := placementDBCredentialTarget(cp)
			leftovers := []client.Object{
				dbCredentialVaultDynamicSecret(target, openBaoDefaultServer, openBaoDefaultKubernetesMount),
				dbCredentialServiceAccount(target),
				placementLeftoverClientCert(cp),
			}
			for _, obj := range leftovers {
				g.Expect(claimChildOwnership(localWriter(), cp, obj, s)).To(Succeed())
			}
			r := newPlacementTestReconciler(t, append([]client.Object{cp}, leftovers...)...)
			ctx := context.Background()

			_, err := r.reconcilePlacement(ctx, cp)
			g.Expect(err).NotTo(HaveOccurred())

			es, err := getPlacementDBCredES(t, r, cp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(es.Spec.DataFrom).To(BeEmpty(), "the Static opt-out must project the KV ExternalSecret")
			g.Expect(es.Spec.Data).To(HaveLen(2))
			g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(placementDBCredentialRemoteKeyFor(cp)))

			_, vdsErr := getPlacementVDS(t, r, cp)
			g.Expect(apierrors.IsNotFound(vdsErr)).To(BeTrue(),
				"the Static opt-out must delete the leftover VaultDynamicSecret")

			saErr := r.Get(ctx, types.NamespacedName{
				Namespace: cp.PlacementNamespace(), Name: placementDBCredentialServiceAccountName,
			}, &corev1.ServiceAccount{})
			g.Expect(apierrors.IsNotFound(saErr)).To(BeTrue(),
				"the Static opt-out must delete the generator's ServiceAccount")

			sweptCert := &unstructured.Unstructured{}
			sweptCert.SetGroupVersionKind(certificateGVK)
			certErr := r.Get(ctx, types.NamespacedName{
				Namespace: cp.PlacementNamespace(), Name: placementDBCredentialClientCertName(cp),
			}, sweptCert)
			g.Expect(apierrors.IsNotFound(certErr)).To(BeTrue(),
				"the Static opt-out must delete the leftover mTLS client Certificate")

			pl := getProjectedPlacement(t, r.Client, cp)
			g.Expect(pl.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))
		})
	}
}

// TestPlacementDBCredentialsDynamicEnabled_DedicatedIsStaticEvenWhenModeBypassed
// is the fail-safe twin: the validating webhook rejects a Dynamic override / mode
// on a dedicated placement database, but a webhook-bypassed CR must still fall
// closed onto Static rather than projecting a generator that could never sync (no
// engine role exists for a dedicated instance).
func TestPlacementDBCredentialsDynamicEnabled_DedicatedIsStaticEvenWhenModeBypassed(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Services.Placement.DedicatedBackingServices = &c5c3v1alpha1.PlacementDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-placement-db"},
			CredentialsMode: commonv1.CredentialsModeDynamic,
			Database:        "placement",
			SecretRef:       commonv1.SecretRefSpec{Name: "placement-db"},
		},
	}

	g.Expect(placementDBCredentialsDynamicEnabled(cp)).To(BeFalse(),
		"a dedicated placement database is never Dynamic, even with the mode written directly into the spec")
}

// TestReconcilePlacement_DynamicObjectsLandInThePlacementNamespace verifies every
// dynamic object lands beside the Placement child in a namespace of its own — the
// ServiceAccount whose token OpenBao authenticates, the mTLS client Certificate,
// the generator, and the ExternalSecret — carrying the ownership labels rather
// than an owner reference, and nothing is left in the ControlPlane's namespace.
func TestReconcilePlacement_DynamicObjectsLandInThePlacementNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Services.Placement.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newPlacementTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	es := &esov1.ExternalSecret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "compute", Name: placementDBCredentialSecretName(cp),
	}, es)).To(Succeed())
	g.Expect(es.OwnerReferences).To(BeEmpty(), "a cross-namespace object cannot carry an owner reference")
	g.Expect(es.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))

	vds := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "compute", Name: placementDBCredentialSecretName(cp),
	}, vds)).To(Succeed())
	g.Expect(vds.Spec.Path).To(Equal("database/mariadb/creds/placement-compute"),
		"the generator's per-tenant path follows the placement namespace")

	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "compute", Name: placementDBCredentialServiceAccountName,
	}, sa)).To(Succeed(), "the generator's SA must authenticate from the namespace the policy grants")

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "compute", Name: placementDBCredentialClientCertName(cp),
	}, cert)).To(Succeed())

	// Nothing may be left in the ControlPlane's own namespace.
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: placementDBCredentialSecretName(cp),
	}, &esov1.ExternalSecret{})).NotTo(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: placementDBCredentialSecretName(cp),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed())
}
