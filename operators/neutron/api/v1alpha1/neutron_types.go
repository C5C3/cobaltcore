// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// Name-length bounds enforced on metadata.name, driven by the ovn-db-sync
// CronJob, the child object with the tightest name budget.
const (
	// MaxCronJobNameLength is the API server's own cap on a CronJob name:
	// DNS1035LabelMaxLength (63) minus the 11-character "-<timestamp>" suffix its
	// controller appends to every Job it spawns.
	MaxCronJobNameLength = 52
	// ovnDBSyncNameSuffix is appended to metadata.name to name the OVN
	// database-synchronisation CronJob. It is duplicated from the controller
	// package (which builds the object) because the api package cannot import the
	// controller.
	ovnDBSyncNameSuffix = "-ovn-db-sync"
	// MaxNeutronNameLength is what those two leave for metadata.name.
	MaxNeutronNameLength = MaxCronJobNameLength - len(ovnDBSyncNameSuffix)
)

// Defaults for the OVN database-synchronisation CronJob, resolved when
// spec.ovnDBSync omits the field.
//
// DefaultOVNDBSyncSchedule is consumed by the reconcile-time resolver and
// deliberately NOT applied by the defaulting webhook, so a partial spec.ovnDBSync
// block keeps tracking this operator default across upgrades instead of freezing
// today's value into the stored CR. DefaultOVNDBSyncMode names the same value the
// +kubebuilder:default marker on OVNDBSyncSpec.SyncMode carries, which markers
// cannot reference from a Go constant.
const (
	DefaultOVNDBSyncSchedule = "0 * * * *"
	DefaultOVNDBSyncMode     = "log"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Release",type="string",JSONPath=".status.installedRelease"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Neutron is the Schema for the neutrons API. It deploys the Neutron network
// service against an OVN control plane: the API server, the RPC workers, the
// database and cache connections, the Keystone integration, the message bus, and
// the recurring OVN database synchronisation.
//
// The OVN metadata agent is a separate kind (NeutronMetadataAgent). It runs on
// the compute and network nodes, in the privileged namespace the OVNChassis
// DaemonSets live in, which is not a namespace the API Deployment belongs in.
type Neutron struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NeutronSpec   `json:"spec,omitempty"`
	Status NeutronStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NeutronList contains a list of Neutron.
type NeutronList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Neutron `json:"items"`
}

