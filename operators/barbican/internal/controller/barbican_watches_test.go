// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Barbican watch mappers. These plain handler.MapFunc
// closures are exercised directly against a pre-indexed fake client, mirroring
// glance_watches_test.go.
package controller

import (
	"context"
	"fmt"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

// namedSecret returns a bare Secret object for a watch event (no data needed —
// the mappers key off the name and namespace only).
func namedSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
}

// --- secretToBarbicanWithStoresMapper ---

func TestSecretToBarbicanWithStoresMapper_BarbicanReferencedSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	c := barbicanFakeClientBuilder(testBarbican()).Build()
	mapper := secretToBarbicanWithStoresMapper(c)

	// spec.serviceUser.secretRef.name of test-barbican.
	reqs := mapper(context.Background(), namedSecret("barbican-service-user"))
	g.Expect(reqs).To(ConsistOf(barbicanRequest))

	// spec.database.secretRef.name of test-barbican.
	reqs = mapper(context.Background(), namedSecret("barbican-db"))
	g.Expect(reqs).To(ConsistOf(barbicanRequest))
}

func TestSecretToBarbicanWithStoresMapper_StoreSecretWakesParentBarbican(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testBrownfieldStore()
	store.Spec.OpenBao.Server.CABundleSecretRef = &barbicanv1alpha1.SecretNameRefSpec{Name: "brownfield-ca"}
	c := barbicanFakeClientBuilder(testBarbican(), store).Build()
	mapper := secretToBarbicanWithStoresMapper(c)

	// Both the AppRole credentials and the trust bundle are referenced only by
	// the store, so only the store leg produces the request — for the store's
	// parent Barbican.
	g.Expect(mapper(context.Background(), namedSecret("brownfield-approle"))).To(ConsistOf(barbicanRequest))
	g.Expect(mapper(context.Background(), namedSecret("brownfield-ca"))).To(ConsistOf(barbicanRequest))
}

func TestSecretToBarbicanWithStoresMapper_UnionsAndDeduplicates(t *testing.T) {
	g := NewGomegaWithT(t)

	// A Secret referenced by BOTH the Barbican spec (serviceUser) and a store
	// attached to that same Barbican must yield exactly one request.
	store := testBrownfieldStore()
	store.Spec.OpenBao.Server.CredentialsSecretRef.Name = "barbican-service-user"
	c := barbicanFakeClientBuilder(testBarbican(), store).Build()

	reqs := secretToBarbicanWithStoresMapper(c)(context.Background(), namedSecret("barbican-service-user"))
	g.Expect(reqs).To(ConsistOf(barbicanRequest))
}

func TestSecretToBarbicanWithStoresMapper_UnreferencedSecretEnqueuesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	c := barbicanFakeClientBuilder(testBarbican(), testBrownfieldStore()).Build()
	reqs := secretToBarbicanWithStoresMapper(c)(context.Background(), namedSecret("unrelated"))
	g.Expect(reqs).To(BeEmpty())
}

func TestSecretToBarbicanWithStoresMapper_StoreListErrorKeepsBaseResults(t *testing.T) {
	g := NewGomegaWithT(t)

	c := barbicanFakeClientBuilder(testBarbican(), testBrownfieldStore()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*barbicanv1alpha1.BarbicanSecretStoreList); ok {
					return fmt.Errorf("simulated list error")
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()

	// The store leg logs and drops out; the Barbican name index still resolves.
	reqs := secretToBarbicanWithStoresMapper(c)(context.Background(), namedSecret("barbican-db"))
	g.Expect(reqs).To(ConsistOf(barbicanRequest))

	// With only the failing leg able to match, nothing is enqueued.
	reqs = secretToBarbicanWithStoresMapper(c)(context.Background(), namedSecret("brownfield-approle"))
	g.Expect(reqs).To(BeEmpty())
}

// --- barbicanSecretStoreToBarbicanMapper ---

func TestBarbicanSecretStoreToBarbicanMapper_EnqueuesParent(t *testing.T) {
	g := NewGomegaWithT(t)
	mapper := barbicanSecretStoreToBarbicanMapper()

	g.Expect(mapper(context.Background(), testManagedStore())).To(ConsistOf(barbicanRequest))

	// An empty barbicanRef (bypassed admission) enqueues nothing rather than a
	// request with an empty name.
	empty := testManagedStore()
	empty.Spec.BarbicanRef.Name = ""
	g.Expect(mapper(context.Background(), empty)).To(BeEmpty())

	// An event for an unrelated kind maps to nothing.
	g.Expect(mapper(context.Background(), namedSecret("brownfield-approle"))).To(BeEmpty())
}

// --- mariaDBToBarbicanMapper ---

func TestMariaDBToBarbicanMapper_EnqueuesReferencingBarbicans(t *testing.T) {
	g := NewGomegaWithT(t)

	managed := testBarbican()
	managed.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db"}
	// A brownfield Barbican addresses its database by host, so no cluster event
	// can match it.
	brownfield := testBarbican()
	brownfield.Name = "other-barbican"
	c := barbicanFakeClientBuilder(managed, brownfield).Build()

	mariadb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: testNamespace},
	}
	g.Expect(mariaDBToBarbicanMapper(c)(context.Background(), mariadb)).To(ConsistOf(barbicanRequest))

	// A cluster nothing references enqueues nothing.
	unrelated := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-db", Namespace: testNamespace},
	}
	g.Expect(mariaDBToBarbicanMapper(c)(context.Background(), unrelated)).To(BeEmpty())
}

// --- esoStoreToBarbicanMapper ---

func TestEsoStoreToBarbicanMapper_ClusterStoreWakesDefaultingBarbicans(t *testing.T) {
	g := NewGomegaWithT(t)

	// test-barbican omits spec.secretStoreRef, so it resolves to the shared
	// cluster store; the pinned one must stay untouched.
	pinned := testBarbican()
	pinned.Name = "pinned-barbican"
	pinned.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced,
		Name: "tenant-store",
	}
	c := barbicanFakeClientBuilder(testBarbican(), pinned).Build()

	clusterStore := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	reqs := esoStoreToBarbicanMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), clusterStore)
	g.Expect(reqs).To(ConsistOf(barbicanRequest))

	// A cluster store no Barbican routes through enqueues nothing.
	foreign := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: "foreign-store"}}
	g.Expect(esoStoreToBarbicanMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), foreign)).To(BeEmpty())
}

func TestEsoStoreToBarbicanMapper_NamespacedStoreWakesOnlyItsOwn(t *testing.T) {
	g := NewGomegaWithT(t)

	pinned := testBarbican()
	pinned.Name = "pinned-barbican"
	pinned.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced,
		Name: "tenant-store",
	}
	c := barbicanFakeClientBuilder(testBarbican(), pinned).Build()

	store := &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: "tenant-store", Namespace: testNamespace}}
	reqs := esoStoreToBarbicanMapper(c, commonv1.SecretStoreKindNamespaced)(context.Background(), store)
	g.Expect(reqs).To(ConsistOf(reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "pinned-barbican"},
	}))

	// The cluster-scoped registration ignores the namespaced store even under a
	// name collision: the kinds do not match.
	collision := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: "tenant-store"}}
	g.Expect(esoStoreToBarbicanMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), collision)).To(BeEmpty())
}
