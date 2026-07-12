// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

func TestEffectiveStoreRef_NilDefaultsToClusterStore(t *testing.T) {
	g := gomega.NewWithT(t)
	got := EffectiveStoreRef(nil)
	g.Expect(got.Kind).To(gomega.Equal(commonv1.SecretStoreKindCluster))
	g.Expect(got.Name).To(gomega.Equal(OpenBaoClusterStoreName))
}

func TestEffectiveStoreRef_EmptyKindDefaultsToClusterKind(t *testing.T) {
	g := gomega.NewWithT(t)
	// A ref persisted without a kind (webhook-bypass) must still resolve to the
	// cluster kind rather than the empty string.
	got := EffectiveStoreRef(&commonv1.SecretStoreRefSpec{Name: "some-store"})
	g.Expect(got.Kind).To(gomega.Equal(commonv1.SecretStoreKindCluster))
	g.Expect(got.Name).To(gomega.Equal("some-store"))
}

func TestEffectiveStoreRef_ExplicitNamespacedPassesThrough(t *testing.T) {
	g := gomega.NewWithT(t)
	got := EffectiveStoreRef(&commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced,
		Name: "openbao-tenant-store",
	})
	g.Expect(got.Kind).To(gomega.Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(got.Name).To(gomega.Equal("openbao-tenant-store"))
}

func TestIsStoreRefReady_DispatchesClusterKind(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newScheme()
	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-cluster-store"},
		Status: esov1.SecretStoreStatus{Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(store).WithStatusSubresource(store).Build()

	ref := commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindCluster, Name: "openbao-cluster-store"}
	ready, err := IsStoreRefReady(context.Background(), c, ref, "ignored-namespace")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ready).To(gomega.BeTrue())
}

func TestIsStoreRefReady_DispatchesNamespacedKind(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newScheme()
	store := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "tenant-a"},
		Status: esov1.SecretStoreStatus{Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(store).WithStatusSubresource(store).Build()

	ref := commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store"}

	// Resolved in the CR's own namespace: found and ready in tenant-a.
	ready, err := IsStoreRefReady(context.Background(), c, ref, "tenant-a")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ready).To(gomega.BeTrue())

	// The same-named store does not exist in a foreign namespace, so the gate
	// treats it as not-ready rather than reaching across namespaces.
	ready, err = IsStoreRefReady(context.Background(), c, ref, "tenant-b")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ready).To(gomega.BeFalse())
}

func TestIsStoreRefReady_UnknownKindErrors(t *testing.T) {
	g := gomega.NewWithT(t)
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()

	ref := commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreRefKind("Bogus"), Name: "x"}
	ready, err := IsStoreRefReady(context.Background(), c, ref, "ns")
	// An unknown kind must surface an error, never silently fall back to a store
	// the caller did not select.
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(ready).To(gomega.BeFalse())
}

func TestESOSecretStoreRef_MapsKindAndName(t *testing.T) {
	g := gomega.NewWithT(t)
	got := ESOSecretStoreRef(commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	})
	g.Expect(got.Kind).To(gomega.Equal("SecretStore"))
	g.Expect(got.Name).To(gomega.Equal("openbao-tenant-store"))
}

func TestPushSecretStoreRefs_SingleElementMapsKindAndName(t *testing.T) {
	g := gomega.NewWithT(t)
	got := PushSecretStoreRefs(commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindCluster, Name: "openbao-cluster-store",
	})
	g.Expect(got).To(gomega.HaveLen(1))
	g.Expect(got[0].Kind).To(gomega.Equal("ClusterSecretStore"))
	g.Expect(got[0].Name).To(gomega.Equal("openbao-cluster-store"))
}
