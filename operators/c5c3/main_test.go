// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the c5c3 operator entrypoint wiring (CC-0110, REQ-019). These assert
// the manager's scheme and leader-election identity WITHOUT standing up a live
// cluster or envtest: the package-level scheme is populated by init(), so every
// API type the reconcilers create/own/watch must resolve to a registered GVK,
// and the leader-election ID must stay pinned to the deploy-stack value.
package main

import (
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestSchemeRegistersAllExpectedGVKs asserts the package-level scheme — built in
// init() and handed to bootstrap.Run — recognises every API kind the
// ControlPlane and CredentialRotation reconcilers create, own, or watch. A
// missing AddToScheme would otherwise only surface at runtime as a "no kind is
// registered for the type ..." crash on the first reconcile.
func TestSchemeRegistersAllExpectedGVKs(t *testing.T) {
	g := NewGomegaWithT(t)

	expected := []schema.GroupVersionKind{
		// c5c3 own types.
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "ControlPlane"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "ControlPlaneList"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "CredentialRotation"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "CredentialRotationList"},
		// Keystone child CR.
		{Group: "keystone.openstack.c5c3.io", Version: "v1alpha1", Kind: "Keystone"},
		// MariaDB child CR.
		{Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "MariaDB"},
		// ESO: PushSecret (v1alpha1) and ClusterSecretStore/ExternalSecret (v1).
		{Group: "external-secrets.io", Version: "v1alpha1", Kind: "PushSecret"},
		{Group: "external-secrets.io", Version: "v1", Kind: "ClusterSecretStore"},
		{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"},
		// K-ORC: ApplicationCredential, Service, Endpoint.
		{Group: "openstack.k-orc.cloud", Version: "v1alpha1", Kind: "ApplicationCredential"},
		{Group: "openstack.k-orc.cloud", Version: "v1alpha1", Kind: "Service"},
		{Group: "openstack.k-orc.cloud", Version: "v1alpha1", Kind: "Endpoint"},
		// Core client-go type the Secret watch and admin-secret reads rely on.
		{Group: "", Version: "v1", Kind: "Secret"},
	}

	for _, gvk := range expected {
		g.Expect(scheme.Recognizes(gvk)).To(BeTrue(),
			"scheme must recognise %s (a reconciler creates/owns/watches it)", gvk)
	}
}

// TestSchemeDoesNotRegisterMemcached documents the DECISION that the Memcached CR
// (memcached.c5c3.io) is deliberately NOT registered: it ships no Go module, so
// SetupWithManager's Owns(unstructured) resolves the GVK via the cluster
// RESTMapper at runtime rather than via the typed scheme. If a future change
// accidentally registers it, this test flags the drift so the comment in init()
// can be revisited.
func TestSchemeDoesNotRegisterMemcached(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(scheme.Recognizes(schema.GroupVersionKind{
		Group: "memcached.c5c3.io", Version: "v1beta1", Kind: "Memcached",
	})).To(BeFalse(),
		"Memcached must NOT be in the scheme; it is owned as unstructured via the RESTMapper")
}

// TestLeaderElectionIDPinned asserts the leader-election lock identity stays the
// deploy-stack value. A rename would let two managers both believe they hold the
// lock (different lock names), so this value is contractually frozen.
func TestLeaderElectionIDPinned(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(leaderElectionID).To(Equal("c5c3.openstack.c5c3.io"),
		"LeaderElectionID must stay pinned to the deploy-stack value")
}
