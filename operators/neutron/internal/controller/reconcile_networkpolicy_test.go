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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// testBusPort is the TLS AMQP port the messaging step reports for the shared
// RabbitMQ fixture, and the port the policy has to open for the RPC bus.
const testBusPort int32 = 5671

// resolvedOVN is the endpoint pair the OVN step resolves for the shared
// OVNCentral fixture: Northbound on 6641, Southbound on 6642.
func resolvedOVN() resolvedOVNEndpoints {
	return resolvedOVNEndpoints{nbAddress: testNorthboundAddress, sbAddress: testSouthboundAddress}
}

// newNetworkPolicyTestReconciler builds a reconciler whose fake client can apply
// a NetworkPolicy. The fake client's default typed converter rejects one
// ("expected objects with types from the same schema"); the deduced converter
// applies it uniformly. Server-Side Apply against a real API server is exercised
// by the internal/common envtest suites.
func newNetworkPolicyTestReconciler(objs ...client.Object) *NeutronReconciler {
	return &NeutronReconciler{
		Client:   neutronFakeClientBuilder(objs...).WithTypeConverters(managedfields.NewDeducedTypeConverter()).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(50),
	}
}

func neutronNetworkPolicySpec() *neutronv1alpha1.NetworkPolicySpec {
	return &neutronv1alpha1.NetworkPolicySpec{
		Ingress: []neutronv1alpha1.NetworkPolicyIngressSource{{
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
	neutron := validNeutron()
	stale := &networkingv1.NetworkPolicy{}
	stale.Name = testNeutronName
	stale.Namespace = testNamespace
	r := newNeutronTestReconciler(neutron, stale)

	res, err := r.reconcileNetworkPolicy(context.Background(), r.Client, neutron, resolvedOVN(), testBusPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := neutronCondition(neutron, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyNotRequired))

	var gone networkingv1.NetworkPolicy
	err = r.Get(context.Background(), neutronKey(neutron), &gone)
	g.Expect(err).To(HaveOccurred(), "stale NetworkPolicy must be deleted when disabled")
}

func TestReconcileNetworkPolicy_EnabledAppliesPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.NetworkPolicy = neutronNetworkPolicySpec()
	r := newNetworkPolicyTestReconciler(neutron)
	r.OperatorNamespace = "neutron-system"

	res, err := r.reconcileNetworkPolicy(context.Background(), r.Client, neutron, resolvedOVN(), testBusPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := neutronCondition(neutron, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyReady))

	var np networkingv1.NetworkPolicy
	g.Expect(r.Get(context.Background(), neutronKey(neutron), &np)).To(Succeed())
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2),
		"reconcileNetworkPolicy must thread r.OperatorNamespace into the policy")
	// DNS, database, keystone, cache, OVN, messaging.
	g.Expect(np.Spec.Egress).To(HaveLen(6),
		"reconcileNetworkPolicy must thread the OVN endpoints and the bus port into the egress set")
}

func TestReconcileNetworkPolicy_EmptyIngressFailsClosed(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.NetworkPolicy = &neutronv1alpha1.NetworkPolicySpec{}
	r := newNeutronTestReconciler(neutron)

	_, err := r.reconcileNetworkPolicy(context.Background(), r.Client, neutron, resolvedOVN(), testBusPort)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to create NetworkPolicy that would allow all ingress"))
}

// The egress order is part of the contract: DNS first, then the database, the
// keystone endpoint, the cache, the OVN databases, the message bus, and the
// operator's own additional rules last.
func TestBuildNeutronNetworkPolicy_IngressAndEgressOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.NetworkPolicy = neutronNetworkPolicySpec()

	np := buildNeutronNetworkPolicy(neutron, "neutron-system", resolvedOVN(), testBusPort)

	g.Expect(np.Spec.PodSelector.MatchLabels).To(Equal(naming.SelectorLabels(neutronAppName, neutron.Name)))
	g.Expect(np.Spec.Ingress).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(int(neutronAPIPort)))
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
	g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).
		To(HaveKeyWithValue("kubernetes.io/metadata.name", "monitoring"))
	g.Expect(np.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).
		To(HaveKeyWithValue("kubernetes.io/metadata.name", "neutron-system"))

	// DNS (53 UDP + 53 TCP), database (3306), keystone (5000, the port of
	// validNeutron's keystoneEndpoint), cache (11211), OVN (6641 + 6642), bus (5671).
	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 5000, 11211, 6641, 6642, 5671}))
}

