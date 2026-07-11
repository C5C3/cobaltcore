// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// SetupEnvTest starts an envtest API server with etcd, installs fake CRDs, and
// returns a configured controller-runtime client, a context, and its cancel
// function. The environment is torn down automatically via t.Cleanup().
func SetupEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return SetupEnvTestWithCRDs(t, SharedScheme(), fakeCRDsDirs())
}

// SetupEnvTestWithCRDs starts a webhook-less envtest API server with the given
// scheme and CRD directories and returns a direct (non-caching) client, so tests
// can submit CRs and observe exactly the schema-layer validation the API server
// enforces (kubebuilder markers + x-kubernetes-validations CEL rules) without a
// validating webhook short-circuiting the rejection. This is the shared body of
// the per-operator no-webhook setups. Tear-down is wired via t.Cleanup().
func SetupEnvTestWithCRDs(t testing.TB, scheme *k8sruntime.Scheme, crdDirs []string) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	env := &envtest.Environment{
		CRDDirectoryPaths:     crdDirs,
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest environment: %v", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		// Stop the environment before registering cleanup since client
		// creation failed before we could register t.Cleanup for env.Stop().
		if stopErr := env.Stop(); stopErr != nil {
			t.Logf("additionally failed to stop envtest environment: %v", stopErr)
		}
		t.Fatalf("failed to create controller-runtime client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if err := env.Stop(); err != nil {
			t.Errorf("failed to stop envtest environment: %v", err)
		}
	})

	return c, ctx, cancel
}

// SkipIfEnvTestUnavailable skips the calling test if the KUBEBUILDER_ASSETS
// environment variable is not set or the expected etcd binary is not present.
// This is the single, authoritative skip guard for all envtest-based
// integration tests.
func SkipIfEnvTestUnavailable(t testing.TB) {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS not set, skipping integration test")
	}
	if _, err := os.Stat(filepath.Join(assets, "etcd")); err != nil { //nolint:gosec // G703: assets path from KUBEBUILDER_ASSETS env var, not user input
		t.Skipf("envtest binaries not found at %s, skipping integration test", assets)
	}
}

// sharedScheme is the lazily-initialized scheme used by SharedScheme.
var (
	sharedScheme     *k8sruntime.Scheme
	sharedSchemeOnce sync.Once
)

// SharedScheme returns a runtime.Scheme pre-populated with the core, batch,
// apiextensions, and external operator API groups. The scheme is constructed
// once and reused across all callers (extended).
func SharedScheme() *k8sruntime.Scheme {
	sharedSchemeOnce.Do(func() {
		sharedScheme = k8sruntime.NewScheme()
		// clientgoscheme adds core/v1, apps/v1, batch/v1, and other built-in
		// groups from client-go.
		utilruntime.Must(clientgoscheme.AddToScheme(sharedScheme))
		// apiextensionsv1 is needed for CRD list/get operations in integration
		// tests.
		utilruntime.Must(apiextensionsv1.AddToScheme(sharedScheme))
		// External operator types for typed client operations.
		utilruntime.Must(mariadbv1alpha1.AddToScheme(sharedScheme))
		utilruntime.Must(esov1.AddToScheme(sharedScheme))
		utilruntime.Must(esov1alpha1.AddToScheme(sharedScheme))
		utilruntime.Must(certmanagerv1.AddToScheme(sharedScheme))
	})
	return sharedScheme
}

// CommonFakeCRDDirs returns the absolute paths to all controller-specific
// subdirectories under the shared fake_crds/ tree. Each subdirectory groups
// CRDs by the external operator that owns them. Exported so the per-operator
// testutil packages can feed the shared fake CRDs into their own envtest
// environments without re-rolling the runtime.Caller path math — this
// package is the single owner of the fake_crds location.
func CommonFakeCRDDirs() []string {
	return fakeCRDsDirs()
}

// CommonExternalSchemes returns the AddToScheme functions for the external API
// groups a database-backed service operator's reconciler typically registers:
// MariaDB, ESO v1 and v1alpha1, cert-manager, and Gateway API. Operators compose
// these with their own CR types via BuildScheme; an operator that needs only a
// subset (or additional single-consumer groups like K-ORC) composes the ones it
// needs instead.
func CommonExternalSchemes() []func(*k8sruntime.Scheme) error {
	return []func(*k8sruntime.Scheme) error{
		mariadbv1alpha1.AddToScheme,
		esov1.AddToScheme,
		esov1alpha1.AddToScheme,
		certmanagerv1.AddToScheme,
		gatewayv1.Install,
	}
}

// BuildScheme creates a runtime.Scheme with the core client-go and
// apiextensions types plus every caller-provided AddToScheme function
// registered, in order. It is created fresh per call so tests never share
// scheme state and SharedScheme() stays unmodified. The per-operator testutil
// packages pass their API types and the external operator types their
// reconciler needs.
func BuildScheme(addToScheme ...func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	s := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(apiextensionsv1.AddToScheme(s))
	for _, add := range addToScheme {
		utilruntime.Must(add(s))
	}
	return s
}

// fakeCRDsDirs returns the absolute paths to all controller-specific
// subdirectories under fake_crds/. Each subdirectory groups CRDs by the
// external operator that owns them.
func fakeCRDsDirs() []string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("envtest: runtime.Caller failed to determine source file path")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "fake_crds")

	entries, err := os.ReadDir(root)
	if err != nil {
		panic(fmt.Sprintf("envtest: failed to read fake_crds directory %s: %v", root, err))
	}

	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	if len(dirs) == 0 {
		panic(fmt.Sprintf("envtest: no subdirectories found in fake_crds directory %s", root))
	}
	return dirs
}
