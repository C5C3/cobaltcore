// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides Neutron-specific test utilities for envtest integration tests.
package testutil

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// SkipIfEnvTestUnavailable re-exports the common skip guard for envtest-based
// integration tests. Call as the first statement in each integration test function.
var SkipIfEnvTestUnavailable = commonenvtest.SkipIfEnvTestUnavailable

// SetupNeutronEnvTest starts an envtest API server with the Neutron and
// NeutronMetadataAgent CRDs installed, the webhook server configured and running,
// and the caller-registered defaulting/validating webhooks. It returns a direct
// (non-caching) controller-runtime client, a context, and its cancel function.
// The environment is torn down automatically via t.Cleanup().
//
// Parameters:
//   - addToScheme registers the Neutron API types with the runtime scheme.
//     Callers pass neutronv1alpha1.AddToScheme to avoid an import cycle between
//     the testutil package and the v1alpha1 package.
//   - registerWebhooks sets up webhook handlers with the manager. The webhook
//     manifests installed by envtest carry BOTH the Neutron and
//     NeutronMetadataAgent entries (failurePolicy=Fail), so the callback MUST
//     serve both handlers or admission of the unserved kind fails.
//
// The scheme is local to this helper: internal/common's SharedScheme is NOT
// modified.
func SetupNeutronEnvTest(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := neutronPaths()

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "Neutron",
		Scheme:            commonenvtest.BuildScheme(addToScheme),
		CRDDirectoryPaths: []string{crdDir},
		WebhookDir:        webhookDir,
		RegisterWebhooks:  registerWebhooks,
	})
}

// SetupNeutronEnvTestNoWebhook starts an envtest API server with the Neutron and
// NeutronMetadataAgent CRDs and the sibling OVN CRDs installed, and no webhook
// configurations or webhooks at all. It returns a direct controller-runtime
// client so tests can submit CRs and observe exactly the schema-layer validation
// the API server enforces (kubebuilder validation markers +
// x-kubernetes-validations CEL rules) without the defense-in-depth webhooks
// short-circuiting the rejection or filling defaults. Tear-down is wired via
// t.Cleanup().
//
// This is intended for tests that must attribute a rejection to the CRD layer
// alone. Most Neutron rules are enforced twice, by the schema and by the
// validating webhook. If a CEL rule were dropped, the equivalent
// SetupNeutronEnvTest-based test could silently keep passing because the webhook
// would still reject the CR; using this helper makes the CRD-layer rule the only
// enforcement point in scope.
//
// Both Neutron kinds reference the OVN kinds by name only, so the OVN CRD
// directory is not needed to admit them. It is installed anyway so a controller
// test can create the OVNCentral or OVNChassis a reconciler resolves.
func SetupNeutronEnvTestNoWebhook(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, _ := neutronPaths()
	return commonenvtest.SetupEnvTestWithCRDs(t,
		buildControllerScheme(addToScheme), []string{crdDir, ovnCRDDir()})
}

// SetupNeutronEnvTestWithController starts an envtest API server with the
// Neutron and NeutronMetadataAgent CRDs, the sibling OVN CRDs, webhook
// configurations, fake CRDs for external operators (MariaDB, ESO, Gateway API,
// ...), and a controller-runtime Manager hosting the caller-registered webhooks
// and reconcilers. It returns a direct (non-caching) client, a context, and its
// cancel function. The environment is torn down automatically via t.Cleanup().
//
// Parameters:
//   - addToScheme registers the Neutron API types with the runtime scheme.
//   - registerWebhooks sets up both webhook handlers with the manager.
//   - registerController wires the NeutronReconciler and the
//     NeutronMetadataAgentReconciler onto the manager (both controllers run in
//     one manager, a second reconciler rather than a second binary). The callback
//     owns reconciler construction, so a test that needs a specific resolver or
//     controller options injects them there.
func SetupNeutronEnvTestWithController(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
	registerController func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := neutronPaths()

	// Combine the Neutron and OVN CRD dirs with the common fake CRD dirs (ESO,
	// gateway-api, mariadb, ...) so the reconcilers' external kinds resolve.
	crdDirs := append([]string{crdDir, ovnCRDDir()}, commonenvtest.CommonFakeCRDDirs()...)

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:               "Neutron",
		Scheme:             buildControllerScheme(addToScheme),
		CRDDirectoryPaths:  crdDirs,
		WebhookDir:         webhookDir,
		RegisterWebhooks:   registerWebhooks,
		RegisterController: registerController,
	})
}

// neutronPaths returns absolute paths to the Neutron CRD and webhook
// configuration directories, resolved relative to this source file via
// runtime.Caller(0).
func neutronPaths() (crdDir, webhookDir string) {
	base := callerDir()
	crdDir = filepath.Join(base, "..", "..", "config", "crd", "bases")
	webhookDir = filepath.Join(base, "..", "..", "config", "webhook")
	return crdDir, webhookDir
}

// ovnCRDDir returns the absolute path to the sibling OVN operator's CRD
// directory, which carries the OVNCentral and OVNChassis kinds both Neutron
// kinds reference.
func ovnCRDDir() string {
	return filepath.Join(callerDir(), "..", "..", "..", "ovn", "config", "crd", "bases")
}

// callerDir returns the directory this source file lives in, the anchor every
// path above is resolved against.
func callerDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("testutil: runtime.Caller failed to determine source file path")
	}
	return filepath.Dir(thisFile)
}

// buildControllerScheme creates a runtime.Scheme that includes all types the
// Neutron and NeutronMetadataAgent reconcilers need: Neutron API types, core K8s
// types, ESO (credential gates), Gateway API (HTTPRoute), MariaDB (database
// provisioning and the cluster watch), and the OVN types the two refs resolve to.
// It is created fresh per test.
func buildControllerScheme(addToScheme func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	return commonenvtest.BuildScheme(
		// External operator types the reconcilers register.
		esov1.AddToScheme,
		gatewayv1.Install,
		mariadbv1alpha1.AddToScheme,
		// Sibling operator types the two refs resolve to.
		ovnv1alpha1.AddToScheme,
		// Neutron types.
		addToScheme,
	)
}
