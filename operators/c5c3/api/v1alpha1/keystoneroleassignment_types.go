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
// +kubebuilder:printcolumn:name="Role",type="string",JSONPath=".spec.role"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 bytes; it is carried as a label value on the order's children"

// KeystoneRoleAssignment orders one Keystone role for an ordered user on an
// ordered project, from a namespace the referenced ControlPlane assigns to a
// service owner. It reports the role, user and project ids in status. No Secret
// is delivered: the user's own Secret scopes a token to the project through
// OS_PROJECT_NAME and OS_PROJECT_DOMAIN_NAME.
//
// spec.userRef names a KeystoneUser and spec.projectRef a KeystoneProject in
// the order's own namespace on the order's own cluster, both ordered from the
// same ControlPlane. A KeystoneService account or a project another namespace
// ordered cannot be referenced.
//
// The spec.namespaceAssignments entry for the order's namespace and cluster is
// the consent, and its allowedRoles lists the roles an order may request. A
// role outside the list reports RoleNotAllowed with the list named. Removing
// the entry, or the role from the list, freezes the order: nothing is
// projected, repaired or swept, and an assignment that already exists in
// Keystone stays. Deleting the order still tears it down.
//
// Two orders in one namespace with the same userRef, projectRef and role would
// project one Keystone assignment twice, and deleting one would unassign the
// other's role. The younger order reports DuplicateRoleAssignment naming the
// older one and projects nothing.
//
// While the order exists, deleting the referenced KeystoneUser or
// KeystoneProject holds on ReferencedByRoleAssignments. Deleting the order
// itself holds on ReferencedByApplicationCredentials while a
// KeystoneApplicationCredential in the same namespace names the same userRef
// and projectRef.
//
// The kind has no webhook, because a target cluster runs none: every admission
// rule is a schema rule. An empty spec.controlPlaneRef.namespace means the
// order's own namespace; the reconciler resolves the default.
type KeystoneRoleAssignment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeystoneRoleAssignmentSpec   `json:"spec"`
	Status KeystoneRoleAssignmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeystoneRoleAssignmentList contains a list of KeystoneRoleAssignment.
type KeystoneRoleAssignmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeystoneRoleAssignment `json:"items"`
}

// KeystoneOrderRef names an order in the same namespace on the same cluster.
type KeystoneOrderRef struct {
	// Name is the referenced order's metadata.name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
}

// KeystoneRoleAssignmentSpec defines the desired state of a
// KeystoneRoleAssignment.
type KeystoneRoleAssignmentSpec struct {
	// ControlPlaneRef names the ControlPlane whose identity plane the role is
	// assigned in. It must be the ControlPlane both referenced orders name.
	//
	// Both halves are frozen after creation. Re-pointing a live order would
	// strand the assignment it created on the old plane. Delete and re-create
	// the order to move it. The namespace rule spells the field __namespace__,
	// the escape Kubernetes CEL requires for a property named after a CEL
	// reserved word.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="controlPlaneRef.name is immutable; delete and re-create the KeystoneRoleAssignment to move it to another ControlPlane"
	// +kubebuilder:validation:XValidation:rule="(has(self.__namespace__) ? self.__namespace__ : '') == (has(oldSelf.__namespace__) ? oldSelf.__namespace__ : '')",message="controlPlaneRef.namespace is immutable; delete and re-create the KeystoneRoleAssignment to move it to another ControlPlane"
	ControlPlaneRef ControlPlaneRefSpec `json:"controlPlaneRef"`

	// UserRef names the KeystoneUser in the order's namespace whose user the
	// role is assigned to. It is frozen after creation.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="userRef is immutable; delete and re-create the KeystoneRoleAssignment to bind another user"
	UserRef KeystoneOrderRef `json:"userRef"`

	// ProjectRef names the KeystoneProject in the order's namespace the role is
	// assigned on. It is frozen after creation.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="projectRef is immutable; delete and re-create the KeystoneRoleAssignment to bind another project"
	ProjectRef KeystoneOrderRef `json:"projectRef"`

	// Role is the Keystone role name, matched against the allowedRoles of the
	// namespace's assignment entry by its case-sensitive name. The pattern and
	// caps mirror K-ORC's KeystoneName. It is frozen after creation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="role is immutable; delete and re-create the KeystoneRoleAssignment to assign another role"
	Role string `json:"role"`
}

// KeystoneRoleAssignmentStatus defines the observed state of a
// KeystoneRoleAssignment.
type KeystoneRoleAssignmentStatus struct {
	// Conditions are AssignmentReady and the aggregate Ready.
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

	// RoleID is the Keystone role id. Empty until the role is assigned.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	RoleID string `json:"roleID,omitempty"`

	// UserID is the Keystone id of the referenced user. Empty until the role is
	// assigned.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	UserID string `json:"userID,omitempty"`

	// ProjectID is the Keystone id of the referenced project. Empty until the
	// role is assigned.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	ProjectID string `json:"projectID,omitempty"`
}

func init() {
	SchemeBuilder.Register(&KeystoneRoleAssignment{}, &KeystoneRoleAssignmentList{})
}
