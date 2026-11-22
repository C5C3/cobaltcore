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

// DefaultRevalidatorThreads is the ovs-vswitchd revalidator thread count
// rendered when spec.ovs.revalidatorThreads is unset.
const DefaultRevalidatorThreads int32 = 2

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

	// Jobs sizes and prioritizes the pods of the maintenance Jobs: apply,
	// evacuate and chassis-del, and on a chassis with spec.fabric the route
	// probe. Unset resources default to a 70m CPU request and 368Mi memory as
	// request and limit, and an unset priority class renders none. The Jobs
	// take spec.tolerations. The block has no placement fields: the apply Job
	// is pinned to its node, and a node selector that node does not match
	// fails the pinned pod's kubelet admission.
	// +optional
	Jobs *commonv1.JobBaseSpec `json:"jobs,omitempty"`

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

	// Fabric attaches the selected nodes to the BGP fabric. Every node with an
	// entry in fabric.nodes gets the fabric VRF, the fabric loopback and one
	// OVS peer bridge per dedicated uplink, and the port mapping ovn-controller
	// reads to tie a router port to a peer bridge. A node that also carries
	// the label openstack.c5c3.io/frr-k8s=true gets an FRRConfiguration in
	// frr-k8s-system. The router and port options that make OVN advertise or
	// learn routes are rows of the Northbound database, which the CMS writes;
	// this block covers the chassis side only. When nil no node is attached.
	// +optional
	Fabric *OVNFabricSpec `json:"fabric,omitempty"`

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

	// OVS tunes the ovs-vswitchd container: its resources and its revalidator
	// thread count. When nil the operator renders no requests or limits for the
	// container and pins DefaultRevalidatorThreads revalidators. The local
	// ovsdb-server container beside it takes none from any field.
	// +optional
	OVS *OVNChassisOVSSpec `json:"ovs,omitempty"`

	// Controller tunes the ovn-controller container. When nil the operator
	// renders no requests or limits for the container.
	// +optional
	Controller *OVNChassisContainerSpec `json:"controller,omitempty"`

	// VerticalAutoscaling opts both chassis DaemonSets, Open vSwitch and
	// ovn-controller, into a VerticalPodAutoscaler each that controls the
	// requests of their containers. On a cluster that does not serve
	// autoscaling.k8s.io/v1 VerticalPodAutoscaler, the CR reports
	// VPAReady=False with reason VPANotInstalled.
	// +optional
	VerticalAutoscaling *VerticalAutoscalingSpec `json:"verticalAutoscaling,omitempty"`

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

// OVNFabricSpec attaches a class of chassis nodes to the BGP fabric. The node
// network stays untouched: BGP runs unnumbered over dedicated uplinks inside a
// VRF of its own, and the uplinks belong to OVS peer bridges.
type OVNFabricSpec struct {
	// VRF is the Linux VRF that holds the uplinks, the loopback and the BGP
	// sessions (contract clause C10). Keeping them out of the main table is
	// what leaves the node network untouched.
	// +optional
	// +kubebuilder:default={}
	VRF OVNFabricVRFSpec `json:"vrf,omitempty"`

	// Uplinks are the dedicated fabric uplinks, the same on every selected
	// node. Each NIC is taken into an OVS peer bridge, and FRR peers over the
	// bridge's own interface.
	// +listType=map
	// +listMapKey=interface
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	Uplinks []OVNFabricUplink `json:"uplinks"`

	// MTU is the MTU of every uplink and peer bridge (contract clause C6).
	// The leaves set the same MTU on their node ports, so the two ends of a
	// fabric link agree on it.
	// +optional
	// +kubebuilder:default=9000
	// +kubebuilder:validation:Minimum=1280
	// +kubebuilder:validation:Maximum=9216
	MTU int32 `json:"mtu,omitempty"`

	// Nodes is the per-node table the network team owns: the private ASN and
	// the fabric loopbacks of each node. A selected node without an entry
	// stays unattached, and FabricReady names it.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=1000
	Nodes []OVNFabricNode `json:"nodes,omitempty"`

	// AnnounceableRanges are the ranges a node may announce host routes from,
	// besides its own loopback (contract clauses C4 and C8). A route OVN
	// installs outside them never leaves the node, because the node's outbound
	// prefix lists permit nothing else.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Format=cidr
	AnnounceableRanges []string `json:"announceableRanges,omitempty"`

	// AdvertiseVRFIDs are the dynamic-routing-vrf-id values of every logical
	// router whose routes the node announces. ovn-controller installs those
	// routes into the VRF ovnvrf<id>, and the fabric router imports it.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=4294967295
	AdvertiseVRFIDs []int64 `json:"advertiseVRFIDs,omitempty"`

	// LearnVRFIDs are the dynamic-routing-vrf-id values of every logical
	// router that learns the leaf defaults. Their VRF ovnvrf<id> imports the
	// fabric VRF, and ovn-controller reads the learned routes from it.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=4294967295
	LearnVRFIDs []int64 `json:"learnVRFIDs,omitempty"`

	// BFD carries the BFD timers of every fabric session (contract clause C5).
	// +optional
	// +kubebuilder:default={}
	BFD OVNFabricBFDSpec `json:"bfd,omitempty"`
}

