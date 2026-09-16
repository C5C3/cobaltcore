// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/naming"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// newNetworkPolicyTestReconciler builds a reconciler whose fake client can apply
// a NetworkPolicy. The fake client's default typed converter rejects one
// ("expected objects with types from the same schema"); the deduced converter
// applies it uniformly. Server-Side Apply against a real API server is exercised
// by the internal/common envtest suites.
func newNetworkPolicyTestReconciler(objs ...client.Object) *NovaReconciler {
	return &NovaReconciler{
		Client:   novaFakeClientBuilder(objs...).WithTypeConverters(managedfields.NewDeducedTypeConverter()).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(50),
	}
}

func novaNetworkPolicySpec() *novav1alpha1.NetworkPolicySpec {
	return &novav1alpha1.NetworkPolicySpec{
		Ingress: []novav1alpha1.NetworkPolicyIngressSource{{
			NamespaceSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "monitoring"},
			},
		}},
	}
}

// hardenedNova returns the shared fixture with network isolation switched on.
func hardenedNova() *novav1alpha1.Nova {
	nova := validNova()
	nova.Spec.NetworkPolicy = novaNetworkPolicySpec()
	return nova
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

// namespacePeerNames returns the namespace each peer selects by the well-known
// metadata.name label, in peer order.
func namespacePeerNames(peers []networkingv1.NetworkPolicyPeer) []string {
	var names []string
	for _, peer := range peers {
		names = append(names, peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	}
	return names
}

func TestReconcileNetworkPolicy_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	stale := &networkingv1.NetworkPolicy{}
	stale.Name = testNovaName
	stale.Namespace = testNamespace
	r := newNovaTestReconciler(nova, stale)

	res, err := r.reconcileNetworkPolicy(context.Background(), r.Client, nova, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := novaCondition(nova, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyNotRequired))

	var gone networkingv1.NetworkPolicy
	g.Expect(r.Get(context.Background(), objectKey(testNovaName), &gone)).NotTo(Succeed(),
		"the stale NetworkPolicy must be deleted when isolation is disabled")
}

func TestReconcileNetworkPolicy_EnabledAppliesPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := hardenedNova()
	r := newNetworkPolicyTestReconciler(nova)
	r.OperatorNamespace = "nova-system"

	res, err := r.reconcileNetworkPolicy(context.Background(), r.Client, nova, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := novaCondition(nova, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyReady))

	var np networkingv1.NetworkPolicy
	g.Expect(r.Get(context.Background(), objectKey(testNovaName), &np)).To(Succeed())
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(3),
		"reconcileNetworkPolicy must thread r.OperatorNamespace into the policy")
	// DNS, the two schemas, the cache, Keystone, the siblings, the bus.
	g.Expect(np.Spec.Egress).To(HaveLen(7),
		"reconcileNetworkPolicy must thread the bus port into the egress set")

	// The console proxy dials the hypervisors' VNC servers, which nothing in the
	// main egress set opens. Its own policy selects the proxy pods alone and adds
	// the display range to nothing else.
	var console networkingv1.NetworkPolicy
	g.Expect(r.Get(context.Background(), objectKey(consoleProxyName(nova)), &console)).To(Succeed())
	g.Expect(console.Spec.PodSelector.MatchLabels).To(Equal(componentSelectorLabels(nova, componentConsoleProxy)))
	g.Expect(console.Spec.PolicyTypes).To(Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeEgress}),
		"the proxy's ingress stays governed by the main policy")
	g.Expect(console.Spec.Egress).To(HaveLen(1))
	g.Expect(console.Spec.Egress[0].Ports).To(HaveLen(1))
	port := console.Spec.Egress[0].Ports[0]
	g.Expect(port.Protocol).To(HaveValue(Equal(corev1.ProtocolTCP)))
	g.Expect(port.Port.IntValue()).To(Equal(5900))
	g.Expect(port.EndPort).To(HaveValue(Equal(int32(65535))))
}

// The console egress policy is there exactly while the main policy isolates a
// projected proxy: switching either off removes it, so a disabled proxy or a
// Nova without isolation keeps no stale opening.
func TestReconcileNetworkPolicy_ConsoleProxyPolicyFollowsItsPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name string
		nova func() *novav1alpha1.Nova
	}{
		{name: "isolation off", nova: validNova},
		{name: "console proxy disabled", nova: func() *novav1alpha1.Nova {
			nova := disabledProxyNova()
			nova.Spec.NetworkPolicy = novaNetworkPolicySpec()
			return nova
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := tc.nova()
			stale := &networkingv1.NetworkPolicy{}
			stale.Name = consoleProxyName(nova)
			stale.Namespace = testNamespace
			r := newNetworkPolicyTestReconciler(nova, stale)

			_, err := r.reconcileNetworkPolicy(context.Background(), r.Client, nova, testEgressPort)

			g.Expect(err).NotTo(HaveOccurred())
			var gone networkingv1.NetworkPolicy
			g.Expect(r.Get(context.Background(), objectKey(consoleProxyName(nova)), &gone)).NotTo(Succeed())
		})
	}
}

