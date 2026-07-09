// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c5c3/forge/internal/common/naming"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// Selector label keys and values used by the Deployment pod selector, webhook
// TSC validation, and commonLabels(). The keys are re-exports of the shared
// naming-convention constants (internal/common/naming) so that the webhook,
// controller, and every other operator reference the same source of truth —
// prevents silent drift.
const (
	LabelKeyName     = naming.LabelKeyName
	LabelKeyInstance = naming.LabelKeyInstance
	AppName          = "horizon"
)

// DefaultCacheBackend is the Django CACHES backend materialized by the
// defaulting webhook when spec.cache.backend is empty. Horizon is a Django
// app, so the backend is a Django cache backend path — not the oslo.cache
// dogpile path keystone defaults to on the same shared CacheSpec field.
const DefaultCacheBackend = "django.core.cache.backends.memcached.PyMemcacheCache"

// DefaultSecretKeyKey is the Secret data key the defaulting webhook
// materializes on spec.secretKeyRef.key when it is empty. It matches the
// secretKey the kind-infrastructure ExternalSecret writes.
const DefaultSecretKeyKey = "secret-key"

// WebSSO defaults materialized by the defaulting webhook when spec.websso is
// enabled. Horizon's login form always offers a local-credentials option
// alongside the federated ones; without it, enabling SSO would lock out every
// non-federated account (including the bootstrap admin).
const (
	// DefaultWebSSOCredentialsChoiceID is the choice id of the local-credentials
	// fallback. It is Horizon's own reserved id: openstack_auth treats an
	// auth_type absent from WEBSSO_IDP_MAPPING as a local login, and
	// "credentials" is the id upstream documents for it.
	DefaultWebSSOCredentialsChoiceID = "credentials"
	// DefaultWebSSOCredentialsChoiceLabel is the label rendered beside the
	// local-credentials fallback in the login form's "Authenticate using"
	// dropdown.
	DefaultWebSSOCredentialsChoiceLabel = "Keystone Credentials"
)

// maxWebSSOChoices mirrors the +kubebuilder:validation:MaxItems marker on
// WebSSOSpec.Choices. The defaulting webhook consults it because mutating
// admission runs BEFORE schema validation: a prepend that grew the list past
// the marker would be rejected by the API server naming a count the operator
// never wrote.
const maxWebSSOChoices = 17

// DefaultMultiDomainDefaultDomain is the Keystone domain the defaulting
// webhook materializes on spec.multiDomain.defaultDomain when it is empty. It
// matches keystone's own [identity] default_domain_id.
const DefaultMultiDomainDefaultDomain = "Default"

// Django setting names owned by the spec.websso and spec.multiDomain blocks.
// They are exported so the settings renderer and the validating webhook (which
// rejects an extraConfig override of a setting the typed block already owns)
// share one source of truth rather than repeating the literals.
const (
	SettingWebSSOEnabled        = "WEBSSO_ENABLED"
	SettingWebSSOChoices        = "WEBSSO_CHOICES"
	SettingWebSSOIDPMapping     = "WEBSSO_IDP_MAPPING"
	SettingWebSSOInitialChoice  = "WEBSSO_INITIAL_CHOICE"
	SettingWebSSOKeystoneURL    = "WEBSSO_KEYSTONE_URL"
	SettingWebSSOUseHTTPReferer = "WEBSSO_USE_HTTP_REFERER"

	SettingMultiDomainSupport        = "OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT"
	SettingMultiDomainDefaultDomain  = "OPENSTACK_KEYSTONE_DEFAULT_DOMAIN"
	SettingMultiDomainDomainDropdown = "OPENSTACK_KEYSTONE_DOMAIN_DROPDOWN"
	SettingMultiDomainDomainChoices  = "OPENSTACK_KEYSTONE_DOMAIN_CHOICES"
)

// WebSSOSettingNames and MultiDomainSettingNames enumerate the settings each
// typed block renders. The validating webhook walks them to reject an
// extraConfig entry that would silently win the merge and contradict the block.
var (
	WebSSOSettingNames = []string{
		SettingWebSSOEnabled,
		SettingWebSSOChoices,
		SettingWebSSOIDPMapping,
		SettingWebSSOInitialChoice,
		SettingWebSSOKeystoneURL,
		SettingWebSSOUseHTTPReferer,
	}
	MultiDomainSettingNames = []string{
		SettingMultiDomainSupport,
		SettingMultiDomainDefaultDomain,
		SettingMultiDomainDomainDropdown,
		SettingMultiDomainDomainChoices,
	}
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Horizon is the Schema for the horizons API.
type Horizon struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HorizonSpec   `json:"spec,omitempty"`
	Status HorizonStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HorizonList contains a list of Horizon.
type HorizonList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Horizon `json:"items"`
}

