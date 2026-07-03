// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

func TestManagerConfig_validate_nilScheme(t *testing.T) {
	cfg := ManagerConfig{
		Scheme:           nil,
		LeaderElectionID: "test.c5c3.io",
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for nil Scheme, got nil")
	}
	if err.Error() != "bootstrap: Scheme must not be nil" {
		t.Fatalf("unexpected error message: %s", err.Error())
	}
}

func TestManagerConfig_validate_emptyLeaderElectionID(t *testing.T) {
	cfg := ManagerConfig{
		Scheme:           runtime.NewScheme(),
		LeaderElectionID: "",
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for empty LeaderElectionID, got nil")
	}
	if err.Error() != "bootstrap: LeaderElectionID must not be empty" {
		t.Fatalf("unexpected error message: %s", err.Error())
	}
}

func TestManagerConfig_validate_valid(t *testing.T) {
	cfg := ManagerConfig{
		Scheme:           runtime.NewScheme(),
		LeaderElectionID: "test.c5c3.io",
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("expected no error for valid config, got: %v", err)
	}
}

func TestManagerConfig_validate_validWithSetupFunc(t *testing.T) {
	cfg := ManagerConfig{
		Scheme:           runtime.NewScheme(),
		LeaderElectionID: "test.c5c3.io",
		SetupFunc: func(_ ctrl.Manager, _ bool, _ int) error {
			return nil
		},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("expected no error for valid config with SetupFunc, got: %v", err)
	}
}

// TestManagerConfig_validate_validWithNamespace verifies that a ManagerConfig
// with a Namespace field set passes validation, allowing callers to opt into
// namespace scoping programmatically.
func TestManagerConfig_validate_validWithNamespace(t *testing.T) {
	cfg := ManagerConfig{
		Scheme:           runtime.NewScheme(),
		LeaderElectionID: "test.c5c3.io",
		Namespace:        "tenant-a",
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("expected no error for valid config with Namespace, got: %v", err)
	}
}

