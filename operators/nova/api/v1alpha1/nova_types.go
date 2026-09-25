// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=novas
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Release",type="string",JSONPath=".status.installedRelease"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Nova is the Schema for the novas API. It deploys the Nova compute control
// plane: the API server under uWSGI, the metadata API, the scheduler, the
// conductor, the noVNC console proxy, and the database, cache, message-bus and
// Keystone integrations.
//
// The compute nodes themselves are not part of this CR. A nova-compute process
// runs on the hypervisor, outside this cluster's control, and registers itself
// over the message bus. What this CR publishes for it is the compute contract:
// the rendered nova.conf fragment and the bus credentials, named by
// status.computeConfigSecretRef.
type Nova struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NovaSpec   `json:"spec,omitempty"`
	Status NovaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NovaList contains a list of Nova.
type NovaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Nova `json:"items"`
}

// NovaSpec defines the desired state of Nova.
//
// The targetClusterRef transition rules (evaluated only on UPDATE) freeze the
// ref: adding it, removing it, or renaming it after creation is rejected at the
// schema layer, so the guarantee holds even when the validating webhook is
// down. Moving a service between clusters is not a supported mutation.
//
// The three database rules keep the pair of schemas apart. Nova splits its state
// across two databases: spec.apiDatabase holds the global tables (cells, flavors,
// instance mappings) and spec.database holds the per-cell instance state. Both
// run their own db-sync against a different set of migrations, so pointing them
// at one schema installs both migration histories into it, and the cell block
// also owns cell0, the "<database>_cell0" schema the API block must not name
// either. They also share a credential path, so a deployment cannot issue one of
// them dynamic credentials and the other a static password.
//
// The console rule pairs the proxy Deployment with the switch that projects it.
// A disabled proxy has no Deployment to size, and spec.consoleProxy.deployment
// is a pointer so that an absent block stays absent: a value block would
// serialize as an empty object on every CR and make the rule fire on a
// deployment nobody wrote. For the same reason the defaulting webhook removes
// the block it materialized once the proxy is switched off, so the rule only
// ever meets a block written past that webhook.
//
// The remote-compute rule ties the second compute contract to a verified bus.
// A compute on another cluster reaches the broker across a cluster boundary,
// with the broker credentials in the URL, and the messaging CA bundle is the
// only trust anchor either contract carries.
// +kubebuilder:validation:XValidation:rule="has(self.targetClusterRef) == has(oldSelf.targetClusterRef)",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.targetClusterRef) || !has(oldSelf.targetClusterRef) || self.targetClusterRef.name == oldSelf.targetClusterRef.name",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.apiDatabase.database != self.database.database",message="apiDatabase and database must name different schemas"
// +kubebuilder:validation:XValidation:rule="self.apiDatabase.database != self.database.database + '_cell0'",message="apiDatabase must not name the cell0 schema derived from database"
// +kubebuilder:validation:XValidation:rule="(has(self.apiDatabase.credentialsMode) ? self.apiDatabase.credentialsMode : 'Static') == (has(self.database.credentialsMode) ? self.database.credentialsMode : 'Static')",message="apiDatabase and database must use the same credentialsMode"
// +kubebuilder:validation:XValidation:rule="!has(self.consoleProxy) || !has(self.consoleProxy.enabled) || self.consoleProxy.enabled || !has(self.consoleProxy.deployment)",message="consoleProxy.deployment must not be set when consoleProxy.enabled is false"
// +kubebuilder:validation:XValidation:rule="!has(self.remoteCompute) || has(self.messaging.tls)",message="remoteCompute requires messaging.tls: a compute on another cluster verifies the broker against the messaging CA bundle"
type NovaSpec struct {
	// OpenStackRelease names the OpenStack release this operator deploys and
	// drives. It governs install/upgrade release tracking:
	// status.installedRelease is promoted to this value after a successful
	// db-sync. It is deliberately kept separate from the image tag so
	// digest-pinned images keep working: pinning spec.image.digest disables
	// tag-based release tracking, but this field still tells the operator which
	// schema to converge to.
	//
	// The pattern matches the OpenStack date-based release scheme (YYYY.N where N
	// is 1 or 2, the two-releases-per-year cadence, e.g. 2025.2, 2026.1). The
	// [12] minor class keeps this CRD pattern, the validating webhook, and
	// release.ParseRelease in agreement so a non-cadence minor (e.g. 2025.9) is
	// rejected at every layer.
	// +kubebuilder:validation:Pattern=`^\d{4}\.[12]$`
	OpenStackRelease string `json:"openStackRelease"`

	// Image defines the Nova container image reference. Like the sibling
	// operators, the field carries no immutability rule: image upgrades are
	// routine. Every process this operator projects (API, metadata, scheduler,
	// conductor, console proxy, and the db-sync, cell-setup and archive Jobs)
	// runs this one image.
	Image commonv1.ImageSpec `json:"image"`

	// APIDatabase defines the MariaDB connection parameters for the nova_api
	// schema, the global half of Nova's state: the cell map, the flavors, the
	// instance-to-cell mappings, and the API-level quotas. The operator names
	// its managed database instance "<name>-api", so the two schemas of one
	// Nova never collide on a shared MariaDB.
	//
	// It must name a different schema than spec.database and its cell0, and use
	// the same credentialsMode (the rules on this spec). The clusterRef/host
	// mutual-exclusivity rule and the credentialsMode (Static/Dynamic) contract
	// are inherited from commonv1.DatabaseSpec.
	//
	// The schema name and the connection mode are immutable after create. The
	// schema holds the cell map, every instance mapping and every host mapping,
	// so renaming it re-points the whole control plane at a fresh, empty
	// nova_api while that state is orphaned in the old one; flipping the managed
	// (clusterRef) and brownfield (host) mode re-targets the connection the same
	// way. The transition rules below are enforced by the API server, so they
	// hold even when the validating webhook is unavailable.
	// +kubebuilder:validation:XValidation:rule="self.database == oldSelf.database",message="apiDatabase.database is immutable: the schema holds the cell map and every instance mapping"
	// +kubebuilder:validation:XValidation:rule="has(self.clusterRef) == has(oldSelf.clusterRef)",message="apiDatabase mode (managed clusterRef vs brownfield host) is immutable"
	APIDatabase commonv1.DatabaseSpec `json:"apiDatabase"`

	// Database defines the MariaDB connection parameters for the cell schema,
	// the per-cell half of Nova's state: the instances, their migrations and
	// their block-device mappings. The operator names its managed database
	// instance "<name>".
	//
	// The schema's user is granted on "<database>_cell0" as well. cell0 is the
	// holding pen for an instance that failed scheduling and therefore never
	// reached a real cell, and nova-api reads it on every instance list, so a
	// user without it turns a routine list into an error.
	//
	// It must name a different schema than spec.apiDatabase and use the same
	// credentialsMode (the rules on this spec). The name is bounded at 58
	// characters so "<database>_cell0" still fits the 64-character schema limit.
	//
	// The schema name and the connection mode are immutable after create. The
	// cell mappings in nova_api store the schema name literally, so a renamed
	// schema would be migrated and read by the conductor while the API kept
	// reading the old one through the unchanged mapping, splitting the instance
	// records across two schemas. The transition rules below are enforced by the
	// API server, so they hold even when the validating webhook is unavailable.
	// +kubebuilder:validation:XValidation:rule="self.database == oldSelf.database",message="database.database is immutable: the cell mappings store the schema name"
	// +kubebuilder:validation:XValidation:rule="has(self.clusterRef) == has(oldSelf.clusterRef)",message="database mode (managed clusterRef vs brownfield host) is immutable"
	// +kubebuilder:validation:XValidation:rule="size(self.database) <= 58",message="database.database must be at most 58 characters: cell0 is provisioned as <database>_cell0 under the 64-character schema limit"
	Database commonv1.DatabaseSpec `json:"database"`

	// Cache defines the Memcached cache configuration.
	// Supports managed (clusterRef) and brownfield (servers) modes; the
	// clusterRef/servers mutual-exclusivity rule lives on commonv1.CacheSpec.
	Cache commonv1.CacheSpec `json:"cache"`

	// Messaging defines the RabbitMQ connection Nova publishes its RPC calls and
	// notifications on. It is required rather than optional: the API hands every
	// instance request to the conductor over the bus, the conductor asks the
	// scheduler for a host over the bus, and every nova-compute reaches the
	// control plane the same way, so a Nova without a broker accepts requests
	// nothing acts on.
	Messaging commonv1.MessagingSpec `json:"messaging"`

	// API groups the knobs of the nova-api Deployment: the pod-level deployment
	// block and the uWSGI application-server parameters. It is the only one of
	// the five process blocks that scales on request rate, so spec.autoscaling
	// targets this Deployment alone.
	// +optional
	API NovaAPISpec `json:"api,omitempty"`

	// Metadata groups the knobs of the nova-metadata-api Deployment and the
	// shared secret it authenticates the proxied metadata requests with. It is
	// required because that secret has no default: the same value has to be
	// configured on the Neutron side, so the operator cannot invent one.
	Metadata NovaMetadataSpec `json:"metadata"`

	// Scheduler groups the knobs of the nova-scheduler Deployment.
	// +optional
	Scheduler NovaSchedulerSpec `json:"scheduler,omitempty"`

	// Conductor groups the knobs of the nova-conductor Deployment.
	// +optional
	Conductor NovaConductorSpec `json:"conductor,omitempty"`

	// ConsoleProxy groups the knobs of the nova-novncproxy Deployment and the
	// switch that projects it at all.
	// +optional
	ConsoleProxy NovaConsoleProxySpec `json:"consoleProxy,omitempty"`

	// Jobs sizes, prioritizes and places the pods of the db-sync Job (which
	// also runs the cell_v2 steps), the db-expand, db-migrate and db-contract
	// upgrade phases, and the db-archive CronJob. A field left unset falls
	// back to spec.api.deployment: the priority class, the node selector, the
	// tolerations, and the node affinity (never the pod (anti-)affinity). An
	// empty value opts out of the fallback. Unset resources default to a 100m
	// CPU request and 368Mi memory as request and limit.
	// +optional
	Jobs *commonv1.JobSpec `json:"jobs,omitempty"`

	// KeystoneEndpoint is the Keystone endpoint URL Nova authenticates against.
	// It renders as [keystone_authtoken] auth_url in nova.conf and as the
	// auth_url of every client section Nova calls other services through. Nova
	// connects to this URL server-side (token validation on every API request,
	// and a fresh token for every Placement, Neutron, Glance, Cinder and
	// Barbican call), so it must be reachable from the Nova pods. For a
	// colocated control plane that is the cluster-local Service URL, never an
	// externally routable address that only resolves outside the cluster. A
	// plain URL field keeps the operator decoupled from the keystone-operator;
	// the c5c3 ControlPlane operator projects it from its Keystone child by
	// naming convention.
	//
	// It is required. Nova has no Keystone-free posture: an instance boot needs
	// a Placement allocation, a Neutron port and a Glance image, and all three
	// are authenticated calls.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystoneEndpoint string `json:"keystoneEndpoint"`

	// KeystonePublicEndpoint is the browser/client-facing Keystone base URL Nova
	// advertises as [keystone_authtoken] www_authenticate_uri, the address a 401
	// response points unauthenticated clients at. Optional: when empty the
	// operator falls back to KeystoneEndpoint (see
	// EffectiveKeystonePublicEndpoint), which is correct only when the internal
	// and public Keystone URLs coincide.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystonePublicEndpoint string `json:"keystonePublicEndpoint,omitempty"`

	// RemoteCompute publishes a second compute contract, the
	// <name>-remote-compute-config Secret, for a nova-compute on another
	// cluster. It carries the same keys as <name>-compute-config, with the
	// addresses a compute outside this cluster can reach: every auth_url is
	// remoteCompute.keystoneEndpoint, every client section resolves its service
	// through the public catalog row, and transport_url is the broker's external
	// listener. The in-cluster contract keeps its addresses, so a compute beside
	// this Nova keeps reading it. When nil (the default) no remote contract is
	// published, and one published earlier is deleted.
	//
	// It requires spec.messaging.tls (the rule on this spec): the remote compute
	// verifies the broker against the same CA bundle the in-cluster contract
	// carries.
	// +optional
	RemoteCompute *NovaRemoteComputeSpec `json:"remoteCompute,omitempty"`

	// ServiceUser identifies the Keystone service account Nova authenticates as
	// and the Secret holding its password. It is required, like
	// spec.keystoneEndpoint, and the same account is used for token validation
	// and for every outgoing service call.
	ServiceUser ServiceUserSpec `json:"serviceUser"`

	// Region is the Keystone region Nova authenticates against
	// ([keystone_authtoken] region_name and the region of every client section).
	// Optional: when empty the option is omitted and Nova uses the Keystone
	// catalog's default region.
	// +optional
	Region string `json:"region,omitempty"`

	// Endpoints pins the services Nova calls as a client, and switches the two
	// optional ones on. Placement, Neutron and Glance are mandatory siblings of
	// a working compute control plane and are always configured; Cinder and
	// Barbican are opt-in, so a standalone Nova with neither block runs without
	// volume attachment and without encrypted-volume support.
	// +optional
	Endpoints NovaEndpointsSpec `json:"endpoints,omitempty"`

	// DBArchive tunes the recurring database archive that moves Nova's
	// soft-deleted rows into the shadow tables. A nil block resolves exactly
	// like an empty struct: the archive runs on every Nova, since an unbounded
	// soft-delete backlog is a deferred outage rather than a posture worth
	// offering.
	// +optional
	DBArchive *DBArchiveSpec `json:"dbArchive,omitempty"`

	// Gateway configures external exposure of the Nova API via a Gateway API
	// HTTPRoute. When set, the operator creates an HTTPRoute targeting the {name}
	// Service and attaches it to the referenced pre-existing Gateway. When removed
	// (nil), the HTTPRoute is deleted. The Gateway and GatewayClass are
	// infrastructure concerns managed outside this operator.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`

	// NetworkPolicy configures network isolation for Nova pods.
	// When set, a NetworkPolicy is created restricting ingress and egress traffic.
	// When removed (nil), the NetworkPolicy is deleted and traffic flows
	// unrestricted.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`

	// Autoscaling configures horizontal pod autoscaling for the Nova API
	// deployment. When set, a HorizontalPodAutoscaler is created targeting the
	// deployment. When removed, the HPA is deleted. It reaches the API
	// Deployment alone: the metadata API is sized by the instance population
	// rather than by API traffic, and the scheduler, conductor and console proxy
	// are not sized by request rate at all.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Logging configures oslo.log output for the Nova containers.
	// When unset, the defaulting webhook materializes a LoggingSpec with
	// Format=text, Level=INFO, Debug=false.
	// +optional
	Logging *LoggingSpec `json:"logging,omitempty"`

	// ExtraConfig provides free-form INI sections for configuration not covered by
	// explicit CRD fields. It is the deliberate escape hatch for options that have
	// no dedicated knob of their own.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`

	// SecretStoreRef selects the External Secrets store the operator routes this
	// Nova's ExternalSecrets and PushSecrets through. When omitted the operator
	// uses the shared cluster-scoped openbao-cluster-store, so existing
	// deployments keep working unchanged. Set kind to SecretStore with the name of
	// a namespaced store in THIS Nova's namespace to reach OpenBao as a
	// per-tenant identity. The ControlPlane operator projects this field onto the
	// Nova it owns, so operators normally configure it there rather than here.
	// +optional
	SecretStoreRef *commonv1.SecretStoreRefSpec `json:"secretStoreRef,omitempty"`

	// TargetClusterRef names the registered target cluster that receives this
	// Nova's children: Deployments, ConfigMaps, Secrets, and the database
	// CRs. The CR itself stays on the management cluster, and so do its status
	// and its finalizers. When omitted, the children are created on the local
	// cluster (the management cluster the operator runs on) and the deployment
	// behaves exactly like a single-cluster one. The field is immutable (see the
	// transition rules on this spec).
	//
	// A child written to the target carries no owner reference, since nothing
	// on that cluster resolves one into the management cluster. Three labels
	// name the owner instead: openstack.c5c3.io/owner-kind,
	// openstack.c5c3.io/owner-name, and openstack.c5c3.io/owner-namespace.
	// Deleting the CR deletes the children that carry its labels, under a
	// finalizer the CR installs whenever the ref is set, so a target cluster
	// may have the service CRDs installed. A cluster deregistered past the
	// abandon window is the exception: its children cannot be reached and stay
	// behind on it.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`
}

