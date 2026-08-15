// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Glance DB-credentials concern (reconcile_glance_dbcredentials.go):
// the dynamic engine objects mirror the Keystone ones, glance-scoped, and the
// per-service databaseCredentialsMode override plus the Static opt-out decide the
// projection. The fixtures reuse glanceControlPlane / newGlanceTestReconciler from
// reconcile_glance_test.go.
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

	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// getGlanceVDS fetches the projected Glance VaultDynamicSecret generator at its
// derived name/namespace.
func getGlanceVDS(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esgenv1alpha1.VaultDynamicSecret, error) {
	t.Helper()
	vds := &esgenv1alpha1.VaultDynamicSecret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: cp.GlanceNamespace(), Name: glanceDBCredentialSecretName(cp)}, vds)
	return vds, err
}

// getGlanceDBCredES fetches the projected Glance DB-credential ExternalSecret at
// its derived name/namespace.
func getGlanceDBCredES(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (*esov1.ExternalSecret, error) {
	t.Helper()
	es := &esov1.ExternalSecret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: cp.GlanceNamespace(), Name: glanceDBCredentialSecretName(cp)}, es)
	return es, err
}

// TestGlanceDBDynamicKeys_FollowTheGlanceNamespace pins the re-keying of the two
// OpenBao handles onto the GLANCE service namespace: the engine role name, the
// dynamic creds path, and the static KV path all track it, so glance's engine
// plumbing is keystone-independent. The role name MUST stay in sync with
// setup-database-tenant.sh.
func TestGlanceDBDynamicKeys_FollowTheGlanceNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := glanceControlPlane() // no namespace assignment; glance shares "default"
	g.Expect(glanceDBDynamicRoleFor(cp)).To(Equal("glance-default"),
		"an unassigned Glance keeps the ControlPlane-namespace-derived role name")
	g.Expect(glanceDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/glance-default"))
	g.Expect(glanceDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/glance/default/cp/db"))

	cp.Spec.Services.Glance.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "images"}
	g.Expect(glanceDBDynamicRoleFor(cp)).To(Equal("glance-images"))
	g.Expect(glanceDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/glance-images"))
	g.Expect(glanceDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/glance/images/cp/db"))
}

// TestReconcileGlance_DynamicDefaultProjectsEngineObjects verifies a managed
// shared glance database (default Dynamic) projects the generator-backed
// ExternalSecret (no static Data), the VaultDynamicSecret with role glance-db and
// the per-tenant creds path, the glance-db-creds ServiceAccount, and the mTLS
// client Certificate named <glance-name>-db-openbao-client.
func TestReconcileGlance_DynamicDefaultProjectsEngineObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	g.Expect(glanceDBCredentialsDynamicEnabled(cp)).To(BeTrue(),
		"a managed shared glance database defaults to Dynamic")

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// ExternalSecret: generator-backed, no static KV Data, no SecretStoreRef.
	es, err := getGlanceDBCredES(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the Glance DB-credential ExternalSecret")
	g.Expect(es.Spec.Data).To(BeEmpty(), "the Dynamic ExternalSecret must carry no static Data refs")
	g.Expect(es.Spec.SecretStoreRef.Name).To(BeEmpty(),
		"a generator-backed ExternalSecret must not reference a SecretStore")
	g.Expect(es.Spec.DataFrom).To(HaveLen(1))
	g.Expect(es.Spec.DataFrom[0].SourceRef).NotTo(BeNil())
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef).NotTo(BeNil())
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Kind).To(Equal("VaultDynamicSecret"))
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Name).To(Equal(glanceDBCredentialSecretName(cp)))

	// VaultDynamicSecret: role glance-db, per-tenant creds path, same-namespace refs.
	vds, err := getGlanceVDS(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred(), "operator must create the Glance VaultDynamicSecret generator")
	g.Expect(vds.Spec.Path).To(Equal("database/mariadb/creds/glance-default"))
	g.Expect(vds.Spec.Method).To(Equal("GET"))
	g.Expect(vds.Spec.Provider).NotTo(BeNil())
	g.Expect(vds.Spec.Provider.Version).To(Equal(esov1.VaultKVStoreV2),
		"version must be set explicitly — no omitempty, so \"\" fails the CRD enum")
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.Role).To(Equal(glanceDBDynamicVaultRole))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.Role).To(Equal("glance-db"))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.ServiceAccountRef.Name).To(Equal(glanceDBCredentialServiceAccountName))
	g.Expect(vds.Spec.Provider.CAProvider.Name).To(Equal(glanceDBCredentialClientCertName(cp)))
	g.Expect(vds.Spec.Provider.ClientTLS.CertSecretRef.Name).To(Equal(glanceDBCredentialClientCertName(cp)))
	g.Expect(vds.Spec.Provider.ClientTLS.KeySecretRef.Name).To(Equal(glanceDBCredentialClientCertName(cp)))

	// ServiceAccount glance-db-creds.
	g.Expect(glanceDBCredentialServiceAccountName).To(Equal("glance-db-creds"))
	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: cp.GlanceNamespace(), Name: glanceDBCredentialServiceAccountName,
	}, sa)).To(Succeed())

	// Certificate <glance-name>-db-openbao-client with the CA issuer.
	g.Expect(glanceDBCredentialClientCertName(cp)).To(Equal("cp-glance-db-openbao-client"))
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: cp.GlanceNamespace(), Name: glanceDBCredentialClientCertName(cp),
	}, cert)).To(Succeed())
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	g.Expect(issuer).To(Equal(openBaoCAIssuerName))
	commonName, _, _ := unstructured.NestedString(cert.Object, "spec", "commonName")
	g.Expect(commonName).To(Equal(glanceDBCredentialClientCertName(cp) + ".default.svc"))

	// The projected child carries Dynamic.
	gl := getProjectedGlance(t, r.Client, cp)
	g.Expect(gl.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic))
}

