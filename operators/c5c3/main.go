// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package main is the entrypoint for the C5C3 operator (CC-0001, CC-0110).
//
// DEVIATION from architecture/01-project-setup.md (CC-0001):
// Hand-crafted instead of `operator-sdk init` — the SDK scaffolds config/,
// internal/controller/, Dockerfile, and a per-module Makefile that would be
// immediately deleted for this minimal scaffolding phase. The manager setup
// follows kubebuilder v4 / controller-runtime v0.23+ patterns directly.
package main

import (
	"os"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"

	"github.com/c5c3/forge/internal/common/bootstrap"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/forge/operators/c5c3/internal/controller"
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
	// MariaDB CR — projected and Owned by reconcileInfrastructure.
	utilruntime.Must(mariadbv1alpha1.AddToScheme(scheme))
	// ESO PushSecret (v1alpha1) and ClusterSecretStore/ExternalSecret (v1) — the
	// admin-credential push and the K-ORC clouds.yaml gate read/write these.
	utilruntime.Must(esov1alpha1.SchemeBuilder.AddToScheme(scheme))
	utilruntime.Must(esov1.SchemeBuilder.AddToScheme(scheme))
	// K-ORC CRs (ApplicationCredential, Service, Endpoint, ...) — minted/Owned by
	// reconcileKORC and reconcileCatalog.
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
		SetupFunc: func(mgr ctrl.Manager, webhooks bool) error {
			// +kubebuilder:scaffold:builder — register controllers here
			if err := (&controller.ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
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
				if err := (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetClient()}).SetupWebhookWithManager(mgr); err != nil {
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
