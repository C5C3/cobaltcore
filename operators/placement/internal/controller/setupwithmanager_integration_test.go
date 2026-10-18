// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration coverage for the real PlacementReconciler.SetupWithManager. The
// inline harness builds the controller by hand (bypassing SetupWithManager): it
// calls the same index registration function but hard-codes the RESTMapper
// latches, the builder chain and the controller options. So the RESTMapper
// probes, the multicluster builder chain, bootstrap.TypedControllerOptions and
// SetupWithManager's own index registration call run only here. This test wires
// the production SetupWithManager onto an envtest-backed manager as main.go
// does, starts it, and waits for the controller's first status write, so a
// regression that registers a watch on a kind the API server does not serve, or
// leaves an informer unsynced, fails here instead of only in a live cluster.
//
// CONSTRAINT: exactly one test in this package binary may call the real
// SetupWithManager. controller-runtime's global controller-name tracker rejects
// a second controller named "placement" registered without
// controller.Options{SkipNameValidation}. The inline harness
// (setupEnvTestWithController) sets SkipNameValidation precisely so it does not
// contend with this test; a future second real-SetupWithManager test would need
// its own process or the same skip.
package controller

import (
	"net/http"
	"testing"

	. "github.com/onsi/gomega"
	ctrl "sigs.k8s.io/controller-runtime"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/placement/internal/testutil"
)

// TestSetupWithManager_StartsManagerWithAllWatches registers the production
// PlacementReconciler via its real SetupWithManager against an envtest manager
// that serves every watched CRD (the operator CRD plus the shared fake CRDs),
// asserts the RESTMapper probes latched, and then waits for the controller to
// stamp status.observedGeneration on a Placement CR.
func TestSetupWithManager_StartsManagerWithAllWatches(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	var r *PlacementReconciler
	c, ctx, _ := testutil.SetupPlacementEnvTestWithController(
		t,
		placementv1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache.
			return (&placementv1alpha1.PlacementWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			// A nil provider engages no remote cluster, so the remote legs add
			// nothing and every local informer must still sync. That is the
			// single-cluster default path: an operator started with an empty
			// --clusters-namespace clears the provider the same way.
			mcMgr, err := mcmanager.WithMultiCluster(mgr, nil)
			if err != nil {
				return err
			}
			r = &PlacementReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("placement-controller"),
				MaxConcurrentReconciles: 1,
				Resolver:                mcMgr,
				// The healthy stub keeps the health-check probe from firing HTTP
				// GETs at a Service DNS name envtest cannot resolve.
				HTTPClient: &stubDoer{status: http.StatusOK},
			}
			return r.SetupWithManager(mcMgr)
		},
	)

	g.Expect(r).NotTo(BeNil(),
		"registerController callback must have constructed the Placement reconciler")
	g.Expect(r.gatewayAPIAvailable).To(BeTrue(),
		"SetupWithManager must detect the Gateway API HTTPRoute CRD served from fake_crds/gateway-api")
	g.Expect(r.vpaAvailable).To(BeTrue(),
		"SetupWithManager must detect the VerticalPodAutoscaler CRD served from fake_crds/vertical-pod-autoscaler")

	ns := createTestNamespace(t, ctx, c)

	placement := integrationPlacement("placement", ns)
	g.Expect(c.Create(ctx, placement)).To(Succeed(), "create Placement fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, placement,
		func() int64 { return placement.Status.ObservedGeneration }, "Placement", eventuallyTimeout, pollInterval)
}