// NeutronSpec defines the desired state of Neutron.
//
// The targetClusterRef transition rules (evaluated only on UPDATE) freeze the
// ref: adding it, removing it, or renaming it after creation is rejected at the
// schema layer, so the guarantee holds even when the validating webhook is
// down. Moving a service between clusters is not a supported mutation.
// +kubebuilder:validation:XValidation:rule="has(self.targetClusterRef) == has(oldSelf.targetClusterRef)",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.targetClusterRef) || !has(oldSelf.targetClusterRef) || self.targetClusterRef.name == oldSelf.targetClusterRef.name",message="targetClusterRef is immutable"
type NeutronSpec struct {
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

	// Deployment groups the pod-level knobs for the Neutron API Deployment
	// (replicas, resources, rollout strategy, graceful-termination timings, and
	// scheduling constraints).
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`

	// Image defines the Neutron container image reference. Like the sibling
	// operators, the field carries no immutability rule: image upgrades are
	// routine.
	Image commonv1.ImageSpec `json:"image"`

	// Database defines the MariaDB connection parameters.
	// Supports managed (clusterRef) and brownfield (host/port) modes. The
	// clusterRef/host mutual-exclusivity rule and the credentialsMode
	// (Static/Dynamic) contract are inherited from commonv1.DatabaseSpec, so they
	// hold here without per-field duplication.
	Database commonv1.DatabaseSpec `json:"database"`

	// Cache defines the Memcached cache configuration.
	// Supports managed (clusterRef) and brownfield (servers) modes; the
	// clusterRef/servers mutual-exclusivity rule lives on commonv1.CacheSpec.
	Cache commonv1.CacheSpec `json:"cache"`

	// KeystoneEndpoint is the Keystone endpoint URL Neutron authenticates
	// against. It renders as [keystone_authtoken] auth_url in neutron.conf.
	// Neutron connects to this URL server-side (token validation on every API
	// request), so it must be reachable from the Neutron pods. For a colocated
	// control plane that is the cluster-local Service URL, never an externally
	// routable address that only resolves outside the cluster. A plain URL field
	// keeps the operator decoupled from the keystone-operator; the c5c3
	// ControlPlane operator projects it from its Keystone child by naming
	// convention.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystoneEndpoint string `json:"keystoneEndpoint"`

	// KeystonePublicEndpoint is the browser/client-facing Keystone base URL
	// Neutron advertises as [keystone_authtoken] www_authenticate_uri, the
	// address a 401 response points unauthenticated clients at. Optional: when
	// empty the operator falls back to KeystoneEndpoint (see
	// EffectiveKeystonePublicEndpoint), which is correct only when the internal
	// and public Keystone URLs coincide.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystonePublicEndpoint string `json:"keystonePublicEndpoint,omitempty"`

	// ServiceUser identifies the Keystone service account Neutron
	// authenticates as and the Secret holding its password. The defaulting
	// webhook fills the username/domain fields (neutron / service / Default /
	// Default); the password Secret reference is required.
	ServiceUser ServiceUserSpec `json:"serviceUser"`

	// Region is the Keystone region Neutron authenticates against
	// ([keystone_authtoken] region_name). Optional: when empty the option is
	// omitted and Neutron uses the Keystone catalog's default region.
	// +optional
	Region string `json:"region,omitempty"`

	// APIServer tunes the Neutron API server process. When nil the operator
	// uses hardcoded defaults.
	// +optional
	APIServer *APIServerSpec `json:"apiServer,omitempty"`

	// Workers groups the pod-level knobs for the neutron-server RPC worker
	// Deployment. The workers run the same neutron-server binary as the API pods
	// with the RPC side enabled, so they scale on the message bus rather than on
	// the HTTP request rate and get their own Deployment.
	// +optional
	Workers WorkersSpec `json:"workers,omitempty"`

	// Messaging defines the RabbitMQ connection Neutron publishes its RPC calls
	// and notifications on. It is required: an OVN Neutron still talks to the
	// message bus for its own RPC fan-out and for the Nova notifications that
	// tell a port it is ready.
	//
	// This is the first service CRD to embed the shared block, so two of its
	// properties materialize here for the first time. The replicas field defaults
	// to 3 and no consumer of this CR reads it: only the managed-mode projection
	// in the c5c3 ControlPlane operator honours it. And a placed Neutron (one with
	// targetClusterRef set) whose bus lives on the management cluster must use
	// secretRef: the transport-URL helper reads and writes in the CR's own
	// namespace through the CR's own children client, which for a placed CR is the
	// target cluster's.
	Messaging commonv1.MessagingSpec `json:"messaging"`

	// OVN names the OVN control plane this Neutron drives. The ML2/OVN mechanism
	// driver writes the logical network model into the Northbound database, so
	// there is exactly one control plane per Neutron.
	OVN OVNSpec `json:"ovn"`

	// OVNDBSync configures the recurring neutron-ovn-db-sync-util run that
	// compares the Neutron database against the OVN Northbound database. When nil
	// no CronJob is created.
	// +optional
	OVNDBSync *OVNDBSyncSpec `json:"ovnDBSync,omitempty"`

	// Gateway configures external exposure of the Neutron API via a Gateway API
	// HTTPRoute. When set, the operator creates an HTTPRoute targeting the {name}
	// Service and attaches it to the referenced pre-existing Gateway. When removed
	// (nil), the HTTPRoute is deleted. The Gateway and GatewayClass are
	// infrastructure concerns managed outside this operator.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`

	// NetworkPolicy configures network isolation for Neutron API pods.
	// When set, a NetworkPolicy is created restricting ingress and egress traffic.
	// When removed (nil), the NetworkPolicy is deleted and traffic flows
	// unrestricted.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`

	// Autoscaling configures horizontal pod autoscaling for the Neutron API
	// deployment. When set, a HorizontalPodAutoscaler is created targeting the
	// deployment. When removed, the HPA is deleted.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Logging configures oslo.log output for the Neutron containers.
	// When unset, the defaulting webhook materializes a LoggingSpec with
	// Format=text, Level=INFO, Debug=false.
	// +optional
	Logging *LoggingSpec `json:"logging,omitempty"`

	// SecretStoreRef selects the External Secrets store the operator routes this
	// Neutron's ExternalSecrets and PushSecrets through. When omitted the operator
	// uses the shared cluster-scoped openbao-cluster-store, so existing
	// deployments keep working unchanged. Set kind to SecretStore with the name of
	// a namespaced store in THIS Neutron's namespace to reach OpenBao as a
	// per-tenant identity. The ControlPlane operator projects this field onto the
	// Neutron it owns, so operators normally configure it there rather than here.
	// +optional
	SecretStoreRef *commonv1.SecretStoreRefSpec `json:"secretStoreRef,omitempty"`

	// TargetClusterRef names the registered target cluster that receives this
	// Neutron's children: Deployments, ConfigMaps, Secrets, and the database
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

	// ExtraConfig provides free-form INI sections for configuration not covered by
	// explicit CRD fields. It is the deliberate escape hatch for options that have
	// no dedicated knob of their own.
	//
	// The option names are validated against the flat union catalog of
	// neutron.conf, ml2_conf.ini and neutron_ovn_metadata_agent.ini, which carries
	// no per-file provenance. A section that belongs to the other process
	// therefore passes validation, and oslo.config ignores it at runtime.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`
}

