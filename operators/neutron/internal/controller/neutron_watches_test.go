// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Neutron watch mappers. These plain handler.MapFunc
// closures are exercised directly against a pre-indexed fake client, mirroring
// barbican_watches_test.go.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// namedSecret returns a bare Secret object for a watch event; no data is needed,
// because the mappers key off the name and namespace only. The namespace is a
// parameter, unlike in the sibling operators' helper, because the OVN client
// Secret and the Neutron waiting on it are rarely in the same one.
func namedSecret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

// requestFor is the reconcile request for a Neutron other than the shared
// fixture neutronRequest addresses: one in another namespace, or a second one
// beside it.
func requestFor(namespace, name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

// --- secretToNeutronMapper --------------------------------------------------

// TestSecretToNeutronMapper_ReferencedSecretsEnqueueTheCR walks the three spec
// references the name index carries. The transport-URL Secret is a brownfield
// shape only: a managed Neutron derives the URL from the RabbitmqCluster it
// names and references no Secret for it.
func TestSecretToNeutronMapper_ReferencedSecretsEnqueueTheCR(t *testing.T) {
	brownfield := validNeutron()
	brownfield.Spec.Messaging.ClusterRef = nil
	brownfield.Spec.Messaging.SecretRef = &commonv1.SecretRefSpec{Name: "neutron-transport", Key: "transport_url"}

	tests := []struct {
		name    string
		neutron *neutronv1alpha1.Neutron
		secret  string
	}{
		{name: "spec.database.secretRef.name", neutron: validNeutron(), secret: "neutron-db"},
		{name: "spec.serviceUser.secretRef.name", neutron: validNeutron(), secret: "neutron-service-user"},
		{name: "spec.messaging.secretRef.name", neutron: brownfield, secret: "neutron-transport"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c := neutronFakeClientBuilder(tc.neutron).Build()

			reqs := secretToNeutronMapper(c)(context.Background(), namedSecret(testNamespace, tc.secret))
			g.Expect(reqs).To(ConsistOf(neutronRequest))
		})
	}
}

// TestSecretToNeutronMapper_OVNClientSecretWakesTheDrivingNeutron pins the leg
// the name index cannot serve. A central publishes its client identity beside
// itself in the privileged networking namespace, while the Neutron presenting
// that identity lives with the rest of the control plane, so the index-backed
// List, scoped to the Secret's own namespace, looks for the CR where it is not.
func TestSecretToNeutronMapper_OVNClientSecretWakesTheDrivingNeutron(t *testing.T) {
	g := NewGomegaWithT(t)

	driving := validNeutron()
	driving.Namespace = "tenant"
	driving.Spec.OVN.CentralRef = neutronv1alpha1.OVNCentralRef{Name: testOVNCentralName, Namespace: "ovn-system"}
	// A Neutron driving a same-named central in its own namespace must stay
	// untouched: the index is keyed on the qualified reference, not the name.
	elsewhere := validNeutron()
	elsewhere.Name = "elsewhere-neutron"
	central := readyOVNCentral(testOVNCentralName, "ovn-system",
		testNorthboundAddress, testSouthboundAddress, "ovn-client")
	c := neutronFakeClientBuilder(driving, elsewhere, central).Build()

	mapper := secretToNeutronMapper(c)
	g.Expect(mapper(context.Background(), namedSecret("ovn-system", "ovn-client"))).
		To(ConsistOf(requestFor("tenant", testNeutronName)))

	// A Secret sitting next to the central that the central did not publish is
	// not a client identity.
	g.Expect(mapper(context.Background(), namedSecret("ovn-system", "ovn-northbound-tls"))).To(BeEmpty())
}

// TestSecretToNeutronMapper_UnionsAndDeduplicates covers a Secret both legs
// match. Enqueuing it twice would cost a second reconcile pass that reads the
// same state the first one did.
func TestSecretToNeutronMapper_UnionsAndDeduplicates(t *testing.T) {
	g := NewGomegaWithT(t)

	// The central publishes its client identity under the name the CR already
	// carries in spec.database.secretRef, so the index leg and the central leg
	// resolve to the same Neutron.
	central := readyOVNCentral(testOVNCentralName, testNamespace,
		testNorthboundAddress, testSouthboundAddress, "neutron-db")
	c := neutronFakeClientBuilder(validNeutron(), central).Build()

	reqs := secretToNeutronMapper(c)(context.Background(), namedSecret(testNamespace, "neutron-db"))
	g.Expect(reqs).To(ConsistOf(neutronRequest))
}

