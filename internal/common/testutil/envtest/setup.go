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
)

// Feature: CC-0002

// SetupEnvTest starts an envtest API server with etcd, installs fake CRDs, and
// returns a configured controller-runtime client, a context, and its cancel
// function. The environment is torn down automatically via t.Cleanup().
func SetupEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	env := &envtest.Environment{
		CRDDirectoryPaths:     fakeCRDsDirs(),
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest environment: %v", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: SharedScheme()})
	if err != nil {
		// Stop the environment before registering cleanup since client
		// creation failed before we could register t.Cleanup for env.Stop().
		if stopErr := env.Stop(); stopErr != nil {
			t.Logf("additionally failed to stop envtest environment: %v", stopErr)
		}
		t.Fatalf("failed to create controller-runtime client: %v", err)
	}

	// Register cleanup only after client.New succeeds so env.Stop() is
	// called exactly once — either here on success, or explicitly above
	// on failure.
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("failed to stop envtest environment: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return c, ctx, cancel
}

// SkipIfEnvTestUnavailable skips the calling test if the KUBEBUILDER_ASSETS
// environment variable is not set or the expected etcd binary is not present.
// This is the single, authoritative skip guard for all envtest-based
// integration tests (CC-0002).
func SkipIfEnvTestUnavailable(t testing.TB) {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS not set, skipping integration test")
	}
	if _, err := os.Stat(filepath.Join(assets, "etcd")); err != nil {
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
// once and reused across all callers (CC-0002, extended CC-0005).
func SharedScheme() *k8sruntime.Scheme {
	sharedSchemeOnce.Do(func() {
		sharedScheme = k8sruntime.NewScheme()
		// clientgoscheme adds core/v1, apps/v1, batch/v1, and other built-in
		// groups from client-go.
		utilruntime.Must(clientgoscheme.AddToScheme(sharedScheme))
		// apiextensionsv1 is needed for CRD list/get operations in integration
		// tests.
		utilruntime.Must(apiextensionsv1.AddToScheme(sharedScheme))
		// External operator types for typed client operations (CC-0005).
		utilruntime.Must(mariadbv1alpha1.AddToScheme(sharedScheme))
		utilruntime.Must(esov1.AddToScheme(sharedScheme))
		utilruntime.Must(esov1alpha1.AddToScheme(sharedScheme))
		utilruntime.Must(certmanagerv1.AddToScheme(sharedScheme))
	})
	return sharedScheme
}

// fakeCRDsDirs returns the absolute paths to all controller-specific
// subdirectories under fake_crds/. Each subdirectory groups CRDs by the
// external operator that owns them (CC-0002).
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
