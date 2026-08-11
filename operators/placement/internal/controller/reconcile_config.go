// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/cache"
	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/keystoneauth"
	"github.com/c5c3/forge/internal/common/policy"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

// defaultConfigMapRetainCount is the number of historical immutable ConfigMaps
// to retain after pruning. Combined with the current active artefact, this
// allows rollback to 3 previous configurations.
const defaultConfigMapRetainCount = 3

// placementConfigDir is the in-pod directory the rendered config ConfigMap is
// mounted at, read-only and as a whole volume, shadowing the image's own
// /etc/placement. The deployment step points the container at it through
// OS_PLACEMENT_CONFIG_DIR=/etc/placement, and the db-sync Job passes
// --config-file /etc/placement/placement.conf.
//
// The directory is NOT a conf.d: the image's WSGI entry point calls
// placement.wsgi:init_application, whose _get_config_files loads exactly
// $OS_PLACEMENT_CONFIG_DIR/placement.conf — one file, with no directory scan and
// no argv parsing. The ConfigMap's other data keys (policy.yaml, logging.conf)
// are therefore inert for the config loader and are referenced explicitly by the
// options that consume them.
const placementConfigDir = "/etc/placement/"

// File paths inside placementConfigDir.
const (
	// policyFilePath is the oslo.policy path [oslo_policy] policy_file points at
	// when spec.policyOverrides is set.
	policyFilePath = placementConfigDir + "policy.yaml"
	// loggingConfFilePath is the oslo.log fileConfig path [DEFAULT]
	// log_config_append points at when spec.logging.format == "json".
	loggingConfFilePath = placementConfigDir + "logging.conf"
)

// dbConnectionPlaceholder is the placeholder URL written to the
// [placement_database] connection key in placement.conf. The real URL is
// injected at runtime via the OS_PLACEMENT_DATABASE__CONNECTION env var sourced
// from the derived <placement.Name>-db-connection Secret (oslo.config
// OS_<GROUP>__<OPTION> override). The placeholder MUST be a syntactically valid
// pymysql URL so oslo.config parses the file cleanly before the env override is
// applied.
const dbConnectionPlaceholder = "mysql+pymysql://placeholder"

// reconcileConfig renders placement.conf into an immutable ConfigMap (plus
// policy.yaml / logging.conf when applicable) and returns the ConfigMap name for
// the database and deployment steps. Config failures flip SecretsReady=False
// (the Config→SecretsReady mapping the sibling operators use) so the aggregate
// Ready cannot stay stale-True at the new generation.
func (r *PlacementReconciler) reconcileConfig(ctx context.Context, children client.Client, placement *placementv1alpha1.Placement) (ctrl.Result, string, error) {
	// The extraConfig ownership guard is a pure function of the spec. The
	// ExtraConfigHealthy condition is informational and deliberately stays out of
	// subConditionTypes and subReconcilerConditionTypes.
	config.RecordExtraConfigHealth(r.Recorder, placement, &placement.Status.Conditions, placement.Generation,
		config.FindOwnedOverrides(placement.Spec.ExtraConfig, placementv1alpha1.OwnedConfigKeys))

	merged := operatorDefaults(placement)

	// logging is re-derived here (operatorDefaults keeps its own copy) for the
	// logging.conf data-key decision below.
	logging := effectiveLogging(placement.Spec.Logging)

	// extraConfig overrides everything (the true escape hatch).
	if placement.Spec.ExtraConfig != nil {
		merged = config.MergeDefaults(placement.Spec.ExtraConfig, merged)
	}

	// Handle PolicyOverrides: render policy.yaml and wire oslo_policy.policy_file.
	var policyYAML string
	if placement.Spec.PolicyOverrides != nil {
		rendered, err := buildPolicyYAML(ctx, children, placement)
		if err != nil {
			markConfigFailed(placement, err)
			return ctrl.Result{}, "", fmt.Errorf("building policy: %w", err)
		}
		policyYAML = rendered
		if policyYAML != "" {
			merged = config.InjectOsloPolicyConfig(merged, policyFilePath)
		}
	}

	data := map[string]string{
		"placement.conf": config.RenderINI(merged),
	}
	if policyYAML != "" {
		data["policy.yaml"] = policyYAML
	}
	if logging.Format == "json" {
		data["logging.conf"] = config.RenderLoggingConf(logging.Level)
	}

	configMapName, err := config.CreateImmutableConfigMap(ctx, children, r.Scheme, placement,
		placement.Name+"-config", placement.Namespace, data)
	if err != nil {
		markConfigFailed(placement, err)
		return ctrl.Result{}, "", fmt.Errorf("creating config ConfigMap: %w", err)
	}
	if err := config.PruneImmutableConfigMaps(ctx, children, placement, config.PruneOptions{
		BaseName:    placement.Name + "-config",
		Namespace:   placement.Namespace,
		CurrentName: configMapName,
		Retain:      defaultConfigMapRetainCount,
	}); err != nil {
		markConfigFailed(placement, err)
		return ctrl.Result{}, "", fmt.Errorf("pruning config ConfigMaps: %w", err)
	}

	return ctrl.Result{}, configMapName, nil
}

