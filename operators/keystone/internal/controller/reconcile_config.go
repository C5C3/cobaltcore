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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/cache"
	"github.com/c5c3/cobaltcore/internal/common/config"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/plugins"
	"github.com/c5c3/cobaltcore/internal/common/policy"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
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
// invalidate the cache.
type configRenderCacheEntry struct {
	uid                     types.UID
	generation              int64
	policyCMResourceVersion string
	domainsProjected        bool
	federationProjected     bool
	remoteIDAttribute       string
	samlProtocolID          string
	samlRemoteIDAttribute   string
	configMapName           string
}

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
// oslo.log fileConfig snippet rendered by config.RenderLoggingConf when
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
func (r *KeystoneReconciler) reconcileConfig(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone, domainsProjected bool, fed *federationProjection) (string, error) {
	// The extraConfig ownership guard is a pure function of the spec, so it
	// runs before the render-cache short-circuit and needs no render or cache
	// participation. The ExtraConfigHealthy condition is informational and
	// deliberately stays out of subConditionTypes (Ready aggregation) and
	// subReconcilerConditionTypes (metrics attribution).
	config.RecordExtraConfigHealth(r.Recorder, keystone, &keystone.Status.Conditions, keystone.Generation,
		config.FindOwnedOverrides(keystone.Spec.ExtraConfig, keystonev1alpha1.OwnedConfigKeys))

	// Cache short-circuit: the rendered ConfigMap is content-addressed and
	// immutable, and every spec input to the render bumps the CR generation, so
	// a matching (uid, generation, policy-ConfigMap ResourceVersion,
	// projection-state) tuple means the last rendered ConfigMap is still
	// current. Skip the INI/paste/policy rendering and the immutable-ConfigMap
	// write; the extraConfig ownership guard above already ran, so it needs no
	// re-run here.
	policyCMRV, err := r.policyConfigMapResourceVersion(ctx, children, keystone)
	if err != nil {
		return "", err
	}
	if name, ok := r.configRenderCacheHit(keystone, policyCMRV, domainsProjected, fed); ok {
		// Confirm the cached ConfigMap still exists: an out-of-band delete must
		// fall through to a full render/recreate. Owns(ConfigMap) enqueues us on
		// the delete, but the cache would otherwise hand back a deleted name.
		exists, existsErr := r.configMapExists(ctx, children, keystone.Namespace, name)
		if existsErr != nil {
			return "", existsErr
		}
		if exists {
			return name, nil
		}
		r.evictConfigRender(client.ObjectKeyFromObject(keystone))
		log.FromContext(ctx).V(1).Info("cached config ConfigMap missing; re-rendering", "configMap", name)
	}

	// Step 1: Build the operator-owned keystone.conf defaults from the spec.
	defaults := operatorDefaults(keystone, domainsProjected, fed)

	// logging is re-derived here (operatorDefaults keeps its own copy) for the
	// later logging.conf data-key decisions below.
	logging := effectiveLogging(keystone.Spec.Logging)

	merged := defaults

	// Step 2: Merge plugin config (operator defaults win over plugin sections
	// that collide, then user extraConfig wins over both).
	if len(keystone.Spec.Plugins) > 0 {
		pluginConfig, err := plugins.RenderPluginConfig(keystone.Spec.Plugins)
		if err != nil {
			return "", fmt.Errorf("rendering plugin config: %w", err)
		}
		merged = config.MergeDefaults(defaults, pluginConfig)
	}

	// Step 3: Merge extraConfig (extraConfig overrides everything).
	if keystone.Spec.ExtraConfig != nil {
		merged = config.MergeDefaults(keystone.Spec.ExtraConfig, merged)
	}

	// Step 4: Handle PolicyOverrides.
	var policyYAML string
	if keystone.Spec.PolicyOverrides != nil {
		yaml, err := buildPolicyYAML(ctx, children, keystone)
		if err != nil {
			return "", fmt.Errorf("building policy: %w", err)
		}
		policyYAML = yaml
		if policyYAML != "" {
			merged = config.InjectOsloPolicyConfig(merged, "/etc/keystone/keystone.conf.d/policy.yaml")
		}
	}

	// Step 5: Render api-paste.ini.
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

	// Step 6: Create immutable ConfigMap.
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
		data["logging.conf"] = config.RenderLoggingConf(logging.Level)
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

	configMapName, err := config.CreateImmutableConfigMap(ctx, children, r.Scheme, keystone,
		fmt.Sprintf("%s-config", keystone.Name), keystone.Namespace, data)
	if err != nil {
		return "", fmt.Errorf("creating config ConfigMap: %w", err)
	}

	// Memoize the render so a subsequent pass at the same generation, policy
	// ConfigMap ResourceVersion, and projection state returns this name
	// without re-rendering.
	r.storeConfigRender(keystone, policyCMRV, configMapName, domainsProjected, fed)

	return configMapName, nil
}

