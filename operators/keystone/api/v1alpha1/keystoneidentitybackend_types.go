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
// shipped LDAP; Phase 2 adds OIDC federation. SAML follows in a later phase.
// +kubebuilder:validation:Enum=LDAP;OIDC
type IdentityBackendType string

const (
	// IdentityBackendTypeLDAP selects the keystone LDAP identity driver.
	IdentityBackendTypeLDAP IdentityBackendType = "LDAP"
	// IdentityBackendTypeOIDC selects an OIDC federation backend: the
	// controller provisions the Keystone federation API objects (identity
	// provider, mapping, protocol) and the keystone-side sub-reconciler
	// projects the mod_auth_openidc proxy configuration.
	IdentityBackendTypeOIDC IdentityBackendType = "OIDC"
)

// KeystoneIdentityBackendSpec defines the desired state of
// KeystoneIdentityBackend.
//
// The keystoneRef transition rule (evaluated only on UPDATE) makes the
// attachment immutable: re-pointing a backend at a different Keystone would
// leave the old deployment with a provisioned domain nothing manages anymore
// and race the config projection on the new one. Delete and recreate instead.
// The type/ldap and type/oidc union rules enforce "exactly one backend block
// per type" at the schema layer so it holds even when the validating webhook
// is down. mappings/groups are federation vocabulary (OIDC-only for now) and
// extraOptions is documented [ldap] vocabulary, so both are type-gated at the
// schema layer too.
// +kubebuilder:validation:XValidation:rule="self.keystoneRef.name == oldSelf.keystoneRef.name",message="keystoneRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="type is immutable"
// +kubebuilder:validation:XValidation:rule="(self.type == 'LDAP') == has(self.ldap)",message="exactly one backend block matching spec.type must be set (type LDAP requires spec.ldap)"
// +kubebuilder:validation:XValidation:rule="(self.type == 'OIDC') == has(self.oidc)",message="exactly one backend block matching spec.type must be set (type OIDC requires spec.oidc)"
// +kubebuilder:validation:XValidation:rule="!has(self.mappings) || self.type == 'OIDC'",message="mappings are only supported on federation backends (type OIDC)"
// +kubebuilder:validation:XValidation:rule="!has(self.groups) || self.type == 'OIDC'",message="groups are only supported on federation backends (type OIDC)"
// +kubebuilder:validation:XValidation:rule="!has(self.extraOptions) || self.type == 'LDAP'",message="extraOptions carries [ldap] section options and is only supported on type LDAP"
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

	// OIDC configures an OpenID Connect federation backend. Required exactly
	// when type is OIDC (union rule above).
	// +optional
	OIDC *OIDCBackendSpec `json:"oidc,omitempty"`

	// Mappings are the keystone federation mapping rules applied to the
	// backend's protocol. Each rule maps remote assertion attributes (the
	// HTTP_OIDC_* claim headers mod_auth_openidc passes into the WSGI environ)
	// to local users, groups, and projects. The typed shape mirrors keystone's
	// mapping-rule JSON one-to-one (see the reference documentation for the
	// field correspondence); no free-form escape hatch is provided because the
	// rule grammar is closed.
	//
	// MaxItems bounds the aggregate rendered mapping so a single backend
	// cannot bloat the federation objects past reasonable size.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Mappings []MappingRuleSpec `json:"mappings,omitempty"`

	// Groups declares the local keystone groups (in this backend's domain)
	// that the mapping rules target, together with the role assignments that
	// give federated members access. The controller creates missing groups and
	// applies the role assignments idempotently.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Groups []FederationGroupSpec `json:"groups,omitempty"`

	// ExtraOptions provides free-form [ldap] section options not covered by
	// the typed fields, keyed by bare option name (e.g. "page_size"). Options
	// rendered from typed fields, the driver/domain-config wiring, and — when
	// readOnly is true — the write-enabling user_allow_*/group_allow_* options
	// are rejected by the validating webhook so the escape hatch cannot
	// silently contradict the typed spec.
	//
	// MaxProperties and the per-entry key/value length bound the aggregate
	// rendered config at admission (defense-in-depth alongside the renderer's
	// per-domain size budget) so this free-form map cannot be used to bloat the
	// shared domains Secret past the apiserver limit.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 256 && size(self[k]) <= 1024)",message="each extraOptions key must be <=256 characters and each value <=1024 characters"
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
	//
	// The Pattern constrains the name to the grammar shared by Secret data
	// keys and keystone's domain-config file naming (keystone.<name>.conf —
	// keystone derives the domain from the filename), both of which the
	// projection embeds the name into verbatim.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
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
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Description string `json:"description,omitempty"`
}

