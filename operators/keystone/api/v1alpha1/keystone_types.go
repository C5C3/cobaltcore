// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// Selector label keys and values used by the Deployment pod selector, webhook
// TSC validation, and commonLabels(). Exported so that both the webhook and
// controller reference the same constants — prevents silent drift (CC-0075).
const (
	LabelKeyName     = "app.kubernetes.io/name"
	LabelKeyInstance = "app.kubernetes.io/instance"
	AppName          = "keystone"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Release",type="string",JSONPath=".status.installedRelease"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Keystone is the Schema for the keystones API.
type Keystone struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeystoneSpec   `json:"spec,omitempty"`
	Status KeystoneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeystoneList contains a list of Keystone.
type KeystoneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Keystone `json:"items"`
}

// KeystoneSpec defines the desired state of Keystone.
type KeystoneSpec struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	Replicas int32 `json:"replicas,omitempty"`

	// Image defines the Keystone container image reference.
	Image commonv1.ImageSpec `json:"image"`

	// Database defines the MariaDB connection parameters.
	// Supports managed (clusterRef) and brownfield (host/port) modes.
	// +kubebuilder:validation:XValidation:rule="has(self.clusterRef) != has(self.host)",message="exactly one of clusterRef or host must be set"
	Database commonv1.DatabaseSpec `json:"database"`

	// Cache defines the Memcached cache configuration.
	// Supports managed (clusterRef) and brownfield (servers) modes.
	// +kubebuilder:validation:XValidation:rule="has(self.clusterRef) != (has(self.servers) && size(self.servers) > 0)",message="exactly one of clusterRef or servers must be set"
	Cache commonv1.CacheSpec `json:"cache"`

	// Fernet configures Fernet key rotation.
	Fernet FernetSpec `json:"fernet,omitempty"`

	// CredentialKeys configures credential key rotation.
	CredentialKeys CredentialKeysSpec `json:"credentialKeys,omitempty"`

	// TrustFlush configures periodic purging of expired trust delegations (CC-0057).
	// When set, the operator creates a CronJob running keystone-manage trust_flush
	// on the specified schedule. When removed (nil), the CronJob is deleted.
	// +optional
	TrustFlush *TrustFlushSpec `json:"trustFlush,omitempty"`

	// Federation configures Keystone federation (optional).
	// +optional
	Federation *FederationSpec `json:"federation,omitempty"`

	// Bootstrap configures the initial Keystone bootstrap.
	Bootstrap BootstrapSpec `json:"bootstrap"`

	// Middleware defines WSGI middleware filters for the api-paste.ini pipeline.
	// +optional
	Middleware []commonv1.MiddlewareSpec `json:"middleware,omitempty"`

	// Plugins defines service plugins/drivers to configure.
	// +optional
	Plugins []commonv1.PluginSpec `json:"plugins,omitempty"`

	// PolicyOverrides defines custom oslo.policy rules for the service.
	// When set, the operator renders a policy.yaml and configures
	// oslo_policy.policy_file automatically.
	// +optional
	// +kubebuilder:validation:XValidation:rule="(has(self.rules) && size(self.rules) > 0) || self.configMapRef != null",message="at least one of rules or configMapRef must be set"
	// +kubebuilder:validation:XValidation:rule="!has(self.rules) || self.rules.all(k, k != '')",message="policy rule name must not be empty"
	PolicyOverrides *commonv1.PolicySpec `json:"policyOverrides,omitempty"`

	// Autoscaling configures horizontal pod autoscaling for the Keystone API deployment.
	// When set, a HorizontalPodAutoscaler is created targeting the deployment.
	// When removed, the HPA is deleted.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// NetworkPolicy configures network isolation for Keystone API pods.
	// When set, a NetworkPolicy is created restricting ingress and egress traffic.
	// When removed (nil), the NetworkPolicy is deleted and traffic flows unrestricted.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`

	// Resources defines the CPU and memory requests and limits for the Keystone API
	// container. When unset, the defaulting webhook injects sensible defaults
	// (256Mi/512Mi memory, 100m/500m CPU) to ensure Burstable QoS class and
	// enable HPA utilization calculations (CC-0042).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// UWSGI configures the uWSGI application server parameters (CC-0040).
	// When set, the operator uses these values for the uWSGI command in the
	// Deployment. When nil, hardcoded defaults (processes=2, threads=1,
	// httpKeepAlive=true) are used.
	// +optional
	UWSGI *UWSGISpec `json:"uwsgi,omitempty"`

	// TopologySpreadConstraints describes how pods should be spread across
	// topology domains (zones, nodes) to achieve high availability (CC-0075).
	// When nil (unset), the operator injects two default constraints:
	// zone-spread (topology.kubernetes.io/zone) and hostname-spread
	// (kubernetes.io/hostname), both MaxSkew=1 with ScheduleAnyway.
	// When set to a non-nil value (including an empty slice), the user-provided
	// constraints are used verbatim — an empty slice disables defaults.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// PriorityClassName sets the priority class for Keystone API pods (CC-0075).
	// When set, the operator passes the value through to the PodSpec, allowing
	// cluster administrators to control scheduling priority and preemption.
	// When unset, no priority class is configured and the cluster default applies.
	// +optional
	PriorityClassName *string `json:"priorityClassName,omitempty"`

	// ExtraConfig provides free-form INI sections for configuration
	// not covered by explicit CRD fields.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`
}

