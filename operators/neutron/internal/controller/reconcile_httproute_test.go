// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// neutronGatewaySpec returns the external-exposure block the route tests and the
// health-check probe test share.
func neutronGatewaySpec() *neutronv1alpha1.GatewaySpec {
	return &neutronv1alpha1.GatewaySpec{
		ParentRef: neutronv1alpha1.GatewayParentRefSpec{Name: "openstack-gw", Namespace: "envoy-gateway-system"},
		Hostname:  "neutron.127-0-0-1.nip.io",
	}
}

func TestReconcileHTTPRoute_GatewayNilDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	stale := &gatewayv1.HTTPRoute{}
	stale.Name = testNeutronName
	stale.Namespace = testNamespace
	r := newNeutronTestReconciler(neutron, stale)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := neutronCondition(neutron, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))

	var gone gatewayv1.HTTPRoute
	err = r.Get(context.Background(), neutronKey(neutron), &gone)
	g.Expect(err).To(HaveOccurred(), "stale HTTPRoute must be deleted when spec.gateway is nil")
}

func TestReconcileHTTPRoute_GatewayAPINotInstalledWithGatewaySet(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Gateway = neutronGatewaySpec()
	r := newNeutronTestReconciler(neutron)
	r.gatewayAPIAvailable = false

	res, err := r.reconcileHTTPRoute(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := neutronCondition(neutron, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonGatewayAPINotInstalled))
}

func TestReconcileHTTPRoute_NotAcceptedRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Gateway = neutronGatewaySpec()
	r := newNeutronTestReconciler(neutron)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(requeueHTTPRouteAccepted))
	cond := neutronCondition(neutron, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotAccepted))

	var route gatewayv1.HTTPRoute
	g.Expect(r.Get(context.Background(), neutronKey(neutron), &route)).To(Succeed())
}

func TestBuildNeutronHTTPRoute_TargetsAPIService(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Gateway = neutronGatewaySpec()

	route := buildNeutronHTTPRoute(neutron)

	g.Expect(route.Name).To(Equal(testNeutronName))
	g.Expect(route.Spec.Hostnames).To(ContainElement(gatewayv1.Hostname("neutron.127-0-0-1.nip.io")))
	g.Expect(route.Spec.Rules).NotTo(BeEmpty())
	g.Expect(route.Spec.Rules[0].BackendRefs).NotTo(BeEmpty())
	backend := route.Spec.Rules[0].BackendRefs[0]
	g.Expect(string(backend.Name)).To(Equal(testNeutronName))
	g.Expect(backend.Port).To(HaveValue(Equal(gatewayv1.PortNumber(neutronAPIPort))))
	// No timeouts stanza: neutron answers short JSON requests, so the gateway
	// implementation's own default is the right cap.
	g.Expect(route.Spec.Rules[0].Timeouts).To(BeNil())
}

// --- Remote children: the Gateway API answer comes from the target cluster ---

// hrTargetFake builds a target cluster's client like neutronFakeClientBuilder
// builds the management cluster's, behind the RESTMapper the capability probe
// asks. servesHTTPRoute is what separates a target cluster carrying the Gateway
// API CRDs from one without them.
func hrTargetFake(servesHTTPRoute bool, objs ...client.Object) client.Client {
	if servesHTTPRoute {
		return mctestutil.TargetFake(neutronFakeClientBuilder(objs...), httpRouteGVK)
	}
	return mctestutil.TargetFake(neutronFakeClientBuilder(objs...))
}

// The management cluster has no Gateway API and the target cluster has, so only
// the target's own answer can produce the route. Deciding from the latch would
// leave a CR that names a target cluster exposed nowhere.
func TestReconcileHTTPRoute_RemoteChildrenServeTheKindDespiteTheLatch(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Gateway = neutronGatewaySpec()
	r := newNeutronTestReconciler(neutron)
	r.gatewayAPIAvailable = false
	target := hrTargetFake(true)

	_, err := r.reconcileHTTPRoute(context.Background(), mctestutil.RemoteChildren(t, r.Client, target), neutron)

	g.Expect(err).NotTo(HaveOccurred())
	var route gatewayv1.HTTPRoute
	g.Expect(target.Get(context.Background(), neutronKey(neutron), &route)).To(Succeed(),
		"the route belongs on the cluster the children are written to")
}

// The latch says the management cluster serves the kind, the target does not,
// and the message has to name the cluster the operator actually looked at: an
// operator restart refreshes the latch, not the target's CRDs.
func TestReconcileHTTPRoute_RemoteChildrenWithoutTheKindNameTheTargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Gateway = neutronGatewaySpec()
	r := newNeutronTestReconciler(neutron)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(),
		mctestutil.RemoteChildren(t, r.Client, hrTargetFake(false)), neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := neutronCondition(neutron, conditionTypeHTTPRouteReady)
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
	neutron := validNeutron()
	// gateway is nil: the delete path, were it reached.
	r := newNeutronTestReconciler(neutron)
	r.gatewayAPIAvailable = true
	seeded := &gatewayv1.HTTPRoute{}
	seeded.Name = testNeutronName
	seeded.Namespace = testNamespace
	target := hrTargetFake(false, seeded)

	res, err := r.reconcileHTTPRoute(context.Background(), mctestutil.RemoteChildren(t, r.Client, target), neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := neutronCondition(neutron, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))

	var route gatewayv1.HTTPRoute
	g.Expect(target.Get(context.Background(), neutronKey(neutron), &route)).To(Succeed(),
		"no delete may be attempted against a cluster that does not serve the kind")
}

// A probe that fails establishes nothing, so it is neither treated as "the
// target serves it" nor as "it does not": the pass fails and the CR says why.
func TestReconcileHTTPRoute_RemoteProbeFailureSurfacesCapabilityProbeFailed(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Gateway = neutronGatewaySpec()
	r := newNeutronTestReconciler(neutron)
	children := mctestutil.UnprobeableChildren(r.Client)

	_, err := r.reconcileHTTPRoute(context.Background(), children, neutron)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("probing the target cluster for the HTTPRoute kind:"))
	cond := neutronCondition(neutron, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.CapabilityProbeFailed))
}
