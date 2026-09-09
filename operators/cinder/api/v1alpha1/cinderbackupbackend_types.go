// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Bounds and defaults for the backup chunk size. They name the same literals the
// +kubebuilder markers on NFSBackupBackendSpec.FileSize and .Compression carry,
// which markers cannot reference from a Go constant.
const (
	// DefaultBackupFileSize is the backup_file_size rendered when a backup
	// backend leaves fileSize unset: 50 MiB, upstream cinder's own default.
	DefaultBackupFileSize int64 = 52428800
	// MinBackupFileSize is the floor on backup_file_size, one MiB. A chunk
	// smaller than that turns a large volume into a backup of tens of thousands
	// of objects, each with its own round trip.
	MinBackupFileSize int64 = 1048576
	// BackupFileSizeMultiple is the granularity backup_file_size has to be a
	// multiple of: cinder hashes each backup chunk in blocks of
	// backup_sha_block_size_bytes, 32 KiB, and refuses a chunk size the block
	// size does not divide.
	BackupFileSizeMultiple int64 = 32768
	// DefaultBackupCompression is the backup_compression_algorithm rendered when
	// a backup backend leaves compression unset.
	DefaultBackupCompression = "zlib"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Cinder",type="string",JSONPath=".spec.cinderRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CinderBackupBackend is the Schema for the cinderbackupbackends API. One CR
// attaches to a Cinder CR via spec.cinderRef and describes the driver the backup
// service writes volume backups through (Phase 1: NFS).
//
// It is a separate kind from CinderBackend because the two describe different
// things: a CinderBackend is one of several volume backends a Cinder serves and
// gets its own cinder-volume Deployment, while the backup driver is a single
// property of the one cinder-backup Deployment.
//
// Detaching it is not a plain delete. The backup service registers under a host
// identity like a volume service does, so the controller holds the finalizer
// cinder.openstack.c5c3.io/service-remove on this CR: on deletion it runs a
// "cinder-manage service remove" Job for that host and releases the finalizer
// only once that Job succeeds.
type CinderBackupBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CinderBackupBackendSpec   `json:"spec,omitempty"`
	Status CinderBackupBackendStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CinderBackupBackendList contains a list of CinderBackupBackend.
type CinderBackupBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CinderBackupBackend `json:"items"`
}

// CinderBackupBackendType enumerates the supported backup drivers. Phase 1 ships
// NFS.
// +kubebuilder:validation:Enum=NFS
type CinderBackupBackendType string

const (
	// CinderBackupBackendTypeNFS selects the Cinder NFS backup driver.
	CinderBackupBackendTypeNFS CinderBackupBackendType = "NFS"
)

