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

// Feature: CC-0004

// Feature: CC-0005

// PolicyConfigMapKey is the ConfigMap data key that holds oslo.policy rules.
// This key is part of the operator-user contract: users must store their
// policy YAML under this key in the referenced ConfigMap (CC-0005).
const PolicyConfigMapKey = "policy.yaml"

// LoadPolicyFromConfigMap reads the PolicyConfigMapKey key from a ConfigMap and
// parses it into a map of policy rules. Returns an error if the ConfigMap
// does not exist or does not contain the expected key (CC-0005).
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

// MergePolicies merges two PolicySpec objects. Override rules take precedence
// over base rules for matching keys. If override has ConfigMapRef, it replaces
// the base ConfigMapRef. Returns a new PolicySpec without mutating inputs.
func MergePolicies(base, override types.PolicySpec) types.PolicySpec {
	result := types.PolicySpec{}

	// Merge rules: copy base first, then overlay override.
	if base.Rules != nil || override.Rules != nil {
		result.Rules = make(map[string]string)
		for k, v := range base.Rules {
			result.Rules[k] = v
		}
		for k, v := range override.Rules {
			result.Rules[k] = v
		}
	}

	// Handle ConfigMapRef: override wins if set.
	if override.ConfigMapRef != nil {
		result.ConfigMapRef = &corev1.LocalObjectReference{
			Name: override.ConfigMapRef.Name,
		}
	} else if base.ConfigMapRef != nil {
		result.ConfigMapRef = &corev1.LocalObjectReference{
			Name: base.ConfigMapRef.Name,
		}
	}

	return result
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
			allErrs = append(allErrs, field.Required(keyPath, "rule key must not be empty"))
			continue // value check is meaningless for an empty key (CC-0004)
		}
		if rules[k] == "" {
			allErrs = append(allErrs, field.Required(keyPath, "rule value must not be empty"))
		}
	}

	return allErrs
}
