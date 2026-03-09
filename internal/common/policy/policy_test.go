// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/types"
)

// Feature: CC-0004

func TestRenderPolicyYAML(t *testing.T) {
	tests := []struct {
		name  string
		rules map[string]string
		want  string
	}{
		{
			name:  "nil rules",
			rules: nil,
			want:  "",
		},
		{
			name:  "empty rules map",
			rules: map[string]string{},
			want:  "",
		},
		{
			name: "single rule",
			rules: map[string]string{
				"compute:create": "role:admin",
			},
			want: "compute:create: role:admin\n",
		},
		{
			name: "multiple rules sorted alphabetically",
			rules: map[string]string{
				"identity:list_users":   "role:admin",
				"compute:create":        "role:member",
				"network:create_subnet": "role:admin or role:network_admin",
			},
			want: "compute:create: role:member\nidentity:list_users: role:admin\nnetwork:create_subnet: role:admin or role:network_admin\n",
		},
		{
			name: "rules with empty string rule name",
			rules: map[string]string{
				"": "role:admin",
			},
			want: "\"\": role:admin\n",
		},
		{
			name: "rule value with special characters",
			rules: map[string]string{
				"compute:create": "role:admin or (project_id:%(project_id)s and role:member)",
			},
			want: "compute:create: role:admin or (project_id:%(project_id)s and role:member)\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			got, err := RenderPolicyYAML(tt.rules)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}

func TestRenderPolicyYAML_sortedOutput(t *testing.T) {
	g := NewGomegaWithT(t)

	rules := map[string]string{
		"z_rule": "role:admin",
		"a_rule": "role:member",
		"m_rule": "role:reader",
	}

	got, err := RenderPolicyYAML(rules)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal("a_rule: role:member\nm_rule: role:reader\nz_rule: role:admin\n"))
}

func TestMergePolicies(t *testing.T) {
	tests := []struct {
		name     string
		base     types.PolicySpec
		override types.PolicySpec
		want     types.PolicySpec
	}{
		{
			name:     "both empty",
			base:     types.PolicySpec{},
			override: types.PolicySpec{},
			want:     types.PolicySpec{},
		},
		{
			name: "base only with rules",
			base: types.PolicySpec{
				Rules: map[string]string{"compute:create": "role:admin"},
			},
			override: types.PolicySpec{},
			want: types.PolicySpec{
				Rules: map[string]string{"compute:create": "role:admin"},
			},
		},
		{
			name: "override only with rules",
			base: types.PolicySpec{},
			override: types.PolicySpec{
				Rules: map[string]string{"compute:create": "role:member"},
			},
			want: types.PolicySpec{
				Rules: map[string]string{"compute:create": "role:member"},
			},
		},
		{
			name: "override rules take precedence",
			base: types.PolicySpec{
				Rules: map[string]string{
					"compute:create": "role:admin",
					"compute:delete": "role:admin",
				},
			},
			override: types.PolicySpec{
				Rules: map[string]string{
					"compute:create": "role:member",
				},
			},
			want: types.PolicySpec{
				Rules: map[string]string{
					"compute:create": "role:member",
					"compute:delete": "role:admin",
				},
			},
		},
		{
			name: "override adds new rules to base",
			base: types.PolicySpec{
				Rules: map[string]string{
					"compute:create": "role:admin",
				},
			},
			override: types.PolicySpec{
				Rules: map[string]string{
					"identity:list_users": "role:reader",
				},
			},
			want: types.PolicySpec{
				Rules: map[string]string{
					"compute:create":    "role:admin",
					"identity:list_users": "role:reader",
				},
			},
		},
		{
			name: "override ConfigMapRef replaces base ConfigMapRef",
			base: types.PolicySpec{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "base-policy"},
			},
			override: types.PolicySpec{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "override-policy"},
			},
			want: types.PolicySpec{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "override-policy"},
			},
		},
		{
			name: "base ConfigMapRef preserved when override has none",
			base: types.PolicySpec{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "base-policy"},
			},
			override: types.PolicySpec{},
			want: types.PolicySpec{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "base-policy"},
			},
		},
		{
			name: "override ConfigMapRef set when base has none",
			base: types.PolicySpec{},
			override: types.PolicySpec{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "override-policy"},
			},
			want: types.PolicySpec{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "override-policy"},
			},
		},
		{
			name: "full merge with rules and ConfigMapRef",
			base: types.PolicySpec{
				Rules: map[string]string{
					"compute:create": "role:admin",
					"compute:delete": "role:admin",
				},
				ConfigMapRef: &corev1.LocalObjectReference{Name: "base-policy"},
			},
			override: types.PolicySpec{
				Rules: map[string]string{
					"compute:create": "role:member",
					"network:create": "role:admin",
				},
				ConfigMapRef: &corev1.LocalObjectReference{Name: "override-policy"},
			},
			want: types.PolicySpec{
				Rules: map[string]string{
					"compute:create": "role:member",
					"compute:delete": "role:admin",
					"network:create": "role:admin",
				},
				ConfigMapRef: &corev1.LocalObjectReference{Name: "override-policy"},
			},
		},
		{
			name: "both empty-but-non-nil rules preserves non-nil result (CC-0004)",
			base: types.PolicySpec{
				Rules: map[string]string{},
			},
			override: types.PolicySpec{
				Rules: map[string]string{},
			},
			want: types.PolicySpec{
				Rules: map[string]string{},
			},
		},
		{
			name: "nil base rules with override rules",
			base: types.PolicySpec{
				Rules: nil,
			},
			override: types.PolicySpec{
				Rules: map[string]string{"compute:create": "role:member"},
			},
			want: types.PolicySpec{
				Rules: map[string]string{"compute:create": "role:member"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(MergePolicies(tt.base, tt.override)).To(Equal(tt.want))
		})
	}
}

