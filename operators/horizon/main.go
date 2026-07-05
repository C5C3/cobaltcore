// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package main is the entrypoint for the Horizon operator.
//
// Hand-crafted like the keystone operator's main (see its DEVIATION note):
// the manager setup follows kubebuilder v4 / controller-runtime v0.23+
// patterns via the shared bootstrap package.
package main

import (
	"os"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"

	"github.com/c5c3/forge/internal/common/bootstrap"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
	"github.com/c5c3/forge/operators/horizon/internal/controller"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(horizonv1alpha1.AddToScheme(scheme))
	// ESO v1 types are required for the SECRET_KEY gate: reconcileSecrets
	// reads the ExternalSecret and the OpenBao ClusterSecretStore.
	utilruntime.Must(esov1.SchemeBuilder.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	if err := bootstrap.Run(bootstrap.ManagerConfig{
		Scheme:           scheme,
		LeaderElectionID: "horizon.openstack.c5c3.io",
		SetupFunc: func(mgr ctrl.Manager, webhooks bool, maxConcurrentReconciles int) error {
			// +kubebuilder:scaffold:builder — register controllers here
			if err := (&controller.HorizonReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				OperatorNamespace:       bootstrap.DetectOperatorNamespace(),
				MaxConcurrentReconciles: maxConcurrentReconciles,
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			if webhooks {
				// DECISION: the webhook reads through mgr.GetAPIReader()
				// (direct, uncached) rather than mgr.GetClient(). The
				// PriorityClass existence check must not reject a
				// just-created PriorityClass from a stale informer cache, and
				// the cached client's lazy informer start would otherwise
				// happen inside the webhook timeout.
				if err := (&horizonv1alpha1.HorizonWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
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