// DeploymentSpec, AutoscalingSpec, NetworkPolicySpec,
// NetworkPolicyIngressSource, LoggingSpec, GatewaySpec, GatewayParentRefSpec,
// and UWSGISpec are aliased to the shared commonv1 definitions. commonv1
// carries the canonical per-field godoc and validation markers. The aliases
// keep call sites (novav1alpha1.DeploymentSpec and bare DeploymentSpec{}
// literals alike) consistent with the sibling operators.
type (
	DeploymentSpec             = commonv1.DeploymentSpec
	AutoscalingSpec            = commonv1.AutoscalingSpec
	NetworkPolicySpec          = commonv1.NetworkPolicySpec
	NetworkPolicyIngressSource = commonv1.NetworkPolicyIngressSource
	LoggingSpec                = commonv1.LoggingSpec
	GatewaySpec                = commonv1.GatewaySpec
	GatewayParentRefSpec       = commonv1.GatewayParentRefSpec
	UWSGISpec                  = commonv1.UWSGISpec
)

// NovaAPISpec groups the knobs of the nova-api Deployment.
type NovaAPISpec struct {
	// Deployment groups the pod-level knobs for the API Deployment (replicas,
	// resources, rollout strategy, graceful-termination timings, and scheduling
	// constraints).
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`

	// UWSGI configures the uWSGI application server the API runs under. When nil
	// the operator uses the shared uWSGI defaults.
	// +optional
	UWSGI *UWSGISpec `json:"uwsgi,omitempty"`
}

// NovaMetadataSpec groups the knobs of the nova-metadata-api Deployment, the
// process that answers an instance's calls to 169.254.169.254. The request does
// not arrive from the instance directly: the Neutron metadata agent proxies it
// and signs it with a shared secret, which is what tells this API which instance
// asked.
//
// Replicas resolve to one rather than the shared DeploymentSpec default of
// three, and may be raised: the metadata API holds no state between requests.
type NovaMetadataSpec struct {
	// Deployment groups the pod-level knobs for the metadata Deployment.
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`

	// UWSGI configures the uWSGI application server the metadata API runs
	// under. When nil the operator uses the shared uWSGI defaults.
	// +optional
	UWSGI *UWSGISpec `json:"uwsgi,omitempty"`

	// SharedSecretRef references the Secret holding the value the Neutron
	// metadata agent signs proxied requests with, rendered as [neutron]
	// metadata_proxy_shared_secret. The key is webhook-defaulted to
	// "shared_secret". The same value has to reach the NeutronMetadataAgent, so
	// the operator reads it rather than generating it: a value only this side
	// knows leaves every metadata request rejected.
	SharedSecretRef commonv1.SecretRefSpec `json:"sharedSecretRef"`

	// Gateway configures external exposure of the metadata API via a Gateway API
	// HTTPRoute on a hostname of its own. It is rarely wanted: the metadata API
	// is reached from inside the cluster by the Neutron metadata agent, not by a
	// user. When removed (nil), the HTTPRoute is deleted.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`
}

