// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// novaGatewaySpec returns an external-exposure block on the shared Gateway, the
// shape all three of Nova's gateway blocks take.
func novaGatewaySpec(hostname string) *novav1alpha1.GatewaySpec {
	return &novav1alpha1.GatewaySpec{
		Hostname:  hostname,
		ParentRef: novav1alpha1.GatewayParentRefSpec{Name: "gw", Namespace: "gateway-system"},
	}
}

// routeCase describes one of Nova's three routes for the lifecycle table: how to
// set and clear its gateway block, the route it names, the condition it reports
// on, and the sub-reconciler that drives it.
type routeCase struct {
	name          string
	routeName     string
	conditionType string
	enable        func(*novav1alpha1.Nova)
	disable       func(*novav1alpha1.Nova)
	reconcile     func(context.Context, *NovaReconciler, *novav1alpha1.Nova) (ctrl.Result, error)
}

// routeCases returns the three routes. The route names are part of the contract:
// a deployer reads them off the cluster, so they are pinned as literals here
// rather than derived from the helpers under test.
func routeCases() []routeCase {
	return []routeCase{
		{
			name:          "API",
			routeName:     testNovaName,
			conditionType: conditionTypeHTTPRouteReady,
			enable:        func(n *novav1alpha1.Nova) { n.Spec.Gateway = novaGatewaySpec("nova.example.com") },
			disable:       func(n *novav1alpha1.Nova) { n.Spec.Gateway = nil },
			reconcile: func(ctx context.Context, r *NovaReconciler, n *novav1alpha1.Nova) (ctrl.Result, error) {
				return r.reconcileHTTPRoute(ctx, r.Client, n)
			},
		},
		{
			name:          "metadata",
			routeName:     testNovaName + "-metadata",
			conditionType: conditionTypeMetadataHTTPRouteReady,
			enable: func(n *novav1alpha1.Nova) {
				n.Spec.Metadata.Gateway = novaGatewaySpec("metadata.example.com")
			},
			disable: func(n *novav1alpha1.Nova) { n.Spec.Metadata.Gateway = nil },
			reconcile: func(ctx context.Context, r *NovaReconciler, n *novav1alpha1.Nova) (ctrl.Result, error) {
				return r.reconcileMetadataHTTPRoute(ctx, r.Client, n)
			},
		},
		{
			name:          "console",
			routeName:     testNovaName + "-console",
			conditionType: conditionTypeConsoleHTTPRouteReady,
			enable: func(n *novav1alpha1.Nova) {
				n.Spec.ConsoleProxy.Gateway = novaGatewaySpec("console.example.com")
			},
			disable: func(n *novav1alpha1.Nova) { n.Spec.ConsoleProxy.Gateway = nil },
			reconcile: func(ctx context.Context, r *NovaReconciler, n *novav1alpha1.Nova) (ctrl.Result, error) {
				return r.reconcileConsoleHTTPRoute(ctx, r.Client, n)
			},
		},
	}
}

// staleRoute returns an HTTPRoute of the given name that a previous pass left
// behind, so the delete paths have something to remove.
func staleRoute(name string) *gatewayv1.HTTPRoute {
	route := &gatewayv1.HTTPRoute{}
	route.Name = name
	route.Namespace = testNamespace
	return route
}

func TestReconcileHTTPRoutes_GatewayNilDeletesAndNotRequired(t *testing.T) {
	for _, tc := range routeCases() {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			tc.disable(nova)
			r := newNovaTestReconciler(nova, staleRoute(tc.routeName))
			r.gatewayAPIAvailable = true

			res, err := tc.reconcile(context.Background(), r, nova)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue())
			cond := novaCondition(nova, tc.conditionType)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))

			var gone gatewayv1.HTTPRoute
			g.Expect(r.Get(context.Background(), objectKey(tc.routeName), &gone)).NotTo(Succeed(),
				"the stale HTTPRoute must be deleted when the gateway block is nil")
		})
	}
}

// A cluster without the Gateway API CRDs cannot hold the route the CR asks for,
// and applying it would fail on a kind the API server does not serve.
func TestReconcileHTTPRoutes_GatewayAPINotInstalled(t *testing.T) {
	for _, tc := range routeCases() {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			tc.enable(nova)
			r := newNovaTestReconciler(nova)
			r.gatewayAPIAvailable = false

			res, err := tc.reconcile(context.Background(), r, nova)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue())
			cond := novaCondition(nova, tc.conditionType)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonGatewayAPINotInstalled))

			var route gatewayv1.HTTPRoute
			g.Expect(r.Get(context.Background(), objectKey(tc.routeName), &route)).NotTo(Succeed(),
				"no route may be applied against a cluster that does not serve the kind")
		})
	}
}

