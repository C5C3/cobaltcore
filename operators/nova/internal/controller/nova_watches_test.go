// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Nova watch mappers. These plain handler.MapFunc closures
// are exercised directly against a pre-indexed fake client, mirroring
// cinder_watches_test.go.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// novaRequest is the reconcile request for the shared Nova fixture.
var novaRequest = reconcile.Request{
	NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testNovaName},
}

// namedSecret returns a bare Secret object for a watch event; the mappers key
// off the name and namespace only.
func namedSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
}

// mapperClient returns a fake client carrying the Secret-name field index
// SetupWithManager registers, which the Secret mapper resolves against.
func mapperClient(objs ...client.Object) client.Client {
	return novaFakeClientBuilder(objs...).Build()
}

func TestNovaSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	// The shared fixture references the two database Secrets, the service user and
	// the metadata shared secret, and derives its transport URL from a
	// RabbitmqCluster.
	g.Expect(novaSecretNameExtractor(validNova())).To(Equal([]string{
		testAPIDBSecret, testCellDBSecret, testServiceUserSecret, testSharedSecret,
	}))

	// A verified bus adds the broker CA bundle.
	g.Expect(novaSecretNameExtractor(novaWithMessagingTLS())).To(Equal([]string{
		testAPIDBSecret, testCellDBSecret, testServiceUserSecret, testSharedSecret, testMessagingCASecret,
	}))

	// A brownfield broker is named by a Secret of its own.
	brownfield := validNova()
	brownfield.Spec.Messaging = commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "nova-transport"},
	}
	g.Expect(novaSecretNameExtractor(brownfield)).To(ContainElement("nova-transport"))

	// The database TLS material is indexed only while the block asks for an
	// encrypted connection: a disabled block keeps its references, and a Secret
	// event on one of them changes nothing the connection reads.
	tlsRefs := commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
	}
	tlsOn := validNova()
	tlsOn.Spec.APIDatabase.TLS = tlsRefs.DeepCopy()
	g.Expect(novaSecretNameExtractor(tlsOn)).To(ContainElements("db-ca", "db-client"))

	// The cell schema carries its own TLS block, so a rotated cell CA or client
	// certificate has to reach the CR as well; the names differ from the API
	// block's so an index over one block alone cannot satisfy both.
	cellTLSOn := validNova()
	cellTLSOn.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "cell-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "cell-db-client"},
	}
	g.Expect(novaSecretNameExtractor(cellTLSOn)).To(ContainElements("cell-db-ca", "cell-db-client"))

	tlsOff := validNova()
	tlsOff.Spec.APIDatabase.TLS = tlsRefs.DeepCopy()
	tlsOff.Spec.APIDatabase.TLS.Mode = "disabled"
	g.Expect(novaSecretNameExtractor(tlsOff)).NotTo(ContainElement("db-ca"))

	// One Secret carrying two roles is indexed once, or the mapper would enqueue
	// the same CR twice for one event.
	shared := validNova()
	shared.Spec.ServiceUser.SecretRef.Name = testAPIDBSecret
	g.Expect(novaSecretNameExtractor(shared)).To(Equal([]string{
		testAPIDBSecret, testCellDBSecret, testSharedSecret,
	}))

	// A CR that bypassed admission can carry an empty name; indexing it would map
	// every nameless Secret event onto this CR.
	empty := validNova()
	empty.Spec.Metadata.SharedSecretRef.Name = ""
	g.Expect(novaSecretNameExtractor(empty)).NotTo(ContainElement(""))

	// controller-runtime never calls the extractor with another type, but a nil
	// return is safer than a panic if it ever does.
	g.Expect(novaSecretNameExtractor(namedSecret("not-a-nova"))).To(BeNil())
}

func TestSecretToNovaMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	c := mapperClient(validNova())
	mapper := secretToNovaMapper(c)

	// The referenced Secrets resolve through the field index.
	g.Expect(mapper(context.Background(), namedSecret(testAPIDBSecret))).To(ConsistOf(novaRequest))
	g.Expect(mapper(context.Background(), namedSecret(testCellDBSecret))).To(ConsistOf(novaRequest))
	g.Expect(mapper(context.Background(), namedSecret(testSharedSecret))).To(ConsistOf(novaRequest))

	// A Secret the operator derived carries an owner reference instead.
	derived := namedSecret(testNovaName + "-api-db-connection")
	derived.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: novav1alpha1.GroupVersion.String(),
		Kind:       "Nova",
		Name:       testNovaName,
		UID:        "nova-uid",
	}}
	g.Expect(mapper(context.Background(), derived)).To(ConsistOf(novaRequest))

	g.Expect(mapper(context.Background(), namedSecret("unrelated"))).To(BeEmpty())
}

// TestNovaSecretNameExtractor_IndexesTheRemoteTransportSecret covers the Secret
// the remote contract reads its transport URL from. It is read on every pass
// while spec.remoteCompute is set, so a rotated value, or a Secret that appears
// after the step waited for it, has to reach the CR. Without the block nothing
// reads it, and an event on it changes nothing.
func TestNovaSecretNameExtractor_IndexesTheRemoteTransportSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(novaSecretNameExtractor(remoteComputeNova())).To(ContainElement(testRemoteTransportSecret))
	g.Expect(novaSecretNameExtractor(novaWithMessagingTLS())).NotTo(ContainElement(testRemoteTransportSecret))

	mapper := secretToNovaMapper(mapperClient(remoteComputeNova()))
	g.Expect(mapper(context.Background(), namedSecret(testRemoteTransportSecret))).To(ConsistOf(novaRequest))
}

// TestMariaDBToNovaMapper_EitherClusterRef pins the two-schema half of the
// watch: Nova holds nova_api and the cell schema on independently referenced
// clusters, so an outage of either one has to reach the CR. A mapper bound to
// one block alone would leave the other schema's DatabaseReady stale until the
// next periodic requeue.
func TestMariaDBToNovaMapper_EitherClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	split := validNova()
	split.Spec.APIDatabase.ClusterRef = &corev1.LocalObjectReference{Name: "api-db"}
	split.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "cell-db"}
	mapper := mariaDBToNovaMapper(mapperClient(split))

	mariaDB := func(name string) *mariadbv1alpha1.MariaDB {
		return &mariadbv1alpha1.MariaDB{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		}
	}
	g.Expect(mapper(context.Background(), mariaDB("api-db"))).To(ConsistOf(novaRequest))
	g.Expect(mapper(context.Background(), mariaDB("cell-db"))).To(ConsistOf(novaRequest))
	g.Expect(mapper(context.Background(), mariaDB("another-db"))).To(BeEmpty())

	// Both schemas on one cluster is the ordinary control plane, and it must be
	// enqueued once: a duplicated request reconciles the same CR twice per event.
	shared := mariaDBToNovaMapper(mapperClient(validNova()))
	g.Expect(shared(context.Background(), mariaDB(testMariaDBName))).To(ConsistOf(novaRequest))

	// A brownfield Nova names hosts instead of clusters, so no MariaDB event
	// concerns it.
	brownfield := validNova()
	brownfield.Spec.APIDatabase.ClusterRef = nil
	brownfield.Spec.Database.ClusterRef = nil
	g.Expect(mariaDBToNovaMapper(mapperClient(brownfield))(context.Background(), mariaDB(testMariaDBName))).
		To(BeEmpty())
}

func TestStoreToNovaMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	c := mapperClient(validNova())

	// A Nova that omits spec.secretStoreRef resolves to the shared cluster store,
	// so an outage of that store wakes it.
	clusterStore := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	g.Expect(storeToNovaMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), clusterStore)).
		To(ConsistOf(novaRequest))

	// The same object watched as a namespaced store matches no CR: the kind is
	// part of the reference.
	g.Expect(storeToNovaMapper(c, commonv1.SecretStoreKindNamespaced)(context.Background(), clusterStore)).
		To(BeEmpty())

	other := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: "another-store"}}
	g.Expect(storeToNovaMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), other)).
		To(BeEmpty())
}