// LDAPBackendSpec configures the keystone LDAP identity driver for one
// domain. Only user-set optional fields are rendered into the per-domain
// config, so upstream keystone defaults apply for everything left unset.
type LDAPBackendSpec struct {
	// URL is the LDAP server URL (ldap:// or ldaps://).
	// +kubebuilder:validation:Pattern=`^ldaps?://`
	// +kubebuilder:validation:MaxLength=512
	URL string `json:"url"`

	// BindCredentialsSecretRef references the Secret holding the bind
	// credentials under the fixed data keys "username" (the bind DN) and
	// "password". The key field must stay empty — the two data keys are
	// fixed by contract (webhook-enforced).
	BindCredentialsSecretRef commonv1.SecretRefSpec `json:"bindCredentialsSecretRef"`

	// Suffix is the LDAP suffix (base DN), e.g. "dc=example,dc=com".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
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
	// +kubebuilder:validation:MaxLength=512
	TreeDN string `json:"treeDN"`

	// Filter is an additional LDAP filter applied to user searches.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Filter string `json:"filter,omitempty"`

	// ObjectClass is the LDAP objectClass for users (keystone default:
	// inetOrgPerson).
	// +kubebuilder:validation:MaxLength=256
	// +optional
	ObjectClass string `json:"objectClass,omitempty"`

	// IDAttribute maps the keystone user ID (keystone default: cn).
	// +kubebuilder:validation:MaxLength=256
	// +optional
	IDAttribute string `json:"idAttribute,omitempty"`

	// NameAttribute maps the keystone user name (keystone default: sn).
	// +kubebuilder:validation:MaxLength=256
	// +optional
	NameAttribute string `json:"nameAttribute,omitempty"`

	// MailAttribute maps the keystone user email (keystone default: mail).
	// +kubebuilder:validation:MaxLength=256
	// +optional
	MailAttribute string `json:"mailAttribute,omitempty"`
}

