// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func finalizeKey() client.ObjectKey {
	return client.ObjectKey{Name: "keystone", Namespace: "openstack"}
}

func namedDatabase() *mariadbv1alpha1.Database {
	return &mariadbv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: "keystone", Namespace: "openstack"}}
}

func namedUser() *mariadbv1alpha1.User {
	return &mariadbv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "keystone", Namespace: "openstack"}}
}

func namedGrant() *mariadbv1alpha1.Grant {
	return &mariadbv1alpha1.Grant{ObjectMeta: metav1.ObjectMeta{Name: "keystone", Namespace: "openstack"}}
}

// namedAdditionalDatabase and namedAdditionalGrant carry the name
// AdditionalResourceName derives for the schema nova_cell0 of the keystone
// instance.
func namedAdditionalDatabase() *mariadbv1alpha1.Database {
	return &mariadbv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: "keystone-nova-cell0", Namespace: "openstack"}}
}

func namedAdditionalGrant() *mariadbv1alpha1.Grant {
	return &mariadbv1alpha1.Grant{ObjectMeta: metav1.ObjectMeta{Name: "keystone-nova-cell0", Namespace: "openstack"}}
}

func TestFinalizeResources_deletesAllThree(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(namedDatabase(), namedUser(), namedGrant()).
		Build()
	ctx := context.Background()

	g.Expect(FinalizeResources(ctx, c, finalizeKey())).To(Succeed())

	for _, obj := range []client.Object{&mariadbv1alpha1.Database{}, &mariadbv1alpha1.User{}, &mariadbv1alpha1.Grant{}} {
		err := c.Get(ctx, finalizeKey(), obj)
		g.Expect(err).To(HaveOccurred())
	}
}

// TestFinalizeResources_idempotentNotFound exercises the absent-resource path:
// deleting when nothing exists (brownfield / already-cleaned) is a no-op success.
func TestFinalizeResources_idempotentNotFound(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()

	g.Expect(FinalizeResources(context.Background(), c, finalizeKey())).To(Succeed())
}

func TestFinalizeResources_propagatesDeleteError(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	boom := errors.New("delete rejected")
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(namedDatabase()).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return boom
			},
		}).Build()

	err := FinalizeResources(context.Background(), c, finalizeKey())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("delete rejected"))
}

// TestFinalizeResources_deletesAdditional covers the variadic tail: the Database
// and the Grant of the additional schema go away with the primary three.
func TestFinalizeResources_deletesAdditional(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(namedDatabase(), namedUser(), namedGrant(), namedAdditionalDatabase(), namedAdditionalGrant()).
		Build()
	ctx := context.Background()

	g.Expect(FinalizeResources(ctx, c, finalizeKey(), "nova_cell0")).To(Succeed())

	for _, obj := range []client.Object{&mariadbv1alpha1.Database{}, &mariadbv1alpha1.User{}, &mariadbv1alpha1.Grant{}} {
		err := c.Get(ctx, finalizeKey(), obj)
		g.Expect(err).To(HaveOccurred())
	}
	for _, obj := range []client.Object{&mariadbv1alpha1.Database{}, &mariadbv1alpha1.Grant{}} {
		err := c.Get(ctx, client.ObjectKey{Name: "keystone-nova-cell0", Namespace: "openstack"}, obj)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}
}

// TestFinalizeResources_additionalAbsentIsIdempotent names a schema whose CRs
// were never created: the additional deletes are no-op NotFounds.
func TestFinalizeResources_additionalAbsentIsIdempotent(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(namedDatabase(), namedUser(), namedGrant()).
		Build()

	g.Expect(FinalizeResources(context.Background(), c, finalizeKey(), "nova_cell0")).To(Succeed())
}

// TestFinalizeResources_additionalDeleteErrorPropagates pins the wrapped error
// on the derived key, not on the key of the service CR.
func TestFinalizeResources_additionalDeleteErrorPropagates(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	boom := errors.New("delete rejected")
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(namedDatabase(), namedUser(), namedGrant(), namedAdditionalDatabase(), namedAdditionalGrant()).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*mariadbv1alpha1.Grant); ok && obj.GetName() == "keystone-nova-cell0" {
					return boom
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()

	err := FinalizeResources(context.Background(), c, finalizeKey(), "nova_cell0")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(Equal("deleting *v1alpha1.Grant openstack/keystone-nova-cell0: delete rejected"))
}

func TestHasLiveResources_falseWhenAbsent(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()

	live, err := HasLiveResources(context.Background(), c, finalizeKey(), "nova_cell0")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(live).To(BeFalse())
}

func TestHasLiveResources_trueWhenLive(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	// Only the User CR exists — any one live resource reports true.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(namedUser()).Build()

	live, err := HasLiveResources(context.Background(), c, finalizeKey())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(live).To(BeTrue())
}

func TestHasLiveResources_falseWhenTerminating(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	// A resource carrying a DeletionTimestamp is not "live" cleanup work. The
	// fake client requires a finalizer for a deletion timestamp to persist.
	term := namedGrant()
	term.Finalizers = []string{"mariadb.mmontes.io/finalizer"}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(term).Build()
	ctx := context.Background()
	g.Expect(c.Delete(ctx, namedGrant())).To(Succeed())

	live, err := HasLiveResources(ctx, c, finalizeKey())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(live).To(BeFalse())
}

func TestHasLiveResources_trueWhenAdditionalLive(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	// The primary three are gone, the additional Database is not: cleanup work
	// remains.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(namedAdditionalDatabase()).Build()

	live, err := HasLiveResources(context.Background(), c, finalizeKey(), "nova_cell0")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(live).To(BeTrue())
}

func TestHasLiveResources_falseWhenAdditionalTerminating(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	// As above, a deletion timestamp on the additional Database is not live
	// cleanup work. The fake client requires a finalizer for it to persist.
	term := namedAdditionalDatabase()
	term.Finalizers = []string{"mariadb.mmontes.io/finalizer"}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(term).Build()
	ctx := context.Background()
	g.Expect(c.Delete(ctx, namedAdditionalDatabase())).To(Succeed())

	live, err := HasLiveResources(ctx, c, finalizeKey(), "nova_cell0")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(live).To(BeFalse())
}
