// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package builders

import (
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

func TestNewSecretBuilder_basic(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := NewSecretBuilder("my-secret", "default").Build()

	g.Expect(secret.Name).To(Equal("my-secret"))
	g.Expect(secret.Namespace).To(Equal("default"))
}

func TestNewSecretBuilder_defaultTypeIsOpaque(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := NewSecretBuilder("my-secret", "default").Build()

	g.Expect(secret.Type).To(Equal(corev1.SecretTypeOpaque))
}

func TestSecretBuilder_WithData(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := NewSecretBuilder("my-secret", "default").
		WithData("key", []byte("value")).
		Build()

	g.Expect(secret.Data).To(HaveKeyWithValue("key", []byte("value")))
}

func TestSecretBuilder_WithData_initializesMap(t *testing.T) {
	g := NewGomegaWithT(t)
	b := NewSecretBuilder("my-secret", "default")

	// Calling WithData on a fresh builder should not panic.
	secret := b.WithData("first", []byte("one")).Build()

	g.Expect(secret.Data).NotTo(BeNil())
	g.Expect(secret.Data).To(HaveKeyWithValue("first", []byte("one")))
}

func TestSecretBuilder_WithStringData(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := NewSecretBuilder("my-secret", "default").
		WithStringData("username", "admin").
		Build()

	g.Expect(secret.StringData).To(HaveKeyWithValue("username", "admin"))
}

func TestSecretBuilder_WithLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	labels := map[string]string{"app": "test", "env": "dev"}
	secret := NewSecretBuilder("my-secret", "default").
		WithLabels(labels).
		Build()

	g.Expect(secret.Labels).To(HaveKeyWithValue("app", "test"))
	g.Expect(secret.Labels).To(HaveKeyWithValue("env", "dev"))
}

func TestSecretBuilder_WithAnnotations(t *testing.T) {
	g := NewGomegaWithT(t)
	annotations := map[string]string{"note": "important"}
	secret := NewSecretBuilder("my-secret", "default").
		WithAnnotations(annotations).
		Build()

	g.Expect(secret.Annotations).To(HaveKeyWithValue("note", "important"))
}

func TestSecretBuilder_WithLabels_copiesMap(t *testing.T) {
	g := NewGomegaWithT(t)
	labels := map[string]string{"app": "test"}
	b := NewSecretBuilder("my-secret", "default").WithLabels(labels)

	// Mutate the caller's map after passing it to WithLabels.
	labels["app"] = "mutated"

	secret := b.Build()
	g.Expect(secret.Labels).To(HaveKeyWithValue("app", "test"),
		"WithLabels should copy the map to prevent external mutation")
}

func TestSecretBuilder_WithAnnotations_copiesMap(t *testing.T) {
	g := NewGomegaWithT(t)
	annotations := map[string]string{"note": "original"}
	b := NewSecretBuilder("my-secret", "default").WithAnnotations(annotations)

	// Mutate the caller's map after passing it to WithAnnotations.
	annotations["note"] = "mutated"

	secret := b.Build()
	g.Expect(secret.Annotations).To(HaveKeyWithValue("note", "original"),
		"WithAnnotations should copy the map to prevent external mutation")
}

func TestSecretBuilder_WithLabels_nilClearsLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := NewSecretBuilder("my-secret", "default").
		WithLabels(map[string]string{"app": "test"}).
		WithLabels(nil).
		Build()

	g.Expect(secret.Labels).To(BeNil())
}

func TestSecretBuilder_WithType(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := NewSecretBuilder("my-secret", "default").
		WithType(corev1.SecretTypeTLS).
		Build()

	g.Expect(secret.Type).To(Equal(corev1.SecretTypeTLS))
}

func TestSecretBuilder_chaining(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := NewSecretBuilder("chained", "ns").
		WithLabels(map[string]string{"app": "test"}).
		WithAnnotations(map[string]string{"note": "yes"}).
		WithData("bin", []byte("data")).
		WithStringData("text", "hello").
		WithType(corev1.SecretTypeDockerConfigJson).
		Build()

	g.Expect(secret.Name).To(Equal("chained"))
	g.Expect(secret.Namespace).To(Equal("ns"))
	g.Expect(secret.Labels).To(HaveKeyWithValue("app", "test"))
	g.Expect(secret.Annotations).To(HaveKeyWithValue("note", "yes"))
	g.Expect(secret.Data).To(HaveKeyWithValue("bin", []byte("data")))
	g.Expect(secret.StringData).To(HaveKeyWithValue("text", "hello"))
	g.Expect(secret.Type).To(Equal(corev1.SecretTypeDockerConfigJson))
}

func TestSecretBuilder_WithOwner(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owner-cm",
			Namespace: "default",
			UID:       "test-uid-12345",
		},
	}

	secret := NewSecretBuilder("owned-secret", "default").
		WithOwner(owner, scheme).
		Build()

	g.Expect(secret.OwnerReferences).To(HaveLen(1))
	g.Expect(secret.OwnerReferences[0].Name).To(Equal("owner-cm"))
	g.Expect(secret.OwnerReferences[0].UID).To(BeEquivalentTo("test-uid-12345"))
}

func TestSecretBuilder_WithOwner_panicsOnInvalidOwner(t *testing.T) {
	g := NewGomegaWithT(t)
	// An empty scheme has no GVK mappings, so SetOwnerReference will fail.
	emptyScheme := runtime.NewScheme()

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owner-cm",
			Namespace: "default",
			UID:       "test-uid",
		},
	}

	g.Expect(func() {
		NewSecretBuilder("owned-secret", "default").
			WithOwner(owner, emptyScheme)
	}).To(Panic())
}

func TestSecretBuilder_Build_returnsIndependentCopies(t *testing.T) {
	g := NewGomegaWithT(t)
	b := NewSecretBuilder("my-secret", "default").
		WithData("key", []byte("original"))

	s1 := b.Build()
	s2 := b.Build()

	// Mutate s1 and verify s2 is unaffected.
	s1.Data["key"] = []byte("mutated")

	g.Expect(s2.Data).To(HaveKeyWithValue("key", []byte("original")),
		"Build should return independent deep copies")
}
