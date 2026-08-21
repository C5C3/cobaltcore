// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// newNetworkPolicyTestReconciler builds a reconciler whose fake client can apply
// a NetworkPolicy. The fake client's default typed converter rejects one
// ("expected objects with types from the same schema"); the deduced converter
// applies it uniformly. Server-Side Apply against a real API server is exercised
// by the internal/common envtest suites.
func newNetworkPolicyTestReconciler(objs ...client.Object) *PlacementReconciler {
	return &PlacementReconciler{
		Client:   placementFakeClientBuilder(objs...).WithTypeConverters(managedfields.NewDeducedTypeConverter()).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(50),
	}
}

func placementNetworkPolicySpec() *placementv1alpha1.NetworkPolicySpec {
	return &placementv1alpha1.NetworkPolicySpec{
		Ingress: []placementv1alpha1.NetworkPolicyIngressSource{{
			NamespaceSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "monitoring"},
			},
		}},
	}
}

func TestReconcileNetworkPolicy_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	stale := &networkingv1.NetworkPolicy{}
	stale.Name = "test-placement"
	stale.Namespace = "default"
	r := newPlacementTestReconciler(placement, stale)

	res, err := r.reconcileNetworkPolicy(context.Background(), r.Client, placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(placement.Status.Conditions, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyNotRequired))

	var gone networkingv1.NetworkPolicy
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-placement"}, &gone)
	g.Expect(err).To(HaveOccurred(), "stale NetworkPolicy must be deleted when disabled")
}

func TestReconcileNetworkPolicy_EnabledAppliesPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.NetworkPolicy = placementNetworkPolicySpec()
	r := newNetworkPolicyTestReconciler(placement)
	r.OperatorNamespace = "placement-system"

	res, err := r.reconcileNetworkPolicy(context.Background(), r.Client, placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(placement.Status.Conditions, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyReady))

	var np networkingv1.NetworkPolicy
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-placement"}, &np)).To(Succeed())
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2),
		"reconcileNetworkPolicy must thread r.OperatorNamespace into the policy")
	g.Expect(np.Spec.Egress).To(HaveLen(4))
}

func TestReconcileNetworkPolicy_EmptyIngressFailsClosed(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.NetworkPolicy = &placementv1alpha1.NetworkPolicySpec{}
	r := newPlacementTestReconciler(placement)

	_, err := r.reconcileNetworkPolicy(context.Background(), r.Client, placement)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to create NetworkPolicy that would allow all ingress"))
}

func TestBuildPlacementNetworkPolicy_IngressAndEgressRules(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.NetworkPolicy = placementNetworkPolicySpec()

	np := buildPlacementNetworkPolicy(placement, "placement-system")

	g.Expect(np.Spec.PodSelector.MatchLabels).To(Equal(selectorLabels(placement)))
	// Ingress restricted to the API port from the user source + operator peer.
	g.Expect(np.Spec.Ingress).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(8778))
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
	g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).
		To(HaveKeyWithValue("kubernetes.io/metadata.name", "monitoring"))
	g.Expect(np.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).
		To(HaveKeyWithValue("kubernetes.io/metadata.name", "placement-system"))

	// Egress: DNS (53), database (3306), keystone (5000, the port of
	// testPlacement's keystoneEndpoint), cache (11211). Placement talks to nothing
	// else, so no object-store egress is rendered.
	g.Expect(np.Spec.Egress).To(HaveLen(4))
	g.Expect(np.Spec.Egress[0].Ports[0].Port.IntValue()).To(Equal(53))
	g.Expect(np.Spec.Egress[1].Ports[0].Port.IntValue()).To(Equal(3306))
	g.Expect(np.Spec.Egress[2].Ports[0].Port.IntValue()).To(Equal(5000))
	g.Expect(np.Spec.Egress[3].Ports[0].Port.IntValue()).To(Equal(11211))
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			g.Expect(port.Port.IntValue()).NotTo(Or(Equal(443), Equal(80)),
				"placement needs no object-store egress")
		}
	}
}

