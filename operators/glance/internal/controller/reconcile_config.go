// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/cache"
	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/keystoneauth"
	"github.com/c5c3/forge/internal/common/plugins"
	"github.com/c5c3/forge/internal/common/policy"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// defaultConfigMapRetainCount is the number of historical immutable
// ConfigMaps/Secrets to retain after pruning. Combined with the current active
// artefact, this allows rollback to 3 previous configurations.
const defaultConfigMapRetainCount = 3

// glanceConfigDir is the in-pod directory the rendered config ConfigMap is
// mounted at (oslo.config --config-dir). The db-sync Job (reconcile_database.go)
// and the deployment step (next commit) mount the same path, so the paste/
// policy/logging file references below stay in lockstep.
const glanceConfigDir = "/etc/glance/glance-api.conf.d/"

// configVolumeName is the pod volume the deployment step (next commit) projects
// the rendered config ConfigMap through. reconcileConfig reads it back off the
// live Deployment to recover the last-good ConfigMap name when a projection is
// invalid — a naming contract shared with the deployment step.
const configVolumeName = "config"

// File paths inside glanceConfigDir. oslo.config's --config-dir only parses
// *.conf files, so the paste/policy/logging keys are inert for the config loader
// and are referenced explicitly by the options that consume them.
const (
	// pasteFilePath is the api-paste.ini path [paste_deploy] config_file points
	// at.
	pasteFilePath = glanceConfigDir + "glance-api-paste.ini"
	// policyFilePath is the oslo.policy path [oslo_policy] policy_file points at
	// when spec.policyOverrides is set.
	policyFilePath = glanceConfigDir + "policy.yaml"
	// loggingConfFilePath is the oslo.log fileConfig path [DEFAULT]
	// log_config_append points at when spec.logging.format == "json".
	loggingConfFilePath = glanceConfigDir + "logging.conf"
)

// Reserved-store filesystem paths. Glance ALWAYS registers the staging and
// tasks stores even in an all-object-store deployment: import staging and async
// task work land on local disk regardless of the image store. The deployment
// step (next commit) mounts an emptyDir at each path.
const (
	glanceStagingStorePath = "/var/lib/glance/staging"
	glanceTasksStorePath   = "/var/lib/glance/tasks-work"
	// glanceImageCachePath is the in-pod image-cache directory. The deployment
	// step mounts a bounded emptyDir there when spec.imageCache is set.
	glanceImageCachePath = "/var/lib/glance/image-cache"
)

// dbConnectionPlaceholder is the placeholder URL written to the [database]
// connection key in glance-api.conf. The real URL is injected at runtime via the
// OS_DATABASE__CONNECTION env var sourced from the derived
// <glance.Name>-db-connection Secret (oslo.config OS_<GROUP>__<OPTION>
// override). The placeholder MUST be a syntactically valid pymysql URL so
// oslo.config parses the file cleanly before the env override is applied.
const dbConnectionPlaceholder = "mysql+pymysql://placeholder"

// configArtifacts names the immutable artefacts the config step produced (or,
// on an invalid projection, the ones the live Deployment currently mounts). The
// deployment step (next commit) mounts the ConfigMap at glanceConfigDir and the
// backends Secret at the backends volume; the database step consumes
// configMapName.
type configArtifacts struct {
	// configMapName is the content-hashed glance-api.conf ConfigMap.
	configMapName string
	// backendsSecretName is the content-hashed backends Secret the deployment
	// step mounts through the backends volume.
	backendsSecretName string
}

