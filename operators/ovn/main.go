// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package main is the entrypoint for the OVN operator.
//
// Hand-crafted like the keystone operator's main (see its DEVIATION note):
// the manager setup follows kubebuilder v4 / controller-runtime v0.23+ patterns
// via the shared bootstrap package.
package main

import (
	"os"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/ovn/internal/controller"

	ctrl "sigs.k8s.io/controller-runtime"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
)

var scheme = bootstrap.NewScheme(
	ovnv1alpha1.AddToScheme,
	// cert-manager types are required for the OVN PKI: reconcileTLS applies the
	// Certificates the databases and their clients present, and the teardown
	// sweep lists them among the OVNCentral child kinds.
	certmanagerv1.AddToScheme,
	// +kubebuilder:scaffold:scheme
)

func main() {
	if err := bootstrap.Run(bootstrap.ManagerConfig{
		Scheme:           scheme,
		LeaderElectionID: "ovn.openstack.c5c3.io",
		// The reconcilers resolve spec.targetClusterRef, so the binary engages
		// the clusters registered in --clusters-namespace.
		TargetClusters: true,
		SetupFunc: func(mcMgr mcmanager.Manager, webhooks bool, maxConcurrentReconciles int) error {
			mgr := mcMgr.GetLocalManager()
			// Register the operator's Prometheus collectors on the
			// controller-runtime registry before wiring controllers, so a
			// duplicate-registration fails startup cleanly instead of panicking
			// mid-reconcile.
			if err := controller.RegisterMetrics(); err != nil {
				return err
			}
			// +kubebuilder:scaffold:builder — register controllers here
			if err := (&controller.OVNCentralReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("ovncentral-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: maxConcurrentReconciles,
				Resolver:                mcMgr,
			}).SetupWithManager(mcMgr); err != nil {
				return err
			}
			// The chassis controller runs in the same manager (a second
			// reconciler, not a second binary). It MUST be registered after the
			// OVNCentral reconciler: that reconciler's SetupWithManager is the
			// single registration site for the OVNChassis field index this
			// controller's central-to-chassis mapper lists through.
			if err := (&controller.OVNChassisReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("ovnchassis-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: maxConcurrentReconciles,
				Resolver:                mcMgr,
			}).SetupWithManager(mcMgr); err != nil {
				return err
			}
			if webhooks {
				// DECISION: the webhooks read through mgr.GetAPIReader()
				// (direct, uncached) rather than mgr.GetClient(), so a
				// cluster-scoped lookup never rejects a just-created object from
				// a stale informer cache and the cached client's lazy informer
				// start does not happen inside the webhook timeout.
				if err := (&ovnv1alpha1.OVNCentralWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
					return err
				}
				if err := (&ovnv1alpha1.OVNChassisWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
					return err
				}
			}
			return nil
		},
	}); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to run manager")
		os.Exit(1)
	}
}
