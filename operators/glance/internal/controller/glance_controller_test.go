// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the field-index extractors and registration helpers shared by
// the Glance and GlanceBackend controllers.
package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// recordingFieldIndexer is a client.FieldIndexer that records the keys it was
// asked to register, so the registration helpers can be exercised without a
// running manager.
type recordingFieldIndexer struct {
	keys []string
}

func (r *recordingFieldIndexer) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	r.keys = append(r.keys, field)
	return nil
}

func TestGlanceSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	// serviceUser + database Secret names, deduplicated.
	glance := testGlance()
	g.Expect(glanceSecretNameExtractor(glance)).To(ConsistOf("glance-service-user", "glance-db"))

	// The same Secret backing both references collapses to one entry.
	glance.Spec.Database.SecretRef.Name = "glance-service-user"
	g.Expect(glanceSecretNameExtractor(glance)).To(ConsistOf("glance-service-user"))

	// An empty database Secret name is skipped.
	glance.Spec.Database.SecretRef.Name = ""
	g.Expect(glanceSecretNameExtractor(glance)).To(ConsistOf("glance-service-user"))

	// The wrong object type yields nil rather than a panic.
	g.Expect(glanceSecretNameExtractor(&corev1.Secret{})).To(BeNil())
}

func TestGlanceBackendSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	// Credentials Secret only.
	b := testGlanceBackend("store", "test-glance")
	g.Expect(glanceBackendSecretNameExtractor(b)).To(ConsistOf("store-s3-creds"))

	// A nil S3 block (bypassed admission) indexes nothing.
	b.Spec.S3 = nil
	g.Expect(glanceBackendSecretNameExtractor(b)).To(BeEmpty())

	// The wrong object type yields nil rather than a panic.
	g.Expect(glanceBackendSecretNameExtractor(&corev1.Secret{})).To(BeNil())
}

func TestGlanceBackendGlanceRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	b := testGlanceBackend("store", "test-glance")
	g.Expect(glanceBackendGlanceRefExtractor(b)).To(ConsistOf("test-glance"))

	// An empty glanceRef (bypassed admission) indexes nothing.
	b.Spec.GlanceRef.Name = ""
	g.Expect(glanceBackendGlanceRefExtractor(b)).To(BeNil())

	// The wrong object type yields nil rather than a panic.
	g.Expect(glanceBackendGlanceRefExtractor(&corev1.Secret{})).To(BeNil())
}

func TestRegisterGlanceIndexes_RegistersSecretNameKey(t *testing.T) {
	g := NewGomegaWithT(t)

	idx := &recordingFieldIndexer{}
	g.Expect(registerGlanceIndexes(context.Background(), idx)).To(Succeed())
	g.Expect(idx.keys).To(ConsistOf(GlanceSecretNameIndexKey))
}

func TestRegisterGlanceBackendIndexes_RegistersBothKeys(t *testing.T) {
	g := NewGomegaWithT(t)

	idx := &recordingFieldIndexer{}
	g.Expect(registerGlanceBackendIndexes(context.Background(), idx)).To(Succeed())
	g.Expect(idx.keys).To(ConsistOf(GlanceBackendGlanceRefIndexKey, GlanceBackendSecretNameIndexKey))
}