func TestReconcileHTTPRoutes_NotAcceptedRequeues(t *testing.T) {
	for _, tc := range routeCases() {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			tc.enable(nova)
			r := newNovaTestReconciler(nova)
			r.gatewayAPIAvailable = true

			res, err := tc.reconcile(context.Background(), r, nova)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(requeueHTTPRouteAccepted))
			cond := novaCondition(nova, tc.conditionType)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotAccepted))

			var route gatewayv1.HTTPRoute
			g.Expect(r.Get(context.Background(), objectKey(tc.routeName), &route)).To(Succeed())
		})
	}
}

// A disabled console proxy has no Service to forward to, so its route is not
// required even though the gateway block stayed on the CR: the same pass that
// deletes the proxy Deployment deletes the route.
func TestReconcileConsoleHTTPRoute_DisabledProxyDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := disabledProxyNova()
	nova.Spec.ConsoleProxy.Gateway = novaGatewaySpec("console.example.com")
	r := newNovaTestReconciler(nova, staleRoute(consoleRouteName(nova)))
	r.gatewayAPIAvailable = true

	res, err := r.reconcileConsoleHTTPRoute(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := novaCondition(nova, conditionTypeConsoleHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))

	var gone gatewayv1.HTTPRoute
	g.Expect(r.Get(context.Background(), objectKey(consoleRouteName(nova)), &gone)).NotTo(Succeed(),
		"a disabled proxy keeps no route to a Service that is gone")
}

// Each route carries its own name, hostname, backend Service and port. The
// console route is the one where the route name and the backend name differ: the
// Service is named after the process, the route after what it exposes.
func TestBuildNovaHTTPRoutes_TargetTheirOwnService(t *testing.T) {
	nova := validNova()
	nova.Spec.Metadata.Gateway = novaGatewaySpec("metadata.example.com")

	for _, tc := range []struct {
		name      string
		route     *gatewayv1.HTTPRoute
		routeName string
		hostname  string
		backend   string
		port      int32
	}{
		{
			name: "API", route: buildAPIHTTPRoute(nova), routeName: testNovaName,
			hostname: "nova.example.com", backend: testNovaName, port: novaAPIPort,
		},
		{
			name: "metadata", route: buildMetadataHTTPRoute(nova), routeName: testNovaName + "-metadata",
			hostname: "metadata.example.com", backend: testNovaName + "-metadata", port: novaMetadataPort,
		},
		{
			name: "console", route: buildConsoleHTTPRoute(nova), routeName: testNovaName + "-console",
			hostname: "console.example.com", backend: testNovaName + "-novncproxy", port: novaConsolePort,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			g.Expect(tc.route.Name).To(Equal(tc.routeName))
			g.Expect(tc.route.Namespace).To(Equal(testNamespace))
			g.Expect(tc.route.Spec.Hostnames).To(ContainElement(gatewayv1.Hostname(tc.hostname)))
			g.Expect(tc.route.Spec.Rules).NotTo(BeEmpty())
			g.Expect(tc.route.Spec.Rules[0].BackendRefs).NotTo(BeEmpty())
			backend := tc.route.Spec.Rules[0].BackendRefs[0]
			g.Expect(string(backend.Name)).To(Equal(tc.backend))
			g.Expect(backend.Port).To(HaveValue(Equal(gatewayv1.PortNumber(tc.port))))
			// No timeouts stanza: the API answers short JSON requests and the
			// console proxy holds a WebSocket open, so the gateway
			// implementation's own default is the right cap for both.
			g.Expect(tc.route.Spec.Rules[0].Timeouts).To(BeNil())
		})
	}
}

// The three routes are named after the front ends they expose, and the console
// proxy gets a hostname of its own rather than a path under the API's: the
// console URL the API hands a browser opens the noVNC page and its WebSocket on
// "/" with nothing but a query string to tell them apart.
func TestNovaRouteNames(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	g.Expect(nova.Name).To(Equal(testNovaName))
	g.Expect(metadataName(nova)).To(Equal("nova-metadata"))
	g.Expect(consoleRouteName(nova)).To(Equal("nova-console"))
	g.Expect(consoleRouteName(nova)).NotTo(Equal(consoleProxyName(nova)),
		"the route is named after what it exposes, the Service after the process behind it")
	g.Expect(buildConsoleHTTPRoute(nova).Spec.Hostnames).
		NotTo(Equal(buildAPIHTTPRoute(nova).Spec.Hostnames),
			"the console proxy answers on a hostname of its own, not on a path under the API's")
}
