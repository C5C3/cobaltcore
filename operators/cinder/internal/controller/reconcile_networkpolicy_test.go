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
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// testShareHosts are the export URLs the backends step reports for two attached
// NFS backends: distinct servers, both speaking NFSv4 on 2049.
var testShareHosts = []string{"tcp://a.nfs.example.com:2049", "tcp://b.nfs.example.com:2049"}

// newNetworkPolicyTestReconciler builds a reconciler whose fake client can apply
// a NetworkPolicy. The fake client's default typed converter rejects one
// ("expected objects with types from the same schema"); the deduced converter
// applies it uniformly. Server-Side Apply against a real API server is exercised
// by the internal/common envtest suites.
func newNetworkPolicyTestReconciler(objs ...client.Object) *CinderReconciler {
	return &CinderReconciler{
		Client:   cinderFakeClientBuilder(objs...).WithTypeConverters(managedfields.NewDeducedTypeConverter()).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(50),
	}
}

func cinderNetworkPolicySpec() *cinderv1alpha1.NetworkPolicySpec {
	return &cinderv1alpha1.NetworkPolicySpec{
		Ingress: []cinderv1alpha1.NetworkPolicyIngressSource{{
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
	cinder := validCinder()
	stale := &networkingv1.NetworkPolicy{}
	stale.Name = testCinderName
	stale.Namespace = testNamespace
	r := newCinderTestReconciler(cinder, stale)

	res, err := r.reconcileNetworkPolicy(context.Background(), r.Client, cinder, testEgressPort, testShareHosts)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := cinderCondition(cinder, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyNotRequired))

	var gone networkingv1.NetworkPolicy
	err = r.Get(context.Background(), objectKey(testCinderName), &gone)
	g.Expect(err).To(HaveOccurred(), "stale NetworkPolicy must be deleted when disabled")
}

func TestReconcileNetworkPolicy_EnabledAppliesPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()
	r := newNetworkPolicyTestReconciler(cinder)
	r.OperatorNamespace = "cinder-system"

	res, err := r.reconcileNetworkPolicy(context.Background(), r.Client, cinder, testEgressPort, testShareHosts)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := cinderCondition(cinder, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyReady))

	var np networkingv1.NetworkPolicy
	g.Expect(r.Get(context.Background(), objectKey(testCinderName), &np)).To(Succeed())
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2),
		"reconcileNetworkPolicy must thread r.OperatorNamespace into the policy")
	// DNS, database, cache, keystone, messaging, exports.
	g.Expect(np.Spec.Egress).To(HaveLen(6),
		"reconcileNetworkPolicy must thread the bus port and the export hosts into the egress set")
}

func TestReconcileNetworkPolicy_EmptyIngressFailsClosed(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.NetworkPolicy = &cinderv1alpha1.NetworkPolicySpec{}
	r := newCinderTestReconciler(cinder)

	_, err := r.reconcileNetworkPolicy(context.Background(), r.Client, cinder, testEgressPort, testShareHosts)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to create NetworkPolicy that would allow all ingress"))
}

// The egress order is part of the contract: DNS first, then the database, the
// cache, the Keystone endpoint, the message bus, the NFS exports, and the
// operator's own additional rules last.
func TestBuildCinderNetworkPolicy_IngressAndEgressOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()

	np := buildCinderNetworkPolicy(cinder, "cinder-system", testEgressPort, testShareHosts)

	g.Expect(np.Spec.PodSelector.MatchLabels).To(Equal(selectorLabels(cinder)))
	g.Expect(np.Spec.Ingress).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(int(cinderAPIPort)))
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
	g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).
		To(HaveKeyWithValue("kubernetes.io/metadata.name", "monitoring"))
	g.Expect(np.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).
		To(HaveKeyWithValue("kubernetes.io/metadata.name", "cinder-system"))

	// DNS (53 UDP + 53 TCP), database (3306), cache (11211), keystone (5000, the
	// port of validCinder's keystoneEndpoint), bus (5672), exports (2049).
	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 11211, 5000, 5672, 2049}))
}