func TestSecretToNeutronMapper_UnreferencedSecretEnqueuesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	central := readyOVNCentral(testOVNCentralName, testNamespace,
		testNorthboundAddress, testSouthboundAddress, "ovn-client")
	mapper := secretToNeutronMapper(neutronFakeClientBuilder(validNeutron(), central).Build())

	g.Expect(mapper(context.Background(), namedSecret(testNamespace, "unrelated"))).To(BeEmpty())

	// The mapper matches on name and namespace, not on kind, because the leg it
	// backs is fed Secrets alone; an object of another kind that no Neutron and
	// no central names is dropped like any other unreferenced one.
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: testNamespace}}
	g.Expect(mapper(context.Background(), configMap)).To(BeEmpty())
}

// TestSecretToNeutronMapper_ListErrorKeepsBaseResults covers both Lists the OVN
// leg makes. handler.MapFunc has no error return, so a leg that cannot answer
// drops out and leaves the results the other one produced; the periodic requeue
// the OVN steps poll with is the fallback for the CRs it missed.
func TestSecretToNeutronMapper_ListErrorKeepsBaseResults(t *testing.T) {
	// The central publishes its client identity under the name the Neutron in its
	// own namespace already references, so one Secret event reaches both legs:
	// the name index resolves the local CR, the OVN leg adds the one driving the
	// central from another namespace.
	central := readyOVNCentral(testOVNCentralName, testNamespace,
		testNorthboundAddress, testSouthboundAddress, "neutron-db")
	crossNamespace := validNeutron()
	crossNamespace.Namespace = "tenant"
	objs := []client.Object{validNeutron(), crossNamespace, central}
	crossRequest := requestFor("tenant", testNeutronName)

	tests := []struct {
		name  string
		funcs interceptor.Funcs
		want  []reconcile.Request
	}{
		{
			// The control: what the two failures below take one leg away from.
			name: "both legs answer",
			want: []reconcile.Request{neutronRequest, crossRequest},
		},
		{
			name: "the OVNCentral List fails",
			funcs: interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*ovnv1alpha1.OVNCentralList); ok {
						return apierrors.NewServiceUnavailable("cache not started")
					}
					return cl.List(ctx, list, opts...)
				},
			},
			want: []reconcile.Request{neutronRequest},
		},
		{
			name: "the index lookup behind the central fails",
			funcs: interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					options := &client.ListOptions{}
					for _, opt := range opts {
						opt.ApplyToList(options)
					}
					// The name index is read in the Secret's own namespace; only
					// the OVN leg lists the Neutrons cluster-wide.
					if _, ok := list.(*neutronv1alpha1.NeutronList); ok && options.Namespace == "" {
						return apierrors.NewServiceUnavailable("cache not started")
					}
					return cl.List(ctx, list, opts...)
				},
			},
			want: []reconcile.Request{neutronRequest},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c := neutronFakeClientBuilder(objs...).WithInterceptorFuncs(tc.funcs).Build()

			reqs := secretToNeutronMapper(c)(context.Background(), namedSecret(testNamespace, "neutron-db"))
			g.Expect(reqs).To(ConsistOf(tc.want))
		})
	}
}

// --- centralToNeutronsMapper ------------------------------------------------

// TestCentralToNeutronsMapper_EnqueuesEveryDrivingNeutron covers both ways a CR
// can name the central: qualified from another namespace, and bare from the
// central's own, which the index resolves against the CR's namespace.
func TestCentralToNeutronsMapper_EnqueuesEveryDrivingNeutron(t *testing.T) {
	g := NewGomegaWithT(t)

	remote := validNeutron()
	remote.Namespace = "tenant"
	remote.Spec.OVN.CentralRef = neutronv1alpha1.OVNCentralRef{Name: testOVNCentralName, Namespace: "ovn-system"}
	sameNamespace := validNeutron()
	sameNamespace.Name = "colocated-neutron"
	sameNamespace.Namespace = "ovn-system"
	sameNamespace.Spec.OVN.CentralRef = neutronv1alpha1.OVNCentralRef{Name: testOVNCentralName}
	// Drives a same-named central in the control-plane namespace instead.
	other := validNeutron()
	c := neutronFakeClientBuilder(remote, sameNamespace, other).Build()

	central := readyOVNCentral(testOVNCentralName, "ovn-system",
		testNorthboundAddress, testSouthboundAddress, "ovn-client")
	g.Expect(centralToNeutronsMapper(c)(context.Background(), central)).To(ConsistOf(
		requestFor("tenant", testNeutronName),
		requestFor("ovn-system", "colocated-neutron"),
	))

	// A central nobody drives enqueues nothing.
	unreferenced := readyOVNCentral("spare", "ovn-system",
		testNorthboundAddress, testSouthboundAddress, "spare-client")
	g.Expect(centralToNeutronsMapper(c)(context.Background(), unreferenced)).To(BeEmpty())
}

