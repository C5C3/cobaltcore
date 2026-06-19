// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package secrets

import (
	"testing"

	. "github.com/onsi/gomega"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestIntegration_WaitForExternalSecret(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	_ = envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-secrets-wait"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-es",
			Namespace: ns.Name,
		},
		Spec: esov1.ExternalSecretSpec{
			SecretStoreRef: esov1.SecretStoreRef{
				Name: "test-store",
				Kind: "ClusterSecretStore",
			},
			Target: esov1.ExternalSecretTarget{
				Name: "target-secret",
			},
		},
	}
	g.Expect(c.Create(ctx, es)).To(Succeed())

	// Created but not ready initially: exists is true, ready is false.
	exists, ready, err := WaitForExternalSecret(ctx, c, client.ObjectKeyFromObject(es))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exists).To(BeTrue())
	g.Expect(ready).To(BeFalse())
}

func TestIntegration_IsSecretReady(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-secrets-ready"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Not found initially.
	ready, err := IsSecretReady(ctx, c, client.ObjectKey{Name: "missing-secret", Namespace: ns.Name})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	// Create secret with known keys.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("s3cret"),
		},
	}
	g.Expect(c.Create(ctx, secret)).To(Succeed())

	// Existence-only check.
	ready, err = IsSecretReady(ctx, c, client.ObjectKeyFromObject(secret))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())

	// All expected keys present.
	ready, err = IsSecretReady(ctx, c, client.ObjectKeyFromObject(secret), "username", "password")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())

	// Missing expected key.
	ready, err = IsSecretReady(ctx, c, client.ObjectKeyFromObject(secret), "username", "missing-key")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestIntegration_GetSecretValue(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-secrets-getval"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "creds-secret",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"password": []byte("s3cret"),
			"username": []byte("admin"),
		},
	}
	g.Expect(c.Create(ctx, secret)).To(Succeed())

	val, err := GetSecretValue(ctx, c, client.ObjectKeyFromObject(secret), "password")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(Equal("s3cret"))
}

func TestIntegration_EnsurePushSecret(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-secrets-ps"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ps-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	ps := validPushSecret("integration-ps", ns.Name)

	// Create.
	g.Expect(EnsurePushSecret(ctx, c, scheme, owner, ps)).To(Succeed())

	created := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(ps), created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("ps-owner"))
	g.Expect(created.Spec.SecretStoreRefs).To(HaveLen(1))

	// A second apply of the unchanged desired PushSecret must not rewrite it, so
	// ESO is not woken to re-push an unchanged credential.
	g.Expect(EnsurePushSecret(ctx, c, scheme, owner, validPushSecret("integration-ps", ns.Name))).To(Succeed())
	reapplied := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(ps), reapplied)).To(Succeed())
	g.Expect(reapplied.ResourceVersion).To(Equal(created.ResourceVersion),
		"converged PushSecret must not be rewritten on a repeated apply")
}

// validPushSecret returns a schema-valid PushSecret mirroring the production
// builders (a ClusterSecretStore ref, a source Secret selector, and one data
// match), so the apply is accepted by the API server.
func validPushSecret(name, namespace string) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: esov1alpha1.PushSecretSpec{
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{{
				Kind: "ClusterSecretStore",
				Name: "openbao-cluster-store",
			}},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{Name: name + "-source"},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{RemoteKey: "openstack/" + name},
				},
			}},
		},
	}
}

func TestIntegration_EnsurePushSecret_idempotent(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-secrets-ps-idem"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ps-owner-idem", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	g.Expect(EnsurePushSecret(ctx, c, scheme, owner, validPushSecret("idem-ps", ns.Name))).To(Succeed())
	g.Expect(EnsurePushSecret(ctx, c, scheme, owner, validPushSecret("idem-ps", ns.Name))).To(Succeed())

	list := &esov1alpha1.PushSecretList{}
	g.Expect(c.List(ctx, list, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}
