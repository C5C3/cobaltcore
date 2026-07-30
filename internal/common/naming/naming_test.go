// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package naming

import (
	"testing"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

func TestAPISelectorLabels(t *testing.T) {
	g := gomega.NewWithT(t)

	labels := APISelectorLabels("keystone", "my-instance")

	g.Expect(labels).To(gomega.Equal(map[string]string{
		"app.kubernetes.io/name":      "keystone",
		"app.kubernetes.io/instance":  "my-instance",
		"app.kubernetes.io/component": "api",
	}))
}

func TestComponentLabels(t *testing.T) {
	g := gomega.NewWithT(t)

	labels := ComponentLabels("keystone", "my-instance", "trust-flush")

	g.Expect(labels).To(gomega.Equal(map[string]string{
		"app.kubernetes.io/name":       "keystone",
		"app.kubernetes.io/instance":   "my-instance",
		"app.kubernetes.io/managed-by": "keystone-operator",
		"app.kubernetes.io/component":  "trust-flush",
	}))
}

// The API Service selector must be satisfied by the API pod template,
// otherwise the Service loses every endpoint on the next rollout.
func TestAPISelectorLabels_SubsetOfAPIComponentLabels(t *testing.T) {
	g := gomega.NewWithT(t)

	pod := ComponentLabels("keystone", "my-instance", ComponentAPI)
	selector := APISelectorLabels("keystone", "my-instance")

	g.Expect(selector).NotTo(gomega.BeEmpty())
	for k, v := range selector {
		g.Expect(pod).To(gomega.HaveKeyWithValue(k, v))
	}
	g.Expect(len(selector)).To(gomega.BeNumerically("<", len(pod)))
}

// SelectorLabels must stay a subset of the labels of every component, not
// just of the API one: the immutable Deployment selector and the NetworkPolicy
// pod selectors match maintenance pods through it, and so does the API Service
// selector until the component label has rolled out everywhere.
func TestSelectorLabels_SubsetOfComponentLabels(t *testing.T) {
	g := gomega.NewWithT(t)

	pod := ComponentLabels("keystone", "my-instance", "trust-flush")
	selector := SelectorLabels("keystone", "my-instance")

	g.Expect(selector).NotTo(gomega.BeEmpty())
	for k, v := range selector {
		g.Expect(pod).To(gomega.HaveKeyWithValue(k, v))
	}
	g.Expect(len(selector)).To(gomega.BeNumerically("<", len(pod)))
}

// The API PodDisruptionBudget selector — SelectorLabels plus ExcludeJobPods —
// must cover every API pod, including one from a template that predates the
// component label, and no Job-created maintenance pod. Keying it on the
// component instead would leave the pods that are actually serving during the
// migration outside every budget, and the eviction API only consults budgets
// that match the pod being evicted.
func TestExcludeJobPods_CoversAPIPodsButNoJobPod(t *testing.T) {
	g := gomega.NewWithT(t)

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels:      SelectorLabels("keystone", "my-instance"),
		MatchExpressions: ExcludeJobPods(),
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	g.Expect(selector.Matches(labels.Set(ComponentLabels("keystone", "my-instance", ComponentAPI)))).To(gomega.BeTrue())
	g.Expect(selector.Matches(labels.Set(CommonLabels("keystone", "my-instance")))).To(gomega.BeTrue(),
		"an API pod predating the component label must stay covered")

	jobPod := ComponentLabels("keystone", "my-instance", "trust-flush")
	jobPod[LabelKeyJobName] = "my-instance-trust-flush-29000000"
	g.Expect(selector.Matches(labels.Set(jobPod))).To(gomega.BeFalse())

	// Another instance is out of scope regardless of the Job label.
	g.Expect(selector.Matches(labels.Set(ComponentLabels("keystone", "other", ComponentAPI)))).To(gomega.BeFalse())
}
