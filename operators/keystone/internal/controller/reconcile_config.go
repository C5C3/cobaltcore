// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/plugins"
	"github.com/c5c3/forge/internal/common/policy"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// conditionTypeLoggingHealthy reflects whether the merged keystone.conf
// preserves the safe logging defaults required for kubectl logs / sidecar
// shippers to receive Keystone application records. The
// only currently-tracked failure mode is the StderrDisabled override emitted
// by spec.extraConfig; healthy reconciles set the condition to True so
// transitions are detectable. The condition is informational and is
// intentionally NOT added to subConditionTypes because a use_stderr=false
// override is an explicit operator choice, not a Ready-blocking failure.
const conditionTypeLoggingHealthy = "LoggingHealthy"

// conditionReasonStderrDisabled is the Reason emitted on the
// LoggingHealthy=False condition (and on the gated LoggingStderrDisabled
// Warning event) when spec.extraConfig overrides [DEFAULT].use_stderr to a
// non-"true" value.
const conditionReasonStderrDisabled = "StderrDisabled"

// conditionReasonStderrEnabled is the Reason emitted on the
// LoggingHealthy=True condition when the merged [DEFAULT].use_stderr is
// "true" — i.e. container stderr will receive oslo.log records as designed
const conditionReasonStderrEnabled = "StderrEnabled"

// defaultConfigMapRetainCount is the number of historical immutable ConfigMaps
// to retain after pruning. Combined with the current active ConfigMap, this
// allows rollback to 3 previous configurations.
const defaultConfigMapRetainCount = 3

// dbConnectionPlaceholder is the placeholder URL written to the [database]
// connection key in keystone.conf. The real URL is injected
// at runtime via the OS_DATABASE__CONNECTION env var sourced from the derived
// <keystone.Name>-db-connection Secret, using oslo.config's
// OS_<GROUP>__<OPTION> environment override. The placeholder MUST be a
// syntactically valid pymysql URL so oslo.config can parse the file cleanly
// before the env override is applied.
const dbConnectionPlaceholder = "mysql+pymysql://placeholder"

// loggingConfFilePath is the on-pod path where the operator writes the
// oslo.log fileConfig snippet rendered by renderLoggingConf when
// spec.logging.format == "json". The same value is set as the
// [DEFAULT].log_config_append entry in keystone.conf, so the renderer and
// the keystone.conf builder must agree on a single source of truth.
const loggingConfFilePath = "/etc/keystone/keystone.conf.d/logging.conf"

