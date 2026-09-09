// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/cache"
	"github.com/c5c3/cobaltcore/internal/common/config"
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	"github.com/c5c3/cobaltcore/internal/common/policy"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// cinderConfigDir is the in-pod directory the rendered config ConfigMap is
// mounted at (oslo.config --config-dir). The migration Jobs
// (reconcile_database.go) and the API, scheduler, volume and backup workloads
// mount the same path, so the policy and logging file references below stay in
// lockstep with the files beside them.
const cinderConfigDir = "/etc/cinder/cinder.conf.d"

// cinderBackendsConfigDir is the in-pod directory each cinder-volume mounts its
// backend projection at. The nfs_shares_config option rendered per backend
// (reconcile_backends.go) names a file inside it.
const cinderBackendsConfigDir = "/etc/cinder/backends.conf.d"

// configVolumeName is the pod volume the workload steps project the rendered
// config ConfigMap through. reconcileConfig reads it back off the live
// Deployment to recover the last-good artefacts when a rendered section is
// unusable — a naming contract shared with the workload steps.
const configVolumeName = "config"

// The data keys of the rendered config ConfigMap.
//
// The whole ConfigMap is what the migration Jobs mount at cinderConfigDir, so
// every key here is a file oslo.config sees in the config directory. Only the
// *.conf files are parsed as configuration; logging.ini and policy.yaml are
// referenced by the options that consume them. logging.ini carries the .ini
// suffix for exactly that reason: named .conf, oslo would parse the oslo.log
// fileConfig document as service configuration.
const (
	cinderConfDataKey    = "cinder.conf"
	schedulerConfDataKey = "scheduler.conf"
	loggingINIDataKey    = "logging.ini"
	policyYAMLDataKey    = "policy.yaml"
)

// File paths inside cinderConfigDir the rendered options point at.
const (
	// loggingINIPath is the oslo.log fileConfig path [DEFAULT] log_config_append
	// names while spec.logging.format is json.
	loggingINIPath = cinderConfigDir + "/" + loggingINIDataKey
	// policyFilePath is the oslo.policy path [oslo_policy] policy_file names
	// while spec.policyOverrides is set.
	policyFilePath = cinderConfigDir + "/" + policyYAMLDataKey
)

// Paths the cinder image ships its static data files at. Cinder's setup.cfg
// installs etc/cinder/* under <prefix>/etc/cinder, and the image installs cinder
// with --prefix /var/lib/openstack (images/cinder/Dockerfile), so the two must
// stay in lockstep. The oslo defaults (/etc/cinder/…) do not exist in the
// config-free image, which is why both options are rendered explicitly.
const (
	// apiPasteConfigPath is the WSGI pipeline definition [DEFAULT]
	// api_paste_config names. It is what puts keystonemiddleware in front of the
	// API.
	apiPasteConfigPath = "/var/lib/openstack/etc/cinder/api-paste.ini"
	// resourceFiltersPath is the query-filter allow-list [DEFAULT]
	// resource_query_filters_file names. Without it the API filters nothing.
	resourceFiltersPath = "/var/lib/openstack/etc/cinder/resource_filters.json"
)

// Writable in-pod paths. Every process runs on a read-only root filesystem, so
// each of these is a volume the workload steps mount.
const (
	// cinderStatePath is [DEFAULT] state_path, the root cinder derives its
	// working directories from.
	cinderStatePath = "/var/lib/cinder"
	// cinderConversionDir is [DEFAULT] image_conversion_dir, the scratch space a
	// create-volume-from-image conversion writes the intermediate image to.
	cinderConversionDir = cinderStatePath + "/conversion"
	// cinderLockPath is [oslo_concurrency] lock_path, the directory the
	// process-local file locks live in.
	cinderLockPath = cinderStatePath + "/tmp"
	// cinderCoordinationURL is [coordination] backend_url. The file driver takes
	// its locks inside the pod rather than across the fleet, which is what the
	// single-writer topology needs: the volume and backup services own their
	// storage through a host identity, and each runs exactly one pod.
	cinderCoordinationURL = "file://" + cinderStatePath + "/coordination"
)

