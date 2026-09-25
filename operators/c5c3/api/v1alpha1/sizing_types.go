// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// The godoc on the sizing types below is kept to a line or two on purpose:
// the inline structs are copied into the ControlPlane CRD at every embedding
// site, so each description is repeated some thirty times in the schema.

// SizingProfileName names a built-in sizing profile.
// +kubebuilder:validation:Enum=Minimal;Standard
type SizingProfileName string

const (
	// SizingProfileMinimal sizes every component for a single small node.
	SizingProfileMinimal SizingProfileName = "Minimal"
	// SizingProfileStandard reproduces the replica counts and the database
	// volume size a ControlPlane without sizing always had, and sets nothing
	// else.
	SizingProfileStandard SizingProfileName = "Standard"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Base",type="string",JSONPath=".spec.base"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SizingProfile is a site-defined sizing profile: a built-in base profile
// overlaid by the values in its spec. A ControlPlane selects it with
// spec.sizing.profileRef and follows every later edit of it.
type SizingProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec holds the base profile and the values that override it.
	// +optional
	Spec SizingProfileSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// SizingProfileList contains a list of SizingProfile.
type SizingProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SizingProfile `json:"items"`
}

// SizingProfileSpec is a built-in base profile plus the values that override
// it, one value at a time.
type SizingProfileSpec struct {
	// Base is the built-in profile the values of this profile override.
	// +kubebuilder:default=Standard
	// +optional
	Base SizingProfileName `json:"base,omitempty"`

	SizingSpec `json:",inline"`
}

// ControlPlaneSizingSpec sizes and places every component the ControlPlane
// creates. The effective sizing is a built-in profile, overlaid by the
// referenced SizingProfile, overlaid by the values set here.
// +kubebuilder:validation:XValidation:rule="!(has(self.profile) && has(self.profileRef))",message="profile and profileRef are mutually exclusive"
type ControlPlaneSizingSpec struct {
	// Profile selects a built-in profile. Unset means Standard unless
	// profileRef is set.
	// +optional
	Profile SizingProfileName `json:"profile,omitempty"`

	// ProfileRef selects a cluster-scoped SizingProfile.
	// +optional
	ProfileRef *SizingProfileRef `json:"profileRef,omitempty"`

	SizingSpec `json:",inline"`
}

