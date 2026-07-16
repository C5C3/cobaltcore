// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package main is the entrypoint for the Glance operator.
//
// Hand-crafted like the keystone operator's main (see its DEVIATION note):
// the manager setup follows kubebuilder v4 / controller-runtime v0.23+ patterns
// via the shared bootstrap package.
package main

import (
	"os"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"

	"github.com/c5c3/forge/internal/common/bootstrap"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
	"github.com/c5c3/forge/operators/glance/internal/controller"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(glancev1alpha1.AddToScheme(scheme))
	// ESO v1 types are required for the credential gates: reconcileSecrets and
	// the GlanceBackend controller read the ExternalSecret and the OpenBao
	// ClusterSecretStore/SecretStore.
	utilruntime.Must(esov1.SchemeBuilder.AddToScheme(scheme))
	// MariaDB types are required so reconcileDatabase can provision and finalize
	// the Database/User/Grant CRs and watch the MariaDB cluster.
	utilruntime.Must(mariadbv1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	if err := bootstrap.Run(bootstrap.ManagerConfig{
		Scheme:           scheme,
		LeaderElectionID: "glance.openstack.c5c3.io",
		SetupFunc: func(mgr ctrl.Manager, webhooks bool, maxConcurrentReconciles int) error {
			// Register the operator's Prometheus collectors on the
			// controller-runtime registry before wiring controllers, so a
			// duplicate-registration fails startup cleanly instead of panicking
			// mid-reconcile.
			if err := controller.RegisterMetrics(); err != nil {
				return err
			}
			// +kubebuilder:scaffold:builder — register controllers here
			if err := (&controller.GlanceReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("glance-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				OperatorNamespace:       bootstrap.DetectOperatorNamespace(),
				MaxConcurrentReconciles: maxConcurrentReconciles,
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			// The dedicated GlanceBackend controller runs in the same manager (a
			// second reconciler, not a second binary). It MUST be registered after
			// GlanceReconciler: that reconciler's SetupWithManager is the single
			// registration site for the GlanceBackend field indexes both
			// controllers use.
			if err := (&controller.GlanceBackendReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("glancebackend-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: maxConcurrentReconciles,
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			if webhooks {
				// DECISION: the webhooks read through mgr.GetAPIReader() (direct,
				// uncached) rather than mgr.GetClient(). The PriorityClass
				// existence check must not reject a just-created PriorityClass from
				// a stale informer cache, and the cached client's lazy informer
				// start would otherwise happen inside the webhook timeout.
				if err := (&glancev1alpha1.GlanceWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
					return err
				}
				if err := (&glancev1alpha1.GlanceBackendWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
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
