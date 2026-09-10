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

	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// cinderGatewaySpec returns the external-exposure block the route tests and the
// health-check probe test share.
func cinderGatewaySpec() *cinderv1alpha1.GatewaySpec {
	return &cinderv1alpha1.GatewaySpec{
		ParentRef: cinderv1alpha1.GatewayParentRefSpec{Name: "openstack-gw", Namespace: "envoy-gateway-system"},
		Hostname:  "cinder.127-0-0-1.nip.io",
	}
}

func TestCinderStatusEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()

	g.Expect(cinderStatusEndpoint(cinder)).To(Equal(internalCinderURL(cinder)),
		"a CR without external exposure still advertises a usable address")

	cinder.Spec.Gateway = cinderGatewaySpec()
	g.Expect(cinderStatusEndpoint(cinder)).To(Equal("https://cinder.127-0-0-1.nip.io/"))
}

func TestReconcileHTTPRoute_GatewayNilDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	stale := &gatewayv1.HTTPRoute{}
	stale.Name = testCinderName
	stale.Namespace = testNamespace
	r := newCinderTestReconciler(cinder, stale)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := cinderCondition(cinder, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))

	var gone gatewayv1.HTTPRoute
	err = r.Get(context.Background(), objectKey(testCinderName), &gone)
	g.Expect(err).To(HaveOccurred(), "stale HTTPRoute must be deleted when spec.gateway is nil")
}

// A cluster without the Gateway API CRDs cannot hold the route the CR asks for,
// and applying it would fail on a kind the API server does not serve.
func TestReconcileHTTPRoute_GatewayAPINotInstalledWithGatewaySet(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.Gateway = cinderGatewaySpec()
	r := newCinderTestReconciler(cinder)
	r.gatewayAPIAvailable = false

	res, err := r.reconcileHTTPRoute(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := cinderCondition(cinder, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonGatewayAPINotInstalled))

	var route gatewayv1.HTTPRoute
	g.Expect(r.Get(context.Background(), objectKey(testCinderName), &route)).NotTo(Succeed(),
		"no route may be applied against a cluster that does not serve the kind")
}

func TestReconcileHTTPRoute_NotAcceptedRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.Gateway = cinderGatewaySpec()
	r := newCinderTestReconciler(cinder)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(requeueHTTPRouteAccepted))
	cond := cinderCondition(cinder, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotAccepted))

	var route gatewayv1.HTTPRoute
	g.Expect(r.Get(context.Background(), objectKey(testCinderName), &route)).To(Succeed())
}

func TestBuildCinderHTTPRoute_TargetsAPIService(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.Gateway = cinderGatewaySpec()

	route := buildCinderHTTPRoute(cinder)

	g.Expect(route.Name).To(Equal(testCinderName))
	g.Expect(route.Spec.Hostnames).To(ContainElement(gatewayv1.Hostname("cinder.127-0-0-1.nip.io")))
	g.Expect(route.Spec.Rules).NotTo(BeEmpty())
	g.Expect(route.Spec.Rules[0].BackendRefs).NotTo(BeEmpty())
	backend := route.Spec.Rules[0].BackendRefs[0]
	g.Expect(string(backend.Name)).To(Equal(testCinderName))
	g.Expect(backend.Port).To(HaveValue(Equal(gatewayv1.PortNumber(cinderAPIPort))))
	// No timeouts stanza: the block-storage API answers short JSON requests, so
	// the gateway implementation's own default is the right cap.
	g.Expect(route.Spec.Rules[0].Timeouts).To(BeNil())
}
