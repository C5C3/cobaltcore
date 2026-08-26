// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// MaxOVNChassisNameLength bounds metadata.name by the child object with the
// tightest name budget, the per-node chassis-deletion Job. Its name is
// "{name}-chassis-del-{8 hex}", 21 characters on top of metadata.name, against
// the 63-character cap Kubernetes puts on an object name.
const MaxOVNChassisNameLength = 42

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=ovnchassis
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".status.desiredNumberScheduled"
// +kubebuilder:printcolumn:name="Ready pods",type="integer",JSONPath=".status.numberReady"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// OVNChassis is the Schema for the ovnchassis API. It puts the Open vSwitch and
// ovn-controller DaemonSets onto the nodes its nodeSelector matches and
// registers each of them as a chassis with the OVNCentral the CR names.
//
// One OVNChassis covers one class of node. A deployment that runs gateway nodes
// on different hardware, or an availability zone with its own bridge mappings,
// gets a second CR rather than a wider selector, because everything below the
// selector (the bridge mappings, the encapsulation, the rollout pace) applies
// uniformly to every node it matches.
type OVNChassis struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OVNChassisSpec   `json:"spec,omitempty"`
	Status OVNChassisStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OVNChassisList contains a list of OVNChassis.
type OVNChassisList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OVNChassis `json:"items"`
}

// OVNChassisSpec defines the desired state of OVNChassis.
//
// The targetClusterRef transition rules (evaluated only on UPDATE) freeze the
// ref, and the centralRef rule freezes the control plane the chassis are
// registered with. Both are enforced at the schema layer so the guarantee holds
// even when the validating webhook is down: repointing a live chassis leaves its
// registration behind in the old Southbound database, where it keeps claiming
// the ports of workloads that have moved on.
// +kubebuilder:validation:XValidation:rule="has(self.targetClusterRef) == has(oldSelf.targetClusterRef)",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.targetClusterRef) || !has(oldSelf.targetClusterRef) || self.targetClusterRef.name == oldSelf.targetClusterRef.name",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.centralRef.name == oldSelf.centralRef.name",message="centralRef is immutable"
type OVNChassisSpec struct {
	// Image defines the container image that runs Open vSwitch and
	// ovn-controller. Both ship in the same OVN image, so one reference covers
	// them. When nil the operator resolves its own default image, which keeps the
	// CR tracking the operator's tested OVN version across upgrades instead of
	// freezing today's tag into stored state.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// CentralRef names the OVNCentral in the same namespace whose Southbound
	// database these chassis connect to, and whose client Secret they mount. It
	// is immutable, enforced by the transition rule above and by the validating
	// webhook.
	CentralRef OVNCentralRef `json:"centralRef"`

	// NodeSelector picks the nodes the DaemonSets land on. It is required and
	// must carry at least one label: an empty selector matches every node in the
	// cluster, which would start ovn-controller on the control-plane nodes and on
	// whatever else happens to join later.
	// +kubebuilder:validation:MinProperties=1
	NodeSelector map[string]string `json:"nodeSelector"`

	// Tolerations let the DaemonSet pods run on tainted nodes. Networking nodes
	// are commonly tainted to keep ordinary workloads off them, and the chassis
	// pods are exactly what has to run there anyway.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Gateway marks the subset of the selected nodes that announce
	// enable-chassis-as-gw, the flag that makes a chassis eligible to host a
	// distributed router's gateway port. Its selector is applied on top of
	// NodeSelector, so it can only narrow the set. When nil no node in this CR
	// is a gateway.
	// +optional
	Gateway *OVNGatewaySpec `json:"gateway,omitempty"`

	// BridgeMappings maps each OpenStack physical network onto the local OVS
	// bridge that reaches it. Every node this CR selects gets the same mapping,
	// which is why a class of node with different wiring belongs in its own CR.
	// +optional
	// +listType=map
	// +listMapKey=physicalNetwork
	BridgeMappings []OVNBridgeMapping `json:"bridgeMappings,omitempty"`

	// EncapType is the tunnel protocol between chassis. Geneve carries the
	// variable-length option header OVN uses for its logical metadata; VXLAN has
	// no room for it and so caps the logical topology, and is offered only for
	// hardware that cannot terminate Geneve.
	// +optional
	// +kubebuilder:default=geneve
	// +kubebuilder:validation:Enum=geneve;vxlan
	EncapType string `json:"encapType,omitempty"`

	// UpdateStrategy paces the DaemonSet rollout. Restarting ovn-controller
	// interrupts the dataplane programming on that node, so the rollout pace is a
	// per-deployment tradeoff rather than something the operator should decide.
	// +optional
	// +kubebuilder:default={}
	UpdateStrategy OVNChassisUpdateStrategy `json:"updateStrategy,omitempty"`

	// RemoteProbeIntervalMs is how long ovn-controller lets its Southbound
	// connection sit idle before probing it. Zero disables the probe, which is
	// what a chassis behind a connection-tracking middlebox needs when the probe
	// itself is what tears the connection down.
	// +optional
	// +kubebuilder:default=60000
	// +kubebuilder:validation:Minimum=0
	RemoteProbeIntervalMs int32 `json:"remoteProbeIntervalMs,omitempty"`

	// OVS tunes the Open vSwitch container. When nil the operator applies its own
	// defaults.
	// +optional
	OVS *OVNChassisContainerSpec `json:"ovs,omitempty"`

	// Controller tunes the ovn-controller container. When nil the operator
	// applies its own defaults.
	// +optional
	Controller *OVNChassisContainerSpec `json:"controller,omitempty"`

	// TargetClusterRef selects the registered target cluster the DaemonSets are
	// created on. When nil they are created on the cluster the operator runs in.
	// The ref is immutable once set.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`
}

