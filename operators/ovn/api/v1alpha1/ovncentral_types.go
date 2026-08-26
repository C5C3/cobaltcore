// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="NB",type="integer",JSONPath=".status.northbound.readyReplicas"
// +kubebuilder:printcolumn:name="SB",type="integer",JSONPath=".status.southbound.readyReplicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// OVNCentral is the Schema for the ovncentrals API. It deploys the OVN control
// plane as a unit: the Northbound and Southbound Raft databases, the northd
// daemon that translates between them, an optional Southbound relay, the
// cert-manager Certificates every connection is authenticated with, and the
// recurring database backup. The two databases share one CR because northd only
// works against both of them and their Raft clusters have to be sized together.
type OVNCentral struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OVNCentralSpec   `json:"spec,omitempty"`
	Status OVNCentralStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OVNCentralList contains a list of OVNCentral.
type OVNCentralList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OVNCentral `json:"items"`
}

// OVNCentralSpec defines the desired state of OVNCentral.
//
// The targetClusterRef transition rules (evaluated only on UPDATE) freeze the
// ref: adding it, removing it, or renaming it after creation is rejected at the
// schema layer, so the guarantee holds even when the validating webhook is
// down. Moving the control plane between clusters is not a supported mutation.
// +kubebuilder:validation:XValidation:rule="has(self.targetClusterRef) == has(oldSelf.targetClusterRef)",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.targetClusterRef) || !has(oldSelf.targetClusterRef) || self.targetClusterRef.name == oldSelf.targetClusterRef.name",message="targetClusterRef is immutable"
type OVNCentralSpec struct {
	// Image defines the container image that runs ovsdb-server, northd, and the
	// relay. All three processes ship in the same OVN image, so one reference
	// covers them. When nil the operator resolves its own default image, which
	// keeps the CR tracking the operator's tested OVN version across upgrades
	// instead of freezing today's tag into stored state.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// Northbound configures the Northbound database, the one the CMS writes the
	// logical network model into.
	// +optional
	// +kubebuilder:default={}
	Northbound OVNDatabaseSpec `json:"northbound,omitempty"`

	// Southbound configures the Southbound database, the one northd writes the
	// translated flows into and every chassis reads from. It is the busier of
	// the two, which is why it can be fronted by a relay.
	// +optional
	// +kubebuilder:default={}
	Southbound OVNDatabaseSpec `json:"southbound,omitempty"`

	// Northd configures the ovn-northd daemon that compiles the Northbound model
	// into Southbound flows.
	// +optional
	// +kubebuilder:default={}
	Northd OVNNorthdSpec `json:"northd,omitempty"`

	// Relay optionally fronts the Southbound database with ovsdb-server relays.
	// Every chassis holds an open Southbound connection, so past a few hundred
	// nodes the read load is what limits the cluster; the relays absorb it and
	// forward only writes to the Raft leader. When nil the chassis connect to
	// the database directly.
	// +optional
	Relay *OVNRelaySpec `json:"relay,omitempty"`

	// TLS names the cert-manager issuer the operator requests every OVN
	// certificate from. It is required: the OVN databases carry the entire
	// logical network model, so an unauthenticated listener would let any pod
	// that can reach the port rewrite the network. Authentication is the whole
	// control today — the operator sets no role= on the OVSDB connection rows,
	// so every certificate this issuer signs has unrestricted read and write on
	// both databases, and the issuer must not be shared with workloads that are
	// not part of this control plane.
	TLS OVNTLSSpec `json:"tls"`

	// Backup configures the recurring database backup. A Raft cluster survives
	// the loss of a minority of its members but not an operator error applied to
	// all of them, so the backup is the only path back from a corrupted logical
	// model. When nil the operator schedules the backup with its own defaults.
	// +optional
	Backup *OVNBackupSpec `json:"backup,omitempty"`

	// TargetClusterRef selects the registered target cluster the children are
	// created on. When nil they are created on the cluster the operator runs in.
	// The ref is immutable once set, enforced by the two transition rules above
	// and by the validating webhook.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`
}

