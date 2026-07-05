// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration coverage for both real c5c3 SetupWithManager paths. The
// ControlPlane full-chain integration test builds the reconciler inline
// (bypassing SetupWithManager), and nothing exercises
// CredentialRotationReconciler.SetupWithManager at all, so the twelve
// ControlPlane Owns (including the unstructured Memcached/Certificate GVKs) and
// both Watches, plus the CredentialRotation builder, are never executed by any
// test. This test wires the production SetupWithManager methods of both
// controllers onto an envtest-backed manager and starts it, mirroring the
// main.go wiring, so a regression that drops a watch or crashes the manager on
// a missing kind fails here instead of only in a live cluster.
//
// CONSTRAINT: exactly one test in this package binary may call these real
// SetupWithManager methods. controller-runtime's global controller-name tracker
// rejects a second controller named "controlplane" registered without
// controller.Options{SkipNameValidation}. The inline harness
// (setupControlPlaneEnvTest) sets SkipNameValidation precisely so it does not
// contend with this test.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	ctrl "sigs.k8s.io/controller-runtime"

	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/forge/operators/c5c3/internal/testutil"
)

// TestSetupWithManager_BothControllersStart registers the production
// ControlPlaneReconciler and CredentialRotationReconciler via their real
// SetupWithManager methods against an envtest manager that has every watched
// CRD installed (c5c3 + keystone CRDs plus the shared fake CRDs for MariaDB,
// Memcached, ESO, cert-manager, and K-ORC). The shared skeleton then starts the
// manager, so every Owns/Watches informer — including the unstructured
// Memcached and Certificate Owns and the ClusterSecretStore Watch — must sync
// against the real API server; a missing watched kind would fail mgr.Start.
func TestSetupWithManager_BothControllersStart(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	registered := false
	// The setup helper starts the manager and blocks until the webhook server is
	// ready; if it returns, mgr.Start succeeded and every registered informer
	// synced. A watched CRD that was missing would have failed mgr.Start and the
	// helper would have surfaced the error via t.Errorf.
	_, _, _ = testutil.SetupC5c3EnvTestWithController(
		t,
		c5c3v1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache.
			return (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			// Mirror operators/c5c3/main.go: both controllers are registered on the
			// same manager.
			if err := (&ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			if err := (&CredentialRotationReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("credentialrotation-controller"),
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			registered = true
			return nil
		},
	)

	g.Expect(registered).To(BeTrue(),
		"both SetupWithManager calls must have completed without error")
}