// TestReconcileGlance_StaticOptOutProjectsKVAndTearsDownDynamic verifies that both
// opt-out routes — the shared credentialsMode: Static and the per-service
// services.glance.databaseCredentialsMode: Static — project the KV-backed
// ExternalSecret, tear down any leftover generator objects, and stamp the child
// Static.
func TestReconcileGlance_StaticOptOutProjectsKVAndTearsDownDynamic(t *testing.T) {
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
				cp.Spec.Services.Glance.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := glanceControlPlane()
			tt.apply(cp)
			g.Expect(glanceDBCredentialsDynamicEnabled(cp)).To(BeFalse())

			// Pre-seed the leftover generator, SA and mTLS client Certificate from a
			// prior Dynamic deployment, each carrying the ownership a live projection
			// stamps — the teardown is gated on it.
			s := glanceTestScheme(t)
			target := glanceDBCredentialTarget(cp)
			leftovers := []client.Object{
				dbCredentialVaultDynamicSecret(target, openBaoDefaultServer, openBaoDefaultKubernetesMount),
				dbCredentialServiceAccount(target),
				glanceLeftoverClientCert(cp),
			}
			for _, obj := range leftovers {
				g.Expect(claimChildOwnership(localWriter(), cp, obj, s)).To(Succeed())
			}
			r := newGlanceTestReconciler(t, append([]client.Object{cp}, leftovers...)...)
			ctx := context.Background()

			_, err := r.reconcileGlance(ctx, cp)
			g.Expect(err).NotTo(HaveOccurred())

			es, err := getGlanceDBCredES(t, r, cp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(es.Spec.DataFrom).To(BeEmpty(), "the Static opt-out must project the KV ExternalSecret")
			g.Expect(es.Spec.Data).To(HaveLen(2))
			g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(glanceDBCredentialRemoteKeyFor(cp)))

			_, vdsErr := getGlanceVDS(t, r, cp)
			g.Expect(apierrors.IsNotFound(vdsErr)).To(BeTrue(),
				"the Static opt-out must delete the leftover VaultDynamicSecret")

			saErr := r.Get(ctx, types.NamespacedName{
				Namespace: cp.GlanceNamespace(), Name: glanceDBCredentialServiceAccountName,
			}, &corev1.ServiceAccount{})
			g.Expect(apierrors.IsNotFound(saErr)).To(BeTrue(),
				"the Static opt-out must delete the generator's ServiceAccount")

			sweptCert := &unstructured.Unstructured{}
			sweptCert.SetGroupVersionKind(certificateGVK)
			certErr := r.Get(ctx, types.NamespacedName{
				Namespace: cp.GlanceNamespace(), Name: glanceDBCredentialClientCertName(cp),
			}, sweptCert)
			g.Expect(apierrors.IsNotFound(certErr)).To(BeTrue(),
				"the Static opt-out must delete the leftover mTLS client Certificate")

			gl := getProjectedGlance(t, r.Client, cp)
			g.Expect(gl.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))
		})
	}
}

