// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/forge/internal/common/multicluster"
)

// ManagerConfig holds per-operator configuration for the shared manager
// bootstrap. Every operator provides its own Scheme (with custom API types
// registered) and a unique LeaderElectionID.
type ManagerConfig struct {
	// Scheme is the runtime scheme with all required API types registered.
	// Must not be nil.
	Scheme *runtime.Scheme

	// LeaderElectionID is a unique identifier for leader election.
	// Must be non-empty and unique across operators sharing a namespace.
	LeaderElectionID string

	// Namespace restricts the operator to watch resources in this namespace
	// only. When empty, the operator watches all namespaces (cluster-wide).
	// This field allows callers constructing a manager programmatically to
	// opt into namespace scoping without going through the --namespace CLI
	// flag. If the --namespace flag is also set, the flag value takes
	// precedence.
	//
	Namespace string

	// SetupFunc is an optional, nil-safe callback invoked after manager
	// creation to register controllers and webhooks. This is where each
	// operator wires its reconcilers. Corresponds to the
	// +kubebuilder:scaffold:builder marker in a standard kubebuilder project.
	// It receives the multicluster manager, because only that manager can hand
	// out a client for a cluster other than the local one; operators that talk
	// to the management cluster only take its GetLocalManager. The third
	// argument is the resolved --max-concurrent-reconciles value; controllers
	// that do not tune concurrency may ignore it.
	SetupFunc func(mgr mcmanager.Manager, webhooks bool, maxConcurrentReconciles int) error

	// RegisterFlags is an optional, nil-safe hook for registering
	// operator-specific flags on the shared flag set. It is invoked after the
	// shared flags are registered and before parsing, so operators can add
	// binary-local flags (e.g. keystone's --federation-metadata-allow-cidrs)
	// without leaking them into the other operator binaries.
	RegisterFlags func(fs *flag.FlagSet)

	// TargetClusters enables the kubeconfig cluster provider: the operator
	// watches --clusters-namespace for registration Secrets and engages the
	// clusters they describe. Only operators whose reconcilers resolve a
	// target-cluster ref set it; one that only ever touches the management
	// cluster leaves it false and then holds no per-cluster caches, no
	// target-cluster credentials, and needs no Secret read outside its own
	// namespace.
	TargetClusters bool
}

// validate returns an error if required fields are missing.
func (c *ManagerConfig) validate() error {
	if c.Scheme == nil {
		return errors.New("bootstrap: Scheme must not be nil")
	}
	if c.LeaderElectionID == "" {
		return errors.New("bootstrap: LeaderElectionID must not be empty")
	}
	return nil
}

// cacheOptions builds cache.Options with the given sync period and optional
// namespace restriction. When namespace is non-empty, DefaultNamespaces is
// populated to restrict the informer cache to that single namespace.
//
// Secrets are the exception: their informer is widened to clustersNamespace as
// well, because the kubeconfig cluster provider reads the target-cluster
// registration Secrets from there and a cache restricted to the operator's own
// namespace would never deliver them. An unrestricted cache (empty namespace)
// already covers clustersNamespace, so it stays untouched. An empty
// clustersNamespace is dropped rather than written into the map: the empty
// string is the cache's all-namespaces key and would widen the Secret informer
// cluster-wide.
func cacheOptions(syncPeriod time.Duration, namespace, clustersNamespace string) cache.Options {
	opts := cache.Options{
		SyncPeriod: &syncPeriod,
	}
	if namespace != "" {
		opts.DefaultNamespaces = map[string]cache.Config{
			namespace: {},
		}
		secretNamespaces := map[string]cache.Config{
			namespace: {},
		}
		if clustersNamespace != "" {
			secretNamespaces[clustersNamespace] = cache.Config{}
		}
		opts.ByObject = map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Namespaces: secretNamespaces},
		}
	}
	return opts
}

