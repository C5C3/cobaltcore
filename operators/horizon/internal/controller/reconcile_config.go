// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c5c3/forge/internal/common/cache"
	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/pysettings"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// Condition type and reason constants for ConfigReady.
const (
	conditionTypeConfigReady        = "ConfigReady"
	conditionReasonConfigRendered   = "ConfigRendered"
	conditionReasonConfigError      = "ConfigError"
	defaultConfigMapRetainCount     = 3
	localSettingsFileName           = "local_settings.py"
	secretKeyEnvVarName             = "HORIZON_SECRET_KEY"
	secretKeySettingName            = "SECRET_KEY"
	defaultDjangoSessionEngine      = "django.contrib.sessions.backends.signed_cookies"
	horizonStaticRoot               = "/var/lib/openstack/horizon-static"
	horizonLocalSettingsMountedPath = "/etc/openstack-dashboard/"
)

// reconcileConfig renders local_settings.py, creates an immutable ConfigMap
// holding it, and sets ConfigReady. It returns the name of the created
// ConfigMap (with content-hash suffix).
func (r *HorizonReconciler) reconcileConfig(ctx context.Context, horizon *horizonv1alpha1.Horizon) (string, error) {
	rendered, err := renderLocalSettings(horizon)
	if err != nil {
		return "", fmt.Errorf("rendering local_settings.py: %w", err)
	}

	configMapName, err := config.CreateImmutableConfigMap(ctx, r.Client, r.Scheme, horizon,
		fmt.Sprintf("%s-config", horizon.Name), horizon.Namespace,
		map[string]string{localSettingsFileName: rendered})
	if err != nil {
		return "", fmt.Errorf("creating config ConfigMap: %w", err)
	}

	conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
		Type:               conditionTypeConfigReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: horizon.Generation,
		Reason:             conditionReasonConfigRendered,
		Message:            "local_settings.py rendered",
	})
	return configMapName, nil
}

// renderLocalSettings builds the Django settings module for the dashboard.
// The operator defaults are merged with spec.extraConfig (user values win),
// then rendered via the shared pysettings renderer. SECRET_KEY never enters
// the rendered file: the preamble reads it from the HORIZON_SECRET_KEY env
// var sourced from spec.secretKeyRef (the webhook rejects a SECRET_KEY
// extraConfig override).
func renderLocalSettings(horizon *horizonv1alpha1.Horizon) (string, error) {
	settings, err := defaultSettings(horizon)
	if err != nil {
		return "", err
	}
	// extraConfig overrides everything (the true escape hatch) — except
	// SECRET_KEY, which is env-sourced and must never enter the ConfigMap. The
	// validating webhook rejects a SECRET_KEY override, but this render-time
	// guard is the fail-closed backstop for a bypassed webhook (old stored
	// objects, disabled admission, direct etcd writes): the preamble emits
	// SECRET_KEY before these assignments, so a later assignment would win in
	// Python and leak plaintext key material into a namespace-readable ConfigMap.
	for name, value := range horizon.Spec.ExtraConfig {
		if name == secretKeySettingName {
			continue
		}
		settings[name] = value
	}

	preamble := []string{
		"import os",
		"",
		"# Rendered by the horizon-operator; edits are overwritten on the next reconcile.",
		"# SECRET_KEY is injected via the pod environment (spec.secretKeyRef) so the",
		"# key material never enters this ConfigMap.",
		fmt.Sprintf("%s = os.environ[%q]", secretKeySettingName, secretKeyEnvVarName),
	}
	return pysettings.Render(preamble, settings)
}

// defaultSettings returns the operator-managed Django settings.
func defaultSettings(horizon *horizonv1alpha1.Horizon) (map[string]apiextensionsv1.JSON, error) {
	logging := effectiveLogging(horizon.Spec.Logging)

	values := map[string]any{
		// Signed-cookie sessions: no session state in Memcached or a
		// database, so cache loss degrades hit-rate without logging users out
		// and no database sub-reconcilers exist at all (design decision D1).
		"SESSION_ENGINE": defaultDjangoSessionEngine,
		"CACHES": map[string]any{
			"default": map[string]any{
				"BACKEND":  effectiveCacheBackend(horizon),
				"LOCATION": cacheLocations(horizon),
			},
		},
		"OPENSTACK_KEYSTONE_URL": horizon.Spec.KeystoneEndpoint,
		// The dashboard's server-side clients resolve service URLs from the
		// Keystone catalog, not from OPENSTACK_KEYSTONE_URL. The upstream
		// default ("publicURL") selects browser-facing catalog entries, which
		// a colocated in-cluster dashboard cannot reach when they point at a
		// host-only address (a kind port-mapping, an external LB). The
		// dashboard runs next to the services it fronts, so it uses the
		// internal interface — the same posture kolla-ansible and
		// openstack-helm ship. Deployments fronting a remote cloud override
		// this via spec.extraConfig. The legacy "...URL" spelling is
		// load-bearing: openstack_dashboard's ENDPOINT_TYPE_TO_INTERFACE map
		// only knows publicURL/internalURL/adminURL, and a bare "internal"
		// fails every catalog lookup.
		"OPENSTACK_ENDPOINT_TYPE": "internalURL",
		// ALLOWED_HOSTS is a closed allow-list of the Hosts the dashboard is
		// actually reached under (see allowedHosts). Keeping Django's
		// Host-header validation in force — rather than disabling it with "*"
		// — closes the Host-header poisoning class that a wildcard waves
		// through.
		"ALLOWED_HOSTS": allowedHosts(horizon),
		"DEBUG":         logging.Debug != nil && *logging.Debug,
		// Static assets are pre-built at image-build time with offline
		// compression; the runtime settings must agree or template rendering
		// fails looking for a missing manifest.
		"COMPRESS_OFFLINE": true,
		"STATIC_ROOT":      horizonStaticRoot,
		"WEBROOT":          "/",
		"LOGGING":          djangoLogging(logging),
	}

	maps.Copy(values, webSSOSettings(horizon))
	maps.Copy(values, multiDomainSettings(horizon))

	settings := make(map[string]apiextensionsv1.JSON, len(values))
	for name, v := range values {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encoding default setting %s: %w", name, err)
		}
		settings[name] = apiextensionsv1.JSON{Raw: raw}
	}
	return settings, nil
}

