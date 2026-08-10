// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Barbican DB-credentials concern
// (reconcile_barbican_dbcredentials.go): the target the service-agnostic builders
// in reconcile_dbcredentials.go consume, the two OpenBao handles it carries, and
// the per-service databaseCredentialsMode override plus the Static opt-out that
// decide the mode. The concern is pure data — no Barbican sub-reconciler consumes
// it yet — so the fixture is a bare ControlPlane rather than a fake client.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// barbicanDBCredentialsControlPlane builds a ControlPlane on the managed SHARED
// database with a Barbican service block, the shape the Dynamic default applies
// to. The dedicated secret store is the cheapest of the two required
// spec.services.barbican.secretStore modes; the DB-credential concern reads
// neither.
func barbicanDBCredentialsControlPlane() *c5c3v1alpha1.ControlPlane {
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
				Barbican: &c5c3v1alpha1.ServiceBarbicanSpec{
					SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
						Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
					},
				},
			},
		},
	}
}

// TestBarbicanDBDynamicKeys_FollowTheBarbicanNamespace pins the re-keying of the
// two OpenBao handles onto the BARBICAN service namespace: the engine role name,
// the dynamic creds path, and the static KV path all track it, so barbican's
// engine plumbing is keystone-independent. The role name MUST stay in sync with
// setup-database-tenant.sh.
func TestBarbicanDBDynamicKeys_FollowTheBarbicanNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := barbicanDBCredentialsControlPlane() // no namespace assignment; barbican shares "default"
	g.Expect(barbicanDBDynamicRoleFor(cp)).To(Equal("barbican-default"),
		"an unassigned Barbican keeps the ControlPlane-namespace-derived role name")
	g.Expect(barbicanDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/barbican-default"))
	g.Expect(barbicanDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/barbican/default/cp/db"))

	cp.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "secrets"}
	g.Expect(barbicanDBDynamicRoleFor(cp)).To(Equal("barbican-secrets"))
	g.Expect(barbicanDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/barbican-secrets"))
	g.Expect(barbicanDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/barbican/secrets/cp/db"))
}

// TestBarbicanDBCredentialTarget_CarriesTheBarbicanInputs pins every field the
// service-agnostic builders read off the target: the fixed ServiceAccount name the
// barbican-db auth role binds, that role, the two per-ControlPlane object names
// derived from the projected Barbican child, and the two OpenBao paths.
func TestBarbicanDBCredentialTarget_CarriesTheBarbicanInputs(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanDBCredentialsControlPlane()

	// The names the barbican-db role binds in setup-auth.sh.
	g.Expect(barbicanDBCredentialServiceAccountName).To(Equal("barbican-db-creds"))
	g.Expect(barbicanDBDynamicVaultRole).To(Equal("barbican-db"))
	// The two per-ControlPlane object names track the projected Barbican child.
	g.Expect(barbicanDBCredentialSecretName(cp)).To(Equal("cp-barbican-db-credentials"))
	g.Expect(barbicanDBCredentialClientCertName(cp)).To(Equal("cp-barbican-db-openbao-client"))

	target := barbicanDBCredentialTarget(cp)
	g.Expect(target.qualifier).To(Equal("Barbican"))
	g.Expect(target.prefix()).To(Equal("Barbican "), "the target must name Barbican in the messages it emits")
	g.Expect(target.namespace).To(Equal("default"))
	g.Expect(target.secretName).To(Equal(barbicanDBCredentialSecretName(cp)))
	g.Expect(target.certName).To(Equal(barbicanDBCredentialClientCertName(cp)))
	g.Expect(target.saName).To(Equal(barbicanDBCredentialServiceAccountName))
	g.Expect(target.vaultRole).To(Equal(barbicanDBDynamicVaultRole))
	g.Expect(target.credsPath).To(Equal("database/mariadb/creds/barbican-default"))
	g.Expect(target.kvPath).To(Equal(barbicanDBCredentialRemoteKeyFor(cp)))
	g.Expect(target.storeRef).To(Equal(effectiveControlPlaneStoreRef(cp)))
}

// TestBarbicanDBCredentialTarget_FollowsTheAssignedNamespace verifies that a
// services.barbican.namespace assignment moves the whole concern with the service:
// every dynamic object is built in the namespace the generator's policy grants,
// and both OpenBao handles re-key onto it.
func TestBarbicanDBCredentialTarget_FollowsTheAssignedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanDBCredentialsControlPlane()
	cp.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "secrets", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}

	target := barbicanDBCredentialTarget(cp)
	g.Expect(target.namespace).To(Equal("secrets"), "every dynamic object lands beside the Barbican child")
	g.Expect(target.credsPath).To(Equal("database/mariadb/creds/barbican-secrets"))
	g.Expect(target.kvPath).To(Equal("openstack/barbican/secrets/cp/db"))
	// The object names are ControlPlane-derived, so the move leaves them untouched.
	g.Expect(target.secretName).To(Equal("cp-barbican-db-credentials"))
	g.Expect(target.certName).To(Equal("cp-barbican-db-openbao-client"))
	g.Expect(target.saName).To(Equal("barbican-db-creds"),
		"the generator's SA keeps the fixed name the barbican-db role binds in any namespace")
}

// TestBarbicanDBCredentialsDynamicEnabled covers the mode predicate on both
// values: the managed shared default is Dynamic, and each of the four routes onto
// the Static branch — the shared opt-out, the per-service override, a brownfield
// shared database, and a dedicated barbican database — falls closed.
func TestBarbicanDBCredentialsDynamicEnabled(t *testing.T) {
	for _, tt := range []struct {
		name    string
		apply   func(cp *c5c3v1alpha1.ControlPlane)
		dynamic bool
	}{
		{
			name:    "managed shared database defaults to Dynamic",
			apply:   func(*c5c3v1alpha1.ControlPlane) {},
			dynamic: true,
		},
		{
			name: "per-service Dynamic override wins over the shared Static mode",
			apply: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeStatic
				cp.Spec.Services.Barbican.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
			},
			dynamic: true,
		},
		{
			name: "shared credentialsMode Static",
			apply: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeStatic
			},
		},
		{
			name: "per-service databaseCredentialsMode Static",
			apply: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Barbican.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
			},
		},
		{
			name: "brownfield shared database has no engine to issue from",
			apply: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure.Database.ClusterRef = nil
			},
		},
		{
			// The validating webhook rejects a Dynamic mode on a dedicated barbican
			// database; a webhook-bypassed CR must still fall closed onto Static
			// rather than projecting a generator that could never sync (no engine
			// role exists for a dedicated instance).
			name: "dedicated barbican database, even with Dynamic written into the spec",
			apply: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Barbican.DedicatedBackingServices = &c5c3v1alpha1.BarbicanDedicatedBackingServicesSpec{
					Database: &commonv1.DatabaseSpec{
						ClusterRef:      &corev1.LocalObjectReference{Name: "cp-barbican-db"},
						CredentialsMode: commonv1.CredentialsModeDynamic,
						Database:        "barbican",
						SecretRef:       commonv1.SecretRefSpec{Name: "barbican-db"},
					},
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := barbicanDBCredentialsControlPlane()
			tt.apply(cp)
			g.Expect(barbicanDBCredentialsDynamicEnabled(cp)).To(Equal(tt.dynamic))
		})
	}
}