// clusterOptions builds the options every target cluster the kubeconfig
// provider engages is created with. The per-cluster clients must decode the
// same kinds the local manager does: cluster.New otherwise defaults to the
// client-go global scheme, which carries no CRD kind, and a child write of a
// CRD-backed kind fails on the target with "no kind is registered".
//
// The cache is unrestricted here, and the operator's own --namespace does not
// reach it. What a target cluster's cache covers is decided per cluster by the
// namespaces key of that cluster's registration Secret, which the provider
// reads: the scope belongs to the credential the Secret carries, and two
// clusters registered with the same operator may well grant it in different
// namespaces. A registration that names none keeps the all-namespaces cache
// cluster.New builds. The clusters namespace is deliberately not passed on
// either: the registration Secrets live on the management cluster, so no target
// cache has to be widened for them.
func clusterOptions(scheme *runtime.Scheme, syncPeriod time.Duration) []cluster.Option {
	return []cluster.Option{func(o *cluster.Options) {
		o.Scheme = scheme
		o.Cache = cacheOptions(syncPeriod, "", "")
	}}
}

// targetClustersNamespace returns the namespace registration Secrets are read
// from, and with it the one switch for everything target clusters cost: the
// kubeconfig provider, its registration-Secret watch, and the one-namespace
// widening of the Secret informer that watch needs.
//
// It is empty — target clusters off — for an operator that does not resolve
// spec.targetClusterRef, and for an install that clears --clusters-namespace. A
// namespace-scoped deployment does exactly that: its Role grants nothing
// outside its own namespace, so a widened Secret informer would never sync and
// the manager would fail to start.
func targetClustersNamespace(cfg ManagerConfig, opts runOptions) string {
	if !cfg.TargetClusters {
		return ""
	}
	return opts.clustersNamespace
}

// zapOptions returns the base zap logging options shared by all operators.
// Development is false so the production operator binaries default to the
// controller-runtime production logging profile: JSON encoder, info-level
// verbosity, and stacktraces only at error level. A true value would ship a
// console encoder, debug-level verbosity, and a DPanic that panics — none of
// which is appropriate for a production binary. Operators opt back into
// development logging explicitly via the --zap-devel, --zap-log-level, and
// --zap-encoder flags that BindFlags registers against these options.
func zapOptions() zap.Options {
	return zap.Options{Development: false}
}

// runOptions holds the values parsed from the command-line flags. It is
// produced by parseRunOptions and consumed by run, keeping flag parsing
// separate from manager construction so each can be tested in isolation.
type runOptions struct {
	metricsAddr             string
	probeAddr               string
	enableLeaderElection    bool
	enableWebhooks          bool
	syncPeriod              time.Duration
	namespace               string
	clustersNamespace       string
	maxConcurrentReconciles int
	zapOpts                 zap.Options
}