// TestParseRunOptions_defaults verifies the flag defaults when no args are
// supplied, including that namespace defaults to ManagerConfig.Namespace.
func TestParseRunOptions_defaults(t *testing.T) {
	cfg := ManagerConfig{Scheme: runtime.NewScheme(), LeaderElectionID: "test.c5c3.io", Namespace: "tenant-a"}

	opts, err := parseRunOptions(cfg, nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if opts.metricsAddr != ":8080" {
		t.Fatalf("metricsAddr = %q, want :8080", opts.metricsAddr)
	}
	if opts.probeAddr != ":8081" {
		t.Fatalf("probeAddr = %q, want :8081", opts.probeAddr)
	}
	if !opts.enableWebhooks {
		t.Fatal("enableWebhooks = false, want true (default)")
	}
	if opts.enableLeaderElection {
		t.Fatal("enableLeaderElection = true, want false (default)")
	}
	if opts.syncPeriod != 10*time.Minute {
		t.Fatalf("syncPeriod = %v, want 10m", opts.syncPeriod)
	}
	if opts.namespace != "tenant-a" {
		t.Fatalf("namespace = %q, want tenant-a (from cfg)", opts.namespace)
	}
	// The flag defaults to the shared default of 2.
	if opts.maxConcurrentReconciles != defaultMaxConcurrentReconciles {
		t.Fatalf("maxConcurrentReconciles = %d, want %d (shared default)",
			opts.maxConcurrentReconciles, defaultMaxConcurrentReconciles)
	}
}

// TestParseRunOptions_injectedArgs verifies that flag values are read from the
// supplied args, and that --namespace overrides ManagerConfig.Namespace.
func TestParseRunOptions_injectedArgs(t *testing.T) {
	cfg := ManagerConfig{Scheme: runtime.NewScheme(), LeaderElectionID: "test.c5c3.io", Namespace: "tenant-a"}

	args := []string{
		"--metrics-bind-address=:9090",
		"--health-probe-bind-address=:9091",
		"--leader-elect=true",
		"--enable-webhooks=false",
		"--sync-period=5m",
		"--namespace=tenant-b",
		"--max-concurrent-reconciles=8",
	}
	opts, err := parseRunOptions(cfg, args)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if opts.metricsAddr != ":9090" || opts.probeAddr != ":9091" {
		t.Fatalf("addresses = %q/%q, want :9090/:9091", opts.metricsAddr, opts.probeAddr)
	}
	// An explicit --max-concurrent-reconciles wins over the cfg default.
	if opts.maxConcurrentReconciles != 8 {
		t.Fatalf("maxConcurrentReconciles = %d, want 8 (CLI)", opts.maxConcurrentReconciles)
	}
	if !opts.enableLeaderElection {
		t.Fatal("enableLeaderElection = false, want true")
	}
	if opts.enableWebhooks {
		t.Fatal("enableWebhooks = true, want false")
	}
	if opts.syncPeriod != 5*time.Minute {
		t.Fatalf("syncPeriod = %v, want 5m", opts.syncPeriod)
	}
	if opts.namespace != "tenant-b" {
		t.Fatalf("namespace = %q, want tenant-b (CLI override)", opts.namespace)
	}
}

// TestParseRunOptions_reentrant proves the parser is callable more than once
// with different args, without the flag-redefinition panic that the previous
// flag.CommandLine + flag.Parse implementation would raise on a second call.
func TestParseRunOptions_reentrant(t *testing.T) {
	cfg := ManagerConfig{Scheme: runtime.NewScheme(), LeaderElectionID: "test.c5c3.io"}

	first, err := parseRunOptions(cfg, []string{"--metrics-bind-address=:1111"})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := parseRunOptions(cfg, []string{"--metrics-bind-address=:2222"})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first.metricsAddr != ":1111" {
		t.Fatalf("first.metricsAddr = %q, want :1111", first.metricsAddr)
	}
	if second.metricsAddr != ":2222" {
		t.Fatalf("second.metricsAddr = %q, want :2222", second.metricsAddr)
	}
}

// TestParseRunOptions_invalidArgReturnsError verifies that an unknown flag is
// reported as an error rather than exiting the process (ContinueOnError),
// keeping the parser testable.
func TestParseRunOptions_invalidArgReturnsError(t *testing.T) {
	cfg := ManagerConfig{Scheme: runtime.NewScheme(), LeaderElectionID: "test.c5c3.io"}

	if _, err := parseRunOptions(cfg, []string{"--no-such-flag"}); err == nil {
		t.Fatal("expected an error for an unknown flag, got nil")
	}
}

// TestCacheOptions_withNamespace verifies that cacheOptions with a non-empty
// namespace returns cache.Options with DefaultNamespaces containing that
// namespace key.
func TestCacheOptions_withNamespace(t *testing.T) {
	syncPeriod := 10 * time.Minute
	opts := cacheOptions(syncPeriod, "tenant-a")

	if opts.DefaultNamespaces == nil {
		t.Fatal("expected DefaultNamespaces to be non-nil when namespace is set")
	}
	if _, ok := opts.DefaultNamespaces["tenant-a"]; !ok {
		t.Fatal("expected DefaultNamespaces to contain 'tenant-a'")
	}
}

// TestCacheOptions_withoutNamespace verifies that cacheOptions with an empty
// namespace returns cache.Options with nil DefaultNamespaces, allowing
// cluster-wide watches.
func TestCacheOptions_withoutNamespace(t *testing.T) {
	syncPeriod := 10 * time.Minute
	opts := cacheOptions(syncPeriod, "")

	if opts.DefaultNamespaces != nil {
		t.Fatalf("expected DefaultNamespaces to be nil when namespace is empty, got: %v", opts.DefaultNamespaces)
	}
}

// TestCacheOptions_syncPeriodPreserved verifies that SyncPeriod is always
// configured regardless of the namespace value.
func TestCacheOptions_syncPeriodPreserved(t *testing.T) {
	syncPeriod := 10 * time.Minute

	opts := cacheOptions(syncPeriod, "tenant-a")
	if opts.SyncPeriod == nil || *opts.SyncPeriod != syncPeriod {
		t.Fatalf("expected SyncPeriod %v with namespace set, got: %v", syncPeriod, opts.SyncPeriod)
	}

	opts = cacheOptions(syncPeriod, "")
	if opts.SyncPeriod == nil || *opts.SyncPeriod != syncPeriod {
		t.Fatalf("expected SyncPeriod %v without namespace, got: %v", syncPeriod, opts.SyncPeriod)
	}
}

// TestCacheOptions_singleNamespaceEntry verifies that DefaultNamespaces has
// exactly one entry with an empty cache.Config value when namespace is set
func TestCacheOptions_singleNamespaceEntry(t *testing.T) {
	syncPeriod := 10 * time.Minute
	opts := cacheOptions(syncPeriod, "tenant-a")

	if len(opts.DefaultNamespaces) != 1 {
		t.Fatalf("expected exactly 1 entry in DefaultNamespaces, got: %d", len(opts.DefaultNamespaces))
	}
	cfg := opts.DefaultNamespaces["tenant-a"]
	if !reflect.DeepEqual(cfg, cache.Config{}) {
		t.Fatalf("expected empty cache.Config value, got: %+v", cfg)
	}
}
