// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultNFSMountOptions is the mount option string rendered when an NFS
// backend or backup backend leaves mountOptions empty. It pins NFSv4.1, a soft
// mount with a 3-second timeout and two retransmits: a hard mount would block a
// cinder-volume process indefinitely on an unreachable server, taking the whole
// backend down with it, while the soft mount surfaces the failure as an I/O
// error the driver reports. It names the same literal the +kubebuilder:default
// markers on NFSBackendSpec.MountOptions and NFSBackupBackendSpec.MountOptions
// carry, which markers cannot reference from a Go constant.
const DefaultNFSMountOptions = "nfsvers=4.1,soft,timeo=30,retrans=2"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Cinder",type="string",JSONPath=".spec.cinderRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CinderBackend is the Schema for the cinderbackends API. One CR attaches to a
// Cinder CR via spec.cinderRef and describes one volume backend (Phase 1: NFS).
// The Cinder-side sub-reconciler projects a dedicated cinder-volume Deployment
// for each attached backend, so the backends a Cinder serves are added and
// removed without editing the Cinder CR.
//
// Detaching a backend is not a plain delete. A cinder-volume owns the volumes it
// created under its host identity, and a host no service backs leaves those
// volumes unreachable, so the controller holds the finalizer
// cinder.openstack.c5c3.io/service-remove on this CR: on deletion it runs a
// "cinder-manage service remove" Job for the backend's host and releases the
// finalizer only once that Job succeeds.
type CinderBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CinderBackendSpec   `json:"spec,omitempty"`
	Status CinderBackendStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CinderBackendList contains a list of CinderBackend.
type CinderBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CinderBackend `json:"items"`
}

// CinderBackendType enumerates the supported volume drivers. Phase 1 ships NFS.
// +kubebuilder:validation:Enum=NFS
type CinderBackendType string

const (
	// CinderBackendTypeNFS selects the Cinder NFS volume driver.
	CinderBackendTypeNFS CinderBackendType = "NFS"
)

// CinderBackendSpec defines the desired state of CinderBackend.
//
// The cinderRef transition rule (evaluated only on UPDATE) makes the attachment
// immutable: re-pointing a backend at a different Cinder would strand the
// volumes the old deployment created under a host identity nothing serves
// anymore. The type rule freezes the driver for the same reason: the volumes
// already on the backend were created by the driver that is being replaced.
// Delete and recreate instead, which runs the service-remove Job. The type/nfs
// union rule enforces "exactly one backend block matching spec.type" at the
// schema layer so it holds even when the validating webhook is down.
// +kubebuilder:validation:XValidation:rule="self.cinderRef.name == oldSelf.cinderRef.name",message="cinderRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="type is immutable"
// +kubebuilder:validation:XValidation:rule="(self.type == 'NFS') == has(self.nfs)",message="exactly one backend block matching spec.type must be set (type NFS requires spec.nfs)"
type CinderBackendSpec struct {
	// CinderRef names the Cinder CR in the same namespace this backend attaches
	// to. The referenced CR does not have to exist at admission time (GitOps
	// ordering: the backend may be applied before the Cinder CR); a dangling
	// reference surfaces as Ready=False.
	CinderRef CinderRefSpec `json:"cinderRef"`

	// Type selects the volume driver. Phase 1 supports NFS only.
	Type CinderBackendType `json:"type"`

	// NFS configures the NFS volume driver. Required exactly when type is NFS
	// (union rule above).
	// +optional
	NFS *NFSBackendSpec `json:"nfs,omitempty"`

	// ImageVolumeCache turns on the per-backend image-volume cache: the first
	// volume created from a given image is kept and later requests for the same
	// image clone it on the backend instead of pulling the image through Glance
	// again. It needs spec.internalTenant on the referenced Cinder, since the
	// cached volumes are owned by the deployment rather than by the requesting
	// tenant.
	// +optional
	ImageVolumeCache *ImageVolumeCacheSpec `json:"imageVolumeCache,omitempty"`

	// ExtraOptions provides free-form [<name>] backend-section options not
	// covered by the typed fields, keyed by bare option name. The validating
	// webhook rejects options already owned by the typed fields (a denylist) so
	// the escape hatch cannot silently contradict the typed spec.
	//
	// MaxProperties and the per-entry key/value length bound the aggregate
	// rendered config at admission so this free-form map cannot bloat the backend
	// section past reasonable size.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 256 && size(self[k]) <= 1024)",message="each extraOptions key must be <=256 characters and each value <=1024 characters"
	// +optional
	ExtraOptions map[string]string `json:"extraOptions,omitempty"`
}

