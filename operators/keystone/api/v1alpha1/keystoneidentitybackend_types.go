// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Domain",type="string",JSONPath=".spec.domain.name"
// +kubebuilder:printcolumn:name="Keystone",type="string",JSONPath=".spec.keystoneRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// KeystoneIdentityBackend is the Schema for the keystoneidentitybackends API.
// One CR attaches to a Keystone CR via spec.keystoneRef and describes one
// external identity backend (Phase 1: an LDAP/AD-backed domain). A dedicated
// controller owns the backend lifecycle — finalizer, domain provisioning,
// per-backend conditions — while the keystone-side identitybackends
// sub-reconciler aggregates all attached, DomainReady backends into one
// content-hashed domains Secret mounted at /etc/keystone/domains/.
type KeystoneIdentityBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeystoneIdentityBackendSpec   `json:"spec,omitempty"`
	Status KeystoneIdentityBackendStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeystoneIdentityBackendList contains a list of KeystoneIdentityBackend.
type KeystoneIdentityBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeystoneIdentityBackend `json:"items"`
}

// IdentityBackendType enumerates the supported backend drivers. Phase 1
// ships LDAP only; the federation phases extend the enum (OIDC, SAML).
// +kubebuilder:validation:Enum=LDAP
type IdentityBackendType string

// IdentityBackendTypeLDAP selects the keystone LDAP identity driver.
const IdentityBackendTypeLDAP IdentityBackendType = "LDAP"

// KeystoneIdentityBackendSpec defines the desired state of
// KeystoneIdentityBackend.
//
// The keystoneRef transition rule (evaluated only on UPDATE) makes the
// attachment immutable: re-pointing a backend at a different Keystone would
// leave the old deployment with a provisioned domain nothing manages anymore
// and race the config projection on the new one. Delete and recreate instead.
// The type/ldap union rule enforces "exactly one backend block per type" at
// the schema layer so it holds even when the validating webhook is down.
// +kubebuilder:validation:XValidation:rule="self.keystoneRef.name == oldSelf.keystoneRef.name",message="keystoneRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="type is immutable"
// +kubebuilder:validation:XValidation:rule="(self.type == 'LDAP') == has(self.ldap)",message="exactly one backend block matching spec.type must be set (type LDAP requires spec.ldap)"
type KeystoneIdentityBackendSpec struct {
	// KeystoneRef names the Keystone CR in the same namespace this backend
	// attaches to. The referenced CR does not have to exist at admission time
	// (GitOps ordering: the backend may be applied before the Keystone CR);
	// a dangling reference surfaces as DomainReady=False/KeystoneNotFound.
	KeystoneRef KeystoneRefSpec `json:"keystoneRef"`

	// Domain describes the Keystone domain this backend provides.
	Domain DomainSpec `json:"domain"`

	// Type selects the backend driver. Phase 1 supports LDAP only.
	Type IdentityBackendType `json:"type"`

	// LDAP configures the LDAP/AD connection, tree layout, and attribute
	// mapping. Required exactly when type is LDAP (union rule above).
	// +optional
	LDAP *LDAPBackendSpec `json:"ldap,omitempty"`

	// ExtraOptions provides free-form [ldap] section options not covered by
	// the typed fields, keyed by bare option name (e.g. "page_size"). Options
	// rendered from typed fields, the driver/domain-config wiring, and — when
	// readOnly is true — the write-enabling user_allow_*/group_allow_* options
	// are rejected by the validating webhook so the escape hatch cannot
	// silently contradict the typed spec.
	// +optional
	ExtraOptions map[string]string `json:"extraOptions,omitempty"`
}

