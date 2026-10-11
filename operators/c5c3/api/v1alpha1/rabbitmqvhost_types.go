// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The deletion policies of a RabbitMQVhost.
const (
	// RabbitMQVhostDeletionPolicyRetain keeps the vhost, and the queues and
	// messages in it, on the broker when the order is deleted.
	RabbitMQVhostDeletionPolicyRetain = "Retain"
	// RabbitMQVhostDeletionPolicyDelete deletes the vhost from the broker with
	// the order.
	RabbitMQVhostDeletionPolicyDelete = "Delete"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="ControlPlane",type="string",JSONPath=".spec.controlPlaneRef.name"
// +kubebuilder:printcolumn:name="Vhost",type="string",JSONPath=".status.vhost"
// +kubebuilder:printcolumn:name="Generation",type="integer",JSONPath=".status.passwordGeneration"
// +kubebuilder:printcolumn:name="NextRotation",type="string",JSONPath=".status.nextRotation"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 bytes; it is carried as a label value on the order's children"

// RabbitMQVhost orders one RabbitMQ vhost on the ControlPlane's managed message
// bus, with one user that holds configure, write and read permissions of ".*"
// on it and no management tags, from a namespace the referenced ControlPlane
// assigns to a service owner. The operator writes the credentials into the
// Secret <metadata.name>-credentials beside the order, with the keys
// transport_url, host, port, username, password and vhost.
//
// The vhost, the user and the permission are Vhost, User and Permission CRs
// of the RabbitMQ Messaging Topology Operator (rabbitmq.com/v1beta1), created
// in the ControlPlane's namespace on the management cluster beside the
// RabbitmqCluster spec.infrastructure.messaging.clusterRef names. They carry
// ownership labels; the topology operator creates them on the broker.
//
// Only a managed bus serves the order. A ControlPlane that attaches to a
// brownfield broker through spec.infrastructure.messaging.secretRef refuses it
// with MessagingNotManaged: the operator knows such a broker by its transport
// URL alone and holds no admin account on it. A managed bus runs no TLS
// listener, so the delivered Secret carries no CA.
//
// The vhost name is derived, not chosen: <metadata.name>-<8 hex>, where the
// hash covers the order's cluster, namespace and name. Two orders never share
// a vhost, and an order re-created with the same name reattaches to a vhost a
// Retain deletion left behind. The user of password generation N is
// <vhost>-v<N>.
//
// The spec.namespaceAssignments entry for the order's namespace and cluster is
// the only consent. Without an entry the order reports NamespaceNotAssigned
// and is frozen: nothing is provisioned, rotated, delivered, repaired or swept,
// and what it created stays. Deleting the order still tears everything down.
//
// spec.rotation schedules the rotation. Every spec.rotation.interval (default
// 720h) the operator creates a successor user with a new password, switches
// the delivered Secret once the broker holds it, and deletes the superseded
// user after spec.rotation.gracePeriod (default 24h), so the delivered
// credential is never invalid. An interval of 0s turns the schedule off.
// Raising spec.passwordGeneration above status.passwordGeneration rotates at
// once.
//
// spec.deletionPolicy decides what deleting the order does to the vhost: Retain
// (the default) keeps it with its queues and messages, Delete removes it. The
// user, its permission and the backed-up credentials go with the order either
// way.
//
// The kind has no webhook, because a target cluster runs none: every
// admission rule is a schema rule. An empty spec.controlPlaneRef.namespace
// means the order's own namespace, a zero spec.passwordGeneration means 1, an
// absent rotation interval or grace period means its default, and an empty
// spec.deletionPolicy means Retain; the reconciler resolves all of them.
type RabbitMQVhost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RabbitMQVhostSpec   `json:"spec"`
	Status RabbitMQVhostStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RabbitMQVhostList contains a list of RabbitMQVhost.
type RabbitMQVhostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RabbitMQVhost `json:"items"`
}

