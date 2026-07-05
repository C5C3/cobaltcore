// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides Horizon-specific test utilities for envtest integration tests.
package testutil

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	commonenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// SkipIfEnvTestUnavailable re-exports the common skip guard for envtest-based
// integration tests. Call as the first statement in each integration test function.
var SkipIfEnvTestUnavailable = commonenvtest.SkipIfEnvTestUnavailable

// SetupHorizonEnvTestWithController starts an envtest API server with the
// Horizon CRD, webhook configurations, fake CRDs for external operators
// (ESO, Gateway API), and a controller-runtime Manager running the
// HorizonReconciler. It returns a direct (non-caching) client, a context,
// and its cancel function. The environment is torn down automatically via
// t.Cleanup().
//
// Parameters:
//   - addToScheme registers the Horizon API types with the runtime scheme.
//     Callers pass horizonv1alpha1.AddToScheme to avoid an import cycle
//     between the testutil package and the v1alpha1 package.
//   - registerWebhooks sets up webhook handlers with the manager.
//   - registerController registers the HorizonReconciler via SetupWithManager.
func SetupHorizonEnvTestWithController(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
	registerController func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := horizonPaths()

	// Combine the Horizon CRD dir with the common fake CRD dirs (ESO,
	// gateway-api, memcached).
	crdDirs := append([]string{crdDir}, commonenvtest.CommonFakeCRDDirs()...)

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:               "Horizon",
		Scheme:             buildControllerScheme(addToScheme),
		CRDDirectoryPaths:  crdDirs,
		WebhookDir:         webhookDir,
		RegisterWebhooks:   registerWebhooks,
		RegisterController: registerController,
	})
}

// horizonPaths returns absolute paths to the Horizon CRD and webhook
// configuration directories, resolved relative to this source file via
// runtime.Caller(0).
func horizonPaths() (crdDir, webhookDir string) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("testutil: runtime.Caller failed to determine source file path")
	}
	base := filepath.Dir(thisFile)
	crdDir = filepath.Join(base, "..", "..", "config", "crd", "bases")
	webhookDir = filepath.Join(base, "..", "..", "config", "webhook")
	return crdDir, webhookDir
}

// buildControllerScheme creates a runtime.Scheme that includes all types
// needed by the HorizonReconciler: Horizon API types, core K8s types, ESO
// types, and Gateway API types. It is created fresh per test.
func buildControllerScheme(addToScheme func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	return commonenvtest.BuildScheme(
		// External operator types needed by the reconciler.
		esov1.AddToScheme,
		// Gateway API types for HTTPRoute reconciliation.
		gatewayv1.Install,
		// Horizon types.
		addToScheme,
	)
}