// DeploymentSpec, AutoscalingSpec, NetworkPolicySpec,
// NetworkPolicyIngressSource, LoggingSpec, GatewaySpec, and
// GatewayParentRefSpec are aliased to the shared commonv1 definitions.
// commonv1 carries the canonical per-field godoc and validation markers.
// The aliases keep call sites (neutronv1alpha1.DeploymentSpec and bare
// DeploymentSpec{} literals alike) consistent with the sibling operators.
type (
	DeploymentSpec             = commonv1.DeploymentSpec
	AutoscalingSpec            = commonv1.AutoscalingSpec
	NetworkPolicySpec          = commonv1.NetworkPolicySpec
	NetworkPolicyIngressSource = commonv1.NetworkPolicyIngressSource
	LoggingSpec                = commonv1.LoggingSpec
	GatewaySpec                = commonv1.GatewaySpec
	GatewayParentRefSpec       = commonv1.GatewayParentRefSpec
)

// ServiceUserSpec identifies the Keystone service account Neutron uses to
// validate tokens and call other services, and references the Secret holding its
// password. The name and domain fields are optional; the defaulting webhook
// materializes them (username neutron, projectName service, userDomainName and
// projectDomainName Default), so a minimal CR need only supply the password
// Secret reference.
type ServiceUserSpec struct {
	// Username is the Keystone username Neutron authenticates as
	// ([keystone_authtoken] username). Webhook-defaulted to "neutron".
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

// APIServerSpec tunes the Neutron API server process. It carries the uWSGI
// parameters alone: the operator runs neutron-api as a WSGI application, so
// there is no launch mode to select and no worker count to configure outside
// uWSGI. The RPC worker counts live in [DEFAULT] and are derived from
// spec.workers instead.
type APIServerSpec struct {
	// UWSGI configures the uWSGI application server parameters.
	// +optional
	UWSGI *UWSGISpec `json:"uwsgi,omitempty"`
}

// UWSGISpec is aliased to the shared commonv1 definition (see the
// DeploymentSpec alias block above for the rationale).
type UWSGISpec = commonv1.UWSGISpec

// WorkersSpec configures the neutron-server RPC workers, the processes that
// serve the agent RPC and the maintenance tasks the API pods do not run.
type WorkersSpec struct {
	// Deployment groups the pod-level knobs for the worker Deployment (replicas,
	// resources, rollout strategy, graceful-termination timings, and scheduling
	// constraints).
	// +optional
	Deployment commonv1.DeploymentSpec `json:"deployment,omitempty"`
}

// OVNSpec names the OVN control plane this Neutron drives.
type OVNSpec struct {
	// CentralRef names the OVNCentral whose Northbound and Southbound databases
	// the ML2/OVN mechanism driver connects to.
	CentralRef OVNCentralRef `json:"centralRef"`
}

// OVNCentralRef names an OVNCentral. Unlike the reference on OVNChassis, this
// one carries a namespace: the OVN control plane commonly lives in the
// privileged networking namespace while the Neutron API lives with the rest of
// the control plane, and the operator reads the connection details out of the
// OVNCentral's status and its client Secret rather than mounting the Secret
// directly.
type OVNCentralRef struct {
	// Name is the OVNCentral's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the namespace the OVNCentral lives in. The defaulting webhook
	// fills it with this CR's namespace when empty.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// OVNDBSyncSpec configures the recurring neutron-ovn-db-sync-util run. Neutron
// and the OVN Northbound database each hold their own copy of the logical
// network model, and a write that fails halfway through leaves them apart. The
// utility walks both and reports, or repairs, the difference.
//
// A nil block means no CronJob at all: the sync reads and can rewrite the entire
// logical model, so scheduling it is a deliberate choice rather than a default.
type OVNDBSyncSpec struct {
	// Schedule is the standard cron expression the sync CronJob runs on. When
	// empty the operator resolves DefaultOVNDBSyncSchedule at reconcile time. The
	// value is checked by the validating webhook rather than by a CRD pattern: the
	// accepted grammar includes descriptors such as @daily, which no regex
	// expresses without also rejecting valid expressions.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// SyncMode selects what the utility does with the difference it finds. In log
	// mode it exits 0 whether or not the two databases agree, so the drift report
	// lives in the Job log and the status condition reports Job outcomes only. In
	// repair mode it deletes the Northbound objects Neutron does not know about
	// and creates the ones it is missing.
	// +optional
	// +kubebuilder:validation:Enum=log;repair
	// +kubebuilder:default=log
	SyncMode string `json:"syncMode,omitempty"`

	// Suspend pauses the sync CronJob without deleting it. It is the escape hatch
	// for a maintenance window in which a repair-mode run would fight an operator
	// editing the Northbound database by hand.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// NeutronStatus defines the observed state of Neutron.
type NeutronStatus struct {
	// Conditions represent the latest available observations of the Neutron
	// state. Each condition carries an ObservedGeneration so consumers can tell a
	// stale condition from one reflecting the current spec; use the conditions
	// helper (internal/common/conditions) to upsert them.
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

	// Endpoint is the Neutron API endpoint URL clients use.
	Endpoint string `json:"endpoint,omitempty"`

	// InstalledRelease is the OpenStack release whose database schema is currently
	// installed, promoted to spec.openStackRelease after a successful db-sync.
	InstalledRelease string `json:"installedRelease,omitempty"`

	// InstalledImage is the spec.image reference that migrated the schema
	// InstalledRelease names. It is what keeps release tracking honest for a
	// digest-pinned image: a digest carries no parseable release, so a
	// spec.openStackRelease bump that leaves spec.image untouched leaves the
	// db-sync Job's pod template unchanged, runs no migration at all, and would
	// still promote InstalledRelease. The release gate compares against this field
	// to refuse that transition.
	InstalledImage string `json:"installedImage,omitempty"`

	// TargetRelease is the spec.openStackRelease being converged to during an
	// active upgrade.
	TargetRelease string `json:"targetRelease,omitempty"`

	// UpgradePhase is the current expand-migrate-contract phase during an active
	// release upgrade (Expanding, Migrating, RollingUpdate, Contracting); empty
	// when no upgrade is in flight.
	UpgradePhase commonv1.UpgradePhase `json:"upgradePhase,omitempty"`
}

// EffectiveKeystonePublicEndpoint resolves the [keystone_authtoken]
// www_authenticate_uri value: the explicit KeystonePublicEndpoint when set,
// otherwise KeystoneEndpoint. It is resolved at render time rather than
// webhook-defaulted so a later edit to keystoneEndpoint keeps being tracked by
// the fallback instead of freezing a once-defaulted value.
func (s *NeutronSpec) EffectiveKeystonePublicEndpoint() string {
	if s.KeystonePublicEndpoint != "" {
		return s.KeystonePublicEndpoint
	}
	return s.KeystoneEndpoint
}

func init() {
	SchemeBuilder.Register(&Neutron{}, &NeutronList{})
}