// RabbitMQVhostSpec defines the desired state of a RabbitMQVhost.
type RabbitMQVhostSpec struct {
	// ControlPlaneRef names the ControlPlane whose managed message bus the vhost
	// is created on. An order on a target cluster sets the namespace: the
	// ControlPlane lives on the management cluster, in a namespace the order's
	// own namespace does not name.
	//
	// Both halves are frozen after creation. Re-pointing a live order would
	// strand the vhost and the user it created on the old bus. Delete and
	// re-create the order to move it. The namespace rule spells the field
	// __namespace__, the escape Kubernetes CEL requires for a property named
	// after a CEL reserved word.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="controlPlaneRef.name is immutable; delete and re-create the RabbitMQVhost to move it to another ControlPlane"
	// +kubebuilder:validation:XValidation:rule="(has(self.__namespace__) ? self.__namespace__ : '') == (has(oldSelf.__namespace__) ? oldSelf.__namespace__ : '')",message="controlPlaneRef.namespace is immutable; delete and re-create the RabbitMQVhost to move it to another ControlPlane"
	ControlPlaneRef ControlPlaneRefSpec `json:"controlPlaneRef"`

	// PasswordGeneration is the lowest password generation the order must hold.
	// The schedule raises the live generation on its own, so raising this above
	// status.passwordGeneration rotates the password at once. It may only
	// increase.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self >= oldSelf",message="passwordGeneration may only increase"
	PasswordGeneration int64 `json:"passwordGeneration,omitempty"`

	// Rotation schedules the rotation of the password. An order that omits the
	// block gets the defaults of its two fields.
	//
	// The API server applies the two field defaults before it validates an
	// object, so the block rule always sees both fields. The has() guards are
	// for the CRD's own check of the block default {}, which runs without them.
	// +optional
	// +kubebuilder:default={}
	// +kubebuilder:validation:XValidation:rule="!has(self.interval) || !has(self.gracePeriod) || duration(self.interval) == duration('0s') || duration(self.gracePeriod) < duration(self.interval)",message="rotation.gracePeriod must be shorter than rotation.interval"
	Rotation RabbitMQVhostRotationSpec `json:"rotation,omitempty"`

	// DeletionPolicy decides what deleting the order does to the vhost on the
	// broker. Retain keeps the vhost with its queues and messages; the platform
	// operator removes it with rabbitmqctl delete_vhost. Delete removes it. The
	// user, its permission and the backed-up credentials go with the order
	// either way. The policy may change at any time and applies to the next
	// deletion.
	// +optional
	// +kubebuilder:default=Retain
	// +kubebuilder:validation:Enum=Retain;Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// RabbitMQVhostRotationSpec schedules the rotation of an ordered vhost's
// password.
type RabbitMQVhostRotationSpec struct {
	// Interval is the time between two rotations. 0s turns the schedule off. The
	// interval is at most 87600h, so the next rotation time stays inside the
	// range of a Go duration.
	// +optional
	// +kubebuilder:default="720h"
	// +kubebuilder:validation:XValidation:rule="duration(self) == duration('0s') || duration(self) >= duration('1m')",message="rotation.interval is 0s or at least 1m"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('87600h')",message="rotation.interval is at most 87600h"
	Interval *metav1.Duration `json:"interval,omitempty"`

	// GracePeriod is how long the superseded user stays on the broker after the
	// delivered Secret switched to its successor.
	// +optional
	// +kubebuilder:default="24h"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="rotation.gracePeriod is not negative"
	GracePeriod *metav1.Duration `json:"gracePeriod,omitempty"`
}

// RabbitMQVhostStatus defines the observed state of a RabbitMQVhost.
type RabbitMQVhostStatus struct {
	// Conditions are VhostReady, DeliveryReady and the aggregate Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the .metadata.generation the controller last
	// reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Vhost is the name of the vhost on the broker, <metadata.name>-<8 hex>.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Vhost string `json:"vhost,omitempty"`

	// Username is the broker user the delivered Secret carries,
	// <vhost>-v<passwordGeneration>.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Username string `json:"username,omitempty"`

	// Host is the broker host the delivered Secret names.
	// +optional
	// +kubebuilder:validation:MaxLength=262
	Host string `json:"host,omitempty"`

	// Port is the broker port the delivered Secret names.
	// +optional
	Port int32 `json:"port,omitempty"`

	// PasswordGeneration is the password generation of the user the delivered
	// Secret carries.
	// +optional
	PasswordGeneration int64 `json:"passwordGeneration,omitempty"`

	// LastRotation is when the current user became the delivered one.
	// +optional
	LastRotation *metav1.Time `json:"lastRotation,omitempty"`

	// NextRotation is when the next scheduled rotation is due: lastRotation plus
	// the interval. Absent with the schedule off.
	// +optional
	NextRotation *metav1.Time `json:"nextRotation,omitempty"`

	// PreviousPasswordGeneration is the generation of the superseded user. Set
	// only during its grace period.
	// +optional
	PreviousPasswordGeneration int64 `json:"previousPasswordGeneration,omitempty"`

	// PreviousPasswordDeleteAt is when the superseded user is deleted. Set only
	// during its grace period.
	// +optional
	PreviousPasswordDeleteAt *metav1.Time `json:"previousPasswordDeleteAt,omitempty"`

	// SecretName is the name of the delivered Secret in the order's namespace,
	// <metadata.name>-credentials.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// SecretKeys lists the keys of the delivered Secret once it carries the
	// current user: transport_url, host, port, username, password and vhost.
	// +optional
	// +listType=atomic
	SecretKeys []string `json:"secretKeys,omitempty"`
}

func init() {
	SchemeBuilder.Register(&RabbitMQVhost{}, &RabbitMQVhostList{})
}
