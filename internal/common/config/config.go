// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Feature: CC-0004

// placeholderRe matches {{KEY}} placeholders in config values.
var placeholderRe = regexp.MustCompile(`\{\{([^}]+)\}\}`)

// RenderINI renders a map of INI sections into an INI format string.
// Sections are sorted alphabetically for deterministic output.
// Keys within each section are sorted alphabetically.
// Section names must be non-empty; an empty section name produces "[]",
// which is invalid INI. Callers are responsible for ensuring non-empty
// section names before calling this function.
func RenderINI(sections map[string]map[string]string) string {
	if len(sections) == 0 {
		return ""
	}

	sectionNames := make([]string, 0, len(sections))
	for name := range sections {
		sectionNames = append(sectionNames, name)
	}
	sort.Strings(sectionNames)

	var b strings.Builder
	for i, name := range sectionNames {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%s]\n", name)

		keys := make([]string, 0, len(sections[name]))
		for k := range sections[name] {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			fmt.Fprintf(&b, "%s = %s\n", k, sections[name][k])
		}
	}
	return b.String()
}

// MergeDefaults merges user-provided config with operator defaults.
// User values take precedence over defaults. Returns a new map without
// mutating the inputs.
func MergeDefaults(userConfig, defaults map[string]map[string]string) map[string]map[string]string {
	result := make(map[string]map[string]string)

	// Copy all defaults first.
	for section, kvs := range defaults {
		result[section] = make(map[string]string, len(kvs))
		for k, v := range kvs {
			result[section][k] = v
		}
	}

	// Overlay user config (user values win).
	for section, kvs := range userConfig {
		if _, ok := result[section]; !ok {
			result[section] = make(map[string]string, len(kvs))
		}
		for k, v := range kvs {
			result[section][k] = v
		}
	}

	return result
}

// InjectSecrets replaces {{SECRET_KEY}} placeholders in config values
// with the corresponding values from the secrets map. Returns a new map
// without mutating the input config. Unresolved placeholders are left as-is.
func InjectSecrets(config map[string]map[string]string, secrets map[string]string) map[string]map[string]string {
	result := make(map[string]map[string]string, len(config))
	for section, kvs := range config {
		result[section] = make(map[string]string, len(kvs))
		for k, v := range kvs {
			result[section][k] = placeholderRe.ReplaceAllStringFunc(v, func(match string) string {
				key := match[2 : len(match)-2] // strip {{ and }}
				if secret, ok := secrets[key]; ok {
					return secret
				}
				return match
			})
		}
	}
	return result
}

// InjectOsloPolicyConfig returns a config map with oslo_policy configuration
// injected. If policyFilePath is non-empty, it creates a deep copy of the
// input map (via MergeDefaults), ensures the oslo_policy section exists, sets
// the policy_file key, and returns the copy without mutating the input.
// If policyFilePath is empty,
// it returns the original config reference unchanged (no copy is made).
func InjectOsloPolicyConfig(config map[string]map[string]string, policyFilePath string) map[string]map[string]string {
	if policyFilePath == "" {
		return config
	}
	result := MergeDefaults(config, nil)
	if result["oslo_policy"] == nil {
		result["oslo_policy"] = make(map[string]string)
	}
	result["oslo_policy"]["policy_file"] = policyFilePath
	return result
}

// Feature: CC-0005

// CreateImmutableConfigMap creates an immutable ConfigMap with a content-hash
// suffix in its name. It computes a SHA-256 hash of the data map (sorted keys
// for determinism) and appends the first 8 hex chars as suffix: {name}-{hash[:8]}.
// The ConfigMap's Immutable field is set to true. A controller owner reference
// is set using the provided owner. Uses controllerutil.CreateOrUpdate to
// create or update the ConfigMap.
func CreateImmutableConfigMap(ctx context.Context, c client.Client, owner client.Object, name string, data map[string]string) (*corev1.ConfigMap, error) {
	hash := hashConfigMapData(data)
	hashedName := fmt.Sprintf("%s-%s", name, hash[:8])

	immutable := true
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hashedName,
			Namespace: owner.GetNamespace(),
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, c, cm, func() error {
		cm.Data = copyStringMap(data)
		cm.Immutable = &immutable
		return controllerutil.SetControllerReference(owner, cm, c.Scheme())
	})
	if err != nil {
		return nil, fmt.Errorf("creating or updating immutable ConfigMap %s/%s: %w", owner.GetNamespace(), hashedName, err)
	}

	return cm, nil
}

// copyStringMap returns a shallow copy of the given map to prevent mutations
// from propagating to the caller's data or to an immutable ConfigMap's stored
// data.
func copyStringMap(m map[string]string) map[string]string {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// hashConfigMapData computes a deterministic SHA-256 hash of ConfigMap data.
// Null byte separators are written between key and value, and after each entry,
// to prevent hash collisions from key/value boundary ambiguity (e.g.
// {"ab":"cd"} vs {"a":"bcd"}).
func hashConfigMapData(data map[string]string) string {
	h := sha256.New()
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(data[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
