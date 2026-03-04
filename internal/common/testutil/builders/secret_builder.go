// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package builders

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Feature: CC-0002

// SecretBuilder provides a fluent API for constructing corev1.Secret objects
// in tests.
type SecretBuilder struct {
	secret corev1.Secret
}

// NewSecretBuilder creates a new SecretBuilder with the given name and namespace.
func NewSecretBuilder(name, namespace string) *SecretBuilder {
	return &SecretBuilder{
		secret: corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Type: corev1.SecretTypeOpaque,
		},
	}
}

// WithLabels sets labels on the Secret. The provided map is copied to prevent
// the caller from mutating the builder's internal state.
func (b *SecretBuilder) WithLabels(labels map[string]string) *SecretBuilder {
	if labels == nil {
		b.secret.Labels = nil
		return b
	}
	copied := make(map[string]string, len(labels))
	for k, v := range labels {
		copied[k] = v
	}
	b.secret.Labels = copied
	return b
}

// WithAnnotations sets annotations on the Secret. The provided map is copied
// to prevent the caller from mutating the builder's internal state.
func (b *SecretBuilder) WithAnnotations(annotations map[string]string) *SecretBuilder {
	if annotations == nil {
		b.secret.Annotations = nil
		return b
	}
	copied := make(map[string]string, len(annotations))
	for k, v := range annotations {
		copied[k] = v
	}
	b.secret.Annotations = copied
	return b
}

// WithData adds a key-value pair to the Secret's Data field (raw bytes).
func (b *SecretBuilder) WithData(key string, value []byte) *SecretBuilder {
	if b.secret.Data == nil {
		b.secret.Data = make(map[string][]byte)
	}
	b.secret.Data[key] = value
	return b
}

// WithStringData adds a key-value pair to the Secret's StringData field.
func (b *SecretBuilder) WithStringData(key, value string) *SecretBuilder {
	if b.secret.StringData == nil {
		b.secret.StringData = make(map[string]string)
	}
	b.secret.StringData[key] = value
	return b
}

// WithType sets the Secret type (e.g., corev1.SecretTypeOpaque).
func (b *SecretBuilder) WithType(secretType corev1.SecretType) *SecretBuilder {
	b.secret.Type = secretType
	return b
}

// WithOwner sets an owner reference on the Secret.
func (b *SecretBuilder) WithOwner(owner metav1.Object, scheme *runtime.Scheme) *SecretBuilder {
	if err := controllerutil.SetOwnerReference(owner, &b.secret, scheme); err != nil {
		panic("builders: failed to set owner reference: " + err.Error())
	}
	return b
}

// Build returns the constructed corev1.Secret. It returns a deep copy to
// prevent mutation of the builder state.
func (b *SecretBuilder) Build() *corev1.Secret {
	return b.secret.DeepCopy()
}