// TestCentralToNeutronsMapper_IgnoresNonCentralObjects is the guard against a
// future caller wiring the mapper onto a second kind: an object of another kind
// must map to nothing rather than to whatever Neutrons drive the OVNCentral that
// happens to share its name.
func TestCentralToNeutronsMapper_IgnoresNonCentralObjects(t *testing.T) {
	g := NewGomegaWithT(t)

	c := neutronFakeClientBuilder(validNeutron()).Build()
	g.Expect(centralToNeutronsMapper(c)(context.Background(),
		namedSecret(testNamespace, testOVNCentralName))).To(BeEmpty())
}

// TestCentralToNeutronsMapper_ListErrorMapsToNothing pins the contract a List the
// mapper cannot answer falls under: nothing is enqueued, rather than a partial
// fan-out that would look like the CRs left out no longer drive the central.
func TestCentralToNeutronsMapper_ListErrorMapsToNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	c := neutronFakeClientBuilder(validNeutron()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewServiceUnavailable("cache not started")
			},
		}).Build()

	central := readyOVNCentral(testOVNCentralName, testNamespace,
		testNorthboundAddress, testSouthboundAddress, "ovn-client")
	g.Expect(centralToNeutronsMapper(c)(context.Background(), central)).To(BeEmpty())
}

// --- mariaDBToNeutronMapper -------------------------------------------------

func TestMariaDBToNeutronMapper_EnqueuesReferencingNeutrons(t *testing.T) {
	g := NewGomegaWithT(t)

	managed := validNeutron()
	managed.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db"}
	// A brownfield Neutron addresses its database by host, so no cluster event
	// can match it.
	brownfield := validNeutron()
	brownfield.Name = "brownfield-neutron"
	c := neutronFakeClientBuilder(managed, brownfield).Build()

	mariadb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: testNamespace},
	}
	g.Expect(mariaDBToNeutronMapper(c)(context.Background(), mariadb)).To(ConsistOf(neutronRequest))

	// A cluster nothing references enqueues nothing.
	unrelated := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-db", Namespace: testNamespace},
	}
	g.Expect(mariaDBToNeutronMapper(c)(context.Background(), unrelated)).To(BeEmpty())
}

// --- esoStoreToNeutronMapper ------------------------------------------------

// pinnedNeutron returns a Neutron routing its secrets through a namespaced
// SecretStore instead of the shared cluster store the fixture defaults to.
func pinnedNeutron() *neutronv1alpha1.Neutron {
	neutron := validNeutron()
	neutron.Name = "pinned-neutron"
	neutron.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced,
		Name: "tenant-store",
	}
	return neutron
}

func TestEsoStoreToNeutronMapper_ClusterStoreWakesDefaultingNeutrons(t *testing.T) {
	g := NewGomegaWithT(t)

	// The fixture omits spec.secretStoreRef, so it resolves to the shared cluster
	// store; the pinned one must stay untouched.
	c := neutronFakeClientBuilder(validNeutron(), pinnedNeutron()).Build()

	clusterStore := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	reqs := esoStoreToNeutronMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), clusterStore)
	g.Expect(reqs).To(ConsistOf(neutronRequest))

	// A cluster store no Neutron routes through enqueues nothing.
	foreign := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: "foreign-store"}}
	g.Expect(esoStoreToNeutronMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), foreign)).
		To(BeEmpty())
}

func TestEsoStoreToNeutronMapper_NamespacedStoreWakesOnlyItsOwn(t *testing.T) {
	g := NewGomegaWithT(t)

	c := neutronFakeClientBuilder(validNeutron(), pinnedNeutron()).Build()

	store := &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: "tenant-store", Namespace: testNamespace}}
	reqs := esoStoreToNeutronMapper(c, commonv1.SecretStoreKindNamespaced)(context.Background(), store)
	g.Expect(reqs).To(ConsistOf(requestFor(testNamespace, "pinned-neutron")))

	// The cluster-scoped registration ignores the namespaced store even under a
	// name collision: the kinds do not match.
	collision := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: "tenant-store"}}
	g.Expect(esoStoreToNeutronMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), collision)).
		To(BeEmpty())
}
