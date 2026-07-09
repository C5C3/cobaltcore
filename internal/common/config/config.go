// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// RenderINI renders a map of INI sections into an INI format string.
// Sections are sorted alphabetically for deterministic output.
// Keys within each section are sorted alphabetically.
// Section names must be non-empty; an empty section name produces "[]",
// which is invalid INI. Callers are responsible for ensuring non-empty
// section names before calling this function.
//
// It is the single-valued adapter over RenderINIMulti: every key renders as
// exactly one "key = value" line. Callers needing an oslo MultiStrOpt (a key
// repeated once per value, e.g. [federation] trusted_dashboard) lift their
// merged map with LiftSections and render with RenderINIMulti.
func RenderINI(sections map[string]map[string]string) string {
	return RenderINIMulti(LiftSections(sections))
}

// RenderINIMulti renders a map of INI sections whose values are ordered slices
// into an INI format string, emitting one "key = value" line per slice element
// in slice order. This is the wire form oslo.config's MultiStrOpt consumes: a
// repeated key accumulates into a list, so [federation] trusted_dashboard with
// two origins must appear as two separate lines rather than one joined value.
//
// Sections and keys are sorted alphabetically for deterministic output — the
// rendered config feeds a content-addressed ConfigMap name, so an unstable
// order would churn the name on every reconcile. Values within a key keep
// their slice order, since oslo preserves it and callers may depend on the
// first entry (e.g. a preferred dashboard origin).
//
// A key whose slice is empty is omitted entirely, so an absent option falls
// back to oslo's compiled-in default rather than being overridden with an
// empty value. A section with no rendered keys still emits its header, which
// matches RenderINI's behavior for an empty section map.
func RenderINIMulti(sections map[string]map[string][]string) string {
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
			for _, v := range sections[name][k] {
				fmt.Fprintf(&b, "%s = %s\n", k, v)
			}
		}
	}
	return b.String()
}