// Every export speaks NFSv4 on the same port, so a Cinder with any number of
// attached backends opens 2049 exactly once — and a Cinder whose backends step
// reported none opens no export rule at all, rather than an empty rule that
// would open everything.
func TestBuildCinderNetworkPolicy_ExportEgressFollowsTheAttachedBackends(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hosts     []string
		wantPorts []int
	}{
		{name: "two backends on distinct servers", hosts: testShareHosts, wantPorts: []int{53, 53, 3306, 11211, 5000, 5672, 2049}},
		{name: "one backend", hosts: testShareHosts[:1], wantPorts: []int{53, 53, 3306, 11211, 5000, 5672, 2049}},
		{name: "no backends attached yet", hosts: nil, wantPorts: []int{53, 53, 3306, 11211, 5000, 5672}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cinder := validCinder()
			cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()

			np := buildCinderNetworkPolicy(cinder, "", testEgressPort, tc.hosts)

			g.Expect(egressPorts(np)).To(Equal(tc.wantPorts))
		})
	}
}

// The bus rule is port-only and follows the transport URL: a broker on the TLS
// port opens 5671, and the waiting pass, which materialised no transport URL and
// reports port 0, opens nothing.
func TestBuildCinderNetworkPolicy_MessagingEgressFollowsTheTransportURLPort(t *testing.T) {
	for _, tc := range []struct {
		name       string
		egressPort int32
		wantPorts  []int
	}{
		{name: "amqp", egressPort: 5672, wantPorts: []int{53, 53, 3306, 11211, 5000, 5672, 2049}},
		{name: "amqps", egressPort: 5671, wantPorts: []int{53, 53, 3306, 11211, 5000, 5671, 2049}},
		{name: "no transport URL yet", egressPort: 0, wantPorts: []int{53, 53, 3306, 11211, 5000, 2049}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cinder := validCinder()
			cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()

			np := buildCinderNetworkPolicy(cinder, "", tc.egressPort, testShareHosts)

			g.Expect(egressPorts(np)).To(Equal(tc.wantPorts))
			if tc.egressPort == 0 {
				return
			}
			bus := np.Spec.Egress[len(np.Spec.Egress)-2]
			g.Expect(bus.Ports).To(HaveLen(1))
			g.Expect(bus.Ports[0].Protocol).To(HaveValue(Equal(corev1.ProtocolTCP)))
			g.Expect(bus.To).To(BeEmpty(),
				"the broker has no resolvable selector, so the rule stays port-only")
		})
	}
}

// Keystone egress is not optional for a Cinder that validates tokens:
// keystonemiddleware calls spec.keystoneEndpoint server-side on every
// authenticated request, while both readiness signals keep passing because both
// hit the unauthenticated /healthcheck. A Keystone-free Cinder validates nothing
// and must not open a port it never dials.
func TestBuildCinderNetworkPolicy_KeystoneEgress(t *testing.T) {
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
			cinder := validCinder()
			cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()
			cinder.Spec.KeystoneEndpoint = tc.endpoint

			np := buildCinderNetworkPolicy(cinder, "", testEgressPort, testShareHosts)

			g.Expect(egressPorts(np)).To(ContainElement(tc.wantPort),
				"the Keystone endpoint port must be reachable from the Cinder pods")
		})
	}

	t.Run("a Keystone-free Cinder opens no Keystone port", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := keystoneFreeCinder()
		cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()

		np := buildCinderNetworkPolicy(cinder, "", testEgressPort, testShareHosts)

		g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 11211, 5672, 2049}))
	})
}

// Glance serves the image a create-volume-from-image request reads and Barbican
// holds the keys castellan encrypts volumes with. Each endpoint is optional, and
// an unset one must contribute no port: the rules are port-only, so a spurious
// 443 would open every https destination the pods can route to.
func TestBuildCinderNetworkPolicy_ServiceEgressFollowsTheConfiguredEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name      string
		glance    string
		barbican  string
		wantPorts []int
	}{
		{name: "neither endpoint set", wantPorts: []int{53, 53, 3306, 11211, 5000, 5672, 2049}},
		{
			name:      "glance alone",
			glance:    "http://glance.openstack.svc:9292",
			wantPorts: []int{53, 53, 3306, 11211, 5000, 9292, 5672, 2049},
		},
		{
			name:      "barbican alone",
			barbican:  "https://barbican.example.com",
			wantPorts: []int{53, 53, 3306, 11211, 5000, 443, 5672, 2049},
		},
		{
			name:      "both, sorted ascending in one rule",
			glance:    "http://glance.openstack.svc:9292",
			barbican:  "http://barbican.openstack.svc:9311",
			wantPorts: []int{53, 53, 3306, 11211, 5000, 9292, 9311, 5672, 2049},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cinder := validCinder()
			cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()
			cinder.Spec.GlanceEndpoint = tc.glance
			if tc.barbican != "" {
				cinder.Spec.KeyManager = &cinderv1alpha1.KeyManagerSpec{
					Type:     cinderv1alpha1.KeyManagerTypeBarbican,
					Barbican: &cinderv1alpha1.BarbicanKeyManagerSpec{Endpoint: tc.barbican},
				}
			}

			np := buildCinderNetworkPolicy(cinder, "", testEgressPort, testShareHosts)

			g.Expect(egressPorts(np)).To(Equal(tc.wantPorts))
		})
	}
}