// reconcileConfig renders glance-api.conf and glance-api-paste.ini into an
// immutable ConfigMap (plus policy.yaml / logging.conf when applicable) and
// returns the artefact names for the database and deployment steps. Config
// failures flip SecretsReady=False (the Config→SecretsReady mapping keystone
// uses) so the aggregate Ready cannot stay stale-True at the new generation.
//
// When the backends projection is invalid (the exactly-one-default rule is
// unmet), it does NOT re-render: it returns the artefact names the live
// Deployment currently mounts so downstream steps keep using the last-good
// config (D3 last-good retention), or empty names on first install.
func (r *GlanceReconciler) reconcileConfig(ctx context.Context, children client.Client, glance *glancev1alpha1.Glance, projection backendsProjection) (ctrl.Result, configArtifacts, error) {
	// The extraConfig ownership guard is a pure function of the spec, so it
	// runs before the invalid-projection short-circuit: the condition is
	// maintained on the last-good-retention path too. The ExtraConfigHealthy
	// condition is informational and deliberately stays out of
	// subConditionTypes and subReconcilerConditionTypes.
	config.RecordExtraConfigHealth(r.Recorder, glance, &glance.Status.Conditions, glance.Generation,
		config.FindOwnedOverrides(glance.Spec.ExtraConfig, glancev1alpha1.OwnedConfigKeys))

	if !projection.valid {
		// Last-good retention: keep whatever the running Deployment mounts rather
		// than re-rendering against an invalid projection.
		return r.lastGoodArtifacts(ctx, children, glance)
	}

	defaults := operatorDefaults(glance, projection)

	// logging is re-derived here (operatorDefaults keeps its own copy) for the
	// later logging.conf data-key decision below.
	logging := effectiveLogging(glance.Spec.Logging)

	merged := defaults

	// Merge plugin config (operator defaults win over plugin sections that
	// collide, then user extraConfig wins over both).
	if len(glance.Spec.Plugins) > 0 {
		pluginConfig, err := plugins.RenderPluginConfig(glance.Spec.Plugins)
		if err != nil {
			markConfigFailed(glance, err)
			return ctrl.Result{}, configArtifacts{}, fmt.Errorf("rendering plugin config: %w", err)
		}
		merged = config.MergeDefaults(defaults, pluginConfig)
	}

	// extraConfig overrides everything (the true escape hatch).
	if glance.Spec.ExtraConfig != nil {
		merged = config.MergeDefaults(glance.Spec.ExtraConfig, merged)
	}

	// Handle PolicyOverrides: render policy.yaml and wire oslo_policy.policy_file.
	var policyYAML string
	if glance.Spec.PolicyOverrides != nil {
		yaml, err := buildPolicyYAML(ctx, children, glance)
		if err != nil {
			markConfigFailed(glance, err)
			return ctrl.Result{}, configArtifacts{}, fmt.Errorf("building policy: %w", err)
		}
		policyYAML = yaml
		if policyYAML != "" {
			merged = config.InjectOsloPolicyConfig(merged, policyFilePath)
		}
	}

	pasteINI, err := renderPasteINI(glance)
	if err != nil {
		markConfigFailed(glance, err)
		return ctrl.Result{}, configArtifacts{}, fmt.Errorf("rendering glance-api-paste.ini: %w", err)
	}

	data := map[string]string{
		"glance-api.conf":      config.RenderINI(merged),
		"glance-api-paste.ini": pasteINI,
	}
	if policyYAML != "" {
		data["policy.yaml"] = policyYAML
	}
	if logging.Format == "json" {
		data["logging.conf"] = config.RenderLoggingConf(logging.Level)
	}

	configMapName, err := config.CreateImmutableConfigMap(ctx, children, r.Scheme, glance,
		glance.Name+"-config", glance.Namespace, data)
	if err != nil {
		markConfigFailed(glance, err)
		return ctrl.Result{}, configArtifacts{}, fmt.Errorf("creating config ConfigMap: %w", err)
	}
	if err := config.PruneImmutableConfigMaps(ctx, children, glance, config.PruneOptions{
		BaseName:    glance.Name + "-config",
		Namespace:   glance.Namespace,
		CurrentName: configMapName,
		Retain:      defaultConfigMapRetainCount,
	}); err != nil {
		markConfigFailed(glance, err)
		return ctrl.Result{}, configArtifacts{}, fmt.Errorf("pruning config ConfigMaps: %w", err)
	}

	return ctrl.Result{}, configArtifacts{configMapName: configMapName, backendsSecretName: projection.secretName}, nil
}

