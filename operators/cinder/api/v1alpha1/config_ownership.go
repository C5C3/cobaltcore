// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "github.com/c5c3/cobaltcore/internal/common/config"

// The owned-config-key registry below records the keys the operator computes and
// renders into cinder.conf, so the post-merge health guard can reason about user
// overrides from one source of truth.
//
// Two rules govern what belongs in it:
//
//   - The registry is static. A conditionally rendered key — the
//     [keystone_authtoken] region_name / memcached_servers pair, which
//     keystoneauth.Section emits only for a CR that configures them, the
//     [DEFAULT] default_log_levels / log_config_append pair, which the renderer
//     emits only for a CR that sets spec.logging.perLoggerLevels or selects the
//     json format, or the [barbican] keys the renderer emits only while
//     spec.keyManager is set — is registered unconditionally, because the
//     registry documents "this key is not the user's to set", not "this key is
//     currently rendered".
//
//   - An entry is Reported (honored-but-surfaced through the ExtraConfigHealthy
//     condition) unless honoring the override would already have done the damage
//     by the time the condition surfaces it. The Rejected entries are the
//     connection strings, the credential material, the switches that select a
//     security control, and the privsep escape hatches: rendering one copies a
//     credential into the config Secret every pod mounts, points a process at a
//     database the operator did not provision, takes the API off token
//     validation, or hands the privileged helper a command of the user's
//     choosing — and all of it is done the moment the pods load the file, while
//     ExtraConfigHealthy is informational and excluded from the Ready
//     aggregation.
//
// [DEFAULT] enabled_backends is deliberately absent: it is not rendered into
// cinder.conf at all. Each cinder-volume Deployment gets a per-backend overlay
// naming its single backend, so the key never appears in the file this registry
// governs.

