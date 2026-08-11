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

	commonv1 "github.com/c5c3/forge/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
)

// openBaoHosts is the server URL set the secret-store projection reports for the
// networkpolicy egress: one https store on the default port.
var openBaoHosts = []string{"https://openbao.openstack.svc.cluster.local:8200"}

// newNetworkPolicyTestReconciler builds a reconciler whose fake client can apply
// a NetworkPolicy. The fake client's default typed converter rejects one
// ("expected objects with types from the same schema"); the deduced converter
// applies it uniformly. Server-Side Apply against a real API server is exercised
// by the internal/common envtest suites.
func newNetworkPolicyTestReconciler(objs ...client.Object) *BarbicanReconciler {
	return &BarbicanReconciler{
		Client:   barbicanFakeClientBuilder(objs...).WithTypeConverters(managedfields.NewDeducedTypeConverter()).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(50),
	}
}

func barbicanNetworkPolicySpec() *barbicanv1alpha1.NetworkPolicySpec {
	return &barbicanv1alpha1.NetworkPolicySpec{
		Ingress: []barbicanv1alpha1.NetworkPolicyIngressSource{{
			NamespaceSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "monitoring"},
			},
		}},
	}
}

// egressPorts flattens the TCP/UDP ports of every egress rule, in rule order, so
// a test can assert the whole sequence rather than one rule at a time.
func egressPorts(np *networkingv1.NetworkPolicy) []int {
	var ports []int
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			ports = append(ports, port.Port.IntValue())
		}
	}
	return ports
}

func TestReconcileNetworkPolicy_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	stale := &networkingv1.NetworkPolicy{}
	stale.Name = testBarbicanName
	stale.Namespace = testNamespace
	r := newBarbicanTestReconciler(barbican, stale)

	res, err := r.reconcileNetworkPolicy(context.Background(), r.Client, barbican, validProjection())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := barbicanCondition(barbican, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyNotRequired))

	var gone networkingv1.NetworkPolicy
	err = r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testBarbicanName}, &gone)
	g.Expect(err).To(HaveOccurred(), "stale NetworkPolicy must be deleted when disabled")
}

func TestReconcileNetworkPolicy_EnabledAppliesPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.NetworkPolicy = barbicanNetworkPolicySpec()
	projection := validProjection()
	projection.hosts = openBaoHosts
	r := newNetworkPolicyTestReconciler(barbican)
	r.OperatorNamespace = "barbican-system"

	res, err := r.reconcileNetworkPolicy(context.Background(), r.Client, barbican, projection)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := barbicanCondition(barbican, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyReady))

	var np networkingv1.NetworkPolicy
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testBarbicanName}, &np)).To(Succeed())
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2),
		"reconcileNetworkPolicy must thread r.OperatorNamespace into the policy")
	g.Expect(np.Spec.Egress).To(HaveLen(5),
		"reconcileNetworkPolicy must thread the projection's hosts into the OpenBao egress")
}

func TestReconcileNetworkPolicy_EmptyIngressFailsClosed(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.NetworkPolicy = &barbicanv1alpha1.NetworkPolicySpec{}
	r := newBarbicanTestReconciler(barbican)

	_, err := r.reconcileNetworkPolicy(context.Background(), r.Client, barbican, validProjection())

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to create NetworkPolicy that would allow all ingress"))
}

// The egress order is part of the contract: DNS first, then the database, the
// cache, the OpenBao servers, and the operator's own additional rules last.
func TestBuildBarbicanNetworkPolicy_IngressAndEgressOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.NetworkPolicy = barbicanNetworkPolicySpec()

	np := buildBarbicanNetworkPolicy(barbican, "barbican-system", openBaoHosts)

	g.Expect(np.Spec.PodSelector.MatchLabels).To(Equal(selectorLabels(barbican)))
	g.Expect(np.Spec.Ingress).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(barbicanAPIPort))
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
	g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).
		To(HaveKeyWithValue("kubernetes.io/metadata.name", "monitoring"))
	g.Expect(np.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).
		To(HaveKeyWithValue("kubernetes.io/metadata.name", "barbican-system"))

	// DNS (53 UDP + 53 TCP), database (3306), keystone (5000, the port of
	// testBarbican's keystoneEndpoint), cache (11211), OpenBao (8200).
	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 5000, 11211, 8200}))
}

