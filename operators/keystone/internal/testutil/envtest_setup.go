// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides Keystone-specific test utilities for envtest integration tests (CC-0012).
package testutil

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

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
// The scheme is local to this helper — SharedScheme() in internal/common is NOT modified (CC-0012, REQ-003).
func SetupKeystoneEnvTest(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	crdDir, webhookDir := keystonePaths()

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{webhookDir},
		},
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start Keystone envtest environment: %v", err)
	}

	s := buildScheme(addToScheme)

	// Create a controller-runtime manager to host the webhook server.
	// The manager's webhook server binds to the host/port/certDir that
	// envtest allocated and patched into the webhook configurations.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: s,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    env.WebhookInstallOptions.LocalServingHost,
			Port:    env.WebhookInstallOptions.LocalServingPort,
			CertDir: env.WebhookInstallOptions.LocalServingCertDir,
		}),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		if stopErr := env.Stop(); stopErr != nil {
			t.Logf("additionally failed to stop envtest environment: %v", stopErr)
		}
		t.Fatalf("failed to create controller-runtime manager: %v", err)
	}

	// Register the Keystone defaulting and validating webhooks with the manager.
	if err := registerWebhooks(mgr); err != nil {
		if stopErr := env.Stop(); stopErr != nil {
			t.Logf("additionally failed to stop envtest environment: %v", stopErr)
		}
		t.Fatalf("failed to register Keystone webhooks: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start the manager (and webhook server) in a background goroutine.
	// mgrStopped is closed when mgr.Start returns, so cleanup can block
	// until the manager has fully released its ports (CC-0012).
	mgrStopped := make(chan struct{})
	go func() {
		defer close(mgrStopped)
		if err := mgr.Start(ctx); err != nil {
			// Use t.Errorf instead of t.Fatalf because this runs in a goroutine.
			t.Errorf("manager exited with error: %v", err)
		}
	}()

	// Wait for the webhook server to become ready before returning.
	if err := waitForWebhookServer(
		env.WebhookInstallOptions.LocalServingHost,
		env.WebhookInstallOptions.LocalServingPort,
		10*time.Second,
	); err != nil {
		cancel()
		<-mgrStopped
		if stopErr := env.Stop(); stopErr != nil {
			t.Logf("additionally failed to stop envtest environment: %v", stopErr)
		}
		t.Fatalf("webhook server did not become ready: %v", err)
	}

	// Use a direct (non-caching) client for test assertions. The manager's
	// caching client can return stale data on immediate Create→Get sequences
	// because the informer may not have processed the object yet (CC-0012).
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		cancel()
		<-mgrStopped
		if stopErr := env.Stop(); stopErr != nil {
			t.Logf("additionally failed to stop envtest environment: %v", stopErr)
		}
		t.Fatalf("failed to create direct client: %v", err)
	}

	t.Cleanup(func() {
		cancel()
		<-mgrStopped // ensure manager has fully stopped and ports are released
		if err := env.Stop(); err != nil {
			t.Errorf("failed to stop Keystone envtest environment: %v", err)
		}
	})

	return c, ctx, cancel
}

// waitForWebhookServer polls the webhook server's TLS endpoint until it
// accepts connections or the timeout is reached.
func waitForWebhookServer(host string, port int, timeout time.Duration) error {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	deadline := time.Now().Add(timeout)
	dialer := &net.Dialer{Timeout: time.Second}
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // envtest self-signed cert

	for time.Now().Before(deadline) {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("webhook server at %s not ready within %v", addr, timeout)
}

// keystonePaths returns absolute paths to the Keystone CRD and webhook
// configuration directories, resolved relative to this source file via
// runtime.Caller(0) (CC-0012, REQ-003).
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
// caller-provided types registered. It is created fresh per test to avoid sharing
// state between tests and to keep SharedScheme() unmodified (CC-0012, REQ-003).
func buildScheme(addToScheme func(*k8sruntime.Scheme) error) *k8sruntime.Scheme {
	s := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(apiextensionsv1.AddToScheme(s))
	utilruntime.Must(addToScheme(s))
	return s
}
