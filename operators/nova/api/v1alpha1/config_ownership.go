// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "github.com/c5c3/cobaltcore/internal/common/config"

// The owned-config-key registry below records the keys the operator computes and
// renders into nova.conf, so the post-merge health guard can reason about user
// overrides from one source of truth.
//
// Two rules govern what belongs in it:
//
//   - The registry is static. A conditionally rendered key, such as the
//     [keystone_authtoken] region_name / memcached_servers pair, which
//     keystoneauth.Section emits only for a CR that configures them, the
//     [DEFAULT] default_log_levels / log_config_append pair, which the renderer
//     emits only for a CR that sets spec.logging.perLoggerLevels or selects the
//     json format, or the [cinder] and [barbican] keys the renderer emits only
//     while the matching spec.endpoints block is enabled, is registered
//     unconditionally, because the registry documents "this key is not the
//     user's to set", not "this key is currently rendered".
//
//   - An entry is Reported (honored-but-surfaced through the ExtraConfigHealthy
//     condition) unless honoring the override would already have done the damage
//     by the time the condition surfaces it. The Rejected entries are the
//     connection strings, the credential material, and the switches that select
//     a security control: rendering one copies a credential into the config
//     Secret every pod mounts, points a process at a database the operator did
//     not provision, takes the broker connection off TLS, or moves the console
//     proxy off the address its Service routes to. All of it is done the moment
//     the pods load the file, while ExtraConfigHealthy is informational and
//     excluded from the Ready aggregation.
//
// [api] auth_strategy is deliberately absent: nova 32.0.0 and 33.0.0 register no
// such option in either embedded catalog, so the operator neither renders it nor
// claims it. Keystone middleware is wired through the api-paste pipeline
// instead.

