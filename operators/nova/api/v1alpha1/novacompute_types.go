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

// MaxNovaComputeNameLength bounds metadata.name by the label it becomes: the
// app.kubernetes.io/instance value of every child and, on a target cluster, the
// openstack.c5c3.io/owner-name value. Kubernetes caps a label value at 63
// characters.
const MaxNovaComputeNameLength = 63

// ComputeConfigMirrorLabel marks a Secret the ControlPlane copied into a
// compute cluster's namespace: the compute contract "<nova>-compute-config"
// and, when the ControlPlane provisions the hypervisor operator's account, its
// auth Secret "<nova>" plus HypervisorOperatorAuthSecretSuffix. The last
// NovaCompute of a Nova on that cluster deletes each of the two carrying it
// when it is torn down, and leaves a Secret without it alone: that one was put
// there by hand.
const ComputeConfigMirrorLabel = "nova.openstack.c5c3.io/compute-config-mirror"

// HypervisorOperatorAuthSecretSuffix completes the name of the Secret the
// ControlPlane writes for openstack-hypervisor-operator: "<nova>" plus this
// suffix, beside the Nova and on every compute cluster a NovaCompute of it runs
// on. The reap of the last pool on a cluster derives the same name from the
// Nova's name alone.
//
// #nosec G101 -- Secret name suffix, not a credential.
const HypervisorOperatorAuthSecretSuffix = "-hypervisor-operator-auth"

// NovaComputeNodePhase is where one node stands in the pool's lifecycle.
// +kubebuilder:validation:Enum=Pending;Active;Draining;Releasing;Conflict
type NovaComputeNodePhase string

// The node phases.
const (
	// NovaComputeNodePending is a selected node whose nova-compute has not
	// registered a compute service under the node name yet.
	NovaComputeNodePending NovaComputeNodePhase = "Pending"
	// NovaComputeNodeActive is a selected node with a registered service.
	NovaComputeNodeActive NovaComputeNodePhase = "Active"
	// NovaComputeNodeDraining is a node that left the pool and keeps its pod
	// while Nova still reports instances on it. Its service is disabled.
	NovaComputeNodeDraining NovaComputeNodePhase = "Draining"
	// NovaComputeNodeReleasing is a node with no instances left, whose pod has
	// been released and whose compute service is deleted once the pod is gone.
	NovaComputeNodeReleasing NovaComputeNodePhase = "Releasing"
	// NovaComputeNodeConflict is a selected node another NovaCompute of the same
	// Nova already holds. It runs no pod of this CR.
	NovaComputeNodeConflict NovaComputeNodePhase = "Conflict"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=novacomputes
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".status.desiredNumberScheduled"
// +kubebuilder:printcolumn:name="Ready pods",type="integer",JSONPath=".status.numberReady"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NovaCompute is the Schema for the novacomputes API. It runs nova-compute on
// one node pool of a compute cluster: a DaemonSet on the nodes its nodeSelector
// matches, joined to the Nova the CR names through that Nova's compute
// contract.
//
// One NovaCompute covers one pool. Nodes with different libvirt settings get a
// second CR rather than a wider selector, because everything below the
// selector applies uniformly to every node it matches. Every pool of a Nova
// maps into that Nova's single cell.
type NovaCompute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NovaComputeSpec   `json:"spec,omitempty"`
	Status NovaComputeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NovaComputeList contains a list of NovaCompute.
type NovaComputeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NovaCompute `json:"items"`
}

// NovaComputeSpec defines the desired state of NovaCompute.
//
// The targetClusterRef transition rules (evaluated only on UPDATE) freeze the
// ref, and the novaRef rule freezes the control plane the pool joins. Both are
// enforced at the schema layer, so the guarantee holds even when the validating
// webhook is down: a pool moved to another cluster or another Nova leaves its
// compute services registered where it was.
// +kubebuilder:validation:XValidation:rule="has(self.targetClusterRef) == has(oldSelf.targetClusterRef)",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.targetClusterRef) || !has(oldSelf.targetClusterRef) || self.targetClusterRef.name == oldSelf.targetClusterRef.name",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.novaRef.name == oldSelf.novaRef.name",message="novaRef is immutable"
type NovaComputeSpec struct {
	// NovaRef names the Nova in the same namespace this pool joins. The pods
	// mount that Nova's compute contract, and the node services are registered
	// with its API. It is immutable.
	NovaRef NovaRef `json:"novaRef"`

	// NodeSelector picks the nodes of the pool. It is required and must carry
	// at least one label: an empty selector matches every node in the cluster.
	// It may change, and a node that stops matching is drained before its pod
	// goes: it keeps nova-compute until Nova reports no instance on it.
	// +kubebuilder:validation:MinProperties=1
	NodeSelector map[string]string `json:"nodeSelector"`

	// Tolerations let the pods run on tainted nodes. The webhook rejects a
	// toleration of kvm.cloud.sap/offboarding:NoExecute without
	// tolerationSeconds: openstack-hypervisor-operator deletes a node's compute
	// service only once every agent pod on it is gone.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Image is the nova-compute image. When nil the operator runs
	// ghcr.io/c5c3/nova-compute tagged with the referenced Nova's
	// status.installedRelease, so the pool follows the control plane once that
	// has upgraded and not before.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// Libvirt carries the [libvirt] options the pool renders.
	// +optional
	// +kubebuilder:default={}
	Libvirt NovaComputeLibvirtSpec `json:"libvirt,omitempty"`

	// UpdateStrategy paces the DaemonSet rollout.
	// +optional
	// +kubebuilder:default={}
	UpdateStrategy NovaComputeUpdateStrategy `json:"updateStrategy,omitempty"`

	// Resources defines the CPU and memory requests and limits of the
	// nova-compute container. When nil the container carries none.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// ExtraConfig provides free-form INI sections merged over the pool's
	// rendered config. It is the per-pool override surface. The keys the pods
	// take from their environment or their mounts are rejected at admission.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`

	// TargetClusterRef selects the registered target cluster the DaemonSet is
	// created on. When nil it is created on the cluster the operator runs in.
	// The ref is immutable once set.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`
}

