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
	// marker in a standard kubebuilder project. The third argument is the
	// resolved --max-concurrent-reconciles value; controllers that do not tune
	// concurrency may ignore it.
	SetupFunc func(mgr ctrl.Manager, webhooks bool, maxConcurrentReconciles int) error
}

// defaultMaxConcurrentReconciles is the default for the
// --max-concurrent-reconciles flag. The controller-runtime default of 1
// serialises reconciles across CRs; 2 lets a slow or flapping CR no longer
// block every other CR while keeping the extra worker footprint modest for a
// control-plane component.
const defaultMaxConcurrentReconciles = 2

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

	fs.IntVar(&o.maxConcurrentReconciles, "max-concurrent-reconciles", defaultMaxConcurrentReconciles,
		"Maximum number of reconciles that may run concurrently for a controller "+
			"(controller-runtime MaxConcurrentReconciles). Applied by controllers "+
			"that opt in; controllers that do not tune concurrency ignore it. "+
			"Defaults to 2.")

	o.zapOpts = zapOptions()
	o.zapOpts.BindFlags(fs)

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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: cfg.Scheme,
		Cache:  cacheOptions(opts.syncPeriod, opts.namespace),
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
	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("problem running manager: %w", err)
	}
	return nil
}
