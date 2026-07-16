// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package main is the entrypoint for the C5C3 operator.
//
// DEVIATION from architecture/01-project-setup.md
// Hand-crafted instead of `operator-sdk init` — the SDK scaffolds config/,
// internal/controller/, Dockerfile, and a per-module Makefile that would be
// immediately deleted for this minimal scaffolding phase. The manager setup
// follows kubebuilder v4 / controller-runtime v0.23+ patterns directly.
package main

import (
	"os"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"

	"github.com/c5c3/forge/internal/common/bootstrap"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/forge/operators/c5c3/internal/controller"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
)

// leaderElectionID is the unique leader-election lock identifier for the c5c3
// operator. KEEP this exact value: it is referenced by the deploy stack RBAC and
// is asserted by main_test.go so a rename cannot silently break leader election.
const leaderElectionID = "c5c3.openstack.c5c3.io"

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	// c5c3 own API types (ControlPlane, CredentialRotation).
	utilruntime.Must(c5c3v1alpha1.AddToScheme(scheme))
	// Keystone CR — the ControlPlane reconciler projects and Owns a Keystone child.
	utilruntime.Must(keystonev1alpha1.AddToScheme(scheme))
	utilruntime.Must(horizonv1alpha1.AddToScheme(scheme))
	// MariaDB CR — projected and Owned by reconcileInfrastructure.
	utilruntime.Must(mariadbv1alpha1.AddToScheme(scheme))
	// ESO PushSecret (v1alpha1) and ClusterSecretStore/ExternalSecret (v1) — the
	// admin-credential push and the K-ORC clouds.yaml gate read/write these.
	utilruntime.Must(esov1alpha1.SchemeBuilder.AddToScheme(scheme))
	utilruntime.Must(esov1.SchemeBuilder.AddToScheme(scheme))
	// ESO VaultDynamicSecret generator — projected and Owned by
	// reconcileDBCredentials to issue short-lived DB credentials in Dynamic mode.
	utilruntime.Must(esgenv1alpha1.AddToScheme(scheme))
	// K-ORC CRs (ApplicationCredential, Service, Endpoint, ...) — minted/Owned by
	// reconcileKORC and reconcileCatalog. This same group registration also
	// covers the Role/RoleAssignment kinds used for the service-account role
	// projection.
	utilruntime.Must(orcv1alpha1.AddToScheme(scheme))
	// Memcached (memcached.c5c3.io) is deliberately NOT registered: it ships no Go
	// module, so SetupWithManager's Owns(unstructured) resolves the GVK via the
	// cluster RESTMapper at runtime — no scheme registration is required.
	// +kubebuilder:scaffold:scheme
}

func main() {
	if err := bootstrap.Run(bootstrap.ManagerConfig{
		Scheme:           scheme,
		LeaderElectionID: leaderElectionID,
		SetupFunc: func(mgr ctrl.Manager, webhooks bool, maxConcurrentReconciles int) error {
			// Register the operator's Prometheus collectors on the
			// controller-runtime registry before wiring controllers, so a
			// duplicate-registration fails startup cleanly instead of panicking
			// mid-reconcile.
			if err := controller.RegisterMetrics(); err != nil {
				return err
			}
			// +kubebuilder:scaffold:builder — register controllers here
			if err := (&controller.ControlPlaneReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("controlplane-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: maxConcurrentReconciles,
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			if err := (&controller.CredentialRotationReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("credentialrotation-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			if webhooks {
				// DECISION Client must be non-nil for the
				// one-ControlPlane-per-namespace ValidateCreate check. It reads
				// through mgr.GetAPIReader() (direct, uncached) rather than
				// mgr.GetClient(): two concurrent CREATEs must not both see an
				// empty informer cache and both be admitted, and even sequential
				// creates within the cache-sync window would both pass against
				// the cached client.
				if err := (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
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
