// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="ControlPlane",type="string",JSONPath=".spec.controlPlaneRef.name"
// +kubebuilder:printcolumn:name="User",type="string",JSONPath=".spec.userRef.name"
// +kubebuilder:printcolumn:name="Project",type="string",JSONPath=".spec.projectRef.name"
// +kubebuilder:printcolumn:name="Generation",type="integer",JSONPath=".status.credentialGeneration"
// +kubebuilder:printcolumn:name="NextRotation",type="string",JSONPath=".status.nextRotation"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 bytes; it is carried as a label value on the order's children"

// KeystoneApplicationCredential orders one Keystone application credential for
// an ordered user on an ordered project, from a namespace the referenced
// ControlPlane assigns to a service owner. The operator mints the credential
// as the user itself, with a token scoped to the project, and writes its id and
// secret into the Secret <metadata.name>-credentials beside the order, with the
// keys clouds.yaml, application_credential_id and application_credential_secret.
//
// spec.userRef names a KeystoneUser and spec.projectRef a KeystoneProject in
// the order's own namespace on the order's own cluster, both ordered from the
// same ControlPlane. A KeystoneRoleAssignment in the namespace must have
// assigned the user a role on the project: the credential takes the token's
// roles, and a user without a role on the project gets no project-scoped token.
// Without one the order reports NoRoleOnProject and mints nothing.
//
// The spec.namespaceAssignments entry for the order's namespace and cluster is
// the only consent. Removing it freezes the order: nothing is minted, rotated,
// delivered, repaired or swept, and the delivered credential stays. Deleting
// the order still tears everything down.
//
// spec.rotation schedules the rotation. Every spec.rotation.interval (default
// 720h) the operator mints a successor, switches the delivered Secret once the
// successor is Available in Keystone, and deletes the superseded credential
// after spec.rotation.gracePeriod (default 24h), so the delivered credential is
// never invalid. A credential minted while the schedule is on expires in
// Keystone at its mint time plus the interval plus the grace period, so a
// credential the operator fails to delete dies on its own. An interval of 0s
// turns the schedule off; a credential minted then carries no expiry. Raising
// spec.credentialGeneration above status.credentialGeneration rotates at once.
//
// While the order exists, deleting the referenced KeystoneUser, the referenced
// KeystoneProject or a KeystoneRoleAssignment binding the same user and project
// holds on ReferencedByApplicationCredentials.
//
// The kind has no webhook, because a target cluster runs none: every admission
// rule is a schema rule. An empty spec.controlPlaneRef.namespace means the
// order's own namespace, a zero spec.credentialGeneration means 1, and an
// absent rotation interval or grace period means its default; the reconciler
// resolves all of them.
type KeystoneApplicationCredential struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeystoneApplicationCredentialSpec   `json:"spec"`
	Status KeystoneApplicationCredentialStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeystoneApplicationCredentialList contains a list of
// KeystoneApplicationCredential.
type KeystoneApplicationCredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeystoneApplicationCredential `json:"items"`
}

// KeystoneApplicationCredentialSpec defines the desired state of a
// KeystoneApplicationCredential.
type KeystoneApplicationCredentialSpec struct {
	// ControlPlaneRef names the ControlPlane whose identity plane the credential
	// is minted in. It must be the ControlPlane both referenced orders name.
	//
	// Both halves are frozen after creation. Re-pointing a live order would
	// strand the credentials it minted on the old plane. Delete and re-create the
	// order to move it. The namespace rule spells the field __namespace__, the
	// escape Kubernetes CEL requires for a property named after a CEL reserved
	// word.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="controlPlaneRef.name is immutable; delete and re-create the KeystoneApplicationCredential to move it to another ControlPlane"
	// +kubebuilder:validation:XValidation:rule="(has(self.__namespace__) ? self.__namespace__ : '') == (has(oldSelf.__namespace__) ? oldSelf.__namespace__ : '')",message="controlPlaneRef.namespace is immutable; delete and re-create the KeystoneApplicationCredential to move it to another ControlPlane"
	ControlPlaneRef ControlPlaneRefSpec `json:"controlPlaneRef"`

	// UserRef names the KeystoneUser in the order's namespace the credential is
	// minted for. It is frozen after creation.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="userRef is immutable; delete and re-create the KeystoneApplicationCredential to bind another user"
	UserRef KeystoneOrderRef `json:"userRef"`

	// ProjectRef names the KeystoneProject in the order's namespace the
	// credential is scoped to. It is frozen after creation.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="projectRef is immutable; delete and re-create the KeystoneApplicationCredential to bind another project"
	ProjectRef KeystoneOrderRef `json:"projectRef"`

	// CredentialGeneration is the lowest credential generation the order must
	// hold. The schedule raises the live generation on its own, so raising this
	// above status.credentialGeneration rotates the credential at once. It may
	// only increase.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self >= oldSelf",message="credentialGeneration may only increase"
	CredentialGeneration int64 `json:"credentialGeneration,omitempty"`

	// Rotation schedules the rotation of the credential. An order that omits
	// the block gets the defaults of its two fields.
	//
	// The API server applies the two field defaults before it validates an
	// object, so the block rule always sees both fields. The has() guards are
	// for the CRD's own check of the block default {}, which runs without them.
	// +optional
	// +kubebuilder:default={}
	// +kubebuilder:validation:XValidation:rule="!has(self.interval) || !has(self.gracePeriod) || duration(self.interval) == duration('0s') || duration(self.gracePeriod) < duration(self.interval)",message="rotation.gracePeriod must be shorter than rotation.interval"
	Rotation KeystoneApplicationCredentialRotationSpec `json:"rotation,omitempty"`
}

