// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/cache"
	"github.com/c5c3/cobaltcore/internal/common/config"
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	"github.com/c5c3/cobaltcore/internal/common/plugins"
	"github.com/c5c3/cobaltcore/internal/common/policy"
	"github.com/c5c3/cobaltcore/internal/common/release"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

// defaultConfigRetainCount is the number of historical immutable config Secrets
// to retain after pruning. Combined with the current active artefact, this
// allows rollback to 3 previous configurations.
const defaultConfigRetainCount = 3

// barbicanConfigDir is the in-pod directory the rendered config Secret is
// mounted at. Barbican offers no --config-dir and resolves both of its startup
// files by name rather than through a configurable path: oslo.config loads
// barbican.conf from its fixed search path and the API app looks
// barbican-api-paste.ini up the same way, so /etc/barbican is the one directory
// that can carry either. Everything the pods read therefore mounts here.
const barbicanConfigDir = "/etc/barbican/"

// policyFilePath is the oslo.policy path [oslo_policy] policy_file points at
// when spec.policyOverrides is set.
const policyFilePath = barbicanConfigDir + "policy.yaml"

// barbicanPasteDataKey is the data key inside the config Secret holding the
// rendered barbican-api-paste.ini. Its sibling key is barbicanConfDataKey.
const barbicanPasteDataKey = "barbican-api-paste.ini"

// policyFileDataKey is the config Secret data key carrying the rendered
// policy.yaml; it is the file name policyFilePath points at inside the mount.
const policyFileDataKey = "policy.yaml"

// barbicanAPIPort is the port the Barbican API listens on. The deployment,
// healthcheck and httproute steps address the same port, so the constant lives
// with the endpoint helper that first needed it.
const barbicanAPIPort = 9311

// keystonePipelineName is the paste pipeline composite:main routes /v1 to, and
// therefore the one spec.middleware extends. The remaining pipelines the image
// ships are rendered verbatim and are unreachable through composite:main.
const keystonePipelineName = "barbican-api-keystone"

// dbConnectionPlaceholder is the placeholder URL written to the [database]
// connection key in barbican.conf. The real URL is injected at runtime via the
// OS_DATABASE__CONNECTION env var sourced from the derived
// <barbican.Name>-db-connection Secret (oslo.config OS_<GROUP>__<OPTION>
// override). The placeholder MUST be a syntactically valid pymysql URL so
// oslo.config parses the file cleanly before the env override is applied.
const dbConnectionPlaceholder = "mysql+pymysql://placeholder"

