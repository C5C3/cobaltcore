// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The deletion policies of a MariaDBDatabase.
const (
	// MariaDBDatabaseDeletionPolicyRetain keeps the schema and its data when the
	// order is deleted. It is the default.
	MariaDBDatabaseDeletionPolicyRetain = "Retain"
	// MariaDBDatabaseDeletionPolicyDelete drops the schema and its data with the
	// order.
	MariaDBDatabaseDeletionPolicyDelete = "Delete"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="ControlPlane",type="string",JSONPath=".spec.controlPlaneRef.name"
// +kubebuilder:printcolumn:name="Database",type="string",JSONPath=".status.databaseName"
// +kubebuilder:printcolumn:name="Refreshed",type="date",JSONPath=".status.credentialsRefreshedAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 bytes; it is carried as a label value on the order's children"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.databaseName) ? self.spec.databaseName : self.metadata.name.replace('-', '_')) == (has(oldSelf.spec.databaseName) ? oldSelf.spec.databaseName : oldSelf.metadata.name.replace('-', '_'))",message="databaseName is immutable; delete and re-create the MariaDBDatabase to order another database"
// +kubebuilder:validation:XValidation:rule="has(self.spec.databaseName) || self.metadata.name.matches('^[a-z0-9-]{1,64}$')",message="metadata.name is not a MySQL identifier once its dashes are underscores; set spec.databaseName"

// MariaDBDatabase orders one MariaDB database, and one user with grants on it,
// from a namespace the referenced ControlPlane assigns to a service owner. The
// operator creates the schema on the ControlPlane's shared managed MariaDB,
// writes an OpenBao database-engine role that issues users holding ALL
// PRIVILEGES on that schema and nothing else, and copies each issued
// credential into the Secret <metadata.name>-credentials beside the order, with
// the keys host, port, database, username, password and, when the database
// runs TLS, ca.crt.
//
// The order lives in the assigned namespace, on the cluster that namespace is
// assigned on: the management cluster or a registered target cluster. Status,
// the finalizer and the delivered Secret are written on the order's own
// cluster. The ESO generator and ExternalSecret that read the credential live
// in the ControlPlane's namespace on the management cluster and carry ownership
// labels; the mariadb-operator Database CR lives beside the MariaDB, in
// Keystone's namespace on Keystone's cluster.
//
// The spec.namespaceAssignments entry for the order's namespace and cluster is
// the only consent. Without an entry the order reports NamespaceNotAssigned and
// is frozen: nothing is provisioned, delivered, repaired or swept, and what it
// created stays. Deleting the order still tears everything down.
//
// The database is always the ControlPlane's shared managed database, the one
// its Keystone uses, because the OpenBao database engine holds a connection for
// that database only. An order is refused with DynamicCredentialsUnavailable
// when that database does not issue dynamic credentials: an External-mode
// ControlPlane, a brownfield database, a dedicated Keystone database and
// credentialsMode Static all refuse. A schema the order did not create is never
// taken over: a MariaDB system schema is refused with DatabaseNameReserved, and
// a name another Database CR on the same MariaDB carries with
// DatabaseCollision.
//
// The c5c3-operator writes the engine role itself through its own OpenBao
// identity (the c5c3-operator auth role and policy), which reaches the order
// roles and their leases and nothing else. The cadence is fixed and equals the
// ControlPlane's own services': ESO reads a fresh credential every 24 hours,
// and OpenBao drops each issued user when its 48-hour lease ends. A consumer
// re-reads the Secret at least once a day; a superseded login keeps working for
// 24 hours after the Secret moved on.
//
// spec.deletionPolicy decides what deleting the order does to the data. The
// user, its leases and the engine role are always removed with the order; the
// schema and its data are dropped only under Delete, and stay under Retain, the
// default.
//
// An order on the cluster the MariaDB runs on receives the in-cluster Service
// address. An order on any other cluster receives the ControlPlane's
// spec.infrastructure.publishedDatabaseEndpoint and, without one, is refused
// with DatabaseNotPublished.
//
// The kind has no webhook, because a target cluster runs none: every
// admission rule is a schema rule. An empty spec.databaseName means
// metadata.name with every dash replaced by an underscore, and an empty
// spec.controlPlaneRef.namespace means the order's own namespace; the
// reconciler resolves both defaults. The effective database name is frozen
// after creation.
type MariaDBDatabase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MariaDBDatabaseSpec   `json:"spec"`
	Status MariaDBDatabaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MariaDBDatabaseList contains a list of MariaDBDatabase.
type MariaDBDatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MariaDBDatabase `json:"items"`
}