// OVNDatabaseSpec configures one of the two OVN Raft databases.
//
// The three transition rules (evaluated only on UPDATE) freeze the fields that
// are baked into the cluster when its first member creates the database file.
// Raft membership changes and an election-timer change both need an
// ovsdb-tool/ovs-appctl procedure against the running cluster that the operator
// does not perform, and the storage size and class come from a StatefulSet
// volumeClaimTemplate, which the API server itself rejects updates to. Rejecting
// them here reports the constraint at admission instead of letting the
// StatefulSet update fail later.
// +kubebuilder:validation:XValidation:rule="self.replicas == oldSelf.replicas",message="replicas is immutable: Raft membership changes are not supported"
// +kubebuilder:validation:XValidation:rule="self.electionTimerMs == oldSelf.electionTimerMs",message="electionTimerMs is immutable: it is applied when the clustered database is created"
// +kubebuilder:validation:XValidation:rule="self.storage == oldSelf.storage",message="storage is immutable: volumeClaimTemplates cannot change"
type OVNDatabaseSpec struct {
	// Replicas is the number of Raft members. It must be odd: an even cluster
	// tolerates no more failures than the odd one below it and has two ways to
	// split the vote. Five is the practical ceiling, past which the write
	// latency of the extra round trips outweighs the added fault tolerance.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	// +kubebuilder:validation:XValidation:rule="self % 2 == 1",message="replicas must be odd"
	Replicas int32 `json:"replicas,omitempty"`

	// Storage sizes the per-member PersistentVolumeClaim holding the database
	// file and its Raft log.
	// +optional
	// +kubebuilder:default={}
	Storage OVNStorageSpec `json:"storage,omitempty"`

	// ExternallyReachable publishes every Raft member of this database on a node
	// port, making it reachable on the IP of every node of the cluster. It is
	// off by default: the databases carry the entire logical network model, and
	// the only client that needs them from outside the cluster is an OVNChassis
	// on a node without cluster networking, which dials the Southbound database
	// alone. Turn it on for that case, and only for the database that case
	// needs.
	// +optional
	// +kubebuilder:default=false
	ExternallyReachable bool `json:"externallyReachable,omitempty"`

	// NodePortBase is the first node port of the range this database is reachable
	// on from outside the cluster once externallyReachable is set. Each member
	// gets its own port, assigned in ordinal order from this base, because a Raft
	// client has to address the individual members rather than a load-balanced
	// name. When nil the operator resolves 30641 for the Northbound and 30651 for
	// the Southbound database. The webhook rejects a base that leaves fewer ports
	// below 32767 than there are replicas, and rejects two ranges that overlap.
	// +optional
	// +kubebuilder:validation:Minimum=30000
	// +kubebuilder:validation:Maximum=32767
	NodePortBase *int32 `json:"nodePortBase,omitempty"`

	// ElectionTimerMs is how long a follower waits without hearing from the
	// leader before it starts an election. Raising it keeps a cluster whose
	// members are separated by a slow link from re-electing on every hiccup, at
	// the cost of a longer write outage after a genuine leader loss. The value is
	// written into the database when it is created, so it cannot be changed
	// afterwards through this field.
	// +optional
	// +kubebuilder:default=1000
	// +kubebuilder:validation:Minimum=1000
	// +kubebuilder:validation:Maximum=180000
	ElectionTimerMs int32 `json:"electionTimerMs,omitempty"`

	// InactivityProbeMs is how long ovsdb-server lets a client connection sit
	// idle before probing it. Zero disables the probe, which is what a client
	// behind a connection-tracking middlebox needs when the probe itself is what
	// tears the connection down.
	// +optional
	// +kubebuilder:default=60000
	// +kubebuilder:validation:Minimum=0
	InactivityProbeMs int32 `json:"inactivityProbeMs,omitempty"`

	// Resources defines the CPU and memory requests and limits for the
	// ovsdb-server container. When nil the operator applies its own defaults.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// OVNStorageSpec sizes a PersistentVolumeClaim. It is shared by the two database
// members and by the backup volume, which have the same two knobs.
type OVNStorageSpec struct {
	// Size is the requested volume size. The pattern admits binary units only:
	// a decimal "1G" differs from "1Gi" by enough to matter on a volume this
	// small, and accepting both invites the confusion.
	// +optional
	// +kubebuilder:default="1Gi"
	// +kubebuilder:validation:Pattern=`^[0-9]+(Mi|Gi|Ti)$`
	Size string `json:"size,omitempty"`

	// StorageClassName selects the StorageClass. When nil the cluster's default
	// class is used.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// OVNNorthdSpec configures ovn-northd, the daemon that compiles the Northbound
// logical model into the Southbound flow table.
type OVNNorthdSpec struct {
	// Deployment groups the pod-level knobs for the northd Deployment (replicas,
	// resources, rollout strategy, graceful-termination timings, and scheduling
	// constraints). Only one northd instance is active at a time: the others sit
	// in standby and take over when the active one loses its lock.
	// +optional
	Deployment commonv1.DeploymentSpec `json:"deployment,omitempty"`

	// Threads is the number of parallel logical-flow computation threads. More
	// threads shorten the compile pass on a large logical model, but past a
	// handful the lock contention inside northd eats the gain, so the ceiling
	// stays low.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	Threads int32 `json:"threads,omitempty"`
}

// OVNRelaySpec configures the ovsdb-server relays in front of the Southbound
// database. Relays are stateless caches, so they scale independently of the Raft
// cluster behind them.
type OVNRelaySpec struct {
	// Replicas is the number of relay pods. Unlike the database replicas this is
	// a plain scaling knob with no odd-count or immutability constraint.
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas"`

	// Resources defines the CPU and memory requests and limits for the relay
	// container. When nil the operator applies its own defaults.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// OVNTLSSpec names the cert-manager issuer every OVN certificate is requested
// from.
type OVNTLSSpec struct {
	// IssuerRef selects the issuer. It must be CA-capable: OVN authenticates
	// peers against the issuing CA certificate, so an issuer that cannot expose
	// one (ACME, for instance) produces certificates the databases reject.
	IssuerRef OVNIssuerRef `json:"issuerRef"`
}

// OVNIssuerRef references a cert-manager Issuer or ClusterIssuer.
type OVNIssuerRef struct {
	// Name is the issuer's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Kind selects the issuer scope. It defaults to ClusterIssuer because the OVN
	// CA is normally shared with the chassis namespaces, which a namespaced
	// Issuer cannot reach.
	// +optional
	// +kubebuilder:default=ClusterIssuer
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	Kind string `json:"kind,omitempty"`
}

// OVNBackupSpec tunes the recurring database backup. The operator projects a
// CronJob that snapshots both databases with ovsdb-tool and keeps the snapshots
// on a PersistentVolumeClaim, optionally copying them to an S3 bucket.
//
// The snapshot volume is a child of the OVNCentral like any other, so deleting
// the CR deletes it along with the database volumes: the backup covers an
// operator error applied to every Raft member, not the removal of the object
// that owns it. Configure s3 for a copy that outlives the CR.
//
// The operator resolves the schedule and retention at reconcile time rather than
// in the defaulting webhook, so a field left unset keeps tracking the operator
// defaults across upgrades instead of freezing today's values into the stored
// CR.
type OVNBackupSpec struct {
	// Schedule is the cron expression the backup runs on. When empty the operator
	// resolves DefaultBackupSchedule. The expression is checked by the validating
	// webhook, which parses it with the same library the CronJob controller uses.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// RetentionDays is how long a snapshot is kept before the next run deletes
	// it. When nil the operator resolves DefaultBackupRetentionDays.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RetentionDays *int32 `json:"retentionDays,omitempty"`

	// Suspend stops the CronJob from firing without deleting it or the snapshots
	// already taken. It is the switch to reach for during a maintenance window
	// that would otherwise snapshot a half-migrated database.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Storage sizes the PersistentVolumeClaim the snapshots are written to.
	// +optional
	// +kubebuilder:default={}
	Storage OVNStorageSpec `json:"storage,omitempty"`

	// S3 optionally copies each snapshot off-cluster. Without it the snapshots
	// share the fate of the cluster that holds them, which covers an operator
	// error but not the loss of the cluster itself.
	// +optional
	S3 *OVNBackupS3Spec `json:"s3,omitempty"`
}

// OVNBackupS3Spec addresses the S3-compatible bucket the snapshots are copied to.
type OVNBackupS3Spec struct {
	// Bucket is the target bucket name.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Prefix is the key prefix each snapshot is written under, so one bucket can
	// hold the backups of several deployments.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Endpoint is the S3 service URL. It has to be HTTPS: the upload carries the
	// access key beside a full snapshot of both databases, and SigV4
	// authenticates a request without encrypting it, so a plaintext endpoint
	// puts the credentials and the whole logical network model on the wire.
	// +kubebuilder:validation:Pattern=`^https://`
	Endpoint string `json:"endpoint"`

	// Region is the S3 region. It is optional because most S3-compatible
	// implementations ignore it.
	// +optional
	Region string `json:"region,omitempty"`

	// CredentialsSecretRef references the Secret holding the access key, under
	// the keys access-key-id and secret-access-key. The Secret lives in the
	// OVNCentral's own namespace, beside the children the operator creates.
	CredentialsSecretRef commonv1.SecretRefSpec `json:"credentialsSecretRef"`

	// Image defines the container image the upload step runs. When nil the
	// operator resolves its own backup-shifter image.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`
}

// OVNCentralStatus defines the observed state of OVNCentral.
type OVNCentralStatus struct {
	// Conditions represent the latest available observations of the OVNCentral
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

	// Northbound reports the observed state of the Northbound database.
	// +optional
	Northbound OVNDatabaseStatus `json:"northbound,omitempty"`

	// Southbound reports the observed state of the Southbound database.
	// +optional
	Southbound OVNDatabaseStatus `json:"southbound,omitempty"`

	// RelayAddress is the connection string for the Southbound relay Service,
	// "ssl:<clusterIP>:6642". It is set while spec.relay is set and cleared when
	// the relay is removed.
	// +optional
	RelayAddress string `json:"relayAddress,omitempty"`

	// ClientSecretName names the Secret holding the client certificate every OVN
	// client authenticates with: tls.crt, tls.key, and ca.crt. An OVNChassis
	// mounts this Secret, so it is the field that connects the two kinds.
	// +optional
	ClientSecretName string `json:"clientSecretName,omitempty"`

	// InstalledImage is the image reference the running control plane was
	// projected from. It is what tells a rollout that has not reached the pods
	// yet from one that has.
	// +optional
	InstalledImage string `json:"installedImage,omitempty"`
}

// OVNDatabaseStatus reports the observed state of one OVN Raft database.
//
// Both connection strings are always IP literals, never DNS names: ovsdb-server
// resolves a remote once at startup and never again, so a name whose address
// changes leaves the client wedged against the old one. They list the members in
// ordinal order.
type OVNDatabaseStatus struct {
	// InternalDbAddress is the connection string for clients inside the cluster,
	// "ssl:<clusterIP>:<6641|6642>" per member, comma-separated. Each member has
	// its own Service, because a Raft client addresses the members individually.
	// +optional
	InternalDbAddress string `json:"internalDbAddress,omitempty"`

	// DbAddress is the connection string for clients outside the cluster,
	// "ssl:<node InternalIP>:<nodePortBase+ordinal>" per member,
	// comma-separated. It is what an OVNChassis on a node without cluster
	// networking connects to, and it is empty unless externallyReachable is set:
	// a database that is not published on node ports has no address outside the
	// cluster.
	// +optional
	DbAddress string `json:"dbAddress,omitempty"`

	// ReadyReplicas is the number of Raft members that are ready. A cluster keeps
	// serving writes while a minority is down, so this is a health signal rather
	// than an availability one.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
}

func init() {
	SchemeBuilder.Register(&OVNCentral{}, &OVNCentralList{})
}
