// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides Barbican-specific test utilities for envtest integration tests.
package testutil

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
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

// SetupBarbicanEnvTest starts an envtest API server with the Barbican and
// BarbicanSecretStore CRDs installed, the webhook server configured and running,
// and the caller-registered defaulting/validating webhooks. It returns a direct
// (non-caching) controller-runtime client, a context, and its cancel function.
// The environment is torn down automatically via t.Cleanup().
//
// Parameters:
//   - addToScheme registers the Barbican API types with the runtime scheme.
//     Callers pass barbicanv1alpha1.AddToScheme to avoid an import cycle between
//     the testutil package and the v1alpha1 package.
//   - registerWebhooks sets up webhook handlers with the manager. The webhook
//     manifests installed by envtest carry BOTH the Barbican and
//     BarbicanSecretStore entries (failurePolicy=Fail), so the callback MUST
//     serve both handlers or admission of the unserved kind fails.
//
// The scheme is local to this helper: internal/common's SharedScheme is NOT
// modified.
func SetupBarbicanEnvTest(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := barbicanPaths()

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "Barbican",
		Scheme:            commonenvtest.BuildScheme(addToScheme),
		CRDDirectoryPaths: []string{crdDir},
		WebhookDir:        webhookDir,
		RegisterWebhooks:  registerWebhooks,
	})
}

// SetupBarbicanEnvTestNoWebhook starts an envtest API server with only the
// Barbican and BarbicanSecretStore CRDs installed, and no webhook
// configurations or webhooks at all. It returns a direct controller-runtime
// client so tests can submit CRs and observe exactly the schema-layer validation
// the API server enforces (kubebuilder validation markers +
// x-kubernetes-validations CEL rules) without the defense-in-depth webhooks
// short-circuiting the rejection or filling defaults. Tear-down is wired via
// t.Cleanup().
//
// This is intended for tests that must attribute a rejection to the CRD layer
// alone. The store union rules (barbicanRef and type immutability, the
// type/openBao union, the two OpenBao modes, the mount layout a managed store is
// frozen to) are enforced both by the schema and by the validating webhook. If
// a CEL rule were dropped, the equivalent SetupBarbicanEnvTest-based test could
// silently keep passing because the webhook would still reject the CR; using
// this helper makes the CRD-layer rule the only enforcement point in scope.
func SetupBarbicanEnvTestNoWebhook(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, _ := barbicanPaths()
	return commonenvtest.SetupEnvTestWithCRDs(t, commonenvtest.BuildScheme(addToScheme), []string{crdDir})
}

// SetupBarbicanEnvTestWithController starts an envtest API server with the
// Barbican and BarbicanSecretStore CRDs, webhook configurations, fake CRDs for
// external operators (MariaDB, ESO, Gateway API, openbao-operator, ...), and a
// controller-runtime Manager hosting the caller-registered webhooks and
// reconcilers. It returns a direct (non-caching) client, a context, and its
// cancel function. The environment is torn down automatically via t.Cleanup().
//
// Parameters:
//   - addToScheme registers the Barbican API types with the runtime scheme.
//   - registerWebhooks sets up both webhook handlers with the manager.
//   - registerController wires the BarbicanReconciler and the
//     BarbicanSecretStoreReconciler onto the manager (both controllers run in
//     one manager, a second reconciler rather than a second binary). The
//     callback owns reconciler construction, so a test that needs the store
//     reconciler's OpenBao/token/clock seams injects them there.
func SetupBarbicanEnvTestWithController(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
	registerController func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := barbicanPaths()

	// Combine the Barbican CRD dir with the common fake CRD dirs (ESO,
	// gateway-api, mariadb, openbao-operator, ...) so the reconcilers' external
	// kinds resolve.
	crdDirs := append([]string{crdDir}, commonenvtest.CommonFakeCRDDirs()...)

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:               "Barbican",
		Scheme:             buildControllerScheme(addToScheme),
		CRDDirectoryPaths:  crdDirs,
		WebhookDir:         webhookDir,
		RegisterWebhooks:   registerWebhooks,
		RegisterController: registerController,
	})
}

// barbicanPaths returns absolute paths to the Barbican CRD and webhook
// configuration directories, resolved relative to this source file via
// runtime.Caller(0).
func barbicanPaths() (crdDir, webhookDir string) {
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
// BarbicanReconciler and BarbicanSecretStoreReconciler need: Barbican API types,
// core K8s types, ESO (credential gates and store watches), Gateway API
// (HTTPRoute), MariaDB (database provisioning and the cluster watch), and
// openbao-operator (the OpenBaoCluster a managed store provisions against). It
// is created fresh per test.
func buildControllerScheme(addToScheme func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	return commonenvtest.BuildScheme(
		// External operator types the reconcilers register.
		esov1.AddToScheme,
		gatewayv1.Install,
		mariadbv1alpha1.AddToScheme,
		openbaov1alpha1.AddToScheme,
		// Barbican types.
		addToScheme,
	)
}