// operatorDefaults builds the operator-owned keystone.conf sections from the
// CRD spec: the static [DEFAULT]/[token]/… scaffolding plus the
// domain-projection, federation, per-logger-level, json-logging, and cache
// server conditionals. It is a pure function of the spec (no cluster access),
// so the registry drift-guard test can call it directly to assert the rendered
// defaults stay in lockstep with keystonev1alpha1.OwnedConfigKeys.
func operatorDefaults(keystone *keystonev1alpha1.Keystone, domainsProjected bool, fed *federationProjection) map[string]map[string]string {
	logging := effectiveLogging(keystone.Spec.Logging)
	defaults := map[string]map[string]string{
		"DEFAULT": {
			"keystone_user":  "",
			"keystone_group": "",
			// route oslo.log records to stderr so kubectl logs
			// surfaces them. Users may override via spec.extraConfig — the
			// extraConfig ownership guard reports the override.
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
		defaults["federation"] = map[string]string{"sso_callback_template": ssoCallbackTemplateFilePath}
		// OIDC reads the asserted issuer from [openid] remote_id_attribute;
		// rendered only when an OIDC backend is projected.
		if fed.RemoteIDAttribute != "" {
			defaults["openid"] = map[string]string{"remote_id_attribute": fed.RemoteIDAttribute}
		}
		// SAML reads the asserted IdP entityID from a per-protocol
		// [<protocolID>] remote_id_attribute section (keystone registers the
		// group dynamically — the same per-protocol-beats-[federation] mechanism
		// as [openid]); rendered only when a SAML backend is projected. The
		// webhook rejects a protocolID that collides with an operator-owned
		// section, so this can never clobber one.
		if fed.SAMLProtocolID != "" {
			defaults[fed.SAMLProtocolID] = map[string]string{"remote_id_attribute": fed.SAMLRemoteIDAttribute}
		}
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

	// Resolve cache servers.
	serverList := resolveCacheServers(keystone)
	defaults["cache"]["memcache_servers"] = serverList
	defaults["memcache"] = map[string]string{
		"servers": serverList,
	}

	return defaults
}

// policyConfigMapResourceVersion returns the ResourceVersion of the external
// oslo.policy ConfigMap referenced by spec.policyOverrides.configMapRef, or ""
// when none is referenced. It is the single render input that lives outside the
// CR spec, so it is folded into the config-render cache key. A NotFound is
// reported as "" so the render path runs and surfaces the missing-ConfigMap
// error via buildPolicyYAML rather than caching against a phantom.
func (r *KeystoneReconciler) policyConfigMapResourceVersion(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone) (string, error) {
	po := keystone.Spec.PolicyOverrides
	if po == nil || po.ConfigMapRef == nil {
		return "", nil
	}
	var cm corev1.ConfigMap
	if err := children.Get(ctx, client.ObjectKey{Namespace: keystone.Namespace, Name: po.ConfigMapRef.Name}, &cm); err != nil {
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

// federationCacheKeyOf extracts the config-render inputs a federation
// projection contributes to the cache key: whether federation is projected, the
// OIDC remote-id attribute, and the SAML protocol/remote-id — a SAML attach or
// detach flips the [<protocolID>] section without bumping the Keystone
// generation, so both must invalidate the cache.
func federationCacheKeyOf(fed *federationProjection) (projected bool, remoteIDAttribute, samlProtocolID, samlRemoteIDAttribute string) {
	if fed == nil {
		return false, "", "", ""
	}
	return true, fed.RemoteIDAttribute, fed.SAMLProtocolID, fed.SAMLRemoteIDAttribute
}

// configRenderCacheHit reports whether the memoized render for this CR is still
// valid: matching UID, generation, policy-ConfigMap ResourceVersion, and
// identity-backend projection state (domains and federation).
func (r *KeystoneReconciler) configRenderCacheHit(keystone *keystonev1alpha1.Keystone, policyCMRV string, domainsProjected bool, fed *federationProjection) (name string, ok bool) {
	federationProjected, remoteIDAttribute, samlProtocolID, samlRemoteIDAttribute := federationCacheKeyOf(fed)
	r.configRenderCacheMu.Lock()
	defer r.configRenderCacheMu.Unlock()
	entry, found := r.configRenderCache[client.ObjectKeyFromObject(keystone)]
	if !found {
		return "", false
	}
	if entry.uid != keystone.UID || entry.generation != keystone.Generation ||
		entry.policyCMResourceVersion != policyCMRV || entry.domainsProjected != domainsProjected ||
		entry.federationProjected != federationProjected || entry.remoteIDAttribute != remoteIDAttribute ||
		entry.samlProtocolID != samlProtocolID || entry.samlRemoteIDAttribute != samlRemoteIDAttribute {
		return "", false
	}
	return entry.configMapName, true
}

// storeConfigRender memoizes a successful render.
func (r *KeystoneReconciler) storeConfigRender(keystone *keystonev1alpha1.Keystone, policyCMRV, configMapName string, domainsProjected bool, fed *federationProjection) {
	federationProjected, remoteIDAttribute, samlProtocolID, samlRemoteIDAttribute := federationCacheKeyOf(fed)
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
		samlProtocolID:          samlProtocolID,
		samlRemoteIDAttribute:   samlRemoteIDAttribute,
		configMapName:           configMapName,
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
func (r *KeystoneReconciler) configMapExists(ctx context.Context, children client.Client, namespace, name string) (bool, error) {
	var cm corev1.ConfigMap
	if err := children.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking config ConfigMap %s: %w", name, err)
	}
	return true, nil
}

// pruneStaleConfigMaps removes historical immutable ConfigMaps that exceed
// the retain count, keeping only the newest historical ConfigMaps plus the
// currently active one.
func (r *KeystoneReconciler) pruneStaleConfigMaps(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone, configMapName string) error {
	baseName := fmt.Sprintf("%s-config", keystone.Name)
	return config.PruneImmutableConfigMaps(ctx, children, r.Scheme, keystone, config.PruneOptions{
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