func TestReconcileNetworkPolicy_EmptyIngressFailsClosed(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.NetworkPolicy = &novav1alpha1.NetworkPolicySpec{}
	r := newNovaTestReconciler(nova)

	_, err := r.reconcileNetworkPolicy(context.Background(), r.Client, nova, testEgressPort)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to create NetworkPolicy that would allow all ingress"))
}

// One rule carries all three front-end ports, and its peers are the configured
// sources, then every namespace a Gateway fronts one of them from, then the
// operator's own. The gateway namespaces are deduplicated: one Gateway commonly
// carries all three routes.
func TestBuildNovaNetworkPolicy_IngressPortsAndPeers(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := hardenedNova()
	nova.Spec.Metadata.Gateway = novaGatewaySpec("metadata.example.com")

	np := buildNovaNetworkPolicy(nova, "nova-system", testEgressPort)

	g.Expect(np.Spec.Ingress).To(HaveLen(1))
	var ports []int
	for _, port := range np.Spec.Ingress[0].Ports {
		g.Expect(port.Protocol).To(HaveValue(Equal(corev1.ProtocolTCP)))
		ports = append(ports, port.Port.IntValue())
	}
	g.Expect(ports).To(Equal([]int{
		int(novaAPIPort), int(novaMetadataPort), int(novaConsolePort),
	}))
	g.Expect(namespacePeerNames(np.Spec.Ingress[0].From)).To(Equal([]string{
		"monitoring", "gateway-system", "nova-system",
	}), "the three gateway blocks share one Gateway namespace, which is peered once")

	// Three Gateways in three namespaces contribute three peers, in spec order.
	nova.Spec.Metadata.Gateway.ParentRef.Namespace = "metadata-gw"
	nova.Spec.ConsoleProxy.Gateway.ParentRef.Namespace = "console-gw"
	np = buildNovaNetworkPolicy(nova, "nova-system", testEgressPort)
	g.Expect(namespacePeerNames(np.Spec.Ingress[0].From)).To(Equal([]string{
		"monitoring", "gateway-system", "metadata-gw", "console-gw", "nova-system",
	}))

	// An empty parentRef.namespace means the Gateway lives beside the Nova.
	nova.Spec.Gateway.ParentRef.Namespace = ""
	nova.Spec.Metadata.Gateway = nil
	nova.Spec.ConsoleProxy.Gateway = nil
	np = buildNovaNetworkPolicy(nova, "", testEgressPort)
	g.Expect(namespacePeerNames(np.Spec.Ingress[0].From)).To(Equal([]string{
		"monitoring", testNamespace,
	}), "an operator whose own namespace is unknown adds no peer for it")

	// A disabled console proxy has no route, so its gateway block opens no
	// namespace, the way the console route step ignores it.
	disabled := disabledProxyNova()
	disabled.Spec.NetworkPolicy = novaNetworkPolicySpec()
	disabled.Spec.ConsoleProxy.Gateway = novaGatewaySpec("console.example.com")
	disabled.Spec.ConsoleProxy.Gateway.ParentRef.Namespace = "console-gw"
	np = buildNovaNetworkPolicy(disabled, "", testEgressPort)
	g.Expect(namespacePeerNames(np.Spec.Ingress[0].From)).NotTo(ContainElement("console-gw"))
}

// The egress order is part of the contract: DNS first, then the nova_api schema,
// the cell schema, the cache, Keystone, the siblings Nova calls as a client, the
// message bus, and the deployer's own additional rules last. The two schemas sit
// on different ports here because each may live on a MariaDB of its own: a
// policy that opened one schema's port for both would block every connection to
// the other.
func TestBuildNovaNetworkPolicy_EgressOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := hardenedNova()
	nova.Spec.APIDatabase.Port = 3307

	np := buildNovaNetworkPolicy(nova, "nova-system", testEgressPort)

	g.Expect(np.Spec.PodSelector.MatchLabels).To(Equal(naming.SelectorLabels(novaAppName, nova.Name)))
	// DNS (53 UDP + 53 TCP), nova_api (3307), the cell schema (3306), the cache
	// (11211), Keystone (5000, the port of validNova's keystoneEndpoint), the
	// five siblings sorted ascending (cinder 8776, placement 8778, glance 9292,
	// barbican 9311, neutron 9696), the bus (5672).
	g.Expect(egressPorts(np)).To(Equal([]int{
		53, 53, 3307, 3306, 11211, 5000, 8776, 8778, 9292, 9311, 9696, 5672,
	}))

	// An override replaces the conventional port of its own sibling alone.
	nova.Spec.Endpoints.Placement.Override = "https://placement.example.com:8443"
	np = buildNovaNetworkPolicy(nova, "nova-system", testEgressPort)
	g.Expect(egressPorts(np)).To(Equal([]int{
		53, 53, 3307, 3306, 11211, 5000, 8443, 8776, 9292, 9311, 9696, 5672,
	}))

	// The deployer's own rules are appended after every auto-derived one. This is
	// how the console proxy is given its path to the hypervisors' VNC ports.
	vncPort := intstr.FromInt32(5900)
	tcp := corev1.ProtocolTCP
	nova.Spec.NetworkPolicy.AdditionalEgress = []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &vncPort}},
	}}
	np = buildNovaNetworkPolicy(nova, "nova-system", testEgressPort)
	g.Expect(egressPorts(np)).To(Equal([]int{
		53, 53, 3307, 3306, 11211, 5000, 8443, 8776, 9292, 9311, 9696, 5672, 5900,
	}))
}

