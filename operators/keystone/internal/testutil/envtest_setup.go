// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides Keystone-specific test utilities for envtest integration tests.
package testutil

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// SkipIfEnvTestUnavailable re-exports the common skip guard for envtest-based
// integration tests. Call as the first statement in each integration test function.
var SkipIfEnvTestUnavailable = commonenvtest.SkipIfEnvTestUnavailable

// SetupKeystoneEnvTest starts an envtest API server with the Keystone CRD installed,
// webhook server configured and running, and the Keystone defaulting/validating
// webhooks registered. It returns a controller-runtime client, a context,
// and its cancel function. The environment is torn down automatically via t.Cleanup().
//
// Parameters:
//   - addToScheme registers the Keystone API types with the runtime scheme.
//     Callers pass keystonev1alpha1.AddToScheme to avoid an import cycle between
//     the testutil package and the v1alpha1 package.
//   - registerWebhooks sets up webhook handlers with the controller-runtime manager.
//     Callers pass a closure that calls KeystoneWebhook.SetupWebhookWithManager(mgr).
//
// The scheme is local to this helper — SharedScheme() in internal/common is NOT modified.
func SetupKeystoneEnvTest(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := keystonePaths()

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "Keystone",
		Scheme:            buildScheme(addToScheme),
		CRDDirectoryPaths: []string{crdDir},
		WebhookDir:        webhookDir,
		RegisterWebhooks:  registerWebhooks,
	})
}

// SetupKeystoneEnvTestNoWebhook starts an envtest API server with only the
// Keystone CRD installed — no webhook configurations, no validating webhooks
// It returns a direct controller-runtime client so tests can submit
// CRs and observe exactly the schema-layer validation the API server enforces
// (kubebuilder validation markers + x-kubernetes-validations CEL rules)
// without the defense-in-depth validating webhook short-circuiting the
// rejection. Tear-down is wired via t.Cleanup().
//
// This is intended for tests that must distinguish a CRD CEL rejection from a
// webhook rejection — e.g. the CRD CEL rule on spec.database that
// requires caBundleSecretRef.name and clientCertSecretRef.name when
// tls.enabled is true. If the CEL rule were ever removed, the equivalent
// SetupKeystoneEnvTest-based test would silently keep passing because the
// validating webhook would still reject the CR. Using this helper makes the
// CRD-layer rule the only enforcement point in scope, so the test fails the
// moment that rule is removed.
func SetupKeystoneEnvTestNoWebhook(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, _ := keystonePaths()
	return commonenvtest.SetupEnvTestWithCRDs(t, buildScheme(addToScheme), []string{crdDir})
}

// keystonePaths returns absolute paths to the Keystone CRD and webhook
// configuration directories, resolved relative to this source file via
// runtime.Caller(0).
func keystonePaths() (crdDir, webhookDir string) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("testutil: runtime.Caller failed to determine source file path")
	}
	base := filepath.Dir(thisFile)
	crdDir = filepath.Join(base, "..", "..", "config", "crd", "bases")
	webhookDir = filepath.Join(base, "..", "..", "config", "webhook")
	return crdDir, webhookDir
}

// buildScheme creates a runtime.Scheme with core types, apiextensions, and the
// caller-provided types registered, via the shared commonenvtest.BuildScheme.
func buildScheme(addToScheme func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	return commonenvtest.BuildScheme(addToScheme)
}

// buildControllerScheme creates a runtime.Scheme that includes all types
// needed by the KeystoneReconciler: Keystone API types, core K8s types,
// and all external operator types (MariaDB, ESO, cert-manager).
// It is created fresh per test.
func buildControllerScheme(addToScheme func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	// The reconciler's external API groups (MariaDB, ESO v1/v1alpha1,
	// cert-manager, Gateway API) are exactly the shared common set.
	return commonenvtest.BuildScheme(append(commonenvtest.CommonExternalSchemes(), addToScheme)...)
}

// SetupMinimalEnvTest starts a minimal envtest API server with the Keystone
// CRD installed and returns a controller-runtime Manager whose scheme knows
// the Keystone API type. No webhooks, no fake external CRDs, no reconciler.
// The manager is NOT started — callers can invoke mgr.GetFieldIndexer() or
// other pre-Start APIs without incurring a background goroutine.
// Tear-down is wired via t.Cleanup.
//
// Intended for tests that need a real FieldIndexer against a live API server
// without paying for the full controller-registration helper
func SetupMinimalEnvTest(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
) (ctrl.Manager, context.Context, context.CancelFunc) {
	t.Helper()

	// keystonePaths() returns (crdDir, webhookDir string) — no error. The
	// blank identifier discards the webhook directory, which this minimal
	// setup does not install. Fail-fast on a missing CRD directory is already
	// covered by envtest's ErrorIfCRDPathMissing=true below.
	crdDir, _ := keystonePaths()
	return commonenvtest.SetupUnstartedManager(t, commonenvtest.UnstartedManagerConfig{
		Scheme:            buildScheme(addToScheme),
		CRDDirectoryPaths: []string{crdDir},
	})
}

// SetupKeystoneEnvTestWithController starts an envtest API server with the
// Keystone CRD, webhook configurations, fake CRDs for external operators
// (MariaDB, ESO, cert-manager), and a controller-runtime Manager running the
// KeystoneReconciler. It returns a direct (non-caching) client, a context,
// and its cancel function. The environment is torn down automatically via
// t.Cleanup().
//
// Parameters:
//   - addToScheme registers the Keystone API types with the runtime scheme.
//   - registerWebhooks sets up webhook handlers with the manager.
//   - registerController registers the KeystoneReconciler via SetupWithManager.
func SetupKeystoneEnvTestWithController(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
	registerController func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := keystonePaths()

	// Combine Keystone CRD dir with common fake CRD dirs.
	crdDirs := append([]string{crdDir}, commonenvtest.CommonFakeCRDDirs()...)

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:               "Keystone",
		Scheme:             buildControllerScheme(addToScheme),
		CRDDirectoryPaths:  crdDirs,
		WebhookDir:         webhookDir,
		RegisterWebhooks:   registerWebhooks,
		RegisterController: registerController,
	})
}
