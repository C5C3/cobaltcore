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
//     default_log_levels / log_config_append, the [keystone_authtoken]
//     region_name / memcached_servers, and the [oslo_policy] policy_file — are
//     registered unconditionally, because the registry documents "this key is
//     not the user's to set", not "this key is currently rendered". A key the
//     operator owns whenever it renders it stays owned even in the CRs where
//     that render is skipped.
//
//   - An entry is Reported (honored-but-surfaced through the ExtraConfigHealthy
//     condition) unless honoring the override would already have done the damage
//     by the time the condition surfaces it. Those are Rejected, and the
//     validating webhook blocks them at admission: [keystone_authtoken] password,
//     because the service password would land in the namespace-readable
//     ConfigMap, and the six [import_filtering_opts] keys, because a silently
//     loosened web-download filter is the very thing spec.importFiltering exists
//     to make visible.

// OwnedConfigKeys is the registry of glance-api.conf keys the operator owns.
// Reported entries are honored-but-surfaced when overridden in spec.extraConfig;
// the Rejected entries ([keystone_authtoken] password and the six
// [import_filtering_opts] keys) are blocked at admission instead. The entries
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
}

// importFilteringOverrideImpact is the shared consequence note for the six
// Rejected [import_filtering_opts] keys, rendered into the admission error.
const importFilteringOverrideImpact = "the web-download URI filter is platform security policy; setting it " +
	"through extraConfig bypasses the exclusivity rules, the host INI guard, and the admission warning that " +
	"flags a loosened filter"
