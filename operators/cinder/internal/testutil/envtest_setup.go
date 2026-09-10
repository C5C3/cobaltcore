// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides Cinder-specific test utilities for envtest integration tests.
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
)

// SkipIfEnvTestUnavailable re-exports the common skip guard for envtest-based
// integration tests. Call as the first statement in each integration test function.
var SkipIfEnvTestUnavailable = commonenvtest.SkipIfEnvTestUnavailable

// SetupCinderEnvTest starts an envtest API server with the Cinder,
// CinderBackend and CinderBackupBackend CRDs installed, the webhook server
// configured and running, and the caller-registered defaulting/validating
// webhooks. It returns a direct (non-caching) controller-runtime client, a
// context, and its cancel function. The environment is torn down automatically
// via t.Cleanup().
//
// Parameters:
//   - addToScheme registers the Cinder API types with the runtime scheme.
//     Callers pass cinderv1alpha1.AddToScheme to avoid an import cycle between
//     the testutil package and the v1alpha1 package.
//   - registerWebhooks sets up webhook handlers with the manager. The webhook
//     manifests installed by envtest carry all three kinds (failurePolicy=Fail),
//     so the callback MUST serve all three handlers or admission of an unserved
//     kind fails.
//
// The scheme is local to this helper: internal/common's SharedScheme is NOT
// modified.
func SetupCinderEnvTest(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := cinderPaths()

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "Cinder",
		Scheme:            commonenvtest.BuildScheme(addToScheme),
		CRDDirectoryPaths: []string{crdDir},
		WebhookDir:        webhookDir,
		RegisterWebhooks:  registerWebhooks,
	})
}

// SetupCinderEnvTestNoWebhook starts an envtest API server with only the three
// Cinder CRDs installed — no webhook configurations, no webhooks. It returns a
// direct controller-runtime client so tests can submit CRs and observe exactly
// the schema-layer validation the API server enforces (kubebuilder validation
// markers + x-kubernetes-validations CEL rules) without the defense-in-depth
// webhooks short-circuiting the rejection or filling defaults. Tear-down is
// wired via t.Cleanup().
//
// This is intended for tests that must attribute a rejection to the CRD layer
// alone — the cinderRef/type transition rules, the two single-writer rules on
// the volume and backup Deployments, and the Keystone pairing rules, all of
// which the validating webhook also checks. If a CEL rule were dropped, the
// equivalent SetupCinderEnvTest-based test could silently keep passing because
// the webhook would still reject the CR; using this helper makes the CRD-layer
// rule the only enforcement point in scope.
func SetupCinderEnvTestNoWebhook(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, _ := cinderPaths()
	return commonenvtest.SetupEnvTestWithCRDs(t, commonenvtest.BuildScheme(addToScheme), []string{crdDir})
}

// SetupCinderEnvTestWithController starts an envtest API server with the three
// Cinder CRDs, webhook configurations, fake CRDs for external operators
// (MariaDB, ESO, Gateway API, ...), and a controller-runtime Manager hosting the
// caller-registered webhooks and reconcilers. It returns a direct (non-caching)
// client, a context, and its cancel function. The environment is torn down
// automatically via t.Cleanup().
//
// Parameters:
//   - addToScheme registers the Cinder API types with the runtime scheme.
//   - registerWebhooks sets up all three webhook handlers with the manager.
//   - registerController wires the CinderReconciler, the CinderBackendReconciler
//     and the CinderBackupBackendReconciler onto the manager (all three run in
//     one manager, further reconcilers rather than further binaries).
func SetupCinderEnvTestWithController(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
	registerController func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := cinderPaths()

	// Combine the Cinder CRD dir with the common fake CRD dirs (ESO, gateway-api,
	// mariadb, ...) so the reconcilers' external kinds resolve.
	crdDirs := append([]string{crdDir}, commonenvtest.CommonFakeCRDDirs()...)

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:               "Cinder",
		Scheme:             buildControllerScheme(addToScheme),
		CRDDirectoryPaths:  crdDirs,
		WebhookDir:         webhookDir,
		RegisterWebhooks:   registerWebhooks,
		RegisterController: registerController,
	})
}

// buildControllerScheme creates a runtime.Scheme that includes all types the
// three reconcilers need: the Cinder API types, core Kubernetes types, ESO (the
// credential gate and the store watches), Gateway API (HTTPRoute), and MariaDB
// (database provisioning and the cluster watch). It is created fresh per test.
func buildControllerScheme(addToScheme func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	return commonenvtest.BuildScheme(
		// External operator types the reconcilers register.
		esov1.AddToScheme,
		gatewayv1.Install,
		mariadbv1alpha1.AddToScheme,
		// Cinder types.
		addToScheme,
	)
}

// cinderPaths returns absolute paths to the Cinder CRD and webhook configuration
// directories, resolved relative to this source file via runtime.Caller(0).
func cinderPaths() (crdDir, webhookDir string) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("testutil: runtime.Caller failed to determine source file path")
	}
	base := filepath.Dir(thisFile)
	crdDir = filepath.Join(base, "..", "..", "config", "crd", "bases")
	webhookDir = filepath.Join(base, "..", "..", "config", "webhook")
	return crdDir, webhookDir
}