// OwnedConfigKeys is the registry of nova.conf keys the operator owns for the
// Nova kind. Reported entries are honored-but-surfaced when overridden in
// spec.extraConfig; the Rejected entries are blocked at admission by the
// validating webhook instead. The [keystone_authtoken] and [service_user]
// entries mirror the maps keystoneauth.Section and keystoneauth.ServiceUserSection
// render, the [placement], [neutron] and [cinder] credential keys the map
// keystoneauth.ClientSection renders, and the [oslo_messaging_rabbit] entries
// the map messaging.RabbitSection renders.
var OwnedConfigKeys = []config.OwnedKey{
	// [DEFAULT]
	{Section: "DEFAULT", Key: "use_stderr", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "debug", OwnedBy: "spec.logging.debug"},
	{Section: "DEFAULT", Key: "default_log_levels", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "log_config_append", OwnedBy: "operator-computed"},
	{Section: "DEFAULT", Key: "state_path", OwnedBy: "operator-computed", Impact: "the operator mounts a writable volume at this path; another path names a directory the container cannot write"},
	// web is the directory the console proxy serves the noVNC client from. It is
	// Rejected rather than reported: the operator mounts the client the image
	// ships at this path, and a path the pod does not carry leaves the browser
	// with a blank page and no error from the proxy itself.
	{Section: "DEFAULT", Key: "web", Rejected: true, OwnedBy: "operator-computed", Impact: "the console proxy serves the noVNC client from this directory; another path either is empty or is not the client the image ships"},
	// transport_url is never emitted into the file: it arrives through the
	// OS_DEFAULT__TRANSPORT_URL env override sourced from the transport-URL
	// Secret, so a file value is inert at runtime. It is Rejected because
	// rendering it would copy the broker credentials into the config Secret every
	// pod mounts.
	{Section: "DEFAULT", Key: "transport_url", Rejected: true, OwnedBy: "spec.messaging", Impact: "the transport URL is env-injected via OS_DEFAULT__TRANSPORT_URL; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

	// [api]
	//
	// local_metadata_per_cell selects whether the metadata API reaches every cell
	// or only its own. The operator runs one cell, and the metadata Deployment is
	// configured against the cell database accordingly.
	{Section: "api", Key: "local_metadata_per_cell", OwnedBy: "operator-computed", Impact: "the switch decides which cell databases the metadata API reads; flipping it points the API at cells this deployment did not configure it for"},

	// [database] / [api_database]
	//
	// Both connection strings carry a database password. They are env-injected
	// via OS_DATABASE__CONNECTION and OS_API_DATABASE__CONNECTION, so a file value
	// is inert at runtime and only achieves putting the credential into the
	// rendered config Secret.
	{Section: "database", Key: "connection", Rejected: true, OwnedBy: "spec.database", Impact: "the runtime value comes from the OS_DATABASE__CONNECTION env override; a file override is ignored at runtime and copies credential material into the rendered config Secret"},
	{Section: "api_database", Key: "connection", Rejected: true, OwnedBy: "spec.apiDatabase", Impact: "the runtime value comes from the OS_API_DATABASE__CONNECTION env override; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

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
	// password is never emitted by keystoneauth.Section: the middleware reads it
	// from the OS_KEYSTONE_AUTHTOKEN__PASSWORD env override, so a file value is
	// inert at runtime. It is Rejected for the same reason transport_url is.
	{Section: "keystone_authtoken", Key: "password", Rejected: true, OwnedBy: "spec.serviceUser.secretRef", Impact: "the middleware password is env-injected via OS_KEYSTONE_AUTHTOKEN__PASSWORD; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

	// [service_user] — rendered by keystoneauth.ServiceUserSection. It is what
	// lets nova present its own token alongside the user's when it calls
	// Placement, Neutron, Glance or Cinder, so a request outlives the user
	// token's expiry.
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

	// [placement] — the credentials rendered by keystoneauth.ClientSection plus
	// the two keys that address the service. Nova claims an allocation here
	// before every instance boots.
	{Section: "placement", Key: "auth_type", OwnedBy: "operator-computed"},
	{Section: "placement", Key: "auth_url", OwnedBy: "spec.keystoneEndpoint"},
	{Section: "placement", Key: "username", OwnedBy: "spec.serviceUser"},
	{Section: "placement", Key: "project_name", OwnedBy: "spec.serviceUser"},
	{Section: "placement", Key: "user_domain_name", OwnedBy: "spec.serviceUser"},
	{Section: "placement", Key: "project_domain_name", OwnedBy: "spec.serviceUser"},
	{Section: "placement", Key: "region_name", OwnedBy: "spec.region"},
	{Section: "placement", Key: "valid_interfaces", OwnedBy: "spec.endpoints.placement", Impact: "the interface decides which catalog entry nova resolves; the public one commonly names an address the pods cannot reach"},
	{Section: "placement", Key: "endpoint_override", OwnedBy: "spec.endpoints.placement.override", Impact: "the address is where the instance allocations are claimed; another one holds none of this deployment's inventories"},
	{Section: "placement", Key: "password", Rejected: true, OwnedBy: "spec.serviceUser.secretRef", Impact: "the client password is env-injected via OS_PLACEMENT__PASSWORD; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

	// [neutron] — the client credentials plus the metadata-proxy pair. Nova
	// creates and binds a port for every instance through this section.
	{Section: "neutron", Key: "auth_type", OwnedBy: "operator-computed"},
	{Section: "neutron", Key: "auth_url", OwnedBy: "spec.keystoneEndpoint"},
	{Section: "neutron", Key: "username", OwnedBy: "spec.serviceUser"},
	{Section: "neutron", Key: "project_name", OwnedBy: "spec.serviceUser"},
	{Section: "neutron", Key: "user_domain_name", OwnedBy: "spec.serviceUser"},
	{Section: "neutron", Key: "project_domain_name", OwnedBy: "spec.serviceUser"},
	{Section: "neutron", Key: "region_name", OwnedBy: "spec.region"},
	{Section: "neutron", Key: "valid_interfaces", OwnedBy: "spec.endpoints.neutron", Impact: "the interface decides which catalog entry nova resolves; the public one commonly names an address the pods cannot reach"},
	{Section: "neutron", Key: "endpoint_override", OwnedBy: "spec.endpoints.neutron.override", Impact: "the address is where the instance ports are created; another one serves a network this deployment does not own"},
	{Section: "neutron", Key: "service_metadata_proxy", OwnedBy: "operator-computed", Impact: "the switch is what makes the metadata API trust the proxied request headers, so turning it off leaves every instance without metadata"},
	{Section: "neutron", Key: "password", Rejected: true, OwnedBy: "spec.serviceUser.secretRef", Impact: "the client password is env-injected via OS_NEUTRON__PASSWORD; a file override is ignored at runtime and copies credential material into the rendered config Secret"},
	// metadata_proxy_shared_secret is what the metadata API verifies the proxied
	// request signature against. It is env-injected, so a file value is inert at
	// runtime and only copies the secret into the config Secret every pod mounts.
	{Section: "neutron", Key: "metadata_proxy_shared_secret", Rejected: true, OwnedBy: "spec.metadata.sharedSecretRef", Impact: "the shared secret is env-injected; a file override is ignored at runtime and copies the value the metadata signature is verified with into the rendered config Secret"},

	// [glance] — Nova reads the image of every instance it boots. The section
	// carries no credentials of its own: nova reaches Glance with the
	// [service_user] token.
	{Section: "glance", Key: "region_name", OwnedBy: "spec.region"},
	{Section: "glance", Key: "valid_interfaces", OwnedBy: "spec.endpoints.glance", Impact: "the interface decides which catalog entry nova resolves; the public one commonly names an address the pods cannot reach"},
	{Section: "glance", Key: "endpoint_override", OwnedBy: "spec.endpoints.glance.override", Impact: "the address is where the instance images are read; another one holds none of the images this deployment boots from"},

	// [cinder] — rendered while spec.endpoints.cinder is enabled. The section
	// addresses the volume service by catalog entry rather than by interface,
	// which is why its three addressing keys differ from the siblings above.
	{Section: "cinder", Key: "auth_type", OwnedBy: "operator-computed"},
	{Section: "cinder", Key: "auth_url", OwnedBy: "spec.keystoneEndpoint"},
	{Section: "cinder", Key: "username", OwnedBy: "spec.serviceUser"},
	{Section: "cinder", Key: "project_name", OwnedBy: "spec.serviceUser"},
	{Section: "cinder", Key: "user_domain_name", OwnedBy: "spec.serviceUser"},
	{Section: "cinder", Key: "project_domain_name", OwnedBy: "spec.serviceUser"},
	{Section: "cinder", Key: "os_region_name", OwnedBy: "spec.region"},
	{Section: "cinder", Key: "catalog_info", OwnedBy: "spec.endpoints.cinder", Impact: "the catalog tuple decides which entry nova resolves the volume service from; another one names a service type this deployment does not publish"},
	{Section: "cinder", Key: "endpoint_template", OwnedBy: "spec.endpoints.cinder.override", Impact: "the template is where the volume attachments are made; another one addresses a volume service this deployment does not own"},
	{Section: "cinder", Key: "password", Rejected: true, OwnedBy: "spec.serviceUser.secretRef", Impact: "the client password is env-injected via OS_CINDER__PASSWORD; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

	// [key_manager] / [barbican] — rendered while spec.endpoints.barbican is
	// enabled. It is what lets nova read the key of an encrypted volume.
	{Section: "key_manager", Key: "backend", OwnedBy: "spec.endpoints.barbican", Impact: "the backend is what castellan reads volume-encryption keys from; another one holds none of the keys this deployment's encrypted volumes were written with"},
	{Section: "barbican", Key: "auth_endpoint", OwnedBy: "spec.keystoneEndpoint", Impact: "castellan authenticates against this Keystone; another one issues tokens Barbican does not accept"},
	{Section: "barbican", Key: "barbican_endpoint", OwnedBy: "spec.endpoints.barbican.override", Impact: "the address is where the volume-encryption keys live; another one holds none of the keys this deployment's encrypted volumes were written with"},
	{Section: "barbican", Key: "barbican_endpoint_type", OwnedBy: "spec.endpoints.barbican", Impact: "the endpoint type picks which catalog entry castellan resolves, which the operator pins because it configures the endpoint explicitly"},
	{Section: "barbican", Key: "barbican_region_name", OwnedBy: "spec.region"},
	{Section: "barbican", Key: "send_service_user_token", OwnedBy: "operator-computed", Impact: "the switch is what lets a key read outlive the user token that started the request"},

	// [oslo_messaging_rabbit] — rendered by messaging.RabbitSection.
	{Section: "oslo_messaging_rabbit", Key: "rabbit_quorum_queue", OwnedBy: "operator-computed", Impact: "the queue type is fixed when the queue is declared, so a mismatch with the broker's existing queues fails the declaration"},
	{Section: "oslo_messaging_rabbit", Key: "rabbit_transient_quorum_queue", OwnedBy: "operator-computed", Impact: "the queue type is fixed when the queue is declared, so a mismatch with the broker's existing queues fails the declaration"},
	{Section: "oslo_messaging_rabbit", Key: "use_queue_manager", OwnedBy: "operator-computed"},
	// The TLS pair follows the presence of spec.messaging.tls. Both are Rejected:
	// the first selects whether the bus that carries every RPC call is encrypted,
	// and the second points the verification at a file the operator mounts.
	{Section: "oslo_messaging_rabbit", Key: "ssl", Rejected: true, OwnedBy: "spec.messaging.tls", Impact: "it follows the presence of the TLS block, so an override either drops the encryption on the bus that carries every RPC call or demands TLS the broker does not speak"},
	{Section: "oslo_messaging_rabbit", Key: "ssl_ca_file", Rejected: true, OwnedBy: "spec.messaging.tls.caBundleSecretRef", Impact: "the operator mounts the CA bundle at this path; another path names a file the pod does not carry, so the broker certificate is verified against a trust store nobody wrote"},

	// [oslo_messaging_notifications]
	{Section: "oslo_messaging_notifications", Key: "driver", OwnedBy: "operator-computed", Impact: "the driver decides whether the instance lifecycle notifications are published at all"},

	// [upgrade_levels]
	{Section: "upgrade_levels", Key: "compute", OwnedBy: "operator-computed", Impact: "auto caps the compute RPC version at the oldest nova-compute still registered; a pinned or absent cap sends messages the computes that upgrade after the control plane cannot read"},

	// [oslo_concurrency]
	{Section: "oslo_concurrency", Key: "lock_path", OwnedBy: "operator-computed", Impact: "the operator mounts a writable volume at this path; another path names a directory the container cannot write"},

	// [cache] — the oslo.cache block nova keeps its resolved cell mappings and
	// its Keystone token validations in.
	{Section: "cache", Key: "enabled", OwnedBy: "spec.cache", Impact: "with the cache off every request re-reads the cell mapping and re-validates the token, which the deployment was not sized for"},
	{Section: "cache", Key: "backend", OwnedBy: "spec.cache.backend"},
	{Section: "cache", Key: "memcache_servers", OwnedBy: "spec.cache", Impact: "the address is the memcached the operator provisioned; another one is a cache nothing else in the deployment writes"},

	// [scheduler] / [conductor]
	{Section: "scheduler", Key: "discover_hosts_in_cells_interval", OwnedBy: "operator-computed", Impact: "the interval is what maps a newly registered compute node into a cell; without it a new hypervisor stays invisible to scheduling"},
	{Section: "scheduler", Key: "workers", OwnedBy: "spec.scheduler.workers"},
	{Section: "conductor", Key: "workers", OwnedBy: "spec.conductor.workers"},

	// [vnc] — the console-proxy block. The base URL is what the API hands the
	// browser, and the listen pair is what the Service routes to.
	{Section: "vnc", Key: "enabled", OwnedBy: "spec.consoleProxy.enabled", Impact: "the switch is what makes a compute node offer a console at all, so an override either advertises consoles nothing serves or hides the ones that are running"},
	{Section: "vnc", Key: "novncproxy_base_url", OwnedBy: "spec.consoleProxy.gateway", Impact: "the URL is handed to the browser as the console address; another one sends the WebSocket somewhere no proxy answers"},
	// The listen pair is what the console-proxy Service routes to. Both are
	// Rejected: a proxy bound to another address or port keeps its Deployment
	// Ready while every console session fails to connect.
	{Section: "vnc", Key: "novncproxy_host", Rejected: true, OwnedBy: "operator-computed", Impact: "the proxy Service routes to this address; another one leaves the Service with a port nothing listens on while the pod stays Ready"},
	{Section: "vnc", Key: "novncproxy_port", Rejected: true, OwnedBy: "operator-computed", Impact: "the proxy Service routes to this port; another one leaves the Service with a port nothing listens on while the pod stays Ready"},
}