// The OVN egress holds one TCP port per distinct member port of the two
// addresses. Both are comma-separated lists once the databases are published on
// node ports, and a member repeated on a port another member already opened adds
// nothing.
func TestBuildNeutronNetworkPolicy_OVNEgressDeduplicatesMemberPorts(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.NetworkPolicy = neutronNetworkPolicySpec()
	ovn := resolvedOVNEndpoints{
		nbAddress: "ssl:10.0.0.1:30641,ssl:10.0.0.2:30642",
		sbAddress: "ssl:10.0.0.1:30642,ssl:10.0.0.2:30642",
	}

	np := buildNeutronNetworkPolicy(neutron, "", ovn, testBusPort)

	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 5000, 11211, 30641, 30642, 5671}))
}

// A Neutron whose OVNCentral has published nothing yet carries no address, so no
// OVN rule is rendered at all: an egress rule with no port would open nothing
// and an empty peer list would open everything.
func TestBuildNeutronNetworkPolicy_NoOVNRuleWithoutAddresses(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.NetworkPolicy = neutronNetworkPolicySpec()

	np := buildNeutronNetworkPolicy(neutron, "", resolvedOVNEndpoints{}, testBusPort)

	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 5000, 11211, 5671}))
}

// The bus rule is port-only and follows the transport URL: a broker on the
// plaintext port opens 5672, and the waiting pass, which materialised no
// transport URL and reports port 0, opens nothing.
func TestBuildNeutronNetworkPolicy_MessagingEgressFollowsTheTransportURLPort(t *testing.T) {
	for _, tc := range []struct {
		name       string
		egressPort int32
		wantPorts  []int
	}{
		{name: "amqps", egressPort: 5671, wantPorts: []int{53, 53, 3306, 5000, 11211, 6641, 6642, 5671}},
		{name: "amqp", egressPort: 5672, wantPorts: []int{53, 53, 3306, 5000, 11211, 6641, 6642, 5672}},
		{name: "no transport URL yet", egressPort: 0, wantPorts: []int{53, 53, 3306, 5000, 11211, 6641, 6642}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			neutron := validNeutron()
			neutron.Spec.NetworkPolicy = neutronNetworkPolicySpec()

			np := buildNeutronNetworkPolicy(neutron, "", resolvedOVN(), tc.egressPort)

			g.Expect(egressPorts(np)).To(Equal(tc.wantPorts))
			if tc.egressPort == 0 {
				return
			}
			bus := np.Spec.Egress[len(np.Spec.Egress)-1]
			g.Expect(bus.Ports).To(HaveLen(1))
			g.Expect(bus.Ports[0].Protocol).To(HaveValue(Equal(corev1.ProtocolTCP)))
			g.Expect(bus.To).To(BeEmpty(),
				"the broker has no resolvable selector, so the rule stays port-only")
		})
	}
}

// Keystone egress is not optional: keystonemiddleware validates the token of
// every authenticated request against spec.keystoneEndpoint server-side. Without
// the rule the policy's default-deny egress drops those connections while both
// readiness signals, the kubelet probes and the operator health check, keep
// passing, because both hit the unauthenticated version document. The port is
// derived from the endpoint URL, so an https endpoint on the default port is
// covered too.
func TestBuildNeutronNetworkPolicy_KeystoneEgress(t *testing.T) {
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
			neutron := validNeutron()
			neutron.Spec.NetworkPolicy = neutronNetworkPolicySpec()
			neutron.Spec.KeystoneEndpoint = tc.endpoint

			np := buildNeutronNetworkPolicy(neutron, "", resolvedOVN(), testBusPort)

			var ports []int
			for _, rule := range np.Spec.Egress {
				for _, port := range rule.Ports {
					if *port.Protocol == corev1.ProtocolTCP {
						ports = append(ports, port.Port.IntValue())
					}
				}
			}
			g.Expect(ports).To(ContainElement(tc.wantPort),
				"the Keystone endpoint port must be reachable from the Neutron pods")
		})
	}
}