func TestMergePolicies_doesNotMutateInputs(t *testing.T) {
	g := NewGomegaWithT(t)

	base := types.PolicySpec{
		Rules: map[string]string{
			"compute:create": "role:admin",
		},
		ConfigMapRef: &corev1.LocalObjectReference{Name: "base-policy"},
	}
	override := types.PolicySpec{
		Rules: map[string]string{
			"compute:create": "role:member",
		},
	}

	result := MergePolicies(base, override)

	// Mutating the result should not affect the inputs.
	result.Rules["compute:create"] = "mutated"
	result.Rules["new_rule"] = "new_value"

	g.Expect(base.Rules["compute:create"]).To(Equal("role:admin"))
	g.Expect(override.Rules["compute:create"]).To(Equal("role:member"))
	g.Expect(base.Rules).NotTo(HaveKey("new_rule"))
	g.Expect(override.Rules).NotTo(HaveKey("new_rule"))

	// Mutating result.ConfigMapRef should not affect base.ConfigMapRef (CC-0004).
	if result.ConfigMapRef != nil && base.ConfigMapRef != nil {
		result.ConfigMapRef.Name = "mutated"
		g.Expect(base.ConfigMapRef.Name).To(Equal("base-policy"))
	}
}

func TestValidatePolicyRules(t *testing.T) {
	fldPath := field.NewPath("spec", "policy", "rules")

	tests := []struct {
		name      string
		rules     map[string]string
		wantCount int
		wantField string
		wantMsg   string
	}{
		{
			name:      "nil rules returns no errors",
			rules:     nil,
			wantCount: 0,
		},
		{
			name:      "empty rules map returns no errors",
			rules:     map[string]string{},
			wantCount: 0,
		},
		{
			name:      "valid rules return no errors",
			rules:     map[string]string{"compute:create": "role:admin"},
			wantCount: 0,
		},
		{
			name:      "multiple valid rules return no errors",
			rules:     map[string]string{"compute:create": "role:admin", "identity:list": "role:reader"},
			wantCount: 0,
		},
		{
			name:      "rule with empty value returns error",
			rules:     map[string]string{"compute:delete": ""},
			wantCount: 1,
			wantField: "spec.policy.rules[compute:delete]",
			wantMsg:   "rule value must not be empty",
		},
		{
			name:      "rule with empty key returns error",
			rules:     map[string]string{"": "role:admin"},
			wantCount: 1,
			wantField: "spec.policy.rules[]",
			wantMsg:   "rule key must not be empty",
		},
		{
			name:      "rule with empty key and empty value returns one error (CC-0004)",
			rules:     map[string]string{"": ""},
			wantCount: 1,
			wantField: "spec.policy.rules[]",
			wantMsg:   "rule key must not be empty",
		},
		{
			name: "multiple empty values returns multiple errors",
			rules: map[string]string{
				"compute:create": "role:admin",
				"compute:delete": "",
				"network:create": "",
			},
			wantCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			errs := ValidatePolicyRules(tt.rules, fldPath)
			g.Expect(errs).To(HaveLen(tt.wantCount))
			if tt.wantField != "" {
				g.Expect(errs[0].Field).To(Equal(tt.wantField))
			}
			if tt.wantMsg != "" {
				g.Expect(errs[0].Detail).To(Equal(tt.wantMsg))
			}
		})
	}
}

