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
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.catalog.serviceType"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// KeystoneService registers one service against the identity plane of a
// ControlPlane: an optional catalog entry (spec.catalog) and an optional
// service account (spec.account), at least one of which must be set. The CR
// is mode-independent — the same declaration works against a Managed and an
// External ControlPlane. Its own metadata takes the roles the ControlPlane's
// inline surfaces give to per-entry fields: child resources and the consumer
// Secret derive their names from metadata.name, and credential delivery lands
// in the CR's own namespace.
//
// DECISION: the operator wiring (RBAC, manager setup, watches) lands in a
// follow-up package of the same effort, so the reconciler paces itself with
// requeue timers rather than watches until it arrives. It also resolves the
// effective user, domain and service names itself rather than trusting the
// defaulting webhook, because admission can be absent rather than merely
// unavailable (an unregistered webhook during install, a GitOps or etcd
// restore replaying stored objects).
type KeystoneService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeystoneServiceSpec   `json:"spec,omitempty"`
	Status KeystoneServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeystoneServiceList contains a list of KeystoneService.
type KeystoneServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeystoneService `json:"items"`
}

// KeystoneServiceSpec defines the desired state of a KeystoneService.
//
// The at-least-one-block rule holds at the schema layer so it binds even
// while there is no validating webhook: a CR with neither a catalog nor an
// account block declares nothing a reconciler could act on.
// +kubebuilder:validation:XValidation:rule="has(self.catalog) || has(self.account)",message="at least one of spec.catalog or spec.account must be set"
type KeystoneServiceSpec struct {
	// ControlPlaneRef names the ControlPlane whose identity plane this
	// service registers against.
	//
	// The reference is frozen after creation: every child this CR projects
	// authenticates through the referenced plane's admin credential, so
	// re-pointing a live registration would strand the Keystone user, project
	// and catalog row it already created on the old plane, owned by nothing.
	// Delete and re-create the CR to register against another ControlPlane.
	// The namespace half is frozen by the validating webhook instead of here:
	// an empty value means the CR's own namespace, and a CEL rule on a spec
	// field cannot read metadata.namespace to resolve it.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="controlPlaneRef.name is immutable; delete and re-create the KeystoneService to register against another ControlPlane"
	ControlPlaneRef ControlPlaneRefSpec `json:"controlPlaneRef"`

	// Catalog declares the service's catalog entry: one service row plus at
	// most one endpoint row per interface.
	// +optional
	Catalog *KeystoneServiceCatalogSpec `json:"catalog,omitempty"`

	// Account declares the service's Keystone account: a user with an
	// operator-generated, OpenBao-backed, rotatable password, its project,
	// and the roles assigned to it — the composite the ControlPlane's inline
	// service accounts declare today, delivered into this CR's own namespace.
	// +optional
	Account *KeystoneServiceAccountSpec `json:"account,omitempty"`
}

// ControlPlaneRefSpec references a ControlPlane CR by name and, optionally,
// namespace.
type ControlPlaneRefSpec struct {
	// Name is the referenced ControlPlane CR's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the referenced ControlPlane CR's namespace. Empty (the
	// default) means the KeystoneService's own namespace. The referenced
	// ControlPlane does not have to exist at admission time (GitOps ordering:
	// the registration may be applied before the ControlPlane); a dangling
	// reference surfaces as a status condition, not an admission error.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
}

