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
// service writes volume backups through, either an NFS export (type NFS) or a
// Ceph RBD pool (type RBD).
//
// It is a separate kind from CinderBackend because the two describe different
// things: a CinderBackend is one of several volume backends a Cinder serves and
// gets its own cinder-volume Deployment, while the backup driver is a single
// property of the one cinder-backup Deployment.
//
// Detaching it is a plain delete. The backup service registers as
// "<cinder>-backup" and re-registers under that identity whenever it starts, so
// there is no per-target service row to unregister and this kind carries no
// finalizer. The parent Cinder sees the detach through its watch, deletes the
// "<cinder>-backup" Deployment and prunes the Secrets this CR was rendered into
// on its next pass.
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

// CinderBackupBackendType enumerates the supported backup drivers: the chunked
// NFS driver and the Ceph backup driver.
// +kubebuilder:validation:Enum=NFS;RBD
type CinderBackupBackendType string

const (
	// CinderBackupBackendTypeNFS selects the Cinder NFS backup driver.
	CinderBackupBackendTypeNFS CinderBackupBackendType = "NFS"
	// CinderBackupBackendTypeRBD selects the Cinder Ceph backup driver, which
	// writes the backups as RBD images into a Ceph pool.
	CinderBackupBackendTypeRBD CinderBackupBackendType = "RBD"
)

// CinderBackupBackendSpec defines the desired state of CinderBackupBackend.
//
// The cinderRef transition rule (evaluated only on UPDATE) makes the attachment
// immutable: re-pointing the backup driver at a different Cinder would leave the
// backups it already wrote recorded against a deployment that cannot read them.
// The type rule freezes the driver for the same reason: a restore reads the
// backup with the driver that wrote it. Delete and recreate instead, which runs
// the service-remove Job. The union rule enforces "exactly one backup backend
// block matching spec.type" at the schema layer for both types (spec.nfs for
// NFS, spec.rbd for RBD) so it holds even when the validating webhook is down.
// +kubebuilder:validation:XValidation:rule="self.cinderRef.name == oldSelf.cinderRef.name",message="cinderRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="type is immutable"
// +kubebuilder:validation:XValidation:rule="(self.type == 'NFS') == has(self.nfs) && (self.type == 'RBD') == has(self.rbd)",message="exactly one backup backend block matching spec.type must be set (type NFS requires spec.nfs, type RBD requires spec.rbd)"
type CinderBackupBackendSpec struct {
	// CinderRef names the Cinder CR in the same namespace this backup backend
	// attaches to. The referenced CR does not have to exist at admission time
	// (GitOps ordering: the backup backend may be applied before the Cinder CR);
	// a dangling reference surfaces as Ready=False.
	CinderRef CinderRefSpec `json:"cinderRef"`

	// Type selects the backup driver: NFS or RBD.
	Type CinderBackupBackendType `json:"type"`

	// NFS configures the NFS backup driver. Required exactly when type is NFS
	// (union rule above).
	// +optional
	NFS *NFSBackupBackendSpec `json:"nfs,omitempty"`

	// RBD configures the Ceph backup driver. Required exactly when type is RBD
	// (union rule above).
	// +optional
	RBD *RBDBackupBackendSpec `json:"rbd,omitempty"`

	// FileSize is the size in bytes of one backup chunk
	// ([DEFAULT] backup_file_size): cinder splits a volume into objects of this
	// size and writes them one at a time, so it bounds both the memory a backup
	// holds and the work a failed chunk costs.
	//
	// It has to be a multiple of 32768, the block size cinder hashes chunks in
	// (backup_sha_block_size_bytes), which the MultipleOf marker enforces.
	//
	// The option belongs to the chunked NFS driver: an RBD target neither
	// renders nor reads it, because the Ceph driver splits a full copy by
	// backup_ceph_chunk_size, which extraOptions reaches.
	// +optional
	// +kubebuilder:validation:Minimum=1048576
	// +kubebuilder:validation:MultipleOf=32768
	// +kubebuilder:default=52428800
	FileSize *int64 `json:"fileSize,omitempty"`

	// Compression selects the algorithm each chunk is compressed with
	// ([DEFAULT] backup_compression_algorithm). "none" writes the chunks
	// uncompressed, which trades backup capacity for CPU on the cinder-backup
	// pod.
	//
	// The option belongs to the chunked NFS driver: an RBD target neither
	// renders nor reads it, because the Ceph driver compresses nothing.
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

// RBDBackupBackendSpec configures the Cinder Ceph backup driver. The operator
// creates no pool and no cephx user: both exist on the Ceph cluster before the
// backup target is applied, and the key of the user reaches the operator
// through the Secret keySecretRef names. Every field below reaches a file, a
// command line or a NetworkPolicy verbatim, which is why the patterns are
// allowlists.
//
// It is a type of its own rather than RBDBackendSpec because a backup target
// has no use for secretUUID: no hypervisor attaches a backup image, so no
// libvirt secret looks its key up.
type RBDBackupBackendSpec struct {
	// Pool is the Ceph pool the backups are written to, rendered as
	// backup_ceph_pool.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	Pool string `json:"pool"`

	// User is the cephx user the driver authenticates as, without its "client."
	// prefix. It is rendered as backup_ceph_user, reaches the Ceph tools as the
	// --id argument, names the [client.<user>] section of the projected keyring
	// and the keyring's file name /etc/ceph/<cluster>.client.<user>.keyring.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('client.')",message="user is the cephx name without its client. prefix (write cinder-backup, not client.cinder-backup)"
	User string `json:"user"`

	// Monitors lists the Ceph monitor addresses, each a hostname or an IPv4
	// address with an optional port. The list is rendered comma-joined, in list
	// order, as the mon_host line of the projected ceph.conf. A bare host is
	// tried on the msgr2 port 3300 and then on the msgr1 port 6789. IPv6
	// literals are not admitted.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`
	Monitors []string `json:"monitors"`

	// Networks lists the IPv4 CIDRs the Ceph cluster answers on. They are
	// unioned with the networks of the Cinder's RBD volume backends into the one
	// Ceph egress rule of its NetworkPolicy, which opens the monitor ports 3300
	// and 6789 and the OSD port range 6800 to 7300. The list has to cover the
	// addresses the monitors and the OSDs answer on; for a Rook cluster in the
	// same Kubernetes cluster that is the pod network and, because Rook
	// advertises the monitors through Services, the service network. The field
	// is required rather than optional so a backup target admitted on a Cinder
	// without spec.networkPolicy keeps its egress when the policy is enabled
	// later.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`
	Networks []string `json:"networks"`

	// ClusterName is the Ceph cluster name. The Ceph backup driver takes no
	// cluster argument, so the name decides the two projected file names alone:
	// /etc/ceph/<cluster>.conf, which backup_ceph_conf names, and
	// /etc/ceph/<cluster>.client.<user>.keyring.
	// +optional
	// +kubebuilder:default=ceph
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	ClusterName string `json:"clusterName,omitempty"`

	// KeySecretRef names the Secret that holds the cephx key of the user under
	// the data key userKey (RBDKeySecretDataKey). The Secret lives in this
	// backup target's namespace on the cluster the Cinder's
	// spec.targetClusterRef names, which is where the backup pod runs.
	KeySecretRef SecretNameRefSpec `json:"keySecretRef"`
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