// The RabbitMQ CA bundle mount. [oslo_messaging_rabbit] ssl_ca_file names the
// file, and the workload steps project the bundle Secret at the directory.
const (
	rabbitmqCAMountPath = "/etc/rabbitmq-ca"
	rabbitmqCAFilePath  = rabbitmqCAMountPath + "/ca.crt"
)

// privsepHelperCommand is the command line the two privsep contexts start their
// privileged helper with. It repeats the config directory because the helper is
// a fresh process that loads the same configuration the unprivileged service
// did, and the config-free image ships no /etc/cinder/cinder.conf for it to fall
// back to.
const privsepHelperCommand = "privsep-helper --config-dir " + cinderConfigDir

// dbConnectionPlaceholder is the placeholder URL written to the [database]
// connection key in cinder.conf. The real URL is injected at runtime via the
// OS_DATABASE__CONNECTION env var sourced from the derived
// <cinder.Name>-db-connection Secret (oslo.config OS_<GROUP>__<OPTION>
// override). The placeholder MUST be a syntactically valid pymysql URL so
// oslo.config parses the file cleanly before the env override is applied.
const dbConnectionPlaceholder = "mysql+pymysql://placeholder"

// configArtifacts names the immutable config ConfigMap the config step produced
// (or, when a rendered section is unusable, the one the live Deployment
// currently mounts). The database step consumes configMapName; the workload
// steps mount the named keys.
type configArtifacts struct {
	// configMapName is the content-hashed cinder.conf ConfigMap.
	configMapName string
	// dataKeys are the ConfigMap's data keys, sorted. The workload steps project
	// a subset of them per process, so they need to know which optional files
	// (logging.ini, policy.yaml) this render produced.
	dataKeys []string
}