// Keystone egress is not optional: keystonemiddleware validates the token of
// every authenticated request against spec.keystoneEndpoint server-side. Without
// the rule the policy's default-deny egress drops those connections while both
// readiness signals — the kubelet probes and the operator health check — keep
// passing, because both hit the unauthenticated healthcheck app. The port is
// derived from the endpoint URL, so an https endpoint on the default port is
// covered too.
func TestBuildBarbicanNetworkPolicy_KeystoneEgress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		wantPort int
	}{
		{name: "explicit port", endpoint: "http://keystone.openstack.svc:5000/v3", wantPort: 5000},
		{name: "https default", endpoint: "https://keystone.example.com/v3", wantPort: 443},
		{name: "http default", endpoint: "http://keystone.example.com/v3", wantPort: 80},
		{name: "unparseable falls back closed", endpoint: "://nonsense", wantPort: 443},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			barbican := testBarbican()
			barbican.Spec.NetworkPolicy = barbicanNetworkPolicySpec()
			barbican.Spec.KeystoneEndpoint = tc.endpoint

			np := buildBarbicanNetworkPolicy(barbican, "", openBaoHosts)

			var ports []int
			for _, rule := range np.Spec.Egress {
				for _, port := range rule.Ports {
					if *port.Protocol == corev1.ProtocolTCP {
						ports = append(ports, port.Port.IntValue())
					}
				}
			}
			g.Expect(ports).To(ContainElement(tc.wantPort),
				"the Keystone endpoint port must be reachable from the Barbican pods")
		})
	}
}

// A Barbican whose stores are all gone (or none attached yet) carries no host,
// so no OpenBao rule is rendered at all — an egress rule with no port would open
// nothing and an empty peer list would open everything.
func TestBuildBarbicanNetworkPolicy_NoOpenBaoRuleWithoutHosts(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.NetworkPolicy = barbicanNetworkPolicySpec()

	np := buildBarbicanNetworkPolicy(barbican, "", nil)

	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 5000, 11211}))
}

func TestBuildBarbicanNetworkPolicy_OmitsCacheWhenAbsent(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.NetworkPolicy = barbicanNetworkPolicySpec()
	barbican.Spec.Cache = commonv1.CacheSpec{}

	np := buildBarbicanNetworkPolicy(barbican, "", openBaoHosts)

	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 5000, 8200}))
}

func TestBuildBarbicanNetworkPolicy_AdditionalEgressAppendedLast(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	port9999 := intstr.FromInt32(9999)
	tcp := corev1.ProtocolTCP
	barbican.Spec.NetworkPolicy = barbicanNetworkPolicySpec()
	barbican.Spec.NetworkPolicy.AdditionalEgress = []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port9999}},
	}}

	np := buildBarbicanNetworkPolicy(barbican, "", openBaoHosts)

	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 5000, 11211, 8200, 9999}))
}

// The pod selector matches on name and instance only, so the API pods and the
// db-clean pods alike keep inheriting the auto-derived egress rules even though
// their component labels differ. Without the DNS and database egress,
// barbican-manage cannot reach the database and every clean-up run fails.
func TestBuildBarbicanNetworkPolicy_PodSelectorIsWide(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.NetworkPolicy = barbicanNetworkPolicySpec()

	podSelector := buildBarbicanNetworkPolicy(barbican, "", openBaoHosts).Spec.PodSelector

	g.Expect(podSelector.MatchLabels).NotTo(HaveKey("app.kubernetes.io/component"))
	selector := labels.SelectorFromSet(podSelector.MatchLabels)
	apiPodLabels := buildBarbicanDeployment(barbican, validProjection(), deploymentConfigSecretName, "", "").Spec.Template.Labels
	g.Expect(selector.Matches(labels.Set(apiPodLabels))).To(BeTrue(),
		"API pods must stay selected by the NetworkPolicy")
	cleanPodLabels := dbCleanCronJob(barbican, deploymentConfigSecretName).Spec.JobTemplate.Spec.Template.Labels
	g.Expect(selector.Matches(labels.Set(cleanPodLabels))).To(BeTrue(),
		"db-clean pods must stay selected by the NetworkPolicy")
}
