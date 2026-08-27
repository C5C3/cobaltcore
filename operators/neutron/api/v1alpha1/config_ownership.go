// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "github.com/c5c3/cobaltcore/internal/common/config"

// The two owned-config-key registries below record the keys the operator
// computes and renders, one per kind, so the post-merge health guard can reason
// about user overrides from one source of truth.
//
// Two rules govern what belongs in either of them:
//
//   - The registry is static. A conditionally rendered key — the
//     [keystone_authtoken] region_name / memcached_servers pair, which
//     keystoneauth.Section emits only for a CR that configures them, or the
//     [DEFAULT] nova_metadata_* pair the agent renders only while
//     spec.novaMetadata is set — is registered unconditionally, because the
//     registry documents "this key is not the user's to set", not "this key is
//     currently rendered".
//
//   - An entry is Reported (honored-but-surfaced through the ExtraConfigHealthy
//     condition) unless honoring the override would already have done the damage
//     by the time the condition surfaces it. The Rejected entries are the
//     connection strings, the credential material and the switches that select a
//     security control: rendering one copies a credential into the config Secret
//     every pod mounts, points a process at a database the operator did not
//     provision, or takes the API off token validation or the ports off their
//     ACLs — and all of it is done the moment the pods load the file, while
//     ExtraConfigHealthy is informational and excluded from the Ready
//     aggregation.