// webSSOSettings renders the WEBSSO_* Django settings from spec.websso.
// A nil or disabled block emits nothing at all, so a CR that never opts into
// federated login renders byte-identically to the pre-websso operator (the
// same convention keystone's reconcileConfig uses for a nil federation
// projection).
//
// WEBSSO_USE_HTTP_REFERER is pinned to False. It defaults to True upstream,
// which makes openstack_auth derive the Keystone URL it validates the returned
// token against from the browser's Referer header — i.e. from the EXTERNAL
// gateway hostname. That URL is resolved server-side from inside the pod,
// where the external name either does not resolve or (on kind, where
// *.127-0-0-1.nip.io resolves to 127.0.0.1) resolves to the dashboard pod
// itself. With False, openstack_auth validates against OPENSTACK_KEYSTONE_URL
// — spec.keystoneEndpoint, the cluster-local Service URL — which is exactly
// the address the dashboard can reach.
//
// WEBSSO_KEYSTONE_URL is the counterpart for the browser-facing leg: the SSO
// redirect the user's browser follows must point at the externally routable
// Keystone, never at the cluster-local Service URL. It is omitted when unset
// so Horizon falls back to OPENSTACK_KEYSTONE_URL.
func webSSOSettings(horizon *horizonv1alpha1.Horizon) map[string]any {
	websso := horizon.Spec.WebSSO
	if websso == nil || !websso.Enabled {
		return nil
	}

	// WEBSSO_CHOICES is a sequence of (id, label) pairs feeding a Django
	// ChoiceField; order is preserved so the operator's ordering (credentials
	// first) is what the login form shows.
	choices := make([]any, 0, len(websso.Choices))
	for _, c := range websso.Choices {
		choices = append(choices, []any{c.ID, c.Label})
	}

	values := map[string]any{
		horizonv1alpha1.SettingWebSSOEnabled:        true,
		horizonv1alpha1.SettingWebSSOChoices:        choices,
		horizonv1alpha1.SettingWebSSOUseHTTPReferer: false,
	}

	// WEBSSO_IDP_MAPPING maps a choice id onto an (identity provider, protocol)
	// pair. A choice absent from the mapping is a local login, which is how the
	// credentials fallback stays a username/password form — so an empty mapping
	// is omitted rather than rendered as an empty dict.
	if len(websso.IDPMapping) > 0 {
		mapping := make(map[string]any, len(websso.IDPMapping))
		for id, target := range websso.IDPMapping {
			mapping[id] = []any{target.IdentityProvider, target.Protocol}
		}
		values[horizonv1alpha1.SettingWebSSOIDPMapping] = mapping
	}
	if websso.InitialChoice != "" {
		values[horizonv1alpha1.SettingWebSSOInitialChoice] = websso.InitialChoice
	}
	if websso.KeystoneURL != "" {
		values[horizonv1alpha1.SettingWebSSOKeystoneURL] = websso.KeystoneURL
	}
	return values
}

// multiDomainSettings renders the OPENSTACK_KEYSTONE_MULTIDOMAIN_* Django
// settings from spec.multiDomain. A nil or disabled block emits nothing.
//
// The domain dropdown is gated twice, matching upstream: Horizon only consults
// OPENSTACK_KEYSTONE_DOMAIN_CHOICES when OPENSTACK_KEYSTONE_DOMAIN_DROPDOWN is
// True, so the choices are omitted with the dropdown to keep the rendered
// module free of settings that do nothing.
func multiDomainSettings(horizon *horizonv1alpha1.Horizon) map[string]any {
	md := horizon.Spec.MultiDomain
	if md == nil || !md.Enabled {
		return nil
	}

	defaultDomain := md.DefaultDomain
	if defaultDomain == "" {
		defaultDomain = horizonv1alpha1.DefaultMultiDomainDefaultDomain
	}
	values := map[string]any{
		horizonv1alpha1.SettingMultiDomainSupport:       true,
		horizonv1alpha1.SettingMultiDomainDefaultDomain: defaultDomain,
	}

	if md.DomainDropdown {
		choices := make([]any, 0, len(md.DomainChoices))
		for _, c := range md.DomainChoices {
			choices = append(choices, []any{c.Name, c.Label})
		}
		values[horizonv1alpha1.SettingMultiDomainDomainDropdown] = true
		values[horizonv1alpha1.SettingMultiDomainDomainChoices] = choices
	}
	return values
}

