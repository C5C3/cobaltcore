// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/multicluster"
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

// finalizeOwner is the service CR the owned-resource tests clean up for. A
// ConfigMap stands in for it: the ownership checks read only its name,
// namespace, UID, and the kind the scheme resolves.
func finalizeOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "keystone", Namespace: "openstack", UID: "owner-uid"}}
}

// ownedBy marks obj as controlled by owner, the way the local provisioning
// path claims it.
func ownedBy(t *testing.T, owner, obj client.Object) client.Object {
	t.Helper()
	if err := controllerutil.SetControllerReference(owner, obj, newScheme()); err != nil {
		t.Fatalf("setting the controller reference: %v", err)
	}
	return obj
}

func TestFinalizeOwnedResources_deletesOwnedAndReportsRequest(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	owner := finalizeOwner()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		ownedBy(t, owner, namedDatabase()),
		ownedBy(t, owner, namedUser()),
		ownedBy(t, owner, namedGrant()),
	).Build()
	ctx := context.Background()

	requested, err := FinalizeOwnedResources(ctx, c, s, owner)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(requested).To(BeTrue())
	for _, obj := range []client.Object{&mariadbv1alpha1.Database{}, &mariadbv1alpha1.User{}, &mariadbv1alpha1.Grant{}} {
		g.Expect(apierrors.IsNotFound(c.Get(ctx, finalizeKey(), obj))).To(BeTrue(), "%T must be deleted", obj)
	}

	// Nothing is left, so a second pass requests nothing and the caller can
	// release its finalizer.
	requested, err = FinalizeOwnedResources(ctx, c, s, owner)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(requested).To(BeFalse())
}

// A Database nobody claimed and a User another controller owns share the
// owner's name. Neither belongs to the owner, so both survive.
func TestFinalizeOwnedResources_preservesForeignResources(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	owner := finalizeOwner()
	other := finalizeOwner()
	other.UID = "someone-else"
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(namedDatabase(), ownedBy(t, other, namedUser())).Build()
	ctx := context.Background()

	requested, err := FinalizeOwnedResources(ctx, c, s, owner)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(requested).To(BeFalse())

	var db mariadbv1alpha1.Database
	g.Expect(c.Get(ctx, finalizeKey(), &db)).To(Succeed())
	g.Expect(db.DeletionTimestamp.IsZero()).To(BeTrue())
	var user mariadbv1alpha1.User
	g.Expect(c.Get(ctx, finalizeKey(), &user)).To(Succeed())
	g.Expect(user.DeletionTimestamp.IsZero()).To(BeTrue())
}

func TestFinalizeOwnedResources_skipsTerminating(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	owner := finalizeOwner()
	// The fake client requires a finalizer for a deletion timestamp to persist.
	term := ownedBy(t, owner, namedGrant())
	term.SetFinalizers([]string{"mariadb.mmontes.io/finalizer"})
	deletes := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(term).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes++
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	ctx := context.Background()
	g.Expect(c.Delete(ctx, namedGrant())).To(Succeed())
	deletes = 0

	requested, err := FinalizeOwnedResources(ctx, c, s, owner)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(requested).To(BeFalse())
	g.Expect(deletes).To(BeZero(), "a terminating child must not be deleted again")
}

// On a target cluster a child carries the ownership labels instead of an owner
// reference. A same-named object without them is somebody else's.
func TestFinalizeOwnedResources_remoteMatchesOwnershipLabels(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	owner := finalizeOwner()
	labels, err := multicluster.OwnerLabels(s, owner)
	g.Expect(err).NotTo(HaveOccurred())
	labelled := namedDatabase()
	labelled.Labels = labels
	c := multicluster.Remote(fake.NewClientBuilder().WithScheme(s).
		WithObjects(labelled, namedUser()).Build())
	ctx := context.Background()

	requested, err := FinalizeOwnedResources(ctx, c, s, owner)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(requested).To(BeTrue())
	g.Expect(apierrors.IsNotFound(c.Get(ctx, finalizeKey(), &mariadbv1alpha1.Database{}))).To(BeTrue())
	var user mariadbv1alpha1.User
	g.Expect(c.Get(ctx, finalizeKey(), &user)).To(Succeed())
	g.Expect(user.DeletionTimestamp.IsZero()).To(BeTrue())
}

