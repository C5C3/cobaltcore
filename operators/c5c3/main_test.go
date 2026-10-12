// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the c5c3 operator entrypoint wiring. These assert
// the manager's scheme and leader-election identity WITHOUT standing up a live
// cluster or envtest: the package-level scheme is populated at init time, so every
// API type the reconcilers create/own/watch must resolve to a registered GVK,
// and the leader-election ID must stay pinned to the deploy-stack value.
package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/yaml"
)

// TestSchemeRegistersAllExpectedGVKs asserts the package-level scheme — built by
// bootstrap.NewScheme and handed to bootstrap.Run — recognises every API kind the
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
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneService"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneServiceList"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneUser"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneUserList"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneProject"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneProjectList"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneRoleAssignment"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneRoleAssignmentList"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneCatalogEntry"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneCatalogEntryList"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneApplicationCredential"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneApplicationCredentialList"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "MariaDBDatabase"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "MariaDBDatabaseList"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "RabbitMQVhost"},
		{Group: "c5c3.io", Version: "v1alpha1", Kind: "RabbitMQVhostList"},
		// Keystone child CR.
		{Group: "keystone.openstack.c5c3.io", Version: "v1alpha1", Kind: "Keystone"},
		// MariaDB child CR.
		{Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "MariaDB"},
		// ESO: PushSecret (v1alpha1) and ClusterSecretStore/ExternalSecret (v1).
		{Group: "external-secrets.io", Version: "v1alpha1", Kind: "PushSecret"},
		{Group: "external-secrets.io", Version: "v1", Kind: "ClusterSecretStore"},
		{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"},
		// K-ORC: the admin ApplicationCredential plus every kind the ControlPlane
		// and KeystoneService reconcilers project and Own.
		{Group: "openstack.k-orc.cloud", Version: "v1alpha1", Kind: "ApplicationCredential"},
		{Group: "openstack.k-orc.cloud", Version: "v1alpha1", Kind: "Service"},
		{Group: "openstack.k-orc.cloud", Version: "v1alpha1", Kind: "Endpoint"},
		{Group: "openstack.k-orc.cloud", Version: "v1alpha1", Kind: "User"},
		{Group: "openstack.k-orc.cloud", Version: "v1alpha1", Kind: "Domain"},
		{Group: "openstack.k-orc.cloud", Version: "v1alpha1", Kind: "Project"},
		{Group: "openstack.k-orc.cloud", Version: "v1alpha1", Kind: "Role"},
		{Group: "openstack.k-orc.cloud", Version: "v1alpha1", Kind: "RoleAssignment"},
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
// accidentally registers it, this test flags the drift so the comment on the
// NewScheme argument list can be revisited.
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

// TestAssignedNamespaceKindsAreTheOrderKindsAndTheSecret pins the kinds whose
// target-cluster informers reach an owner's namespace: the seven order kinds and
// Secret. A kind missing here leaves its orders unread on a target.
func TestAssignedNamespaceKindsAreTheOrderKindsAndTheSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	gvks := make([]schema.GroupVersionKind, 0, len(assignedNamespaceKinds))
	for _, obj := range assignedNamespaceKinds {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		g.Expect(err).NotTo(HaveOccurred(), "the scheme must resolve %T", obj)
		gvks = append(gvks, gvk)
	}

	order := func(kind string) schema.GroupVersionKind {
		return schema.GroupVersionKind{Group: "c5c3.io", Version: "v1alpha1", Kind: kind}
	}
	g.Expect(gvks).To(ConsistOf(
		order("KeystoneUser"),
		order("KeystoneProject"),
		order("KeystoneRoleAssignment"),
		order("KeystoneCatalogEntry"),
		order("KeystoneApplicationCredential"),
		order("MariaDBDatabase"),
		order("RabbitMQVhost"),
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"},
	))
}

// assignedRolePath is the access chart's template of the Role every assigned
// namespace on a target gets.
const assignedRolePath = "../../deploy/target-cluster/target-cluster-access/templates/role-assigned.yaml"

// TestAssignedNamespaceKindsAreReadableUnderTheAssignedRole checks every kind in
// assignedNamespaceKinds against the rules of the access chart's assigned Role.
// A kind the Role does not let the operator get, list and watch has an informer
// that is forbidden in every assigned namespace and never syncs. The engagement
// does not wait for that informer, so the operator engages the target and only
// the reads and watches of that kind fail there. The Role grants read on the
// mariadb-operator's databases as well, which the list leaves out on purpose.
func TestAssignedNamespaceKindsAreReadableUnderTheAssignedRole(t *testing.T) {
	g := NewGomegaWithT(t)

	raw, err := os.ReadFile(assignedRolePath)
	g.Expect(err).NotTo(HaveOccurred())

	// What is left without the template's doc comment and every line carrying
	// an action is the Role the range renders per namespace, minus its name,
	// namespace and labels.
	text := string(raw)
	if end := strings.Index(text, "*/}}"); end >= 0 {
		text = text[end+len("*/}}"):]
	}
	var kept []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "{{") || strings.TrimSpace(line) == "---" {
			continue
		}
		kept = append(kept, line)
	}
	role := &rbacv1.Role{}
	g.Expect(yaml.Unmarshal([]byte(strings.Join(kept, "\n")), role)).To(Succeed())
	g.Expect(role.Kind).To(Equal("Role"), "the template should still render a Role")

	grants := func(group, resource string) bool {
		for _, rule := range role.Rules {
			if slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) &&
				slices.Contains(rule.Verbs, "get") && slices.Contains(rule.Verbs, "list") &&
				slices.Contains(rule.Verbs, "watch") {
				return true
			}
		}
		return false
	}
	for _, obj := range assignedNamespaceKinds {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		g.Expect(err).NotTo(HaveOccurred(), "the scheme must resolve %T", obj)
		plural, _ := meta.UnsafeGuessKindToResource(gvk)
		resource := plural.Resource
		g.Expect(grants(gvk.Group, resource)).To(BeTrue(),
			"the assigned Role in %s should grant get, list and watch on %q in group %q",
			assignedRolePath, resource, gvk.Group)
	}
}