// OVNFabricVRFSpec names the fabric VRF device and its routing table.
type OVNFabricVRFSpec struct {
	// Name is the VRF device. It is bounded to 15 characters because the
	// device is a Linux interface and the kernel's IFNAMSIZ leaves 15 usable
	// bytes.
	// +optional
	// +kubebuilder:default=fabric
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_.-]{1,15}$`
	Name string `json:"name,omitempty"`

	// Table is the routing table of the VRF. It must not be one of the
	// kernel's own tables or a table ovn-controller uses for ovnvrf<id>.
	// +optional
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4294967295
	Table int64 `json:"table,omitempty"`
}

// OVNFabricUplink ties one fabric NIC to the OVS peer bridge that takes it.
type OVNFabricUplink struct {
	// Interface is the NIC on the node. It carries link-local addresses only
	// (contract clause C2); the node preparation refuses one with a global
	// address.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_.-]{1,15}$`
	Interface string `json:"interface"`

	// Bridge is the OVS peer bridge that takes the NIC. FRR peers over the
	// bridge's own interface, which the node preparation enslaves to the
	// fabric VRF, and its name is the port name a router port names in
	// dynamic-routing-port-name.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_.-]{1,15}$`
	Bridge string `json:"bridge"`
}

// OVNFabricNode is one row of the per-node fabric table.
type OVNFabricNode struct {
	// Name is the Node's name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// ASN is the node's private 4-byte ASN (contract clause C3). The operator
	// uses it in the generated FRRConfiguration; on a node with a host FRR it
	// records the value the host stanza carries.
	// +kubebuilder:validation:Minimum=4200000000
	// +kubebuilder:validation:Maximum=4294967294
	ASN int64 `json:"asn"`

	// LoopbackIPv4 is the /32 fabric loopback, which is also the BGP router
	// id (contract clause C2).
	// +kubebuilder:validation:Format=ipv4
	LoopbackIPv4 string `json:"loopbackIPv4"`

	// LoopbackIPv6 is the /128 fabric loopback.
	// +kubebuilder:validation:Format=ipv6
	LoopbackIPv6 string `json:"loopbackIPv6"`
}

// OVNFabricBFDSpec carries the BFD timers of the fabric sessions. The bounds
// are frr-k8s's own.
type OVNFabricBFDSpec struct {
	// ReceiveIntervalMs is the minimum interval at which this node accepts
	// BFD control packets.
	// +optional
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=60000
	ReceiveIntervalMs int32 `json:"receiveIntervalMs,omitempty"`

	// TransmitIntervalMs is the minimum interval at which this node sends BFD
	// control packets.
	// +optional
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=60000
	TransmitIntervalMs int32 `json:"transmitIntervalMs,omitempty"`

	// DetectMultiplier is how many control packets may be lost before the
	// session is declared down.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=255
	DetectMultiplier int32 `json:"detectMultiplier,omitempty"`
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

// OVNChassisOVSSpec tunes the ovs-vswitchd container of the OVS DaemonSet.
type OVNChassisOVSSpec struct {
	// OVNChassisContainerSpec carries the container's resources.
	OVNChassisContainerSpec `json:",inline"`

	// RevalidatorThreads is the number of revalidator threads ovs-vswitchd
	// runs. It defaults to 2 and is set as other_config:n-revalidator-threads
	// in the node's Open_vSwitch table before the daemon starts. Unset, OVS
	// starts one revalidator per four handler threads plus one, which is one
	// per four CPUs plus one, so the pod's footprint would follow the node it
	// lands on. The handler threads cannot be pinned: under the kernel
	// datapath's per-CPU upcall dispatch OVS starts one per core in the
	// process's CPU affinity and overrides other_config:n-handler-threads, so
	// only a cpuset bounds them.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RevalidatorThreads *int32 `json:"revalidatorThreads,omitempty"`
}

// OVNChassisContainerSpec tunes one chassis container: ovs-vswitchd in the OVS
// DaemonSet (spec.ovs) or ovn-controller in the controller DaemonSet
// (spec.controller).
type OVNChassisContainerSpec struct {
	// Resources defines the CPU and memory requests and limits for the
	// container and for the init container that prepares it: host-prepare in
	// the OVS DaemonSet, apply-node in the controller DaemonSet. When nil the
	// operator renders none, unless a LimitRange in the namespace fills them
	// in: what a datapath needs depends on the traffic the node carries, so no
	// default fits most hardware.
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

	// ClientSecretName names the Secret holding the OVN client identity (tls.crt,
	// tls.key and ca.crt) in this CR's namespace on the cluster this CR projects
	// onto. The chassis pods mount it, and so does a NeutronMetadataAgent
	// attached to this chassis. It is the OVNCentral's own client Secret when the
	// two CRs project onto the same cluster, and <name>-ovn-client, the copy the
	// operator writes, when they do not.
	// +optional
	ClientSecretName string `json:"clientSecretName,omitempty"`

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

	// Fabric reports that the node may carry the devices of a fabric
	// assignment. It is set from the first pass that renders an assignment for
	// the node, because the node's fabric-node init container can apply that
	// entry before any apply Job runs, and only the apply Job that tore the
	// devices down clears it. While it is set and the node's entry carries no
	// assignment, the node gets that Job.
	// +optional
	Fabric bool `json:"fabric,omitempty"`

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
