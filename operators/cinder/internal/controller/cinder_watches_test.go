// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Cinder / CinderBackend / CinderBackupBackend watch
// mappers. These plain handler.MapFunc closures are exercised directly against a
// pre-indexed fake client, mirroring glance_watches_test.go.
package controller

import (
	"context"
	"errors"
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
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// cinderRequest is the reconcile request for the shared Cinder fixture.
var cinderRequest = reconcile.Request{
	NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testCinderName},
}

// namedSecret returns a bare Secret object for a watch event; the mappers key
// off the name and namespace only.
func namedSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
}

// mapperClient returns a fake client carrying the field indexes the mappers
// resolve against, including the Secret-name index SetupWithManager registers.
func mapperClient(objs ...client.Object) client.Client {
	return cinderFakeClientBuilder(objs...).
		WithIndex(&cinderv1alpha1.Cinder{}, CinderSecretNameIndexKey, cinderSecretNameExtractor).
		Build()
}

func TestCinderSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	// The shared fixture references the database and service-user Secrets and
	// derives its transport URL from a RabbitmqCluster.
	g.Expect(cinderSecretNameExtractor(validCinder())).
		To(Equal([]string{"cinder-db", "cinder-service-user"}))

	// A Keystone-free Cinder carries no service user, so only the database
	// Secret is indexed.
	g.Expect(cinderSecretNameExtractor(keystoneFreeCinder())).To(Equal([]string{"cinder-db"}))

	// A brownfield broker is named by a Secret of its own.
	brownfield := validCinder()
	brownfield.Spec.Messaging = commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "cinder-transport"},
	}
	g.Expect(cinderSecretNameExtractor(brownfield)).
		To(Equal([]string{"cinder-db", "cinder-service-user", "cinder-transport"}))

	// One Secret carrying two roles is indexed once, or the mapper would enqueue
	// the same CR twice for one event.
	shared := validCinder()
	shared.Spec.ServiceUser.SecretRef.Name = "cinder-db"
	g.Expect(cinderSecretNameExtractor(shared)).To(Equal([]string{"cinder-db"}))

	// controller-runtime never calls the extractor with another type, but a nil
	// return is safer than a panic if it ever does.
	g.Expect(cinderSecretNameExtractor(testCinderBackend("nfs1"))).To(BeNil())
}

func TestSecretToCinderMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	c := mapperClient(validCinder())
	mapper := secretToCinderMapper(c)

	// spec.database.secretRef.name and spec.serviceUser.secretRef.name.
	g.Expect(mapper(context.Background(), namedSecret("cinder-db"))).To(ConsistOf(cinderRequest))
	g.Expect(mapper(context.Background(), namedSecret("cinder-service-user"))).To(ConsistOf(cinderRequest))

	// A Secret the operator derived carries an owner reference instead.
	derived := namedSecret("cinder-db-connection")
	derived.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: cinderv1alpha1.GroupVersion.String(),
		Kind:       "Cinder",
		Name:       testCinderName,
		UID:        "cinder-uid",
	}}
	g.Expect(mapper(context.Background(), derived)).To(ConsistOf(cinderRequest))

	// An NFS backend references no Secret, so nothing reaches a Cinder through
	// one.
	g.Expect(mapper(context.Background(), namedSecret("unrelated"))).To(BeEmpty())
}

func TestMariaDBToCinderMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	managed := validCinder()
	managed.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db"}
	c := mapperClient(managed)
	mapper := mariaDBToCinderMapper(c)

	cluster := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: testNamespace},
	}
	g.Expect(mapper(context.Background(), cluster)).To(ConsistOf(cinderRequest))

	other := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "another-db", Namespace: testNamespace},
	}
	g.Expect(mapper(context.Background(), other)).To(BeEmpty())

	// A brownfield Cinder names a host instead of a cluster, so no MariaDB event
	// concerns it.
	brownfield := mapperClient(validCinder())
	g.Expect(mariaDBToCinderMapper(brownfield)(context.Background(), cluster)).To(BeEmpty())
}

func TestStoreToCinderMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	c := mapperClient(validCinder())

	// A Cinder that omits spec.secretStoreRef resolves to the shared cluster
	// store, so an outage of that store wakes it.
	clusterStore := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	g.Expect(storeToCinderMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), clusterStore)).
		To(ConsistOf(cinderRequest))

	// The same object watched as a namespaced store matches no CR: the kind is
	// part of the reference.
	g.Expect(storeToCinderMapper(c, commonv1.SecretStoreKindNamespaced)(context.Background(), clusterStore)).
		To(BeEmpty())

	other := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: "another-store"}}
	g.Expect(storeToCinderMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), other)).
		To(BeEmpty())
}

func TestSatelliteToCinderMappers(t *testing.T) {
	g := NewGomegaWithT(t)

	backendMapper := cinderBackendToCinderMapper()
	g.Expect(backendMapper(context.Background(), testCinderBackend("nfs1"))).To(ConsistOf(cinderRequest))

	// An empty cinderRef (bypassed admission) enqueues nothing rather than a
	// request with an empty name.
	unattached := testCinderBackend("broken")
	unattached.Spec.CinderRef.Name = ""
	g.Expect(backendMapper(context.Background(), unattached)).To(BeEmpty())
	// The mapper is registered on one kind alone, so an object of another type
	// must not enqueue the Cinder that happens to share its name.
	g.Expect(backendMapper(context.Background(), testCinderBackupBackend("backups"))).To(BeNil())

	backupMapper := cinderBackupBackendToCinderMapper()
	g.Expect(backupMapper(context.Background(), testCinderBackupBackend("backups"))).To(ConsistOf(cinderRequest))
	g.Expect(backupMapper(context.Background(), testCinderBackend("nfs1"))).To(BeNil())
}

func TestCinderToSatellitesMappers(t *testing.T) {
	g := NewGomegaWithT(t)

	attached := testCinderBackend("nfs1")
	sibling := testCinderBackend("nfs2")
	foreign := testCinderBackend("elsewhere")
	foreign.Spec.CinderRef.Name = "another-cinder"
	backup := testCinderBackupBackend("backups")
	c := mapperClient(attached, sibling, foreign, backup)

	g.Expect(cinderToCinderBackendsMapper(c)(context.Background(), validCinder())).To(ConsistOf(
		reconcile.Request{NamespacedName: objectKey("nfs1")},
		reconcile.Request{NamespacedName: objectKey("nfs2")},
	))
	g.Expect(cinderToCinderBackupBackendsMapper(c)(context.Background(), validCinder())).To(ConsistOf(
		reconcile.Request{NamespacedName: objectKey("backups")},
	))

	// A Cinder with nothing attached fans out to nothing.
	unattached := validCinder()
	unattached.Name = "another-cinder-without-satellites"
	g.Expect(cinderToCinderBackendsMapper(c)(context.Background(), unattached)).To(BeEmpty())
}

func TestCinderToSatellitesMappers_ListErrorReturnsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	c := cinderFakeClientBuilder(testCinderBackend("nfs1")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList,
				opts ...client.ListOption,
			) error {
				if _, ok := list.(*cinderv1alpha1.CinderBackendList); ok {
					return errors.New("simulated list error")
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()

	g.Expect(cinderToCinderBackendsMapper(c)(context.Background(), validCinder())).To(BeEmpty(),
		"a List error logs and returns nil")
}