// reconcileConfig builds the Keystone configuration and creates an immutable
// ConfigMap containing keystone.conf, api-paste.ini, and optionally policy.yaml.
// It returns the name of the created ConfigMap (with content-hash suffix).
func (r *KeystoneReconciler) reconcileConfig(ctx context.Context, keystone *keystonev1alpha1.Keystone) (string, error) {
	// Step 1: Build keystone.conf INI sections from CRD spec.
	logging := effectiveLogging(keystone.Spec.Logging)
	defaults := map[string]map[string]string{
		"DEFAULT": {
			"keystone_user":  "",
			"keystone_group": "",
			// route oslo.log records to stderr so kubectl logs
			// surfaces them. Users may override via spec.extraConfig — the
			// post-merge guard below emits a Warning event when they do.
			"use_stderr": "true",
			// oslo.log gates several extra-verbose code paths
			// (SQL echo, auth backend tracing) on the debug flag specifically,
			// independent of root logger level. Debug is a nil-preserving *bool:
			// nil means "unset", which renders as the default (false).
			"debug": fmt.Sprintf("%t", logging.Debug != nil && *logging.Debug),
		},
		"token": {
			"provider": "fernet",
		},
		"fernet_tokens": {
			"key_repository":  "/etc/keystone/fernet-keys",
			"max_active_keys": fmt.Sprintf("%d", normalizedFernetMaxActiveKeys(keystone)),
		},
		"credential": {
			"key_repository":  "/etc/keystone/credential-keys",
			"max_active_keys": fmt.Sprintf("%d", normalizedCredentialMaxActiveKeys(keystone)),
		},
		"cache": {
			"enabled": "true",
			"backend": keystone.Spec.Cache.Backend,
		},
		"paste_deploy": {
			"config_file": "/etc/keystone/keystone.conf.d/api-paste.ini",
		},
		"oslo_middleware": {
			"enable_proxy_headers_parsing": "true",
		},
		"oslo_policy": {
			"enforce_scope":        "true",
			"enforce_new_defaults": "true",
		},
		"identity": {
			"default_domain_id": "default",
		},
		"database": {
			"max_retries":             "-1",
			"connection_recycle_time": "600",
			// The real URL is materialized by reconcileDBConnectionSecret into a
			// derived Secret and injected at runtime via OS_DATABASE__CONNECTION
			// (oslo.config env override)..
			"connection": dbConnectionPlaceholder,
		},
	}

	// render PerLoggerLevels into oslo.log's default_log_levels
	// CSV with alphabetically sorted keys for deterministic ConfigMap content
	// hashing. Empty maps omit the key entirely so oslo.log keeps its compiled-in
	// defaults rather than overriding them with an empty list.
	if v := renderDefaultLogLevels(logging.PerLoggerLevels); v != "" {
		defaults["DEFAULT"]["default_log_levels"] = v
	}
	// when format=json the operator renders a logging.conf into
	// the same ConfigMap and points oslo.log at it via log_config_append. Placed
	// in the defaults map (not after the merge) so users may still override the
	// path via spec.extraConfig if they ship their own logging.conf alongside.
	if logging.Format == "json" {
		defaults["DEFAULT"]["log_config_append"] = loggingConfFilePath
	}

	// Step 2: Resolve cache servers.
	serverList := resolveCacheServers(keystone)
	defaults["cache"]["memcache_servers"] = serverList
	defaults["memcache"] = map[string]string{
		"servers": serverList,
	}

	merged := defaults

	// Step 3: Merge plugin config.
	if len(keystone.Spec.Plugins) > 0 {
		pluginConfig, err := plugins.RenderPluginConfig(keystone.Spec.Plugins)
		if err != nil {
			return "", fmt.Errorf("rendering plugin config: %w", err)
		}
		merged = config.MergeDefaults(pluginConfig, defaults)
	}

	// Step 4: Merge extraConfig (extraConfig overrides everything).
	if keystone.Spec.ExtraConfig != nil {
		merged = config.MergeDefaults(keystone.Spec.ExtraConfig, merged)
	}

	// surface the corner case where spec.extraConfig overrode
	// the safe [DEFAULT].use_stderr=true default. We honour the user's override
	// (otherwise extraConfig would not be a true escape hatch) but warn loudly
	// because kubectl logs / sidecar shippers will then see nothing. The
	// Warning event is gated on a transition into LoggingHealthy=False so the
	// 10s reconcile poll does not flood the event stream (gated-event pattern). LoggingHealthy is always set so the condition reflects the
	// current state on every reconcile, regardless of transition.
	r.recordLoggingHealth(keystone, merged)

	// Step 5: Handle PolicyOverrides.
	var policyYAML string
	if keystone.Spec.PolicyOverrides != nil {
		yaml, err := buildPolicyYAML(ctx, r.Client, keystone)
		if err != nil {
			return "", fmt.Errorf("building policy: %w", err)
		}
		policyYAML = yaml
		if policyYAML != "" {
			merged = config.InjectOsloPolicyConfig(merged, "/etc/keystone/keystone.conf.d/policy.yaml")
		}
	}

	// Step 6: Render api-paste.ini.
	apiPasteINI, err := plugins.RenderPastePipelineINI(plugins.PipelineSpec{
		PipelineName: "public_api",
		AppName:      "admin_service",
		AppFactory:   "egg:keystone#service_v3",
		BaseFilters:  []string{"cors", "sizelimit", "http_proxy_to_wsgi", "url_normalize", "request_id"},
		BaseFilterFactories: map[string]string{
			"cors":               "egg:oslo.middleware#cors",
			"sizelimit":          "egg:oslo.middleware#sizelimit",
			"http_proxy_to_wsgi": "egg:oslo.middleware#http_proxy_to_wsgi",
			"url_normalize":      "egg:keystone#url_normalize",
			"request_id":         "egg:oslo.middleware#request_id",
		},
		BaseFilterConfigs: map[string]map[string]string{
			"cors": {"oslo_config_project": "keystone"},
		},
		CompositeRoutes: map[string]string{"/v3": "public_api"},
		Middleware:      keystone.Spec.Middleware,
	})
	if err != nil {
		return "", fmt.Errorf("rendering api-paste.ini: %w", err)
	}

	// Step 7: Create immutable ConfigMap.
	data := map[string]string{
		"keystone.conf": config.RenderINI(merged),
		"api-paste.ini": apiPasteINI,
	}
	if policyYAML != "" {
		data["policy.yaml"] = policyYAML
	}
	// when format=json, ship the oslo.log JSONFormatter config
	// alongside keystone.conf. log_config_append in [DEFAULT] (set above when
	// format=json) points oslo.log at this path. Toggling back to format=text
	// drops both the data key and the log_config_append entry — the resulting
	// content hash differs, so the immutable ConfigMap name changes and the
	// Deployment rolls.
	if logging.Format == "json" {
		data["logging.conf"] = renderLoggingConf(logging.Level)
	}

	configMapName, err := config.CreateImmutableConfigMap(ctx, r.Client, r.Scheme, keystone,
		fmt.Sprintf("%s-config", keystone.Name), keystone.Namespace, data)
	if err != nil {
		return "", fmt.Errorf("creating config ConfigMap: %w", err)
	}

	return configMapName, nil
}

