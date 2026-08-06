// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
)

// barbicanGatewaySpec returns the external-exposure block the route tests and
// the endpoint assertions share.
func barbicanGatewaySpec() *barbicanv1alpha1.GatewaySpec {
	return &barbicanv1alpha1.GatewaySpec{
		ParentRef: barbicanv1alpha1.GatewayParentRefSpec{Name: "openstack-gw", Namespace: "envoy-gateway-system"},
		Hostname:  "barbican.127-0-0-1.nip.io",
	}
}

func TestReconcileHTTPRoute_GatewayNilDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	stale := &gatewayv1.HTTPRoute{}
	stale.Name = testBarbicanName
	stale.Namespace = testNamespace
	r := newBarbicanTestReconciler(barbican, stale)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := barbicanCondition(barbican, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))

	var gone gatewayv1.HTTPRoute
	err = r.Get(context.Background(), barbicanRequest.NamespacedName, &gone)
	g.Expect(err).To(HaveOccurred(), "stale HTTPRoute must be deleted when spec.gateway is nil")
}

func TestReconcileHTTPRoute_GatewayAPINotInstalledWithGatewaySet(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.Gateway = barbicanGatewaySpec()
	r := newBarbicanTestReconciler(barbican)
	r.gatewayAPIAvailable = false

	res, err := r.reconcileHTTPRoute(context.Background(), barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := barbicanCondition(barbican, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonGatewayAPINotInstalled))
}

func TestReconcileHTTPRoute_NotAcceptedRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.Gateway = barbicanGatewaySpec()
	r := newBarbicanTestReconciler(barbican)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(requeueHTTPRouteAccepted))
	cond := barbicanCondition(barbican, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotAccepted))

	var route gatewayv1.HTTPRoute
	g.Expect(r.Get(context.Background(), barbicanRequest.NamespacedName, &route)).To(Succeed())
}

func TestBuildBarbicanHTTPRoute_TargetsAPIService(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.Gateway = barbicanGatewaySpec()

	route := buildBarbicanHTTPRoute(barbican)

	g.Expect(route.Name).To(Equal(testBarbicanName))
	g.Expect(route.Spec.Hostnames).To(ContainElement(gatewayv1.Hostname("barbican.127-0-0-1.nip.io")))
	g.Expect(route.Spec.Rules).NotTo(BeEmpty())
	g.Expect(route.Spec.Rules[0].BackendRefs).NotTo(BeEmpty())
	backend := route.Spec.Rules[0].BackendRefs[0]
	g.Expect(string(backend.Name)).To(Equal(testBarbicanName))
	g.Expect(backend.Port).To(HaveValue(Equal(gatewayv1.PortNumber(barbicanAPIPort))))
	// No timeouts stanza: barbican answers short JSON requests, so the gateway
	// implementation's own default is the right cap.
	g.Expect(route.Spec.Rules[0].Timeouts).To(BeNil())
}

// The health probe never follows spec.gateway: it verifies that the WSGI app is
// serving, not that the ingress path in front of it resolves.
func TestInternalBarbicanURLIgnoresTheGateway(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	want := "http://test-barbican.openstack.svc.cluster.local:9311"

	g.Expect(internalBarbicanURL(barbican)).To(Equal(want))

	barbican.Spec.Gateway = barbicanGatewaySpec()
	g.Expect(internalBarbicanURL(barbican)).To(Equal(want))
	g.Expect(barbicanPublicEndpoint(barbican)).To(Equal("https://barbican.127-0-0-1.nip.io"),
		"the advertised endpoint does follow the gateway")
}
