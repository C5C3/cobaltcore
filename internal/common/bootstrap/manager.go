// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
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

	// SetupFunc is an optional callback invoked after manager creation to
	// register controllers and webhooks. This is where each operator wires
	// its reconcilers. Corresponds to the +kubebuilder:scaffold:builder
	// marker in a standard kubebuilder project.
	SetupFunc func(mgr ctrl.Manager, webhooks bool) error
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
func cacheOptions(syncPeriod time.Duration, namespace string) cache.Options {
	opts := cache.Options{
		SyncPeriod: &syncPeriod,
	}
	if namespace != "" {
		opts.DefaultNamespaces = map[string]cache.Config{
			namespace: {},
		}
	}
	return opts
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

	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var enableWebhooks bool
	var syncPeriod time.Duration
	var namespace string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager, "+
			"ensuring only one active controller manager.")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true,
		"Enable admission webhooks. Set to false for namespace-scoped "+
			"deployments where webhook infrastructure is not available.")
	flag.DurationVar(&syncPeriod, "sync-period", 10*time.Minute,
		"The minimum frequency at which watched resources are reconciled "+
			"(e.g. 10m). Ensures eventual consistency if watch events are missed.")
	flag.StringVar(&namespace, "namespace", cfg.Namespace,
		"If set, restricts the operator to watch resources in this namespace only. "+
			"Used for namespace-scoped deployments. "+
			"Overrides ManagerConfig.Namespace when provided.")

	opts := zapOptions()
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: cfg.Scheme,
		Cache:  cacheOptions(syncPeriod, namespace),
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       cfg.LeaderElectionID,
	})
	if err != nil {
		return fmt.Errorf("unable to start manager: %w", err)
	}

	if cfg.SetupFunc != nil {
		if err := cfg.SetupFunc(mgr, enableWebhooks); err != nil {
			return fmt.Errorf("unable to set up controllers: %w", err)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}

	if namespace != "" {
		setupLog.Info("namespace-scoped mode enabled", "namespace", namespace)
	}
	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("problem running manager: %w", err)
	}
	return nil
}