// CinderRefSpec references a Cinder CR by name in the same namespace. Modeled as
// a dedicated struct (rather than corev1.LocalObjectReference) so the name
// carries the same MinLength schema guard as the shared commonv1.SecretRefSpec.
// The reference is inverted attachment: the backend points at the Cinder, not the
// other way round, so backends can be added and removed without editing the
// Cinder CR.
type CinderRefSpec struct {
	// Name is the referenced Cinder CR's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// NFSBackendSpec configures the Cinder NFS volume driver for one backend. The
// cinder-volume pod mounts the export and keeps one file per volume on it.
type NFSBackendSpec struct {
	// Server is the NFS server the export lives on, as a hostname or an IP
	// address. It reaches the mount command verbatim, which is why the pattern
	// admits only the characters a hostname or an IPv4 address carries.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9.-]+$`
	Server string `json:"server"`

	// Path is the absolute export path on the server. The pattern admits only
	// printable ASCII because the operator writes "server:path" verbatim into the
	// nfs_shares_config file the cinder-volume pod mounts, and the driver reads
	// that file back a line at a time, stripping it and cutting it at the first
	// space to separate the export from its per-share options: a space or a tab
	// in the path would leave cinder addressing a shorter export than the one
	// validated, mounted and given an egress rule, at a mount point derived from
	// that shorter value. A newline would add a second export line outright. The
	// allowlist is what covers the whitespace an \s-based pattern would not: RE2
	// resolves \s to the five ASCII characters, while the python side strips the
	// full Unicode set, so a pasted U+00A0 would divide the two.
	// +kubebuilder:validation:Pattern=`^/[!-~]*$`
	Path string `json:"path"`

	// MountOptions is the comma-separated option string the export is mounted
	// with. The default pins NFSv4.1 and a soft mount (see
	// DefaultNFSMountOptions); override it only where the server demands
	// different semantics, and keep the mount soft, because a hard mount blocks
	// the cinder-volume process on an unreachable server rather than failing the
	// request. The pattern excludes a newline and a carriage return: the value is
	// rendered verbatim as nfs_mount_options, where the reconcile-time guard would
	// drop the whole backend out of the projection rather than reject the edit.
	// +optional
	// +kubebuilder:validation:Pattern=`^[^\n\r]*$`
	// +kubebuilder:default="nfsvers=4.1,soft,timeo=30,retrans=2"
	MountOptions string `json:"mountOptions,omitempty"`
}

// ImageVolumeCacheSpec bounds the per-backend image-volume cache. The cache
// trades backend capacity for create-volume-from-image latency: a cached image
// volume is cloned on the backend, which skips the download through Glance
// entirely.
//
// Both bounds are optional and independent: whichever is reached first evicts
// the least recently used entry. Leaving both unset caches without a bound,
// which is a deliberate choice only where the backend has capacity to spare.
type ImageVolumeCacheSpec struct {
	// Enabled turns the cache on for this backend. It is an explicit field
	// rather than the presence of the block so the bounds can be configured in a
	// CR that keeps the cache off.
	Enabled bool `json:"enabled"`

	// MaxSizeGB caps the total size, in GiB, of the cached image volumes on this
	// backend. When unset the cache is not bounded by size.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxSizeGB *int32 `json:"maxSizeGB,omitempty"`

	// MaxCount caps the number of cached image volumes on this backend. When
	// unset the cache is not bounded by count.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxCount *int32 `json:"maxCount,omitempty"`
}

// CinderBackendStatus defines the observed state of CinderBackend. The dedicated
// CinderBackend controller is the single writer of this status; the Cinder-side
// sub-reconciler only reads it and writes an aggregated condition onto the
// Cinder CR instead.
type CinderBackendStatus struct {
	// Conditions represent the latest available observations of the backend
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
}

func init() {
	SchemeBuilder.Register(&CinderBackend{}, &CinderBackendList{})
}
