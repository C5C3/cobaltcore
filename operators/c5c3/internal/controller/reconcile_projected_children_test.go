// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the shared prune and sweep over projected satellite children
// (pruneProjectedChildren, sweepProjectedChildren), exercised through the
// GlanceBackend target glanceBackendChildren builds.
package controller

import (
	"context"
	"errors"
	"testing"

	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// projectedChildrenNamespace is the service namespace every fixture below lives in.
const projectedChildrenNamespace = "images"

// glanceBackendGroupResource identifies the GlanceBackend resource in the API
// errors the interceptors return.
var glanceBackendGroupResource = schema.GroupResource{
	Group: glancev1alpha1.GroupVersion.Group, Resource: "glancebackends",
}

// ownedGlanceBackend returns a projected GlanceBackend child of cp: it carries
// cp's ownership labels (a cross-namespace child cannot carry an owner reference)
// and the glance child's name prefix, so both halves of the ownership-and-prefix
// test pass.
func ownedGlanceBackend(cp *c5c3v1alpha1.ControlPlane, entryName string) *glancev1alpha1.GlanceBackend {
	return &glancev1alpha1.GlanceBackend{
		ObjectMeta: metav1.ObjectMeta{
			Name:      glanceBackendName(cp, entryName),
			Namespace: projectedChildrenNamespace,
			Labels:    controlPlaneChildLabels(cp),
		},
	}
}

// glanceBackendNoKindMatch is the List error the API server surfaces once the
// GlanceBackend CRD is uninstalled.
func glanceBackendNoKindMatch() error {
	return &meta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: glancev1alpha1.GroupVersion.Group, Kind: "GlanceBackend"},
	}
}

// failingGlanceBackendList returns interceptors whose List of GlanceBackends
// fails with listErr; every other List reaches the fake client.
func failingGlanceBackendList(listErr error) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*glancev1alpha1.GlanceBackendList); ok {
				return listErr
			}
			return c.List(ctx, list, opts...)
		},
	}
}

// failingGlanceBackendDelete returns interceptors whose Delete of a GlanceBackend
// fails with deleteErr; every other Delete reaches the fake client.
func failingGlanceBackendDelete(deleteErr error) interceptor.Funcs {
	return interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*glancev1alpha1.GlanceBackend); ok {
				return deleteErr
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
}

// TestPruneProjectedChildren pins the reconcile-time prune: it reaches exactly the
// children this ControlPlane owns whose name carries the projection prefix and
// that no Keep entry names, and it reports the phase and the child in its errors.
func TestPruneProjectedChildren(t *testing.T) {
	t.Run("deletes an owned prefixed child the Keep set does not name", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		stale := ownedGlanceBackend(cp, "stale")
		declared := ownedGlanceBackend(cp, "primary")
		// A hand-created backend sharing the namespace and the prefix but owned by nobody.
		foreign := &glancev1alpha1.GlanceBackend{
			ObjectMeta: metav1.ObjectMeta{
				Name: glanceBackendName(cp, "byo"), Namespace: projectedChildrenNamespace,
			},
		}
		// An owned backend outside the projection's naming: not ours to prune.
		unprefixed := &glancev1alpha1.GlanceBackend{
			ObjectMeta: metav1.ObjectMeta{
				Name: "other-backend", Namespace: projectedChildrenNamespace,
				Labels: controlPlaneChildLabels(cp),
			},
		}
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(cp, stale, declared, foreign, unprefixed).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		keep := map[string]struct{}{declared.Name: {}}
		err := r.pruneProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, keep))
		g.Expect(err).NotTo(HaveOccurred())

		expectSwept(t, c, stale)
		expectPresent(t, c, declared, foreign, unprefixed)
	})

	t.Run("an empty namespace is nothing to prune", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		g.Expect(r.pruneProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, nil))).To(Succeed(),
			"a nil Keep set over an empty list must not error")
	})

	t.Run("a List error names the prune phase and the namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithInterceptorFuncs(failingGlanceBackendList(apierrors.NewInternalError(errors.New("boom")))).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		err := r.pruneProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, nil))
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(HavePrefix(`listing GlanceBackends in "images" for prune: `))
		g.Expect(err.Error()).To(ContainSubstring("boom"))
	})

	t.Run("a Delete error names the child", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		stale := ownedGlanceBackend(cp, "stale")
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, stale).
			WithInterceptorFuncs(failingGlanceBackendDelete(apierrors.NewInternalError(errors.New("boom")))).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		err := r.pruneProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, nil))
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(HavePrefix("pruning undeclared GlanceBackend images/" + stale.Name + ": "))
		g.Expect(err.Error()).To(ContainSubstring("boom"))
	})

	t.Run("a child deleted underneath the prune is tolerated", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		stale := ownedGlanceBackend(cp, "stale")
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, stale).
			WithInterceptorFuncs(failingGlanceBackendDelete(
				apierrors.NewNotFound(glanceBackendGroupResource, stale.Name))).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		g.Expect(r.pruneProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, nil))).To(Succeed(),
			"a NotFound Delete means the child is already gone")
	})
}

