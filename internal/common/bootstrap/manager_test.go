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

// Feature: CC-0001

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
		SetupFunc: func(_ ctrl.Manager, _ bool) error {
			return nil
		},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("expected no error for valid config with SetupFunc, got: %v", err)
	}
}

// TestManagerConfig_validate_validWithNamespace verifies that a ManagerConfig
// with a Namespace field set passes validation, allowing callers to opt into
// namespace scoping programmatically (CC-0043).
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

// Feature: CC-0043

// TestCacheOptions_withNamespace verifies that cacheOptions with a non-empty
// namespace returns cache.Options with DefaultNamespaces containing that
// namespace key (REQ-001).
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
// cluster-wide watches (REQ-002).
func TestCacheOptions_withoutNamespace(t *testing.T) {
	syncPeriod := 10 * time.Minute
	opts := cacheOptions(syncPeriod, "")

	if opts.DefaultNamespaces != nil {
		t.Fatalf("expected DefaultNamespaces to be nil when namespace is empty, got: %v", opts.DefaultNamespaces)
	}
}

// TestCacheOptions_syncPeriodPreserved verifies that SyncPeriod is always
// configured regardless of the namespace value (REQ-001, REQ-002).
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
// (REQ-001).
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
