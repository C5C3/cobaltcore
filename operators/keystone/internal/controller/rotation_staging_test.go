// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the rotation staging helpers.
package controller

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// TestRotationCompletedAt covers the pure annotation parser threaded through
// observeRotationAge: a nil Secret, a missing annotation, and a malformed
// timestamp are all clean "no value" observations, while a valid RFC3339
// annotation parses back to the stamped instant.
func TestRotationCompletedAt(t *testing.T) {
	g := NewGomegaWithT(t)

	// Nil Secret → no value (the fallback Secret may be nil).
	_, ok := rotationCompletedAt(nil)
	g.Expect(ok).To(BeFalse(), "nil secret must yield no value")

	// Missing annotation → no value.
	_, ok = rotationCompletedAt(&corev1.Secret{})
	g.Expect(ok).To(BeFalse(), "absent annotation must yield no value")

	// Malformed timestamp → no value (best-effort gauge, never an error).
	_, ok = rotationCompletedAt(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{RotationCompletedAnnotation: "not-a-timestamp"}},
	})
	g.Expect(ok).To(BeFalse(), "malformed timestamp must yield no value")

	// Valid RFC3339 → parsed instant.
	stamp := "2026-06-01T12:34:56Z"
	got, ok := rotationCompletedAt(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{RotationCompletedAnnotation: stamp}},
	})
	g.Expect(ok).To(BeTrue())
	want, _ := time.Parse(time.RFC3339, stamp)
	g.Expect(got).To(Equal(want))
}

func TestFernetStagingSecretName_CC0081(t *testing.T) {
	cases := []struct {
		name     string
		ksName   string
		expected string
	}{
		{
			name:     "short name",
			ksName:   "ks",
			expected: "ks-fernet-keys-rotation",
		},
		{
			name:     "conventional name",
			ksName:   "test-keystone",
			expected: "test-keystone-fernet-keys-rotation",
		},
		{
			name:     "long name with multiple dashes",
			ksName:   "openstack-control-plane-keystone",
			expected: "openstack-control-plane-keystone-fernet-keys-rotation",
		},
		{
			name:     "numeric suffix",
			ksName:   "keystone-42",
			expected: "keystone-42-fernet-keys-rotation",
		},
		{
			name:     "single character name",
			ksName:   "k",
			expected: "k-fernet-keys-rotation",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := &keystonev1alpha1.Keystone{
				ObjectMeta: metav1.ObjectMeta{Name: tc.ksName, Namespace: "default"},
			}
			g.Expect(fernetStagingSecretName(ks)).To(Equal(tc.expected))
		})
	}
}

func TestCredentialStagingSecretName_CC0081(t *testing.T) {
	cases := []struct {
		name     string
		ksName   string
		expected string
	}{
		{
			name:     "short name",
			ksName:   "ks",
			expected: "ks-credential-keys-rotation",
		},
		{
			name:     "conventional name",
			ksName:   "test-keystone",
			expected: "test-keystone-credential-keys-rotation",
		},
		{
			name:     "long name with multiple dashes",
			ksName:   "openstack-control-plane-keystone",
			expected: "openstack-control-plane-keystone-credential-keys-rotation",
		},
		{
			name:     "numeric suffix",
			ksName:   "keystone-42",
			expected: "keystone-42-credential-keys-rotation",
		},
		{
			name:     "single character name",
			ksName:   "k",
			expected: "k-credential-keys-rotation",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := &keystonev1alpha1.Keystone{
				ObjectMeta: metav1.ObjectMeta{Name: tc.ksName, Namespace: "default"},
			}
			g.Expect(credentialStagingSecretName(ks)).To(Equal(tc.expected))
		})
	}
}
