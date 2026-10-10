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
// +kubebuilder:printcolumn:name="Project",type="string",JSONPath=".status.projectName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 bytes; it is carried as a label value on the order's children"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.projectName) ? self.spec.projectName : self.metadata.name) == (has(oldSelf.spec.projectName) ? oldSelf.spec.projectName : oldSelf.metadata.name)",message="projectName is immutable; delete and re-create the KeystoneProject to rename its project"

// KeystoneProject orders one Keystone project from a namespace the referenced
// ControlPlane assigns to a service owner. The operator creates the project in
// the ControlPlane's admin domain, where every ordered user lives as well, and
// reports its id in status. No Secret is delivered: a user ordered beside it
// scopes a token to the project once a KeystoneRoleAssignment grants it a role
// there.
//
// The order lives in the assigned namespace, on the cluster that namespace is
// assigned on. Status and the finalizer are written there. The K-ORC Project
// lives in the ControlPlane's namespace on the management cluster and carries
// ownership labels.
//
// The spec.namespaceAssignments entry for the order's namespace and cluster is
// the only consent. Without an entry the order reports NamespaceNotAssigned and
// is frozen: nothing is provisioned, repaired or swept, and the project stays.
// Deleting the order still tears it down.
//
// A project of the same name in the admin domain that the order did not create
// is refused with ProjectCollision and never taken over, and so are the admin
// project and the built-in service projects the ControlPlane reserves.
//
// Deleting the order holds while a KeystoneRoleAssignment in the same namespace
// names it as projectRef, reporting ReferencedByRoleAssignments, and while a
// KeystoneApplicationCredential does, reporting
// ReferencedByApplicationCredentials. The project stays until those orders are
// deleted.
//
// The kind has no webhook, because a target cluster runs none: every admission
// rule is a schema rule. An empty spec.projectName means metadata.name, and an
// empty spec.controlPlaneRef.namespace means the order's own namespace; the
// reconciler resolves both defaults.
type KeystoneProject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeystoneProjectSpec   `json:"spec"`
	Status KeystoneProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeystoneProjectList contains a list of KeystoneProject.
type KeystoneProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeystoneProject `json:"items"`
}

// KeystoneProjectSpec defines the desired state of a KeystoneProject.
type KeystoneProjectSpec struct {
	// ControlPlaneRef names the ControlPlane whose identity plane the project is
	// created in. An order on a target cluster sets the namespace: the
	// ControlPlane lives on the management cluster, in a namespace the order's
	// own namespace does not name.
	//
	// Both halves are frozen after creation. Re-pointing a live order would
	// strand the Keystone project it created on the old plane. Delete and
	// re-create the order to move it. The namespace rule spells the field
	// __namespace__, the escape Kubernetes CEL requires for a property named
	// after a CEL reserved word.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="controlPlaneRef.name is immutable; delete and re-create the KeystoneProject to move it to another ControlPlane"
	// +kubebuilder:validation:XValidation:rule="(has(self.__namespace__) ? self.__namespace__ : '') == (has(oldSelf.__namespace__) ? oldSelf.__namespace__ : '')",message="controlPlaneRef.namespace is immutable; delete and re-create the KeystoneProject to move it to another ControlPlane"
	ControlPlaneRef ControlPlaneRefSpec `json:"controlPlaneRef"`

	// ProjectName is the Keystone project name. Empty (the default) means the
	// order's metadata.name. The pattern and caps mirror K-ORC's KeystoneName.
	//
	// The effective name is frozen after creation by a rule on the whole object,
	// so setting projectName to a value other than metadata.name later is
	// rejected as well. The name identifies a live Keystone project, and an edit
	// would create a second one.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	ProjectName string `json:"projectName,omitempty"`
}

// KeystoneProjectStatus defines the observed state of a KeystoneProject.
type KeystoneProjectStatus struct {
	// Conditions are ProjectReady and the aggregate Ready.
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

	// ProjectID is the Keystone project id. Empty until the project is
	// provisioned.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	ProjectID string `json:"projectID,omitempty"`

	// ProjectName is the effective project name, once provisioned.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	ProjectName string `json:"projectName,omitempty"`

	// DomainName is the domain the project lives in, the ControlPlane's admin
	// domain, once provisioned.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	DomainName string `json:"domainName,omitempty"`
}

func init() {
	SchemeBuilder.Register(&KeystoneProject{}, &KeystoneProjectList{})
}