// KeystoneServiceCatalogSpec declares the catalog entry a KeystoneService
// registers: one service row plus at most one endpoint row per interface.
type KeystoneServiceCatalogSpec struct {
	// ServiceType is the OpenStack service type (e.g. "image", "compute"). It
	// is embedded verbatim in the names of the child K-ORC CRs, hence the
	// DNS-1123 label shape. "identity" is rejected: the identity catalog
	// entry is ControlPlane-owned in both modes (created in Managed, imported
	// in External) and cannot be registered through a KeystoneService.
	//
	// It is frozen after creation, together with the effective service name.
	// The collision probe that decides whether a pre-existing row of this type
	// and name may be taken over runs only while no managed Service child
	// exists, so an in-place edit re-shapes the registered row without ever
	// re-probing: an exact match would silently adopt a row the registration
	// does not own, a near match would duplicate one. Delete and re-create the
	// CR, which runs the probe again.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self != 'identity'",message="the identity catalog entry is ControlPlane-owned and cannot be registered through a KeystoneService"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serviceType is immutable; delete and re-create the KeystoneService to register a different service type"
	ServiceType string `json:"serviceType"`

	// ServiceName optionally overrides the catalog service name. When empty
	// the registration falls back to the CR's metadata.name — NOT to K-ORC's
	// own default, which is the child CR's name: that name carries an
	// operator-generated uniqueness suffix and would leak into the catalog as
	// the advertised service name.
	//
	// The pattern and the caps mirror K-ORC's own OpenStackName, which the
	// name is cast to on the child Service CR: a name admitted here can
	// therefore never be rejected downstream by the K-ORC CRD, which would
	// wedge the reconcile in an exponential backoff loop that no
	// KeystoneService field error explains.
	//
	// It is frozen after creation for the reason serviceType above is. The
	// freeze the validating webhook applies is the load-bearing one: it
	// compares the EFFECTIVE name, so setting an explicit serviceName on a
	// registration that relied on the metadata.name fallback is a rename too,
	// which this rule alone cannot see (a transition rule does not evaluate
	// when the old object left the field unset).
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serviceName is immutable; delete and re-create the KeystoneService to rename its catalog entry"
	ServiceName string `json:"serviceName,omitempty"`

	// Adopt is the explicit consent that a pre-existing catalog service row of
	// this type and name may be taken over. The collision posture mirrors the
	// account block's and is fail-loudly by default: a declared catalog entry
	// whose service row already exists in Keystone surfaces a collision
	// condition and is never touched. Setting adopt=true opts into operator
	// ownership of that row's lifecycle — an adopted row is a managed K-ORC
	// Service, so it is DELETED from the catalog when the KeystoneService is
	// deleted, exactly like one the operator created.
	//
	// Blind creation is not an option the reconciler offers: K-ORC's service
	// actuator matches an existing row on name and type, so a managed create
	// silently adopts an exact match anyway, while a near-match duplicates the
	// row (Keystone enforces no uniqueness on service names). Adopt only what
	// the registration should own.
	// +optional
	Adopt bool `json:"adopt,omitempty"`

	// Endpoints declares the endpoint rows registered for this entry, at most
	// one per interface (apiserver-enforced via listType=map). An entry with
	// no endpoints registers the service row alone.
	// +optional
	// +listType=map
	// +listMapKey=interface
	Endpoints []KeystoneServiceEndpointSpec `json:"endpoints,omitempty"`
}

// KeystoneServiceEndpointSpec declares one endpoint row of a registered
// catalog entry.
type KeystoneServiceEndpointSpec struct {
	// Interface is the catalog interface this endpoint is published under. It
	// keys the listType=map Endpoints list.
	Interface ExternalEndpointType `json:"interface"`

	// URL is the endpoint URL registered in the catalog. maxLength mirrors
	// K-ORC's own EndpointResourceSpec.URL cap, so a URL admitted here can
	// never be rejected downstream by the K-ORC CRD.
	// +kubebuilder:validation:Pattern=`^https?://[^\s/]+`
	// +kubebuilder:validation:MaxLength=1024
	URL string `json:"url"`
}

// KeystoneServiceAccountSpec declares the composite OpenStack service account
// a KeystoneService registers: a K-ORC User with an operator-generated,
// OpenBao-backed, rotatable password, its project (referenced or created),
// and the roles assigned to it. It is the ControlPlane's inline
// ServiceAccountSpec surface minus that surface's name and targetNamespace
// fields: the CR's metadata.name keys the child resources and the consumer
// Secret, and the CR's own namespace is the delivery namespace.
type KeystoneServiceAccountSpec struct {
	// UserName is the OpenStack user name managed in Keystone. Empty (the
	// default) means the CR's metadata.name; the defaulting webhook
	// materializes that default. The pattern and caps mirror K-ORC's own
	// OpenStackName (a comma would only move the rejection to the K-ORC CRD,
	// which wedges the reconcile in an exponential backoff no KeystoneService
	// field error explains).
	//
	// It is frozen after creation: the name identifies a live Keystone user,
	// so an in-place edit would mint a second user and strand the first one,
	// still holding the password its consumers authenticate with. Delete and
	// re-create the CR to rename it. The validating webhook compares the
	// EFFECTIVE name, which is what admits the one benign transition this rule
	// cannot see: materializing the metadata.name fallback onto a CR stored
	// before the defaulting webhook existed.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="userName is immutable; delete and re-create the KeystoneService to rename its user"
	UserName string `json:"userName,omitempty"`

	// DomainName is the OpenStack domain the user and project live in. When
	// empty the reconciler resolves it to the referenced ControlPlane's
	// effective admin domain (spec.korc.adminCredential.domainName). The
	// pattern and caps mirror K-ORC's KeystoneName/OpenStackName filters.
	//
	// It is frozen after creation, because moving a live user and project to
	// another domain is a re-creation, not an edit. The webhook compares the
	// declared value rather than the effective one: resolving the fallback
	// needs the referenced ControlPlane's admin domain, which admission cannot
	// read, so setting an explicit domainName on a CR that relied on the
	// fallback is rejected even where it names that same domain. Delete and
	// re-create the CR to move it.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="domainName is immutable; delete and re-create the KeystoneService to move it to another domain"
	DomainName string `json:"domainName,omitempty"`

	// Adopt is the explicit consent that a pre-existing Keystone user of this
	// name may be taken over. The collision posture is conservative and
	// fail-loudly by default: a declared account whose user already exists in
	// Keystone surfaces a collision condition and is never touched. Setting
	// adopt=true opts into a PASSWORD TAKEOVER of that account — the operator
	// overwrites its password with a generated one — AND into operator
	// ownership of its lifecycle: an adopted user is a managed K-ORC User, so
	// it is DELETED from Keystone when the KeystoneService is deleted,
	// exactly like one the operator created. Adopt only what the registration
	// should own.
	// +optional
	Adopt bool `json:"adopt,omitempty"`

	// Project is the OpenStack project the service user is associated with,
	// either referenced (the default) or created and owned by the
	// registration.
	//
	// Both leaves are frozen after creation. Re-pointing name would leave the
	// role assignments the registration minted behind on the old project,
	// and flipping create would either orphan a project the registration owns
	// or adopt one it does not. The rules live on this field rather than on
	// ServiceAccountProjectSpec because the ControlPlane's inline service
	// accounts share that type and enforce the same freeze in their own
	// webhook arm.
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="project.name is immutable; delete and re-create the KeystoneService to re-point its project"
	// +kubebuilder:validation:XValidation:rule="(has(self.create) && self.create) == (has(oldSelf.create) && oldSelf.create)",message="project.create is immutable; a managed<->referenced flip would orphan or adopt the live project"
	Project ServiceAccountProjectSpec `json:"project"`

	// Roles are the OpenStack role names assigned to the user on the project.
	// Each declared role is projected as one UNMANAGED K-ORC Role import (the
	// role is referenced by name, never created or deleted — Keystone roles
	// are global) plus one MANAGED K-ORC RoleAssignment binding that role to
	// the user on the project.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=255
	// +kubebuilder:validation:items:Pattern=`^[^,]+$`
	Roles []string `json:"roles,omitempty"`

	// Rotation tunes how the account's password is rotated. When nil the mode
	// defaults to Manual (on-demand rotation via a CredentialRotation CR).
	// +optional
	Rotation *ServiceAccountRotationSpec `json:"rotation,omitempty"`
}

