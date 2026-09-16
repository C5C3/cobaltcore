// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/cache"
	"github.com/c5c3/cobaltcore/internal/common/config"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// novaConfigDir is the in-pod directory the rendered config ConfigMap is mounted
// at. The nova image ships no /etc/nova/nova.conf, so every process is started
// with oslo.config's --config-dir pointing here and reads the whole directory:
// the shared nova.conf first, then the single-section overlay of its own role.
// The migration Jobs and the five workloads mount the same path, so the logging
// file reference below stays in lockstep with the file beside it.
const novaConfigDir = "/etc/nova/nova.conf.d"

// configVolumeName is the pod volume the workload steps project the rendered
// config ConfigMap through. lastGoodArtifacts reads it back off the live API
// Deployment to recover the last-good artefacts when a rendered section is
// unusable, which makes the name a contract shared with the workload steps.
const configVolumeName = "config"

// The data keys of the rendered config ConfigMap.
//
// nova.conf is the shared document every process reads. The four *.conf
// overlays beside it carry the options one role owns alone, and each workload
// projects its own overlay next to nova.conf so a scheduler never reads the
// console proxy's listen address. logging.ini carries the .ini suffix so
// oslo.config does not parse the fileConfig document as service configuration
// when it reads the directory.
const (
	novaConfDataKey       = "nova.conf"
	metadataConfDataKey   = "metadata.conf"
	schedulerConfDataKey  = "scheduler.conf"
	conductorConfDataKey  = "conductor.conf"
	novncproxyConfDataKey = "novncproxy.conf"
	loggingINIDataKey     = "logging.ini"
)

// loggingINIPath is the oslo.log fileConfig path [DEFAULT] log_config_append
// names while spec.logging.format is json.
const loggingINIPath = novaConfigDir + "/" + loggingINIDataKey

// Writable in-pod paths. Every process runs on a read-only root filesystem, so
// each of these is a volume the workload steps mount.
const (
	// novaStatePath is [DEFAULT] state_path, the root nova derives its working
	// directories from.
	novaStatePath = "/var/lib/nova"
	// novaLockPath is [oslo_concurrency] lock_path, the directory the
	// process-local file locks live in.
	novaLockPath = novaStatePath + "/tmp"
)

// The RabbitMQ CA bundle mount. [oslo_messaging_rabbit] ssl_ca_file names the
// file, and the workload steps project the bundle Secret at the directory under
// the same file name.
const (
	rabbitmqCAMountPath = "/etc/rabbitmq-ca"
	rabbitmqCAFilePath  = rabbitmqCAMountPath + "/" + database.TLSCAFileName
)

// discoverHostsIntervalSeconds is [scheduler] discover_hosts_in_cells_interval.
// Five minutes bounds how long a newly registered hypervisor waits to become
// schedulable, while the scan itself reads the cell's whole compute node table
// and is not something to run on every periodic tick.
const discoverHostsIntervalSeconds = 300

// The console proxy's serving parameters.
const (
	// novncWebPath is the directory the nova image installs the noVNC client at
	// and [DEFAULT] web names. The proxy serves the client from it, so the
	// browser that opens a console loads the page from the proxy itself.
	novncWebPath = "/usr/share/novnc"
	// novncproxyListenAddress is [vnc] novncproxy_host. The proxy binds every
	// address of the pod, which is what its Service routes to.
	novncproxyListenAddress = "0.0.0.0"
	// novaConsolePort is [vnc] novncproxy_port, the port the proxy listens on
	// and the port the cluster-local console URL addresses it at.
	novaConsolePort = 6080
)

// dbConnectionPlaceholder is the placeholder URL written to both connection
// keys, [api_database] connection and [database] connection. The real URLs are
// injected at runtime via the OS_API_DATABASE__CONNECTION and
// OS_DATABASE__CONNECTION env vars sourced from the two derived connection
// Secrets (oslo.config OS_<GROUP>__<OPTION> override). The placeholder MUST be a
// syntactically valid pymysql URL so oslo.config parses the file cleanly before
// the env overrides are applied.
const dbConnectionPlaceholder = "mysql+pymysql://placeholder"

// defaultConfigMapRetainCount is the number of historical immutable ConfigMaps
// to retain after pruning. Combined with the current active artefact, this
// allows rollback to 3 previous configurations.
const defaultConfigMapRetainCount = 3

