// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration coverage for the real SetupWithManager methods of the Nova and
// NovaCompute reconcilers. The inline harness (registerNovaController and
// registerNovaComputeController) registers the production chains through
// setupWithOptions with SkipNameValidation set, so the probes, indexes and legs
// already run under envtest; what only this test runs is the public wrapper:
// the production bootstrap.TypedControllerOptions and the registration under
// the real controller names. This test wires both SetupWithManager methods onto
// an envtest-backed manager as main.go does, starts it, and waits for each
// controller's first status write.
//
// CONSTRAINT: exactly one test in this package binary may call these real
// SetupWithManager methods. controller-runtime's global controller-name tracker
// rejects a second controller named "nova" or "novacompute" registered without
// controller.Options{SkipNameValidation}. The inline harness
// (registerNovaController and registerNovaComputeController) sets
// SkipNameValidation precisely so it does not contend with this test, and
// multicluster_integration_test.go registers only the kubeconfig provider under
// its own fixed name. A future second real-SetupWithManager test would need its
// own process or the same skip.
package controller

import (
	"net/http"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/testutil"
)

// TestSetupWithManager_AllControllersStart registers the production
// NovaReconciler and NovaComputeReconciler via their real SetupWithManager
// methods against an envtest manager that serves every watched CRD (the
// operator CRDs plus the shared fake CRDs), asserts the RESTMapper probes
// latched, and then waits for each controller to stamp
// status.observedGeneration on a CR of its kind.
func TestSetupWithManager_AllControllersStart(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	var (
		nova    *NovaReconciler
		compute *NovaComputeReconciler
	)
	c, ctx, _ := testutil.SetupNovaEnvTestWithController(
		t,
		novav1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache. The webhook manifests envtest
			// installs carry both kinds (failurePolicy=Fail), so both handlers
			// must be served.
			if err := (&novav1alpha1.NovaWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			return (&novav1alpha1.NovaComputeWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
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
			// The healthy stubs keep the health-check and compute API probes from
			// firing HTTP GETs at a Service DNS name envtest cannot resolve.
			nova = &NovaReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("nova-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: 1,
				Resolver:                mcMgr,
				HTTPClient:              &stubDoer{status: http.StatusOK},
			}
			if err := nova.SetupWithManager(mcMgr); err != nil {
				return err
			}
			compute = &NovaComputeReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("novacompute-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				APIReader:               mgr.GetAPIReader(),
				MaxConcurrentReconciles: 1,
				Resolver:                mcMgr,
				HTTPClient:              &stubDoer{status: http.StatusOK},
			}
			return compute.SetupWithManager(mcMgr)
		},
	)

	g.Expect(nova).NotTo(BeNil(),
		"registerController callback must have constructed the Nova reconciler")
	g.Expect(compute).NotTo(BeNil(),
		"registerController callback must have constructed the NovaCompute reconciler")
	g.Expect(nova.gatewayAPIAvailable).To(BeTrue(),
		"SetupWithManager must detect the Gateway API HTTPRoute CRD served from fake_crds/gateway-api")
	g.Expect(nova.vpaAvailable).To(BeTrue(),
		"SetupWithManager must detect the VerticalPodAutoscaler CRD served from fake_crds/vertical-pod-autoscaler")
	g.Expect(compute.vpaAvailable).To(BeTrue(),
		"the NovaCompute SetupWithManager must detect the VerticalPodAutoscaler CRD served from fake_crds/vertical-pod-autoscaler")

	ns := createTestNamespace(t, ctx, c)

	cr := integrationNovaCR("nova", ns, nil)
	g.Expect(c.Create(ctx, cr)).To(Succeed(), "create Nova fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, cr,
		func() int64 { return cr.Status.ObservedGeneration }, "Nova", eventuallyTimeout, pollInterval)

	// The pool label is namespaced and no Node carries it, so the pool selects
	// no node.
	pool := &novav1alpha1.NovaCompute{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: ns},
		Spec: novav1alpha1.NovaComputeSpec{
			NovaRef:      novav1alpha1.NovaRef{Name: "nova"},
			NodeSelector: map[string]string{"openstack.c5c3.io/nova-compute-pool": ns},
		},
	}
	g.Expect(c.Create(ctx, pool)).To(Succeed(), "create NovaCompute fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, pool,
		func() int64 { return pool.Status.ObservedGeneration }, "NovaCompute", eventuallyTimeout, pollInterval)
}
