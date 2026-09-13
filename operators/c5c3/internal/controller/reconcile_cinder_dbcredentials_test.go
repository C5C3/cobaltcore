// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Cinder DB-credentials concern
// (reconcile_cinder_dbcredentials.go): the target the service-agnostic builders
// in reconcile_dbcredentials.go consume, the two OpenBao handles it carries, and
// the per-service databaseCredentialsMode override plus the Static opt-out that
// decide the mode. Both run against a bare ControlPlane, so they stay independent
// of the projection that consumes the target.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

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