// NovaSchedulerSpec groups the knobs of the nova-scheduler Deployment. The
// scheduler picks a host for every instance the conductor asks it about, off the
// message bus rather than off HTTP, which is why it gets its own Deployment
// instead of sharing the API's.
//
// Replicas resolve to one rather than the shared DeploymentSpec default of
// three, and may be raised: schedulers are peers that read the same host state
// out of Placement and hold nothing between requests.
type NovaSchedulerSpec struct {
	// Deployment groups the pod-level knobs for the scheduler Deployment.
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`

	// Workers is the number of nova-scheduler worker processes per pod. The
	// defaulting webhook resolves two when unset, which keeps one scheduling
	// request from blocking the next inside a pod. Raising it multiplies the
	// database and bus connections the pod holds.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Workers *int32 `json:"workers,omitempty"`
}

// NovaConductorSpec groups the knobs of the nova-conductor Deployment. The
// conductor is the only process that talks to the cell database on behalf of a
// compute node, which is what keeps a compromised hypervisor away from the
// database credentials.
//
// Replicas resolve to one rather than the shared DeploymentSpec default of
// three, and may be raised: conductors are peers that hold nothing between
// requests.
type NovaConductorSpec struct {
	// Deployment groups the pod-level knobs for the conductor Deployment.
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`

	// Workers is the number of nova-conductor worker processes per pod. The
	// defaulting webhook resolves two when unset. Raising it multiplies the
	// database and bus connections the pod holds.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Workers *int32 `json:"workers,omitempty"`
}

