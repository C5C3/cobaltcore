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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/cache"
	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/plugins"
	"github.com/c5c3/forge/internal/common/policy"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// configRenderCacheEntry memoizes a successful config render. The (uid,
// generation, policyCMResourceVersion, domainsProjected,
// federationProjected, remoteIDAttribute) tuple is the cache key: generation
// covers every spec input to the render by construction, uid guards a
// same-name CR recreation, policyCMResourceVersion tracks the external
// oslo.policy ConfigMap referenced by spec.policyOverrides.configMapRef, and
// the projection fields track the identity-backend projection state —
// attaching or detaching a backend flips the [identity]
// domain-specific-driver options (LDAP) or the [auth]/[openid]/[federation]
// sections (OIDC) without bumping the Keystone generation, so both must
// invalidate the cache. useStderr is retained so the LoggingHealthy
// condition/event contract is preserved on the cache-hit path without
// re-deriving the merged config.
type configRenderCacheEntry struct {
	uid                     types.UID
	generation              int64
	policyCMResourceVersion string
	domainsProjected        bool
	federationProjected     bool
	remoteIDAttribute       string
	configMapName           string
	useStderr               string
}

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

// federationAuthMethods is the [auth] methods list rendered when federation
// is active: keystone's compiled-in default (verified identical against the
// pinned 2025.2/28.0.0 and 2026.1/29.0.0 keystone/conf/constants.py
// _DEFAULT_AUTH_METHODS) plus openid. Rendering the full explicit list —
// rather than only the addition — is how oslo.config works: setting the
// option replaces the default entirely, so dropping any entry here would
// silently break password/application-credential auth.
const federationAuthMethods = "external,password,token,oauth1,mapped,application_credential,openid"

// ssoCallbackTemplateFilePath is the on-pod path of the WebSSO callback
// template shipped in the config ConfigMap when federation is active. pip
// installs do not ship /etc/keystone/sso_callback_template.html, so the
// operator provides keystone's canonical template itself.
const ssoCallbackTemplateFilePath = "/etc/keystone/keystone.conf.d/sso_callback_template.html"

// ssoCallbackTemplateHTML is keystone's canonical etc/sso_callback_template.html
// (the 29.0.0 HTML5 revision; 28.0.0 differs only in XHTML syntax): an
// auto-submitting form POSTing the $token to the $host origin — the WebSSO
// hand-off back to the dashboard. Keystone substitutes $host/$token via
// Python string.Template at response time.
const ssoCallbackTemplateHTML = `<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <title>Keystone WebSSO redirect</title>
  </head>
  <body>
     <form id="sso" name="sso" action="$host" method="post">
       Please wait...
       <br>
       <input type="hidden" name="token" id="token" value="$token">
       <noscript>
         <input type="submit" name="submit_no_javascript" id="submit_no_javascript"
            value="If your JavaScript is disabled, please click to continue">
       </noscript>
     </form>
     <script>
       window.onload = function() {
         document.forms['sso'].submit();
       }
     </script>
  </body>
</html>
`

