// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

func glanceGatewaySpec() *glancev1alpha1.GatewaySpec {
	return &glancev1alpha1.GatewaySpec{
		ParentRef: glancev1alpha1.GatewayParentRefSpec{Name: "openstack-gw", Namespace: "envoy-gateway-system"},
		Hostname:  "glance.127-0-0-1.nip.io",
	}
}

func TestReconcileHTTPRoute_GatewayNilDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	stale := &gatewayv1.HTTPRoute{}
	stale.Name = "test-glance"
	stale.Namespace = "default"
	r := newGlanceTestReconciler(glance, stale)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), r.Client, glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))

	var gone gatewayv1.HTTPRoute
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"}, &gone)
	g.Expect(err).To(HaveOccurred(), "stale HTTPRoute must be deleted when spec.gateway is nil")
}

func TestReconcileHTTPRoute_GatewayAPINotInstalledWithGatewaySet(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Gateway = glanceGatewaySpec()
	r := newGlanceTestReconciler(glance)
	r.gatewayAPIAvailable = false

	res, err := r.reconcileHTTPRoute(context.Background(), r.Client, glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonGatewayAPINotInstalled))
}

func TestReconcileHTTPRoute_NotAcceptedRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Gateway = glanceGatewaySpec()
	r := newGlanceTestReconciler(glance)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), r.Client, glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(requeueHTTPRouteAccepted))
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotAccepted))
}

func TestBuildGlanceHTTPRoute_TargetsAPIService(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Gateway = glanceGatewaySpec()

	route := buildGlanceHTTPRoute(glance)

	g.Expect(route.Name).To(Equal("test-glance"))
	g.Expect(route.Spec.Hostnames).To(ContainElement(gatewayv1.Hostname("glance.127-0-0-1.nip.io")))
	g.Expect(route.Spec.Rules).NotTo(BeEmpty())
	g.Expect(route.Spec.Rules[0].BackendRefs).NotTo(BeEmpty())
	backend := route.Spec.Rules[0].BackendRefs[0]
	g.Expect(string(backend.Name)).To(Equal("test-glance"))
	g.Expect(backend.Port).To(HaveValue(Equal(gatewayv1.PortNumber(9292))))
}

// Image uploads and imports stream for minutes to hours, far past the gateway
// implementation's default route timeout (15s on Envoy Gateway), so the route
// raises it. It must not DISABLE it: the rule matches a bare "/" prefix, so the
// timeout is the only request-duration cap in front of the two concurrent
// request slots a glance-api pod serves, and "0s" would let two trickling
// clients pin them forever.
func TestBuildGlanceHTTPRoute_RaisesRequestTimeoutWithoutDisablingIt(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Gateway = glanceGatewaySpec()

	route := buildGlanceHTTPRoute(glance)

	g.Expect(route.Spec.Rules).NotTo(BeEmpty())
	timeouts := route.Spec.Rules[0].Timeouts
	g.Expect(timeouts).NotTo(BeNil())
	g.Expect(timeouts.Request).To(HaveValue(Equal(gatewayv1.Duration("4h"))))
	g.Expect(timeouts.Request).NotTo(HaveValue(Equal(gatewayv1.Duration("0s"))),
		"0s disables the timeout and removes the only cap on a trickling request")
}

func TestGlanceStatusEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()

	g.Expect(glanceStatusEndpoint(glance)).To(Equal("http://test-glance.default.svc.cluster.local:9292/"))

	glance.Spec.Gateway = glanceGatewaySpec()
	g.Expect(glanceStatusEndpoint(glance)).To(Equal("https://glance.127-0-0-1.nip.io/"))
}

// --- Remote children: the Gateway API answer comes from the target cluster ---

// hrTargetFake builds a target cluster's client like glanceFakeClientBuilder
// builds the management cluster's, behind the RESTMapper the capability probe
// asks. servesHTTPRoute is what separates a target cluster carrying the Gateway
// API CRDs from one without them.
func hrTargetFake(servesHTTPRoute bool, objs ...client.Object) client.Client {
	if servesHTTPRoute {
		return mctestutil.TargetFake(glanceFakeClientBuilder(objs...), httpRouteGVK)
	}
	return mctestutil.TargetFake(glanceFakeClientBuilder(objs...))
}

// The management cluster has no Gateway API and the target cluster has, so only
// the target's own answer can produce the route. Deciding from the latch would
// leave a CR that names a target cluster exposed nowhere.
func TestReconcileHTTPRoute_RemoteChildrenServeTheKindDespiteTheLatch(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Gateway = glanceGatewaySpec()
	r := newGlanceTestReconciler(glance)
	r.gatewayAPIAvailable = false
	target := hrTargetFake(true)

	_, err := r.reconcileHTTPRoute(context.Background(), mctestutil.RemoteChildren(t, r.Client, target), glance)

	g.Expect(err).NotTo(HaveOccurred())
	var route gatewayv1.HTTPRoute
	g.Expect(target.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"},
		&route)).To(Succeed(), "the route belongs on the cluster the children are written to")
}

// The latch says the management cluster serves the kind, the target does not,
// and the message has to name the cluster the operator actually looked at — an
// operator restart refreshes the latch, not the target's CRDs.
func TestReconcileHTTPRoute_RemoteChildrenWithoutTheKindNameTheTargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Gateway = glanceGatewaySpec()
	r := newGlanceTestReconciler(glance)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(),
		mctestutil.RemoteChildren(t, r.Client, hrTargetFake(false)), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonGatewayAPINotInstalled))
	g.Expect(cond.Message).To(ContainSubstring("target cluster does not serve"))
	g.Expect(cond.Message).NotTo(ContainSubstring("restart"),
		"restarting the operator re-probes the management cluster, which is not the cluster at fault")
}

// A target cluster that does not serve the kind is never asked to delete it:
// the delete would fail with "no matches for kind HTTPRoute", and the route the
// test seeds proves the flow short-circuited before reaching it.
func TestReconcileHTTPRoute_RemoteChildrenWithoutTheKindSkipTheDelete(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	// gateway is nil: the delete path, were it reached.
	r := newGlanceTestReconciler(glance)
	r.gatewayAPIAvailable = true
	seeded := &gatewayv1.HTTPRoute{}
	seeded.Name = "test-glance"
	seeded.Namespace = "default"
	target := hrTargetFake(false, seeded)

	res, err := r.reconcileHTTPRoute(context.Background(), mctestutil.RemoteChildren(t, r.Client, target), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))

	var route gatewayv1.HTTPRoute
	g.Expect(target.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"},
		&route)).To(Succeed(), "no delete may be attempted against a cluster that does not serve the kind")
}

// A probe that fails establishes nothing, so it is neither treated as "the
// target serves it" nor as "it does not": the pass fails and the CR says why.
func TestReconcileHTTPRoute_RemoteProbeFailureSurfacesCapabilityProbeFailed(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Gateway = glanceGatewaySpec()
	r := newGlanceTestReconciler(glance)
	children := mctestutil.UnprobeableChildren(r.Client)

	_, err := r.reconcileHTTPRoute(context.Background(), children, glance)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("probing the target cluster for the HTTPRoute kind:"))
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.CapabilityProbeFailed))
}
