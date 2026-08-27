// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package main is the entrypoint for the Neutron operator. One binary serves
// both kinds: Neutron, the network API service with its RPC workers and its OVN
// database synchronisation, and NeutronMetadataAgent, the DaemonSet that answers
// the instances' metadata requests on the nodes an OVNChassis programs.
//
// Hand-crafted like the keystone operator's main (see its DEVIATION note):
// the manager setup follows kubebuilder v4 / controller-runtime v0.23+ patterns
// via the shared bootstrap package.
package main

import (
	"os"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/neutron/internal/controller"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"

	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
)

var scheme = bootstrap.NewScheme(
	neutronv1alpha1.AddToScheme,
	// OVN types are required for both kinds: the Neutron reconciler reads the
	// OVNCentral its spec.ovn.centralRef names and watches it for the addresses
	// it publishes, and the agent reconciler resolves its OVNChassis and that
	// chassis's central.
	ovnv1alpha1.AddToScheme,
	// ESO v1 types are required for the credential gate: reconcileSecrets reads
	// the ExternalSecret and the OpenBao ClusterSecretStore/SecretStore.
	esov1.SchemeBuilder.AddToScheme,
	// MariaDB types are required so reconcileDatabase can provision and finalize
	// the Database/User/Grant CRs and watch the MariaDB cluster.
	mariadbv1alpha1.AddToScheme,
	gatewayv1.Install,
	// +kubebuilder:scaffold:scheme
)

func main() {
	if err := bootstrap.Run(bootstrap.ManagerConfig{
		Scheme:           scheme,
		LeaderElectionID: "neutron.openstack.c5c3.io",
		// The reconcilers resolve spec.targetClusterRef, so the binary engages
		// the clusters registered in --clusters-namespace.
		TargetClusters: true,
		SetupFunc: func(mcMgr mcmanager.Manager, webhooks bool, maxConcurrentReconciles int) error {
			mgr := mcMgr.GetLocalManager()
			// Register the operator's Prometheus collectors on the
			// controller-runtime registry before wiring controllers, so a
			// duplicate-registration fails startup cleanly instead of panicking
			// mid-reconcile. One instrumenter serves both pipelines, so this call
			// covers both reconcilers below.
			if err := controller.RegisterMetrics(); err != nil {
				return err
			}
			// +kubebuilder:scaffold:builder — register controllers here
			if err := (&controller.NeutronReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("neutron-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				OperatorNamespace:       bootstrap.DetectOperatorNamespace(),
				MaxConcurrentReconciles: maxConcurrentReconciles,
				Resolver:                mcMgr,
			}).SetupWithManager(mcMgr); err != nil {
				return err
			}
			// The metadata-agent controller runs in the same manager (a second
			// reconciler, not a second binary). It is registered after the Neutron
			// reconciler, the single registration site for the Neutron field
			// indexes, so both controllers find every index in place.
			if err := (&controller.NeutronMetadataAgentReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("neutronmetadataagent-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
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
				if err := (&neutronv1alpha1.NeutronWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
					return err
				}
				if err := (&neutronv1alpha1.NeutronMetadataAgentWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
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