// LiftSections converts a single-valued section map into the multi-valued
// shape RenderINIMulti consumes, lifting each value to a one-element slice.
// It returns a new map without mutating the input, so callers may set
// multi-valued keys on the result after merging their single-valued defaults
// and user overrides through MergeDefaults.
func LiftSections(sections map[string]map[string]string) map[string]map[string][]string {
	if sections == nil {
		return nil
	}
	out := make(map[string]map[string][]string, len(sections))
	for section, kvs := range sections {
		lifted := make(map[string][]string, len(kvs))
		for k, v := range kvs {
			lifted[k] = []string{v}
		}
		out[section] = lifted
	}
	return out
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

// CreateImmutableSecret creates an immutable Secret with a content-hash
// suffix appended to the base name, mirroring CreateImmutableConfigMap for
// data that must not live in a ConfigMap (e.g. rendered per-domain keystone
// config files carrying LDAP bind passwords). The hash ensures content
// changes result in new Secret names, triggering pod restarts. It returns the
// actual name of the created Secret (with hash suffix).
//
// Note: Old Secrets with the same baseName but different hash suffixes
// accumulate during the owner's lifetime since GC only removes them when the
// owner is deleted. Callers (reconcilers) should prune obsolete Secrets after
// rolling updates complete via PruneImmutableSecrets to avoid unbounded growth.
func CreateImmutableSecret(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, baseName, namespace string, data map[string][]byte) (string, error) {
	// Compute deterministic hash from data using the same length-prefixed
	// "len:key=len:value\n" encoding as CreateImmutableConfigMap so each entry
	// is self-delimiting regardless of content.
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
	secret := &corev1.Secret{
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

	if err := controllerutil.SetControllerReference(owner, secret, scheme); err != nil {
		return "", fmt.Errorf("setting owner reference on Secret %s/%s: %w", namespace, name, err)
	}

	if err := c.Create(ctx, secret); err != nil {
		if !errors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating Secret %s/%s: %w", namespace, name, err)
		}
		// Verify the existing Secret is owned by the expected owner to guard
		// against stale GC artefacts or accidental name collisions.
		existing := &corev1.Secret{}
		if getErr := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, existing); getErr != nil {
			return "", fmt.Errorf("fetching existing Secret %s/%s: %w", namespace, name, getErr)
		}
		controllerRef := metav1.GetControllerOf(existing)
		if controllerRef == nil || controllerRef.UID != owner.GetUID() {
			return "", fmt.Errorf("existing Secret %s/%s is not owned by %s/%s",
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

// PruneOptions parameterizes PruneImmutableConfigMaps. Grouping the three
// previously-positional string arguments (BaseName, Namespace, CurrentName)
// into a named struct removes the transposition hazard of a four-string
// signature and makes call sites self-documenting.
type PruneOptions struct {
	// BaseName is the ConfigMap base name (the ConfigBaseLabelKey value) whose
	// hash-suffixed history is pruned.
	BaseName string
	// Namespace is the namespace the ConfigMaps live in.
	Namespace string
	// CurrentName is the actively-mounted ConfigMap that must never be pruned.
	// Empty prunes every historical ConfigMap for the base name.
	CurrentName string
	// Retain is the number of newest historical ConfigMaps to keep in addition
	// to CurrentName. Negative values are clamped to zero.
	Retain int
}

// PruneImmutableConfigMaps deletes stale immutable ConfigMaps that were
// previously created by CreateImmutableConfigMap. It retains the newest
// opts.Retain historical ConfigMaps (by CreationTimestamp) plus the currently
// active one identified by opts.CurrentName. This prevents unbounded
// accumulation of immutable ConfigMaps across reconcile cycles.
//
// Among ConfigMaps that share a CreationTimestamp — common because the
// timestamp has one-second granularity and several may be written in the same
// reconcile — the sort breaks ties deterministically by name (descending), so
// repeated runs prune and retain the same objects rather than an arbitrary
// subset.
//
// Known limitation: ConfigMaps created before the ConfigBaseLabelKey label was
// introduced lack the label and are invisible to the server-side
// selector used by this function. These pre-existing ConfigMaps will not be
// pruned but are bounded in number (no new unlabeled ConfigMaps are created
// after the upgrade) and will be garbage-collected by Kubernetes when the
// owning CR is deleted, since they carry a controller owner reference.
func PruneImmutableConfigMaps(ctx context.Context, c client.Client, owner client.Object, opts PruneOptions) error {
	var allConfigMaps corev1.ConfigMapList
	if err := c.List(ctx, &allConfigMaps, client.InNamespace(opts.Namespace), client.MatchingLabels{ConfigBaseLabelKey: opts.BaseName}); err != nil {
		return fmt.Errorf("listing ConfigMaps in namespace %s: %w", opts.Namespace, err)
	}
	items := make([]client.Object, 0, len(allConfigMaps.Items))
	for i := range allConfigMaps.Items {
		items = append(items, &allConfigMaps.Items[i])
	}
	return pruneImmutableObjects(ctx, c, owner, opts, items, "ConfigMap")
}

// PruneImmutableSecrets deletes stale immutable Secrets previously created by
// CreateImmutableSecret, with the same retain/tie-break/ownership semantics as
// PruneImmutableConfigMaps. Retain: 0 combined with an empty CurrentName
// removes every historical Secret for the base name — the full-cleanup path a
// caller takes when the feature that produced the Secrets is turned off.
func PruneImmutableSecrets(ctx context.Context, c client.Client, owner client.Object, opts PruneOptions) error {
	var allSecrets corev1.SecretList
	if err := c.List(ctx, &allSecrets, client.InNamespace(opts.Namespace), client.MatchingLabels{ConfigBaseLabelKey: opts.BaseName}); err != nil {
		return fmt.Errorf("listing Secrets in namespace %s: %w", opts.Namespace, err)
	}
	items := make([]client.Object, 0, len(allSecrets.Items))
	for i := range allSecrets.Items {
		items = append(items, &allSecrets.Items[i])
	}
	return pruneImmutableObjects(ctx, c, owner, opts, items, "Secret")
}

// pruneImmutableObjects implements the shared prune algorithm for the
// ConfigMap and Secret flavors: filter to the owner's hash-suffixed history
// (never the CurrentName), sort newest-first with a deterministic name
// tie-break, and delete everything past the retain count. kind is only used
// for error/log messages.
func pruneImmutableObjects(ctx context.Context, c client.Client, owner client.Object, opts PruneOptions, items []client.Object, kind string) error {
	logger := log.FromContext(ctx)

	// Clamp negative retain to 0 to prevent panics from misconfigured values.
	retain := opts.Retain
	if retain < 0 {
		retain = 0
	}

	prefix := opts.BaseName + "-"
	var candidates []client.Object
	for _, obj := range items {
		if !strings.HasPrefix(obj.GetName(), prefix) {
			continue
		}
		if obj.GetName() == opts.CurrentName {
			continue
		}
		controllerRef := metav1.GetControllerOf(obj)
		if controllerRef == nil || controllerRef.UID != owner.GetUID() {
			continue
		}
		candidates = append(candidates, obj)
	}

	// Sort candidates by CreationTimestamp descending (newest first). Use a
	// stable sort with a name tie-break so same-second objects retain a
	// deterministic order across runs.
	sort.SliceStable(candidates, func(i, j int) bool {
		ti, tj := candidates[i].GetCreationTimestamp().Time, candidates[j].GetCreationTimestamp().Time
		if ti.Equal(tj) {
			return candidates[i].GetName() > candidates[j].GetName()
		}
		return ti.After(tj)
	})

	if len(candidates) <= retain {
		return nil
	}

	for i := retain; i < len(candidates); i++ {
		obj := candidates[i]
		if err := client.IgnoreNotFound(c.Delete(ctx, obj)); err != nil {
			return fmt.Errorf("deleting stale %s %s/%s: %w", kind, opts.Namespace, obj.GetName(), err)
		}
		logger.Info("pruned stale immutable "+kind, "name", obj.GetName(), "namespace", opts.Namespace, "baseName", opts.BaseName, "ownerName", owner.GetName(), "ownerUID", owner.GetUID())
	}

	return nil
}
