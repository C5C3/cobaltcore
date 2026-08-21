// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "github.com/c5c3/cobaltcore/internal/common/config"

// The owned-config-key registry below records the barbican.conf keys the
// operator computes and renders, so the post-merge health guard can reason about
// user overrides from one source of truth.
//
// Three rules govern what belongs here:
//
//   - The registry is static. Conditionally rendered keys — the
//     [keystone_authtoken] region_name / memcached_servers pair, which
//     keystoneauth.Section emits only for a CR that configures them, the
//     [vault_plugin] ssl_ca_crt_file / namespace pair, which a store carries only
//     in the modes that need them, and the [oslo_policy] policy_file, which is
//     injected only while spec.policyOverrides is set — are registered
//     unconditionally, because the registry documents "this key is not the
//     user's to set", not "this key is currently rendered".
//
//   - An entry is Reported (honored-but-surfaced through the ExtraConfigHealthy
//     condition) unless honoring the override would already have done the damage
//     by the time the condition surfaces it. The Rejected entries are the three
//     credential keys — [keystone_authtoken] password, [vault_plugin]
//     approle_secret_id and [vault_plugin] root_token_id — blocked by the
//     validating webhook at admission.
//
//   - Only fixed section names appear here. The per-store [secretstore:<name>]
//     sections take their names from the attached BarbicanSecretStore CRs, so
//     they cannot be spelled in a static registry; CatalogExemptSectionPrefixes
//     is what keeps the unknown-option scan off them.