// operatorDefaults builds the operator-owned placement.conf sections from the
// CRD spec: the static [DEFAULT]/[api]/[placement_database]/[keystone_authtoken]
// scaffolding plus the per-logger-level and json-logging conditionals. It is a
// pure function of the spec (no cluster access), so the registry drift-guard
// test can call it directly to assert the rendered defaults stay in lockstep
// with placementv1alpha1.OwnedConfigKeys.
//
// Nothing here depends on spec.openStackRelease: placement has shipped the same
// WSGI application and the same option names across the supported releases, so
// two CRs differing only in their release render byte-identical files.
func operatorDefaults(placement *placementv1alpha1.Placement) map[string]map[string]string {
	logging := effectiveLogging(placement.Spec.Logging)
	defaults := map[string]map[string]string{
		"DEFAULT": {
			// Route oslo.log records to stderr so kubectl logs surfaces them.
			"use_stderr": "true",
			// oslo.log gates several extra-verbose code paths on the debug flag
			// specifically, independent of the root logger level.
			"debug": fmt.Sprintf("%t", *logging.Debug),
		},
		// Placement reads auth_strategy from its own [api] section; the [DEFAULT]
		// option of that name is deprecated upstream.
		"api": {
			"auth_strategy": "keystone",
		},
		// Placement keeps its database options in [placement_database], not in the
		// [database] section its sibling services use.
		"placement_database": {
			"max_retries":             "-1",
			"connection_recycle_time": "600",
			// The real URL is materialized by reconcileDBConnectionSecret into a
			// derived Secret and injected at runtime via
			// OS_PLACEMENT_DATABASE__CONNECTION.
			"connection": dbConnectionPlaceholder,
		},
		"keystone_authtoken": keystoneauth.Section(keystoneauth.SectionParams{
			AuthURL:            placement.Spec.KeystoneEndpoint,
			WWWAuthenticateURI: placement.Spec.EffectiveKeystonePublicEndpoint(),
			Username:           placement.Spec.ServiceUser.Username,
			ProjectName:        placement.Spec.ServiceUser.ProjectName,
			UserDomainName:     placement.Spec.ServiceUser.UserDomainName,
			ProjectDomainName:  placement.Spec.ServiceUser.ProjectDomainName,
			RegionName:         placement.Spec.Region,
			MemcachedServers:   cache.ResolveServers(&placement.Spec.Cache),
		}),
	}

	// PerLoggerLevels render into oslo.log's default_log_levels CSV; empty omits
	// the key so oslo.log keeps its compiled-in defaults.
	if v := config.RenderSortedPairs(logging.PerLoggerLevels, "="); v != "" {
		defaults["DEFAULT"]["default_log_levels"] = v
	}
	// format=json ships a logging.conf and points oslo.log at it via
	// log_config_append.
	if logging.Format == "json" {
		defaults["DEFAULT"]["log_config_append"] = loggingConfFilePath
	}

	return defaults
}

// buildPolicyYAML builds the policy.yaml content from spec.policyOverrides,
// merging inline rules over any ConfigMap-sourced rules (inline wins).
func buildPolicyYAML(ctx context.Context, c client.Client, placement *placementv1alpha1.Placement) (string, error) {
	po := placement.Spec.PolicyOverrides
	if po == nil {
		return "", nil
	}

	var rules map[string]string
	if po.ConfigMapRef != nil {
		cmRules, err := policy.LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{
			Namespace: placement.Namespace,
			Name:      po.ConfigMapRef.Name,
		})
		if err != nil {
			return "", fmt.Errorf("loading policy from ConfigMap: %w", err)
		}
		rules = cmRules
	}
	if len(po.Rules) > 0 {
		if rules == nil {
			rules = make(map[string]string)
		}
		for k, v := range po.Rules {
			rules[k] = v
		}
	}
	return policy.RenderPolicyYAML(rules)
}

// effectiveLogging returns the LoggingSpec to use for config rendering,
// materializing the production defaults (text/INFO/debug=false) through the
// shared LoggingSpec.Default when spec.logging is nil or partially filled — the
// case of a CR that bypassed the defaulting webhook. The returned value is a
// copy, so defaulting never writes back into the CR, and its Debug pointer is
// always non-nil.
func effectiveLogging(spec *placementv1alpha1.LoggingSpec) placementv1alpha1.LoggingSpec {
	var out placementv1alpha1.LoggingSpec
	if spec != nil {
		out = *spec
	}
	out.Default()
	return out
}