// operatorDefaults builds the operator-owned glance-api.conf sections from the
// CRD spec and the backends projection: the static
// [DEFAULT]/[database]/[keystone_authtoken]/… scaffolding plus the
// worker-count, per-logger-level, and json-logging conditionals. It is a pure
// function of the spec (no cluster access), so the registry drift-guard test
// can call it directly to assert the rendered defaults stay in lockstep with
// glancev1alpha1.OwnedConfigKeys. It mirrors keystone's operatorDefaults.
func operatorDefaults(glance *glancev1alpha1.Glance, projection backendsProjection) map[string]map[string]string {
	logging := effectiveLogging(glance.Spec.Logging)
	filtering := effectiveImportFiltering(glance.Spec.ImportFiltering)
	// The image-import plugins. (The variable is not named `plugins` — that is
	// the pipeline helper package renderPasteINI calls below.)
	importPlugins := effectiveImportPlugins(glance.Spec.ImportPlugins)
	// The enabled plugin names, in the order glance runs them. The order is fixed
	// here and is not an input, which is what satisfies upstream's one ordering
	// requirement by construction: decompression has to run before conversion,
	// because converting first would rewrite the archive rather than the disk
	// image inside it.
	var importPluginNames []string
	if importPlugins.Decompression != nil {
		importPluginNames = append(importPluginNames, "image_decompression")
	}
	if importPlugins.Conversion != nil {
		importPluginNames = append(importPluginNames, "image_conversion")
	}
	if importPlugins.InjectMetadata != nil {
		importPluginNames = append(importPluginNames, "inject_image_metadata")
	}
	defaults := map[string]map[string]string{
		"DEFAULT": {
			"enabled_backends": projection.enabledBackends,
			// web-download and copy-image only: glance-direct is deliberately
			// excluded because it stages the uploaded image on the API pod's local
			// disk, and there is no staging volume shared across replicas — a
			// direct import begun on one pod could not be finished by another.
			"enabled_import_methods": "[web-download,copy-image]",
			// Route oslo.log records to stderr so kubectl logs surfaces them.
			"use_stderr": "true",
			// oslo.log gates several extra-verbose code paths on the debug flag
			// specifically, independent of the root logger level. Debug is a
			// nil-preserving *bool: nil renders as the default (false).
			"debug": fmt.Sprintf("%t", logging.Debug != nil && *logging.Debug),
		},
		"database": {
			"max_retries":             "-1",
			"connection_recycle_time": "600",
			// The real URL is materialized by reconcileDBConnectionSecret into a
			// derived Secret and injected at runtime via OS_DATABASE__CONNECTION.
			"connection": dbConnectionPlaceholder,
		},
		"keystone_authtoken": keystoneauth.Section(keystoneauth.SectionParams{
			AuthURL:            glance.Spec.KeystoneEndpoint,
			WWWAuthenticateURI: glance.Spec.EffectiveKeystonePublicEndpoint(),
			Username:           glance.Spec.ServiceUser.Username,
			ProjectName:        glance.Spec.ServiceUser.ProjectName,
			UserDomainName:     glance.Spec.ServiceUser.UserDomainName,
			ProjectDomainName:  glance.Spec.ServiceUser.ProjectDomainName,
			RegionName:         glance.Spec.Region,
			MemcachedServers:   cache.ResolveServers(&glance.Spec.Cache),
		}),
		"glance_store": {
			"default_backend": projection.defaultBackend,
		},
		// Reserved stores: always registered so import staging and async task
		// work have a local landing directory regardless of the image store.
		"os_glance_staging_store": {
			"filesystem_store_datadir": glanceStagingStorePath,
		},
		"os_glance_tasks_store": {
			"filesystem_store_datadir": glanceTasksStorePath,
		},
		"paste_deploy": {
			"flavor":      "keystone",
			"config_file": pasteFilePath,
		},
		// The web-download URI filter. All six oslo ListOpts are rendered
		// unconditionally, the empty ones as "[]": glance's own allowed_schemes
		// ([http, https]) and allowed_ports ([80, 443]) defaults are non-empty,
		// and a non-empty allow-list makes glance ignore the matching deny-list —
		// so a user deny-list only takes effect once the CR sets the sibling
		// allow-list to an explicitly empty list (effectiveImportFiltering never
		// empties one on its own). Every value is bracket-bounded, like the
		// [DEFAULT] enabled_import_methods literal above; see renderBoundedList.
		"import_filtering_opts": {
			"allowed_schemes":    renderBoundedList(filtering.AllowedSchemes),
			"disallowed_schemes": renderBoundedList(filtering.DisallowedSchemes),
			"allowed_hosts":      renderBoundedList(filtering.AllowedHosts),
			"disallowed_hosts":   renderBoundedList(filtering.DisallowedHosts),
			"allowed_ports":      renderPortList(filtering.AllowedPorts),
			"disallowed_ports":   renderPortList(filtering.DisallowedPorts),
		},
		// The image-import plugin pipeline, rendered unconditionally for the reason
		// the filter lists above are: an absent key is not "no plugins", it is
		// glance's own default, and a key the ownership registry rejects in
		// extraConfig has to be one the operator actually writes. A CR without
		// spec.importPlugins renders "[]" and runs no plugin. The option is declared
		// bounds=True upstream (its sample default is "[no_op]") and its items are
		// quoted strings that accept an unquoted name, so the value is
		// bracket-bounded like the filter lists; see renderBoundedList. Rendering it
		// unconditionally means every existing Glance re-renders once when the
		// operator is upgraded and rolls its Deployment through the usual
		// content-hash mechanics — the same one-time roll the filter render caused.
		"image_import_opts": {
			"image_import_plugins": renderBoundedList(importPluginNames),
		},
	}

	// workers is the eventlet API worker count. Below release 2026.1 (eventlet
	// launch mode) it is ALWAYS rendered so the count is deterministic: from
	// spec.apiServer.workers when set, else DefaultEventletWorkers — never the
	// eventlet server's own fallback of one worker per host CPU, which ignores the
	// pod's CPU limit and OOMs the container under load. Under the uWSGI launch
	// mode (2026.1+) the key is inert (uWSGI ignores it), so it is rendered only
	// when explicitly set, for transparency; the webhook warns on that combination.
	if s := glance.Spec.APIServer; s != nil && s.Workers != nil {
		defaults["DEFAULT"]["workers"] = fmt.Sprintf("%d", *s.Workers)
	} else if !glanceUsesUWSGI(glance) {
		defaults["DEFAULT"]["workers"] = fmt.Sprintf("%d", glancev1alpha1.DefaultEventletWorkers)
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
	// The local image cache. Exactly three keys: the directory the deployment
	// step bounds with an emptyDir, the driver, and the pruner threshold derived
	// from that bound. The driver is pinned to sqlite rather than left at
	// glance's own default centralized_db, which keys cache state in the Glance
	// database per worker_self_reference_url and would strand a dead pod's rows
	// there on every replacement; sqlite keeps that state inside the cache
	// directory, so it shares the emptyDir's lifecycle and the emptyDir the
	// pod's. image_cache_stall_time (86400) and image_cache_sqlite_db
	// (cache.db) are deliberately NOT rendered: glance's own defaults are what
	// this deployment wants, and rendering them would only claim ownership of
	// two more keys for no gain. (The variable is not named `cache` — that is
	// the memcached helper package this function calls above.)
	if imageCache := effectiveImageCache(glance.Spec.ImageCache); imageCache != nil {
		defaults["DEFAULT"]["image_cache_dir"] = glanceImageCachePath
		defaults["DEFAULT"]["image_cache_driver"] = "sqlite"
		defaults["DEFAULT"]["image_cache_max_size"] = strconv.FormatInt(imageCacheMaxSizeBytes(*imageCache.SizeLimit), 10)
	}
	// The image_conversion plugin's single option, rendered only while the plugin
	// is in the list above: the section is inert without it, and writing it anyway
	// would claim ownership of a key the deployment never asked about.
	if importPlugins.Conversion != nil {
		defaults["image_conversion"] = map[string]string{
			"output_format": importPlugins.Conversion.OutputFormat,
		}
	}
	// The inject_image_metadata plugin's two options, again only while the plugin
	// is enabled. inject is an oslo DictOpt, rendered as sorted, unquoted
	// "key:value" pairs: oslo splits the value on commas and each pair on its
	// first colon, then takes the rest of the pair as the property value verbatim,
	// so a quote would land in the property rather than delimit it. An empty
	// property map would render an empty value, which only a CR that bypassed the
	// schema reaches — the CRD requires at least one property on the block that
	// enables the plugin. ignore_user_roles is a plain comma join with NO
	// brackets: upstream declares it as an unbounded ListOpt, which reads the
	// brackets as item text — the trap renderBoundedList's godoc documents — so a
	// bracketed value would exempt a role literally named "[admin" and leave the
	// real admin role subject to the injection. An empty resolved list renders an
	// empty value, which is the explicit opt-in to injecting for every role.
	if importPlugins.InjectMetadata != nil {
		defaults["inject_metadata_properties"] = map[string]string{
			"inject":            config.RenderSortedPairs(importPlugins.InjectMetadata.Properties, ":"),
			"ignore_user_roles": strings.Join(importPlugins.InjectMetadata.IgnoreUserRoles, ","),
		}
	}

	return defaults
}

// renderPasteINI renders glance-api-paste.ini, mirroring upstream's shape:
// the flavored name glance loads ([paste_deploy] flavor = keystone →
// glance-api-keystone) is a root composite that mounts the filter pipeline
// (cors → http_proxy_to_wsgi → versionnegotiation → authtoken → context →
// rootapp) at / and the healthcheck app at /healthcheck. Healthcheck must be
// an app, not a pipeline filter: oslo.middleware in glance ≥ 32.0.0 (2026.1)
// raises NotImplementedError for filter-style deployment, crash-looping every
// API pod at startup. Routing /healthcheck above the pipeline also keeps the
// probes outside authtoken, as the old front-of-pipeline filter did.
// Any spec.Middleware is injected into the pipeline via the shared renderer,
// merged with the literal glance sections the PipelineSpec cannot express.
// While spec.imageCache is set the operator adds one more filter of its own,
// `cache`, as the last after-positioned entry — upstream's keystone+caching
// flavor likewise places it directly before the root app, so every request that
// reaches the versioned apps passes the cache first.
func renderPasteINI(glance *glancev1alpha1.Glance) (string, error) {
	middleware := glance.Spec.Middleware
	if glance.Spec.ImageCache != nil {
		// Extend a clone, never the CR's own slice: append would write into its
		// backing array whenever it has spare capacity, so a reconcile would
		// mutate the spec it is rendering from. The `cache` name is reserved for
		// the operator at admission (the imageCache/middleware collision check in
		// the validating webhook), so a CR that reaches the renderer with both
		// cannot have passed it; the shared renderer's duplicate-name check is
		// the backstop for the one that bypassed the webhook.
		middleware = append(append([]commonv1.MiddlewareSpec(nil), glance.Spec.Middleware...), commonv1.MiddlewareSpec{
			Name:          "cache",
			FilterFactory: "glance.api.middleware.cache:CacheFilter.factory",
			Position:      commonv1.PipelinePositionAfter,
		})
	}

	sections, err := plugins.RenderPastePipeline(plugins.PipelineSpec{
		PipelineName: "api",
		// rootapp terminates the pipeline directive; it is a composite section
		// (below), not an [app:*], so no AppFactory is set — that suppresses the
		// [app:rootapp] the renderer would otherwise emit.
		AppName:     "rootapp",
		BaseFilters: []string{"cors", "http_proxy_to_wsgi", "versionnegotiation", "authtoken", "context"},
		Middleware:  middleware,
	})
	if err != nil {
		return "", err
	}

	// Merge the literal root-app and filter sections. These mirror upstream
	// glance-api-paste.ini restricted to the rendered pipeline's members: the
	// filter factories use paste.filter_factory (not the egg: use directive the
	// shared BaseFilterFactories would emit), so they are supplied here.
	for name, section := range glanceStaticPasteSections() {
		sections[name] = section
	}
	return config.RenderINI(sections), nil
}

// glanceStaticPasteSections returns the literal glance-api-paste.ini sections
// the PipelineSpec cannot express: the root and rootapp composites, the
// versioned apps, the healthcheck app, and the base filter factories.
func glanceStaticPasteSections() map[string]map[string]string {
	return map[string]map[string]string{
		"composite:glance-api-keystone": {
			"paste.composite_factory": "glance.api:root_app_factory",
			"/":                       "api",
			"/healthcheck":            "healthcheck",
		},
		"composite:rootapp": {
			"paste.composite_factory": "glance.api:root_app_factory",
			"/":                       "apiversions",
			"/v2":                     "apiv2app",
		},
		"app:healthcheck": {
			"paste.app_factory":    "oslo_middleware:Healthcheck.app_factory",
			"backends":             "disable_by_file",
			"disable_by_file_path": "/etc/glance/healthcheck_disable",
		},
		"app:apiversions": {
			"paste.app_factory": "glance.api.versions:create_resource",
		},
		"app:apiv2app": {
			"paste.app_factory": "glance.api.v2.router:API.factory",
		},
		"filter:cors": {
			"paste.filter_factory": "oslo_middleware.cors:filter_factory",
			"oslo_config_project":  "glance",
			"oslo_config_program":  "glance-api",
		},
		"filter:http_proxy_to_wsgi": {
			"paste.filter_factory": "oslo_middleware:HTTPProxyToWSGI.factory",
		},
		"filter:versionnegotiation": {
			"paste.filter_factory": "glance.api.middleware.version_negotiation:VersionNegotiationFilter.factory",
		},
		"filter:authtoken": {
			"paste.filter_factory": "keystonemiddleware.auth_token:filter_factory",
			"delay_auth_decision":  "true",
		},
		"filter:context": {
			"paste.filter_factory": "glance.api.middleware.context:ContextMiddleware.factory",
		},
	}
}

// lastGoodArtifacts returns the ConfigMap and backends Secret names the running
// Glance Deployment currently mounts, so an invalid projection keeps the
// last-good config instead of re-rendering. On first install (no Deployment
// yet) it returns empty names.
func (r *GlanceReconciler) lastGoodArtifacts(ctx context.Context, children client.Client, glance *glancev1alpha1.Glance) (ctrl.Result, configArtifacts, error) {
	var deploy appsv1.Deployment
	key := client.ObjectKey{Namespace: glance.Namespace, Name: subResourceName(glance)}
	if err := children.Get(ctx, key, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, configArtifacts{}, nil
		}
		return ctrl.Result{}, configArtifacts{}, fmt.Errorf("fetching Deployment %s for last-good config: %w", key, err)
	}

	var art configArtifacts
	for i := range deploy.Spec.Template.Spec.Volumes {
		v := &deploy.Spec.Template.Spec.Volumes[i]
		switch {
		case v.Name == configVolumeName && v.ConfigMap != nil:
			art.configMapName = v.ConfigMap.Name
		case v.Name == backendsVolumeName && v.Secret != nil:
			art.backendsSecretName = v.Secret.SecretName
		}
	}
	return ctrl.Result{}, art, nil
}

