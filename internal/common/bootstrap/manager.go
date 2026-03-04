// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"errors"
	"flag"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// ManagerConfig holds per-operator configuration for the shared manager
// bootstrap. Every operator provides its own Scheme (with custom API types
// registered) and a unique LeaderElectionID.
//
// Feature: CC-0001
type ManagerConfig struct {
	// Scheme is the runtime scheme with all required API types registered.
	// Must not be nil.
	Scheme *runtime.Scheme

	// LeaderElectionID is a unique identifier for leader election.
	// Must be non-empty and unique across operators sharing a namespace.
	LeaderElectionID string

	// SetupFunc is an optional callback invoked after manager creation to
	// register controllers and webhooks. This is where each operator wires
	// its reconcilers. Corresponds to the +kubebuilder:scaffold:builder
	// marker in a standard kubebuilder project.
	SetupFunc func(ctrl.Manager) error
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

// Run bootstraps and starts a controller-runtime manager with standard flag
// parsing, zap logging, metrics, and health/ready probes. It blocks until the
// manager stops or an error occurs.
//
// Callers retain control over scheme registration (including kubebuilder
// scaffold markers) and controller setup via [ManagerConfig].
//
// Feature: CC-0001
func Run(cfg ManagerConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager, "+
			"ensuring only one active controller manager.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: cfg.Scheme,
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
		if err := cfg.SetupFunc(mgr); err != nil {
			return fmt.Errorf("unable to set up controllers: %w", err)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("problem running manager: %w", err)
	}
	return nil
}