// reconcileConfig renders barbican.conf and barbican-api-paste.ini into ONE
// immutable, content-hashed Secret and returns its name for the database and
// deployment steps. The carrier is a Secret rather than a ConfigMap because the
// rendered barbican.conf carries the vault plugin's approle_role_id, and
// barbican reads exactly one configuration file, so the whole document is
// credential-bearing. Config failures flip SecretsReady=False (the
// Config→SecretsReady mapping the sibling operators use) so the aggregate Ready
// cannot stay stale-True at the new generation.
//
// When the secret-store projection is invalid, it does NOT re-render: it returns
// the Secret name the live Deployment currently mounts so downstream steps keep
// using the last-good config, or an empty name on first install.
func (r *BarbicanReconciler) reconcileConfig(ctx context.Context, children client.Client, barbican *barbicanv1alpha1.Barbican, projection secretStoreProjection) (ctrl.Result, string, error) {
	// The extraConfig ownership guard is a pure function of the spec, so it runs
	// before the invalid-projection short-circuit: the condition is maintained on
	// the last-good-retention path too. The ExtraConfigHealthy condition is
	// informational and deliberately stays out of subConditionTypes and
	// subReconcilerConditionTypes.
	config.RecordExtraConfigHealth(r.Recorder, barbican, &barbican.Status.Conditions, barbican.Generation,
		config.FindOwnedOverrides(barbican.Spec.ExtraConfig, barbicanv1alpha1.OwnedConfigKeys))

	if !projection.valid {
		// Last-good retention: keep whatever the running Deployment mounts rather
		// than re-rendering against an invalid projection.
		return r.lastGoodConfigSecret(ctx, children, barbican)
	}

	merged := operatorDefaults(barbican, projection)

	// Merge plugin config (operator defaults win over plugin sections that
	// collide, then user extraConfig wins over both).
	if len(barbican.Spec.Plugins) > 0 {
		pluginConfig, err := plugins.RenderPluginConfig(barbican.Spec.Plugins)
		if err != nil {
			markConfigFailed(barbican, err)
			return ctrl.Result{}, "", fmt.Errorf("rendering plugin config: %w", err)
		}
		merged = config.MergeDefaults(merged, pluginConfig)
	}

	// extraConfig overrides everything (the true escape hatch).
	if barbican.Spec.ExtraConfig != nil {
		merged = config.MergeDefaults(barbican.Spec.ExtraConfig, merged)
	}

	// Handle PolicyOverrides: render policy.yaml and wire oslo_policy.policy_file.
	var policyYAML string
	if barbican.Spec.PolicyOverrides != nil {
		rendered, err := buildPolicyYAML(ctx, children, barbican)
		if err != nil {
			markConfigFailed(barbican, err)
			return ctrl.Result{}, "", fmt.Errorf("building policy: %w", err)
		}
		policyYAML = rendered
		if policyYAML != "" {
			merged = config.InjectOsloPolicyConfig(merged, policyFilePath)
		}
	}

	pasteINI, err := renderPasteINI(barbican)
	if err != nil {
		markConfigFailed(barbican, err)
		return ctrl.Result{}, "", fmt.Errorf("rendering barbican-api-paste.ini: %w", err)
	}

	data := map[string][]byte{
		barbicanConfDataKey:  []byte(config.RenderINI(merged)),
		barbicanPasteDataKey: []byte(pasteINI),
	}
	if policyYAML != "" {
		data[policyFileDataKey] = []byte(policyYAML)
	}

	secretName, err := config.CreateImmutableSecret(ctx, children, r.Scheme, barbican,
		barbican.Name+"-config", barbican.Namespace, data)
	if err != nil {
		markConfigFailed(barbican, err)
		return ctrl.Result{}, "", fmt.Errorf("creating config Secret: %w", err)
	}
	if err := config.PruneImmutableSecrets(ctx, children, r.Scheme, barbican, config.PruneOptions{
		BaseName:    barbican.Name + "-config",
		Namespace:   barbican.Namespace,
		CurrentName: secretName,
		Retain:      defaultConfigRetainCount,
	}); err != nil {
		markConfigFailed(barbican, err)
		return ctrl.Result{}, "", fmt.Errorf("pruning config Secrets: %w", err)
	}

	return ctrl.Result{}, secretName, nil
}

// operatorDefaults builds the operator-owned barbican.conf sections from the CRD
// spec and the secret-store projection. It is a pure function of the spec and
// the projection (no cluster access), so it can be called directly to assert the
// rendered defaults stay in lockstep with barbicanv1alpha1.OwnedConfigKeys.
func operatorDefaults(barbican *barbicanv1alpha1.Barbican, projection secretStoreProjection) map[string]map[string]string {
	defaults := map[string]map[string]string{
		"DEFAULT": {
			// Barbican defaults this to true and creates its tables from the models
			// on API start. The operator migrates through the db-sync Job instead, so
			// the schema has exactly one writer and carries an alembic revision.
			"db_auto_create": "false",
			// The base URL barbican writes into the self-referencing links of its API
			// responses.
			"host_href": barbicanPublicEndpoint(barbican),
		},
		// Barbican keeps its database options in the plain [database] section its
		// sibling services use; barbican 22.0.0 has no [DEFAULT] sql_connection.
		"database": {
			"max_retries":             "-1",
			"connection_recycle_time": "600",
			// The real URL is materialized by reconcileDBConnectionSecret into a
			// derived Secret and injected at runtime via OS_DATABASE__CONNECTION.
			"connection": dbConnectionPlaceholder,
		},
		// The password is delivered through the OS_KEYSTONE_AUTHTOKEN__PASSWORD env
		// override the deployment step wires (keystoneauth.PasswordEnvVar), so it
		// never lands in the rendered file.
		"keystone_authtoken": keystoneauth.Section(keystoneauth.SectionParams{
			AuthURL:            barbican.Spec.KeystoneEndpoint,
			WWWAuthenticateURI: barbican.Spec.EffectiveKeystonePublicEndpoint(),
			Username:           barbican.Spec.ServiceUser.Username,
			ProjectName:        barbican.Spec.ServiceUser.ProjectName,
			UserDomainName:     barbican.Spec.ServiceUser.UserDomainName,
			ProjectDomainName:  barbican.Spec.ServiceUser.ProjectDomainName,
			RegionName:         barbican.Spec.Region,
			MemcachedServers:   cache.ResolveServers(&barbican.Spec.Cache),
		}),
		// Barbican's asynchronous order processing. The queue only moves work to a
		// barbican-worker process, and this operator projects no worker Deployment,
		// so an enabled queue would leave orders pending instead of failing.
		"queue": {
			"enable": "false",
		},
	}

	for name, section := range projection.sections {
		defaults[name] = section
	}
	return defaults
}