// The remote ownership labels name the owner's kind. A scheme that cannot
// resolve it fails the pass and deletes nothing.
func TestFinalizeOwnedResources_remoteUnresolvableOwnerKindFails(t *testing.T) {
	g := NewWithT(t)
	c := multicluster.Remote(fake.NewClientBuilder().WithScheme(newScheme()).
		WithObjects(namedDatabase()).Build())
	ctx := context.Background()

	requested, err := FinalizeOwnedResources(ctx, c, runtime.NewScheme(), finalizeOwner())
	g.Expect(err).To(HaveOccurred())
	g.Expect(requested).To(BeFalse())
	g.Expect(c.Get(ctx, finalizeKey(), &mariadbv1alpha1.Database{})).To(Succeed())
}

func TestFinalizeOwnedResources_propagatesGetError(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("get rejected")
			},
		}).Build()

	requested, err := FinalizeOwnedResources(context.Background(), c, s, finalizeOwner())
	g.Expect(err).To(MatchError("checking *v1alpha1.Database openstack/keystone: get rejected"))
	g.Expect(requested).To(BeFalse())
}

// A failure on the second kind still reports the deletion the first kind
// accepted, and leaves the kinds after it untouched.
func TestFinalizeOwnedResources_propagatesDeleteError(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	owner := finalizeOwner()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(
			ownedBy(t, owner, namedDatabase()),
			ownedBy(t, owner, namedUser()),
			ownedBy(t, owner, namedGrant()),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, isUser := obj.(*mariadbv1alpha1.User); isUser {
					return errors.New("delete rejected")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	ctx := context.Background()

	requested, err := FinalizeOwnedResources(ctx, c, s, owner)
	g.Expect(err).To(MatchError("deleting owned *v1alpha1.User openstack/keystone: delete rejected"))
	g.Expect(requested).To(BeTrue())
	g.Expect(c.Get(ctx, finalizeKey(), &mariadbv1alpha1.Grant{})).To(Succeed())
}

func TestFinalizeOwnedResources_toleratesDeleteNotFound(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	owner := finalizeOwner()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ownedBy(t, owner, namedDatabase())).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "databases"}, obj.GetName())
			},
		}).Build()

	requested, err := FinalizeOwnedResources(context.Background(), c, s, owner)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(requested).To(BeFalse())
}

// The delete is pinned to the UID and resourceVersion that passed the ownership
// check. An object replaced or re-claimed in between is rejected by the API
// server with a conflict, which fails the pass and leaves the object alone.
func TestFinalizeOwnedResources_pinsDeleteToTheObservedObject(t *testing.T) {
	g := NewWithT(t)
	s := newScheme()
	owner := finalizeOwner()
	db := ownedBy(t, owner, namedDatabase())
	db.SetUID("db-uid")
	var sent []client.DeleteOptions
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(db).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				var o client.DeleteOptions
				o.ApplyOptions(opts)
				sent = append(sent, o)
				return apierrors.NewConflict(schema.GroupResource{Resource: "databases"}, obj.GetName(),
					errors.New("the object was replaced"))
			},
		}).Build()
	ctx := context.Background()
	var observed mariadbv1alpha1.Database
	g.Expect(c.Get(ctx, finalizeKey(), &observed)).To(Succeed())

	requested, err := FinalizeOwnedResources(ctx, c, s, owner)
	g.Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)
	g.Expect(requested).To(BeFalse())
	g.Expect(sent).To(HaveLen(1))
	g.Expect(sent[0].Preconditions).NotTo(BeNil())
	g.Expect(sent[0].Preconditions.UID).To(HaveValue(Equal(observed.UID)))
	g.Expect(sent[0].Preconditions.ResourceVersion).To(HaveValue(Equal(observed.ResourceVersion)))
}
