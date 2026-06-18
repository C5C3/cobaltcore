// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package envtest

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// expectedCRD describes a CRD that is expected to be installed by SetupEnvTest.
type expectedCRD struct {
	// name is the fully qualified CRD name (e.g. "mariadbs.k8s.mariadb.com").
	name string
	// group is the API group.
	group string
	// version is the served API version.
	version string
	// kind is the CRD kind.
	kind string
	// namespaced indicates whether the CRD is namespace-scoped.
	namespaced bool
}

// allExpectedCRDs returns the complete set of CRDs shipped in fake_crds/.
func allExpectedCRDs() []expectedCRD {
	return []expectedCRD{
		{name: "certificates.cert-manager.io", group: "cert-manager.io", version: "v1", kind: "Certificate", namespaced: true},
		{name: "clusterissuers.cert-manager.io", group: "cert-manager.io", version: "v1", kind: "ClusterIssuer", namespaced: false},
		{name: "clustersecretstores.external-secrets.io", group: "external-secrets.io", version: "v1", kind: "ClusterSecretStore", namespaced: false},
		{name: "databases.k8s.mariadb.com", group: "k8s.mariadb.com", version: "v1alpha1", kind: "Database", namespaced: true},
		{name: "externalsecrets.external-secrets.io", group: "external-secrets.io", version: "v1", kind: "ExternalSecret", namespaced: true},
		// K-ORC fake CRDs the c5c3 ControlPlane reconciler mints/owns
		// these openstack.k-orc.cloud kinds; they are faked here to keep envtest
		// forgiving (the real CRDs carry strict CEL rules). Domain and User are
		// imported as UNMANAGED resources to anchor the admin ApplicationCredential.
		{name: "applicationcredentials.openstack.k-orc.cloud", group: "openstack.k-orc.cloud", version: "v1alpha1", kind: "ApplicationCredential", namespaced: true},
		{name: "domains.openstack.k-orc.cloud", group: "openstack.k-orc.cloud", version: "v1alpha1", kind: "Domain", namespaced: true},
		{name: "endpoints.openstack.k-orc.cloud", group: "openstack.k-orc.cloud", version: "v1alpha1", kind: "Endpoint", namespaced: true},
		{name: "services.openstack.k-orc.cloud", group: "openstack.k-orc.cloud", version: "v1alpha1", kind: "Service", namespaced: true},
		{name: "users.openstack.k-orc.cloud", group: "openstack.k-orc.cloud", version: "v1alpha1", kind: "User", namespaced: true},
		{name: "grants.k8s.mariadb.com", group: "k8s.mariadb.com", version: "v1alpha1", kind: "Grant", namespaced: true},
		{name: "mariadbs.k8s.mariadb.com", group: "k8s.mariadb.com", version: "v1alpha1", kind: "MariaDB", namespaced: true},
		{name: "memcacheds.memcached.c5c3.io", group: "memcached.c5c3.io", version: "v1beta1", kind: "Memcached", namespaced: true},
		{name: "pushsecrets.external-secrets.io", group: "external-secrets.io", version: "v1alpha1", kind: "PushSecret", namespaced: true},
		{name: "rabbitmqclusters.rabbitmq.com", group: "rabbitmq.com", version: "v1beta1", kind: "RabbitmqCluster", namespaced: true},
		{name: "users.k8s.mariadb.com", group: "k8s.mariadb.com", version: "v1alpha1", kind: "User", namespaced: true},
	}
}

// TestSetupEnvTest_StartStop verifies that SetupEnvTest starts a working
// environment and returns non-nil client/context, and that teardown completes
// without errors and cancels the context.
func TestSetupEnvTest_StartStop(t *testing.T) {
	SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := SetupEnvTest(t)

	// Verify non-nil returns.
	g.Expect(c).NotTo(BeNil(), "SetupEnvTest returned nil client")
	g.Expect(ctx).NotTo(BeNil(), "SetupEnvTest returned nil context")
	g.Expect(cancel).NotTo(BeNil(), "SetupEnvTest returned nil cancel function")

	// Verify the context is alive before teardown.
	g.Expect(ctx.Err()).NotTo(HaveOccurred(), "context should not be cancelled yet")

	// Verify the client is functional by listing namespaces.
	nsList := &unstructured.UnstructuredList{}
	nsList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "NamespaceList",
	})
	g.Expect(c.List(ctx, nsList)).To(Succeed())
	g.Expect(nsList.Items).NotTo(BeEmpty(), "expected at least one namespace (default)")
}