// TestReconcileGlance_StaticOptOutLeavesForeignObjectsAlone is the ownership twin
// of the teardown test above: the Static branch's names are fixed and
// non-CP-derived (glance-db-creds, <glance>-db-openbao-client), so in a
// pre-existing service namespace the operator does not own they can collide with
// somebody else's objects. The mode flip must leave those alone — the operator
// already refuses to ADOPT a foreign object here, so deleting one would be the
// same clobber from the teardown side, and on every reconcile pass.
func TestReconcileGlance_StaticOptOutLeavesForeignObjectsAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := glanceControlPlane()
	cp.Spec.Services.Glance.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	cp.Spec.Services.Glance.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "images", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
	}

	// Same names, no ownership: the platform team's own objects in a namespace
	// that happens to host Glance.
	target := glanceDBCredentialTarget(cp)
	foreign := []client.Object{
		dbCredentialVaultDynamicSecret(target, openBaoDefaultServer, openBaoDefaultKubernetesMount),
		dbCredentialServiceAccount(target),
		glanceLeftoverClientCert(cp),
	}
	r := newGlanceTestReconciler(t, append([]client.Object{cp}, foreign...)...)

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: "images", Name: glanceDBCredentialSecretName(cp)},
		&esgenv1alpha1.VaultDynamicSecret{})).To(Succeed(),
		"a foreign VaultDynamicSecret colliding on the name must survive the Static flip")
	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: "images", Name: glanceDBCredentialServiceAccountName},
		&corev1.ServiceAccount{})).To(Succeed(),
		"a foreign glance-db-creds ServiceAccount must survive the Static flip")
	survivingCert := &unstructured.Unstructured{}
	survivingCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: "images", Name: glanceDBCredentialClientCertName(cp)},
		survivingCert)).To(Succeed(),
		"a foreign mTLS client Certificate colliding on the name must survive the Static flip")
}

// glanceLeftoverClientCert builds the Glance mTLS client Certificate at its
// derived name/namespace, as a prior Dynamic deployment left it.
func glanceLeftoverClientCert(cp *c5c3v1alpha1.ControlPlane) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(glanceDBCredentialClientCertName(cp))
	cert.SetNamespace(cp.GlanceNamespace())
	return cert
}

// TestGlanceDBCredentialsDynamicEnabled_DedicatedIsStaticEvenWhenModeBypassed is
// the fail-safe twin: the validating webhook rejects a Dynamic override / mode on
// a dedicated glance database, but a webhook-bypassed CR must still fall closed
// onto Static rather than projecting a generator that could never sync (no engine
// role exists for a dedicated instance).
func TestGlanceDBCredentialsDynamicEnabled_DedicatedIsStaticEvenWhenModeBypassed(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.DedicatedBackingServices = &c5c3v1alpha1.GlanceDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-glance-db"},
			CredentialsMode: commonv1.CredentialsModeDynamic,
			Database:        "glance",
			SecretRef:       commonv1.SecretRefSpec{Name: "glance-db"},
		},
	}

	g.Expect(glanceDBCredentialsDynamicEnabled(cp)).To(BeFalse(),
		"a dedicated glance database is never Dynamic, even with the mode written directly into the spec")
}

// TestReconcileGlance_DynamicObjectsLandInTheGlanceNamespace verifies every dynamic
// object lands beside the Glance child in a namespace of its own — the
// ServiceAccount whose token OpenBao authenticates, the mTLS client Certificate,
// the generator, and the ExternalSecret — carrying the ownership labels rather
// than an owner reference, and nothing is left in the ControlPlane's namespace.
func TestReconcileGlance_DynamicObjectsLandInTheGlanceNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := glanceControlPlane()
	cp.Spec.Services.Glance.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "images", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newGlanceTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileGlance(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	es := &esov1.ExternalSecret{}
	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: "images", Name: glanceDBCredentialSecretName(cp)}, es)).To(Succeed())
	g.Expect(es.OwnerReferences).To(BeEmpty(), "a cross-namespace object cannot carry an owner reference")
	g.Expect(es.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))

	vds := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: "images", Name: glanceDBCredentialSecretName(cp)}, vds)).To(Succeed())
	g.Expect(vds.Spec.Path).To(Equal("database/mariadb/creds/glance-images"),
		"the generator's per-tenant path follows the glance namespace")

	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: "images", Name: glanceDBCredentialServiceAccountName}, sa)).
		To(Succeed(), "the generator's SA must authenticate from the namespace the policy grants")

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: "images", Name: glanceDBCredentialClientCertName(cp)}, cert)).To(Succeed())

	// Nothing may be left in the ControlPlane's own namespace.
	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: "default", Name: glanceDBCredentialSecretName(cp)},
		&esov1.ExternalSecret{})).NotTo(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: "default", Name: glanceDBCredentialSecretName(cp)},
		&esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed())
}