// NovaConsoleProxySpec groups the knobs of the nova-novncproxy Deployment, the
// process that bridges a browser's noVNC session to the VNC server of the
// hypervisor an instance runs on.
//
// Deployment is a pointer while the sibling blocks carry a value. A
// zero-valued DeploymentSpec marshals as "deployment":{}, which the API server
// then fills with the schema default of three replicas, so a value field would
// put a deployment block on every CR including one that disables the proxy. The
// pointer keeps an absent block absent, which is what the console rule on
// NovaSpec measures.
type NovaConsoleProxySpec struct {
	// Enabled projects the console proxy. The defaulting webhook resolves true
	// when unset: a control plane whose consoles cannot be reached is a
	// deliberate choice rather than the baseline. Setting it to false deletes the
	// Deployment, its Service and its HTTPRoute, and the defaulting webhook
	// removes spec.consoleProxy.deployment in the same request (the rule on
	// NovaSpec forbids a block written past it).
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Deployment groups the pod-level knobs for the console-proxy Deployment.
	// The defaulting webhook materializes it at one replica while the proxy is
	// enabled, and removes it while it is disabled.
	// +optional
	Deployment *DeploymentSpec `json:"deployment,omitempty"`

	// Gateway configures external exposure of the console proxy via a Gateway
	// API HTTPRoute. It takes a hostname of its own rather than a path under the
	// API's: the noVNC client opens a WebSocket against the host the console URL
	// names, and that URL is handed to the browser by the API. When removed
	// (nil), the HTTPRoute is deleted.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`
}

