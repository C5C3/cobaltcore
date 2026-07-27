// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// testHook mirrors the shape every operator webhook has: its own defaulting
// and create/update validation, with the shared no-op delete validation
// embedded.
type testHook struct {
	NoopDeleteValidator[*corev1.ConfigMap]
}

var (
	_ admission.Defaulter[*corev1.ConfigMap] = &testHook{}
	_ admission.Validator[*corev1.ConfigMap] = &testHook{}
)

func (h *testHook) Default(_ context.Context, _ *corev1.ConfigMap) error { return nil }

func (h *testHook) ValidateCreate(_ context.Context, _ *corev1.ConfigMap) (admission.Warnings, error) {
	return nil, nil
}

func (h *testHook) ValidateUpdate(_ context.Context, _, _ *corev1.ConfigMap) (admission.Warnings, error) {
	return nil, nil
}

func TestNoopDeleteValidator_ReturnsNilNil(t *testing.T) {
	g := gomega.NewWithT(t)

	warnings, err := NoopDeleteValidator[*corev1.ConfigMap]{}.ValidateDelete(t.Context(), &corev1.ConfigMap{})

	g.Expect(warnings).To(gomega.BeNil())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}