// OwnedConfigKeys is the registry of cinder.conf keys the operator owns for the
// Cinder kind. Reported entries are honored-but-surfaced when overridden in
// spec.extraConfig; the Rejected entries are blocked at admission by the
// validating webhook instead. The [keystone_authtoken] and [service_user]
// entries mirror the maps keystoneauth.Section and keystoneauth.ServiceUserSection
// render, and the [oslo_messaging_rabbit] entries the map messaging.RabbitSection
// renders.
var OwnedConfigKeys = []config.OwnedKey{
	// [DEFAULT]
	//
	// auth_strategy names the WSGI pipeline api-paste.ini serves the API through,
	// and keystone is the only one of them that runs keystonemiddleware. It is
	// Rejected for the same reason api_paste_config below is: extraConfig has the
	// last word in the merge, so honoring auth_strategy = noauth selects a
	// pipeline without token validation — every request served unauthenticated,
	// and reachable from outside the cluster when spec.gateway is set. The damage
	// is done the moment the pods load the rendered file, long before the
	// ExtraConfigHealthy condition could report it.
	{Section: "DEFAULT", Key: "auth_strategy", Rejected: true, OwnedBy: "operator-computed", Impact: "an override puts the API on the selected auth middleware; anything but keystone disables token validation entirely"},
	// api_paste_config names the WSGI pipeline definition. The operator mounts
	// its own api-paste.ini, and the pipeline is what puts keystonemiddleware in
	// front of the API. It is Rejected rather than reported: a path that names a
	// file the pod does not carry fails the API on start, and a path that names
	// one it does carry can drop the auth filter, and both are done before the
	// ExtraConfigHealthy condition could surface the override.
	{Section: "DEFAULT", Key: "api_paste_config", Rejected: true, OwnedBy: "operator-computed", Impact: "the pipeline is what puts keystonemiddleware in front of the API, so another paste file can serve the API unauthenticated"},
	{Section: "DEFAULT", Key: "resource_query_filters_file", OwnedBy: "operator-computed", Impact: "the operator mounts the filters file at this path; another path names a file the pod does not carry, and the API falls back to filtering nothing"},
	{Section: "DEFAULT", Key: "state_path", OwnedBy: "operator-computed", Impact: "the operator mounts a writable volume at this path; another path names a directory the container cannot write"},
	{Section: "DEFAULT", Key: "image_conversion_dir", OwnedBy: "operator-computed", Impact: "the operator mounts the scratch volume an image conversion needs at this path; another path either is not writable or is not the volume the conversion was sized against"},
	// host is the identity a cinder-volume and the backup service register
	// under, and the volumes a backend owns are keyed by it. An override renames
	// the service, which leaves every volume already created pointing at a host
	// no process serves.
	{Section: "DEFAULT", Key: "host", OwnedBy: "operator-computed", Impact: "the volumes a backend owns are keyed by this identity, so renaming it strands them on a host no service backs"},
	{Section: "DEFAULT", Key: "glance_api_servers", OwnedBy: "spec.glanceEndpoint", Impact: "the address is what create-volume-from-image reads the image through; another one points the request at an image service this deployment does not own"},
	{Section: "DEFAULT", Key: "cinder_internal_tenant_project_id", OwnedBy: "spec.internalTenant.projectID", Impact: "the ID owns the image-volume cache's volumes; another one creates them under a project the deployment does not control"},
	{Section: "DEFAULT", Key: "cinder_internal_tenant_user_id", OwnedBy: "spec.internalTenant.userID", Impact: "the ID owns the image-volume cache's volumes; another one creates them under a user the deployment does not control"},
	{Section: "DEFAULT", Key: "use_stderr", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "debug", OwnedBy: "spec.logging.debug"},
	{Section: "DEFAULT", Key: "default_log_levels", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "log_config_append", OwnedBy: "operator-computed"},
	// transport_url is never emitted into the file — it arrives through the
	// OS_DEFAULT__TRANSPORT_URL env override sourced from the transport-URL
	// Secret, so a file value is inert at runtime. It is Rejected because
	// rendering it would copy the broker credentials into the config Secret every
	// pod mounts.
	{Section: "DEFAULT", Key: "transport_url", Rejected: true, OwnedBy: "spec.messaging", Impact: "the transport URL is env-injected via OS_DEFAULT__TRANSPORT_URL; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

	// [database]
	//
	// connection carries the database password. It is env-injected via
	// OS_DATABASE__CONNECTION, so a file value is inert at runtime and only
	// achieves putting the credential into the rendered config Secret.
	{Section: "database", Key: "connection", Rejected: true, OwnedBy: "spec.database", Impact: "the runtime value comes from the OS_DATABASE__CONNECTION env override; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

	// [keystone_authtoken] — rendered by keystoneauth.Section.
	{Section: "keystone_authtoken", Key: "auth_type", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "auth_url", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "www_authenticate_uri", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "username", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "project_name", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "user_domain_name", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "project_domain_name", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "region_name", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "memcached_servers", OwnedBy: "operator-computed"},
	// password is never emitted by keystoneauth.Section — the middleware reads it
	// from the OS_KEYSTONE_AUTHTOKEN__PASSWORD env override, so a file value is
	// inert at runtime. It is Rejected for the same reason transport_url is.
	{Section: "keystone_authtoken", Key: "password", Rejected: true, OwnedBy: "spec.serviceUser.secretRef", Impact: "the middleware password is env-injected via OS_KEYSTONE_AUTHTOKEN__PASSWORD; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

	// [service_user] — rendered by keystoneauth.ServiceUserSection. It is what
	// lets cinder present its own token alongside the user's when it calls
	// Glance, Nova or Barbican, so a request outlives the user token's expiry.
	{Section: "service_user", Key: "send_service_user_token", OwnedBy: "operator-computed"},
	{Section: "service_user", Key: "auth_type", OwnedBy: "operator-computed"},
	{Section: "service_user", Key: "auth_url", OwnedBy: "operator-computed"},
	{Section: "service_user", Key: "username", OwnedBy: "operator-computed"},
	{Section: "service_user", Key: "project_name", OwnedBy: "operator-computed"},
	{Section: "service_user", Key: "user_domain_name", OwnedBy: "operator-computed"},
	{Section: "service_user", Key: "project_domain_name", OwnedBy: "operator-computed"},
	{Section: "service_user", Key: "region_name", OwnedBy: "operator-computed"},
	// password is env-injected via OS_SERVICE_USER__PASSWORD, so a file value is
	// inert at runtime. Rejected for the same reason its keystone_authtoken twin
	// is.
	{Section: "service_user", Key: "password", Rejected: true, OwnedBy: "spec.serviceUser.secretRef", Impact: "the service-user password is env-injected via OS_SERVICE_USER__PASSWORD; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

	// [key_manager] / [barbican] — rendered while spec.keyManager is set.
	{Section: "key_manager", Key: "backend", OwnedBy: "spec.keyManager.type", Impact: "the backend is what castellan stores volume-encryption keys in; another one either has no keys the deployment wrote or is not configured at all, and every encrypted volume becomes unreadable"},
	{Section: "barbican", Key: "barbican_endpoint", OwnedBy: "spec.keyManager.barbican.endpoint", Impact: "the address is where the volume-encryption keys live; another one holds none of the keys this deployment's encrypted volumes were written with"},
	{Section: "barbican", Key: "barbican_endpoint_type", OwnedBy: "operator-computed", Impact: "the endpoint type picks which catalog entry castellan resolves, which the operator pins because it configures the endpoint explicitly"},
	{Section: "barbican", Key: "auth_endpoint", OwnedBy: "operator-computed", Impact: "castellan authenticates against this Keystone; another one issues tokens Barbican does not accept"},

	// [oslo_messaging_rabbit] — rendered by messaging.RabbitSection.
	{Section: "oslo_messaging_rabbit", Key: "rabbit_quorum_queue", OwnedBy: "operator-computed", Impact: "the queue type is fixed when the queue is declared, so a mismatch with the broker's existing queues fails the declaration"},
	{Section: "oslo_messaging_rabbit", Key: "rabbit_transient_quorum_queue", OwnedBy: "operator-computed", Impact: "the queue type is fixed when the queue is declared, so a mismatch with the broker's existing queues fails the declaration"},
	{Section: "oslo_messaging_rabbit", Key: "use_queue_manager", OwnedBy: "operator-computed"},
	{Section: "oslo_messaging_rabbit", Key: "ssl", OwnedBy: "spec.messaging.tls", Impact: "it follows the presence of the TLS block, so an override either drops the encryption or demands TLS the broker does not speak"},
	{Section: "oslo_messaging_rabbit", Key: "ssl_ca_file", OwnedBy: "spec.messaging.tls.caBundleSecretRef", Impact: "the operator mounts the CA bundle at this path; another path names a file the pod does not carry"},

	// [oslo_messaging_notifications]
	{Section: "oslo_messaging_notifications", Key: "driver", OwnedBy: "operator-computed", Impact: "the driver decides whether the volume lifecycle notifications are published at all"},

	// [oslo_concurrency]
	{Section: "oslo_concurrency", Key: "lock_path", OwnedBy: "operator-computed", Impact: "the operator mounts a writable volume at this path; another path names a directory the container cannot write"},

	// [coordination]
	//
	// backend_url is what the API, the schedulers and the volume services take
	// their distributed locks through. It points at the same Memcached
	// spec.cache names.
	{Section: "coordination", Key: "backend_url", OwnedBy: "spec.cache", Impact: "the lock backend is what keeps two processes off the same volume; another address puts them on separate lock namespaces, so the locks stop excluding each other"},

	// [cinder_sys_admin] / [privsep_osbrick] — the two privsep contexts the
	// volume and backup services run privileged operations through. Both keys are
	// Rejected: helper_command is the command line the unprivileged process
	// executes as root, so an override runs an attacker-chosen binary with the
	// context's privileges, and capabilities is the set that binary keeps. Both
	// take effect the moment the pods load the file.
	//
	// Neither section appears in the embedded option catalogs: oslo.privsep
	// registers a context's section at runtime, so the generator never sees it.
	{Section: "cinder_sys_admin", Key: "helper_command", Rejected: true, OwnedBy: "operator-computed", Impact: "the helper command is what the service runs as root; an override executes a binary of the submitter's choosing with the privsep context's privileges"},
	{Section: "cinder_sys_admin", Key: "capabilities", Rejected: true, OwnedBy: "operator-computed", Impact: "the capability set bounds what the privileged helper may do; widening it hands the helper privileges the pod's security context was written to withhold"},
	{Section: "privsep_osbrick", Key: "helper_command", Rejected: true, OwnedBy: "operator-computed", Impact: "the helper command is what os-brick runs as root; an override executes a binary of the submitter's choosing with the privsep context's privileges"},
	{Section: "privsep_osbrick", Key: "capabilities", Rejected: true, OwnedBy: "operator-computed", Impact: "the capability set bounds what the privileged helper may do; widening it hands the helper privileges the pod's security context was written to withhold"},

	// [oslo_policy]
	{Section: "oslo_policy", Key: "policy_file", OwnedBy: "spec.policyOverrides", Impact: "the operator mounts the rendered policy.yaml at this path; another path names a file the pod does not carry, and the API falls back to the built-in defaults"},
}