// KeystoneRefSpec references a Keystone CR by name in the same namespace.
// Modeled as a dedicated struct (rather than corev1.LocalObjectReference) so
// the name carries the same MinLength schema guard as the shared
// commonv1.SecretRefSpec.
type KeystoneRefSpec struct {
	// Name is the referenced Keystone CR's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// DomainMode selects how the referenced Keystone domain comes into being.
// +kubebuilder:validation:Enum=Manage;Adopt
type DomainMode string

const (
	// DomainModeManage lets the controller create (and on Delete policy,
	// disable + delete) the domain through the identity API.
	DomainModeManage DomainMode = "Manage"
	// DomainModeAdopt resolves a pre-existing domain by name and never
	// mutates it; adopted domains are always retained on deletion.
	DomainModeAdopt DomainMode = "Adopt"
)

// DomainDeletionPolicy controls what happens to a managed domain when the
// backend CR is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type DomainDeletionPolicy string

const (
	// DomainDeletionPolicyRetain leaves the domain in place (default).
	DomainDeletionPolicyRetain DomainDeletionPolicy = "Retain"
	// DomainDeletionPolicyDelete disables the domain and then deletes it.
	DomainDeletionPolicyDelete DomainDeletionPolicy = "Delete"
)

// DomainSpec describes the Keystone domain backed by this CR.
type DomainSpec struct {
	// Name is the Keystone domain name. Immutable after create: renaming
	// would strand the provisioned domain and its per-domain config file.
	// "default" (any case) is rejected — the Default domain hosts the
	// SQL-backed service users and the bootstrap admin (BootstrapSpec has no
	// domain knob, so the bootstrap admin always lives in Default) and must
	// never be re-pointed at an external directory, which could lock out
	// every service account.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="domain.name is immutable"
	// +kubebuilder:validation:XValidation:rule="self.lowerAscii() != 'default'",message="the Default domain hosts the SQL-backed service users and the bootstrap admin and must never be backed by an external identity backend"
	Name string `json:"name"`

	// Mode selects Manage (controller creates the domain) or Adopt
	// (controller resolves a pre-existing domain by name and never mutates
	// it). Immutable after create: flipping the mode would change deletion
	// semantics for a domain that was provisioned under the old contract.
	// +kubebuilder:default=Manage
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="domain.mode is immutable"
	// +optional
	Mode DomainMode `json:"mode,omitempty"`

	// DeletionPolicy controls what happens to a managed domain when this CR
	// is deleted: Retain (default) leaves it in place, Delete disables it and
	// then deletes it. Adopted domains are always retained regardless of this
	// field. Mutable so operators can decide at teardown time.
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy DomainDeletionPolicy `json:"deletionPolicy,omitempty"`

	// Description is projected onto the Keystone domain in Manage mode.
	// +optional
	Description string `json:"description,omitempty"`
}

// LDAPBackendSpec configures the keystone LDAP identity driver for one
// domain. Only user-set optional fields are rendered into the per-domain
// config, so upstream keystone defaults apply for everything left unset.
type LDAPBackendSpec struct {
	// URL is the LDAP server URL (ldap:// or ldaps://).
	// +kubebuilder:validation:Pattern=`^ldaps?://`
	URL string `json:"url"`

	// BindCredentialsSecretRef references the Secret holding the bind
	// credentials under the fixed data keys "username" (the bind DN) and
	// "password". The key field must stay empty — the two data keys are
	// fixed by contract (webhook-enforced).
	BindCredentialsSecretRef commonv1.SecretRefSpec `json:"bindCredentialsSecretRef"`

	// Suffix is the LDAP suffix (base DN), e.g. "dc=example,dc=com".
	// +kubebuilder:validation:MinLength=1
	Suffix string `json:"suffix"`

	// Users describes the user tree layout and attribute mapping.
	Users LDAPUserSpec `json:"users"`

	// Groups optionally describes the group tree layout and attribute
	// mapping. When nil, no group_* options are rendered and keystone's
	// defaults apply.
	// +optional
	Groups *LDAPGroupSpec `json:"groups,omitempty"`

	// ReadOnly forces user_allow_create/update/delete and
	// group_allow_create/update/delete to false so keystone can never write
	// into the corporate directory. Defaults to true; setting it to false is
	// an explicit opt-in to a writable backend.
	// +kubebuilder:default=true
	// +optional
	ReadOnly *bool `json:"readOnly,omitempty"`

	// TLS configures certificate verification for ldaps:// / STARTTLS
	// connections.
	// +optional
	TLS *LDAPTLSSpec `json:"tls,omitempty"`

	// Pool configures the keystone-side LDAP connection pool.
	// +optional
	Pool *LDAPPoolSpec `json:"pool,omitempty"`
}

// LDAPUserSpec describes the LDAP user tree and attribute mapping. Optional
// fields are only rendered when set, so upstream keystone defaults apply
// otherwise.
type LDAPUserSpec struct {
	// TreeDN is the search base for users, e.g. "ou=people,dc=example,dc=com".
	// +kubebuilder:validation:MinLength=1
	TreeDN string `json:"treeDN"`

	// Filter is an additional LDAP filter applied to user searches.
	// +optional
	Filter string `json:"filter,omitempty"`

	// ObjectClass is the LDAP objectClass for users (keystone default:
	// inetOrgPerson).
	// +optional
	ObjectClass string `json:"objectClass,omitempty"`

	// IDAttribute maps the keystone user ID (keystone default: cn).
	// +optional
	IDAttribute string `json:"idAttribute,omitempty"`

	// NameAttribute maps the keystone user name (keystone default: sn).
	// +optional
	NameAttribute string `json:"nameAttribute,omitempty"`

	// MailAttribute maps the keystone user email (keystone default: mail).
	// +optional
	MailAttribute string `json:"mailAttribute,omitempty"`
}

// LDAPGroupSpec describes the LDAP group tree and attribute mapping.
type LDAPGroupSpec struct {
	// TreeDN is the search base for groups.
	// +kubebuilder:validation:MinLength=1
	TreeDN string `json:"treeDN"`

	// Filter is an additional LDAP filter applied to group searches.
	// +optional
	Filter string `json:"filter,omitempty"`

	// ObjectClass is the LDAP objectClass for groups (keystone default:
	// groupOfNames).
	// +optional
	ObjectClass string `json:"objectClass,omitempty"`

	// IDAttribute maps the keystone group ID (keystone default: cn).
	// +optional
	IDAttribute string `json:"idAttribute,omitempty"`

	// NameAttribute maps the keystone group name (keystone default: ou).
	// +optional
	NameAttribute string `json:"nameAttribute,omitempty"`

	// MemberAttribute maps group membership (keystone default: member).
	// +optional
	MemberAttribute string `json:"memberAttribute,omitempty"`
}

// LDAPTLSSpec configures certificate verification for the LDAP connection.
type LDAPTLSSpec struct {
	// CABundleSecretRef references the Secret holding the CA bundle under
	// the fixed data key "ca.crt" (the canonical cert-manager file name,
	// mirroring the database TLS contract). The projection writes the PEM to
	// /etc/keystone/domains/<domain>-ca.pem and points tls_cacertfile at it.
	CABundleSecretRef commonv1.SecretRefSpec `json:"caBundleSecretRef"`
}

// LDAPPoolSpec configures keystone's LDAP connection pool.
type LDAPPoolSpec struct {
	// Enabled turns the pool on (renders use_pool = true).
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Size is the pool size (renders pool_size); only rendered when set.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Size *int32 `json:"size,omitempty"`
}

// KeystoneIdentityBackendStatus defines the observed state of
// KeystoneIdentityBackend. The dedicated KeystoneIdentityBackend controller
// is the single writer of this status; the keystone-side sub-reconciler only
// reads it (DomainReady gates config projection) and writes the aggregated
// IdentityBackendsReady condition onto the Keystone CR instead.
type KeystoneIdentityBackendStatus struct {
	// Conditions represent the latest available observations of the backend
	// state: DomainReady (the referenced domain exists / was provisioned),
	// ConfigProjected (the rendered per-domain config is wired into the
	// running Keystone Deployment), and the aggregate Ready.
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

	// DomainID is the Keystone domain ID this backend provisioned (Manage)
	// or resolved (Adopt). Empty until DomainReady first turns True. The
	// deletion path uses it to disable+delete exactly the domain this CR
	// created, never a same-named foreign one.
	// +optional
	DomainID string `json:"domainID,omitempty"`
}

func init() {
	SchemeBuilder.Register(&KeystoneIdentityBackend{}, &KeystoneIdentityBackendList{})
}