// reconcileConfig renders cinder.conf and scheduler.conf into an immutable
// ConfigMap (plus logging.ini and policy.yaml when applicable) and returns the
// artefact names for the database and workload steps. Config failures flip
// SecretsReady=False (the Config→SecretsReady mapping the sibling operators use)
// so the aggregate Ready cannot stay stale-True at the new generation.
//
// A rendered section carrying a control character does NOT re-render: it returns
// the artefacts the live Deployment currently mounts, so the running pods keep
// their last-good config instead of a document with injected INI lines, and
// empty names on first install.
func (r *CinderReconciler) reconcileConfig(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder,
) (ctrl.Result, configArtifacts, error) {
	// The extraConfig ownership guard is a pure function of the spec. The
	// ExtraConfigHealthy condition it maintains is informational and deliberately
	// stays out of the Ready aggregation.
	config.RecordExtraConfigHealth(r.Recorder, cinder, &cinder.Status.Conditions, cinder.Generation,
		config.FindOwnedOverrides(cinder.Spec.ExtraConfig, cinderv1alpha1.OwnedConfigKeys))

	// extraConfig overrides the operator defaults (the true escape hatch); the
	// ownership guard above reports what it took over.
	merged := config.MergeDefaults(cinder.Spec.ExtraConfig, operatorDefaults(cinder))

	// PolicyOverrides: render policy.yaml and wire oslo_policy.policy_file.
	var policyYAML string
	if cinder.Spec.PolicyOverrides != nil {
		yaml, err := buildPolicyYAML(ctx, children, cinder)
		if err != nil {
			markConfigFailed(cinder, err)
			return ctrl.Result{}, configArtifacts{}, fmt.Errorf("building policy: %w", err)
		}
		policyYAML = yaml
		if policyYAML != "" {
			merged = config.InjectOsloPolicyConfig(merged, policyFilePath)
		}
	}

	// Last line of defense behind the validating webhook: a newline in an option
	// name or value injects further INI lines into the rendered section, and the
	// webhook never sees a CR written past admission. Sections are checked in
	// name order so the reported one is the same on every pass.
	for _, section := range slices.Sorted(maps.Keys(merged)) {
		if err := config.CheckNoControlChars(section, merged[section]); err != nil {
			markConfigFailed(cinder, err)
			return r.lastGoodArtifacts(ctx, children, cinder)
		}
	}

	data := map[string]string{
		cinderConfDataKey: config.RenderINI(merged),
		// The scheduler overlay. Every scheduler pod registers under one identity
		// however many replicas run: schedulers are peers that hold no state, so a
		// per-pod identity would only fill the service registry with entries no
		// volume is keyed by.
		schedulerConfDataKey: "[DEFAULT]\nhost = " + cinder.Name + "-scheduler\n",
	}
	if logging := effectiveLogging(cinder.Spec.Logging); logging.Format == "json" {
		data[loggingINIDataKey] = config.RenderLoggingConf(logging.Level)
	}
	if policyYAML != "" {
		data[policyYAMLDataKey] = policyYAML
	}

	baseName := cinder.Name + "-config"
	configMapName, err := config.CreateImmutableConfigMap(ctx, children, r.Scheme, cinder,
		baseName, cinder.Namespace, data)
	if err != nil {
		markConfigFailed(cinder, err)
		return ctrl.Result{}, configArtifacts{}, fmt.Errorf("creating config ConfigMap: %w", err)
	}
	if err := config.PruneImmutableConfigMaps(ctx, children, r.Scheme, cinder, config.PruneOptions{
		BaseName:    baseName,
		Namespace:   cinder.Namespace,
		CurrentName: configMapName,
		Retain:      defaultConfigMapRetainCount,
	}); err != nil {
		markConfigFailed(cinder, err)
		return ctrl.Result{}, configArtifacts{}, fmt.Errorf("pruning config ConfigMaps: %w", err)
	}

	return ctrl.Result{}, configArtifacts{
		configMapName: configMapName,
		dataKeys:      slices.Sorted(maps.Keys(data)),
	}, nil
}