// A minimal Nova runs without Cinder and Barbican, and a pass that has not
// materialised a transport URL yet reports port 0, which opens no bus rule
// rather than an empty one that would open everything.
func TestBuildNovaNetworkPolicy_OptionalSiblingsAndBus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nova       func() *novav1alpha1.Nova
		egressPort int32
		wantPorts  []int
	}{
		{
			name:       "no optional siblings",
			nova:       novaMinimal,
			egressPort: testEgressPort,
			wantPorts:  []int{53, 53, 3306, 3306, 11211, 5000, 8778, 9292, 9696, 5672},
		},
		{
			name:       "no transport URL yet",
			nova:       novaMinimal,
			egressPort: 0,
			wantPorts:  []int{53, 53, 3306, 3306, 11211, 5000, 8778, 9292, 9696},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := tc.nova()
			nova.Spec.NetworkPolicy = novaNetworkPolicySpec()

			np := buildNovaNetworkPolicy(nova, "", tc.egressPort)

			g.Expect(egressPorts(np)).To(Equal(tc.wantPorts))
		})
	}
}

// The pod selector matches on name and instance only, so the API pods, the
// metadata API, the scheduler, the conductor and the console proxy keep
// inheriting the same auto-derived egress rules even though their component
// labels differ. Without the database egress a conductor cannot read the cell it
// serves, and without the bus egress nothing reaches the compute nodes.
func TestBuildNovaNetworkPolicy_PodSelectorCoversEveryComponent(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := hardenedNova()
	art := workloadArtifacts()

	podSelector := buildNovaNetworkPolicy(nova, "", testEgressPort).Spec.PodSelector

	g.Expect(podSelector.MatchLabels).NotTo(HaveKey(naming.LabelKeyComponent))
	selector := labels.SelectorFromSet(podSelector.MatchLabels)
	covered := map[string]*appsv1.Deployment{
		"API":           buildAPIDeployment(nova, art, workloadDigests{}),
		"metadata":      buildMetadataDeployment(nova, art, workloadDigests{}),
		"scheduler":     buildSchedulerDeployment(nova, art, workloadDigests{}, testEgressPort),
		"conductor":     buildConductorDeployment(nova, art, workloadDigests{}, testEgressPort),
		"console proxy": buildConsoleProxyDeployment(nova, art, workloadDigests{}),
	}
	for component, deploy := range covered {
		g.Expect(selector.Matches(labels.Set(deploy.Spec.Template.Labels))).To(BeTrue(),
			"the %s pods must stay selected by the NetworkPolicy", component)
	}
}

// Every sibling contributes an address: the pinned override when the block
// carries one, and the conventional URL otherwise, because an override-free Nova
// resolves the address from the Keystone catalog, which the operator cannot
// read.
func TestSiblingEgressURLs(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := novaMinimal()

	g.Expect(siblingEgressURLs(nova)).To(Equal([]string{
		"http://placement:8778", "http://neutron:9696", "http://glance:9292",
	}), "a Nova without the optional blocks calls three siblings")

	nova.Spec.Endpoints.Cinder = novav1alpha1.NovaOptionalEndpointSpec{Enabled: true}
	nova.Spec.Endpoints.Barbican = novav1alpha1.NovaOptionalEndpointSpec{
		Enabled: true, Override: "https://barbican.example.com",
	}
	nova.Spec.Endpoints.Glance.Override = "http://glance.openstack.svc:9293"
	g.Expect(siblingEgressURLs(nova)).To(Equal([]string{
		"http://placement:8778", "http://neutron:9696", "http://glance.openstack.svc:9293",
		"http://cinder:8776", "https://barbican.example.com",
	}))

	// An override on a disabled block is not read: the section it addresses is
	// not rendered either.
	nova.Spec.Endpoints.Cinder = novav1alpha1.NovaOptionalEndpointSpec{
		Override: "http://cinder.openstack.svc:8776",
	}
	g.Expect(siblingEgressURLs(nova)).NotTo(ContainElement("http://cinder.openstack.svc:8776"))
}