// KeystoneApplicationCredentialRotationSpec schedules the rotation of an
// ordered application credential.
type KeystoneApplicationCredentialRotationSpec struct {
	// Interval is the time between two mints. 0s turns the schedule off. A
	// credential minted while the schedule is on expires in Keystone at its mint
	// time plus the interval plus the grace period. The interval is at most
	// 87600h, so that sum stays inside the range of a Go duration and the expiry
	// is never computed in the past.
	// +optional
	// +kubebuilder:default="720h"
	// +kubebuilder:validation:XValidation:rule="duration(self) == duration('0s') || duration(self) >= duration('1m')",message="rotation.interval is 0s or at least 1m"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('87600h')",message="rotation.interval is at most 87600h"
	Interval *metav1.Duration `json:"interval,omitempty"`

	// GracePeriod is how long the superseded credential stays in Keystone after
	// the delivered Secret switched to its successor.
	// +optional
	// +kubebuilder:default="24h"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="rotation.gracePeriod is not negative"
	GracePeriod *metav1.Duration `json:"gracePeriod,omitempty"`
}

// KeystoneApplicationCredentialStatus defines the observed state of a
// KeystoneApplicationCredential.
type KeystoneApplicationCredentialStatus struct {
	// Conditions are CredentialReady, DeliveryReady and the aggregate Ready.
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

	// CredentialID is the Keystone id of the credential the delivered Secret
	// carries. Empty until the first credential is minted.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	CredentialID string `json:"credentialID,omitempty"`

	// CredentialGeneration is the generation of the credential the delivered
	// Secret carries.
	// +optional
	CredentialGeneration int64 `json:"credentialGeneration,omitempty"`

	// CredentialExpiresAt is when the current credential expires in Keystone.
	// Absent for a credential minted with the schedule off.
	// +optional
	CredentialExpiresAt *metav1.Time `json:"credentialExpiresAt,omitempty"`

	// LastRotation is when the current credential became the delivered one.
	// +optional
	LastRotation *metav1.Time `json:"lastRotation,omitempty"`

	// NextRotation is when the next scheduled rotation is due: the earlier of
	// lastRotation plus the interval and credentialExpiresAt minus the grace
	// period. Absent with the schedule off.
	// +optional
	NextRotation *metav1.Time `json:"nextRotation,omitempty"`

	// PreviousCredentialID is the Keystone id of the superseded credential. Set
	// only during its grace period.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	PreviousCredentialID string `json:"previousCredentialID,omitempty"`

	// PreviousCredentialGeneration is the generation of the superseded
	// credential. Set only during its grace period.
	// +optional
	PreviousCredentialGeneration int64 `json:"previousCredentialGeneration,omitempty"`

	// PreviousCredentialDeleteAt is when the superseded credential is deleted.
	// Set only during its grace period.
	// +optional
	PreviousCredentialDeleteAt *metav1.Time `json:"previousCredentialDeleteAt,omitempty"`

	// SecretName is the name of the delivered Secret in the order's namespace,
	// <metadata.name>-credentials.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// SecretKeys lists the keys of the delivered Secret once it carries the
	// current credential: clouds.yaml, application_credential_id and
	// application_credential_secret.
	// +optional
	// +listType=atomic
	SecretKeys []string `json:"secretKeys,omitempty"`
}

func init() {
	SchemeBuilder.Register(&KeystoneApplicationCredential{}, &KeystoneApplicationCredentialList{})
}
