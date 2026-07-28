// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "github.com/c5c3/forge/internal/common/config"

// The owned-config-key registry below records the glance-api.conf keys the
// operator computes and renders, so the post-merge health guard can reason
// about user overrides from one source of truth.
//
// Two rules govern what belongs here:
//
//   - The registry is static. Conditionally rendered keys — [DEFAULT] workers /
//     default_log_levels / log_config_append, the three [DEFAULT] image_cache_*
//     keys, the [keystone_authtoken] region_name / memcached_servers, and the
//     [oslo_policy] policy_file — are registered unconditionally, because the
//     registry documents "this key is not the user's to set", not "this key is
//     currently rendered". A key the operator owns whenever it renders it stays
//     owned even in the CRs where that render is skipped.
//
//   - An entry is Reported (honored-but-surfaced through the ExtraConfigHealthy
//     condition) unless honoring the override would already have done the damage
//     by the time the condition surfaces it. Those are Rejected, and the
//     validating webhook blocks them at admission: [keystone_authtoken] password,
//     because the service password would land in the namespace-readable
//     ConfigMap; the six [import_filtering_opts] keys, because a silently
//     loosened web-download filter is the very thing spec.importFiltering exists
//     to make visible; the three [DEFAULT] image_cache_* keys, because each
//     of them ends in an evicted pod or in database rows nothing reclaims; and
//     the four image-import plugin keys, because each one reaches past a
//     guarantee spec.importPlugins makes at admission.