// NovaRef names a Nova in the NovaCompute's own namespace. The reference is
// namespace-local because the pods mount the compute contract Secret, which is
// published (or mirrored) into that namespace.
type NovaRef struct {
	// Name is the Nova's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// NovaComputeLibvirtSpec carries the [libvirt] options the pool renders.
// +kubebuilder:validation:XValidation:rule="(has(self.cpuMode) && self.cpuMode == 'custom') == (has(self.cpuModels) && size(self.cpuModels) > 0)",message="cpuModels is required when cpuMode is custom and must be empty otherwise"
type NovaComputeLibvirtSpec struct {
	// VirtType is [libvirt] virt_type. qemu runs guests without hardware
	// acceleration and is for nodes without KVM.
	// +kubebuilder:default=kvm
	// +kubebuilder:validation:Enum=kvm;qemu
	// +optional
	VirtType string `json:"virtType,omitempty"`

	// CPUMode is [libvirt] cpu_mode. When empty the key is not rendered and
	// Nova's own default applies.
	// +kubebuilder:validation:Enum=host-model;host-passthrough;custom;none
	// +optional
	CPUMode string `json:"cpuMode,omitempty"`

	// CPUModels is [libvirt] cpu_models, rendered comma-joined. It is required
	// with cpuMode custom and must be empty otherwise.
	// +optional
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9_.-]+$`
	CPUModels []string `json:"cpuModels,omitempty"`

	// ImagesType is [libvirt] images_type. When empty the key is not rendered.
	// The rbd, lvm and ploop backends are not offered: the compute image
	// carries neither a Ceph client nor lvm2.
	// +kubebuilder:validation:Enum=default;qcow2;raw;flat
	// +optional
	ImagesType string `json:"imagesType,omitempty"`
}

// NovaComputeUpdateStrategy paces the DaemonSet rollout. It is a narrowed
// DaemonSetUpdateStrategy: maxSurge has no counterpart here, because two
// nova-compute processes on one node would share one libvirt.
type NovaComputeUpdateStrategy struct {
	// Type selects the rollout mode. OnDelete hands the pace to whoever drains
	// the nodes. Every change of the pool's node set rolls the pods under
	// RollingUpdate, so a pool that must not restart nova-compute during a
	// migration takes OnDelete.
	// +optional
	// +kubebuilder:default=RollingUpdate
	// +kubebuilder:validation:Enum=RollingUpdate;OnDelete
	Type string `json:"type,omitempty"`

	// MaxUnavailable is how many nodes may restart nova-compute at once. It
	// applies to RollingUpdate only; the validating webhook rejects it alongside
	// OnDelete. When nil the operator renders 1.
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// NovaComputeStatus defines the observed state of NovaCompute.
type NovaComputeStatus struct {
	// Conditions represent the latest available observations of the NovaCompute
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

	// InstalledImage is the image reference the running DaemonSet was projected
	// from. It is stamped once the rollout is complete.
	// +optional
	InstalledImage string `json:"installedImage,omitempty"`

	// DesiredNumberScheduled is how many nodes the DaemonSet should run on,
	// mirrored from the DaemonSet status.
	// +optional
	DesiredNumberScheduled int32 `json:"desiredNumberScheduled,omitempty"`

	// NumberReady is how many of those nodes have a ready nova-compute pod.
	// +optional
	NumberReady int32 `json:"numberReady,omitempty"`

	// Nodes is the per-node ledger of the pool. A node that stops being
	// selected keeps its entry until its compute service is deleted, so the
	// record of a drain outlives the selection.
	// +optional
	// +listType=map
	// +listMapKey=name
	Nodes []NovaComputeNodeStatus `json:"nodes,omitempty"`
}

// NovaComputeNodeStatus is the state of one node of the pool.
type NovaComputeNodeStatus struct {
	// Name is the node's name, which is also the compute service's host.
	Name string `json:"name"`

	// Phase is where the node stands in the pool's lifecycle.
	Phase NovaComputeNodePhase `json:"phase"`

	// Zone is the node's topology.kubernetes.io/zone label, the availability
	// zone its host aggregate carries.
	// +optional
	Zone string `json:"zone,omitempty"`

	// ServiceID is the id of the nova-compute service registered under the
	// node name.
	// +optional
	ServiceID string `json:"serviceID,omitempty"`

	// ServiceStatus is the service's administrative status as Nova reports it:
	// enabled or disabled.
	// +optional
	ServiceStatus string `json:"serviceStatus,omitempty"`

	// ServiceState is the service's liveness as Nova reports it: up or down.
	// +optional
	ServiceState string `json:"serviceState,omitempty"`

	// DisabledReason is the reason Nova records for a disabled service.
	// +optional
	DisabledReason string `json:"disabledReason,omitempty"`

	// Instances is how many servers Nova reports on a draining node.
	// +optional
	Instances *int32 `json:"instances,omitempty"`

	// ConflictsWith names the NovaCompute that holds a node in Conflict.
	// +optional
	ConflictsWith string `json:"conflictsWith,omitempty"`
}

func init() {
	SchemeBuilder.Register(&NovaCompute{}, &NovaComputeList{})
}