// buildPolicyYAML builds the policy.yaml content from spec.policyOverrides,
// merging inline rules over any ConfigMap-sourced rules (inline wins). It
// mirrors keystone's buildPolicyYAML.
func buildPolicyYAML(ctx context.Context, c client.Client, glance *glancev1alpha1.Glance) (string, error) {
	po := glance.Spec.PolicyOverrides
	if po == nil {
		return "", nil
	}

	var rules map[string]string
	if po.ConfigMapRef != nil {
		cmRules, err := policy.LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{
			Namespace: glance.Namespace,
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
// materializing the production defaults when spec.logging is nil (a CR that
// bypassed the defaulting webhook). It mirrors keystone's effectiveLogging.
func effectiveLogging(spec *glancev1alpha1.LoggingSpec) glancev1alpha1.LoggingSpec {
	out := glancev1alpha1.LoggingSpec{Format: "text", Level: "INFO"}
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

// effectiveImportFiltering returns the six [import_filtering_opts] lists to
// render, materializing the operator defaults for the fields spec.importFiltering
// leaves unset. A nil block behaves exactly like an empty one. It is pure and
// total: no input can fail, so there is no error path.
//
// "Unset" means nil. An explicitly empty list is honored verbatim, which is the
// only way to opt out of a default restriction. No field a user sets can widen
// the default resolved for another one: adding a deny-list entry is a tightening
// edit, and it must never be answered by dropping a baseline. The nil fields
// resolve as:
//
//   - allowedSchemes → DefaultImportAllowedSchemes, allowedPorts →
//     DefaultImportAllowedPorts, both independent of the sibling deny-list.
//     Glance evaluates a deny-list only while the matching allow-list is empty,
//     so a deployment whose deny-list must be authoritative renders the
//     allow-list empty itself (allowedSchemes: []); resolving that carve-out here
//     would trade the pinned scheme and port for every other scheme and port on
//     an edit that asked for neither.
//   - disallowedHosts → DefaultImportDisallowedHosts, and a non-empty user list
//     is unioned onto that baseline rather than replacing it: the denied hosts
//     are a security floor, so adding an entry must not un-deny loopback, the
//     metadata address, or the API server. It resolves empty while allowedHosts
//     is non-empty, because glance ignores the host deny-list in that case and
//     the allow-list is the stricter half anyway.
//   - allowedHosts, disallowedSchemes, disallowedPorts → empty, since glance's
//     own defaults for those are already empty.
//
// The resolved lists may alias the package-level default slices, so callers must
// treat the result as read-only.
func effectiveImportFiltering(spec *glancev1alpha1.ImportFilteringSpec) glancev1alpha1.ImportFilteringSpec {
	var out glancev1alpha1.ImportFilteringSpec
	if spec != nil {
		out = *spec
	}
	if out.AllowedSchemes == nil {
		out.AllowedSchemes = glancev1alpha1.DefaultImportAllowedSchemes
	}
	if out.AllowedPorts == nil {
		out.AllowedPorts = glancev1alpha1.DefaultImportAllowedPorts
	}
	if len(out.AllowedHosts) == 0 {
		switch {
		case out.DisallowedHosts == nil:
			out.DisallowedHosts = glancev1alpha1.DefaultImportDisallowedHosts
		case len(out.DisallowedHosts) > 0:
			out.DisallowedHosts = unionHosts(glancev1alpha1.DefaultImportDisallowedHosts, out.DisallowedHosts)
		}
	}
	return out
}

// effectiveImportPlugins returns the image-import plugins to render,
// materializing the operator defaults for the fields spec.importPlugins leaves
// unset. A nil block behaves exactly like an empty one: both enable no plugin.
// It is pure and total: no input can fail, so there is no error path.
//
// The presence of a sub-block is the opt-in for its plugin, so there is nothing
// to default into an absent one and only the fields of a block that IS set
// resolve:
//
//   - conversion.outputFormat → DefaultImportConversionOutputFormat while empty.
//   - injectMetadata.ignoreUserRoles → DefaultImportInjectIgnoreUserRoles while
//     nil. "Unset" means nil here, the nil-versus-empty contract
//     effectiveImportFiltering follows: an explicitly empty list is honored
//     verbatim, and it is the documented way to inject for admins too.
//
// Resolving here rather than in the defaulting webhook keeps an unset field
// tracking the operator default across upgrades instead of freezing today's value
// into the stored CR — the same contract effectiveImportFiltering and
// effectiveImageCache follow.
//
// The sub-blocks in the result are fresh, so nothing the resolver decides is
// written back into the CR. The property map and the role slice inside them may
// alias the CR's own or the package-level default slice, so callers must treat
// the result as read-only.
func effectiveImportPlugins(spec *glancev1alpha1.ImportPluginsSpec) glancev1alpha1.ImportPluginsSpec {
	var out glancev1alpha1.ImportPluginsSpec
	if spec == nil {
		return out
	}
	if spec.Decompression != nil {
		out.Decompression = &glancev1alpha1.ImportDecompressionSpec{}
	}
	if spec.Conversion != nil {
		format := spec.Conversion.OutputFormat
		if format == "" {
			format = glancev1alpha1.DefaultImportConversionOutputFormat
		}
		out.Conversion = &glancev1alpha1.ImportConversionSpec{OutputFormat: format}
	}
	if spec.InjectMetadata != nil {
		roles := spec.InjectMetadata.IgnoreUserRoles
		if roles == nil {
			roles = glancev1alpha1.DefaultImportInjectIgnoreUserRoles
		}
		out.InjectMetadata = &glancev1alpha1.ImportInjectMetadataSpec{
			Properties:      spec.InjectMetadata.Properties,
			IgnoreUserRoles: roles,
		}
	}
	return out
}

// effectiveImageCache returns the image-cache settings to render, materializing
// the operator defaults for the fields spec.imageCache leaves unset. A nil block
// means the cache is off and resolves to nil — presence of the block is the
// opt-in, so there is nothing to default into. It is pure and total: no input can
// fail, so there is no error path.
//
// Resolving here rather than in the defaulting webhook keeps an unset field
// tracking the operator default across upgrades instead of freezing today's value
// into the stored CR — the same contract effectiveStagingSizeLimit,
// effectiveDBPurge, and effectiveImportFiltering follow.
//
// The result is a copy, so the returned SizeLimit and MaintenanceInterval can be
// dereferenced without touching the CR. Both the config render here and the
// deployment builder call it, so the two never disagree about the bound the
// pruner threshold is derived from.
func effectiveImageCache(spec *glancev1alpha1.ImageCacheSpec) *glancev1alpha1.ImageCacheSpec {
	if spec == nil {
		return nil
	}
	out := spec.DeepCopy()
	if out.SizeLimit == nil {
		out.SizeLimit = ptr.To(glancev1alpha1.DefaultImageCacheSizeLimit())
	}
	if out.MaintenanceInterval == nil {
		out.MaintenanceInterval = &metav1.Duration{Duration: glancev1alpha1.DefaultImageCacheMaintenanceInterval}
	}
	return out
}

// imageCacheMaxSizeBytes derives the rendered [DEFAULT] image_cache_max_size
// from the emptyDir bound: 80% of it, not the bound itself. Glance's pruner only
// ever prunes DOWN TO image_cache_max_size, and only when the maintenance sidecar
// runs it, so everything the API writes between that threshold and the emptyDir
// bound has to fit somewhere — the remaining 20% is exactly that headroom, and it
// is what keeps a burst of downloads between two maintenance passes from
// crossing the bound and tripping the kubelet's ephemeral-storage eviction.
//
// The divide-before-multiply order is load-bearing and must not be reordered:
// the integer division truncates first, which keeps the result at or below the
// true 80% (never above it, where the headroom would shrink), and it also keeps
// the intermediate from overflowing int64 on a very large Quantity.
func imageCacheMaxSizeBytes(limit resource.Quantity) int64 {
	return limit.Value() / 10 * 8
}

// unionHosts returns base followed by every entry of extra base does not already
// carry. Order is deterministic — baseline first, user entries in spec order —
// because the rendered value feeds the config ConfigMap's content hash, and a
// reordering would rotate the ConfigMap and roll the Deployment for nothing. The
// result is a fresh slice, so the package-level baseline is never appended to.
func unionHosts(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base))
	out := make([]string, 0, len(base)+len(extra))
	for _, host := range base {
		seen[host] = struct{}{}
		out = append(out, host)
	}
	for _, host := range extra {
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

// renderBoundedList formats a list as the bracket-bounded value an oslo ListOpt
// declared with bounds=True consumes. All six [import_filtering_opts] options
// are (glance/async_/flows/_internal_plugins/__init__.py), so oslo.config
// rejects a bare comma-joined value from them with `Value should start with
// "["`. It rejects it lazily, on the first read inside validate_import_uri, so
// an unbracketed value leaves the API healthy and fails only the web-download
// import it was meant to filter — with a 500, not a refusal. An empty list
// renders "[]", the empty value oslo parses, never "".
//
// Only for a bounded ListOpt: an unbounded one (oslo.log's default_log_levels)
// reads the brackets as part of the first and last item.
func renderBoundedList(items []string) string {
	return "[" + strings.Join(items, ",") + "]"
}

// renderPortList formats a port list as the decimal, bracket-bounded value an
// [import_filtering_opts] port option consumes. See renderBoundedList.
func renderPortList(ports []int32) string {
	items := make([]string, 0, len(ports))
	for _, port := range ports {
		items = append(items, fmt.Sprintf("%d", port))
	}
	return renderBoundedList(items)
}