// OwnedConfigKeys is the registry of glance-api.conf keys the operator owns.
// Reported entries are honored-but-surfaced when overridden in spec.extraConfig;
// the Rejected entries ([keystone_authtoken] password, the six
// [import_filtering_opts] keys, the three [DEFAULT] image_cache_* keys and the
// four image-import plugin keys) are blocked at admission instead. The entries
// mirror the defaults map built by operatorDefaults (the
// [keystone_authtoken] keys come from keystoneauth.Section) plus the oslo_policy
// policy_file injected by config.InjectOsloPolicyConfig.
var OwnedConfigKeys = []config.OwnedKey{
	// [DEFAULT]
	{Section: "DEFAULT", Key: "enabled_backends", OwnedBy: "operator-computed", Impact: "must agree with the projected backends Secret"},
	{Section: "DEFAULT", Key: "enabled_import_methods", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "use_stderr", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "debug", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "workers", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "default_log_levels", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "log_config_append", OwnedBy: "operator-computed"},

	// [DEFAULT] image cache — derived from spec.imageCache. All three are
	// Rejected. Sizing a cache reads like an operational choice, but each of
	// these three overrides converges on damage that is done before
	// ExtraConfigHealthy — an informational condition that never gates Ready —
	// could surface it. image_cache_max_size above the emptyDir bound gives the
	// pruner a mark the cache can never reach, so it prunes nothing and the
	// volume grows until the kubelet evicts the pod. image_cache_dir points the
	// API's cache writes at a different path; the root filesystem is read-only,
	// so that path is another bounded emptyDir and cache growth is spent out of
	// the staging or tasks-work budget instead. image_cache_driver set back to
	// glance's centralized_db writes cache rows into the shared Glance database.
	// Every one of them is expressible through spec.imageCache or is a decision
	// the operator has to keep, so the override buys no reach the typed field
	// lacks.
	{Section: "DEFAULT", Key: "image_cache_dir", Rejected: true, OwnedBy: "spec.imageCache", Impact: "the operator mounts the bounded cache emptyDir at this path; another path is another volume's bound, so the cache is filled out of the staging or tasks-work budget and evicts the pod mid-import"},
	{Section: "DEFAULT", Key: "image_cache_driver", Rejected: true, OwnedBy: "spec.imageCache", Impact: "only sqlite keeps the cache state inside the pod-local volume; centralized_db strands a dead pod's rows in the shared Glance database on every replacement and no db-purge pass reclaims them"},
	{Section: "DEFAULT", Key: "image_cache_max_size", Rejected: true, OwnedBy: "spec.imageCache", Impact: "the operator derives it as 80% of the emptyDir bound, which is what leaves the pruner a target it can reach; a value at or above that bound leaves nothing to prune down to and the volume grows until the kubelet evicts the pod"},

	// [database]
	{Section: "database", Key: "max_retries", OwnedBy: "operator-computed"},
	{Section: "database", Key: "connection_recycle_time", OwnedBy: "operator-computed"},
	{Section: "database", Key: "connection", OwnedBy: "operator-computed", Impact: "the runtime value comes from the OS_DATABASE__CONNECTION env override, so the file override is ignored"},

	// [keystone_authtoken] — rendered by keystoneauth.Section.
	{Section: "keystone_authtoken", Key: "auth_type", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "auth_url", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "www_authenticate_uri", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "username", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "project_name", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "project_domain_name", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "user_domain_name", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "region_name", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "memcached_servers", OwnedBy: "operator-computed"},
	// password is never emitted by operatorDefaults — keystoneauth.Section
	// deliberately omits it and the middleware reads it from the
	// OS_KEYSTONE_AUTHTOKEN__PASSWORD env override, so a file value is inert at
	// runtime and stays the drift-guard test's "extras" entry (registered but not
	// rendered). It is Rejected: the validating webhook blocks an
	// extraConfig override at admission rather than merely reporting it, because
	// rendering it would leak the service password into the namespace-readable
	// ConfigMap before the ExtraConfigHealthy condition could surface it.
	{Section: "keystone_authtoken", Key: "password", Rejected: true, OwnedBy: "spec.serviceUser.secretRef", Impact: "the middleware password is env-injected via OS_KEYSTONE_AUTHTOKEN__PASSWORD; a file override is ignored at runtime and leaks credential material into a namespace-readable ConfigMap"},

	// [glance_store]
	{Section: "glance_store", Key: "default_backend", OwnedBy: "operator-computed"},

	// [os_glance_staging_store]
	{Section: "os_glance_staging_store", Key: "filesystem_store_datadir", OwnedBy: "operator-computed"},

	// [os_glance_tasks_store]
	{Section: "os_glance_tasks_store", Key: "filesystem_store_datadir", OwnedBy: "operator-computed"},

	// [paste_deploy]
	{Section: "paste_deploy", Key: "flavor", OwnedBy: "operator-computed"},
	{Section: "paste_deploy", Key: "config_file", OwnedBy: "operator-computed"},

	// [oslo_policy]
	{Section: "oslo_policy", Key: "policy_file", OwnedBy: "operator-computed"},

	// [import_filtering_opts] — derived from spec.importFiltering. All six keys
	// are always rendered (empty values included), so all six are owned, and all
	// six are Rejected. spec.importFiltering expresses every one of them, so an
	// extraConfig override buys no reach the typed field lacks; what it does buy
	// is a way around every gate that makes a loosened filter visible — the three
	// mutual-exclusivity rules, ValidateImportFiltering, the host INI guard, and
	// WarnImportFiltering, the designated signal that a deployment dropped the
	// web-download control. Reporting it after the fact would leave an audit
	// reading spec.importFiltering seeing the restrictive default while the
	// rendered config says otherwise.
	{Section: "import_filtering_opts", Key: "allowed_schemes", Rejected: true, OwnedBy: "spec.importFiltering", Impact: importFilteringOverrideImpact},
	{Section: "import_filtering_opts", Key: "disallowed_schemes", Rejected: true, OwnedBy: "spec.importFiltering", Impact: importFilteringOverrideImpact},
	{Section: "import_filtering_opts", Key: "allowed_hosts", Rejected: true, OwnedBy: "spec.importFiltering", Impact: importFilteringOverrideImpact},
	{Section: "import_filtering_opts", Key: "disallowed_hosts", Rejected: true, OwnedBy: "spec.importFiltering", Impact: importFilteringOverrideImpact},
	{Section: "import_filtering_opts", Key: "allowed_ports", Rejected: true, OwnedBy: "spec.importFiltering", Impact: importFilteringOverrideImpact},
	{Section: "import_filtering_opts", Key: "disallowed_ports", Rejected: true, OwnedBy: "spec.importFiltering", Impact: importFilteringOverrideImpact},

	// Image-import plugins — derived from spec.importPlugins. All four are
	// Rejected, for the reason the import_filtering_opts entries are: the typed
	// field expresses every one of them, so an extraConfig override buys no reach
	// it lacks, only a way past the guarantees it makes at admission. An
	// image_import_plugins list written by hand can order conversion ahead of
	// decompression, which converts the archive instead of the disk image inside
	// it; an output_format written by hand escapes the enum and fails on every
	// import instead of at admission; and an inject / ignore_user_roles written
	// by hand escapes the property-name rules that keep a pair inside the oslo
	// Dict syntax, so a stray colon or comma silently injects a property nobody
	// wrote. Reporting any of them after the fact would leave an audit reading
	// spec.importPlugins seeing one pipeline while the rendered config runs
	// another.
	{Section: "image_import_opts", Key: "image_import_plugins", Rejected: true, OwnedBy: "spec.importPlugins", Impact: importPluginsOverrideImpact},
	{Section: "image_conversion", Key: "output_format", Rejected: true, OwnedBy: "spec.importPlugins", Impact: importPluginsOverrideImpact},
	{Section: "inject_metadata_properties", Key: "inject", Rejected: true, OwnedBy: "spec.importPlugins", Impact: importPluginsOverrideImpact},
	{Section: "inject_metadata_properties", Key: "ignore_user_roles", Rejected: true, OwnedBy: "spec.importPlugins", Impact: importPluginsOverrideImpact},
}

// importFilteringOverrideImpact is the shared consequence note for the six
// Rejected [import_filtering_opts] keys, rendered into the admission error.
const importFilteringOverrideImpact = "the web-download URI filter is platform security policy; setting it " +
	"through extraConfig bypasses the exclusivity rules, the host INI guard, and the admission warning that " +
	"flags a loosened filter"

// importPluginsOverrideImpact is the shared consequence note for the four
// Rejected image-import plugin keys, rendered into the admission error.
const importPluginsOverrideImpact = "the image-import plugin pipeline is expressed by spec.importPlugins; " +
	"setting it through extraConfig bypasses the fixed decompression-before-conversion order, the " +
	"output-format enum, and the property-name rules that keep an injected pair inside the oslo Dict syntax"