// barbicanPublicEndpoint returns the externally reachable Barbican API URL: when
// spec.gateway is set, https://{hostname} (implicit port 443, the Gateway
// listener terminates TLS), otherwise the cluster-local Service DNS URL so CRs
// without external exposure still resolve a usable address. It is rendered as
// [DEFAULT] host_href and is what the status endpoint and the httproute step
// report.
//
// The URL carries NO trailing slash: barbican concatenates host_href with the
// resource path when it builds the self-referencing links of an API response, so
// a trailing slash would produce a doubled separator in every link.
func barbicanPublicEndpoint(barbican *barbicanv1alpha1.Barbican) string {
	if barbican.Spec.Gateway != nil {
		return "https://" + barbican.Spec.Gateway.Hostname
	}
	return internalBarbicanURL(barbican)
}

// renderPasteINI renders barbican-api-paste.ini, mirroring the file the release's
// image ships. Only the keystone pipeline composite:main routes /v1 to is built
// through the shared renderer, so spec.middleware extends the pipeline that
// actually serves the API; the remaining pipelines, apps and filter factories are
// merged in verbatim.
func renderPasteINI(barbican *barbicanv1alpha1.Barbican) (string, error) {
	requestID := pasteCarriesRequestID(barbican)

	sections, err := plugins.RenderPastePipeline(plugins.PipelineSpec{
		PipelineName: keystonePipelineName,
		// apiapp terminates the pipeline directive; its [app:apiapp] section is
		// supplied verbatim below with a paste.app_factory (not the egg: use
		// directive the renderer would emit), so no AppFactory is set here.
		AppName:     "apiapp",
		BaseFilters: keystoneBaseFilters(requestID),
		Middleware:  barbican.Spec.Middleware,
	})
	if err != nil {
		return "", err
	}

	for name, section := range barbicanStaticPasteSections(requestID) {
		sections[name] = section
	}
	return config.RenderINI(sections), nil
}

// pasteCarriesRequestID reports whether the release ships the 2026.1 paste
// layout: the oslo request_id filter in every pipeline, and no repoze.profile
// pipeline or filter section. Both deltas ride on the same release boundary, so
// they are decided once. An unparseable release falls back to the older layout,
// which the CRD pattern and the validating webhook make unreachable.
func pasteCarriesRequestID(barbican *barbicanv1alpha1.Barbican) bool {
	rel, err := release.ParseRelease(barbican.Spec.OpenStackRelease)
	if err != nil {
		return false
	}
	return rel.Year > 2026 || (rel.Year == 2026 && rel.Minor >= 1)
}

// keystoneBaseFilters returns the filters of the keystone pipeline in pipeline
// order, ahead of any spec.middleware the shared renderer positions around them.
func keystoneBaseFilters(requestID bool) []string {
	return append(pasteFrontFilters(requestID), "authtoken", "context", "microversion")
}

// pasteFrontFilters returns the filters every barbican pipeline starts with. The
// request_id filter is 2026.1-only and sits directly behind cors, where the
// shipped file places it.
func pasteFrontFilters(requestID bool) []string {
	if requestID {
		return []string{"cors", "request_id", "http_proxy_to_wsgi"}
	}
	return []string{"cors", "http_proxy_to_wsgi"}
}

// pasteLine joins a pipeline's front filters with the rest of its members into
// the space-separated value a [pipeline:*] section carries. It copies front, so
// no caller's slice is appended into.
func pasteLine(front []string, rest ...string) string {
	members := make([]string, 0, len(front)+len(rest))
	members = append(members, front...)
	members = append(members, rest...)
	return strings.Join(members, " ")
}

