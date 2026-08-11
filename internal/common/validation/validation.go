// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package validation provides the webhook validators shared by the service
// operators, each returning a field.ErrorList the caller appends to its
// aggregate.
//
// DECISION (CEL-vs-webhook split): several of these rules also exist as CEL
// XValidation markers on the shared commonv1 types (the database and cache
// XORs, Dynamic-requires-clusterRef, the image tag/digest XOR). The CEL rules
// stay — they are the schema-layer gate the API server enforces even when the
// webhook is unavailable. The webhooks keep a defense-in-depth copy for
// objects that bypass schema validation (old objects, direct etcd writes),
// but that copy is THIS package's single implementation rather than a
// hand-rolled per-operator triplicate: each rule now exists exactly twice —
// once as CEL, once here.
package validation

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// DatabaseXOR enforces that exactly one of clusterRef (managed mode) or host
// (brownfield mode) is set, mirroring the CEL rule on commonv1.DatabaseSpec.
func DatabaseXOR(fldPath *field.Path, db *commonv1.DatabaseSpec) field.ErrorList {
	if (db.ClusterRef != nil) == (db.Host != "") {
		return field.ErrorList{field.Invalid(
			fldPath,
			*db,
			"exactly one of clusterRef or host must be set",
		)}
	}
	return nil
}

// CacheXOR enforces that exactly one of clusterRef (managed mode) or servers
// (brownfield mode) is set, mirroring the CEL rule on commonv1.CacheSpec.
func CacheXOR(fldPath *field.Path, cache *commonv1.CacheSpec) field.ErrorList {
	if (cache.ClusterRef != nil) == (len(cache.Servers) > 0) {
		return field.ErrorList{field.Invalid(
			fldPath,
			*cache,
			"exactly one of clusterRef or servers must be set",
		)}
	}
	return nil
}

// CacheNoControlChars rejects a newline or carriage return in the CacheSpec
// fields that reach the INI renderer verbatim. cache.ResolveServers joins
// spec.cache.servers with commas (brownfield mode) or derives
// "<clusterRef.name>:11211" (managed mode), and every consumer renders the
// result as "memcached_servers = %s" inside [keystone_authtoken]. A newline
// there injects an additional config line — smuggling a whole key past the
// (section, key)-keyed ownership and catalog gates that guard extraConfig,
// which is exactly how an attacker-controlled auth_url would reach
// keystonemiddleware. Its schema-layer twin is the items pattern on
// CacheSpec.Servers plus the XValidation rule on CacheSpec.ClusterRef.Name.
func CacheNoControlChars(fldPath *field.Path, cache *commonv1.CacheSpec) field.ErrorList {
	const detail = "value must not contain a newline or carriage return: it is rendered verbatim " +
		"into the service configuration file, so a newline injects arbitrary config lines"

	var allErrs field.ErrorList
	if cache.ClusterRef != nil && HasControlChars(cache.ClusterRef.Name) {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("clusterRef", "name"),
			cache.ClusterRef.Name,
			detail,
		))
	}
	for i, server := range cache.Servers {
		if HasControlChars(server) {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("servers").Index(i),
				server,
				detail,
			))
		}
	}
	return allErrs
}

// HasControlChars reports whether s contains a newline or carriage return. The
// INI renderer writes every section name, key and value verbatim, so either
// character lets one entry inject additional config lines. It is exported so
// the per-operator webhooks that guard their own typed spec fields share this
// one definition of the character set rather than each carrying a copy that
// could drift from it.
func HasControlChars(s string) bool {
	return strings.ContainsAny(s, "\n\r")
}

// DynamicCredentialsRequireClusterRef enforces that CredentialsMode Dynamic
// (engine-issued credentials) is only used in managed mode (ClusterRef set),
// mirroring the second CEL rule on commonv1.DatabaseSpec.
func DynamicCredentialsRequireClusterRef(fldPath *field.Path, db *commonv1.DatabaseSpec) field.ErrorList {
	if db.CredentialsMode == commonv1.CredentialsModeDynamic && db.ClusterRef == nil {
		return field.ErrorList{field.Invalid(
			fldPath.Child("credentialsMode"),
			db.CredentialsMode,
			"credentialsMode Dynamic requires clusterRef (managed mode)",
		)}
	}
	return nil
}

// CronSchedule rejects a schedule cron.ParseStandard cannot parse. Callers
// keep their own empty-schedule guards — the required-vs-defaulted semantics
// (and the message naming the default) are per-field policy.
func CronSchedule(fldPath *field.Path, schedule string) field.ErrorList {
	if _, err := cron.ParseStandard(schedule); err != nil {
		return field.ErrorList{field.Invalid(
			fldPath,
			schedule,
			fmt.Sprintf("invalid cron expression: %v", err),
		)}
	}
	return nil
}