// metadataOverlay is the metadata API's role overlay. service_metadata_proxy is
// what makes the metadata API trust the instance identity in the headers of a
// request the Neutron metadata agent proxied, and only that front end reads it.
// The shared secret the signature is verified with is not here: it arrives
// through an env override, so it never lands in the ConfigMap every pod mounts.
const metadataOverlay = "[neutron]\nservice_metadata_proxy = true\n"

// novncproxyOverlay is the console proxy's role overlay: the noVNC client
// directory it serves and the address pair its Service routes to. The three
// options belong to the proxy alone, so the API and the scheduler never read
// them.
var novncproxyOverlay = fmt.Sprintf(
	"[DEFAULT]\nweb = %s\n\n[vnc]\nnovncproxy_host = %s\nnovncproxy_port = %d\n",
	novncWebPath, novncproxyListenAddress, novaConsolePort)

// schedulerOverlay is the scheduler's role overlay. The worker count is a
// per-process value, so it lives beside the process rather than in the document
// the conductor and the API read.
func schedulerOverlay(workers int32) string {
	return fmt.Sprintf("[scheduler]\nworkers = %d\n", workers)
}

// conductorOverlay is the conductor's role overlay, the twin of
// schedulerOverlay.
func conductorOverlay(workers int32) string {
	return fmt.Sprintf("[conductor]\nworkers = %d\n", workers)
}

// effectiveWorkers returns the worker count to render, materializing
// novav1alpha1.DefaultWorkers when the block leaves it unset (a CR that bypassed
// the defaulting webhook).
func effectiveWorkers(workers *int32) int32 {
	if workers == nil {
		return novav1alpha1.DefaultWorkers
	}
	return *workers
}

// configArtifacts names the immutable config ConfigMap the config step produced
// (or, when a rendered section is unusable, the one the live API Deployment
// currently mounts). The database step consumes configMapName; the workload
// steps mount the named keys.
type configArtifacts struct {
	// configMapName is the content-hashed nova.conf ConfigMap.
	configMapName string
	// dataKeys are the ConfigMap's data keys, sorted. Each workload projects its
	// own subset of them, so they need to know which optional file (logging.ini)
	// this render produced.
	dataKeys []string
}

// reconcileConfig renders nova.conf and the four role overlays into an immutable
// ConfigMap (plus logging.ini when the spec selects json logging) and returns
// the artefact names for the database and workload steps. Config failures flip
// SecretsReady=False (the Config to SecretsReady mapping the sibling operators
// use) so the aggregate Ready cannot stay stale-True at the new generation.
//
// A rendered section carrying a control character does NOT re-render: it returns
// the artefacts the live API Deployment currently mounts, so the running pods
// keep their last-good config instead of a document with injected INI lines, and
// empty names on first install.
func (r *NovaReconciler) reconcileConfig(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) (ctrl.Result, configArtifacts, error) {
	// The extraConfig ownership guard is a pure function of the spec. The
	// ExtraConfigHealthy condition it maintains is informational and deliberately
	// stays out of the Ready aggregation.
	config.RecordExtraConfigHealth(r.Recorder, nova, &nova.Status.Conditions, nova.Generation,
		config.FindOwnedOverrides(nova.Spec.ExtraConfig, novav1alpha1.OwnedConfigKeys))

	// extraConfig overrides the operator defaults (the true escape hatch); the
	// ownership guard above reports what it took over.
	merged := config.MergeDefaults(nova.Spec.ExtraConfig, operatorDefaults(nova))

	// Last line of defense behind the validating webhook: a newline in an option
	// name or value injects further INI lines into the rendered section, and the
	// webhook never sees a CR written past admission. Sections are checked in
	// name order so the reported one is the same on every pass. The four overlays
	// are not checked: each is built from a constant and an integer, so no CR
	// value reaches them.
	for _, section := range slices.Sorted(maps.Keys(merged)) {
		if err := config.CheckNoControlChars(section, merged[section]); err != nil {
			markConfigFailed(nova, err)
			return r.lastGoodArtifacts(ctx, children, nova)
		}
	}

	// The four overlays ship on every render, including the console proxy's while
	// the proxy is disabled: the ConfigMap is one object for the whole CR, and a
	// key no pod mounts costs nothing, while a key that comes and goes with a
	// switch would rotate the ConfigMap and roll the four workloads that do not
	// care about it.
	data := map[string]string{
		novaConfDataKey:       config.RenderINI(merged),
		metadataConfDataKey:   metadataOverlay,
		schedulerConfDataKey:  schedulerOverlay(effectiveWorkers(nova.Spec.Scheduler.Workers)),
		conductorConfDataKey:  conductorOverlay(effectiveWorkers(nova.Spec.Conductor.Workers)),
		novncproxyConfDataKey: novncproxyOverlay,
	}
	if logging := effectiveLogging(nova.Spec.Logging); logging.Format == "json" {
		data[loggingINIDataKey] = config.RenderLoggingConf(logging.Level)
	}

	baseName := nova.Name + "-config"
	configMapName, err := config.CreateImmutableConfigMap(ctx, children, r.Scheme, nova,
		baseName, nova.Namespace, data)
	if err != nil {
		markConfigFailed(nova, err)
		return ctrl.Result{}, configArtifacts{}, fmt.Errorf("creating config ConfigMap: %w", err)
	}
	if err := config.PruneImmutableConfigMaps(ctx, children, r.Scheme, nova, config.PruneOptions{
		BaseName:    baseName,
		Namespace:   nova.Namespace,
		CurrentName: configMapName,
		Retain:      defaultConfigMapRetainCount,
	}); err != nil {
		markConfigFailed(nova, err)
		return ctrl.Result{}, configArtifacts{}, fmt.Errorf("pruning config ConfigMaps: %w", err)
	}

	return ctrl.Result{}, configArtifacts{
		configMapName: configMapName,
		dataKeys:      slices.Sorted(maps.Keys(data)),
	}, nil
}