// OwnedConfigKeys is the registry of barbican.conf keys the operator owns.
// Reported entries are honored-but-surfaced when overridden in spec.extraConfig;
// the Rejected entries ([keystone_authtoken] password and the two [vault_plugin]
// credential keys) are blocked at admission instead. The [keystone_authtoken]
// entries mirror the map keystoneauth.Section renders, and [oslo_policy]
// policy_file mirrors what config.InjectOsloPolicyConfig injects.
var OwnedConfigKeys = []config.OwnedKey{
	// [DEFAULT]
	//
	// db_auto_create is barbican's "create the schema from the models on API
	// start", and it defaults to true upstream. The operator renders it false and
	// migrates through the db-sync Job instead, so the schema has exactly one
	// writer and carries an alembic revision.
	//
	// host_href is the base URL barbican writes into the self-referencing links
	// of its API responses; the operator derives it from the same endpoint it
	// publishes in the Keystone catalog.
	{Section: "DEFAULT", Key: "db_auto_create", OwnedBy: "operator-computed", Impact: "re-enabling it lets every API pod create tables from the models on start, racing the db-sync Job and leaving a schema alembic never stamped"},
	{Section: "DEFAULT", Key: "host_href", OwnedBy: "operator-computed", Impact: "the links in API responses stop matching the published endpoint, so clients following them address a URL that need not resolve"},

	// [database] — barbican keeps its database options in the plain [database]
	// section its sibling services use.
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
	// password is never emitted by keystoneauth.Section — the middleware reads it
	// from the OS_KEYSTONE_AUTHTOKEN__PASSWORD env override, so a file value is
	// inert at runtime. It is Rejected: the validating webhook blocks an
	// extraConfig override at admission rather than merely reporting it, because
	// rendering it would copy the service password into the config Secret every
	// API pod mounts, before the ExtraConfigHealthy condition could surface it.
	{Section: "keystone_authtoken", Key: "password", Rejected: true, OwnedBy: "spec.serviceUser.secretRef", Impact: "the middleware password is env-injected via OS_KEYSTONE_AUTHTOKEN__PASSWORD; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

	// [secretstore] — the process-global store registry, derived from the
	// BarbicanSecretStore CRs attached to this Barbican. stores_lookup_suffix is
	// the list of suffixes that names the [secretstore:<suffix>] sections
	// barbican then reads, so an override of either key decides which of the
	// rendered per-store sections barbican looks at.
	{Section: "secretstore", Key: "enable_multiple_secret_stores", OwnedBy: "operator-computed", Impact: "the per-store sections are only read while multiple secret stores are enabled"},
	{Section: "secretstore", Key: "stores_lookup_suffix", OwnedBy: "operator-computed", Impact: "the suffix list selects which rendered [secretstore:<name>] sections barbican reads, so a hand-written list can drop an attached store or name one that was never rendered"},

	// [vault_plugin] — the process-global options of barbican's vault secret
	// store, derived from the attached BarbicanSecretStore. Every OpenBao store
	// on the same Barbican shares this one section, which is why the CRD offers
	// spec.openBao.extraOptions for the untyped rest of it.
	{Section: "vault_plugin", Key: "vault_url", OwnedBy: "operator-computed", Impact: "resolved from the store's instanceRef or spec.openBao.server.url; another URL addresses a server the store holds no credentials for"},
	{Section: "vault_plugin", Key: "use_ssl", OwnedBy: "operator-computed", Impact: "it follows the resolved server URL's scheme, so an override either disables verification or demands TLS the server does not speak"},
	{Section: "vault_plugin", Key: "ssl_ca_crt_file", OwnedBy: "operator-computed", Impact: "the operator mounts the store's CA bundle at this path; another path names a file the pod does not carry"},
	{Section: "vault_plugin", Key: "approle_role_id", OwnedBy: "operator-computed", Impact: "the value is read from the store's credentials Secret under role-id, which is the AppRole the mount's policy is bound to"},
	{Section: "vault_plugin", Key: "kv_mountpoint", OwnedBy: "spec.openBao.kvMountpoint of the attached BarbicanSecretStore", Impact: "a managed store's AppRole policy is scoped to the provisioned mount, so another mountpoint is a path the credentials cannot read or write"},
	{Section: "vault_plugin", Key: "namespace", OwnedBy: "spec.openBao.namespace of the attached BarbicanSecretStore", Impact: "the namespace scopes every request, so an override addresses a mount in a namespace the store was not provisioned in"},
	// approle_secret_id is never rendered into the file — it arrives through the
	// OS_VAULT_PLUGIN__APPROLE_SECRET_ID env override sourced from the store's
	// credentials Secret, so a file value is inert at runtime. It is Rejected for
	// the reason [keystone_authtoken] password is: rendering it would put the
	// AppRole secret into the config Secret every API pod mounts, before the
	// ExtraConfigHealthy condition could surface the override.
	{Section: "vault_plugin", Key: "approle_secret_id", Rejected: true, OwnedBy: "the store's credentials Secret", Impact: "the AppRole secret ID is env-injected via OS_VAULT_PLUGIN__APPROLE_SECRET_ID; a file override is ignored at runtime and copies credential material into the rendered config Secret"},
	// root_token_id is the vault plugin's alternative to AppRole authentication.
	// The operator never renders it, and it is registered here purely to close
	// that path: it is Rejected because a root token in the rendered file both
	// lands unbounded credentials in the config Secret every API pod mounts and
	// drops the mount-scoped policy the AppRole carries, and both are done the
	// moment the pods load the file.
	{Section: "vault_plugin", Key: "root_token_id", Rejected: true, OwnedBy: "operator-computed", Impact: "the operator authenticates the plugin through a mount-scoped AppRole; a root token replaces that with an unscoped credential written in plain text into the rendered config Secret"},

	// [queue] — barbican's asynchronous order processing. The operator renders
	// enable false: the queue only moves work to a barbican-worker process, and
	// this operator projects no worker Deployment.
	{Section: "queue", Key: "enable", OwnedBy: "operator-computed", Impact: "enabling the queue makes the API hand order processing to a worker that is not deployed, so orders stay pending instead of failing"},

	// [oslo_policy]
	{Section: "oslo_policy", Key: "policy_file", OwnedBy: "operator-computed"},
}