// ServiceUserSpec identifies the Keystone service account Nova uses to validate
// tokens and call other services, and references the Secret holding its
// password. The name and domain fields are optional; the defaulting webhook
// materializes them (username nova, projectName service, userDomainName and
// projectDomainName Default), so a minimal CR need only supply the password
// Secret reference.
type ServiceUserSpec struct {
	// Username is the Keystone username Nova authenticates as
	// ([keystone_authtoken] username). Webhook-defaulted to "nova".
	// +optional
	Username string `json:"username,omitempty"`

	// ProjectName is the Keystone project the service user scopes to
	// ([keystone_authtoken] project_name). Webhook-defaulted to "service".
	// +optional
	ProjectName string `json:"projectName,omitempty"`

	// UserDomainName is the domain the service user lives in
	// ([keystone_authtoken] user_domain_name). Webhook-defaulted to "Default".
	// +optional
	UserDomainName string `json:"userDomainName,omitempty"`

	// ProjectDomainName is the domain the service project lives in
	// ([keystone_authtoken] project_domain_name). Webhook-defaulted to "Default".
	// +optional
	ProjectDomainName string `json:"projectDomainName,omitempty"`

	// SecretRef references the Secret holding the service user's password. The
	// key is webhook-defaulted to "password".
	SecretRef commonv1.SecretRefSpec `json:"secretRef"`
}

