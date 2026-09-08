// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package satellite

import (
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A ConfigMap stands in for the satellite CR: Collect only reads the name, the
// deletion timestamp and whatever the caller's closures look at. The gate and
// the default rule read an annotation each, standing in for the
// CredentialsReady condition and spec.isDefault.
func satellite(name string, ready, isDefault bool) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns",
		Name:      name,
		Annotations: map[string]string{
			"ready":   boolAnnotation(ready),
			"default": boolAnnotation(isDefault),
		},
	}}
}

func boolAnnotation(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// deleting marks a satellite as terminating, which drops it from the pass.
func deleting(cm *corev1.ConfigMap) *corev1.ConfigMap {
	now := metav1.Now()
	cm.DeletionTimestamp = &now
	return cm
}

func gateReady(cm *corev1.ConfigMap) bool { return cm.Annotations["ready"] == "true" }

func isDefault(cm *corev1.ConfigMap) bool { return cm.Annotations["default"] == "true" }

// names reduces a slice of satellites to their names, which is what the order
// assertions are about.
func names(items []*corev1.ConfigMap) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Name)
	}
	return out
}

func TestCollect(t *testing.T) {
	t.Run("no items at all", func(t *testing.T) {
		g := gomega.NewWithT(t)

		withRule := Collect(CollectParams[corev1.ConfigMap, *corev1.ConfigMap]{
			Gate: gateReady, IsDefault: isDefault,
		})
		g.Expect(withRule.Attached).To(gomega.BeEmpty())
		g.Expect(withRule.Gated).To(gomega.BeEmpty())
		g.Expect(withRule.DefaultCandidates).To(gomega.BeEmpty())
		g.Expect(withRule.Valid).To(gomega.BeFalse(), "a default rule needs exactly one candidate")

		withoutRule := Collect(CollectParams[corev1.ConfigMap, *corev1.ConfigMap]{Gate: gateReady})
		g.Expect(withoutRule.Valid).To(gomega.BeTrue(), "no default rule is always valid")
	})

	t.Run("only deleting items", func(t *testing.T) {
		g := gomega.NewWithT(t)
		items := []*corev1.ConfigMap{
			deleting(satellite("a", true, true)),
			deleting(satellite("b", true, false)),
		}

		got := Collect(CollectParams[corev1.ConfigMap, *corev1.ConfigMap]{
			Items: items, Gate: gateReady, IsDefault: isDefault,
		})

		g.Expect(got.Attached).To(gomega.BeEmpty())
		g.Expect(got.Gated).To(gomega.BeEmpty())
		g.Expect(got.DefaultCandidates).To(gomega.BeEmpty())
		g.Expect(got.Valid).To(gomega.BeFalse())
	})

	t.Run("attached is sorted by name and excludes the deleting items", func(t *testing.T) {
		g := gomega.NewWithT(t)
		items := []*corev1.ConfigMap{
			satellite("c", false, false),
			deleting(satellite("b", true, true)),
			satellite("a", true, true),
		}

		got := Collect(CollectParams[corev1.ConfigMap, *corev1.ConfigMap]{
			Items: items, Gate: gateReady, IsDefault: isDefault,
		})

		g.Expect(names(got.Attached)).To(gomega.Equal([]string{"a", "c"}))
		g.Expect(names(got.Gated)).To(gomega.Equal([]string{"a"}))
		g.Expect(names(got.DefaultCandidates)).To(gomega.Equal([]string{"a"}))
		g.Expect(got.Valid).To(gomega.BeTrue())
		g.Expect(got.Attached[0]).To(gomega.BeIdenticalTo(items[2]), "the collection aliases the caller's objects")
	})

	t.Run("a nil gate passes everything", func(t *testing.T) {
		g := gomega.NewWithT(t)
		items := []*corev1.ConfigMap{satellite("b", false, false), satellite("a", false, false)}

		got := Collect(CollectParams[corev1.ConfigMap, *corev1.ConfigMap]{Items: items})

		g.Expect(names(got.Gated)).To(gomega.Equal([]string{"a", "b"}))
		g.Expect(got.DefaultCandidates).To(gomega.BeEmpty(), "a nil default rule names no candidates")
		g.Expect(got.Valid).To(gomega.BeTrue())
	})

	t.Run("an ungated default is not a candidate", func(t *testing.T) {
		g := gomega.NewWithT(t)
		items := []*corev1.ConfigMap{satellite("a", false, true), satellite("b", true, false)}

		got := Collect(CollectParams[corev1.ConfigMap, *corev1.ConfigMap]{
			Items: items, Gate: gateReady, IsDefault: isDefault,
		})

		g.Expect(names(got.Attached)).To(gomega.Equal([]string{"a", "b"}))
		g.Expect(names(got.Gated)).To(gomega.Equal([]string{"b"}))
		g.Expect(got.DefaultCandidates).To(gomega.BeEmpty())
		g.Expect(got.Valid).To(gomega.BeFalse())
	})

	t.Run("two gated defaults are an invalid projection", func(t *testing.T) {
		g := gomega.NewWithT(t)
		items := []*corev1.ConfigMap{satellite("b", true, true), satellite("a", true, true)}

		got := Collect(CollectParams[corev1.ConfigMap, *corev1.ConfigMap]{
			Items: items, Gate: gateReady, IsDefault: isDefault,
		})

		g.Expect(names(got.DefaultCandidates)).To(gomega.Equal([]string{"a", "b"}),
			"both candidates are reported so the operator can name them")
		g.Expect(got.Valid).To(gomega.BeFalse())
	})
}
