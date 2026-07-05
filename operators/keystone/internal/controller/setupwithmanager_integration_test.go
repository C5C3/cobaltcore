// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration coverage for the real KeystoneReconciler.SetupWithManager. The
// other integration tests build the controller inline (bypassing
// SetupWithManager), so the RESTMapper probes, the field indexer registration,
// and the full Owns/Watches builder chain are never executed by any test. This
// test wires the production SetupWithManager onto an envtest-backed manager and
// starts it, so a regression that drops a watch or crashes the manager on a
// missing kind fails here instead of only in a live cluster.
//
// CONSTRAINT: exactly one test in this package binary may call the real
// SetupWithManager. controller-runtime's global controller-name tracker rejects
// a second controller named "keystone" registered without
// controller.Options{SkipNameValidation}. The inline harness
// (setupEnvTestWithController) sets SkipNameValidation precisely so it does not
// contend with this test; a future second real-SetupWithManager test would need
// its own process or the same skip.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	ctrl "sigs.k8s.io/controller-runtime"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/testutil"
)

// TestSetupWithManager_StartsManagerWithAllWatches registers the production
// KeystoneReconciler via its real SetupWithManager against an envtest manager
// that has every watched CRD installed (the operator CRD plus the shared fake
// CRDs for MariaDB, ESO, cert-manager, and Gateway API). The shared skeleton
// then starts the manager, so all Owns/Watches informers — including the two
// RESTMapper-conditional Owns branches (HTTPRoute, Certificate) — must sync
// against the real API server; a missing watched kind would fail mgr.Start.
func TestSetupWithManager_StartsManagerWithAllWatches(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	var r *KeystoneReconciler
	// The setup helper starts the manager and blocks until the webhook server is
	// ready; if it returns, mgr.Start succeeded and every registered informer
	// synced. A watched CRD that was missing would have failed mgr.Start and the
	// helper would have surfaced the error via t.Errorf.
	_, _, _ = testutil.SetupKeystoneEnvTestWithController(
		t,
		keystonev1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			return (&keystonev1alpha1.KeystoneWebhook{Client: mgr.GetClient()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			r = &KeystoneReconciler{
				Client:     mgr.GetClient(),
				Scheme:     mgr.GetScheme(),
				Recorder:   mgr.GetEventRecorderFor("keystone-controller"),
				HTTPClient: testHealthyHTTPClient(),
			}
			return r.SetupWithManager(mgr)
		},
	)

	g.Expect(r).NotTo(BeNil(),
		"registerController callback must have constructed the reconciler")
	// The fake gateway-api and cert-manager CRDs are loaded, so the RESTMapper
	// probes in SetupWithManager must detect both kinds and enable their
	// conditional Owns/Watches branches.
	g.Expect(r.gatewayAPIAvailable).To(BeTrue(),
		"SetupWithManager must detect the Gateway API CRD from the RESTMapper")
	g.Expect(r.certManagerAvailable).To(BeTrue(),
		"SetupWithManager must detect the cert-manager CRD from the RESTMapper")
}
