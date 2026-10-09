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
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.serviceType"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 bytes; it is carried as a label value on the order's children"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.serviceName) ? self.spec.serviceName : self.metadata.name) == (has(oldSelf.spec.serviceName) ? oldSelf.spec.serviceName : oldSelf.metadata.name)",message="serviceName is immutable; delete and re-create the KeystoneCatalogEntry to rename its catalog entry"

// KeystoneCatalogEntry orders one Keystone catalog entry from a namespace the
// referenced ControlPlane assigns to a service owner: a service row of a type
// and name, and at most one endpoint row per interface, registered in the
// ControlPlane's region. It reports the service and endpoint ids in status.
//
// The order lives in the assigned namespace, on the cluster that namespace is
// assigned on. Status and the finalizer are written there. The K-ORC Service,
// Endpoints and Region import live in the ControlPlane's namespace on the
// management cluster and carry ownership labels.
//
// The spec.namespaceAssignments entry for the order's namespace and cluster is
// the consent, and the entry must set allowCatalogEntries as well: a catalog
// row is visible to every cloud user. Without an entry the order reports
// NamespaceNotAssigned, and without the flag CatalogNotAllowed. Either freezes
// the order: nothing is registered, repaired or swept, and what was created
// stays. Deleting the order still tears it down.
//
// Every service type but identity may be registered, the built-in ones
// included; identity is ControlPlane-owned. A row of the same type and name
// that the order did not create is refused with ServiceCollision and never
// taken over.
//
// The type and the effective name are frozen after creation, because the
// collision probe runs only while no managed Service exists. The endpoints may
// change: an interface the spec no longer declares is removed.
//
// The endpoint types are KeystoneService's, reused rather than copied, so the
// two kinds admit the same endpoint rows.
//
// The kind has no webhook, because a target cluster runs none: every admission
// rule is a schema rule. An empty spec.serviceName means metadata.name, and an
// empty spec.controlPlaneRef.namespace means the order's own namespace; the
// reconciler resolves both defaults.
type KeystoneCatalogEntry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeystoneCatalogEntrySpec   `json:"spec"`
	Status KeystoneCatalogEntryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeystoneCatalogEntryList contains a list of KeystoneCatalogEntry.
type KeystoneCatalogEntryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeystoneCatalogEntry `json:"items"`
}

// KeystoneCatalogEntrySpec defines the desired state of a KeystoneCatalogEntry.
type KeystoneCatalogEntrySpec struct {
	// ControlPlaneRef names the ControlPlane whose catalog the entry is
	// registered in. An order on a target cluster sets the namespace: the
	// ControlPlane lives on the management cluster, in a namespace the order's
	// own namespace does not name.
	//
	// Both halves are frozen after creation. Re-pointing a live order would
	// strand the catalog rows it created on the old plane. Delete and re-create
	// the order to move it. The namespace rule spells the field __namespace__,
	// the escape Kubernetes CEL requires for a property named after a CEL
	// reserved word.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="controlPlaneRef.name is immutable; delete and re-create the KeystoneCatalogEntry to move it to another ControlPlane"
	// +kubebuilder:validation:XValidation:rule="(has(self.__namespace__) ? self.__namespace__ : '') == (has(oldSelf.__namespace__) ? oldSelf.__namespace__ : '')",message="controlPlaneRef.namespace is immutable; delete and re-create the KeystoneCatalogEntry to move it to another ControlPlane"
	ControlPlaneRef ControlPlaneRefSpec `json:"controlPlaneRef"`

	// ServiceType is the OpenStack service type (e.g. "dns"). It has the DNS-1123
	// label shape. "identity" is rejected: the identity catalog entry is
	// ControlPlane-owned. It is frozen after creation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self != 'identity'",message="the identity catalog entry is ControlPlane-owned and cannot be registered through a KeystoneCatalogEntry"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serviceType is immutable; delete and re-create the KeystoneCatalogEntry to register a different service type"
	ServiceType string `json:"serviceType"`

	// ServiceName is the catalog service name. Empty (the default) means the
	// order's metadata.name. The pattern and caps mirror K-ORC's OpenStackName.
	//
	// The effective name is frozen after creation by a rule on the whole object,
	// so setting serviceName to a value other than metadata.name later is
	// rejected as well.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	ServiceName string `json:"serviceName,omitempty"`

	// Endpoints declares the endpoint rows registered for this entry, at most
	// one per interface. An entry with no endpoints registers the service row
	// alone. The list may change after creation.
	// +optional
	// +listType=map
	// +listMapKey=interface
	Endpoints []KeystoneServiceEndpointSpec `json:"endpoints,omitempty"`
}

// KeystoneCatalogEntryStatus defines the observed state of a
// KeystoneCatalogEntry.
type KeystoneCatalogEntryStatus struct {
	// Conditions are CatalogReady and the aggregate Ready.
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

	// ServiceID is the Keystone service id. Empty until the service row is
	// registered.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	ServiceID string `json:"serviceID,omitempty"`

	// ServiceName is the effective catalog service name.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	ServiceName string `json:"serviceName,omitempty"`

	// Endpoints reports one row per declared interface.
	// +optional
	// +listType=map
	// +listMapKey=interface
	Endpoints []KeystoneServiceEndpointStatus `json:"endpoints,omitempty"`
}

func init() {
	SchemeBuilder.Register(&KeystoneCatalogEntry{}, &KeystoneCatalogEntryList{})
}
