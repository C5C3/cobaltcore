// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration coverage for the real SetupWithManager methods of the Barbican
// and BarbicanSecretStore reconcilers. The inline harness builds both
// controllers by hand (bypassing SetupWithManager): it calls the same index
// registration functions but hard-codes the RESTMapper latches, the builder
// chains and the controller options. So the RESTMapper probes, the multicluster
// builder chains, bootstrap.TypedControllerOptions and SetupWithManager's own
// index registration calls run only here. This test wires both production
// SetupWithManager methods onto an envtest-backed manager as main.go does,
// starts it, and waits for each controller's first status write, so a
// regression that registers a watch on a kind the API server does not serve, or
// leaves an informer unsynced, fails here instead of only in a live cluster.
//
// CONSTRAINT: exactly one test in this package binary may call these real
// SetupWithManager methods. controller-runtime's global controller-name tracker
// rejects a second controller named "barbican" or "barbicansecretstore"
// registered without controller.Options{SkipNameValidation}. The inline harness
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
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/barbican/internal/testutil"
)

// TestSetupWithManager_AllControllersStart registers the production
// BarbicanReconciler and BarbicanSecretStoreReconciler via their real
// SetupWithManager methods against an envtest manager that serves every
// watched CRD (the operator CRDs plus the shared fake CRDs), asserts the
// RESTMapper probes latched, and then waits for each controller to stamp
// status.observedGeneration on a CR of its kind.
func TestSetupWithManager_AllControllersStart(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	var (
		barbican *BarbicanReconciler
		store    *BarbicanSecretStoreReconciler
	)
	c, ctx, _ := testutil.SetupBarbicanEnvTestWithController(
		t,
		barbicanv1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache. The webhook manifests envtest
			// installs carry both kinds (failurePolicy=Fail), so both handlers
			// must be served.
			if err := (&barbicanv1alpha1.BarbicanWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			return (&barbicanv1alpha1.BarbicanSecretStoreWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
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
			// The Barbican reconciler is registered first, as in main.go: its
			// SetupWithManager is the single registration site for the field
			// indexes both controllers resolve through.
			barbican = &BarbicanReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("barbican-controller"),
				MaxConcurrentReconciles: 1,
				Resolver:                mcMgr,
				// The healthy stub keeps the health-check probe from firing HTTP
				// GETs at a Service DNS name envtest cannot resolve.
				HTTPClient: &stubDoer{status: http.StatusOK},
			}
			if err := barbican.SetupWithManager(mcMgr); err != nil {
				return err
			}
			// NewOpenBaoClient stays nil (the production constructor): the managed
			// store fixture waits on an absent OpenBaoCluster and dials nothing.
			store = &BarbicanSecretStoreReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("barbicansecretstore-controller"),
				Resolver: mcMgr,
			}
			return store.SetupWithManager(mcMgr)
		},
	)

	g.Expect(barbican).NotTo(BeNil(),
		"registerController callback must have constructed the Barbican reconciler")
	g.Expect(store).NotTo(BeNil(),
		"registerController callback must have constructed the BarbicanSecretStore reconciler")
	g.Expect(barbican.gatewayAPIAvailable).To(BeTrue(),
		"SetupWithManager must detect the Gateway API HTTPRoute CRD served from fake_crds/gateway-api")
	g.Expect(barbican.vpaAvailable).To(BeTrue(),
		"SetupWithManager must detect the VerticalPodAutoscaler CRD served from fake_crds/vertical-pod-autoscaler")

	ns := createTestNamespace(t, ctx, c)

	cr := integrationBarbicanCR("barbican", ns)
	g.Expect(c.Create(ctx, cr)).To(Succeed(), "create Barbican fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, cr,
		func() int64 { return cr.Status.ObservedGeneration }, "Barbican", eventuallyTimeout, pollInterval)

	secretStore := integrationManagedStore("store", ns, "barbican", "openbao")
	g.Expect(c.Create(ctx, secretStore)).To(Succeed(), "create BarbicanSecretStore fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, secretStore,
		func() int64 { return secretStore.Status.ObservedGeneration }, "BarbicanSecretStore", eventuallyTimeout, pollInterval)
}
