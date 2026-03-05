// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
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