// reconcileConfig builds the Keystone configuration and creates an immutable
// ConfigMap containing keystone.conf, api-paste.ini, and optionally policy.yaml.
// It returns the name of the created ConfigMap (with content-hash suffix).
// domainsProjected reports whether reconcileIdentityBackends projected at
// least one per-domain config file; when true the rendered keystone.conf
// turns the domain-specific-drivers machinery on. fed is the federation
// projection (nil when no OIDC backend is projected); when set the rendered
// keystone.conf gains the openid auth method, the [openid]
// remote_id_attribute, and the [federation] section, and the ConfigMap ships
// the WebSSO callback template.
func (r *KeystoneReconciler) reconcileConfig(ctx context.Context, keystone *keystonev1alpha1.Keystone, domainsProjected bool, fed *federationProjection) (string, error) {
	// Cache short-circuit: the rendered ConfigMap is content-addressed and
	// immutable, and every spec input to the render bumps the CR generation, so
	// a matching (uid, generation, policy-ConfigMap ResourceVersion,
	// projection-state) tuple means the last rendered ConfigMap is still
	// current. Skip the INI/paste/policy rendering and the
	// immutable-ConfigMap write, but still re-run recordLoggingHealth so the
	// LoggingHealthy condition/event contract holds on every pass.
	policyCMRV, err := r.policyConfigMapResourceVersion(ctx, keystone)
	if err != nil {
		return "", err
	}
	if name, useStderr, ok := r.configRenderCacheHit(keystone, policyCMRV, domainsProjected, fed); ok {
		// Confirm the cached ConfigMap still exists: an out-of-band delete must
		// fall through to a full render/recreate. Owns(ConfigMap) enqueues us on
		// the delete, but the cache would otherwise hand back a deleted name.
		exists, existsErr := r.configMapExists(ctx, keystone.Namespace, name)
		if existsErr != nil {
			return "", existsErr
		}
		if exists {
			r.recordLoggingHealth(keystone, map[string]map[string]string{"DEFAULT": {"use_stderr": useStderr}})
			return name, nil
		}
		r.evictConfigRender(client.ObjectKeyFromObject(keystone))
		log.FromContext(ctx).V(1).Info("cached config ConfigMap missing; re-rendering", "configMap", name)
	}

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

	// Turn the domain-specific-drivers machinery on when at least one
	// identity backend is projected: keystone then scans domain_config_dir
	// for keystone.<domain>.conf files (the domains Secret mounted by every
	// workload builder). Placed in the defaults map so user extraConfig still
	// wins per MergeDefaults semantics. When nothing is projected the options
	// are omitted entirely, keeping zero-backend CRs byte-identical to the
	// pre-identity-backend render.
	if domainsProjected {
		defaults["identity"]["domain_specific_drivers_enabled"] = "true"
		defaults["identity"]["domain_config_dir"] = domainsMountPath
	}

	// Federation: enable the openid/mapped auth methods, point keystone at
	// the WSGI environ key the proxy asserts the issuer in (the per-protocol
	// [openid] section beats [federation].remote_id_attribute — the
	// spike-validated wiring), and configure the WebSSO callback template the
	// ConfigMap ships below. Placed in the defaults map so user extraConfig
	// (e.g. [federation] trusted_dashboard until the typed field lands) still
	// wins per MergeDefaults. When federation is inactive the sections are
	// omitted entirely, keeping non-federated CRs byte-identical.
	if fed != nil {
		defaults["auth"] = map[string]string{"methods": federationAuthMethods}
		defaults["openid"] = map[string]string{"remote_id_attribute": fed.RemoteIDAttribute}
		defaults["federation"] = map[string]string{"sso_callback_template": ssoCallbackTemplateFilePath}
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
	//
	// [federation] trusted_dashboard is an oslo MultiStrOpt: a dashboard origin
	// per line. Lift the merged single-valued map into the multi-valued shape
	// and set the repeated key there, after the extraConfig merge — the webhook
	// rejects declaring the option in both places, so nothing is being
	// overwritten here. A CR with no origins renders byte-identically to
	// before, since the lifted map is otherwise a one-element-slice image of
	// merged.
	multi := config.LiftSections(merged)
	if origins := trustedDashboards(keystone); len(origins) > 0 {
		if multi["federation"] == nil {
			multi["federation"] = map[string][]string{}
		}
		multi["federation"]["trusted_dashboard"] = origins
	}

	data := map[string]string{
		"keystone.conf": config.RenderINIMulti(multi),
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
	// Federation ships keystone's canonical WebSSO callback template beside
	// keystone.conf (pip installs do not provide it). oslo.config's
	// --config-dir only parses *.conf files, so the extra key is inert for
	// the config loader — the logging.conf precedent. Detaching the last OIDC
	// backend drops the key and the [auth]/[openid]/[federation] sections, so
	// the content hash changes and the Deployment rolls back to the
	// non-federated config.
	if fed != nil {
		data["sso_callback_template.html"] = ssoCallbackTemplateHTML
	}

	configMapName, err := config.CreateImmutableConfigMap(ctx, r.Client, r.Scheme, keystone,
		fmt.Sprintf("%s-config", keystone.Name), keystone.Namespace, data)
	if err != nil {
		return "", fmt.Errorf("creating config ConfigMap: %w", err)
	}

	// Memoize the render so a subsequent pass at the same generation, policy
	// ConfigMap ResourceVersion, and projection state returns this name
	// without re-rendering.
	r.storeConfigRender(keystone, policyCMRV, configMapName, mergedUseStderr(merged), domainsProjected, fed)

	return configMapName, nil
}

// policyConfigMapResourceVersion returns the ResourceVersion of the external
// oslo.policy ConfigMap referenced by spec.policyOverrides.configMapRef, or ""
// when none is referenced. It is the single render input that lives outside the
// CR spec, so it is folded into the config-render cache key. A NotFound is
// reported as "" so the render path runs and surfaces the missing-ConfigMap
// error via buildPolicyYAML rather than caching against a phantom.
func (r *KeystoneReconciler) policyConfigMapResourceVersion(ctx context.Context, keystone *keystonev1alpha1.Keystone) (string, error) {
	po := keystone.Spec.PolicyOverrides
	if po == nil || po.ConfigMapRef == nil {
		return "", nil
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: keystone.Namespace, Name: po.ConfigMapRef.Name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("getting policy ConfigMap %s: %w", po.ConfigMapRef.Name, err)
	}
	return cm.ResourceVersion, nil
}

// trustedDashboards returns the dashboard origins rendered as repeated
// [federation] trusted_dashboard lines. It is deliberately independent of the
// federationProjection: an operator may declare the trusted origin before the
// first OIDC backend attaches, so the [federation] section (and only this key
// in it) renders even when federation is otherwise inactive. It is a pure
// function of the spec, so it contributes nothing to the config-render cache
// key beyond the CR generation that already covers it.
func trustedDashboards(keystone *keystonev1alpha1.Keystone) []string {
	if keystone.Spec.Federation == nil {
		return nil
	}
	return keystone.Spec.Federation.TrustedDashboards
}

// federationCacheKeyOf extracts the two config-render inputs a federation
// projection contributes to the cache key.
func federationCacheKeyOf(fed *federationProjection) (projected bool, remoteIDAttribute string) {
	if fed == nil {
		return false, ""
	}
	return true, fed.RemoteIDAttribute
}

// configRenderCacheHit reports whether the memoized render for this CR is still
// valid: matching UID, generation, policy-ConfigMap ResourceVersion, and
// identity-backend projection state (domains and federation).
func (r *KeystoneReconciler) configRenderCacheHit(keystone *keystonev1alpha1.Keystone, policyCMRV string, domainsProjected bool, fed *federationProjection) (name, useStderr string, ok bool) {
	federationProjected, remoteIDAttribute := federationCacheKeyOf(fed)
	r.configRenderCacheMu.Lock()
	defer r.configRenderCacheMu.Unlock()
	entry, found := r.configRenderCache[client.ObjectKeyFromObject(keystone)]
	if !found {
		return "", "", false
	}
	if entry.uid != keystone.UID || entry.generation != keystone.Generation ||
		entry.policyCMResourceVersion != policyCMRV || entry.domainsProjected != domainsProjected ||
		entry.federationProjected != federationProjected || entry.remoteIDAttribute != remoteIDAttribute {
		return "", "", false
	}
	return entry.configMapName, entry.useStderr, true
}

// storeConfigRender memoizes a successful render.
func (r *KeystoneReconciler) storeConfigRender(keystone *keystonev1alpha1.Keystone, policyCMRV, configMapName, useStderr string, domainsProjected bool, fed *federationProjection) {
	federationProjected, remoteIDAttribute := federationCacheKeyOf(fed)
	r.configRenderCacheMu.Lock()
	defer r.configRenderCacheMu.Unlock()
	if r.configRenderCache == nil {
		r.configRenderCache = make(map[types.NamespacedName]configRenderCacheEntry)
	}
	r.configRenderCache[client.ObjectKeyFromObject(keystone)] = configRenderCacheEntry{
		uid:                     keystone.UID,
		generation:              keystone.Generation,
		policyCMResourceVersion: policyCMRV,
		domainsProjected:        domainsProjected,
		federationProjected:     federationProjected,
		remoteIDAttribute:       remoteIDAttribute,
		configMapName:           configMapName,
		useStderr:               useStderr,
	}
}

// evictConfigRender drops the memoized render for a CR so the next reconcile
// re-renders. Called on CR deletion and when the cached ConfigMap has vanished.
func (r *KeystoneReconciler) evictConfigRender(key types.NamespacedName) {
	r.configRenderCacheMu.Lock()
	defer r.configRenderCacheMu.Unlock()
	delete(r.configRenderCache, key)
}

// configMapExists reports whether the named ConfigMap is present, treating
// NotFound as a clean "absent" rather than an error.
func (r *KeystoneReconciler) configMapExists(ctx context.Context, namespace, name string) (bool, error) {
	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking config ConfigMap %s: %w", name, err)
	}
	return true, nil
}

// mergedUseStderr extracts the effective [DEFAULT].use_stderr value from the
// merged config, defaulting to "true" (the operator-supplied default) when the
// key is somehow absent.
func mergedUseStderr(merged map[string]map[string]string) string {
	if d := merged["DEFAULT"]; d != nil {
		if v, ok := d["use_stderr"]; ok {
			return v
		}
	}
	return "true"
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

// resolveCacheServers returns the memcache server list based on the cache
// spec, delegating to the shared cache resolver.
func resolveCacheServers(keystone *keystonev1alpha1.Keystone) string {
	return cache.ResolveServers(&keystone.Spec.Cache)
}

// resolveDatabaseHost returns the database host:port based on the database
// spec, delegating to the shared database resolver.
func resolveDatabaseHost(keystone *keystonev1alpha1.Keystone) string {
	return database.ResolveHost(&keystone.Spec.Database, keystone.Namespace)
}

// dbPort returns the database port, defaulting to 3306 if not set.
func dbPort(keystone *keystonev1alpha1.Keystone) int32 {
	return database.Port(&keystone.Spec.Database)
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