func TestBuildNeutronNetworkPolicy_OmitsCacheWhenAbsent(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.NetworkPolicy = neutronNetworkPolicySpec()
	neutron.Spec.Cache = commonv1.CacheSpec{}

	np := buildNeutronNetworkPolicy(neutron, "", resolvedOVN(), testBusPort)

	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 5000, 6641, 6642, 5671}))
}

func TestBuildNeutronNetworkPolicy_AdditionalEgressAppendedLast(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	port9999 := intstr.FromInt32(9999)
	tcp := corev1.ProtocolTCP
	neutron.Spec.NetworkPolicy = neutronNetworkPolicySpec()
	neutron.Spec.NetworkPolicy.AdditionalEgress = []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port9999}},
	}}

	np := buildNeutronNetworkPolicy(neutron, "", resolvedOVN(), testBusPort)

	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 5000, 11211, 6641, 6642, 5671, 9999}))
}

// The pod selector matches on name and instance only, so the API pods, both
// worker Deployments and the ovn-db-sync Job pods keep inheriting the same
// auto-derived egress rules even though their component labels differ. Without
// the DNS, database and OVN egress the workers cannot reach what they maintain
// and every ovn-db-sync run fails.
func TestBuildNeutronNetworkPolicy_PodSelectorCoversEveryComponent(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.NetworkPolicy = neutronNetworkPolicySpec()

	podSelector := buildNeutronNetworkPolicy(neutron, "", resolvedOVN(), testBusPort).Spec.PodSelector

	g.Expect(podSelector.MatchLabels).NotTo(HaveKey(naming.LabelKeyComponent))
	selector := labels.SelectorFromSet(podSelector.MatchLabels)
	covered := map[string]map[string]string{
		"API": buildNeutronDeployment(neutron, deploymentConfigMapName, "", "", "", "").Spec.Template.Labels,
		"periodic workers": buildWorkerDeployment(neutron, componentPeriodicWorkers, nil,
			deploymentConfigMapName, "", "", "", "").Spec.Template.Labels,
		"OVN maintenance worker": buildWorkerDeployment(neutron, componentOVNMaintenanceWorker, nil,
			deploymentConfigMapName, "", "", "", "").Spec.Template.Labels,
		"ovn-db-sync": buildOVNDBSyncCronJob(neutron, deploymentConfigMapName).
			Spec.JobTemplate.Spec.Template.Labels,
	}
	for component, podLabels := range covered {
		g.Expect(selector.Matches(labels.Set(podLabels))).To(BeTrue(),
			"the %s pods must stay selected by the NetworkPolicy", component)
	}
}

func TestOVNEgressURLs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nb, sb string
		want   []string
	}{
		{name: "no addresses published yet"},
		{
			name: "one member per address",
			nb:   "ssl:10.96.0.11:6641",
			sb:   "ssl:10.96.0.21:6642",
			want: []string{"tcp://10.96.0.11:6641", "tcp://10.96.0.21:6642"},
		},
		{
			name: "comma-separated node-port lists",
			nb:   "ssl:10.0.0.1:30641,ssl:10.0.0.2:30641",
			sb:   "ssl:10.0.0.1:30642,ssl:10.0.0.2:30642",
			want: []string{
				"tcp://10.0.0.1:30641", "tcp://10.0.0.2:30641",
				"tcp://10.0.0.1:30642", "tcp://10.0.0.2:30642",
			},
		},
		{
			name: "a tcp member is rewritten like an ssl one",
			nb:   "tcp:10.96.0.11:6641",
			want: []string{"tcp://10.96.0.11:6641"},
		},
		{
			name: "whitespace around a member is trimmed",
			nb:   " ssl:10.96.0.11:6641 , ssl:10.96.0.12:6641 ",
			want: []string{"tcp://10.96.0.11:6641", "tcp://10.96.0.12:6641"},
		},
		{
			name: "an IPv6 member keeps its brackets",
			nb:   "ssl:[fd00::11]:6641",
			want: []string{"tcp://[fd00::11]:6641"},
		},
		{
			// Neither an empty member nor a protocol the ovsdb connection syntax does
			// not use is turned into a URL; the rule builder never sees them.
			name: "members without a usable protocol are dropped",
			nb:   "ssl:10.96.0.11:6641,,unix:/var/run/ovn/ovnnb_db.sock",
			want: []string{"tcp://10.96.0.11:6641"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(ovnEgressURLs(tc.nb, tc.sb)).To(Equal(tc.want))
		})
	}
}
