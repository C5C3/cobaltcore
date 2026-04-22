// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the rotation staging helpers (CC-0081).
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

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