// TestSetupEnvTest_CRDsInstalled verifies that all fake CRDs from the
// fake_crds/ directory are installed and discoverable via the API server
func TestSetupEnvTest_CRDsInstalled(t *testing.T) {
	SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := SetupEnvTest(t)

	// List all CRDs from the API server.
	crdList := &apiextensionsv1.CustomResourceDefinitionList{}
	g.Expect(c.List(ctx, crdList)).To(Succeed())

	// Build a set of installed CRD names for lookup.
	installed := make(map[string]bool, len(crdList.Items))
	for _, crd := range crdList.Items {
		installed[crd.Name] = true
	}

	for _, ec := range allExpectedCRDs() {
		g.Expect(installed).To(HaveKey(ec.name), "expected CRD %q to be installed", ec.name)
	}
}

// TestSetupEnvTest_CreateUnstructuredObjects verifies that the client can
// create an unstructured custom resource for each installed fake CRD kind
func TestSetupEnvTest_CreateUnstructuredObjects(t *testing.T) {
	SkipIfEnvTestUnavailable(t)

	c, ctx, _ := SetupEnvTest(t)

	const testNamespace = "default"

	for _, ec := range allExpectedCRDs() {
		t.Run(ec.kind, func(t *testing.T) {
			g := NewGomegaWithT(t)

			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   ec.group,
				Version: ec.version,
				Kind:    ec.kind,
			})

			objName := fmt.Sprintf("test-%s", dasherize(ec.kind))
			obj.SetName(objName)

			if ec.namespaced {
				obj.SetNamespace(testNamespace)
			}

			// Create the object.
			g.Expect(c.Create(ctx, obj)).To(Succeed(), "failed to create %s %q", ec.kind, objName)

			// Verify we can get the object back.
			key := types.NamespacedName{
				Name:      objName,
				Namespace: obj.GetNamespace(),
			}
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(obj.GroupVersionKind())
			g.Expect(c.Get(ctx, key, got)).To(Succeed(), "failed to get created %s %q", ec.kind, objName)
			g.Expect(got.GetName()).To(Equal(objName))
		})
	}
}

// TestSetupEnvTest_TeardownCancelsContext verifies that after the test cleanup
// runs (which tears down envtest), the context returned by SetupEnvTest is
// cancelled.
func TestSetupEnvTest_TeardownCancelsContext(t *testing.T) {
	SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	var capturedCtx context.Context

	// Run SetupEnvTest in a sub-test so that t.Cleanup runs when the
	// sub-test finishes, simulating the full teardown lifecycle.
	t.Run("setup_and_teardown", func(t *testing.T) {
		g := NewGomegaWithT(t)
		_, ctx, _ := SetupEnvTest(t)
		capturedCtx = ctx

		// Context should be alive inside the sub-test.
		g.Expect(ctx.Err()).NotTo(HaveOccurred(), "context should be alive inside sub-test")
	})

	// After the sub-test completes, t.Cleanup() has run, which calls
	// cancel() and env.Stop(). The context should now be done.
	g.Expect(capturedCtx).NotTo(BeNil(), "capturedCtx was never assigned; sub-test may not have run")
	g.Expect(capturedCtx.Err()).To(HaveOccurred(), "expected context to be cancelled after teardown")
}

// dasherize converts a PascalCase or camelCase string to a lowercase
// dash-separated string suitable for Kubernetes object names.
func dasherize(s string) string {
	var result []byte
	for i, c := range []byte(s) {
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				result = append(result, '-')
			}
			result = append(result, c+('a'-'A'))
		} else {
			result = append(result, c)
		}
	}
	return string(result)
}
