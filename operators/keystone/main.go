// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package main is the entrypoint for the Keystone operator.
//
// DEVIATION from the original project-setup design:
// Hand-crafted instead of `operator-sdk init` — the SDK scaffolds config/,
// internal/controller/, Dockerfile, and a per-module Makefile that would be
// immediately deleted for this minimal scaffolding phase. The manager setup
// follows kubebuilder v4 / controller-runtime v0.23+ patterns directly.
package main

import (
	"flag"
	"fmt"
	"os"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"

	"github.com/c5c3/forge/internal/common/bootstrap"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/controller"

	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
)

var scheme = bootstrap.NewScheme(
	keystonev1alpha1.AddToScheme,
	esov1alpha1.SchemeBuilder.AddToScheme,
	esov1.SchemeBuilder.AddToScheme,
	mariadbv1alpha1.AddToScheme,
	gatewayv1.Install,
	// cert-manager v1 scheme is required so reconcile_databasetls.go can
	// create/get cert-manager Certificates via the cached client. Without this, EnsureCertificate fails with "no kind is
	// registered for the type v1.Certificate" on every reconcile, leaving
	// the Keystone CR stuck without a DatabaseTLSReady condition.
	certmanagerv1.AddToScheme,
	// +kubebuilder:scaffold:scheme
)

func main() {
	var federationMetadataAllowCIDRs string
	if err := bootstrap.Run(bootstrap.ManagerConfig{
		Scheme:           scheme,
		LeaderElectionID: "keystone.openstack.c5c3.io",
		// The reconcilers resolve spec.targetClusterRef, so the binary engages
		// the clusters registered in --clusters-namespace.
		TargetClusters: true,
		RegisterFlags: func(fs *flag.FlagSet) {
			fs.StringVar(&federationMetadataAllowCIDRs, "federation-metadata-allow-cidrs", "",
				"Comma-separated CIDRs the operator may additionally dial when fetching "+
					"identity-provider metadata (OIDC discovery documents, SAML IdP metadata), "+
					"e.g. the cluster's Service CIDR for a trusted in-cluster IdP. Loopback, "+
					"link-local (cloud IMDS), multicast, and unspecified addresses stay blocked "+
					"even when covered. Empty (default) keeps all non-public addresses blocked.")
		},
		SetupFunc: func(mcMgr mcmanager.Manager, webhooks bool, maxConcurrentReconciles int) error {
			mgr := mcMgr.GetLocalManager()
			// Register the operator's Prometheus collectors on the
			// controller-runtime registry before wiring controllers, so a
			// duplicate-registration fails startup cleanly instead of panicking
			// mid-reconcile.
			if err := controller.RegisterMetrics(); err != nil {
				return err
			}
			// Fail startup fast on a malformed allowlist rather than silently
			// ignoring it and leaving a trusted in-cluster IdP unreachable.
			allowCIDRs, err := controller.ParseMetadataAllowCIDRs(federationMetadataAllowCIDRs)
			if err != nil {
				return fmt.Errorf("parsing --federation-metadata-allow-cidrs: %w", err)
			}
			// +kubebuilder:scaffold:builder — register controllers here
			if err := (&controller.KeystoneReconciler{
				Client:                       mgr.GetClient(),
				Scheme:                       mgr.GetScheme(),
				Recorder:                     mgr.GetEventRecorderFor("keystone-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				OperatorNamespace:            bootstrap.DetectOperatorNamespace(),
				MaxConcurrentReconciles:      maxConcurrentReconciles,
				FederationMetadataAllowCIDRs: allowCIDRs,
				Resolver:                     mcMgr,
			}).SetupWithManager(mcMgr); err != nil {
				return err
			}
			// The dedicated identity-backend controller runs in the same
			// manager (a second reconciler, not a second binary). It MUST be
			// registered after KeystoneReconciler: that reconciler's
			// SetupWithManager is the single registration site for the
			// KeystoneIdentityBackend field indexes both controllers use.
			if err := (&controller.KeystoneIdentityBackendReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("keystoneidentitybackend-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: maxConcurrentReconciles,
				Resolver:                mcMgr,
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
				// The identity-backend webhook shares the direct-reader rationale:
				// the domain-name uniqueness check must see a just-created sibling
				// backend, not a stale informer cache.
				if err := (&keystonev1alpha1.KeystoneIdentityBackendWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
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
