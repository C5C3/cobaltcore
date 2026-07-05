// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package naming

import (
	"testing"

	"github.com/onsi/gomega"
)

func TestCommonLabels(t *testing.T) {
	g := gomega.NewWithT(t)

	labels := CommonLabels("keystone", "my-instance")

	g.Expect(labels).To(gomega.Equal(map[string]string{
		"app.kubernetes.io/name":       "keystone",
		"app.kubernetes.io/instance":   "my-instance",
		"app.kubernetes.io/managed-by": "keystone-operator",
	}))
}

// SelectorLabels must stay a strict subset of CommonLabels: Deployment
// selectors are immutable, so a key that appears in the selector but not in
// the pod template labels would wedge every rollout.
func TestSelectorLabels_SubsetOfCommonLabels(t *testing.T) {
	g := gomega.NewWithT(t)

	common := CommonLabels("keystone", "my-instance")
	selector := SelectorLabels("keystone", "my-instance")

	g.Expect(selector).NotTo(gomega.BeEmpty())
	for k, v := range selector {
		g.Expect(common).To(gomega.HaveKeyWithValue(k, v))
	}
	g.Expect(len(selector)).To(gomega.BeNumerically("<", len(common)))
}

func TestSubResourceName(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(SubResourceName("keystone-prod")).To(gomega.Equal("keystone-prod"))
	// The empty instance name passes through unchanged — callers own CR-name
	// validity (the apiserver guarantees a non-empty metadata.name).
	g.Expect(SubResourceName("")).To(gomega.Equal(""))
}