// recordLoggingHealth maintains the LoggingHealthy status condition and
// emits the LoggingStderrDisabled Warning event only on a state transition
// into LoggingHealthy=False, Reason=StderrDisabled. The 10s polling cadence
// (RequeueDeploymentPolling) would otherwise flood the event stream because
// the use_stderr guard is purely a function of user spec and never changes
// between reconciles for a steady CR (gated-event pattern).
//
// The condition itself is upserted on every reconcile so status reflects the
// current logging shape even when no transition occurred — a brand-new CR
// admitted with use_stderr=false therefore still ends up with
// LoggingHealthy=False after the first reconcile, just without re-emitting
// the same Warning event on subsequent passes.
func (r *KeystoneReconciler) recordLoggingHealth(
	keystone *keystonev1alpha1.Keystone,
	merged map[string]map[string]string,
) {
	prev := conditions.GetCondition(keystone.Status.Conditions, conditionTypeLoggingHealthy)

	if merged["DEFAULT"] != nil && merged["DEFAULT"]["use_stderr"] != "true" {
		useStderr := merged["DEFAULT"]["use_stderr"]
		if prev == nil || prev.Status != metav1.ConditionFalse || prev.Reason != conditionReasonStderrDisabled {
			r.Recorder.Eventf(keystone, corev1.EventTypeWarning, "LoggingStderrDisabled",
				"spec.extraConfig overrode [DEFAULT].use_stderr to %q; container logs will not reach kubectl logs",
				useStderr)
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeLoggingHealthy,
			Status:             metav1.ConditionFalse,
			Reason:             conditionReasonStderrDisabled,
			ObservedGeneration: keystone.Generation,
			Message: fmt.Sprintf(
				"spec.extraConfig set [DEFAULT].use_stderr=%q; container logs will not reach kubectl logs",
				useStderr,
			),
		})
		return
	}

	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               conditionTypeLoggingHealthy,
		Status:             metav1.ConditionTrue,
		Reason:             conditionReasonStderrEnabled,
		ObservedGeneration: keystone.Generation,
		Message:            "[DEFAULT].use_stderr is true; oslo.log records reach container stderr",
	})
}

// pruneStaleConfigMaps removes historical immutable ConfigMaps that exceed
// the retain count, keeping only the newest historical ConfigMaps plus the
// currently active one.
func (r *KeystoneReconciler) pruneStaleConfigMaps(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) error {
	baseName := fmt.Sprintf("%s-config", keystone.Name)
	return config.PruneImmutableConfigMaps(ctx, r.Client, keystone, config.PruneOptions{
		BaseName:    baseName,
		Namespace:   keystone.Namespace,
		CurrentName: configMapName,
		Retain:      defaultConfigMapRetainCount,
	})
}

// resolveCacheServers returns the memcache server list based on the cache spec.
// In brownfield mode (Servers set), it joins them with commas.
// In managed mode (ClusterRef set), it constructs endpoints from the cluster name.
func resolveCacheServers(keystone *keystonev1alpha1.Keystone) string {
	if len(keystone.Spec.Cache.Servers) > 0 {
		return strings.Join(keystone.Spec.Cache.Servers, ",")
	}
	if keystone.Spec.Cache.ClusterRef != nil {
		// The memcached operator provisions a Deployment + headless Service.
		// Use the Service DNS name which resolves to all pod IPs.
		return fmt.Sprintf("%s:11211", keystone.Spec.Cache.ClusterRef.Name)
	}
	return ""
}