// operatorDefaults builds the operator-owned cinder.conf sections from the CRD
// spec: the static [DEFAULT]/[database]/[oslo_*] scaffolding plus the Keystone,
// key-manager, image-service and logging conditionals. It is a pure function of
// the spec (no cluster access), so the registry drift-guard test can call it
// directly to assert the rendered defaults stay in lockstep with
// cinderv1alpha1.OwnedConfigKeys.
//
// [DEFAULT] enabled_backends is deliberately absent. Each cinder-volume serves
// exactly one backend and gets a per-backend overlay naming it
// (reconcile_backends.go), so the key never belongs in the document the API and
// the scheduler read.
func operatorDefaults(cinder *cinderv1alpha1.Cinder) map[string]map[string]string {
	logging := effectiveLogging(cinder.Spec.Logging)

	// The WSGI pipeline api-paste.ini serves the API through. keystone is the
	// only one of them that runs keystonemiddleware, so a Cinder deployed without
	// the Keystone integration says so in the file rather than naming a pipeline
	// whose middleware has no credentials to authenticate with.
	authStrategy := "noauth"
	if cinder.Spec.KeystoneEndpoint != "" {
		authStrategy = "keystone"
	}

	defaults := map[string]map[string]string{
		"DEFAULT": {
			"auth_strategy":               authStrategy,
			"api_paste_config":            apiPasteConfigPath,
			"resource_query_filters_file": resourceFiltersPath,
			"state_path":                  cinderStatePath,
			"image_conversion_dir":        cinderConversionDir,
			// The identity half the operator owns. cinder appends "@<backend>" to
			// it per volume service, and the volumes a backend owns are keyed by the
			// result, so it is derived from the CR name and never from a pod.
			"host": cinder.Name,
			// Route oslo.log records to stderr so kubectl logs surfaces them.
			"use_stderr": "true",
			// oslo.log gates several extra-verbose code paths on the debug flag
			// specifically, independent of the root logger level. Debug is a
			// nil-preserving *bool: nil renders as the default (false).
			"debug": fmt.Sprintf("%t", logging.Debug != nil && *logging.Debug),
		},
		"database": {
			// The real URL is materialized by reconcileDBConnectionSecret into a
			// derived Secret and injected at runtime via OS_DATABASE__CONNECTION.
			"connection": dbConnectionPlaceholder,
		},
		"oslo_messaging_rabbit": messaging.RabbitSection(cinder.Spec.Messaging.TLS, rabbitmqCAFilePath),
		// Volume lifecycle notifications are not consumed by anything this
		// deployment runs, so they are dropped at the source rather than published
		// into a queue nothing drains.
		"oslo_messaging_notifications": {"driver": "noop"},
		"oslo_concurrency":             {"lock_path": cinderLockPath},
		"coordination":                 {"backend_url": cinderCoordinationURL},
		// The two privsep contexts the volume and backup services run privileged
		// operations through. capabilities is rendered empty on purpose: the pod's
		// security context grants none, so the helper keeps none, and rendering the
		// key rather than omitting it is what makes it the operator's to own.
		"cinder_sys_admin": {"helper_command": privsepHelperCommand, "capabilities": ""},
		"privsep_osbrick":  {"helper_command": privsepHelperCommand, "capabilities": ""},
	}

	// The image service create-volume-from-image reads through. Omitted when
	// unset, so a Cinder with Keystone resolves Glance from the catalog instead.
	if cinder.Spec.GlanceEndpoint != "" {
		defaults["DEFAULT"]["glance_api_servers"] = cinder.Spec.GlanceEndpoint
	}
	// The project and user the image-volume cache owns its volumes as, so a
	// cached volume belongs to the deployment rather than to whichever tenant's
	// request populated the cache.
	if tenant := cinder.Spec.InternalTenant; tenant != nil {
		defaults["DEFAULT"]["cinder_internal_tenant_project_id"] = tenant.ProjectID
		defaults["DEFAULT"]["cinder_internal_tenant_user_id"] = tenant.UserID
	}
	// PerLoggerLevels render into oslo.log's default_log_levels CSV; empty omits
	// the key so oslo.log keeps its compiled-in defaults.
	if levels := config.RenderSortedPairs(logging.PerLoggerLevels, "="); levels != "" {
		defaults["DEFAULT"]["default_log_levels"] = levels
	}
	// format=json ships a logging.ini and points oslo.log at it via
	// log_config_append.
	if logging.Format == "json" {
		defaults["DEFAULT"]["log_config_append"] = loggingINIPath
	}

	if serviceUser := keystoneServiceUser(cinder); serviceUser != nil {
		params := keystoneauth.SectionParams{
			AuthURL:            cinder.Spec.KeystoneEndpoint,
			WWWAuthenticateURI: cinder.Spec.EffectiveKeystonePublicEndpoint(),
			Username:           serviceUser.Username,
			ProjectName:        serviceUser.ProjectName,
			UserDomainName:     serviceUser.UserDomainName,
			ProjectDomainName:  serviceUser.ProjectDomainName,
			RegionName:         cinder.Spec.Region,
			MemcachedServers:   cache.ResolveServers(&cinder.Spec.Cache),
		}
		defaults["keystone_authtoken"] = keystoneauth.Section(params)
		// The same account, sending a token of cinder's own alongside the user's,
		// so a long-running call to Glance or Barbican outlives the user token.
		defaults["service_user"] = keystoneauth.ServiceUserSection(params)
	}

	// castellan reaches Barbican with the Keystone credentials above, so the
	// block carries the endpoint alone. The nil-Barbican case belongs to a CR
	// that bypassed the union rule: nothing is rendered, and an encrypted volume
	// type fails at use rather than the operator failing at render.
	if km := cinder.Spec.KeyManager; km != nil && km.Barbican != nil {
		defaults["key_manager"] = map[string]string{"backend": "barbican"}
		defaults["barbican"] = map[string]string{
			"barbican_endpoint": km.Barbican.Endpoint,
			// The endpoint is configured explicitly, so the catalog lookup the type
			// selects never runs; pinning it keeps castellan off the public entry
			// if it ever does.
			"barbican_endpoint_type": "internal",
			"auth_endpoint":          cinder.Spec.KeystoneEndpoint,
		}
	}

	return defaults
}