// Keystone egress is not optional: keystonemiddleware validates the token of
// every authenticated request against spec.keystoneEndpoint server-side. Without
// the rule the policy's default-deny egress drops those connections while both
// readiness signals — the kubelet probes and the operator health check — keep
// passing, because both hit the unauthenticated version document. The port is
// derived from the endpoint URL, so an https endpoint on the default port is
// covered too.
func TestBuildPlacementNetworkPolicy_KeystoneEgress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		wantPort int
	}{
		{name: "explicit port", endpoint: "http://keystone.default.svc:5000/v3", wantPort: 5000},
		{name: "https default", endpoint: "https://keystone.example.com/v3", wantPort: 443},
		{name: "http default", endpoint: "http://keystone.example.com/v3", wantPort: 80},
		{name: "unparseable falls back closed", endpoint: "://nonsense", wantPort: 443},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			placement := testPlacement()
			placement.Spec.NetworkPolicy = placementNetworkPolicySpec()
			placement.Spec.KeystoneEndpoint = tc.endpoint

			np := buildPlacementNetworkPolicy(placement, "")

			var ports []int
			for _, rule := range np.Spec.Egress {
				for _, port := range rule.Ports {
					if *port.Protocol == corev1.ProtocolTCP {
						ports = append(ports, port.Port.IntValue())
					}
				}
			}
			g.Expect(ports).To(ContainElement(tc.wantPort),
				"the Keystone endpoint port must be reachable from the Placement pods")
		})
	}
}

// The pod selector matches on name and instance only, so the API pods and the
// db-sync pods alike keep inheriting the auto-derived egress rules even though
// their component labels differ. Without the DNS and database egress,
// placement-manage cannot reach the database and every migration fails.
func TestBuildPlacementNetworkPolicy_PodSelectorIsWide(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.NetworkPolicy = placementNetworkPolicySpec()

	podSelector := buildPlacementNetworkPolicy(placement, "").Spec.PodSelector

	g.Expect(podSelector.MatchLabels).NotTo(HaveKey("app.kubernetes.io/component"))
	selector := labels.SelectorFromSet(podSelector.MatchLabels)
	apiPodLabels := buildPlacementDeployment(placement, "test-placement-config", "", "").Spec.Template.Labels
	g.Expect(selector.Matches(labels.Set(apiPodLabels))).To(BeTrue(),
		"API pods must stay selected by the NetworkPolicy")
}

func TestBuildPlacementNetworkPolicy_OmitsCacheWhenAbsent(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.NetworkPolicy = placementNetworkPolicySpec()
	placement.Spec.Cache = commonv1.CacheSpec{}

	np := buildPlacementNetworkPolicy(placement, "")

	// Only DNS, database, and keystone egress remain.
	g.Expect(np.Spec.Egress).To(HaveLen(3))
	g.Expect(np.Spec.Egress[0].Ports[0].Port.IntValue()).To(Equal(53))
	g.Expect(np.Spec.Egress[1].Ports[0].Port.IntValue()).To(Equal(3306))
	g.Expect(np.Spec.Egress[2].Ports[0].Port.IntValue()).To(Equal(5000))
}

func TestBuildPlacementNetworkPolicy_AdditionalEgressAppended(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	np9999 := intstr.FromInt32(9999)
	tcp := corev1.ProtocolTCP
	placement.Spec.NetworkPolicy = placementNetworkPolicySpec()
	placement.Spec.NetworkPolicy.AdditionalEgress = []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &np9999}},
	}}

	np := buildPlacementNetworkPolicy(placement, "")

	// Auto rules (DNS, DB, keystone, cache) then the user-supplied rule last.
	last := np.Spec.Egress[len(np.Spec.Egress)-1]
	g.Expect(last.Ports[0].Port.IntValue()).To(Equal(9999))
}
