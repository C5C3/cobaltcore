// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides OVN-specific test utilities for envtest integration tests.
package testutil

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
)

// SkipIfEnvTestUnavailable re-exports the common skip guard for envtest-based
// integration tests. Call as the first statement in each integration test function.
var SkipIfEnvTestUnavailable = commonenvtest.SkipIfEnvTestUnavailable

// SetupOVNEnvTest starts an envtest API server with the OVNCentral and
// OVNChassis CRDs installed, the webhook server configured and running, and the
// caller-registered defaulting/validating webhooks. It returns a direct
// (non-caching) controller-runtime client, a context, and its cancel function.
// The environment is torn down automatically via t.Cleanup().
//
// Parameters:
//   - addToScheme registers the OVN API types with the runtime scheme. Callers
//     pass ovnv1alpha1.AddToScheme to avoid an import cycle between the testutil
//     package and the v1alpha1 package.
//   - registerWebhooks sets up webhook handlers with the manager. The webhook
//     manifests installed by envtest carry BOTH the OVNCentral and OVNChassis
//     entries (failurePolicy=Fail), so the callback MUST serve both handlers or
//     admission of the unserved kind fails.
//
// The scheme is local to this helper: internal/common's SharedScheme is NOT
// modified.
func SetupOVNEnvTest(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := ovnPaths()

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "OVN",
		Scheme:            commonenvtest.BuildScheme(addToScheme),
		CRDDirectoryPaths: []string{crdDir},
		WebhookDir:        webhookDir,
		RegisterWebhooks:  registerWebhooks,
	})
}

// SetupOVNEnvTestNoWebhook starts an envtest API server with only the OVNCentral
// and OVNChassis CRDs installed, and no webhook configurations or webhooks at
// all. It returns a direct controller-runtime client so tests can submit CRs and
// observe exactly the schema-layer validation the API server enforces
// (kubebuilder validation markers + x-kubernetes-validations CEL rules) without
// the defense-in-depth webhooks short-circuiting the rejection or filling
// defaults. Tear-down is wired via t.Cleanup().
//
// This is intended for tests that must attribute a rejection to the CRD layer
// alone. Most OVN rules are enforced twice, by the schema and by the validating
// webhook. If a CEL rule were dropped, the equivalent SetupOVNEnvTest-based test
// could silently keep passing because the webhook would still reject the CR;
// using this helper makes the CRD-layer rule the only enforcement point in
// scope.
func SetupOVNEnvTestNoWebhook(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, _ := ovnPaths()
	return commonenvtest.SetupEnvTestWithCRDs(t, commonenvtest.BuildScheme(addToScheme), []string{crdDir})
}

// SetupOVNEnvTestWithController starts an envtest API server with the OVNCentral
// and OVNChassis CRDs, webhook configurations, fake CRDs for external operators
// (cert-manager, ...), and a controller-runtime Manager hosting the
// caller-registered webhooks and reconcilers. It returns a direct (non-caching)
// client, a context, and its cancel function. The environment is torn down
// automatically via t.Cleanup().
//
// Parameters:
//   - addToScheme registers the OVN API types with the runtime scheme.
//   - registerWebhooks sets up both webhook handlers with the manager.
//   - registerController wires the OVNCentralReconciler and the
//     OVNChassisReconciler onto the manager (both controllers run in one
//     manager, a second reconciler rather than a second binary). The callback
//     owns reconciler construction, so a test that needs a specific resolver or
//     controller options injects them there. It must register the OVNCentral
//     reconciler first when it registers both: that reconciler's setup is the
//     single registration site for the OVNChassis field index.
func SetupOVNEnvTestWithController(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
	registerController func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := ovnPaths()

	// Combine the OVN CRD dir with the common fake CRD dirs (cert-manager, ...)
	// so the reconcilers' external kinds resolve.
	crdDirs := append([]string{crdDir}, commonenvtest.CommonFakeCRDDirs()...)

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:               "OVN",
		Scheme:             buildControllerScheme(addToScheme),
		CRDDirectoryPaths:  crdDirs,
		WebhookDir:         webhookDir,
		RegisterWebhooks:   registerWebhooks,
		RegisterController: registerController,
	})
}

// ovnPaths returns absolute paths to the OVN CRD and webhook configuration
// directories, resolved relative to this source file via runtime.Caller(0).
func ovnPaths() (crdDir, webhookDir string) {
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
// OVNCentral and OVNChassis reconcilers need: OVN API types, core K8s types, and
// cert-manager (the Certificates the TLS step applies and watches). It is
// created fresh per test.
func buildControllerScheme(addToScheme func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	return commonenvtest.BuildScheme(
		// External operator types the reconcilers register.
		certmanagerv1.AddToScheme,
		// OVN types.
		addToScheme,
	)
}
