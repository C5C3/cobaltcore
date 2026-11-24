// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/testutil"
)

const testFinalizer = "example.c5c3.io/cleanup"

func TestEnsureFinalizer_AddsAndPersists(t *testing.T) {
	g := gomega.NewWithT(t)

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cr", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(cm).Build()

	added, err := EnsureFinalizer(context.Background(), c, cm, testFinalizer)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(added).To(gomega.BeTrue())

	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(cm), fetched)).To(gomega.Succeed())
	g.Expect(fetched.Finalizers).To(gomega.ContainElement(testFinalizer),
		"the finalizer must be persisted, not only set in memory")

	// Second call: already present, no write, added=false.
	added, err = EnsureFinalizer(context.Background(), c, fetched, testFinalizer)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(added).To(gomega.BeFalse())
}

// A failed Update (e.g. a resourceVersion conflict) must surface as an error
// attributed to the finalizer, never as a silent added=false.
func TestEnsureFinalizer_UpdateErrorSurfaces(t *testing.T) {
	g := gomega.NewWithT(t)

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cr", Namespace: "ns"}}
	updateErr := fmt.Errorf("simulated conflict")
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(cm).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return updateErr
			},
		}).Build()

	added, err := EnsureFinalizer(context.Background(), c, cm, testFinalizer)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(testFinalizer)))
	g.Expect(added).To(gomega.BeFalse())
}

// keepFinalizer is a second finalizer the RemoveFinalizer tests leave on the
// object, so the fake keeps it after the release instead of deleting it.
const keepFinalizer = "example.c5c3.io/keep"

// finalizedConfigMap persists a ConfigMap carrying finalizers and returns a
// fake client that runs funcs and counts its Updates in the returned pointer.
func finalizedConfigMap(t *testing.T, funcs interceptor.Funcs, finalizers ...string) (client.Client, *corev1.ConfigMap, *int) {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cr", Namespace: "ns", Finalizers: finalizers}}
	updates := 0
	next := funcs.Update
	funcs.Update = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		updates++
		if next != nil {
			return next(ctx, c, obj, opts...)
		}
		return c.Update(ctx, obj, opts...)
	}
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(cm).
		WithInterceptorFuncs(funcs).Build()
	fetched := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cm), fetched); err != nil {
		t.Fatalf("reading the seeded ConfigMap: %v", err)
	}
	return c, fetched, &updates
}

// persistedFinalizers reads the stored object and returns its finalizers.
func persistedFinalizers(g *gomega.WithT, c client.Client, cm *corev1.ConfigMap) []string {
	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(cm), fetched)).To(gomega.Succeed())
	return fetched.Finalizers
}

func TestRemoveFinalizer_RemovesAndPersists(t *testing.T) {
	g := gomega.NewWithT(t)
	c, cm, _ := finalizedConfigMap(t, interceptor.Funcs{}, testFinalizer, keepFinalizer)

	outcome, err := RemoveFinalizer(context.Background(), c, cm, testFinalizer)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(outcome).To(gomega.Equal(FinalizerRemoved))
	g.Expect(persistedFinalizers(g, c, cm)).To(gomega.Equal([]string{keepFinalizer}),
		"the removal must be persisted, not only made in memory")
}

func TestRemoveFinalizer_RemovesSeveralInOneUpdate(t *testing.T) {
	g := gomega.NewWithT(t)
	const second = "example.c5c3.io/second"
	c, cm, updates := finalizedConfigMap(t, interceptor.Funcs{}, testFinalizer, second, keepFinalizer)

	outcome, err := RemoveFinalizer(context.Background(), c, cm, testFinalizer, second)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(outcome).To(gomega.Equal(FinalizerRemoved))
	g.Expect(*updates).To(gomega.Equal(1), "both finalizers must go in one Update")
	g.Expect(persistedFinalizers(g, c, cm)).To(gomega.Equal([]string{keepFinalizer}))
}

// An object that carries none of the named finalizers has nothing to release,
// so RemoveFinalizer issues no Update that could fail on a stale copy.
func TestRemoveFinalizer_AbsentFinalizerWritesNothing(t *testing.T) {
	g := gomega.NewWithT(t)
	c, cm, updates := finalizedConfigMap(t, testutil.NotFoundOnUpdate(), keepFinalizer)

	outcome, err := RemoveFinalizer(context.Background(), c, cm, testFinalizer)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(outcome).To(gomega.Equal(FinalizerRemoved))
	g.Expect(*updates).To(gomega.BeZero())
}

func TestRemoveFinalizer_NoFinalizerNamedWritesNothing(t *testing.T) {
	g := gomega.NewWithT(t)
	c, cm, updates := finalizedConfigMap(t, testutil.NotFoundOnUpdate(), testFinalizer)

	outcome, err := RemoveFinalizer(context.Background(), c, cm)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(outcome).To(gomega.Equal(FinalizerRemoved))
	g.Expect(*updates).To(gomega.BeZero())
	g.Expect(cm.Finalizers).To(gomega.Equal([]string{testFinalizer}))
}

// A release that finds the object gone, because an earlier pass released it
// and the cache still held the old copy, has nothing left to do: it ends
// without an error and without a line at the default log level.
func TestRemoveFinalizer_NotFoundReportsObjectGone(t *testing.T) {
	g := gomega.NewWithT(t)
	c, cm, _ := finalizedConfigMap(t, testutil.NotFoundOnUpdate(), testFinalizer)
	ctx, logs := contextWithLogCapture()

	outcome, err := RemoveFinalizer(ctx, c, cm, testFinalizer)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(outcome).To(gomega.Equal(FinalizerObjectGone))
	g.Expect(*logs).To(gomega.BeEmpty())
}

// A release on a stale copy loses to the stored object: nothing is written,
// the finalizer stays persisted, and one Info line records the conflict.
func TestRemoveFinalizer_ConflictReportsConflict(t *testing.T) {
	g := gomega.NewWithT(t)
	c, cm, _ := finalizedConfigMap(t, testutil.ConflictFirstUpdate(), testFinalizer)
	ctx, logs := contextWithLogCapture()

	outcome, err := RemoveFinalizer(ctx, c, cm, testFinalizer)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(outcome).To(gomega.Equal(FinalizerConflict))
	g.Expect(persistedFinalizers(g, c, cm)).To(gomega.Equal([]string{testFinalizer}))
	g.Expect(*logs).To(gomega.ConsistOf(gomega.And(
		gomega.ContainSubstring(`"msg"="finalizer removal conflict, requeueing"`),
		gomega.ContainSubstring(`"level"=0`))))
}

func TestRemoveFinalizer_OtherErrorIsReturnedUnwrapped(t *testing.T) {
	g := gomega.NewWithT(t)
	injected := apierrors.NewInternalError(errors.New("etcd unavailable"))
	c, cm, _ := finalizedConfigMap(t, interceptor.Funcs{
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return injected
		},
	}, testFinalizer)
	ctx, logs := contextWithLogCapture()

	outcome, err := RemoveFinalizer(ctx, c, cm, testFinalizer)

	g.Expect(outcome).To(gomega.Equal(FinalizerRemovalFailed))
	g.Expect(errors.Is(err, injected)).To(gomega.BeTrue())
	g.Expect(err.Error()).To(gomega.Equal(injected.Error()),
		"the error must come back unwrapped, so the caller's wrapper is its only prefix")
	g.Expect(*logs).To(gomega.BeEmpty())
}