// OwnedConfigKeys is the registry of neutron.conf and ml2_conf.ini keys the
// operator owns for the Neutron kind. Reported entries are honored-but-surfaced
// when overridden in spec.extraConfig; the Rejected entries are blocked at
// admission by the validating webhook instead. The [keystone_authtoken] entries
// mirror the map keystoneauth.Section renders.
var OwnedConfigKeys = []config.OwnedKey{
	// [DEFAULT]
	{Section: "DEFAULT", Key: "core_plugin", OwnedBy: "operator-computed", Impact: "the operator deploys the ML2 plugin; another core plugin loads a mechanism stack the rendered ml2 sections do not configure"},
	{Section: "DEFAULT", Key: "service_plugins", OwnedBy: "operator-computed", Impact: "the list carries the OVN router and the OVN-backed extensions; dropping one leaves the API serving an extension nothing implements"},
	// auth_strategy names the WSGI pipeline api-paste.ini serves the API through,
	// and keystone is the only one of them that runs keystonemiddleware. It is
	// Rejected for the same reason api_paste_config below is: extraConfig has the
	// last word in the merge, so honoring auth_strategy = noauth selects a
	// pipeline without token validation — every request served unauthenticated,
	// and reachable from outside the cluster when spec.gateway is set. The damage
	// is done the moment the pods load the rendered file, long before the
	// ExtraConfigHealthy condition could report it.
	{Section: "DEFAULT", Key: "auth_strategy", Rejected: true, OwnedBy: "operator-computed", Impact: "an override puts the API on the selected auth middleware; anything but keystone disables token validation entirely"},
	{Section: "DEFAULT", Key: "state_path", OwnedBy: "operator-computed", Impact: "the operator mounts a writable volume at this path; another path names a directory the container cannot write"},
	{Section: "DEFAULT", Key: "rpc_workers", OwnedBy: "spec.workers.deployment.replicas", Impact: "the worker count is derived from the worker Deployment, so a file override runs a different number of processes than the pod is sized for"},
	{Section: "DEFAULT", Key: "rpc_state_report_workers", OwnedBy: "operator-computed", Impact: "the state-report workers are sized against the same Deployment as rpc_workers"},
	{Section: "DEFAULT", Key: "dhcp_agent_notification", OwnedBy: "operator-computed", Impact: "OVN serves DHCP from the logical model and no DHCP agent is deployed, so enabling the notifications queues RPC casts nothing consumes"},
	{Section: "DEFAULT", Key: "notify_nova_on_port_status_changes", OwnedBy: "operator-computed", Impact: "the notification is what tells Nova a port is wired; disabling it leaves instances waiting for a vif-plugged event that never arrives"},
	{Section: "DEFAULT", Key: "notify_nova_on_port_data_changes", OwnedBy: "operator-computed", Impact: "the notification is what tells Nova a port is wired; disabling it leaves instances waiting for a vif-plugged event that never arrives"},
	{Section: "DEFAULT", Key: "debug", OwnedBy: "spec.logging.debug"},
	// api_paste_config names the WSGI pipeline definition. The operator mounts
	// its own api-paste.ini, and the pipeline is what puts keystonemiddleware in
	// front of the API. It is Rejected rather than reported: a path that names a
	// file the pod does not carry fails the API on start, and a path that names
	// one it does carry can drop the auth filter, and both are done before the
	// ExtraConfigHealthy condition could surface the override.
	{Section: "DEFAULT", Key: "api_paste_config", Rejected: true, OwnedBy: "operator-computed", Impact: "the pipeline is what puts keystonemiddleware in front of the API, so another paste file can serve the API unauthenticated"},
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
	{Section: "keystone_authtoken", Key: "project_domain_name", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "user_domain_name", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "region_name", OwnedBy: "operator-computed"},
	{Section: "keystone_authtoken", Key: "memcached_servers", OwnedBy: "operator-computed"},
	// password is never emitted by keystoneauth.Section — the middleware reads it
	// from the OS_KEYSTONE_AUTHTOKEN__PASSWORD env override, so a file value is
	// inert at runtime. It is Rejected for the same reason transport_url is.
	{Section: "keystone_authtoken", Key: "password", Rejected: true, OwnedBy: "spec.serviceUser.secretRef", Impact: "the middleware password is env-injected via OS_KEYSTONE_AUTHTOKEN__PASSWORD; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

	// [oslo_messaging_notifications] / [oslo_messaging_rabbit]
	{Section: "oslo_messaging_notifications", Key: "driver", OwnedBy: "operator-computed", Impact: "the driver decides whether the Nova port notifications are published at all"},
	{Section: "oslo_messaging_rabbit", Key: "rabbit_quorum_queue", OwnedBy: "operator-computed", Impact: "the queue type is fixed when the queue is declared, so a mismatch with the broker's existing queues fails the declaration"},
	{Section: "oslo_messaging_rabbit", Key: "rabbit_transient_quorum_queue", OwnedBy: "operator-computed", Impact: "the queue type is fixed when the queue is declared, so a mismatch with the broker's existing queues fails the declaration"},
	{Section: "oslo_messaging_rabbit", Key: "use_queue_manager", OwnedBy: "operator-computed"},
	{Section: "oslo_messaging_rabbit", Key: "ssl", OwnedBy: "spec.messaging.tls", Impact: "it follows the presence of the TLS block, so an override either drops the encryption or demands TLS the broker does not speak"},
	{Section: "oslo_messaging_rabbit", Key: "ssl_ca_file", OwnedBy: "spec.messaging.tls.caBundleSecretRef", Impact: "the operator mounts the CA bundle at this path; another path names a file the pod does not carry"},

	// [oslo_concurrency]
	{Section: "oslo_concurrency", Key: "lock_path", OwnedBy: "operator-computed", Impact: "the operator mounts a writable volume at this path; another path names a directory the container cannot write"},

	// [ml2] and the per-type-driver sections.
	{Section: "ml2", Key: "mechanism_drivers", OwnedBy: "operator-computed", Impact: "the operator deploys OVN alone; another driver list loads a mechanism whose agents are not deployed"},
	{Section: "ml2", Key: "type_drivers", OwnedBy: "operator-computed", Impact: "a type driver the OVN mechanism does not implement accepts networks it can never realize"},
	{Section: "ml2", Key: "tenant_network_types", OwnedBy: "operator-computed", Impact: "a tenant network type the OVN mechanism does not implement accepts networks it can never realize"},
	{Section: "ml2", Key: "extension_drivers", OwnedBy: "operator-computed"},
	{Section: "ml2_type_geneve", Key: "vni_ranges", OwnedBy: "operator-computed"},
	{Section: "ml2_type_geneve", Key: "max_header_size", OwnedBy: "operator-computed", Impact: "the header size has to match what the chassis encapsulation reserves, or the tunnelled frames exceed the path MTU"},
	{Section: "ml2_type_flat", Key: "flat_networks", OwnedBy: "operator-computed"},

	// [securitygroup]
	//
	// enable_security_group is what makes the ML2/OVN mechanism driver program
	// the ACLs a port's security groups describe. Rejected, not Reported:
	// disabling it stops the driver from writing them, so every newly bound
	// instance port is reachable from every other one and the rules the user
	// wrote are no longer enforced. That is done as soon as the pods load the
	// file, which is the criterion above.
	{Section: "securitygroup", Key: "enable_security_group", Rejected: true, OwnedBy: "operator-computed", Impact: "disabling it drops the port security the OVN ACLs implement, so every instance port becomes reachable from every other"},

	// [ovn] — the ML2/OVN mechanism driver's own section. The connection strings
	// and the client certificate paths are Rejected: they point the driver at a
	// database and identify it to that database, so an override either rewrites
	// another deployment's logical model or silently drops the authentication.
	{Section: "ovn", Key: "ovn_nb_connection", Rejected: true, OwnedBy: "spec.ovn.centralRef", Impact: "the connection string is resolved from the referenced OVNCentral; another address points the mechanism driver at a logical model it does not own"},
	{Section: "ovn", Key: "ovn_sb_connection", Rejected: true, OwnedBy: "spec.ovn.centralRef", Impact: "the connection string is resolved from the referenced OVNCentral; another address points the mechanism driver at a logical model it does not own"},
	{Section: "ovn", Key: "ovn_nb_private_key", Rejected: true, OwnedBy: "operator-computed", Impact: "the operator mounts the client keypair the OVNCentral issuer signed; another path names a file the pod does not carry, and the connection falls back to no client identity"},
	{Section: "ovn", Key: "ovn_nb_certificate", Rejected: true, OwnedBy: "operator-computed", Impact: "the operator mounts the client keypair the OVNCentral issuer signed; another path names a file the pod does not carry, and the connection falls back to no client identity"},
	{Section: "ovn", Key: "ovn_nb_ca_cert", Rejected: true, OwnedBy: "operator-computed", Impact: "the CA bundle is what verifies the database endpoint; another path either fails the handshake or trusts a server the operator did not provision"},
	{Section: "ovn", Key: "ovn_sb_private_key", Rejected: true, OwnedBy: "operator-computed", Impact: "the operator mounts the client keypair the OVNCentral issuer signed; another path names a file the pod does not carry, and the connection falls back to no client identity"},
	{Section: "ovn", Key: "ovn_sb_certificate", Rejected: true, OwnedBy: "operator-computed", Impact: "the operator mounts the client keypair the OVNCentral issuer signed; another path names a file the pod does not carry, and the connection falls back to no client identity"},
	{Section: "ovn", Key: "ovn_sb_ca_cert", Rejected: true, OwnedBy: "operator-computed", Impact: "the CA bundle is what verifies the database endpoint; another path either fails the handshake or trusts a server the operator did not provision"},
	{Section: "ovn", Key: "ovn_l3_scheduler", OwnedBy: "operator-computed"},
	{Section: "ovn", Key: "ovn_metadata_enabled", OwnedBy: "operator-computed", Impact: "it is what makes OVN serve 169.254.169.254 to the instances the NeutronMetadataAgent answers for"},
}

// MetadataAgentOwnedConfigKeys is the registry of
// neutron_ovn_metadata_agent.ini keys the operator owns for the
// NeutronMetadataAgent kind. It is a separate registry from OwnedConfigKeys
// because the two kinds render different files: the agent's [ovn] section
// carries only the Southbound half, and its [DEFAULT] carries the Nova metadata
// keys the API never writes.
var MetadataAgentOwnedConfigKeys = []config.OwnedKey{
	// [DEFAULT]
	{Section: "DEFAULT", Key: "state_path", OwnedBy: "operator-computed", Impact: "the operator mounts a writable volume at this path; another path names a directory the container cannot write"},
	{Section: "DEFAULT", Key: "debug", OwnedBy: "spec.logging.debug"},
	{Section: "DEFAULT", Key: "nova_metadata_host", OwnedBy: "spec.novaMetadata.host"},
	{Section: "DEFAULT", Key: "nova_metadata_port", OwnedBy: "spec.novaMetadata.port"},
	// metadata_proxy_shared_secret is env-injected from the referenced Secret, so
	// a file value is inert at runtime. It is Rejected because rendering it would
	// copy the secret the agent signs forwarded requests with into the config
	// Secret every agent pod mounts.
	{Section: "DEFAULT", Key: "metadata_proxy_shared_secret", Rejected: true, OwnedBy: "spec.novaMetadata.sharedSecretRef", Impact: "the shared secret is env-injected from the referenced Secret; a file override is ignored at runtime and copies credential material into the rendered config Secret"},

	// [ovs] / [ovn] — the two databases the agent reads.
	{Section: "ovs", Key: "ovsdb_connection", Rejected: true, OwnedBy: "spec.chassisRef", Impact: "the agent reads the local OVS database over the socket the chassis pods share; another address points it at a node whose ports it is not answering for"},
	{Section: "ovn", Key: "ovn_sb_connection", Rejected: true, OwnedBy: "spec.chassisRef", Impact: "the connection string is resolved from the OVNCentral the referenced chassis registers with; another address points the agent at a logical model it does not serve"},
	{Section: "ovn", Key: "ovn_sb_private_key", Rejected: true, OwnedBy: "operator-computed", Impact: "the operator mounts the client keypair the OVNCentral issuer signed; another path names a file the pod does not carry, and the connection falls back to no client identity"},
	{Section: "ovn", Key: "ovn_sb_certificate", Rejected: true, OwnedBy: "operator-computed", Impact: "the operator mounts the client keypair the OVNCentral issuer signed; another path names a file the pod does not carry, and the connection falls back to no client identity"},
	{Section: "ovn", Key: "ovn_sb_ca_cert", Rejected: true, OwnedBy: "operator-computed", Impact: "the CA bundle is what verifies the database endpoint; another path either fails the handshake or trusts a server the operator did not provision"},

	// [oslo_messaging_notifications] / [oslo_messaging_rabbit] — rendered only
	// while spec.messaging is set, registered unconditionally.
	{Section: "oslo_messaging_notifications", Key: "driver", OwnedBy: "operator-computed"},
	{Section: "oslo_messaging_rabbit", Key: "rabbit_quorum_queue", OwnedBy: "operator-computed", Impact: "the queue type is fixed when the queue is declared, so a mismatch with the broker's existing queues fails the declaration"},
	{Section: "oslo_messaging_rabbit", Key: "rabbit_transient_quorum_queue", OwnedBy: "operator-computed", Impact: "the queue type is fixed when the queue is declared, so a mismatch with the broker's existing queues fails the declaration"},
	{Section: "oslo_messaging_rabbit", Key: "use_queue_manager", OwnedBy: "operator-computed"},
	{Section: "oslo_messaging_rabbit", Key: "ssl", OwnedBy: "spec.messaging.tls", Impact: "it follows the presence of the TLS block, so an override either drops the encryption or demands TLS the broker does not speak"},
	{Section: "oslo_messaging_rabbit", Key: "ssl_ca_file", OwnedBy: "spec.messaging.tls.caBundleSecretRef", Impact: "the operator mounts the CA bundle at this path; another path names a file the pod does not carry"},

	// [oslo_concurrency]
	{Section: "oslo_concurrency", Key: "lock_path", OwnedBy: "operator-computed", Impact: "the operator mounts a writable volume at this path; another path names a directory the container cannot write"},
}
