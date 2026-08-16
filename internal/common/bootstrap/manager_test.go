// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"flag"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
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
		SetupFunc: func(_ mcmanager.Manager, _ bool, _ int) error {
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
	if opts.clustersNamespace != "c5c3-clusters" {
		t.Fatalf("clustersNamespace = %q, want c5c3-clusters (default)", opts.clustersNamespace)
	}
	// The flag defaults to the shared default of 2.
	if opts.maxConcurrentReconciles != DefaultMaxConcurrentReconciles {
		t.Fatalf("maxConcurrentReconciles = %d, want %d (shared default)",
			opts.maxConcurrentReconciles, DefaultMaxConcurrentReconciles)
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
		"--clusters-namespace=foo",
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
	if opts.clustersNamespace != "foo" {
		t.Fatalf("clustersNamespace = %q, want foo (CLI override)", opts.clustersNamespace)
	}
}

// TestParseRunOptions_emptyClustersNamespace pins the form the namespace-scoped
// chart renders: `--clusters-namespace=` has to parse to the empty string, the
// value that switches target clusters off.
func TestParseRunOptions_emptyClustersNamespace(t *testing.T) {
	cfg := ManagerConfig{Scheme: runtime.NewScheme(), LeaderElectionID: "test.c5c3.io"}

	opts, err := parseRunOptions(cfg, []string{"--clusters-namespace="})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if opts.clustersNamespace != "" {
		t.Fatalf("clustersNamespace = %q, want the empty string", opts.clustersNamespace)
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

// TestParseRunOptions_registerFlags verifies that a RegisterFlags hook can
// register an operator-specific flag on the shared flag set and that an
// injected custom arg is parsed into the bound variable, while the shared
// flag defaults (metricsAddr) remain intact.
func TestParseRunOptions_registerFlags(t *testing.T) {
	var custom string
	cfg := ManagerConfig{
		Scheme:           runtime.NewScheme(),
		LeaderElectionID: "test.c5c3.io",
		RegisterFlags: func(fs *flag.FlagSet) {
			fs.StringVar(&custom, "custom", "", "An operator-specific flag.")
		},
	}

	opts, err := parseRunOptions(cfg, []string{"--custom=x"})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if custom != "x" {
		t.Fatalf("custom = %q, want x (from RegisterFlags hook)", custom)
	}
	if opts.metricsAddr != ":8080" {
		t.Fatalf("metricsAddr = %q, want :8080 (shared default)", opts.metricsAddr)
	}
}

// applyClusterOptions applies the options for the given scheme to a fresh
// cluster.Options and returns the result.
func applyClusterOptions(scheme *runtime.Scheme, into cluster.Options) cluster.Options {
	for _, opt := range clusterOptions(scheme, 10*time.Minute) {
		opt(&into)
	}
	return into
}

// TestClusterOptions_appliesTheOperatorScheme verifies that a target cluster
// the kubeconfig provider engages is built with the operator's own scheme.
// Leaving it unset would hand the cluster the client-go global scheme, which
// knows no CRD kind, and every child write of a CRD-backed kind would fail on
// the target cluster.
func TestClusterOptions_appliesTheOperatorScheme(t *testing.T) {
	s := runtime.NewScheme()

	applied := applyClusterOptions(s, cluster.Options{})
	if applied.Scheme != s {
		t.Fatalf("cluster Scheme = %v, want the operator scheme", applied.Scheme)
	}
}

// TestClusterOptions_overridesAPresetScheme covers the case where the options
// are applied on top of a cluster.Options that already carries a scheme: the
// operator's scheme has to win, otherwise a caller-side default would decide
// which kinds the target cluster can decode.
func TestClusterOptions_overridesAPresetScheme(t *testing.T) {
	s := runtime.NewScheme()
	preset := runtime.NewScheme()

	applied := applyClusterOptions(s, cluster.Options{Scheme: preset})
	if applied.Scheme != s {
		t.Fatal("expected the operator scheme to replace the preset one")
	}
}

// TestClusterOptions_clusterWideOperatorKeepsAnUnrestrictedTargetCache pins the
// default these options hand every engaged cluster: no namespace restriction,
// and the operator's sync period. Whether a given target cluster is cached
// cluster-wide or per namespace is decided by the namespaces key of its
// registration Secret, which the provider reads and this function knows nothing
// about, so what is left here is the unrestricted default it starts from.
func TestClusterOptions_clusterWideOperatorKeepsAnUnrestrictedTargetCache(t *testing.T) {
	applied := applyClusterOptions(runtime.NewScheme(), cluster.Options{})

	if applied.Cache.DefaultNamespaces != nil {
		t.Fatalf("expected an unrestricted target cache, got: %v", applied.Cache.DefaultNamespaces)
	}
	// The registration Secrets are read on the management cluster, so no
	// per-object widening travels to the target: it would pull a second
	// namespace of Secrets out of every engaged cluster.
	if applied.Cache.ByObject != nil {
		t.Fatalf("expected no per-object cache scoping on the target cluster, got: %v", applied.Cache.ByObject)
	}
	if applied.Cache.SyncPeriod == nil || *applied.Cache.SyncPeriod != 10*time.Minute {
		t.Fatalf("expected the operator's sync period on the target cache, got: %v", applied.Cache.SyncPeriod)
	}
}

// TestTargetClustersNamespace verifies the single gate that decides whether an
// operator pays for target clusters at all. An operator that never resolves
// spec.targetClusterRef must not engage clusters or widen its Secret informer,
// and an install that clears --clusters-namespace must be able to switch the
// whole feature off — a namespace-scoped deployment relies on it, because its
// Role covers its own namespace only and a widened informer would never sync.
func TestTargetClustersNamespace(t *testing.T) {
	tests := []struct {
		name              string
		targetClusters    bool
		clustersNamespace string
		want              string
	}{
		{"enabled", true, "c5c3-clusters", "c5c3-clusters"},
		{"operator opts out", false, "c5c3-clusters", ""},
		{"install opts out", true, "", ""},
		{"both off", false, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ManagerConfig{TargetClusters: tc.targetClusters}
			opts := runOptions{clustersNamespace: tc.clustersNamespace}
			if got := targetClustersNamespace(cfg, opts); got != tc.want {
				t.Fatalf("targetClustersNamespace() = %q, want %q", got, tc.want)
			}
		})
	}
}

// secretNamespaces returns the namespaces the options scope the Secret
// informer to. ByObject is keyed by a client.Object pointer, so a lookup with a
// fresh &corev1.Secret{} never matches the stored key and the entry has to be
// found by type.
func secretNamespaces(t *testing.T, opts cache.Options) map[string]cache.Config {
	t.Helper()
	for obj, byObject := range opts.ByObject {
		if _, ok := obj.(*corev1.Secret); ok {
			return byObject.Namespaces
		}
	}
	t.Fatal("expected a ByObject entry for *corev1.Secret")
	return nil
}

// TestCacheOptions_withNamespace verifies that cacheOptions with a non-empty
// namespace returns cache.Options with DefaultNamespaces containing that
// namespace key.
func TestCacheOptions_withNamespace(t *testing.T) {
	syncPeriod := 10 * time.Minute
	opts := cacheOptions(syncPeriod, "tenant-a", "c5c3-clusters")

	if opts.DefaultNamespaces == nil {
		t.Fatal("expected DefaultNamespaces to be non-nil when namespace is set")
	}
	if _, ok := opts.DefaultNamespaces["tenant-a"]; !ok {
		t.Fatal("expected DefaultNamespaces to contain 'tenant-a'")
	}
}

// TestCacheOptions_withoutNamespace verifies that cacheOptions with an empty
// namespace returns cache.Options with nil DefaultNamespaces, allowing
// cluster-wide watches. A cluster-wide cache already reaches the clusters
// namespace, so Secrets need no per-object scope either.
func TestCacheOptions_withoutNamespace(t *testing.T) {
	syncPeriod := 10 * time.Minute
	opts := cacheOptions(syncPeriod, "", "c5c3-clusters")

	if opts.DefaultNamespaces != nil {
		t.Fatalf("expected DefaultNamespaces to be nil when namespace is empty, got: %v", opts.DefaultNamespaces)
	}
	if opts.ByObject != nil {
		t.Fatalf("expected ByObject to be nil when namespace is empty, got: %v", opts.ByObject)
	}
}

// TestCacheOptions_syncPeriodPreserved verifies that SyncPeriod is always
// configured regardless of the namespace value.
func TestCacheOptions_syncPeriodPreserved(t *testing.T) {
	syncPeriod := 10 * time.Minute

	opts := cacheOptions(syncPeriod, "tenant-a", "c5c3-clusters")
	if opts.SyncPeriod == nil || *opts.SyncPeriod != syncPeriod {
		t.Fatalf("expected SyncPeriod %v with namespace set, got: %v", syncPeriod, opts.SyncPeriod)
	}

	opts = cacheOptions(syncPeriod, "", "c5c3-clusters")
	if opts.SyncPeriod == nil || *opts.SyncPeriod != syncPeriod {
		t.Fatalf("expected SyncPeriod %v without namespace, got: %v", syncPeriod, opts.SyncPeriod)
	}
}

// TestCacheOptions_singleNamespaceEntry verifies that DefaultNamespaces has
// exactly one entry with an empty cache.Config value when namespace is set
func TestCacheOptions_singleNamespaceEntry(t *testing.T) {
	syncPeriod := 10 * time.Minute
	opts := cacheOptions(syncPeriod, "tenant-a", "c5c3-clusters")

	if len(opts.DefaultNamespaces) != 1 {
		t.Fatalf("expected exactly 1 entry in DefaultNamespaces, got: %d", len(opts.DefaultNamespaces))
	}
	cfg := opts.DefaultNamespaces["tenant-a"]
	if !reflect.DeepEqual(cfg, cache.Config{}) {
		t.Fatalf("expected empty cache.Config value, got: %+v", cfg)
	}
}

// TestCacheOptions_scopedSecretsSpanTheClustersNamespace verifies that under
// namespace scoping the Secret informer covers the clusters namespace as well.
// Without it the kubeconfig provider would never observe a registration Secret,
// and no target cluster would ever be engaged.
func TestCacheOptions_scopedSecretsSpanTheClustersNamespace(t *testing.T) {
	syncPeriod := 10 * time.Minute
	opts := cacheOptions(syncPeriod, "tenant-a", "c5c3-clusters")

	nss := secretNamespaces(t, opts)
	if len(nss) != 2 {
		t.Fatalf("expected the Secret informer to span 2 namespaces, got %d: %v", len(nss), nss)
	}
	for _, ns := range []string{"tenant-a", "c5c3-clusters"} {
		if _, ok := nss[ns]; !ok {
			t.Fatalf("expected the Secret informer to cover %q, got: %v", ns, nss)
		}
	}
}

// TestCacheOptions_scopedSecretsCollapseEqualNamespaces verifies that an
// operator watching the clusters namespace itself ends up with a single Secret
// informer entry rather than a duplicated one.
func TestCacheOptions_scopedSecretsCollapseEqualNamespaces(t *testing.T) {
	syncPeriod := 10 * time.Minute
	opts := cacheOptions(syncPeriod, "c5c3-clusters", "c5c3-clusters")

	nss := secretNamespaces(t, opts)
	if len(nss) != 1 {
		t.Fatalf("expected exactly 1 Secret namespace when both are equal, got %d: %v", len(nss), nss)
	}
	if _, ok := nss["c5c3-clusters"]; !ok {
		t.Fatalf("expected the Secret informer to cover 'c5c3-clusters', got: %v", nss)
	}
}

// TestCacheOptions_scopedSecretsDropEmptyClustersNamespace verifies that an
// empty clusters namespace is left out of the map. The empty string is the
// cache's all-namespaces key, so writing it would widen the Secret informer
// cluster-wide and break the namespace scoping the operator was started with.
func TestCacheOptions_scopedSecretsDropEmptyClustersNamespace(t *testing.T) {
	syncPeriod := 10 * time.Minute
	opts := cacheOptions(syncPeriod, "tenant-a", "")

	nss := secretNamespaces(t, opts)
	if len(nss) != 1 {
		t.Fatalf("expected exactly 1 Secret namespace, got %d: %v", len(nss), nss)
	}
	if _, ok := nss[cache.AllNamespaces]; ok {
		t.Fatalf("expected no all-namespaces key in the Secret informer, got: %v", nss)
	}
}