// OVNCentralRef names an OVNCentral in the OVNChassis's own namespace. The
// reference is deliberately namespace-local: the chassis mount the client Secret
// that OVNCentral publishes, and a Secret cannot be mounted across namespaces.
type OVNCentralRef struct {
	// Name is the OVNCentral's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// OVNGatewaySpec narrows the selected nodes down to the ones that announce
// enable-chassis-as-gw.
type OVNGatewaySpec struct {
	// NodeSelector picks the gateway nodes out of the set spec.nodeSelector
	// already matched. It is required and must carry at least one label: an empty
	// selector would promote every selected node to a gateway, which spreads the
	// external connectivity across nodes that have no uplink to carry it.
	// +kubebuilder:validation:MinProperties=1
	NodeSelector map[string]string `json:"nodeSelector"`
}

// OVNBridgeMapping ties one OpenStack physical network to one local OVS bridge.
type OVNBridgeMapping struct {
	// PhysicalNetwork is the provider-network name Neutron knows the segment by.
	// The grammar is the DNS-1123 label the Neutron network name is bounded to.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	PhysicalNetwork string `json:"physicalNetwork"`

	// Bridge is the local OVS bridge name. It is bounded to 15 characters
	// because the bridge appears as a Linux interface and the kernel's IFNAMSIZ
	// leaves 15 usable bytes.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_.-]{1,15}$`
	Bridge string `json:"bridge"`
}

// OVNChassisUpdateStrategy paces the DaemonSet rollout. It is a narrowed
// DaemonSetUpdateStrategy: maxSurge has no counterpart here, because a second
// ovn-controller on the same node would contend with the first one over the
// local OVS database.
type OVNChassisUpdateStrategy struct {
	// Type selects the rollout mode. OnDelete hands the pace to whoever drains
	// the nodes, which is what a deployment with an external maintenance workflow
	// wants.
	// +optional
	// +kubebuilder:default=RollingUpdate
	// +kubebuilder:validation:Enum=RollingUpdate;OnDelete
	Type string `json:"type,omitempty"`

	// MaxUnavailable is how many selected nodes may lose their dataplane
	// programming at once. It applies to RollingUpdate only; the validating
	// webhook rejects it alongside OnDelete rather than letting it read as
	// effective. When nil the operator renders 1.
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// OVNChassisContainerSpec tunes one of the two containers in the chassis pod.
type OVNChassisContainerSpec struct {
	// Resources defines the CPU and memory requests and limits for the container.
	// When nil the operator applies its own defaults.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// OVNChassisStatus defines the observed state of OVNChassis.
type OVNChassisStatus struct {
	// Conditions represent the latest available observations of the OVNChassis
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

	// InstalledImage is the image reference the running DaemonSets were projected
	// from. It is what tells a rollout that has not reached the nodes yet from one
	// that has.
	// +optional
	InstalledImage string `json:"installedImage,omitempty"`

	// DesiredNumberScheduled is how many nodes the DaemonSets should run on,
	// mirrored from the DaemonSet status.
	// +optional
	DesiredNumberScheduled int32 `json:"desiredNumberScheduled,omitempty"`

	// NumberReady is how many of those nodes have a ready chassis pod.
	// +optional
	NumberReady int32 `json:"numberReady,omitempty"`

	// Nodes carries the per-node registration state. It is what the controller
	// works from rather than re-deriving from the Southbound database on every
	// pass: a node that stops being selected disappears from the node list but
	// keeps its chassis registration, so the record of it has to outlive the
	// selection.
	// +optional
	// +listType=map
	// +listMapKey=name
	Nodes []OVNChassisNodeStatus `json:"nodes,omitempty"`
}

// OVNChassisNodeStatus is the registration state of one node.
type OVNChassisNodeStatus struct {
	// Name is the node's name.
	Name string `json:"name"`

	// SystemID is the chassis identity ovn-controller registered in the
	// Southbound database. It is what a chassis-deletion Job addresses, so it has
	// to be recorded before the node goes away.
	// +optional
	SystemID string `json:"systemID,omitempty"`

	// Gateway reports whether the node currently announces enable-chassis-as-gw.
	// +optional
	Gateway bool `json:"gateway,omitempty"`

	// ConfigHash is a hash of the entry last applied to the node, so a
	// configuration change is distinguishable from a node that has simply not
	// reported back yet.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`

	// GatewayEvacuated reports that the evacuation Job moved the gateway ports
	// off this node and succeeded. It resets when the node takes the gateway role
	// back, so a node that is re-promoted is not treated as still drained.
	// +optional
	GatewayEvacuated bool `json:"gatewayEvacuated,omitempty"`

	// Leaving marks a node that is no longer selected, or no longer in the
	// cluster, and whose chassis registration has still to be deleted from the
	// Southbound database. Until that deletion lands the stale chassis keeps
	// claiming the ports of workloads that have moved elsewhere.
	// +optional
	Leaving bool `json:"leaving,omitempty"`
}

func init() {
	SchemeBuilder.Register(&OVNChassis{}, &OVNChassisList{})
}