// LDAPGroupSpec describes the LDAP group tree and attribute mapping.
type LDAPGroupSpec struct {
	// TreeDN is the search base for groups.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	TreeDN string `json:"treeDN"`

	// Filter is an additional LDAP filter applied to group searches.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Filter string `json:"filter,omitempty"`

	// ObjectClass is the LDAP objectClass for groups (keystone default:
	// groupOfNames).
	// +kubebuilder:validation:MaxLength=256
	// +optional
	ObjectClass string `json:"objectClass,omitempty"`

	// IDAttribute maps the keystone group ID (keystone default: cn).
	// +kubebuilder:validation:MaxLength=256
	// +optional
	IDAttribute string `json:"idAttribute,omitempty"`

	// NameAttribute maps the keystone group name (keystone default: ou).
	// +kubebuilder:validation:MaxLength=256
	// +optional
	NameAttribute string `json:"nameAttribute,omitempty"`

	// MemberAttribute maps group membership (keystone default: member).
	// +kubebuilder:validation:MaxLength=256
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

// OIDC defaulting constants. The defaulting webhook materializes them; the
// +kubebuilder:default markers on the fields keep the same literals as
// defense-in-depth for callers that bypass the webhook (e.g. envtest).
const (
	// DefaultOIDCProtocolID is the keystone federation protocol ID.
	DefaultOIDCProtocolID = "openid"
	// DefaultOIDCRemoteIDAttribute is the WSGI environ key carrying the
	// asserted issuer (mod_auth_openidc's OIDCClaimPrefix "OIDC-" spelling of
	// the iss claim), validated hands-on by the federation spike.
	DefaultOIDCRemoteIDAttribute = "HTTP_OIDC_ISS"
	// DefaultOIDCResponseType is the authorization-code flow response type.
	DefaultOIDCResponseType = "code"
)

// DefaultOIDCScopes are the scopes requested when spec.oidc.scopes is empty.
var DefaultOIDCScopes = []string{"openid", "email", "profile"}

// OIDCSessionType selects the mod_auth_openidc session storage.
// client-cookie keeps the whole session in the browser cookie (HA-safe: no
// shared server-side cache across replicas); client-cookie-persistent is the
// same with a persistent cookie surviving browser restarts.
// +kubebuilder:validation:Enum=client-cookie;client-cookie-persistent
type OIDCSessionType string

const (
	// OIDCSessionTypeClientCookie keeps the session in the browser cookie.
	OIDCSessionTypeClientCookie OIDCSessionType = "client-cookie"
	// OIDCSessionTypeClientCookiePersistent additionally persists the cookie.
	OIDCSessionTypeClientCookiePersistent OIDCSessionType = "client-cookie-persistent"
)

// OIDCStateInputHeaders selects which request headers mod_auth_openidc folds
// into the state cookie hash. "none" is the HA-safe default validated by the
// federation spike: replicas behind one Service must not bind state to
// per-connection headers.
// +kubebuilder:validation:Enum=none;user-agent;x-forwarded-for;both
type OIDCStateInputHeaders string

// OIDCStateInputHeadersNone folds no request headers into the state hash.
const OIDCStateInputHeadersNone OIDCStateInputHeaders = "none"

// OIDCBackendSpec configures an OpenID Connect federation backend for one
// domain: the identity provider metadata, the relying-party client, and the
// mod_auth_openidc behavior knobs whose defaults were validated hands-on
// against live IdPs (the Phase-0 federation spike).
//
// Discovery is either metadata-driven (providerMetadataURL, defaulted from
// issuer by the webhook) or fully explicit (endpoints); the two shapes are
// mutually exclusive (CEL rule below plus webhook defense-in-depth).
// +kubebuilder:validation:XValidation:rule="!(has(self.providerMetadataURL) && has(self.endpoints))",message="providerMetadataURL and endpoints are mutually exclusive"
type OIDCBackendSpec struct {
	// Issuer is the OIDC issuer URL exactly as the IdP asserts it (the `iss`
	// claim), e.g. "https://keycloak.example.com/realms/forge". It names the
	// identity provider in the metadata directory (the scheme-stripped,
	// URL-encoded issuer is the metadata file basename) and is registered as
	// the keystone identity provider's remote ID.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=512
	Issuer string `json:"issuer"`

	// ProviderMetadataURL points at the IdP's OIDC discovery document. When
	// neither it nor endpoints is set, the defaulting webhook derives
	// "<issuer>/.well-known/openid-configuration". The operator fetches the
	// document at reconcile time and pre-provisions it into the read-only
	// metadata directory (mod_auth_openidc cannot self-cache there).
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=512
	// +optional
	ProviderMetadataURL string `json:"providerMetadataURL,omitempty"`

	// Endpoints spells the provider endpoints out explicitly instead of
	// fetching the discovery document — for air-gapped operators or IdPs whose
	// metadata URL is unreachable from the operator pod.
	// +optional
	Endpoints *OIDCEndpointsSpec `json:"endpoints,omitempty"`

	// ClientID is the relying-party client ID registered at the IdP.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ClientID string `json:"clientID"`

	// ClientSecretRef references the Secret holding the relying-party client
	// secret under the fixed data key "clientSecret". The key field must stay
	// empty — the data key is fixed by contract (webhook-enforced), mirroring
	// the LDAP bind Secret contract.
	ClientSecretRef commonv1.SecretRefSpec `json:"clientSecretRef"`

	// ProtocolID is the keystone federation protocol ID the websso/auth URLs
	// embed (…/protocols/<protocolID>/websso). Defaults to "openid".
	// +kubebuilder:default=openid
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	ProtocolID string `json:"protocolID,omitempty"`

	// IdentityProviderName is the keystone identity provider ID this backend
	// provisions. Defaults to the CR name. Unique per referenced Keystone
	// (webhook-enforced) because it is a path segment of the federation API
	// objects and the protected websso/auth Locations.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	IdentityProviderName string `json:"identityProviderName,omitempty"`

	// RemoteIDAttribute is the WSGI environ key keystone reads the asserted
	// issuer from ([openid] remote_id_attribute). The spike validated
	// HTTP_OIDC_ISS (mod_auth_openidc's OIDCClaimPrefix "OIDC-" spelling of
	// the iss claim); it must be uniform across every OIDC backend attached to
	// one Keystone because it renders into the single [openid] section.
	// +kubebuilder:default=HTTP_OIDC_ISS
	// +kubebuilder:validation:MaxLength=128
	// +optional
	RemoteIDAttribute string `json:"remoteIDAttribute,omitempty"`

	// Scopes are the OIDC scopes requested from the IdP. Defaults to
	// ["openid", "email", "profile"].
	// +kubebuilder:default={"openid","email","profile"}
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Scopes []string `json:"scopes,omitempty"`

	// ResponseType is the OIDC response type. Defaults to "code" (the
	// authorization-code flow — the only flow the spike validated).
	// +kubebuilder:default=code
	// +kubebuilder:validation:MaxLength=64
	// +optional
	ResponseType string `json:"responseType,omitempty"`

	// OAuth2Introspection turns the proxy into an OAuth2 resource server for
	// this backend so CLI clients can present IdP-issued bearer tokens
	// directly. mod_auth_openidc's OIDCOAuth* directives are server-scoped, so
	// at most one OIDC backend per Keystone may enable this
	// (webhook-enforced).
	// +optional
	OAuth2Introspection *OIDCIntrospectionSpec `json:"oauth2Introspection,omitempty"`

	// SessionType selects the mod_auth_openidc session storage. Defaults to
	// client-cookie (HA-safe: no server-side session cache shared across
	// replicas).
	// +kubebuilder:default=client-cookie
	// +optional
	SessionType OIDCSessionType `json:"sessionType,omitempty"`

	// StateInputHeaders selects the request headers folded into the state
	// cookie hash. Defaults to none (HA-safe).
	// +kubebuilder:default=none
	// +optional
	StateInputHeaders OIDCStateInputHeaders `json:"stateInputHeaders,omitempty"`
}

// OIDCEndpointsSpec spells out the provider endpoints mod_auth_openidc needs
// when the discovery document is not fetched. All URL fields share the
// https?:// scheme guard with issuer/providerMetadataURL.
type OIDCEndpointsSpec struct {
	// AuthorizationEndpoint is the OAuth2 authorization endpoint.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=512
	AuthorizationEndpoint string `json:"authorizationEndpoint"`

	// TokenEndpoint is the OAuth2 token endpoint.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=512
	TokenEndpoint string `json:"tokenEndpoint"`

	// JWKSURI is the provider's JSON Web Key Set document.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=512
	JWKSURI string `json:"jwksURI"`

	// UserinfoEndpoint is the optional OIDC userinfo endpoint.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=512
	// +optional
	UserinfoEndpoint string `json:"userinfoEndpoint,omitempty"`

	// EndSessionEndpoint is the optional RP-initiated logout endpoint.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=512
	// +optional
	EndSessionEndpoint string `json:"endSessionEndpoint,omitempty"`

	// IntrospectionEndpoint is the optional OAuth2 token introspection
	// endpoint (required when oauth2Introspection is enabled with explicit
	// endpoints).
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=512
	// +optional
	IntrospectionEndpoint string `json:"introspectionEndpoint,omitempty"`
}

// OIDCIntrospectionSpec opts one backend into the OAuth2 resource-server role
// (bearer-token introspection for CLI clients).
type OIDCIntrospectionSpec struct {
	// Enabled turns bearer-token introspection on.
	Enabled bool `json:"enabled"`

	// TLSVerify controls certificate verification on the introspection
	// call. The endpoint itself must be https (mod_auth_openidc rejects http
	// introspection endpoints at config-parse time); setting this to false
	// renders OIDCOAuthSSLValidateServer Off so self-signed or private-CA
	// endpoints work — an explicit opt-out for test and lab environments
	// until a CA-bundle field ships. Defaults to true (verify).
	// +optional
	TLSVerify *bool `json:"tlsVerify,omitempty"`
}

// MappingRuleSpec is one keystone federation mapping rule: remote assertion
// matchers plus the local objects they map to. The typed shape mirrors
// keystone's mapping-rule JSON one-to-one (camelCase field names map to the
// snake_case JSON keys; see the reference documentation table); the grammar
// is closed, so no free-form escape hatch is needed.
type MappingRuleSpec struct {
	// Local lists the local Identity API objects the matched remote user maps
	// to (user, groups, projects).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Local []MappingLocalRuleSpec `json:"local"`

	// Remote lists the assertion matchers. Types are full WSGI environ names
	// (e.g. HTTP_OIDC_ISS) — keystone's assertion_prefix stays empty so the
	// claim headers arrive unmodified.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Remote []MappingRemoteRuleSpec `json:"remote"`
}

// MappingRemoteRuleSpec matches one remote assertion attribute (keystone
// mapping-rule "remote" object).
type MappingRemoteRuleSpec struct {
	// Type is the assertion attribute name — the full WSGI environ key, e.g.
	// HTTP_OIDC_ISS or HTTP_OIDC_PREFERRED_USERNAME.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Type string `json:"type"`

	// Regex evaluates the matcher strings as regular expressions when true.
	// +optional
	Regex *bool `json:"regex,omitempty"`

	// AnyOneOf matches when any listed string appears in the attribute.
	// Mutually exclusive with NotAnyOf.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	AnyOneOf []string `json:"anyOneOf,omitempty"`

	// NotAnyOf rejects the rule when any listed string appears in the
	// attribute. Mutually exclusive with AnyOneOf.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	NotAnyOf []string `json:"notAnyOf,omitempty"`

	// Blacklist filters the listed strings out of the attribute values.
	// Mutually exclusive with Whitelist.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Blacklist []string `json:"blacklist,omitempty"`

	// Whitelist passes only the listed strings through. Mutually exclusive
	// with Blacklist.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Whitelist []string `json:"whitelist,omitempty"`
}

// MappingLocalRuleSpec is one keystone mapping-rule "local" object.
type MappingLocalRuleSpec struct {
	// Domain scopes the local objects to a domain.
	// +optional
	Domain *MappingDomainSpec `json:"domain,omitempty"`

	// Group maps the remote user into one group.
	// +optional
	Group *MappingGroupSpec `json:"group,omitempty"`

	// GroupIDs maps the remote user into the groups named by an assertion
	// value (keystone "group_ids", usually "{0}").
	// +kubebuilder:validation:MaxLength=256
	// +optional
	GroupIDs string `json:"groupIds,omitempty"`

	// Groups maps the remote user into the semicolon-separated group list an
	// assertion value carries (keystone "groups", usually "{0}").
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Groups string `json:"groups,omitempty"`

	// Projects grants the mapped user roles on projects.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Projects []MappingProjectSpec `json:"projects,omitempty"`

	// User maps the remote identity to a local user representation.
	// +optional
	User *MappingUserSpec `json:"user,omitempty"`
}

// MappingDomainSpec references a domain by ID or name (mutually exclusive,
// keystone-enforced).
type MappingDomainSpec struct {
	// ID is the domain ID.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	ID string `json:"id,omitempty"`

	// Name is the domain name.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Name string `json:"name,omitempty"`
}

// MappingGroupSpec references a group by ID, or by name plus domain.
type MappingGroupSpec struct {
	// ID is the group ID.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	ID string `json:"id,omitempty"`

	// Name is the group name.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Name string `json:"name,omitempty"`

	// Domain the named group lives in.
	// +optional
	Domain *MappingDomainSpec `json:"domain,omitempty"`
}

// MappingProjectSpec grants roles on one project (keystone mapping-rule
// "projects" entry).
type MappingProjectSpec struct {
	// Name is the project name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`

	// Roles are the roles granted on the project.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Roles []MappingProjectRoleSpec `json:"roles,omitempty"`
}

// MappingProjectRoleSpec names one role.
type MappingProjectRoleSpec struct {
	// Name is the role name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`
}

// MappingUserSpec is the keystone mapping-rule "user" object.
type MappingUserSpec struct {
	// ID is the local user ID (type local only).
	// +kubebuilder:validation:MaxLength=64
	// +optional
	ID string `json:"id,omitempty"`

	// Name is the user name, usually an assertion placeholder like "{0}".
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Name string `json:"name,omitempty"`

	// Email is the user email, usually an assertion placeholder.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Email string `json:"email,omitempty"`

	// Domain the user belongs to.
	// +optional
	Domain *MappingDomainSpec `json:"domain,omitempty"`

	// Type is the keystone user type: ephemeral (shadow user, the federation
	// default) or local (pre-existing local user).
	// +kubebuilder:validation:Enum=ephemeral;local
	// +optional
	Type string `json:"type,omitempty"`
}

// FederationGroupSpec declares one local keystone group (in the backend's
// domain) the mapping rules target, plus the role assignments granting its
// members access.
type FederationGroupSpec struct {
	// Name is the group name inside the backend's domain.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`

	// Description is projected onto the group at creation.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Description string `json:"description,omitempty"`

	// RoleAssignments grant the group roles on projects or on the backend's
	// domain.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	RoleAssignments []FederationRoleAssignmentSpec `json:"roleAssignments,omitempty"`
}

// FederationRoleAssignmentSpec grants one role to the group, scoped to
// exactly one of a project or the backend's domain (CEL rule below plus
// webhook defense-in-depth).
// +kubebuilder:validation:XValidation:rule="has(self.project) != (has(self.domain) && self.domain)",message="exactly one of project or domain must be set"
type FederationRoleAssignmentSpec struct {
	// Role is the role name to grant (e.g. "member").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Role string `json:"role"`

	// Project scopes the assignment to a project.
	// +optional
	Project *FederationProjectScopeSpec `json:"project,omitempty"`

	// Domain scopes the assignment to the backend's domain.
	// +optional
	Domain bool `json:"domain,omitempty"`
}

// FederationProjectScopeSpec names the project a role assignment targets.
type FederationProjectScopeSpec struct {
	// Name is the project name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`

	// DomainName is the domain the project lives in; defaults to the
	// backend's own domain when empty.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	DomainName string `json:"domainName,omitempty"`
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
	// running Keystone Deployment), for OIDC backends FederationObjectsReady
	// (identity provider + protocol upserted) and MappingsReady (mapping
	// rules, groups, and role assignments applied), and the aggregate Ready.
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

// EffectiveIdentityProviderName returns spec.oidc.identityProviderName,
// falling back to the CR name (the documented default). The defaulting
// webhook materializes the value at admission; this helper keeps the
// controller and the sibling-uniqueness check correct for CRs that bypassed
// it (envtest, direct unit invocation).
func (b *KeystoneIdentityBackend) EffectiveIdentityProviderName() string {
	if b.Spec.OIDC != nil && b.Spec.OIDC.IdentityProviderName != "" {
		return b.Spec.OIDC.IdentityProviderName
	}
	return b.Name
}

// EffectiveOIDCProtocolID returns spec.oidc.protocolID, falling back to the
// documented default ("openid").
func (b *KeystoneIdentityBackend) EffectiveOIDCProtocolID() string {
	if b.Spec.OIDC != nil && b.Spec.OIDC.ProtocolID != "" {
		return b.Spec.OIDC.ProtocolID
	}
	return DefaultOIDCProtocolID
}

// EffectiveRemoteIDAttribute returns spec.oidc.remoteIDAttribute, falling
// back to the documented default (HTTP_OIDC_ISS).
func (b *KeystoneIdentityBackend) EffectiveRemoteIDAttribute() string {
	if b.Spec.OIDC != nil && b.Spec.OIDC.RemoteIDAttribute != "" {
		return b.Spec.OIDC.RemoteIDAttribute
	}
	return DefaultOIDCRemoteIDAttribute
}

func init() {
	SchemeBuilder.Register(&KeystoneIdentityBackend{}, &KeystoneIdentityBackendList{})
}