// TopologySpreadSelector enforces that every custom TopologySpreadConstraint
// carries a labelSelector whose matchLabels equal the Deployment's selector
// labels exactly, with no matchExpressions. A selector that widens or narrows
// beyond the Deployment's intent would spread (or fail to spread) the wrong
// pods.
func TopologySpreadSelector(fldPath *field.Path, tscs []corev1.TopologySpreadConstraint, requiredMatchLabels map[string]string) field.ErrorList {
	var allErrs field.ErrorList
	for i, tsc := range tscs {
		if tsc.LabelSelector == nil {
			allErrs = append(allErrs, field.Required(
				fldPath.Index(i).Child("labelSelector"),
				"labelSelector is required on each TopologySpreadConstraint",
			))
			continue
		}
		if !maps.Equal(tsc.LabelSelector.MatchLabels, requiredMatchLabels) {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Index(i).Child("labelSelector"),
				tsc.LabelSelector.MatchLabels,
				fmt.Sprintf("labelSelector.matchLabels must equal the Deployment selector labels %v", requiredMatchLabels),
			))
		}
		// Reject MatchExpressions to prevent selectors that widen or narrow
		// beyond the Deployment's intent. Only exact matchLabels are allowed.
		if len(tsc.LabelSelector.MatchExpressions) > 0 {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Index(i).Child("labelSelector", "matchExpressions"),
				tsc.LabelSelector.MatchExpressions,
				"matchExpressions are not allowed; labelSelector must use matchLabels only",
			))
		}
	}
	return allErrs
}

// SecretStoreRef is the defense-in-depth twin of the CRD markers on
// commonv1.SecretStoreRefSpec: a nil ref is valid (the operators default to the
// shared cluster store), an empty name is field.Required, and a kind outside
// the {ClusterSecretStore, SecretStore} enum is field.NotSupported. It lets the
// webhook reject an invalid store reference that reached etcd through a bypass
// of schema validation.
func SecretStoreRef(fldPath *field.Path, ref *commonv1.SecretStoreRefSpec) field.ErrorList {
	if ref == nil {
		return nil
	}
	var allErrs field.ErrorList
	if ref.Name == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), "store name must be set"))
	}
	switch ref.Kind {
	case "", commonv1.SecretStoreKindCluster, commonv1.SecretStoreKindNamespaced:
		// Empty kind is accepted here and defaulted to ClusterSecretStore by
		// the CRD marker / EffectiveStoreRef.
	default:
		allErrs = append(allErrs, field.NotSupported(
			fldPath.Child("kind"),
			ref.Kind,
			[]string{string(commonv1.SecretStoreKindCluster), string(commonv1.SecretStoreKindNamespaced)},
		))
	}
	return allErrs
}

// TargetClusterRef is the defense-in-depth twin of the CRD markers on
// commonv1.TargetClusterRefSpec: a nil ref is valid (the CR's children are
// created on the management cluster the operator runs on) and an empty name is
// field.Required. It lets the webhook reject an unresolvable cluster reference
// that reached etcd through a bypass of schema validation.
func TargetClusterRef(fldPath *field.Path, ref *commonv1.TargetClusterRefSpec) field.ErrorList {
	if ref == nil {
		return nil
	}
	if ref.Name == "" {
		return field.ErrorList{field.Required(fldPath.Child("name"), "target cluster name must be set")}
	}
	return nil
}

// TargetClusterRefImmutable is the webhook-layer twin of the two transition CEL
// rules the workload CRDs carry on spec.targetClusterRef: adding the ref,
// removing it, or renaming it is rejected once the CR exists, because each of
// those strands the children already created on the previously selected
// cluster. Both messages contain the string the CEL rules pin, so a client sees
// the same wording whichever layer rejects the update.
func TargetClusterRefImmutable(fldPath *field.Path, oldRef, newRef *commonv1.TargetClusterRefSpec) field.ErrorList {
	if (oldRef == nil) != (newRef == nil) {
		return field.ErrorList{field.Invalid(
			fldPath,
			newRef,
			"targetClusterRef is immutable (adding or removing it after creation is not permitted)",
		)}
	}
	if oldRef == nil || oldRef.Name == newRef.Name {
		return nil
	}
	return field.ErrorList{field.Invalid(
		fldPath.Child("name"),
		newRef.Name,
		"targetClusterRef is immutable (the children already exist on the previously named cluster)",
	)}
}

// PriorityClassExists verifies that name references an existing
// scheduling.k8s.io/v1 PriorityClass, catching typos at admission time. A nil
// Reader or an empty name skips the check — programmatically constructed
// webhooks without a client stay permissive rather than failing closed on a
// lookup they cannot perform.
func PriorityClassExists(ctx context.Context, c client.Reader, fldPath *field.Path, name string) field.ErrorList {
	if name == "" || c == nil {
		return nil
	}
	pc := &schedulingv1.PriorityClass{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, pc); err != nil {
		if apierrors.IsNotFound(err) {
			return field.ErrorList{field.NotFound(fldPath, name)}
		}
		return field.ErrorList{field.InternalError(
			fldPath,
			fmt.Errorf("failed to look up PriorityClass: %w", err),
		)}
	}
	return nil
}