// CinderBackupBackendSpec defines the desired state of CinderBackupBackend.
//
// The cinderRef transition rule (evaluated only on UPDATE) makes the attachment
// immutable: re-pointing the backup driver at a different Cinder would leave the
// backups it already wrote recorded against a deployment that cannot read them.
// The type rule freezes the driver for the same reason: a restore reads the
// backup with the driver that wrote it. Delete and recreate instead, which runs
// the service-remove Job. The type/nfs union rule enforces "exactly one backend
// block matching spec.type" at the schema layer so it holds even when the
// validating webhook is down.
// +kubebuilder:validation:XValidation:rule="self.cinderRef.name == oldSelf.cinderRef.name",message="cinderRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="type is immutable"
// +kubebuilder:validation:XValidation:rule="(self.type == 'NFS') == has(self.nfs)",message="exactly one backup backend block matching spec.type must be set (type NFS requires spec.nfs)"
type CinderBackupBackendSpec struct {
	// CinderRef names the Cinder CR in the same namespace this backup backend
	// attaches to. The referenced CR does not have to exist at admission time
	// (GitOps ordering: the backup backend may be applied before the Cinder CR);
	// a dangling reference surfaces as Ready=False.
	CinderRef CinderRefSpec `json:"cinderRef"`

	// Type selects the backup driver. Phase 1 supports NFS only.
	Type CinderBackupBackendType `json:"type"`

	// NFS configures the NFS backup driver. Required exactly when type is NFS
	// (union rule above).
	// +optional
	NFS *NFSBackupBackendSpec `json:"nfs,omitempty"`

	// FileSize is the size in bytes of one backup chunk
	// ([DEFAULT] backup_file_size): cinder splits a volume into objects of this
	// size and writes them one at a time, so it bounds both the memory a backup
	// holds and the work a failed chunk costs.
	//
	// It has to be a multiple of 32768, the block size cinder hashes chunks in
	// (backup_sha_block_size_bytes), which the MultipleOf marker enforces.
	// +optional
	// +kubebuilder:validation:Minimum=1048576
	// +kubebuilder:validation:MultipleOf=32768
	// +kubebuilder:default=52428800
	FileSize *int64 `json:"fileSize,omitempty"`

	// Compression selects the algorithm each chunk is compressed with
	// ([DEFAULT] backup_compression_algorithm). "none" writes the chunks
	// uncompressed, which trades backup capacity for CPU on the cinder-backup
	// pod.
	// +optional
	// +kubebuilder:validation:Enum=none;zlib;bz2;zstd
	// +kubebuilder:default=zlib
	Compression string `json:"compression,omitempty"`

	// ExtraOptions provides free-form backup-section options not covered by the
	// typed fields, keyed by bare option name. The validating webhook rejects
	// options already owned by the typed fields (a denylist) so the escape hatch
	// cannot silently contradict the typed spec.
	//
	// MaxProperties and the per-entry key/value length bound the aggregate
	// rendered config at admission so this free-form map cannot bloat the section
	// past reasonable size.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 256 && size(self[k]) <= 1024)",message="each extraOptions key must be <=256 characters and each value <=1024 characters"
	// +optional
	ExtraOptions map[string]string `json:"extraOptions,omitempty"`
}

// NFSBackupBackendSpec configures the Cinder NFS backup driver. The
// cinder-backup pod mounts the export and writes the backup chunks onto it as
// files.
type NFSBackupBackendSpec struct {
	// Server is the NFS server the export lives on, as a hostname or an IP
	// address. It reaches the mount command verbatim, which is why the pattern
	// admits only the characters a hostname or an IPv4 address carries.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9.-]+$`
	Server string `json:"server"`

	// Path is the absolute export path on the server. The pattern admits only
	// printable ASCII the same way NFSBackendSpec.Path does: the value is
	// rendered verbatim as the backup_share option, and both cinder and this
	// operator derive the export's mount directory from that string, while
	// oslo.config strips what surrounds an option value before cinder reads it —
	// and strips the whole Unicode whitespace set, not the five ASCII characters
	// an \s-based pattern would exclude — so a trailing space, or a U+00A0 pasted
	// in with the path, would leave the export mounted at a directory the backup
	// driver does not look under. A newline is excluded on top of that, because
	// the reconcile-time guard answers one by deleting the backup Deployment
	// outright rather than by rejecting the edit.
	// +kubebuilder:validation:Pattern=`^/[!-~]*$`
	Path string `json:"path"`

	// MountOptions is the comma-separated option string the export is mounted
	// with. The default pins NFSv4.1 and a soft mount (see
	// DefaultNFSMountOptions); override it only where the server demands
	// different semantics, and keep the mount soft, because a hard mount blocks
	// the cinder-backup process on an unreachable server rather than failing the
	// request. The pattern excludes a newline and a carriage return for the same
	// reason spec.nfs.path does.
	// +optional
	// +kubebuilder:validation:Pattern=`^[^\n\r]*$`
	// +kubebuilder:default="nfsvers=4.1,soft,timeo=30,retrans=2"
	MountOptions string `json:"mountOptions,omitempty"`
}

// CinderBackupBackendStatus defines the observed state of CinderBackupBackend.
// The dedicated CinderBackupBackend controller is the single writer of this
// status; the Cinder-side sub-reconciler only reads it and writes an aggregated
// condition onto the Cinder CR instead.
type CinderBackupBackendStatus struct {
	// Conditions represent the latest available observations of the backup
	// backend state. Each condition carries an ObservedGeneration so consumers
	// can tell a stale condition from one reflecting the current spec; use the
	// conditions helper (internal/common/conditions) to upsert them.
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
	SchemeBuilder.Register(&CinderBackupBackend{}, &CinderBackupBackendList{})
}