// KeystoneServiceStatus defines the observed state of a KeystoneService.
type KeystoneServiceStatus struct {
	// Conditions represent the latest available observations of the
	// registration state: one condition per declared block plus the aggregate
	// Ready. Upsert via the shared conditions helper.
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

	// Catalog reports the observed state of the declared catalog entry. It is
	// nil while no catalog block is declared.
	// +optional
	Catalog *KeystoneServiceCatalogStatus `json:"catalog,omitempty"`

	// Account reports the observed state of the declared service account. It
	// is nil while no account block is declared.
	// +optional
	Account *KeystoneServiceAccountStatus `json:"account,omitempty"`
}

// KeystoneServiceCatalogStatus reports the observed state of a registered
// catalog entry.
type KeystoneServiceCatalogStatus struct {
	// ServiceID is the OpenStack service id K-ORC resolved (or created). Empty
	// until the Service child is Available.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	ServiceID string `json:"serviceID,omitempty"`

	// Endpoints reports the registered endpoint rows, one per declared
	// interface.
	// +optional
	// +listType=map
	// +listMapKey=interface
	Endpoints []KeystoneServiceEndpointStatus `json:"endpoints,omitempty"`
}

// KeystoneServiceEndpointStatus reports the observed state of one registered
// endpoint row.
type KeystoneServiceEndpointStatus struct {
	// Interface is the catalog interface the endpoint is published under. It
	// keys the listType=map Endpoints list.
	Interface ExternalEndpointType `json:"interface"`

	// ID is the OpenStack endpoint id K-ORC resolved (or created). Empty until
	// the Endpoint child is Available.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	ID string `json:"id,omitempty"`
}

// KeystoneServiceAccountStatus reports the observed state of a registered
// service account. It is the per-CR twin of the ControlPlane's inline
// ServiceAccountStatus, minus that type's name key (a CR carries one account)
// and secretNamespace (delivery is always the CR's own namespace).
type KeystoneServiceAccountStatus struct {
	// SecretName is the name of the materialized Secret carrying the account's
	// credentials (key "password" and a ready-to-use "clouds.yaml"), in the
	// KeystoneService's own namespace. It is the documented, stable handle
	// consumers read the credentials from.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// UserID is the OpenStack user id K-ORC resolved (or created). Empty until
	// the User is Available.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	UserID string `json:"userID,omitempty"`

	// ProjectID is the OpenStack project id K-ORC resolved (or created). Empty
	// until the Project is Available.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	ProjectID string `json:"projectID,omitempty"`

	// PasswordGeneration is the monotonically increasing generation of the
	// password currently applied to the user. It increments on every rotation.
	// +optional
	PasswordGeneration int64 `json:"passwordGeneration,omitempty"`

	// LastPasswordRotation is the timestamp of the last successful password
	// rotation.
	// +optional
	LastPasswordRotation *metav1.Time `json:"lastPasswordRotation,omitempty"`
}

func init() {
	SchemeBuilder.Register(&KeystoneService{}, &KeystoneServiceList{})
}