// operatorDefaults builds the operator-owned nova.conf sections from the CRD
// spec: the static [DEFAULT]/[api]/[oslo_*] scaffolding, the two placeholder
// connection strings, the Keystone sections, the client sections of the services
// nova calls, and the console-proxy switch. It is a pure function of the spec
// (no cluster access), so the registry drift-guard test can call it directly to
// assert the rendered defaults stay in lockstep with
// novav1alpha1.OwnedConfigKeys.
//
// [DEFAULT] host is deliberately absent. Every process registers under its own
// pod name, which is what lets the scheduler and conductor replicas hold
// separate service records and elect a leader among themselves; one shared
// identity would collapse the fleet into a single record.
func operatorDefaults(nova *novav1alpha1.Nova) map[string]map[string]string {
	spec := &nova.Spec
	logging := effectiveLogging(spec.Logging)
	params := clientSectionParams(spec)

	defaults := map[string]map[string]string{
		"DEFAULT": {
			// Route oslo.log records to stderr so kubectl logs surfaces them.
			"use_stderr": "true",
			// oslo.log gates several extra-verbose code paths on the debug flag
			// specifically, independent of the root logger level. Debug is a
			// nil-preserving *bool: nil renders as the default (false).
			"debug":      fmt.Sprintf("%t", logging.Debug != nil && *logging.Debug),
			"state_path": novaStatePath,
		},
		// The operator runs one global metadata front end rather than one per
		// cell, so the API resolves an instance through the API database instead
		// of reading a single cell it was deployed beside.
		"api": {"local_metadata_per_cell": "false"},
		// Both real URLs are materialized by reconcileDBConnectionSecrets into
		// derived Secrets and injected at runtime via OS_API_DATABASE__CONNECTION
		// and OS_DATABASE__CONNECTION.
		"api_database":       {"connection": dbConnectionPlaceholder},
		"database":           {"connection": dbConnectionPlaceholder},
		"keystone_authtoken": keystoneauth.Section(params),
		// The same account, sending a token of nova's own alongside the user's, so
		// a boot that outlives the user token still reaches Placement, Neutron,
		// Glance and Cinder.
		"service_user":          keystoneauth.ServiceUserSection(params),
		"placement":             catalogClientSection(params, spec.Endpoints.Placement.Override),
		"neutron":               catalogClientSection(params, spec.Endpoints.Neutron.Override),
		"glance":                glanceSection(spec),
		"oslo_messaging_rabbit": messaging.RabbitSection(spec.Messaging.TLS, rabbitmqCAFilePath),
		// Instance lifecycle notifications are not consumed by anything this
		// deployment runs, so they are dropped at the source rather than published
		// into a queue nothing drains.
		"oslo_messaging_notifications": {"driver": "noop"},
		"oslo_concurrency":             {"lock_path": novaLockPath},
		// Computes run on other clusters and upgrade after the control plane, so
		// the compute RPC client must not send a message version newer than the
		// oldest nova-compute still registered. auto reads that version out of
		// the service records instead of the newest one this release speaks.
		"upgrade_levels": {"compute": "auto"},
		// The oslo.cache block nova keeps its resolved cell mappings in. It is the
		// same memcached the token middleware above caches validated tokens in.
		"cache": {
			"enabled":          "true",
			"backend":          spec.Cache.Backend,
			"memcache_servers": cache.ResolveServers(&spec.Cache),
		},
		// The periodic pass that maps a newly registered compute node into the
		// cell. Without it a hypervisor that joined the bus stays invisible to
		// scheduling until someone runs discover_hosts by hand.
		"scheduler": {"discover_hosts_in_cells_interval": strconv.Itoa(discoverHostsIntervalSeconds)},
		"vnc":       vncSection(nova),
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

	// The two optional siblings. A standalone Nova runs without either, and a
	// section naming a service the deployment does not run would only fail at
	// first use.
	if spec.Endpoints.Cinder.Enabled {
		defaults["cinder"] = cinderSection(spec)
	}
	if spec.Endpoints.Barbican.Enabled {
		defaults["key_manager"] = map[string]string{"backend": "barbican"}
		defaults["barbican"] = barbicanSection(spec)
	}

	return defaults
}

// clientSectionParams returns the Keystone parameters every section of nova.conf
// that authenticates is rendered from: the token middleware, the outgoing
// service token, and the client sections of Placement, Neutron and Cinder. The
// compute contract renders its own copy of the client sections from the same
// helper, so the control plane and the compute nodes cannot drift apart on the
// account they call a service as.
//
// The password is absent by construction: keystoneauth renders none of the
// sections with one, and each is injected at runtime through its own
// OS_<SECTION>__PASSWORD env override.
func clientSectionParams(spec *novav1alpha1.NovaSpec) keystoneauth.SectionParams {
	return keystoneauth.SectionParams{
		AuthURL:            spec.KeystoneEndpoint,
		WWWAuthenticateURI: spec.EffectiveKeystonePublicEndpoint(),
		Username:           spec.ServiceUser.Username,
		ProjectName:        spec.ServiceUser.ProjectName,
		UserDomainName:     spec.ServiceUser.UserDomainName,
		ProjectDomainName:  spec.ServiceUser.ProjectDomainName,
		RegionName:         spec.Region,
		MemcachedServers:   cache.ResolveServers(&spec.Cache),
	}
}

// catalogClientSection renders the credentials plus the two addressing keys a
// client section resolved from the Keystone catalog carries. valid_interfaces
// pins the internal entry, which is the one a colocated control plane can reach;
// an override addresses the service directly and leaves the catalog out of it.
func catalogClientSection(params keystoneauth.SectionParams, override string) map[string]string {
	section := keystoneauth.ClientSection(params)
	section["valid_interfaces"] = "internal"
	if override != "" {
		section["endpoint_override"] = override
	}
	return section
}

// glanceSection renders [glance], the section nova reads the image of every
// instance it boots through. It carries no credentials of its own: nova reaches
// Glance with the token of the request it is serving, backed by the outgoing
// service token from [service_user].
func glanceSection(spec *novav1alpha1.NovaSpec) map[string]string {
	section := map[string]string{"valid_interfaces": "internal"}
	if spec.Region != "" {
		section["region_name"] = spec.Region
	}
	if override := spec.Endpoints.Glance.Override; override != "" {
		section["endpoint_override"] = override
	}
	return section
}

// cinderSection renders [cinder], the section that attaches a volume to an
// instance and boots an instance from one. The section addresses the volume
// service by catalog tuple rather than by interface, so its three addressing
// keys differ from the siblings above: the region is os_region_name, and an
// override is an endpoint_template.
func cinderSection(spec *novav1alpha1.NovaSpec) map[string]string {
	params := clientSectionParams(spec)
	// The region reaches the section through os_region_name below. nova registers
	// no region_name option in [cinder], so rendering one would only be an option
	// oslo.config rejects at load.
	params.RegionName = ""

	section := keystoneauth.ClientSection(params)
	section["catalog_info"] = "block-storage:cinder:internalURL"
	if spec.Region != "" {
		section["os_region_name"] = spec.Region
	}
	if override := spec.Endpoints.Cinder.Override; override != "" {
		section["endpoint_template"] = override
	}
	return section
}

// barbicanSection renders [barbican], the section castellan reads the key of an
// encrypted volume through. It carries no credentials either: castellan
// authenticates against the Keystone named here with the service account, and
// send_service_user_token is what lets a key read outlive the user token that
// started the request.
func barbicanSection(spec *novav1alpha1.NovaSpec) map[string]string {
	section := map[string]string{
		"auth_endpoint": spec.KeystoneEndpoint,
		// The endpoint type pins the internal catalog entry. It matters for the
		// deployment that configures no barbican_endpoint below and resolves the
		// address from the catalog instead.
		"barbican_endpoint_type":  "internal",
		"send_service_user_token": "true",
	}
	if spec.Region != "" {
		section["barbican_region_name"] = spec.Region
	}
	if override := spec.Endpoints.Barbican.Override; override != "" {
		section["barbican_endpoint"] = override
	}
	return section
}

// vncSection renders [vnc], the block that decides whether an instance offers a
// console at all. A disabled proxy renders the switch off and no base URL: the
// URL names a Service that is not projected, and an address nothing answers is
// worse than no console offer.
func vncSection(nova *novav1alpha1.Nova) map[string]string {
	if !nova.Spec.ConsoleProxyEnabled() {
		return map[string]string{"enabled": "false"}
	}
	return map[string]string{
		"enabled":             "true",
		"novncproxy_base_url": consoleBaseURL(nova),
	}
}

// consoleBaseURL returns the address the API hands a browser as the console of
// an instance. The gateway hostname wins when the console proxy is exposed
// through one, because the browser reaching that URL sits outside the cluster.
// Without a gateway the URL is the cluster-local Service, which is what a
// deployment whose consoles are reached from inside the cluster (an e2e suite,
// say) needs.
func consoleBaseURL(nova *novav1alpha1.Nova) string {
	if gateway := nova.Spec.ConsoleProxy.Gateway; gateway != nil {
		return fmt.Sprintf("https://%s/vnc_lite.html", gateway.Hostname)
	}
	return fmt.Sprintf("http://%s-novncproxy.%s.svc.cluster.local:%d/vnc_lite.html",
		nova.Name, nova.Namespace, novaConsolePort)
}

// effectiveLogging returns the LoggingSpec to use for config rendering,
// materializing the production defaults when spec.logging is nil (a CR that
// bypassed the defaulting webhook). It mirrors the sibling operators'
// effectiveLogging.
func effectiveLogging(spec *novav1alpha1.LoggingSpec) novav1alpha1.LoggingSpec {
	out := novav1alpha1.LoggingSpec{Format: "text", Level: "INFO"}
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

// lastGoodArtifacts returns the config ConfigMap the running Nova API Deployment
// currently mounts, so an unusable render keeps the last-good config instead of
// replacing it. On first install (no Deployment yet) it returns empty artefacts.
//
// The data keys come from the ConfigMap itself, not from the API Deployment's
// volume Items: that volume projects the API's own subset of the files and never
// the metadata overlay, so keys read off it would drop metadata.conf from the
// metadata Deployment's projection, whose OS_NOVA_CONFIG_FILES still names the
// file, and roll it into a crash loop on a missing config file.
func (r *NovaReconciler) lastGoodArtifacts(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) (ctrl.Result, configArtifacts, error) {
	var deploy appsv1.Deployment
	key := client.ObjectKey{Namespace: nova.Namespace, Name: nova.Name}
	if err := children.Get(ctx, key, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, configArtifacts{}, nil
		}
		return ctrl.Result{}, configArtifacts{}, fmt.Errorf("fetching Deployment %s for last-good config: %w", key, err)
	}

	var art configArtifacts
	for _, volume := range deploy.Spec.Template.Spec.Volumes {
		if volume.Name == configVolumeName && volume.ConfigMap != nil {
			art.configMapName = volume.ConfigMap.Name
			break
		}
	}
	if art.configMapName == "" {
		return ctrl.Result{}, art, nil
	}

	var cm corev1.ConfigMap
	cmKey := client.ObjectKey{Namespace: nova.Namespace, Name: art.configMapName}
	if err := children.Get(ctx, cmKey, &cm); err != nil {
		return ctrl.Result{}, configArtifacts{}, fmt.Errorf("fetching last-good ConfigMap %s: %w", cmKey, err)
	}
	art.dataKeys = slices.Sorted(maps.Keys(cm.Data))
	return ctrl.Result{}, art, nil
}