// NovaEndpointsSpec pins the services Nova calls as a client. Each block
// configures one client section of nova.conf.
//
// An empty override leaves Nova resolving the service from the Keystone catalog
// with valid_interfaces = internal, which is the right answer for a colocated
// control plane. An override is for the deployment whose catalog carries an
// address the Nova pods cannot reach, or none at all.
type NovaEndpointsSpec struct {
	// Placement configures the [placement] section. Nova claims every instance's
	// resources against Placement before it boots, so the service is mandatory
	// and the block only decides how it is addressed.
	// +optional
	Placement NovaEndpointSpec `json:"placement,omitempty"`

	// Neutron configures the [neutron] section. Nova creates and binds a port
	// for every instance, so the service is mandatory and the block only decides
	// how it is addressed.
	// +optional
	Neutron NovaEndpointSpec `json:"neutron,omitempty"`

	// Glance configures the [glance] section. Nova reads the image of every
	// instance it boots, so the service is mandatory and the block only decides
	// how it is addressed.
	// +optional
	Glance NovaEndpointSpec `json:"glance,omitempty"`

	// Cinder configures the [cinder] section, which is what attaches a volume to
	// an instance and what boots an instance from one. A standalone Nova
	// defaults it off: a deployment without block storage has no Cinder to name.
	// +optional
	Cinder NovaOptionalEndpointSpec `json:"cinder,omitempty"`

	// Barbican configures the [key_manager] and [barbican] sections, which is
	// what lets Nova read the key of an encrypted volume. A standalone Nova
	// defaults it off: a deployment without a key manager has no Barbican to
	// name.
	// +optional
	Barbican NovaOptionalEndpointSpec `json:"barbican,omitempty"`
}

// NovaEndpointSpec addresses a service Nova always calls.
type NovaEndpointSpec struct {
	// Override is the endpoint URL Nova calls the service at, bypassing the
	// Keystone catalog. Optional: when empty the client section carries
	// valid_interfaces = internal instead and Nova resolves the address from the
	// catalog on every call.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	Override string `json:"override,omitempty"`
}

