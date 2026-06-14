// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"testing"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// ManagedEnvTestConfig configures StartManagedEnvTest. It captures the only
// parts that differ between the keystone and c5c3 webhook-hosting test setups —
// the scheme, the CRD/webhook directories, and the registration callbacks — so
// the timing-sensitive manager-hosting skeleton lives in exactly one place.
type ManagedEnvTestConfig struct {
	// Name labels the environment in fatal/error messages, e.g. "Keystone".
	Name string
	// Scheme is the runtime scheme the manager and the returned client use. It
	// is built fresh per test by the caller so internal/common's SharedScheme is
	// never mutated.
	Scheme *k8sruntime.Scheme
	// CRDDirectoryPaths are the directories envtest loads CRDs from.
	CRDDirectoryPaths []string
	// WebhookDir is the directory holding the webhook configuration manifests
	// that envtest patches with the local serving host/port/cert.
	WebhookDir string
	// RegisterWebhooks wires the operator's webhooks onto the manager. Required.
	RegisterWebhooks func(ctrl.Manager) error
	// RegisterController wires the operator's reconciler onto the manager. It is
	// optional: pass nil for webhook-only setups that exercise admission alone.
	RegisterController func(ctrl.Manager) error
	// WebhookReadyTimeout bounds the wait for the webhook server to accept
	// connections. Defaults to 10s when zero.
	WebhookReadyTimeout time.Duration
}

// StartManagedEnvTest starts an envtest API server with the configured CRDs and
// webhook configurations, runs a controller-runtime manager hosting the webhook
// server (and, when configured, the reconciler) in a background goroutine, and
// returns a direct (non-caching) client for assertions plus the manager
// context and its cancel function.
//
// The manager's metrics and health-probe servers are disabled (BindAddress
// "0") so multiple sequential envtest managers do not fight over the default
// :8080/:8081 ports. Tear-down — registered via t.Cleanup — cancels the
// context and blocks on the manager goroutine before stopping envtest, so the
// webhook/API-server ports are fully released before the next test starts.
func StartManagedEnvTest(t testing.TB, cfg ManagedEnvTestConfig) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()

	timeout := cfg.WebhookReadyTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     cfg.CRDDirectoryPaths,
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{cfg.WebhookDir},
		},
	}

	restCfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start %s envtest environment: %v", cfg.Name, err)
	}

	// Host the webhook server on the host/port/certDir envtest allocated and
	// patched into the webhook configurations.
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: cfg.Scheme,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    env.WebhookInstallOptions.LocalServingHost,
			Port:    env.WebhookInstallOptions.LocalServingPort,
			CertDir: env.WebhookInstallOptions.LocalServingCertDir,
		}),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		stopEnv(t, env)
		t.Fatalf("failed to create %s controller-runtime manager: %v", cfg.Name, err)
	}

	if err := cfg.RegisterWebhooks(mgr); err != nil {
		stopEnv(t, env)
		t.Fatalf("failed to register %s webhooks: %v", cfg.Name, err)
	}
	if cfg.RegisterController != nil {
		if err := cfg.RegisterController(mgr); err != nil {
			stopEnv(t, env)
			t.Fatalf("failed to register %s controller: %v", cfg.Name, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start the manager (and webhook server) in a background goroutine.
	// mgrStopped is closed when mgr.Start returns, so cleanup can block until
	// the manager has fully released its ports.
	mgrStopped := make(chan struct{})
	go func() {
		defer close(mgrStopped)
		if err := mgr.Start(ctx); err != nil {
			// t.Errorf rather than t.Fatalf because this runs in a goroutine.
			t.Errorf("%s manager exited with error: %v", cfg.Name, err)
		}
	}()

	if err := waitForWebhookServer(
		env.WebhookInstallOptions.LocalServingHost,
		env.WebhookInstallOptions.LocalServingPort,
		timeout,
	); err != nil {
		cancel()
		<-mgrStopped
		stopEnv(t, env)
		t.Fatalf("%s webhook server did not become ready: %v", cfg.Name, err)
	}

	// Use a direct (non-caching) client for assertions. The manager's caching
	// client can return stale data on immediate Create→Get sequences because
	// the informer may not have processed the object yet.
	c, err := client.New(restCfg, client.Options{Scheme: cfg.Scheme})
	if err != nil {
		cancel()
		<-mgrStopped
		stopEnv(t, env)
		t.Fatalf("failed to create %s direct client: %v", cfg.Name, err)
	}

	t.Cleanup(func() {
		cancel()
		<-mgrStopped // ensure the manager has fully stopped and ports are released
		if err := env.Stop(); err != nil {
			t.Errorf("failed to stop %s envtest environment: %v", cfg.Name, err)
		}
	})

	return c, ctx, cancel
}

// waitForWebhookServer polls the webhook server's TLS endpoint until it accepts
// connections or the timeout is reached.
func waitForWebhookServer(host string, port int, timeout time.Duration) error {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	deadline := time.Now().Add(timeout)
	dialer := &net.Dialer{Timeout: time.Second}
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // envtest self-signed cert

	for time.Now().Before(deadline) {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsCfg) //nolint:noctx // test utility polling loop, context propagation not needed
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("webhook server at %s not ready within %v", addr, timeout)
}

// stopEnv stops env, logging rather than failing on error. Callers invoke it
// only on an already-failing setup path immediately before t.Fatalf.
func stopEnv(t testing.TB, env *envtest.Environment) {
	t.Helper()
	if err := env.Stop(); err != nil {
		t.Logf("additionally failed to stop envtest environment: %v", err)
	}
}