// barbicanStaticPasteSections returns the barbican-api-paste.ini sections the
// PipelineSpec cannot express: the urlmap composite, the pipelines other than
// the keystone one, the WSGI apps, and the base filter factories. They mirror
// the file the release's image ships.
//
// Healthcheck is routed as an app under /healthcheck rather than as a pipeline
// filter, which is what upstream ships and what keeps the probes outside
// authtoken.
func barbicanStaticPasteSections(requestID bool) map[string]map[string]string {
	front := pasteFrontFilters(requestID)

	sections := map[string]map[string]string{
		"composite:main": {
			"use":          "egg:Paste#urlmap",
			"/":            "barbican_version",
			"/healthcheck": "healthcheck",
			"/v1":          keystonePipelineName,
		},
		"pipeline:barbican_version": {
			"pipeline": pasteLine(front, "microversion", "versionapp"),
		},
		"pipeline:barbican_api": {
			"pipeline": pasteLine(front, "unauthenticated-context", "microversion", "apiapp"),
		},
		"app:apiapp": {
			"paste.app_factory": "barbican.api.app:create_main_app",
		},
		"app:versionapp": {
			"paste.app_factory": "barbican.api.app:create_version_app",
		},
		"app:healthcheck": {
			"paste.app_factory":    "oslo_middleware:Healthcheck.app_factory",
			"backends":             "disable_by_file",
			"disable_by_file_path": barbicanConfigDir + "healthcheck_disable",
		},
		"filter:simple": {
			"paste.filter_factory": "barbican.api.middleware.simple:SimpleFilter.factory",
		},
		"filter:unauthenticated-context": {
			"paste.filter_factory": "barbican.api.middleware.context:UnauthenticatedContextMiddleware.factory",
		},
		"filter:context": {
			"paste.filter_factory": "barbican.api.middleware.context:ContextMiddleware.factory",
		},
		"filter:microversion": {
			"paste.filter_factory": "barbican.api.middleware.microversion:MicroversionMiddleware.factory",
		},
		"filter:audit": {
			"paste.filter_factory": "keystonemiddleware.audit:filter_factory",
			"audit_map_file":       barbicanConfigDir + "api_audit_map.conf",
		},
		"filter:authtoken": {
			"paste.filter_factory": "keystonemiddleware.auth_token:filter_factory",
		},
		"filter:cors": {
			"paste.filter_factory": "oslo_middleware.cors:filter_factory",
			"oslo_config_project":  "barbican",
		},
		"filter:http_proxy_to_wsgi": {
			"paste.filter_factory": "oslo_middleware:HTTPProxyToWSGI.factory",
		},
	}

	if requestID {
		sections["filter:request_id"] = map[string]string{
			"paste.filter_factory": "oslo_middleware.request_id:RequestId.factory",
		}
		sections["pipeline:barbican-api-keystone-audit"] = map[string]string{
			"pipeline": pasteLine(front, "authtoken", "context", "microversion", "audit", "apiapp"),
		}
		return sections
	}

	// The audit pipeline of the older file starts at http_proxy_to_wsgi: it
	// predates the cors entry the 2026.1 file gained.
	sections["pipeline:barbican-api-keystone-audit"] = map[string]string{
		"pipeline": "http_proxy_to_wsgi authtoken context microversion audit apiapp",
	}
	// The repoze.profile pipeline and its filter, dropped upstream in 2026.1.
	sections["pipeline:barbican-profile"] = map[string]string{
		"pipeline": pasteLine(front, "unauthenticated-context", "microversion",
			"egg:Paste#cgitb", "egg:Paste#httpexceptions", "profile", "apiapp"),
	}
	sections["filter:profile"] = map[string]string{
		"use":                   "egg:repoze.profile",
		"log_filename":          "myapp.profile",
		"cachegrind_filename":   "cachegrind.out.myapp",
		"discard_first_request": "true",
		"path":                  "/__profile__",
		"flush_at_shutdown":     "true",
		"unwind":                "false",
	}
	return sections
}

// lastGoodConfigSecret returns the config Secret name the running Barbican
// Deployment currently mounts, so an invalid secret-store projection keeps the
// last-good config instead of re-rendering. On first install (no Deployment yet)
// it returns an empty name.
func (r *BarbicanReconciler) lastGoodConfigSecret(ctx context.Context, children client.Client, barbican *barbicanv1alpha1.Barbican) (ctrl.Result, string, error) {
	var deploy appsv1.Deployment
	key := client.ObjectKey{Namespace: barbican.Namespace, Name: subResourceName(barbican)}
	if err := children.Get(ctx, key, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, "", nil
		}
		return ctrl.Result{}, "", fmt.Errorf("fetching Deployment %s for last-good config: %w", key, err)
	}

	for i := range deploy.Spec.Template.Spec.Volumes {
		v := &deploy.Spec.Template.Spec.Volumes[i]
		if v.Name == configVolumeName && v.Secret != nil {
			return ctrl.Result{}, v.Secret.SecretName, nil
		}
	}
	return ctrl.Result{}, "", nil
}

// buildPolicyYAML builds the policy.yaml content from spec.policyOverrides,
// merging inline rules over any ConfigMap-sourced rules (inline wins).
func buildPolicyYAML(ctx context.Context, c client.Client, barbican *barbicanv1alpha1.Barbican) (string, error) {
	po := barbican.Spec.PolicyOverrides
	if po == nil {
		return "", nil
	}

	var rules map[string]string
	if po.ConfigMapRef != nil {
		cmRules, err := policy.LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{
			Namespace: barbican.Namespace,
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
