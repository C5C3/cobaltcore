// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the NeutronMetadataAgent watch mappers.
package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// agentFor builds a NeutronMetadataAgent running alongside the named OVNChassis.
func agentFor(name, namespace, chassis string) *neutronv1alpha1.NeutronMetadataAgent {
	cr := validAgent()
	cr.Name = name
	cr.Namespace = namespace
	cr.Spec.ChassisRef = neutronv1alpha1.OVNChassisRef{Name: chassis}
	return cr
}

// agentReq is the reconcile request for an agent addressed by name.
func agentReq(namespace, name string) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKey{Namespace: namespace, Name: name}}
}

// brokenListClient is the fake client every mapper's List fails on, so the
// log-and-map-to-nothing contract of handler.MapFunc can be asserted.
func brokenListClient() client.Client {
	return neutronFakeClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewServiceUnavailable("cache not started")
			},
		}).Build()
}

// The Secret mapper resolves both referenced names through the index, and maps a
// Secret nobody references to nothing rather than to every agent beside it.
func TestSecretToAgentMapper(t *testing.T) {
	withNova := agentFor("nova-signer", testNamespace, testOVNChassisName)
	withNova.Spec.NovaMetadata = &neutronv1alpha1.NovaMetadataSpec{
		SharedSecretRef: &commonv1.SecretRefSpec{Name: "nova-metadata-secret", Key: "shared_secret"},
	}
	withBus := agentFor("bus-reader", testNamespace, testOVNChassisName)
	withBus.Spec.Messaging = &commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "external-bus", Key: commonv1.DefaultTransportURLSecretKey},
	}

	tests := []struct {
		name   string
		secret string
		want   []reconcile.Request
	}{
		{
			name:   "the shared-secret reference",
			secret: "nova-metadata-secret",
			want:   []reconcile.Request{agentReq(testNamespace, "nova-signer")},
		},
		{
			name:   "the brownfield transport-URL reference",
			secret: "external-bus",
			want:   []reconcile.Request{agentReq(testNamespace, "bus-reader")},
		},
		{
			name:   "a Secret nobody references",
			secret: "unrelated",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c := neutronFakeClientBuilder(withNova, withBus).Build()
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:      tc.secret,
				Namespace: testNamespace,
			}}

			requests := secretToAgentMapper(c)(context.Background(), secret)

			if len(tc.want) == 0 {
				g.Expect(requests).To(BeEmpty())
				return
			}
			g.Expect(requests).To(ConsistOf(tc.want))
		})
	}
}

// The chassis leg has to select exactly the agents of the chassis that changed:
// a namespace holds the agents of every chassis deployed into it, and a chassis
// of the same name in another namespace says nothing about these.
func TestChassisToAgentsMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	c := neutronFakeClientBuilder(
		agentFor("edge", testNamespace, testOVNChassisName),
		agentFor("second", testNamespace, testOVNChassisName),
		agentFor("other-chassis", testNamespace, "gateway"),
		agentFor("elsewhere", "openstack-two", testOVNChassisName),
	).Build()

	requests := chassisToAgentsMapper(c)(context.Background(),
		readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName))

	g.Expect(requests).To(ConsistOf(
		agentReq(testNamespace, "edge"),
		agentReq(testNamespace, "second"),
	))
}

// An object of another kind maps to nothing rather than to whatever agents
// happen to name a chassis of the same name, and a List the cache cannot answer
// maps to nothing rather than to a partial fan-out.
func TestChassisToAgentsMapper_IgnoresOtherKindsAndListErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	c := neutronFakeClientBuilder(agentFor("edge", testNamespace, testOVNChassisName)).Build()
	notAChassis := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      testOVNChassisName,
		Namespace: testNamespace,
	}}

	g.Expect(chassisToAgentsMapper(c)(context.Background(), notAChassis)).To(BeNil())
	g.Expect(chassisToAgentsMapper(brokenListClient())(context.Background(),
		readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName))).To(BeEmpty())
}

// The central leg is the one that matters most: the two values an agent cannot
// start without are published on the central's status, and an agent names a
// chassis rather than a central, so the event has to travel through every
// chassis attached to it.
func TestCentralToAgentsMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	c := neutronFakeClientBuilder(
		readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName),
		readyOVNChassis("gateway", testNamespace, testOVNCentralName),
		readyOVNChassis("unrelated", testNamespace, "other-central"),
		agentFor("edge", testNamespace, testOVNChassisName),
		agentFor("gateway-agent", testNamespace, "gateway"),
		agentFor("unrelated-agent", testNamespace, "unrelated"),
		agentFor("elsewhere", "openstack-two", testOVNChassisName),
	).Build()

	requests := centralToAgentsMapper(c)(context.Background(),
		readyOVNCentral(testOVNCentralName, testNamespace,
			testNorthboundAddress, testSouthboundAddress, "ovn-client"))

	g.Expect(requests).To(ConsistOf(
		agentReq(testNamespace, "edge"),
		agentReq(testNamespace, "gateway-agent"),
	))
}

