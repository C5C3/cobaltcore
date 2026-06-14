// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides c5c3-specific test utilities for envtest integration
// tests of the ControlPlane reconciler.
package testutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// SkipIfEnvTestUnavailable re-exports the common skip guard for envtest-based
// integration tests. Call as the first statement in each integration test
// function.
var SkipIfEnvTestUnavailable = commonenvtest.SkipIfEnvTestUnavailable

// SetupC5c3EnvTestWithController starts an envtest API server with the c5c3 CRDs,
// the Keystone CRD (the reconciler Owns a Keystone child), fake CRDs for the
// external operators the ControlPlane reconciler talks to (MariaDB, Memcached,
// external-secrets, cert-manager, K-ORC), the ControlPlane webhook
// configurations, and a controller-runtime Manager running the
// ControlPlaneReconciler. It returns a direct (non-caching) client, a context,
// and its cancel function. The environment is torn down automatically via
// t.Cleanup().
//
// Parameters:
//   - addToScheme registers the c5c3 API types with the runtime scheme. Callers
//     pass c5c3v1alpha1.AddToScheme to avoid an import cycle between the testutil
//     package and the v1alpha1 package.
//   - registerWebhooks sets up webhook handlers with the manager. Callers pass a
//     closure that calls ControlPlaneWebhook.SetupWebhookWithManager(mgr).
//   - registerController registers the ControlPlaneReconciler via
//     SetupWithManager (or an inline builder for multi-test setups).
//
// The scheme is built fresh per test — internal/common's SharedScheme() is NOT
// modified, mirroring the keystone testutil discipline.
func SetupC5c3EnvTestWithController(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
	registerController func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	return commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:               "c5c3",
		Scheme:             buildControllerScheme(addToScheme),
		CRDDirectoryPaths:  crdDirectoryPaths(),
		WebhookDir:         c5c3WebhookDir(),
		RegisterWebhooks:   registerWebhooks,
		RegisterController: registerController,
	})
}

// crdDirectoryPaths returns the absolute CRD directories envtest loads for a
// ControlPlane integration test, resolved relative to this
// source file via runtime.Caller(0):
//   - c5c3 CRDs (controlplanes, credentialrotations, secretaggregates).
//   - the Keystone CRD (the reconciler Owns a Keystone child).
//   - every shared fake CRD dir under internal/common/testutil/fake_crds/*
//     (mariadb-operator, memcached-operator, external-secrets, cert-manager,
//     k-orc, ...) so the external operator kinds the reconciler create-or-updates
//     resolve in the apiserver RESTMapper.
func crdDirectoryPaths() []string {
	base := callerDir()
	c5c3CRDDir := filepath.Join(base, "..", "..", "config", "crd", "bases")
	keystoneCRDDir := filepath.Join(base, "..", "..", "..", "keystone", "config", "crd", "bases")

	dirs := []string{c5c3CRDDir, keystoneCRDDir}
	return append(dirs, commonFakeCRDsDirs()...)
}

// c5c3WebhookDir returns the absolute path to the c5c3 webhook configuration
// directory, resolved relative to this source file via runtime.Caller(0).
func c5c3WebhookDir() string {
	return filepath.Join(callerDir(), "..", "..", "config", "webhook")
}

// callerDir returns the directory containing this source file, resolved via
// runtime.Caller(0) so the absolute CRD/webhook paths are independent of the
// process working directory.
func callerDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("testutil: runtime.Caller failed to determine source file path")
	}
	return filepath.Dir(thisFile)
}

// commonFakeCRDsDirs returns absolute paths to every subdirectory of the shared
// fake CRD tree (internal/common/testutil/fake_crds), resolved relative to this
// source file. Loading all subdirs mirrors the keystone testutil helper so the
// c5c3 reconciler's external operator kinds (MariaDB, Memcached, ESO,
// cert-manager, K-ORC) all resolve without enumerating them here.
func commonFakeCRDsDirs() []string {
	// Navigate from operators/c5c3/internal/testutil/ → repo root →
	// internal/common/testutil/fake_crds/.
	root := filepath.Join(callerDir(), "..", "..", "..", "..", "internal", "common", "testutil", "fake_crds")

	entries, err := os.ReadDir(root)
	if err != nil {
		panic(fmt.Sprintf("testutil: failed to read fake_crds directory %s: %v", root, err))
	}

	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	if len(dirs) == 0 {
		panic(fmt.Sprintf("testutil: no subdirectories found in fake_crds directory %s", root))
	}
	return dirs
}

// buildControllerScheme creates a runtime.Scheme that includes all types needed
// by the ControlPlaneReconciler: the c5c3 API types, core K8s types,
// apiextensions, and the external operator types the reconciler uses as TYPED
// client objects — MariaDB, Keystone, external-secrets (v1 + v1alpha1), and
// K-ORC. It is built fresh per test.
//
// DECISION Memcached (memcached.c5c3.io) is deliberately NOT
// registered — it ships no Go module, so the reconciler handles it as an
// *unstructured.Unstructured carrying memcachedGVK (see reconcile_infrastructure.go).
// Its CRD is still loaded into envtest via the memcached-operator fake CRD dir so
// the apiserver can serve the unstructured object, but no scheme registration is
// required.
//
// DECISION cert-manager is NOT registered either — the
// ControlPlane reconciler references no cert-manager types (unlike the keystone
// reconciler), so adding certmanagerv1 to the scheme would promote an otherwise
// indirect dependency for no benefit. Its fake CRD remains loaded for parity with
// the shared fake_crds tree but needs no scheme entry.
func buildControllerScheme(addToScheme func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	s := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(apiextensionsv1.AddToScheme(s))
	// External operator types the reconciler manipulates as typed objects.
	utilruntime.Must(mariadbv1alpha1.AddToScheme(s))
	utilruntime.Must(keystonev1alpha1.AddToScheme(s))
	utilruntime.Must(esov1.AddToScheme(s))
	utilruntime.Must(esov1alpha1.AddToScheme(s))
	utilruntime.Must(orcv1alpha1.AddToScheme(s))
	// c5c3 API types (ControlPlane, CredentialRotation, ...).
	utilruntime.Must(addToScheme(s))
	return s
}