// parseRunOptions registers the operator's flags on a fresh flag.FlagSet and
// parses args into a runOptions. Using a dedicated FlagSet (rather than the
// process-global flag.CommandLine) makes parsing reentrant and testable: it can
// be called more than once and with injected args without a flag-redefinition
// panic. The namespace flag defaults to cfg.Namespace so a programmatically
// configured manager can opt into namespace scoping without a CLI flag.
func parseRunOptions(cfg ManagerConfig, args []string) (runOptions, error) {
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)

	var o runOptions
	fs.StringVar(&o.metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	fs.StringVar(&o.probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	fs.BoolVar(&o.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager, "+
			"ensuring only one active controller manager.")
	fs.BoolVar(&o.enableWebhooks, "enable-webhooks", true,
		"Enable admission webhooks. Set to false for namespace-scoped "+
			"deployments where webhook infrastructure is not available.")
	fs.DurationVar(&o.syncPeriod, "sync-period", 10*time.Minute,
		"The minimum frequency at which watched resources are reconciled "+
			"(e.g. 10m). Ensures eventual consistency if watch events are missed.")
	fs.StringVar(&o.namespace, "namespace", cfg.Namespace,
		"If set, restricts the operator to watch resources in this namespace only. "+
			"Used for namespace-scoped deployments. "+
			"Overrides ManagerConfig.Namespace when provided.")
	fs.StringVar(&o.clustersNamespace, "clusters-namespace", "c5c3-clusters",
		"Namespace on the management cluster watched for target-cluster "+
			"kubeconfig Secrets. Empty disables target clusters entirely: no "+
			"cluster is engaged and no Secret is read outside the watched "+
			"namespace. Ignored by operators that do not support target clusters.")

	fs.IntVar(&o.maxConcurrentReconciles, "max-concurrent-reconciles", DefaultMaxConcurrentReconciles,
		"Maximum number of reconciles that may run concurrently for a controller "+
			"(controller-runtime MaxConcurrentReconciles). Applied by controllers "+
			"that opt in; controllers that do not tune concurrency ignore it. "+
			"Defaults to 2.")

	o.zapOpts = zapOptions()
	o.zapOpts.BindFlags(fs)

	if cfg.RegisterFlags != nil {
		cfg.RegisterFlags(fs)
	}

	if err := fs.Parse(args); err != nil {
		return runOptions{}, fmt.Errorf("parsing flags: %w", err)
	}
	return o, nil
}

// Run bootstraps and starts a controller-runtime manager with standard flag
// parsing, zap logging, metrics, and health/ready probes. It blocks until the
// manager stops or an error occurs.
//
// Callers retain control over scheme registration (including kubebuilder
// scaffold markers) and controller setup via [ManagerConfig].
func Run(cfg ManagerConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	opts, err := parseRunOptions(cfg, os.Args[1:])
	if err != nil {
		return err
	}
	return run(cfg, opts)
}

// run constructs and starts the manager from already-parsed options. It is the
// side-effecting core that Run wraps after flag parsing; splitting it out keeps
// Run a thin, reentrant entry point.
func run(cfg ManagerConfig, opts runOptions) error {
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts.zapOpts)))
	setupLog := ctrl.Log.WithName("setup")
	ctx := ctrl.SetupSignalHandler()

	clustersNamespace := targetClustersNamespace(cfg, opts)

	// Typed as the interface, so leaving it unset really is a nil provider:
	// mcmanager then reports a named cluster as unresolvable instead of
	// dereferencing a typed nil.
	var provider mcruntime.Provider
	var clusters *multicluster.KubeconfigProvider
	if clustersNamespace != "" {
		clusters = multicluster.NewKubeconfigProvider(multicluster.KubeconfigProviderOptions{
			Namespace:      clustersNamespace,
			ClusterOptions: clusterOptions(cfg.Scheme, opts.syncPeriod),
		})
		provider = clusters
	}

	mgr, err := mcmanager.New(ctrl.GetConfigOrDie(), provider, ctrl.Options{
		Scheme: cfg.Scheme,
		Cache:  cacheOptions(opts.syncPeriod, opts.namespace, clustersNamespace),
		Metrics: metricsserver.Options{
			BindAddress: opts.metricsAddr,
		},
		HealthProbeBindAddress: opts.probeAddr,
		LeaderElection:         opts.enableLeaderElection,
		LeaderElectionID:       cfg.LeaderElectionID,
	})
	if err != nil {
		return fmt.Errorf("unable to start manager: %w", err)
	}

	// The provider's engagement machinery has to be registered before the
	// controllers, so that cluster engagement precedes the first reconcile.
	if clusters != nil {
		if err := clusters.SetupWithManager(ctx, mgr); err != nil {
			return fmt.Errorf("unable to set up cluster provider: %w", err)
		}
	}

	if cfg.SetupFunc != nil {
		if err := cfg.SetupFunc(mgr, opts.enableWebhooks, opts.maxConcurrentReconciles); err != nil {
			return fmt.Errorf("unable to set up controllers: %w", err)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}

	if opts.namespace != "" {
		setupLog.Info("namespace-scoped mode enabled", "namespace", opts.namespace)
	}
	if clustersNamespace != "" {
		setupLog.Info("target clusters enabled", "clustersNamespace", clustersNamespace)
	}
	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("problem running manager: %w", err)
	}
	return nil
}
