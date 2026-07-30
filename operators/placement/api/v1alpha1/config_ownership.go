// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "github.com/c5c3/forge/internal/common/config"

// The owned-config-key registry below records the placement.conf keys the
// operator computes and renders, so the post-merge health guard can reason about
// user overrides from one source of truth.
//
// Two rules govern what belongs here:
//
//   - The registry is static. Conditionally rendered keys — the
//     [keystone_authtoken] region_name / memcached_servers pair, which
//     keystoneauth.Section emits only for a CR that configures them, the
//     [DEFAULT] default_log_levels / log_config_append pair, which the renderer
//     emits only for a CR that sets spec.logging.perLoggerLevels or selects the
//     json format, and the [oslo_policy] policy_file, which is injected only
//     while spec.policyOverrides is set — are registered unconditionally,
//     because the registry documents "this key is not the user's to set", not
//     "this key is currently rendered".
//
//   - An entry is Reported (honored-but-surfaced through the ExtraConfigHealthy
//     condition) unless honoring the override would already have done the damage
//     by the time the condition surfaces it. The Rejected entries are
//     [keystone_authtoken] password and auth_strategy in both of its spellings,
//     blocked by the validating webhook at admission.

// OwnedConfigKeys is the registry of placement.conf keys the operator owns.
// Reported entries are honored-but-surfaced when overridden in spec.extraConfig;
// the Rejected entries ([keystone_authtoken] password and both auth_strategy
// spellings) are blocked at admission instead. The [keystone_authtoken] entries
// mirror the map keystoneauth.Section renders, and [oslo_policy] policy_file
// mirrors what config.InjectOsloPolicyConfig injects.
var OwnedConfigKeys = []config.OwnedKey{
	// [DEFAULT]
	{Section: "DEFAULT", Key: "use_stderr", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "debug", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "default_log_levels", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "log_config_append", OwnedBy: "operator-computed"},

	// [api] — placement reads auth_strategy from its own [api] section rather
	// than from [DEFAULT], where the option is deprecated.
	//
	// Rejected, not Reported: extraConfig has the last word in the merge, so
	// honoring auth_strategy = noauth2 would put the API on the no-auth
	// middleware — every request unauthenticated, with project and role taken
	// from the x-auth-token header, and reachable from outside the cluster when
	// spec.gateway is set. The damage is done the moment the pods load the
	// rendered file, long before the ExtraConfigHealthy condition could report it.
	//
	// [DEFAULT] auth_strategy is placement's deprecated alias for the same option
	// (see the catalog's DEFAULT.deprecated map). oslo.config honors it, and both
	// guards key on the exact (section, key) pair, so the [api] entry alone would
	// leave that spelling open. It is registered here purely to close that path —
	// the operator never renders it.
	{Section: "api", Key: "auth_strategy", Rejected: true, OwnedBy: "operator-computed", Impact: "an override puts the API on the selected auth middleware; noauth2 disables token validation entirely and takes project and role from the x-auth-token header"},
	{Section: "DEFAULT", Key: "auth_strategy", Rejected: true, OwnedBy: "operator-computed", Impact: "the deprecated alias of [api] auth_strategy, still honored by oslo.config, so it disables token validation the same way"},

	// [placement_database] — placement keeps its database options here, not in
	// the [database] section its sibling services use.
	{Section: "placement_database", Key: "connection", OwnedBy: "operator-computed", Impact: "the runtime value comes from the OS_PLACEMENT_DATABASE__CONNECTION env override, so the file override is ignored"},
	{Section: "placement_database", Key: "max_retries", OwnedBy: "operator-computed"},
	{Section: "placement_database", Key: "connection_recycle_time", OwnedBy: "operator-computed"},

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
	// password is never emitted by keystoneauth.Section — the middleware reads it
	// from the OS_KEYSTONE_AUTHTOKEN__PASSWORD env override, so a file value is
	// inert at runtime. It is Rejected: the validating webhook blocks an
	// extraConfig override at admission rather than merely reporting it, because
	// rendering it would leak the service password into the namespace-readable
	// ConfigMap before the ExtraConfigHealthy condition could surface it.
	{Section: "keystone_authtoken", Key: "password", Rejected: true, OwnedBy: "spec.serviceUser.secretRef", Impact: "the middleware password is env-injected via OS_KEYSTONE_AUTHTOKEN__PASSWORD; a file override is ignored at runtime and leaks credential material into a namespace-readable ConfigMap"},

	// [oslo_policy]
	{Section: "oslo_policy", Key: "policy_file", OwnedBy: "operator-computed"},
}