// allowedHosts returns the Django ALLOWED_HOSTS entries the dashboard must
// accept, as a closed allow-list rather than "*". Three Hosts reach the pod:
// the kubelet HTTP readiness/startup probes send a fixed probeHostHeader (the
// probes pin the Host header so it is not the dynamic pod IP — see
// reconcile_deployment.go), in-cluster clients reach the dashboard Service
// under any of the standard Kubernetes DNS forms of its name (the
// health-check sub-reconciler uses the FQDN, the e2e login fetch the .svc
// short form), and — when external exposure is configured — the Gateway
// forwards spec.gateway.hostname. All of these names are cluster-controlled,
// so listing them keeps the Host-header poisoning protection intact.
// Operators who front the dashboard with additional hostnames override
// ALLOWED_HOSTS via spec.extraConfig (user values win); ["*"] stays
// available there as an explicit, documented opt-out.
func allowedHosts(horizon *horizonv1alpha1.Horizon) []any {
	name := subResourceName(horizon)
	hosts := []any{
		probeHostHeader,
		name,
		fmt.Sprintf("%s.%s", name, horizon.Namespace),
		fmt.Sprintf("%s.%s.svc", name, horizon.Namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", name, horizon.Namespace),
	}
	if horizon.Spec.Gateway != nil && horizon.Spec.Gateway.Hostname != "" {
		hosts = append(hosts, horizon.Spec.Gateway.Hostname)
	}
	return hosts
}

// djangoLogging derives the Django LOGGING dictConfig from the shared
// LoggingSpec: a single stderr console handler so kubectl logs surfaces the
// dashboard records, the root logger at spec.logging.level, and one named
// logger per perLoggerLevels entry.
func djangoLogging(logging horizonv1alpha1.LoggingSpec) map[string]any {
	cfg := map[string]any{
		"version":                  1,
		"disable_existing_loggers": false,
		"handlers": map[string]any{
			"console": map[string]any{
				"class":  "logging.StreamHandler",
				"stream": "ext://sys.stderr",
			},
		},
		"root": map[string]any{
			"handlers": []any{"console"},
			"level":    logging.Level,
		},
	}
	if len(logging.PerLoggerLevels) > 0 {
		loggers := make(map[string]any, len(logging.PerLoggerLevels))
		for name, level := range logging.PerLoggerLevels {
			loggers[name] = map[string]any{"level": level}
		}
		cfg["loggers"] = loggers
	}
	return cfg
}

// effectiveLogging returns the LoggingSpec to use for config rendering,
// materializing the production defaults when spec.logging is nil. The
// defaulting webhook materializes the same baseline at admission, so this
// fallback only matters when a CR bypasses the webhook.
func effectiveLogging(spec *horizonv1alpha1.LoggingSpec) horizonv1alpha1.LoggingSpec {
	out := horizonv1alpha1.LoggingSpec{Format: "text", Level: "INFO"}
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

// effectiveCacheBackend returns the Django cache backend, defaulting to
// PyMemcacheCache when spec.cache.backend is empty (a CR that bypassed the
// defaulting webhook).
func effectiveCacheBackend(horizon *horizonv1alpha1.Horizon) string {
	if backend := horizon.Spec.Cache.Backend; backend != "" {
		return backend
	}
	return horizonv1alpha1.DefaultCacheBackend
}

// cacheLocations resolves the Memcached endpoint list for the Django CACHES
// LOCATION entry, delegating to the shared cache resolver and splitting its
// comma-joined form into the list Django expects.
func cacheLocations(horizon *horizonv1alpha1.Horizon) []any {
	joined := cache.ResolveServers(&horizon.Spec.Cache)
	if joined == "" {
		return nil
	}
	servers := strings.Split(joined, ",")
	locations := make([]any, 0, len(servers))
	for _, s := range servers {
		locations = append(locations, s)
	}
	return locations
}

// pruneStaleConfigMaps removes historical immutable ConfigMaps that exceed
// the retain count, keeping only the newest historical ConfigMaps plus the
// currently active one.
func (r *HorizonReconciler) pruneStaleConfigMaps(ctx context.Context, horizon *horizonv1alpha1.Horizon, configMapName string) error {
	baseName := fmt.Sprintf("%s-config", horizon.Name)
	return config.PruneImmutableConfigMaps(ctx, r.Client, horizon, config.PruneOptions{
		BaseName:    baseName,
		Namespace:   horizon.Namespace,
		CurrentName: configMapName,
		Retain:      defaultConfigMapRetainCount,
	})
}
