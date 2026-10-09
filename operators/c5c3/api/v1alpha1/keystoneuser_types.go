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
// +kubebuilder:printcolumn:name="Generation",type="integer",JSONPath=".status.passwordGeneration"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 bytes; it is carried as a label value on the order's children"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.userName) ? self.spec.userName : self.metadata.name) == (has(oldSelf.spec.userName) ? oldSelf.spec.userName : oldSelf.metadata.name)",message="userName is immutable; delete and re-create the KeystoneUser to rename its user"

// KeystoneUser orders one Keystone user from a namespace the referenced
// ControlPlane assigns to a service owner. The operator creates the user in the
// ControlPlane's admin domain, keeps its password in OpenBao, and writes the
// credentials into the Secret <metadata.name>-credentials beside the order, with
// the keys clouds.yaml and password.
//
// The order lives in the assigned namespace, on the cluster that namespace is
// assigned on: the management cluster or a registered target cluster. Status,
// the finalizer and the delivered Secret are written on the order's own
// cluster. The K-ORC User, its password Secrets and the PushSecret backing the
// password up to OpenBao live in the ControlPlane's namespace on the management
// cluster and carry ownership labels.
//
// The spec.namespaceAssignments entry for the order's namespace and cluster is
// the only consent. spec.korc.serviceRegistrations.allowedNamespaces and the
// ControlPlane's own namespaces admit nothing. Without an entry the order
// reports NamespaceNotAssigned and is frozen: nothing is provisioned,
// delivered, repaired or swept, and what it created stays. Deleting the order
// still tears everything down.
//
// The user is unscoped: it has no project and no role, so its clouds.yaml
// carries no project keys and yields an unscoped token. A Keystone user of the
// same name that the order did not create is refused with
// ServiceAccountCollision and never taken over.
//
// Raising spec.passwordGeneration rotates the password. The delivered Secret
// is rewritten with the new one, and status.passwordGeneration reports the
// generation Keystone holds.
//
// The kind has no webhook, because a target cluster runs none: every
// admission rule is a schema rule. An empty spec.userName means metadata.name,
// and an empty spec.controlPlaneRef.namespace means the order's own namespace;
// the reconciler resolves both defaults.
type KeystoneUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeystoneUserSpec   `json:"spec"`
	Status KeystoneUserStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeystoneUserList contains a list of KeystoneUser.
type KeystoneUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeystoneUser `json:"items"`
}

// KeystoneUserSpec defines the desired state of a KeystoneUser.
type KeystoneUserSpec struct {
	// ControlPlaneRef names the ControlPlane whose identity plane the user is
	// created in. An order on a target cluster sets the namespace: the
	// ControlPlane lives on the management cluster, in a namespace the order's
	// own namespace does not name.
	//
	// Both halves are frozen after creation. Re-pointing a live order would
	// strand the Keystone user it created on the old plane. Delete and re-create
	// the order to move it. The namespace rule spells the field __namespace__,
	// the escape Kubernetes CEL requires for a property named after a CEL
	// reserved word.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="controlPlaneRef.name is immutable; delete and re-create the KeystoneUser to move it to another ControlPlane"
	// +kubebuilder:validation:XValidation:rule="(has(self.__namespace__) ? self.__namespace__ : '') == (has(oldSelf.__namespace__) ? oldSelf.__namespace__ : '')",message="controlPlaneRef.namespace is immutable; delete and re-create the KeystoneUser to move it to another ControlPlane"
	ControlPlaneRef ControlPlaneRefSpec `json:"controlPlaneRef"`

	// UserName is the Keystone user name. Empty (the default) means the order's
	// metadata.name. The pattern and caps mirror K-ORC's OpenStackName.
	//
	// The effective name is frozen after creation by a rule on the whole object,
	// so setting userName to a value other than metadata.name later is rejected
	// as well. The name identifies a live Keystone user, and an edit would create
	// a second one.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	UserName string `json:"userName,omitempty"`

	// PasswordGeneration is the generation of the user's password. Raising it
	// rotates the password: the operator generates a new one, Keystone applies
	// it, and the delivered Secret is rewritten. It may only increase.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self >= oldSelf",message="passwordGeneration may only increase"
	PasswordGeneration int64 `json:"passwordGeneration,omitempty"`
}

// KeystoneUserStatus defines the observed state of a KeystoneUser.
type KeystoneUserStatus struct {
	// Conditions are UserReady, DeliveryReady and the aggregate Ready.
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

	// UserID is the Keystone user id. Empty until the user is provisioned.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	UserID string `json:"userID,omitempty"`

	// PasswordGeneration is the password generation Keystone holds.
	// +optional
	PasswordGeneration int64 `json:"passwordGeneration,omitempty"`

	// LastPasswordRotation is when the password was last rotated.
	// +optional
	LastPasswordRotation *metav1.Time `json:"lastPasswordRotation,omitempty"`

	// SecretName is the name of the delivered Secret in the order's namespace,
	// <metadata.name>-credentials.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// SecretKeys lists the keys of the delivered Secret once it carries the
	// current password: clouds.yaml and password.
	// +optional
	// +listType=atomic
	SecretKeys []string `json:"secretKeys,omitempty"`
}

func init() {
	SchemeBuilder.Register(&KeystoneUser{}, &KeystoneUserList{})
}