// SizingProfileRef names a cluster-scoped SizingProfile.
type SizingProfileRef struct {
	// Name is the name of the SizingProfile.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// PodPlacementSpec picks the nodes a component's pods run on.
type PodPlacementSpec struct {
	// NodeSelector restricts the pods to nodes carrying every listed label.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations let the pods onto nodes with matching taints.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// PriorityClassName sets the pods' priority class; "" sets none.
	// +optional
	PriorityClassName *string `json:"priorityClassName,omitempty"`
}

// SpreadConstraintSpec spreads a component's pods across a topology; the
// ControlPlane adds the pod selector.
type SpreadConstraintSpec struct {
	// MaxSkew is the largest allowed pod-count difference between domains.
	// +kubebuilder:validation:Minimum=1
	MaxSkew int32 `json:"maxSkew"`

	// TopologyKey is the node label that defines a domain.
	// +kubebuilder:validation:MinLength=1
	TopologyKey string `json:"topologyKey"`

	// WhenUnsatisfiable is DoNotSchedule or ScheduleAnyway.
	// +kubebuilder:validation:Enum=DoNotSchedule;ScheduleAnyway
	WhenUnsatisfiable corev1.UnsatisfiableConstraintAction `json:"whenUnsatisfiable"`
}

// ContainerSizingSpec sizes a component's container.
type ContainerSizingSpec struct {
	// Resources are the container's requests and limits, merged per resource.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// PinnedSizingSpec sizes and places a component with a fixed replica count.
type PinnedSizingSpec struct {
	ContainerSizingSpec `json:",inline"`
	PodPlacementSpec    `json:",inline"`
}

// ScaledSizingSpec sizes and places a component with a replica count.
type ScaledSizingSpec struct {
	// Replicas is the pod count.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	PinnedSizingSpec `json:",inline"`
}

// DeploymentSizingSpec sizes, places and spreads a Deployment.
type DeploymentSizingSpec struct {
	ScaledSizingSpec `json:",inline"`

	// SpreadConstraints replace the Deployment's default spread.
	// +optional
	SpreadConstraints []SpreadConstraintSpec `json:"spreadConstraints,omitempty"`
}

// ProcessSizingSpec sets the uWSGI process and thread counts.
type ProcessSizingSpec struct {
	// Processes is the uWSGI process count.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Processes *int32 `json:"processes,omitempty"`

	// Threads is the thread count per uWSGI process.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Threads *int32 `json:"threads,omitempty"`
}

// APISizingSpec sizes a service API Deployment.
type APISizingSpec struct {
	DeploymentSizingSpec `json:",inline"`
	ProcessSizingSpec    `json:",inline"`

	// Autoscaling adds a HorizontalPodAutoscaler; it replaces as a whole.
	// +optional
	Autoscaling *commonv1.AutoscalingSpec `json:"autoscaling,omitempty"`
}

// HorizonAPISizingSpec sizes the dashboard Deployment.
type HorizonAPISizingSpec struct {
	DeploymentSizingSpec `json:",inline"`

	// Autoscaling adds a HorizontalPodAutoscaler; it replaces as a whole.
	// +optional
	Autoscaling *commonv1.AutoscalingSpec `json:"autoscaling,omitempty"`
}

// MetadataAPISizingSpec sizes the Nova metadata API Deployment.
type MetadataAPISizingSpec struct {
	DeploymentSizingSpec `json:",inline"`
	ProcessSizingSpec    `json:",inline"`
}

// WorkerSizingSpec sizes a Deployment of RPC workers.
type WorkerSizingSpec struct {
	DeploymentSizingSpec `json:",inline"`

	// Workers is the worker process count per pod.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Workers *int32 `json:"workers,omitempty"`
}

// JobSizingSpec sizes and prioritizes a service's Job pods.
type JobSizingSpec struct {
	ContainerSizingSpec `json:",inline"`

	// PriorityClassName sets the Job pods' priority class; "" sets none.
	// +optional
	PriorityClassName *string `json:"priorityClassName,omitempty"`
}

// KeystoneSizingSpec sizes the Keystone components.
type KeystoneSizingSpec struct {
	// API sizes the API Deployment.
	// +optional
	API *APISizingSpec `json:"api,omitempty"`
	// Jobs sizes the Job pods.
	// +optional
	Jobs *JobSizingSpec `json:"jobs,omitempty"`
	// FederationProxy sizes the federation proxy sidecar.
	// +optional
	FederationProxy *ContainerSizingSpec `json:"federationProxy,omitempty"`
}

// HorizonSizingSpec sizes the Horizon components.
type HorizonSizingSpec struct {
	// API sizes the dashboard Deployment.
	// +optional
	API *HorizonAPISizingSpec `json:"api,omitempty"`
}

// APIServiceSizingSpec sizes a service that runs an API and Jobs.
type APIServiceSizingSpec struct {
	// API sizes the API Deployment.
	// +optional
	API *APISizingSpec `json:"api,omitempty"`
	// Jobs sizes the Job pods.
	// +optional
	Jobs *JobSizingSpec `json:"jobs,omitempty"`
}

// NeutronSizingSpec sizes the Neutron components.
type NeutronSizingSpec struct {
	// API sizes the API Deployment.
	// +optional
	API *APISizingSpec `json:"api,omitempty"`
	// Workers sizes both RPC worker Deployments.
	// +optional
	Workers *ScaledSizingSpec `json:"workers,omitempty"`
	// Jobs sizes the Job pods.
	// +optional
	Jobs *JobSizingSpec `json:"jobs,omitempty"`
}

// CinderSizingSpec sizes the Cinder components.
type CinderSizingSpec struct {
	// API sizes the API Deployment.
	// +optional
	API *APISizingSpec `json:"api,omitempty"`
	// Scheduler sizes the scheduler Deployment.
	// +optional
	Scheduler *DeploymentSizingSpec `json:"scheduler,omitempty"`
	// Volume sizes every volume Deployment.
	// +optional
	Volume *PinnedSizingSpec `json:"volume,omitempty"`
	// Backup sizes the backup Deployment.
	// +optional
	Backup *PinnedSizingSpec `json:"backup,omitempty"`
	// Jobs sizes the Job pods.
	// +optional
	Jobs *JobSizingSpec `json:"jobs,omitempty"`
}

// NovaSizingSpec sizes the Nova components.
type NovaSizingSpec struct {
	// API sizes the API Deployment.
	// +optional
	API *APISizingSpec `json:"api,omitempty"`
	// Metadata sizes the metadata API Deployment.
	// +optional
	Metadata *MetadataAPISizingSpec `json:"metadata,omitempty"`
	// Scheduler sizes the scheduler Deployment.
	// +optional
	Scheduler *WorkerSizingSpec `json:"scheduler,omitempty"`
	// Conductor sizes the conductor Deployment.
	// +optional
	Conductor *WorkerSizingSpec `json:"conductor,omitempty"`
	// ConsoleProxy sizes the console-proxy Deployment.
	// +optional
	ConsoleProxy *DeploymentSizingSpec `json:"consoleProxy,omitempty"`
	// Jobs sizes the Job pods.
	// +optional
	Jobs *JobSizingSpec `json:"jobs,omitempty"`
}

// DatabaseSizingSpec sizes every managed MariaDB.
type DatabaseSizingSpec struct {
	// Replicas is the MariaDB replica count, frozen at creation.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// StorageSize is the volume size per replica, frozen at creation.
	// +kubebuilder:validation:Pattern=`^[0-9]+(Mi|Gi|Ti)$`
	// +optional
	StorageSize string `json:"storageSize,omitempty"`

	PinnedSizingSpec `json:",inline"`
}

// CacheSizingSpec sizes every managed Memcached.
type CacheSizingSpec struct {
	// Replicas is the Memcached replica count.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	ContainerSizingSpec `json:",inline"`
}

// SizingSpec sizes and places every component a ControlPlane creates. The
// top-level placement is the fallback of every component that sets none.
type SizingSpec struct {
	PodPlacementSpec `json:",inline"`

	// Database sizes every managed MariaDB.
	// +optional
	Database *DatabaseSizingSpec `json:"database,omitempty"`
	// Cache sizes every managed Memcached.
	// +optional
	Cache *CacheSizingSpec `json:"cache,omitempty"`
	// Messaging sizes the managed RabbitmqCluster.
	// +optional
	Messaging *ScaledSizingSpec `json:"messaging,omitempty"`
	// SecretStore sizes the dedicated OpenBao of Barbican.
	// +optional
	SecretStore *ContainerSizingSpec `json:"secretStore,omitempty"`
	// Keystone sizes the Keystone components.
	// +optional
	Keystone *KeystoneSizingSpec `json:"keystone,omitempty"`
	// Horizon sizes the Horizon components.
	// +optional
	Horizon *HorizonSizingSpec `json:"horizon,omitempty"`
	// Glance sizes the Glance components.
	// +optional
	Glance *APIServiceSizingSpec `json:"glance,omitempty"`
	// Placement sizes the Placement components.
	// +optional
	Placement *APIServiceSizingSpec `json:"placement,omitempty"`
	// Barbican sizes the Barbican components.
	// +optional
	Barbican *APIServiceSizingSpec `json:"barbican,omitempty"`
	// Neutron sizes the Neutron components.
	// +optional
	Neutron *NeutronSizingSpec `json:"neutron,omitempty"`
	// Cinder sizes the Cinder components.
	// +optional
	Cinder *CinderSizingSpec `json:"cinder,omitempty"`
	// Nova sizes the Nova components.
	// +optional
	Nova *NovaSizingSpec `json:"nova,omitempty"`
}

func init() {
	SchemeBuilder.Register(&SizingProfile{}, &SizingProfileList{})
}
