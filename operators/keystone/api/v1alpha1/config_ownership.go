// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "github.com/c5c3/cobaltcore/internal/common/config"

// The owned-config-key registry below records the keystone.conf keys the
// operator computes and renders, so the post-merge health guard and the
// validating webhook can reason about user overrides from one source of truth.
//
// Two rules govern what belongs here:
//
//   - The registry is static. Conditionally rendered keys — the federation
//     sections, the [identity] domain-projection keys, the json-logging
//     [DEFAULT] log_config_append — are registered unconditionally, because the
//     registry documents "this key is not the user's to set", not "this key is
//     currently rendered". A key the operator owns whenever it renders it stays
//     owned even in the CRs where that render is skipped.
//
//   - The sanctioned-override exception [cache] enabled is deliberately NOT
//     registered. docs/reference/keystone/keystone-crd.md documents overriding
//     it via spec.extraConfig as the supported way to disable keystone-side
//     caching, so a user override of it must stay silent rather than be
//     reported or rejected.

// OwnedConfigKeys is the registry of keystone.conf keys the operator owns.
// Reported entries are honored-but-surfaced when overridden in
// spec.extraConfig; the single Rejected entry is blocked at admission. The
// entries mirror the defaults map built in reconcileConfig plus the oslo_policy
// policy_file injected by config.InjectOsloPolicyConfig.
var OwnedConfigKeys = []config.OwnedKey{
	// [DEFAULT]
	{Section: "DEFAULT", Key: "keystone_user", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "keystone_group", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "use_stderr", OwnedBy: "operator-computed", Impact: "container logs will no longer reach kubectl logs"},
	{Section: "DEFAULT", Key: "debug", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "default_log_levels", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "log_config_append", OwnedBy: "operator-computed"},

	// [token]
	{Section: "token", Key: "provider", OwnedBy: "operator-computed", Impact: "fernet key projection and rotation assume the fernet provider"},

	// [fernet_tokens]
	{Section: "fernet_tokens", Key: "key_repository", OwnedBy: "operator-computed"},
	{Section: "fernet_tokens", Key: "max_active_keys", OwnedBy: "operator-computed", Impact: "the operator projects this many fernet keys into the pod from spec.fernet.maxActiveKeys; the override desynchronizes the rendered config from the mounted key material"},

	// [credential]
	{Section: "credential", Key: "key_repository", OwnedBy: "operator-computed"},
	{Section: "credential", Key: "max_active_keys", OwnedBy: "operator-computed"},

	// [cache]
	{Section: "cache", Key: "backend", OwnedBy: "operator-computed"},
	{Section: "cache", Key: "memcache_servers", OwnedBy: "operator-computed"},

	// [memcache]
	{Section: "memcache", Key: "servers", OwnedBy: "operator-computed"},

	// [paste_deploy]
	{Section: "paste_deploy", Key: "config_file", OwnedBy: "operator-computed"},

	// [oslo_middleware]
	{Section: "oslo_middleware", Key: "enable_proxy_headers_parsing", OwnedBy: "operator-computed"},

	// [oslo_policy]
	{Section: "oslo_policy", Key: "enforce_scope", OwnedBy: "operator-computed"},
	{Section: "oslo_policy", Key: "enforce_new_defaults", OwnedBy: "operator-computed"},
	{Section: "oslo_policy", Key: "policy_file", OwnedBy: "operator-computed"},

	// [identity]
	{Section: "identity", Key: "default_domain_id", OwnedBy: "operator-computed"},
	{Section: "identity", Key: "domain_specific_drivers_enabled", OwnedBy: "operator-computed"},
	{Section: "identity", Key: "domain_config_dir", OwnedBy: "operator-computed"},

	// [database]
	{Section: "database", Key: "max_retries", OwnedBy: "operator-computed"},
	{Section: "database", Key: "connection_recycle_time", OwnedBy: "operator-computed"},
	{Section: "database", Key: "connection", OwnedBy: "operator-computed", Impact: "the runtime value comes from the OS_DATABASE__CONNECTION env override, so the file override is ignored"},

	// [auth]
	{Section: "auth", Key: "methods", OwnedBy: "operator-computed", Impact: "oslo replaces the full default list, so dropping an entry disables that auth method"},

	// [federation]
	{Section: "federation", Key: "sso_callback_template", OwnedBy: "operator-computed"},

	// [openid]
	{Section: "openid", Key: "remote_id_attribute", OwnedBy: "operator-computed"},

	// The sole Rejected entry: trusted_dashboard is owned by the typed
	// spec.federation.trustedDashboards list, so the webhook blocks a colliding
	// extraConfig override rather than merely reporting it.
	{Section: "federation", Key: "trusted_dashboard", Rejected: true, OwnedBy: "spec.federation.trustedDashboards"},
}
