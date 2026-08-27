// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// MaxNeutronMetadataAgentNameLength bounds metadata.name. The name is the
// app.kubernetes.io/instance label value on every child, and the owner-name
// label value on the children of a placed CR; Kubernetes caps a label value at
// 63 characters.
const MaxNeutronMetadataAgentNameLength = 63

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".status.desiredNumberScheduled"
// +kubebuilder:printcolumn:name="Ready pods",type="integer",JSONPath=".status.numberReady"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NeutronMetadataAgent is the Schema for the neutronmetadataagents API. It puts
// the OVN metadata agent onto the nodes an OVNChassis already runs on: one
// DaemonSet whose pods share the chassis's local OVS database and answer the
// 169.254.169.254 requests the instances on that node make.
//
// The agent is a separate kind from Neutron because it belongs in the chassis's
// namespace. Those pods are privileged and run on the compute and network nodes,
// which is not where the Neutron API Deployment belongs.
type NeutronMetadataAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NeutronMetadataAgentSpec   `json:"spec,omitempty"`
	Status NeutronMetadataAgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NeutronMetadataAgentList contains a list of NeutronMetadataAgent.
type NeutronMetadataAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NeutronMetadataAgent `json:"items"`
}

// NeutronMetadataAgentSpec defines the desired state of NeutronMetadataAgent.
//
// The targetClusterRef transition rules (evaluated only on UPDATE) freeze the
// ref, and the chassisRef rule freezes the chassis the agent runs alongside.
// Both are enforced at the schema layer so the guarantee holds even when the
// validating webhook is down: an agent re-pointed at another chassis lands on
// another set of nodes, where the local OVS databases carry none of the ports it
// was answering for.
// +kubebuilder:validation:XValidation:rule="has(self.targetClusterRef) == has(oldSelf.targetClusterRef)",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.targetClusterRef) || !has(oldSelf.targetClusterRef) || self.targetClusterRef.name == oldSelf.targetClusterRef.name",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.chassisRef.name == oldSelf.chassisRef.name",message="chassisRef is immutable"
type NeutronMetadataAgentSpec struct {
	// OpenStackRelease names the OpenStack release this agent runs. It selects the
	// option catalog spec.extraConfig is validated against.
	//
	// The pattern matches the OpenStack date-based release scheme (YYYY.N where N
	// is 1 or 2, the two-releases-per-year cadence, e.g. 2025.2, 2026.1). The
	// [12] minor class keeps this CRD pattern, the validating webhook, and
	// release.ParseRelease in agreement so a non-cadence minor (e.g. 2025.9) is
	// rejected at every layer.
	// +kubebuilder:validation:Pattern=`^\d{4}\.[12]$`
	OpenStackRelease string `json:"openStackRelease"`

	// Image defines the Neutron container image the agent runs from. It is
	// required: the agent is deployed next to an OVNChassis whose image this
	// operator does not resolve, so there is no tested pairing to fall back on.
	Image commonv1.ImageSpec `json:"image"`

	// ChassisRef names the OVNChassis this agent runs alongside. The agent reads
	// the chassis's local OVS database over the socket the chassis pods share, so
	// the two land on the same nodes and in the same namespace.
	ChassisRef OVNChassisRef `json:"chassisRef"`

	// Messaging optionally defines the RabbitMQ connection the agent uses. The OVN
	// metadata agent opens no RPC and no notification connection of its own: it
	// reads what it needs out of the Southbound database. It is offered because
	// n_rpc.init, which config.init calls unconditionally, parses oslo.messaging's
	// default rabbit:// transport URL without dialing it, so a deployment that
	// wants the agent to carry the same bus configuration as the API can say so.
	// When set, the agent gets the same transport-URL env override the API pods
	// get.
	// +optional
	Messaging *commonv1.MessagingSpec `json:"messaging,omitempty"`

	// NovaMetadata points the agent at the Nova metadata API it proxies to. Nova
	// is not onboarded onto this operator, so a nil block renders nothing and the
	// oslo defaults apply.
	// +optional
	NovaMetadata *NovaMetadataSpec `json:"novaMetadata,omitempty"`

	// Resources defines the CPU and memory requests and limits for the agent
	// container. When empty the operator applies the shared container defaults,
	// the ones a defaulted DeploymentSpec carries.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Logging configures oslo.log output for the agent container. When unset, the
	// defaulting webhook materializes a LoggingSpec with Format=text, Level=INFO,
	// Debug=false.
	// +optional
	Logging *LoggingSpec `json:"logging,omitempty"`

	// TargetClusterRef names the registered target cluster that receives this
	// agent's children. The CR itself stays on the management cluster, and so do
	// its status and its finalizers. When omitted, the children are created on the
	// local cluster. The field is immutable (see the transition rules on this
	// spec).
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`

	// ExtraConfig provides free-form INI sections for
	// neutron_ovn_metadata_agent.ini configuration not covered by explicit CRD
	// fields.
	//
	// The option names are validated against the flat union catalog of
	// neutron.conf, ml2_conf.ini and neutron_ovn_metadata_agent.ini, which carries
	// no per-file provenance. A section that belongs to the API process therefore
	// passes validation, and oslo.config ignores it at runtime.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`
}

// OVNChassisRef names an OVNChassis in the agent's own namespace. The reference
// is deliberately namespace-local: the agent pods mount the chassis's runtime
// directory and the Secret holding its Southbound client certificate, and
// neither is reachable across namespaces. It is why the CR lives in the
// chassis's privileged namespace rather than with the Neutron API.
type OVNChassisRef struct {
	// Name is the OVNChassis's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// NovaMetadataSpec points the agent at the Nova metadata API. The agent
// terminates the instance's request to 169.254.169.254 and forwards it to that
// API with the instance identity attached, signed with the shared secret both
// sides carry.
type NovaMetadataSpec struct {
	// Host is the address of the Nova metadata API. When empty the oslo default
	// applies.
	// +optional
	Host string `json:"host,omitempty"`

	// Port is the port the Nova metadata API listens on. The defaulting webhook
	// fills it with 8775, nova-api-metadata's own default, when the block is set
	// and the port is zero.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// SharedSecretRef references the Secret holding the secret the agent signs
	// forwarded requests with. Nova rejects an unsigned request when it is
	// configured with a secret of its own, so the two values have to match. The
	// key is webhook-defaulted to "shared_secret".
	// +optional
	SharedSecretRef *commonv1.SecretRefSpec `json:"sharedSecretRef,omitempty"`
}

// NeutronMetadataAgentStatus defines the observed state of NeutronMetadataAgent.
type NeutronMetadataAgentStatus struct {
	// Conditions represent the latest available observations of the
	// NeutronMetadataAgent state. Each condition carries an ObservedGeneration so
	// consumers can tell a stale condition from one reflecting the current spec;
	// use the conditions helper (internal/common/conditions) to upsert them.
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

	// InstalledImage is the image reference the running DaemonSet was projected
	// from. It is what tells a rollout that has not reached the nodes yet from one
	// that has.
	// +optional
	InstalledImage string `json:"installedImage,omitempty"`

	// DesiredNumberScheduled is how many nodes the DaemonSet should run on,
	// mirrored from the DaemonSet status.
	// +optional
	DesiredNumberScheduled int32 `json:"desiredNumberScheduled,omitempty"`

	// NumberReady is how many of those nodes have a ready agent pod.
	// +optional
	NumberReady int32 `json:"numberReady,omitempty"`
}

func init() {
	SchemeBuilder.Register(&NeutronMetadataAgent{}, &NeutronMetadataAgentList{})
}