func TestBuildCinderNetworkPolicy_OmitsCacheWhenAbsent(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()
	cinder.Spec.Cache = commonv1.CacheSpec{}

	np := buildCinderNetworkPolicy(cinder, "", testEgressPort, testShareHosts)

	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 5000, 5672, 2049}))
}

func TestBuildCinderNetworkPolicy_AdditionalEgressAppendedLast(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	port9999 := intstr.FromInt32(9999)
	tcp := corev1.ProtocolTCP
	cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()
	cinder.Spec.NetworkPolicy.AdditionalEgress = []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port9999}},
	}}

	np := buildCinderNetworkPolicy(cinder, "", testEgressPort, testShareHosts)

	g.Expect(egressPorts(np)).To(Equal([]int{53, 53, 3306, 11211, 5000, 5672, 2049, 9999}))
}

// The pod selector matches on name and instance only, so the API pods, the
// scheduler, every volume service, the backup service and the Job pods keep
// inheriting the same auto-derived egress rules even though their component
// labels differ. Without the export egress a volume service cannot mount the
// share it serves, and without the database egress every purge run fails.
func TestBuildCinderNetworkPolicy_PodSelectorCoversEveryComponent(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()

	podSelector := buildCinderNetworkPolicy(cinder, "", testEgressPort, testShareHosts).Spec.PodSelector

	g.Expect(podSelector.MatchLabels).NotTo(HaveKey(naming.LabelKeyComponent))
	selector := labels.SelectorFromSet(podSelector.MatchLabels)
	backend := backendProjection{name: "nfs-a", server: "a.nfs.example.com", path: "/exports/a", secretName: "s"}
	backup := &backupProjection{name: "backup-a", secretName: "b"}
	covered := map[string]map[string]string{
		"API": buildCinderDeployment(cinder, workloadArtifacts(), workloadDigests{}).Spec.Template.Labels,
		"scheduler": buildSchedulerDeployment(cinder, workloadArtifacts(), workloadDigests{},
			testEgressPort).Spec.Template.Labels,
		"volume": buildVolumeDeployment(cinder, backend, workloadArtifacts(), workloadDigests{},
			testEgressPort).Spec.Template.Labels,
		"backup": buildBackupDeployment(cinder, backup, []backendProjection{backend}, workloadArtifacts(),
			workloadDigests{}, testEgressPort).Spec.Template.Labels,
		"db-purge": dbPurgeCronJob(cinder, workloadArtifacts()).Spec.JobTemplate.Spec.Template.Labels,
	}
	for component, podLabels := range covered {
		g.Expect(selector.Matches(labels.Set(podLabels))).To(BeTrue(),
			"the %s pods must stay selected by the NetworkPolicy", component)
	}
}

func TestServiceEgressURLs(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()

	g.Expect(serviceEgressURLs(cinder)).To(BeEmpty(), "neither endpoint is set on the shared fixture")

	cinder.Spec.GlanceEndpoint = "http://glance.openstack.svc:9292"
	// A key manager whose union block is absent contributes nothing rather than
	// panicking: the CEL rule pairs them, but the operator reads CRs it did not
	// admit after an upgrade.
	cinder.Spec.KeyManager = &cinderv1alpha1.KeyManagerSpec{Type: cinderv1alpha1.KeyManagerTypeBarbican}
	g.Expect(serviceEgressURLs(cinder)).To(Equal([]string{"http://glance.openstack.svc:9292"}))

	cinder.Spec.KeyManager.Barbican = &cinderv1alpha1.BarbicanKeyManagerSpec{
		Endpoint: "http://barbican.openstack.svc:9311",
	}
	g.Expect(serviceEgressURLs(cinder)).To(Equal([]string{
		"http://glance.openstack.svc:9292", "http://barbican.openstack.svc:9311",
	}))
}
