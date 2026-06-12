// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/c5c3/forge/internal/common/types"
)

// PolicyConfigMapKey is the ConfigMap data key that holds oslo.policy rules.
// This key is part of the operator-user contract: users must store their
// policy YAML under this key in the referenced ConfigMap.
const PolicyConfigMapKey = "policy.yaml"

// LoadPolicyFromConfigMap reads the PolicyConfigMapKey key from a ConfigMap and
// parses it into a map of policy rules. Returns an error if the ConfigMap
// does not exist or does not contain the expected key.
func LoadPolicyFromConfigMap(ctx context.Context, c client.Client, key client.ObjectKey) (map[string]string, error) {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, key, &cm); err != nil {
		return nil, fmt.Errorf("getting ConfigMap %s: %w", key, err)
	}

	raw, ok := cm.Data[PolicyConfigMapKey]
	if !ok {
		return nil, fmt.Errorf("ConfigMap %s does not contain key %q", key, PolicyConfigMapKey)
	}

	var rules map[string]string
	if err := yaml.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, fmt.Errorf("parsing %s from ConfigMap %s: %w", PolicyConfigMapKey, key, err)
	}

	if rules == nil {
		return nil, fmt.Errorf("ConfigMap %s key %q is empty or parsed to nil", key, PolicyConfigMapKey)
	}

	return rules, nil
}

// RenderPolicyYAML renders oslo.policy rules as a YAML string.
// Keys are sorted alphabetically for deterministic output.
// Returns an empty string for nil or empty rules.
func RenderPolicyYAML(rules map[string]string) (string, error) {
	if len(rules) == 0 {
		return "", nil
	}

	// Build an ordered map using a slice of key-value pairs to ensure
	// alphabetical key ordering in the output.
	keys := make([]string, 0, len(rules))
	for k := range rules {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	ordered := make([]byte, 0, 256)
	for _, k := range keys {
		entry := map[string]string{k: rules[k]}
		b, err := yaml.Marshal(entry)
		if err != nil {
			return "", fmt.Errorf("failed to marshal policy rule %q: %w", k, err)
		}
		ordered = append(ordered, b...)
	}

	return string(ordered), nil
}

// MergePolicies merges a base policy with an override policy into a single
// freshly allocated *types.PolicySpec. Override values win on conflict:
//
//   - Rules: start from base.Rules, then override rules overwrite on key
//     conflict.
//   - ConfigMapRef: override wins when set, otherwise base's is used.
//
// Nil handling: both nil returns nil; if exactly one side is nil a fresh copy
// of the other is returned. The inputs are never mutated or aliased — the
// returned struct, its Rules map, and its ConfigMapRef are independent copies.
func MergePolicies(base, override *types.PolicySpec) *types.PolicySpec {
	if base == nil && override == nil {
		return nil
	}
	if base == nil {
		return copyPolicySpec(override)
	}
	if override == nil {
		return copyPolicySpec(base)
	}

	merged := &types.PolicySpec{}

	// Merge Rules: base is the base, override overwrites on key conflict.
	if base.Rules != nil || override.Rules != nil {
		rules := make(map[string]string, len(base.Rules)+len(override.Rules))
		for k, v := range base.Rules {
			rules[k] = v
		}
		for k, v := range override.Rules {
			rules[k] = v
		}
		merged.Rules = rules
	}

	// ConfigMapRef: override wins when set, else fall back to base.
	switch {
	case override.ConfigMapRef != nil:
		merged.ConfigMapRef = override.ConfigMapRef.DeepCopy()
	case base.ConfigMapRef != nil:
		merged.ConfigMapRef = base.ConfigMapRef.DeepCopy()
	}

	return merged
}

// copyPolicySpec returns a fresh *types.PolicySpec whose Rules map and
// ConfigMapRef are independent copies of src's, so callers can mutate the
// result without affecting the original. src must be non-nil.
func copyPolicySpec(src *types.PolicySpec) *types.PolicySpec {
	out := &types.PolicySpec{}
	if src.Rules != nil {
		rules := make(map[string]string, len(src.Rules))
		for k, v := range src.Rules {
			rules[k] = v
		}
		out.Rules = rules
	}
	if src.ConfigMapRef != nil {
		out.ConfigMapRef = src.ConfigMapRef.DeepCopy()
	}
	return out
}

// ValidatePolicyRules validates policy rules for empty keys and values.
// Returns field.ErrorList for structured, multi-error reporting compatible
// with Kubernetes webhook validation. The fldPath parameter is the field
// path prefix for error reporting (e.g., field.NewPath("spec", "policy", "rules")).
// If fldPath is nil, it defaults to field.NewPath("rules").
func ValidatePolicyRules(rules map[string]string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if len(rules) == 0 {
		return allErrs
	}

	if fldPath == nil {
		fldPath = field.NewPath("rules")
	}

	// Iterate in sorted order for deterministic error ordering.
	keys := make([]string, 0, len(rules))
	for k := range rules {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		keyPath := fldPath.Key(k)
		if k == "" {
			allErrs = append(allErrs, field.Required(keyPath, "policy rule name must not be empty"))
			continue // value check is meaningless for an empty key
		}
		if rules[k] == "" {
			allErrs = append(allErrs, field.Required(keyPath, "policy rule value must not be empty"))
		}
	}

	return allErrs
}
