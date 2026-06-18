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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

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

// ConfigBaseLabelKey is the label key applied to immutable ConfigMaps created
// by CreateImmutableConfigMap, identifying the base name that generated them.
// PruneImmutableConfigMaps uses this label as a server-side selector to avoid
// listing all ConfigMaps in the namespace.
const ConfigBaseLabelKey = "forge.c5c3.io/config-base"

// hashTruncateLen is the number of hex characters kept from the SHA-256
// content hash when building immutable ConfigMap name suffixes. 8 hex chars
// (32 bits) yield a collision probability of ~1 in 4 billion per base name,
// which is acceptable for ConfigMap naming.
const hashTruncateLen = 8

// CreateImmutableConfigMap creates an immutable ConfigMap with a content-hash
// suffix appended to the base name. The hash ensures configuration changes
// result in new ConfigMap names, triggering pod restarts. It returns the
// actual name of the created ConfigMap (with hash suffix).
//
// Note: Old ConfigMaps with the same baseName but different hash suffixes
// accumulate during the owner's lifetime since GC only removes them when the
// owner is deleted. Callers (reconcilers) should prune obsolete ConfigMaps
// after rolling updates complete to avoid unbounded growth.
func CreateImmutableConfigMap(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, baseName, namespace string, data map[string]string) (string, error) {
	// Compute deterministic hash from data. Keys are sorted and encoded using
	// length-prefixed format "len:key=len:value\n" to make each entry
	// self-delimiting regardless of content (e.g. values with embedded
	// newlines or '=' characters).
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%d:%s=%d:%s\n", len(k), k, len(data[k]), data[k])
	}
	hash := hex.EncodeToString(h.Sum(nil))[:hashTruncateLen]
	name := fmt.Sprintf("%s-%s", baseName, hash)

	immutable := true
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				ConfigBaseLabelKey: baseName,
			},
		},
		Data:      data,
		Immutable: &immutable,
	}

	if err := controllerutil.SetControllerReference(owner, cm, scheme); err != nil {
		return "", fmt.Errorf("setting owner reference on ConfigMap %s/%s: %w", namespace, name, err)
	}

	if err := c.Create(ctx, cm); err != nil {
		if !errors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating ConfigMap %s/%s: %w", namespace, name, err)
		}
		// Verify the existing ConfigMap is owned by the expected owner
		// to guard against stale GC artefacts or accidental name
		// collisions.
		existingCM := &corev1.ConfigMap{}
		if getErr := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, existingCM); getErr != nil {
			return "", fmt.Errorf("fetching existing ConfigMap %s/%s: %w", namespace, name, getErr)
		}
		controllerRef := metav1.GetControllerOf(existingCM)
		if controllerRef == nil || controllerRef.UID != owner.GetUID() {
			return "", fmt.Errorf("existing ConfigMap %s/%s is not owned by %s/%s",
				namespace, name, owner.GetNamespace(), owner.GetName())
		}
	}

	return name, nil
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
	// MergeDefaults(config, nil) is used as a deep-copy operation: it copies
	// all sections and keys from config into a new map with no defaults.
	result := MergeDefaults(config, nil)
	if result["oslo_policy"] == nil {
		result["oslo_policy"] = make(map[string]string)
	}
	result["oslo_policy"]["policy_file"] = policyFilePath
	return result
}

// PruneImmutableConfigMaps deletes stale immutable ConfigMaps that were
// previously created by CreateImmutableConfigMap. It retains the newest
// `retain` historical ConfigMaps (by CreationTimestamp) plus the currently
// active one identified by currentName. This prevents unbounded accumulation
// of immutable ConfigMaps across reconcile cycles.
//
// Known limitation: ConfigMaps created before the ConfigBaseLabelKey label was
// introduced lack the label and are invisible to the server-side
// selector used by this function. These pre-existing ConfigMaps will not be
// pruned but are bounded in number (no new unlabeled ConfigMaps are created
// after the upgrade) and will be garbage-collected by Kubernetes when the
// owning CR is deleted, since they carry a controller owner reference.
func PruneImmutableConfigMaps(ctx context.Context, c client.Client, owner client.Object, baseName, namespace, currentName string, retain int) error {
	logger := log.FromContext(ctx)

	// Clamp negative retain to 0 to prevent panics from misconfigured values.
	if retain < 0 {
		retain = 0
	}

	var allConfigMaps corev1.ConfigMapList
	if err := c.List(ctx, &allConfigMaps, client.InNamespace(namespace), client.MatchingLabels{ConfigBaseLabelKey: baseName}); err != nil {
		return fmt.Errorf("listing ConfigMaps in namespace %s: %w", namespace, err)
	}

	prefix := baseName + "-"
	var candidates []corev1.ConfigMap
	for _, cm := range allConfigMaps.Items {
		if !strings.HasPrefix(cm.Name, prefix) {
			continue
		}
		if cm.Name == currentName {
			continue
		}
		controllerRef := metav1.GetControllerOf(&cm)
		if controllerRef == nil || controllerRef.UID != owner.GetUID() {
			continue
		}
		candidates = append(candidates, cm)
	}

	// Sort candidates by CreationTimestamp descending (newest first).
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreationTimestamp.After(candidates[j].CreationTimestamp.Time)
	})

	if len(candidates) <= retain {
		return nil
	}

	for i := retain; i < len(candidates); i++ {
		cm := candidates[i]
		if err := client.IgnoreNotFound(c.Delete(ctx, &cm)); err != nil {
			return fmt.Errorf("deleting stale ConfigMap %s/%s: %w", namespace, cm.Name, err)
		}
		logger.Info("pruned stale immutable ConfigMap", "name", cm.Name, "namespace", namespace, "baseName", baseName, "ownerName", owner.GetName(), "ownerUID", owner.GetUID())
	}

	return nil
}