// resolveDatabaseHost returns the database host:port based on the database spec.
// In managed mode (ClusterRef set), it constructs a service DNS name.
// In brownfield mode (Host set), it uses the explicit host:port.
func resolveDatabaseHost(keystone *keystonev1alpha1.Keystone) string {
	if keystone.Spec.Database.ClusterRef != nil {
		return fmt.Sprintf("%s.%s.svc:%d",
			keystone.Spec.Database.ClusterRef.Name,
			keystone.Namespace,
			dbPort(keystone))
	}
	return fmt.Sprintf("%s:%d", keystone.Spec.Database.Host, dbPort(keystone))
}

// dbPort returns the database port, defaulting to 3306 if not set.
func dbPort(keystone *keystonev1alpha1.Keystone) int32 {
	if keystone.Spec.Database.Port > 0 {
		return keystone.Spec.Database.Port
	}
	return 3306
}

// buildPolicyYAML builds the policy.yaml content from PolicyOverrides.
func buildPolicyYAML(ctx context.Context, c client.Client, keystone *keystonev1alpha1.Keystone) (string, error) {
	po := keystone.Spec.PolicyOverrides
	if po == nil {
		return "", nil
	}

	var rules map[string]string

	// Load external policy from ConfigMap if set.
	if po.ConfigMapRef != nil {
		cmRules, err := policy.LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{
			Namespace: keystone.Namespace,
			Name:      po.ConfigMapRef.Name,
		})
		if err != nil {
			return "", fmt.Errorf("loading policy from ConfigMap: %w", err)
		}
		rules = cmRules
	}

	// Merge inline rules over external rules (inline wins).
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
// materializing the production defaults when spec.logging is nil. The
// defaulting webhook materializes the same baseline at admission, so this
// fallback only matters when a CR bypasses the webhook (e.g. a pre-existing
// CR observed by a freshly upgraded operator). Mirrors the UWSGISpec
// nil-tolerance pattern at reconcile_deployment.go:317.
func effectiveLogging(spec *keystonev1alpha1.LoggingSpec) keystonev1alpha1.LoggingSpec {
	out := keystonev1alpha1.LoggingSpec{Format: "text", Level: "INFO"}
	if spec == nil {
		return out
	}
	out = *spec
	if out.Format == "" {
		out.Format = "text"
	}
	if out.Level == "" {
		out.Level = "INFO"
	}
	return out
}

// renderDefaultLogLevels formats PerLoggerLevels as oslo.log's
// default_log_levels CSV ("name=LEVEL,..."), with keys sorted alphabetically
// so the rendered keystone.conf — and therefore the immutable ConfigMap
// content hash — is independent of Go's randomized map iteration order
// Returns "" for empty input so the caller can omit the
// key entirely rather than overriding oslo.log defaults with an empty list.
func renderDefaultLogLevels(perLogger map[string]string) string {
	if len(perLogger) == 0 {
		return ""
	}
	keys := make([]string, 0, len(perLogger))
	for k := range perLogger {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, perLogger[k]))
	}
	return strings.Join(pairs, ",")
}

// renderLoggingConf builds the logging.conf written to loggingConfFilePath
// and consumed by oslo.log via log_config_append when
// spec.logging.format == "json". It wires
// oslo_log.formatters.JSONFormatter to a stderr StreamHandler so the
// Keystone API container emits one JSON record per log line for direct
// ingest by Loki/OpenSearch.
//
// DECISION: the task brief describes a "four-section" file, but Python's
// logging.config.fileConfig grammar requires six sections — three index
// sections ([loggers], [handlers], [formatters]) plus one named subsection
// per declared root-logger / handler / formatter. Emitting fewer would not
// parse in oslo.log; this six-section shape is the minimal valid file and is
// pinned by TestReconcileConfig_LoggingJSONPathRendersLoggingConfAndAppend.
func renderLoggingConf(level string) string {
	return strings.Join([]string{
		"[loggers]",
		"keys = root",
		"",
		"[handlers]",
		"keys = stderr",
		"",
		"[formatters]",
		"keys = json",
		"",
		"[logger_root]",
		"level = " + level,
		"handlers = stderr",
		"",
		"[handler_stderr]",
		"class = StreamHandler",
		"args = (sys.stderr,)",
		// level = NOTSET defers filtering to [logger_root]: the handler emits
		// every record the root logger forwards. Hardcoding the root level
		// here would silently shadow spec.logging.level; do not "fix" this to
		// match the root level.
		"level = NOTSET",
		"formatter = json",
		"",
		"[formatter_json]",
		"class = oslo_log.formatters.JSONFormatter",
		"",
	}, "\n")
}