// NovaOptionalEndpointSpec addresses a service Nova calls only when the
// deployment runs it.
type NovaOptionalEndpointSpec struct {
	// Enabled renders the client section at all. It is false by default, so a
	// standalone Nova runs without the integration rather than with a section
	// pointing at a service that is not there.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Override is the endpoint URL Nova calls the service at, bypassing the
	// Keystone catalog. Optional: when empty the client section carries
	// valid_interfaces = internal instead and Nova resolves the address from the
	// catalog on every call. It is read only while Enabled is true.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	Override string `json:"override,omitempty"`
}

// NovaRemoteComputeSpec names the two addresses the remote compute contract
// cannot derive from the rest of the spec: the Keystone URL a compute on
// another cluster authenticates against, and the broker listener it connects
// to.
type NovaRemoteComputeSpec struct {
	// KeystoneEndpoint is the Keystone v3 URL a compute on another cluster
	// authenticates against. It renders as every auth_url of the remote
	// fragment and as [barbican] auth_endpoint, so it must resolve from the
	// compute cluster's nodes. It must use https: the nova service-user
	// password is sent to it on every token request, across a cluster boundary.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https://`
	KeystoneEndpoint string `json:"keystoneEndpoint"`

	// TransportURLSecretRef names a Secret in this Nova's namespace, on the
	// cluster its children run on, holding the complete rabbit:// URL of the
	// broker's external listener. The operator reads it and never writes it,
	// and copies the value into the remote contract's transport_url. The
	// defaulting webhook materializes an empty key to "transport_url".
	TransportURLSecretRef commonv1.SecretRefSpec `json:"transportURLSecretRef"`
}