// HorizonSpec defines the desired state of Horizon.
//
// Horizon is deliberately the thin profile among the service operators:
// a stateless Django/WSGI dashboard with no message bus, no service-catalog
// endpoints of its own, and no db-sync/fernet/bootstrap/upgrade machinery.
// Sessions use Django signed cookies; Memcached only backs the Django cache,
// so losing it degrades cache hit-rate without logging users out.
type HorizonSpec struct {
	// Deployment groups the pod-level knobs for the Horizon dashboard
	// Deployment (replicas, resources, rollout strategy, graceful-termination
	// timings, and scheduling constraints).
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`

	// Image defines the Horizon container image reference. Like keystone's,
	// the field carries no immutability rule — image upgrades are routine.
	Image commonv1.ImageSpec `json:"image"`

	// Cache defines the Memcached configuration backing the Django cache.
	// Supports managed (clusterRef) and brownfield (servers) modes; the
	// clusterRef/servers mutual-exclusivity rule lives on commonv1.CacheSpec.
	// Backend is a Django cache backend path (defaulted to PyMemcacheCache),
	// not an oslo.cache dogpile path.
	Cache commonv1.CacheSpec `json:"cache"`

	// KeystoneEndpoint is the Keystone endpoint URL the dashboard
	// authenticates against (OPENSTACK_KEYSTONE_URL). The dashboard's Django
	// backend connects to this URL server-side, so it must be reachable from
	// the dashboard pods — for a colocated control plane that is the
	// cluster-local Service URL, never an externally routable address that
	// only resolves outside the cluster. A plain URL field keeps
	// the operator decoupled from the keystone-operator; the c5c3 ControlPlane
	// operator projects it from its Keystone child by naming convention.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystoneEndpoint string `json:"keystoneEndpoint"`

	// SecretKeyRef references the Secret holding the Django SECRET_KEY.
	// The key material is injected into the dashboard pods as an environment
	// variable and never rendered into the settings ConfigMap. A shared
	// SECRET_KEY across replicas is required for signed-cookie session
	// consistency. The referenced Secret is expected to be ESO-managed (an
	// ExternalSecret synced from OpenBao), mirroring the keystone credential
	// pattern; key defaults to "secret-key".
	SecretKeyRef commonv1.SecretRefSpec `json:"secretKeyRef"`

	// Gateway configures external exposure of the dashboard via a Gateway API
	// HTTPRoute. When set, the operator creates an HTTPRoute targeting the
	// {name} Service on port 8080 and attaches it to the referenced
	// pre-existing Gateway. When removed (nil), the HTTPRoute is deleted.
	// The Gateway and GatewayClass are infrastructure concerns managed outside
	// this operator.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`

	// NetworkPolicy configures network isolation for Horizon dashboard pods.
	// When set, a NetworkPolicy is created restricting ingress and egress
	// traffic. When removed (nil), the NetworkPolicy is deleted and traffic
	// flows unrestricted.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`

	// Autoscaling configures horizontal pod autoscaling for the dashboard
	// deployment. When set, a HorizontalPodAutoscaler is created targeting the
	// deployment. When removed, the HPA is deleted.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Logging configures Django log output for the dashboard container.
	// When unset, the defaulting webhook materializes a LoggingSpec with
	// Format=text, Level=INFO, Debug=false.
	// +optional
	Logging *LoggingSpec `json:"logging,omitempty"`

	// WebSSO configures the federated single-sign-on choices offered on the
	// login page. When nil (the default) the operator renders no WEBSSO_*
	// settings at all and the dashboard offers local credentials only. The
	// c5c3 ControlPlane operator projects this block from the federation
	// backends attached to its Keystone child; standalone installations set it
	// directly.
	// +optional
	WebSSO *WebSSOSpec `json:"websso,omitempty"`

	// MultiDomain configures multi-domain login: the domain field (or dropdown)
	// on the login form, needed when users live in domains other than the
	// default one — typically LDAP-backed domains. When nil (the default) the
	// operator renders no OPENSTACK_KEYSTONE_MULTIDOMAIN_* settings and the
	// login form authenticates against the default domain only.
	// +optional
	MultiDomain *MultiDomainSpec `json:"multiDomain,omitempty"`

	// ExtraConfig provides free-form Django settings rendered verbatim into
	// local_settings.py after the operator defaults, so user values win.
	// Keys are Django setting names (e.g. SESSION_TIMEOUT); values are
	// arbitrary JSON converted structurally to Python literals.
	// A CEL rule over the keys is not expressible here — the API server
	// cannot build CEL type information for preserve-unknown-fields map
	// values — so the empty-key and SECRET_KEY guards are enforced by the
	// validating webhook only.
	// +optional
	ExtraConfig map[string]apiextensionsv1.JSON `json:"extraConfig,omitempty"`
}

