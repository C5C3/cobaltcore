// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package secrets

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esov1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// Feature: CC-0005

func helperCreateNamespace(ctx context.Context, g *GomegaWithT, c client.Client, name string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	return ns
}

func helperCreateOwnerConfigMap(ctx context.Context, g *GomegaWithT, c client.Client, name, namespace string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())
	return cm
}

// --- WaitForExternalSecret ---

func TestIntegration_WaitForExternalSecret_NotReady(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := helperCreateNamespace(ctx, g, c, "test-es-notready-ns")

	es := &esov1beta1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-es",
			Namespace: ns.Name,
		},
	}
	g.Expect(c.Create(ctx, es)).To(Succeed())

	ready, err := WaitForExternalSecret(ctx, c, ns.Name, "my-es")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created ExternalSecret should not be ready")
}

func TestIntegration_WaitForExternalSecret_Ready(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := helperCreateNamespace(ctx, g, c, "test-es-ready-ns")

	es := &esov1beta1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-es",
			Namespace: ns.Name,
		},
	}
	g.Expect(c.Create(ctx, es)).To(Succeed())

	// Simulate sync by updating the status.
	es.Status.Conditions = append(es.Status.Conditions, esov1beta1.ExternalSecretStatusCondition{
		Type:   esov1beta1.ExternalSecretReady,
		Status: corev1.ConditionTrue,
		Reason: "SecretSynced",
	})
	g.Expect(c.Status().Update(ctx, es)).To(Succeed())

	ready, err := WaitForExternalSecret(ctx, c, ns.Name, "my-es")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue(), "ExternalSecret with Ready=True should be ready")
}

// --- IsSecretReady ---

func TestIntegration_IsSecretReady_NotFound(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := helperCreateNamespace(ctx, g, c, "test-secret-notfound-ns")

	ready, err := IsSecretReady(ctx, c, ns.Name, "missing-secret")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestIntegration_IsSecretReady_Exists(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := helperCreateNamespace(ctx, g, c, "test-secret-exists-ns")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: ns.Name,
		},
	}
	g.Expect(c.Create(ctx, secret)).To(Succeed())

	ready, err := IsSecretReady(ctx, c, ns.Name, "my-secret")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

// --- GetSecretValue ---

func TestIntegration_GetSecretValue_ReadsValue(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := helperCreateNamespace(ctx, g, c, "test-secretval-ns")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"password": []byte("s3cret"),
		},
	}
	g.Expect(c.Create(ctx, secret)).To(Succeed())

	val, err := GetSecretValue(ctx, c, ns.Name, "my-secret", "password")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(Equal("s3cret"))
}

// --- EnsurePushSecret ---

func TestIntegration_EnsurePushSecret_CreatesWithOwnerRef(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := helperCreateNamespace(ctx, g, c, "test-pushsecret-ns")
	owner := helperCreateOwnerConfigMap(ctx, g, c, "ps-owner", ns.Name)

	desired := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ps",
			Namespace: ns.Name,
		},
		Spec: esov1alpha1.PushSecretSpec{
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{
				{Name: "my-store"},
			},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{Name: "source-secret"},
			},
		},
	}

	g.Expect(EnsurePushSecret(ctx, c, owner, desired)).To(Succeed())

	// Verify the PushSecret exists in the cluster.
	fetched := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-ps", Namespace: ns.Name}, fetched)).To(Succeed())

	// Verify spec.
	g.Expect(fetched.Spec.SecretStoreRefs).To(HaveLen(1))
	g.Expect(fetched.Spec.SecretStoreRefs[0].Name).To(Equal("my-store"))

	// Verify owner reference.
	g.Expect(fetched.OwnerReferences).NotTo(BeEmpty())
	g.Expect(fetched.OwnerReferences[0].UID).To(Equal(owner.UID))
	g.Expect(fetched.OwnerReferences[0].Name).To(Equal(owner.Name))
}
