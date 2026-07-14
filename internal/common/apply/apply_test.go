// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package apply

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       "test-uid",
		},
	}
}

func testConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "applied-cm",
			Namespace: "default",
		},
		Data: map[string]string{"key": "value"},
	}
}

func TestEnsureObject_createsWithOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	cm := testConfigMap()
	g.Expect(EnsureObject(context.Background(), c, s, owner, cm, FieldManager)).To(Succeed())

	// The caller's object is overwritten with the server response, so it carries
	// a resourceVersion after a successful apply.
	g.Expect(cm.ResourceVersion).NotTo(BeEmpty())

	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(cm), fetched)).To(Succeed())
	g.Expect(fetched.Data).To(HaveKeyWithValue("key", "value"))
	g.Expect(fetched.OwnerReferences).To(HaveLen(1))
	g.Expect(fetched.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(*fetched.OwnerReferences[0].Controller).To(BeTrue())
}

func TestEnsureObject_usesServerSideApplyOptions(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	var fieldManager string
	var forced bool
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				ao := &client.ApplyOptions{}
				ao.ApplyOptions(opts)
				fieldManager = ao.FieldManager
				forced = ao.Force != nil && *ao.Force
				return cl.Apply(ctx, obj, opts...)
			},
		}).
		Build()

	g.Expect(EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)).To(Succeed())
	g.Expect(fieldManager).To(Equal(FieldManager))
	g.Expect(forced).To(BeTrue(), "apply must set ForceOwnership")
}

func TestEnsureObject_retriesOnConflict(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	applyCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				applyCalls++
				if applyCalls == 1 {
					// A benign field-manager conflict must be retried, not surfaced.
					return apierrors.NewConflict(
						schema.GroupResource{Resource: "configmaps"}, "applied-cm",
						apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "applied-cm", nil),
					)
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).
		Build()

	g.Expect(EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)).To(Succeed())
	g.Expect(applyCalls).To(Equal(2), "conflict on the first apply must be retried once and then succeed")
}

func TestEnsureObject_propagatesApplyError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "applied-cm", nil)
			},
		}).
		Build()

	err := EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue())
}

func TestEnsureObject_errorsWhenGVKUnknown(t *testing.T) {
	g := NewGomegaWithT(t)
	// Scheme knows the owner (ConfigMap) but not the applied type (CronJob), so
	// GVK resolution for the applied object fails before any apply is attempted.
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	owner := testOwner()

	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(owner).Build()

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "cj", Namespace: "default"},
	}
	err := EnsureObject(context.Background(), c, s, owner, cronJob, FieldManager)
	g.Expect(err).To(HaveOccurred())
}

// TestEnsureUnownedObject_appliesWithoutOwnerReference pins the cross-namespace
// apply path: the object lands with no owner reference at all, so nothing
// garbage-collects it — the caller carries ownership via labels and an explicit
// teardown instead.
func TestEnsureUnownedObject_appliesWithoutOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()

	cm := testConfigMap()
	cm.Namespace = "elsewhere"
	g.Expect(EnsureUnownedObject(context.Background(), c, s, cm, FieldManager)).To(Succeed())

	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(cm), fetched)).To(Succeed())
	g.Expect(fetched.Data).To(HaveKeyWithValue("key", "value"))
	g.Expect(fetched.OwnerReferences).To(BeEmpty(), "a cross-namespace child must carry no owner reference")
}

// TestEnsureObject_rejectsCrossNamespaceOwner is the reason EnsureUnownedObject
// exists: Kubernetes garbage collection does not cascade across namespaces, so
// SetControllerReference refuses a foreign-namespace child — the apply never even
// reaches the API server.
func TestEnsureObject_rejectsCrossNamespaceOwner(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner() // namespace "default"

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	cm := testConfigMap()
	cm.Namespace = "elsewhere"
	err := EnsureObject(context.Background(), c, s, owner, cm, FieldManager)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("setting owner reference"))

	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(cm), &corev1.ConfigMap{})).NotTo(Succeed(),
		"nothing may be applied once the owner reference is refused")
}