func TestValidatePolicyRules_nilFldPathDefaultsToRules(t *testing.T) {
	g := NewGomegaWithT(t)

	rules := map[string]string{"compute:delete": ""}
	errs := ValidatePolicyRules(rules, nil)
	g.Expect(errs).To(HaveLen(1))
	g.Expect(errs[0].Field).To(Equal("rules[compute:delete]"))
	g.Expect(errs[0].Detail).To(Equal("rule value must not be empty"))
}

// Feature: CC-0005

func newPolicyScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func TestLoadPolicyFromConfigMap_success(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newPolicyScheme()
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-policy",
			Namespace: "default",
		},
		Data: map[string]string{
			PolicyConfigMapKey: "compute:create: role:admin\ncompute:delete: role:admin\n",
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cm).Build()

	rules, err := LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{Name: "my-policy", Namespace: "default"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rules).To(Equal(map[string]string{
		"compute:create": "role:admin",
		"compute:delete": "role:admin",
	}))
}

func TestLoadPolicyFromConfigMap_missingKey(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newPolicyScheme()
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-policy",
			Namespace: "default",
		},
		Data: map[string]string{
			"other.yaml": "some: data\n",
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cm).Build()

	_, err := LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{Name: "my-policy", Namespace: "default"})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(PolicyConfigMapKey))
}

func TestLoadPolicyFromConfigMap_notFound(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newPolicyScheme()
	ctx := context.Background()

	c := fake.NewClientBuilder().WithScheme(s).Build()

	_, err := LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{Name: "missing", Namespace: "default"})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("getting ConfigMap"))
}

func TestLoadPolicyFromConfigMap_emptyValue(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newPolicyScheme()
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty-policy",
			Namespace: "default",
		},
		Data: map[string]string{
			PolicyConfigMapKey: "",
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cm).Build()

	_, err := LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{Name: "empty-policy", Namespace: "default"})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("empty or parsed to nil"))
}

func TestLoadPolicyFromConfigMap_nullYAML(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newPolicyScheme()
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "null-policy",
			Namespace: "default",
		},
		Data: map[string]string{
			PolicyConfigMapKey: "null\n",
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cm).Build()

	_, err := LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{Name: "null-policy", Namespace: "default"})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("empty or parsed to nil"))
}

func TestLoadPolicyFromConfigMap_invalidYAML(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newPolicyScheme()
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-policy",
			Namespace: "default",
		},
		Data: map[string]string{
			PolicyConfigMapKey: ": : : not valid yaml [[[",
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cm).Build()

	_, err := LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{Name: "bad-policy", Namespace: "default"})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("parsing " + PolicyConfigMapKey))
}
