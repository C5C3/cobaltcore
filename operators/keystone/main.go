// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package main is the entrypoint for the Keystone operator.
//
// DEVIATION from architecture/01-project-setup.md
// Hand-crafted instead of `operator-sdk init` — the SDK scaffolds config/,
// internal/controller/, Dockerfile, and a per-module Makefile that would be
// immediately deleted for this minimal scaffolding phase. The manager setup
// follows kubebuilder v4 / controller-runtime v0.23+ patterns directly.
package main

import (
	"os"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"

	"github.com/c5c3/forge/internal/common/bootstrap"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/controller"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(keystonev1alpha1.AddToScheme(scheme))
	utilruntime.Must(esov1alpha1.SchemeBuilder.AddToScheme(scheme))
	utilruntime.Must(esov1.SchemeBuilder.AddToScheme(scheme))
	utilruntime.Must(mariadbv1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	// cert-manager v1 scheme is required so reconcile_databasetls.go can
	// create/get cert-manager Certificates via the cached client. Without this, EnsureCertificate fails with "no kind is
	// registered for the type v1.Certificate" on every reconcile, leaving
	// the Keystone CR stuck without a DatabaseTLSReady condition.
	utilruntime.Must(certmanagerv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	if err := bootstrap.Run(bootstrap.ManagerConfig{
		Scheme:           scheme,
		LeaderElectionID: "keystone.openstack.c5c3.io",
		SetupFunc: func(mgr ctrl.Manager, webhooks bool) error {
			// +kubebuilder:scaffold:builder — register controllers here
			if err := (&controller.KeystoneReconciler{
				Client:            mgr.GetClient(),
				Scheme:            mgr.GetScheme(),
				Recorder:          mgr.GetEventRecorderFor("keystone-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				OperatorNamespace: controller.DetectOperatorNamespace(),
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			if webhooks {
				// DECISION: the webhook reads through mgr.GetAPIReader() (direct,
				// uncached) rather than mgr.GetClient(). The PriorityClass existence
				// check must not reject a just-created PriorityClass from a stale
				// informer cache, and the cached client's lazy informer start would
				// otherwise happen inside the webhook timeout.
				if err := (&keystonev1alpha1.KeystoneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
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
