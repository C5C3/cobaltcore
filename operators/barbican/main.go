// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package main is the entrypoint for the Barbican operator.
//
// Hand-crafted like the keystone operator's main (see its DEVIATION note):
// the manager setup follows kubebuilder v4 / controller-runtime v0.23+ patterns
// via the shared bootstrap package.
package main

import (
	"os"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/barbican/internal/controller"

	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
)

var scheme = bootstrap.NewScheme(
	barbicanv1alpha1.AddToScheme,
	// ESO v1 types are required for the credential gate: reconcileSecrets reads
	// the ExternalSecret and the OpenBao ClusterSecretStore/SecretStore.
	esov1.SchemeBuilder.AddToScheme,
	// MariaDB types are required so reconcileDatabase can provision and finalize
	// the Database/User/Grant CRs and watch the MariaDB cluster.
	mariadbv1alpha1.AddToScheme,
	gatewayv1.Install,
	// OpenBao types are required so the BarbicanSecretStore controller can read
	// the OpenBaoCluster a managed store provisions against as a typed object and
	// watch its Available condition.
	openbaov1alpha1.AddToScheme,
	// +kubebuilder:scaffold:scheme
)

func main() {
	if err := bootstrap.Run(bootstrap.ManagerConfig{
		Scheme:           scheme,
		LeaderElectionID: "barbican.openstack.c5c3.io",
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
			if err := (&controller.BarbicanReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("barbican-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				OperatorNamespace:       bootstrap.DetectOperatorNamespace(),
				MaxConcurrentReconciles: maxConcurrentReconciles,
				Resolver:                mcMgr,
			}).SetupWithManager(mcMgr); err != nil {
				return err
			}
			// The dedicated BarbicanSecretStore controller runs in the same manager
			// (a second reconciler, not a second binary). It MUST be registered
			// after BarbicanReconciler: that reconciler's SetupWithManager is the
			// single registration site for the BarbicanSecretStore field indexes
			// both controllers use.
			if err := (&controller.BarbicanSecretStoreReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("barbicansecretstore-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				Resolver: mcMgr,
			}).SetupWithManager(mcMgr); err != nil {
				return err
			}
			if webhooks {
				// DECISION: the webhooks read through mgr.GetAPIReader() (direct,
				// uncached) rather than mgr.GetClient(). The PriorityClass existence
				// check and the sibling-store lookup must not reject a just-created
				// object from a stale informer cache, and the cached client's lazy
				// informer start would otherwise happen inside the webhook timeout.
				if err := (&barbicanv1alpha1.BarbicanWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
					return err
				}
				if err := (&barbicanv1alpha1.BarbicanSecretStoreWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
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