// A central no chassis in its namespace attaches to reaches no agent, and the
// two failure shapes map to nothing.
func TestCentralToAgentsMapper_UnreferencedAndFailures(t *testing.T) {
	g := NewGomegaWithT(t)
	central := readyOVNCentral("lonely", testNamespace,
		testNorthboundAddress, testSouthboundAddress, "ovn-client")
	c := neutronFakeClientBuilder(
		readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName),
		agentFor("edge", testNamespace, testOVNChassisName),
	).Build()

	g.Expect(centralToAgentsMapper(c)(context.Background(), central)).To(BeEmpty())

	notACentral := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "lonely", Namespace: testNamespace}}
	g.Expect(centralToAgentsMapper(c)(context.Background(), notACentral)).To(BeNil())
	g.Expect(centralToAgentsMapper(brokenListClient())(context.Background(), central)).To(BeEmpty())
}

// One agent reachable through two chassis of the same central is enqueued once:
// a duplicate request is work the workqueue would coalesce anyway, and the
// dedup keeps the fan-out proportional to the agents rather than to the pairs.
func TestCentralToAgentsMapper_DeduplicatesAcrossChassis(t *testing.T) {
	g := NewGomegaWithT(t)
	// Two chassis of one central, and an agent indexed under each of them.
	c := neutronFakeClientBuilder(
		readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName),
		readyOVNChassis("gateway", testNamespace, testOVNCentralName),
		agentFor("edge", testNamespace, testOVNChassisName),
	).Build()

	requests := centralToAgentsMapper(c)(context.Background(),
		readyOVNCentral(testOVNCentralName, testNamespace,
			testNorthboundAddress, testSouthboundAddress, "ovn-client"))

	g.Expect(requests).To(HaveLen(1))
	g.Expect(requests).To(ConsistOf(agentReq(testNamespace, "edge")))
}

// The two index extractors are what both OVN legs and the Secret leg resolve
// through, so a wrong-type object or an empty reference has to index under
// nothing rather than under the zero value.
func TestAgentIndexExtractors(t *testing.T) {
	t.Run("the Secret index", func(t *testing.T) {
		tests := []struct {
			name string
			obj  client.Object
			want []string
		}{
			{
				name: "an agent referencing neither Secret",
				obj:  validAgent(),
			},
			{
				name: "both references are indexed",
				obj: func() client.Object {
					cr := withNovaMetadata("shared_secret")
					cr.Spec.Messaging = &commonv1.MessagingSpec{
						SecretRef: &commonv1.SecretRefSpec{Name: "external-bus"},
					}
					return cr
				}(),
				want: []string{testAgentSharedSecretName, "external-bus"},
			},
			{
				name: "one Secret serving both references is indexed once",
				obj: func() client.Object {
					cr := withNovaMetadata("shared_secret")
					cr.Spec.Messaging = &commonv1.MessagingSpec{
						SecretRef: &commonv1.SecretRefSpec{Name: testAgentSharedSecretName},
					}
					return cr
				}(),
				want: []string{testAgentSharedSecretName},
			},
			{
				name: "an empty reference is skipped",
				obj: func() client.Object {
					cr := withNovaMetadata("shared_secret")
					cr.Spec.NovaMetadata.SharedSecretRef.Name = ""
					return cr
				}(),
			},
			{
				name: "a wrong-type object indexes under nothing",
				obj:  readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName),
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				g := NewGomegaWithT(t)
				got := agentSecretNameExtractor(tc.obj)
				if len(tc.want) == 0 {
					g.Expect(got).To(BeEmpty())
					return
				}
				g.Expect(got).To(Equal(tc.want))
			})
		}
	})

	t.Run("the chassis index", func(t *testing.T) {
		g := NewGomegaWithT(t)

		g.Expect(agentChassisRefExtractor(validAgent())).To(Equal([]string{testOVNChassisName}))

		withoutName := validAgent()
		withoutName.Spec.ChassisRef.Name = ""
		g.Expect(agentChassisRefExtractor(withoutName)).To(BeEmpty())

		g.Expect(agentChassisRefExtractor(&ovnv1alpha1.OVNChassis{})).To(BeEmpty())
	})
}
