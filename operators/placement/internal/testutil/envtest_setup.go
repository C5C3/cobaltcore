// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides Placement-specific test utilities for envtest integration tests.
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

	commonenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// SkipIfEnvTestUnavailable re-exports the common skip guard for envtest-based
// integration tests. Call as the first statement in each integration test function.
var SkipIfEnvTestUnavailable = commonenvtest.SkipIfEnvTestUnavailable

// SetupPlacementEnvTest starts an envtest API server with the Placement CRD
// installed, the webhook server configured and running, and the caller-registered
// defaulting/validating webhooks. It returns a direct (non-caching)
// controller-runtime client, a context, and its cancel function. The environment
// is torn down automatically via t.Cleanup().
//
// Parameters:
//   - addToScheme registers the Placement API types with the runtime scheme.
//     Callers pass placementv1alpha1.AddToScheme to avoid an import cycle between
//     the testutil package and the v1alpha1 package.
//   - registerWebhooks sets up the webhook handler with the manager. The webhook
//     manifests installed by envtest carry the Placement entries with
//     failurePolicy=Fail, so the callback MUST serve the handler or admission of
//     every Placement fails.
//
// The scheme is local to this helper — internal/common's SharedScheme is NOT
// modified.
func SetupPlacementEnvTest(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := placementPaths()

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "Placement",
		Scheme:            commonenvtest.BuildScheme(addToScheme),
		CRDDirectoryPaths: []string{crdDir},
		WebhookDir:        webhookDir,
		RegisterWebhooks:  registerWebhooks,
	})
}

// SetupPlacementEnvTestNoWebhook starts an envtest API server with only the
// Placement CRD installed — no webhook configurations, no webhooks. It returns a
// direct controller-runtime client so tests can submit CRs and observe exactly
// the schema-layer validation the API server enforces (kubebuilder validation
// markers + x-kubernetes-validations CEL rules) without the defense-in-depth
// webhooks short-circuiting the rejection or filling defaults. Tear-down is
// wired via t.Cleanup().
//
// This is intended for tests that must attribute a rejection to the CRD layer
// alone. Several placement rules — the openStackRelease pattern, the
// keystoneEndpoint scheme, the uWSGI keep-alive cross-field rule — are enforced
// both by the schema and by the validating webhook. If the schema rule were
// dropped, the equivalent SetupPlacementEnvTest-based test could silently keep
// passing because the webhook would still reject the CR; using this helper makes
// the CRD-layer rule the only enforcement point in scope.
func SetupPlacementEnvTestNoWebhook(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, _ := placementPaths()
	return commonenvtest.SetupEnvTestWithCRDs(t, commonenvtest.BuildScheme(addToScheme), []string{crdDir})
}

// SetupPlacementEnvTestWithController starts an envtest API server with the
// Placement CRD, webhook configurations, fake CRDs for external operators
// (MariaDB, ESO, Gateway API, ...), and a controller-runtime Manager hosting the
// caller-registered webhook and reconciler. It returns a direct (non-caching)
// client, a context, and its cancel function. The environment is torn down
// automatically via t.Cleanup().
//
// Parameters:
//   - addToScheme registers the Placement API types with the runtime scheme.
//   - registerWebhooks sets up the webhook handler with the manager.
//   - registerController wires the PlacementReconciler onto the manager.
func SetupPlacementEnvTestWithController(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
	registerController func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := placementPaths()

	// Combine the Placement CRD dir with the common fake CRD dirs (ESO,
	// gateway-api, mariadb, ...) so the reconciler's external kinds resolve.
	crdDirs := append([]string{crdDir}, commonenvtest.CommonFakeCRDDirs()...)

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:               "Placement",
		Scheme:             buildControllerScheme(addToScheme),
		CRDDirectoryPaths:  crdDirs,
		WebhookDir:         webhookDir,
		RegisterWebhooks:   registerWebhooks,
		RegisterController: registerController,
	})
}

// placementPaths returns absolute paths to the Placement CRD and webhook
// configuration directories, resolved relative to this source file via
// runtime.Caller(0).
func placementPaths() (crdDir, webhookDir string) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("testutil: runtime.Caller failed to determine source file path")
	}
	base := filepath.Dir(thisFile)
	crdDir = filepath.Join(base, "..", "..", "config", "crd", "bases")
	webhookDir = filepath.Join(base, "..", "..", "config", "webhook")
	return crdDir, webhookDir
}

// buildControllerScheme creates a runtime.Scheme that includes all types the
// PlacementReconciler needs: Placement API types, core K8s types, ESO
// (credential gates and store watches), Gateway API (HTTPRoute), and MariaDB
// (database provisioning and the cluster watch). It is created fresh per test.
func buildControllerScheme(addToScheme func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	return commonenvtest.BuildScheme(
		// External operator types the reconciler registers.
		esov1.AddToScheme,
		gatewayv1.Install,
		mariadbv1alpha1.AddToScheme,
		// Placement types.
		addToScheme,
	)
}