// AutoscalingSpec defines the parameters for horizontal pod autoscaling (CC-0038).
// +kubebuilder:validation:XValidation:rule="has(self.targetCPUUtilization) || has(self.targetMemoryUtilization)",message="at least one of targetCPUUtilization or targetMemoryUtilization must be set"
type AutoscalingSpec struct {
	// MinReplicas is the lower bound for the number of replicas.
	// Defaults to the current spec.replicas value if unset.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the upper bound for the number of replicas.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// TargetCPUUtilization is the target average CPU utilization (percentage).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetCPUUtilization *int32 `json:"targetCPUUtilization,omitempty"`

	// TargetMemoryUtilization is the target average memory utilization (percentage).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetMemoryUtilization *int32 `json:"targetMemoryUtilization,omitempty"`
}

// NetworkPolicySpec defines network isolation for Keystone API pods (CC-0039).
// When applied, the operator creates a NetworkPolicy that restricts ingress
// to TCP 5000 from the specified sources and auto-derives egress rules for
// DNS, MariaDB (from database.ClusterRef), and Memcached (from cache.ClusterRef).
// +kubebuilder:validation:XValidation:rule="size(self.ingress) > 0",message="at least one ingress source must be specified"
type NetworkPolicySpec struct {
	// Ingress defines the sources allowed to reach Keystone API on TCP 5000.
	// Each source specifies a namespace selector and an optional pod selector.
	// Multiple sources produce multiple From peers in a single ingress rule
	// (OR across peers, AND within a peer's selectors).
	Ingress []NetworkPolicyIngressSource `json:"ingress"`

	// AdditionalEgress defines extra egress rules appended after auto-derived
	// rules (DNS, MariaDB, Memcached). Use this for brownfield backends,
	// external APIs, or any target not covered by ClusterRef auto-derivation.
	// +optional
	AdditionalEgress []networkingv1.NetworkPolicyEgressRule `json:"additionalEgress,omitempty"`
}

// NetworkPolicyIngressSource defines a source from which traffic is allowed
// to reach the Keystone API pods on TCP 5000 (CC-0039).
type NetworkPolicyIngressSource struct {
	// NamespaceSelector selects namespaces from which traffic is allowed.
	// All pods in matching namespaces can reach Keystone on port 5000
	// unless PodSelector further restricts the set.
	NamespaceSelector map[string]string `json:"namespaceSelector"`

	// PodSelector optionally restricts allowed traffic to pods matching
	// these labels within the selected namespaces. When set, only pods
	// matching both NamespaceSelector AND PodSelector can reach Keystone
	// (AND logic within a single peer).
	// +optional
	PodSelector map[string]string `json:"podSelector,omitempty"`
}

// UWSGISpec defines the uWSGI application server parameters (CC-0040).
// Exposed as an optional pointer field on KeystoneSpec so that existing CRs
// without spec.uwsgi continue to work with hardcoded defaults in the reconciler.
type UWSGISpec struct {
	// Processes is the number of uWSGI worker processes.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	Processes int32 `json:"processes,omitempty"`

	// Threads is the number of threads per uWSGI worker process.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Threads int32 `json:"threads,omitempty"`

	// HTTPKeepAlive enables the --http-keepalive flag on the uWSGI process.
	// When false, the flag is omitted from the command.
	// +kubebuilder:default=true
	HTTPKeepAlive bool `json:"httpKeepAlive,omitempty"`
}