// TestSweepProjectedChildren pins the teardown shape: every owned, prefixed child
// is deleted once and reported as remaining until it disappears, an already
// Terminating child is reported without a second Delete, and an uninstalled CRD is
// nothing to sweep rather than an error that wedges the teardown.
func TestSweepProjectedChildren(t *testing.T) {
	t.Run("deletes an owned prefixed child and reports it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		owned := ownedGlanceBackend(cp, "primary")
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, owned).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		remaining, err := r.sweepProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, nil))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remaining).To(Equal([]string{"images/" + owned.Name}))
		expectSwept(t, c, owned)
	})

	t.Run("reports an already Terminating child without deleting it again", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		// The fake client accepts a Terminating fixture only with a finalizer holding
		// it in place, which is also what keeps it listable here.
		terminating := ownedGlanceBackend(cp, "primary")
		terminating.DeletionTimestamp = ptr.To(metav1.Now())
		terminating.Finalizers = []string{"glance.openstack.c5c3.io/cleanup"}

		var deleted []string
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, terminating).
			WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deleted = append(deleted, obj.GetName())
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		remaining, err := r.sweepProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, nil))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remaining).To(Equal([]string{"images/" + terminating.Name}),
			"a Terminating child is still reported so the finalizer waits for it")
		g.Expect(deleted).To(BeEmpty(), "a child already Terminating must not be deleted again")
	})

	t.Run("leaves a Keep name, a foreign child and an unprefixed child alone", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		kept := ownedGlanceBackend(cp, "primary")
		foreign := &glancev1alpha1.GlanceBackend{
			ObjectMeta: metav1.ObjectMeta{
				Name: glanceBackendName(cp, "byo"), Namespace: projectedChildrenNamespace,
			},
		}
		unprefixed := &glancev1alpha1.GlanceBackend{
			ObjectMeta: metav1.ObjectMeta{
				Name: "other-backend", Namespace: projectedChildrenNamespace,
				Labels: controlPlaneChildLabels(cp),
			},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, kept, foreign, unprefixed).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		remaining, err := r.sweepProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, map[string]struct{}{kept.Name: {}}))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remaining).To(BeNil(), "nothing sweepable must report nothing to wait for")
		expectPresent(t, c, kept, foreign, unprefixed)
	})

	t.Run("an uninstalled CRD is nothing to sweep", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithInterceptorFuncs(failingGlanceBackendList(glanceBackendNoKindMatch())).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		remaining, err := r.sweepProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, nil))
		g.Expect(err).NotTo(HaveOccurred(), "an absent CRD must not wedge the teardown")
		g.Expect(remaining).To(BeNil())
	})

	t.Run("any other List error names the teardown phase and the namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithInterceptorFuncs(failingGlanceBackendList(apierrors.NewInternalError(errors.New("boom")))).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		remaining, err := r.sweepProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, nil))
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(HavePrefix(`listing GlanceBackends in "images" for cross-namespace teardown: `))
		g.Expect(err.Error()).To(ContainSubstring("boom"))
		g.Expect(remaining).To(BeNil())
	})

	t.Run("a Delete error names the child", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		owned := ownedGlanceBackend(cp, "primary")
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, owned).
			WithInterceptorFuncs(failingGlanceBackendDelete(apierrors.NewInternalError(errors.New("boom")))).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		remaining, err := r.sweepProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, nil))
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(HavePrefix("deleting GlanceBackend images/" + owned.Name + ": "))
		g.Expect(err.Error()).To(ContainSubstring("boom"))
		g.Expect(remaining).To(BeNil())
	})

	t.Run("nothing owned reports nothing to wait for", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespaceTeardownScheme(t)
		cp := korcControlPlane()

		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		remaining, err := r.sweepProjectedChildren(context.Background(), cp,
			glanceBackendChildren(cp, projectedChildrenNamespace, nil))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remaining).To(BeNil())
	})
}