// DBArchiveSpec tunes the recurring database archive. Nova never hard-deletes on
// its own: deleting an instance only flips its row to deleted, so the tables
// grow for the lifetime of the deployment and every query that scans them gets
// slower. The operator projects a CronJob running "nova-manage db
// archive_deleted_rows", which moves a soft-deleted row out of the live table
// into its shadow twin.
//
// The archive is a move, not a purge: the rows survive in the shadow tables and
// stay available for accounting. Reclaiming that space is a separate
// "nova-manage db purge" the operator does not schedule.
//
// The operator resolves the knobs at reconcile time rather than materializing
// them into the CR, so a field left unset keeps tracking the operator defaults
// across upgrades instead of freezing today's values into the stored CR. A nil
// block therefore resolves exactly like an empty struct: 1000 rows per table per
// batch, swept daily, one second of sleep between batches.
type DBArchiveSpec struct {
	// Schedule is the standard cron expression the archive CronJob runs on. When
	// empty the operator resolves DefaultDBArchiveSchedule. The value is checked
	// by the validating webhook rather than by a CRD pattern: the accepted
	// grammar includes descriptors such as @daily, which no regex expresses
	// without also rejecting valid expressions.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// MaxRows bounds how many rows one batch moves per table, the --max_rows
	// argument. When unset the operator resolves DefaultDBArchiveMaxRows. The
	// bound is what keeps a batch on a long-neglected database from holding
	// table locks for the length of the backlog. A run repeats batches until
	// nothing is left to move or its time budget is spent, and the next run picks
	// up where it stopped.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxRows *int32 `json:"maxRows,omitempty"`

	// Sleep is how many seconds the run waits between batches. When unset the
	// operator resolves DefaultDBArchiveSleep. Zero
	// runs the batches back to back, which finishes sooner at the cost of the
	// database serving the API at the same time.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Sleep *int32 `json:"sleep,omitempty"`

	// RetentionDays keeps the most recent deletions out of the archive: when
	// set, the run passes --before with today's date minus this many days, so a
	// row soft-deleted inside the window stays in the live table. When unset no
	// --before is passed and every soft-deleted row is eligible.
	//
	// The window also gates the task_log table, the record of the instance usage
	// audit periods. Its rows are never soft-deleted, so archiving it without a
	// date moves the current audit period out from under the
	// os-instance_usage_audit_log API that still reports on it: the run passes
	// --task-log only together with --before.
	//
	// Lowering it, or dropping it after it was set, widens what the next run
	// moves. That is reversible (the rows are in the shadow tables) but it is
	// still a change of scope nobody asked for twice, so the validating webhook
	// warns on it.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RetentionDays *int32 `json:"retentionDays,omitempty"`

	// Suspend pauses the archive CronJob without deleting it. It is the escape
	// hatch for a brownfield deployment onboarding onto this operator: the first
	// run works through a backlog that has never been archived, so an operator
	// who wants to stage that can suspend the CronJob, pick a retention window
	// covering the deployment's full history, and step it down. The condition
	// stays True while suspended: a paused archive is a deliberate posture, not
	// a failure.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// NovaStatus defines the observed state of Nova.
type NovaStatus struct {
	// Conditions represent the latest available observations of the Nova state.
	// Each condition carries an ObservedGeneration so consumers can tell a stale
	// condition from one reflecting the current spec; use the conditions helper
	// (internal/common/conditions) to upsert them.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the .metadata.generation the controller last
	// reconciled, so a stale status is distinguishable from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Endpoint is the Nova API endpoint URL clients use.
	Endpoint string `json:"endpoint,omitempty"`

	// InstalledRelease is the OpenStack release whose database schema is currently
	// installed, promoted to spec.openStackRelease after a successful db-sync.
	InstalledRelease string `json:"installedRelease,omitempty"`

	// TargetRelease is the spec.openStackRelease being converged to during an
	// active upgrade.
	TargetRelease string `json:"targetRelease,omitempty"`

	// UpgradePhase is the current expand-migrate-contract phase during an active
	// release upgrade (Expanding, Migrating, RollingUpdate, Contracting); empty
	// when no upgrade is in flight.
	UpgradePhase commonv1.UpgradePhase `json:"upgradePhase,omitempty"`

	// Cells lists the cells the operator mapped into the nova_api database,
	// cell0 first. It reports what the cell-setup Job recorded, which is what
	// makes the UUIDs usable: a "nova-manage cell_v2" command that addresses one
	// cell takes the UUID, and it is generated at map time rather than derived
	// from the name.
	// +optional
	Cells []NovaCellStatus `json:"cells,omitempty"`

	// ComputeConfigSecretRef names the Secret carrying the compute contract: the
	// nova.conf fragment a nova-compute needs to join this control plane, and
	// the bus credentials it connects with. It is the handover point to the
	// compute nodes, which this operator does not deploy.
	// +optional
	ComputeConfigSecretRef *corev1.LocalObjectReference `json:"computeConfigSecretRef,omitempty"`

	// RemoteComputeConfigSecretRef names the Secret carrying the remote compute
	// contract, the one whose addresses a nova-compute on another cluster
	// reaches. The name is the Secret's on this Nova's own cluster: a copy on a
	// compute cluster carries the name its copier gives it, and the ControlPlane
	// mirror keeps the in-cluster contract's name, the one
	// computeConfigSecretRef gives. It is set while spec.remoteCompute is set
	// and the Secret has been written, and cleared when the block is removed.
	// +optional
	RemoteComputeConfigSecretRef *corev1.LocalObjectReference `json:"remoteComputeConfigSecretRef,omitempty"`
}

// NovaCellStatus pairs a mapped cell with the UUID nova assigned it.
type NovaCellStatus struct {
	// Name is the cell name as it appears in the cell map: "cell0" for the
	// holding pen and "cell1" for the single real cell.
	Name string `json:"name"`

	// UUID is the identifier nova generated when the cell was mapped. It is what
	// a per-cell "nova-manage cell_v2" command addresses the cell by.
	UUID string `json:"uuid"`
}

// EffectiveKeystonePublicEndpoint resolves the [keystone_authtoken]
// www_authenticate_uri value: the explicit KeystonePublicEndpoint when set,
// otherwise KeystoneEndpoint. It is resolved at render time rather than
// webhook-defaulted so a later edit to keystoneEndpoint keeps being tracked by
// the fallback instead of freezing a once-defaulted value.
func (s *NovaSpec) EffectiveKeystonePublicEndpoint() string {
	if s.KeystonePublicEndpoint != "" {
		return s.KeystonePublicEndpoint
	}
	return s.KeystoneEndpoint
}

// ConsoleProxyEnabled reports whether the console proxy is projected. An unset
// switch is enabled, which is the value the defaulting webhook materializes, so
// the two agree for a CR that reached the webhook and this method still answers
// for one that did not.
func (s *NovaSpec) ConsoleProxyEnabled() bool {
	return s.ConsoleProxy.Enabled == nil || *s.ConsoleProxy.Enabled
}

func init() {
	SchemeBuilder.Register(&Nova{}, &NovaList{})
}