// keystoneServiceUser returns the service account the Keystone sections render
// from, or nil when this Cinder configures no Keystone integration. The CEL
// pairing rule on CinderSpec keeps spec.keystoneEndpoint and spec.serviceUser
// together, so the two-field check only splits for a CR written past admission;
// such a CR still renders auth_strategy = keystone and fails at startup for want
// of credentials, rather than crashing the reconcile.
func keystoneServiceUser(cinder *cinderv1alpha1.Cinder) *cinderv1alpha1.ServiceUserSpec {
	if cinder.Spec.KeystoneEndpoint == "" {
		return nil
	}
	return cinder.Spec.ServiceUser
}

// effectiveLogging returns the LoggingSpec to use for config rendering,
// materializing the production defaults when spec.logging is nil (a CR that
// bypassed the defaulting webhook). It mirrors the sibling operators'
// effectiveLogging.
func effectiveLogging(spec *cinderv1alpha1.LoggingSpec) cinderv1alpha1.LoggingSpec {
	out := cinderv1alpha1.LoggingSpec{Format: "text", Level: "INFO"}
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

// buildPolicyYAML builds the policy.yaml content from spec.policyOverrides,
// merging inline rules over any ConfigMap-sourced rules (inline wins). It
// mirrors the sibling operators' buildPolicyYAML.
func buildPolicyYAML(ctx context.Context, c client.Client, cinder *cinderv1alpha1.Cinder) (string, error) {
	po := cinder.Spec.PolicyOverrides
	if po == nil {
		return "", nil
	}

	var rules map[string]string
	if po.ConfigMapRef != nil {
		cmRules, err := policy.LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{
			Namespace: cinder.Namespace,
			Name:      po.ConfigMapRef.Name,
		})
		if err != nil {
			return "", fmt.Errorf("loading policy from ConfigMap: %w", err)
		}
		rules = cmRules
	}
	if len(po.Rules) > 0 {
		if rules == nil {
			rules = make(map[string]string, len(po.Rules))
		}
		maps.Copy(rules, po.Rules)
	}
	return policy.RenderPolicyYAML(rules)
}

// lastGoodArtifacts returns the config ConfigMap the running Cinder API
// Deployment currently mounts, so an unusable render keeps the last-good config
// instead of replacing it. The data keys come from the volume's Items, which is
// how the workload step projects the per-process file subset. On first install
// (no Deployment yet) it returns empty artefacts.
func (r *CinderReconciler) lastGoodArtifacts(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder,
) (ctrl.Result, configArtifacts, error) {
	var deploy appsv1.Deployment
	key := client.ObjectKey{Namespace: cinder.Namespace, Name: cinder.Name}
	if err := children.Get(ctx, key, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, configArtifacts{}, nil
		}
		return ctrl.Result{}, configArtifacts{}, fmt.Errorf("fetching Deployment %s for last-good config: %w", key, err)
	}

	var art configArtifacts
	for _, volume := range deploy.Spec.Template.Spec.Volumes {
		if volume.Name != configVolumeName || volume.ConfigMap == nil {
			continue
		}
		art.configMapName = volume.ConfigMap.Name
		for _, item := range volume.ConfigMap.Items {
			art.dataKeys = append(art.dataKeys, item.Key)
		}
		slices.Sort(art.dataKeys)
		break
	}
	return ctrl.Result{}, art, nil
}