// FernetSpec defines Fernet key rotation configuration.
type FernetSpec struct {
	// RotationSchedule is a cron expression for key rotation.
	// +kubebuilder:default="0 0 * * 0"
	RotationSchedule string `json:"rotationSchedule,omitempty"`

	// MaxActiveKeys is the maximum number of active Fernet keys.
	// +kubebuilder:validation:Minimum=3
	// +kubebuilder:default=3
	MaxActiveKeys int32 `json:"maxActiveKeys,omitempty"`
}

// CredentialKeysSpec defines credential key rotation configuration.
type CredentialKeysSpec struct {
	// RotationSchedule is a cron expression for credential key rotation.
	// +kubebuilder:default="0 0 * * 0"
	RotationSchedule string `json:"rotationSchedule,omitempty"`

	// MaxActiveKeys is the maximum number of active credential keys.
	// +kubebuilder:validation:Minimum=3
	// +kubebuilder:default=3
	MaxActiveKeys int32 `json:"maxActiveKeys,omitempty"`
}

// TrustFlushSpec configures periodic purging of expired trust delegations (CC-0057).
// Exposed as an optional pointer field on KeystoneSpec so that existing CRs
// without spec.trustFlush continue to work — the reconciler skips CronJob
// creation when the pointer is nil.
type TrustFlushSpec struct {
	// Schedule is a cron expression controlling when keystone-manage trust_flush runs.
	// +kubebuilder:default="0 * * * *"
	Schedule string `json:"schedule,omitempty"`

	// Suspend pauses the CronJob without deleting it.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`

	// Args provides additional CLI flags passed to keystone-manage trust_flush.
	// +optional
	Args []string `json:"args,omitempty"`
}

// FederationSpec defines Keystone federation configuration.
type FederationSpec struct {
	// Enabled activates federation support.
	Enabled bool `json:"enabled"`
}

// BootstrapSpec defines Keystone bootstrap parameters.
type BootstrapSpec struct {
	// AdminUser is the admin username for the bootstrap.
	// +kubebuilder:default="admin"
	AdminUser string `json:"adminUser,omitempty"`

	// AdminPasswordSecretRef references the Secret containing the admin password.
	AdminPasswordSecretRef commonv1.SecretRefSpec `json:"adminPasswordSecretRef"`

	// Region is the Keystone region name.
	// +kubebuilder:default="RegionOne"
	Region string `json:"region,omitempty"`

	// PublicEndpoint is the externally routable Keystone endpoint URL used for
	// --bootstrap-public-url. When unset, the cluster-local service DNS is used
	// as a fallback. External clients (CLI users, Horizon, federation partners)
	// require a routable address here (CC-0013).
	// +optional
	PublicEndpoint string `json:"publicEndpoint,omitempty"`
}

// UpgradePhase represents the current phase of a database upgrade (CC-0056).
// +kubebuilder:validation:Enum=Expanding;Migrating;RollingUpdate;Contracting
type UpgradePhase string

const (
	UpgradePhaseExpanding     UpgradePhase = "Expanding"
	UpgradePhaseMigrating     UpgradePhase = "Migrating"
	UpgradePhaseRollingUpdate UpgradePhase = "RollingUpdate"
	UpgradePhaseContracting   UpgradePhase = "Contracting"
)

// KeystoneStatus defines the observed state of Keystone.
type KeystoneStatus struct {
	// Conditions represent the latest available observations of the Keystone state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Endpoint is the Keystone API endpoint URL.
	Endpoint string `json:"endpoint,omitempty"`

	// InstalledRelease is the OpenStack release version currently deployed (CC-0056).
	InstalledRelease string `json:"installedRelease,omitempty"`

	// TargetRelease is the upgrade target release during an active upgrade (CC-0056).
	TargetRelease string `json:"targetRelease,omitempty"`

	// UpgradePhase is the current phase of a database upgrade (CC-0056).
	UpgradePhase UpgradePhase `json:"upgradePhase,omitempty"`
}

func init() {
	SchemeBuilder.Register(&Keystone{}, &KeystoneList{})
}