// WebSSOSpec configures Horizon's federated single-sign-on surface. Each
// field maps onto one upstream WEBSSO_* Django setting, rendered by the
// settings renderer so operators never hand-write Python literals into
// spec.extraConfig.
//
// The CEL rules below are mirrored by the validating webhook as
// defense-in-depth: an initialChoice or an idpMapping key that names no
// declared choice would render a login form whose selected option resolves to
// nothing, failing only when a user clicks it.
//
// +kubebuilder:validation:XValidation:rule="!self.enabled || (has(self.choices) && size(self.choices) > 0)",message="websso.choices must contain at least one choice when websso.enabled is true"
// +kubebuilder:validation:XValidation:rule="!has(self.initialChoice) || size(self.initialChoice) == 0 || (has(self.choices) && self.choices.exists(c, c.id == self.initialChoice))",message="websso.initialChoice must match the id of one of websso.choices"
// +kubebuilder:validation:XValidation:rule="!has(self.idpMapping) || self.idpMapping.all(k, has(self.choices) && self.choices.exists(c, c.id == k))",message="every websso.idpMapping key must match the id of one of websso.choices"
type WebSSOSpec struct {
	// Enabled turns the SSO selector on the login page on (WEBSSO_ENABLED).
	// When false the remaining fields are inert, so a block can be prepared
	// and switched on later.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Choices are the ordered entries of the login page's "Authenticate using"
	// dropdown (WEBSSO_CHOICES). Order is preserved verbatim: the first entry
	// is what the form shows before the user picks. The defaulting webhook
	// prepends the local-credentials fallback when it is absent, so enabling
	// SSO can never lock out non-federated accounts.
	//
	// The bound of 17 counts the list AFTER that prepend: the 16 federated
	// choices the IDPMapping bound allows, plus the fallback. A list that
	// already carries a choice with id "credentials" may therefore hold 17
	// entries; one that does not is effectively bounded at 16, and the
	// defaulting webhook rejects a 17th rather than growing it into an API
	// server rejection naming a count the operator never wrote.
	// +optional
	// +kubebuilder:validation:MaxItems=17
	Choices []WebSSOChoice `json:"choices,omitempty"`

	// IDPMapping maps a choice id onto the Keystone identity provider and
	// federation protocol the dashboard hands off to (WEBSSO_IDP_MAPPING).
	// A choice with no mapping entry is a local login — that is how the
	// credentials fallback stays a plain username/password form. Every key
	// must name a declared choice (CEL + webhook enforced).
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	IDPMapping map[string]WebSSOIDPTarget `json:"idpMapping,omitempty"`

	// InitialChoice preselects one of Choices by id (WEBSSO_INITIAL_CHOICE).
	// When empty the defaulting webhook sets it to the credentials fallback,
	// so an SSO-enabled login page still opens on the local form.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	InitialChoice string `json:"initialChoice,omitempty"`

	// KeystoneURL is the BROWSER-facing Keystone base URL the SSO redirect is
	// built from (WEBSSO_KEYSTONE_URL). It exists because
	// spec.keystoneEndpoint is by contract the cluster-local Service URL,
	// consumed server-side by the Django backend — redirecting a browser there
	// would fail to resolve. Set it to the externally routable Keystone
	// endpoint (e.g. "https://keystone.example.com/v3"); when empty the
	// setting is omitted and Horizon falls back to spec.keystoneEndpoint,
	// which only works when the two happen to coincide.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystoneURL string `json:"keystoneURL,omitempty"`
}

