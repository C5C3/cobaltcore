// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