// MariaDBDatabaseSpec defines the desired state of a MariaDBDatabase.
type MariaDBDatabaseSpec struct {
	// ControlPlaneRef names the ControlPlane whose shared MariaDB the database is
	// created on. An order on a target cluster sets the namespace: the
	// ControlPlane lives on the management cluster, in a namespace the order's
	// own namespace does not name.
	//
	// Both halves are frozen after creation. Re-pointing a live order would
	// strand the database it created on the old plane. Delete and re-create the
	// order to move it. The namespace rule spells the field __namespace__, the
	// escape Kubernetes CEL requires for a property named after a CEL reserved
	// word.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="controlPlaneRef.name is immutable; delete and re-create the MariaDBDatabase to move it to another ControlPlane"
	// +kubebuilder:validation:XValidation:rule="(has(self.__namespace__) ? self.__namespace__ : '') == (has(oldSelf.__namespace__) ? oldSelf.__namespace__ : '')",message="controlPlaneRef.namespace is immutable; delete and re-create the MariaDBDatabase to move it to another ControlPlane"
	ControlPlaneRef ControlPlaneRefSpec `json:"controlPlaneRef"`

	// DatabaseName is the SQL schema name. Empty (the default) means the
	// order's metadata.name with every dash replaced by an underscore. The
	// bounds are the MySQL identifier character set and length limit.
	//
	// The effective name is frozen after creation by a rule on the whole object,
	// so setting databaseName to a value other than the default later is
	// rejected as well. The name identifies a live schema, and an edit would
	// create a second one.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_]+$`
	DatabaseName string `json:"databaseName,omitempty"`

	// DeletionPolicy decides whether deleting the order drops the schema and its
	// data. Retain (the default) keeps them; Delete drops them. The issued user,
	// its leases and the OpenBao role are removed under both. The field may be
	// changed at any time; the value in force when the order is deleted applies.
	// +optional
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// MariaDBDatabaseStatus defines the observed state of a MariaDBDatabase.
type MariaDBDatabaseStatus struct {
	// Conditions are DatabaseReady, DeliveryReady and the aggregate Ready.
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

	// DatabaseName is the resolved SQL schema name.
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`

	// Host is the database host as delivered.
	// +optional
	Host string `json:"host,omitempty"`

	// Port is the database port as delivered.
	// +optional
	Port int32 `json:"port,omitempty"`

	// RoleName is the OpenBao database-engine role that issues the order's
	// users.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	RoleName string `json:"roleName,omitempty"`

	// CredentialsRefreshedAt is when ESO last issued the credential the
	// delivered Secret carries.
	// +optional
	CredentialsRefreshedAt *metav1.Time `json:"credentialsRefreshedAt,omitempty"`

	// SecretName is the name of the delivered Secret in the order's namespace,
	// <metadata.name>-credentials.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// SecretKeys lists the keys of the delivered Secret once it carries the
	// current credential: host, port, database, username, password and, when the
	// database runs TLS, ca.crt.
	// +optional
	// +listType=atomic
	SecretKeys []string `json:"secretKeys,omitempty"`
}

func init() {
	SchemeBuilder.Register(&MariaDBDatabase{}, &MariaDBDatabaseList{})
}