// WebSSOChoice is one entry of the login page's authentication-method dropdown.
type WebSSOChoice struct {
	// ID identifies the choice. It is submitted as the form's auth_type value
	// and is the key IDPMapping and InitialChoice reference. The character set
	// is restricted because the value round-trips through a URL query string.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+$`
	ID string `json:"id"`

	// Label is the human-readable text rendered for this choice.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Label string `json:"label"`
}

// WebSSOIDPTarget names the Keystone federation endpoint a websso choice
// hands the browser off to.
type WebSSOIDPTarget struct {
	// IdentityProvider is the Keystone identity-provider id (the
	// KeystoneIdentityBackend's effective identityProviderName).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	IdentityProvider string `json:"identityProvider"`

	// Protocol is the Keystone federation protocol id (e.g. "openid").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Protocol string `json:"protocol"`
}

// MultiDomainSpec configures the login form's domain handling. Horizon only
// renders the domain dropdown when DomainDropdown is true AND DomainChoices is
// non-empty; with Enabled alone the form shows a free-text domain field.
//
// +kubebuilder:validation:XValidation:rule="!self.domainDropdown || self.enabled",message="multiDomain.domainDropdown requires multiDomain.enabled"
// +kubebuilder:validation:XValidation:rule="!self.domainDropdown || (has(self.domainChoices) && size(self.domainChoices) > 0)",message="multiDomain.domainChoices must contain at least one choice when multiDomain.domainDropdown is true"
type MultiDomainSpec struct {
	// Enabled turns on multi-domain login
	// (OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT), adding a domain input to the
	// login form.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// DefaultDomain is the domain assumed for users who do not supply one
	// (OPENSTACK_KEYSTONE_DEFAULT_DOMAIN). The defaulting webhook materializes
	// "Default" when empty.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	DefaultDomain string `json:"defaultDomain,omitempty"`

	// DomainDropdown replaces the free-text domain field with a select
	// populated from DomainChoices (OPENSTACK_KEYSTONE_DOMAIN_DROPDOWN), so
	// users need not know how to spell their domain.
	// +kubebuilder:default=false
	DomainDropdown bool `json:"domainDropdown,omitempty"`

	// DomainChoices are the ordered entries of the domain dropdown
	// (OPENSTACK_KEYSTONE_DOMAIN_CHOICES). Rendered only when DomainDropdown
	// is true.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	DomainChoices []DomainChoice `json:"domainChoices,omitempty"`
}

// DomainChoice is one entry of the login page's domain dropdown.
type DomainChoice struct {
	// Name is the Keystone domain name submitted with the login form.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`

	// Label is the human-readable text rendered for this domain.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Label string `json:"label"`
}

// DeploymentSpec, AutoscalingSpec, NetworkPolicySpec,
// NetworkPolicyIngressSource, LoggingSpec, GatewaySpec, and
// GatewayParentRefSpec are aliased to the shared commonv1 definitions —
// commonv1 carries the canonical per-field godoc and validation markers.
// The aliases keep call sites (horizonv1alpha1.DeploymentSpec and bare
// DeploymentSpec{} literals alike) consistent with the keystone operator.
type (
	DeploymentSpec             = commonv1.DeploymentSpec
	AutoscalingSpec            = commonv1.AutoscalingSpec
	NetworkPolicySpec          = commonv1.NetworkPolicySpec
	NetworkPolicyIngressSource = commonv1.NetworkPolicyIngressSource
	LoggingSpec                = commonv1.LoggingSpec
	GatewaySpec                = commonv1.GatewaySpec
	GatewayParentRefSpec       = commonv1.GatewayParentRefSpec
)

// HorizonStatus defines the observed state of Horizon.
type HorizonStatus struct {
	// Conditions represent the latest available observations of the Horizon
	// state. Each condition carries an ObservedGeneration so consumers can tell
	// a stale condition from one reflecting the current spec; use the conditions
	// helper (internal/common/conditions) to upsert them.
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

	// Endpoint is the dashboard URL.
	Endpoint string `json:"endpoint,omitempty"`
}

func init() {
	SchemeBuilder.Register(&Horizon{}, &HorizonList{})
}
