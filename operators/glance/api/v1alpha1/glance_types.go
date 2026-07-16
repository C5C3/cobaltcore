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
// +kubebuilder:printcolumn:name="Release",type="string",JSONPath=".status.installedRelease"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Glance is the Schema for the glances API. It deploys the Glance image service:
// the API server (eventlet glance-api below release 2026.1, uWSGI from 2026.1),
// its database and cache connections, and the Keystone integration. Image stores
// (S3 today) attach out-of-band through GlanceBackend CRs rather than living in
// this spec.
type Glance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GlanceSpec   `json:"spec,omitempty"`
	Status GlanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GlanceList contains a list of Glance.
type GlanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Glance `json:"items"`
}

// GlanceSpec defines the desired state of Glance.
type GlanceSpec struct {
	// OpenStackRelease names the OpenStack release this operator deploys and
	// drives. It governs two things: (a) the Glance API launch mode — the
	// eventlet glance-api server below 2026.1, uWSGI from 2026.1 onward — and
	// (b) install/upgrade release tracking (status.installedRelease is promoted
	// to this value after a successful db-sync). It is deliberately kept separate
	// from the image tag so digest-pinned images keep working: pinning
	// spec.image.digest disables tag-based release tracking, but this field still
	// tells the operator which schema and launch mode to converge to.
	//
	// The pattern matches the OpenStack date-based release scheme (YYYY.N where N
	// is 1 or 2 — the two-releases-per-year cadence, e.g. 2025.2, 2026.1). The
	// [12] minor class keeps this CRD pattern, the validating webhook, and
	// release.ParseRelease in agreement so a non-cadence minor (e.g. 2025.9) is
	// rejected at every layer.
	// +kubebuilder:validation:Pattern=`^\d{4}\.[12]$`
	OpenStackRelease string `json:"openStackRelease"`

	// Deployment groups the pod-level knobs for the Glance API Deployment
	// (replicas, resources, rollout strategy, graceful-termination timings, and
	// scheduling constraints).
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`

	// Image defines the Glance container image reference. Like the sibling
	// operators, the field carries no immutability rule — image upgrades are
	// routine.
	Image commonv1.ImageSpec `json:"image"`

	// Database defines the MariaDB connection parameters.
	// Supports managed (clusterRef) and brownfield (host/port) modes. The
	// clusterRef/host mutual-exclusivity rule and the credentialsMode
	// (Static/Dynamic) contract are inherited from commonv1.DatabaseSpec, so they
	// hold here without per-field duplication.
	Database commonv1.DatabaseSpec `json:"database"`

	// Cache defines the Memcached cache configuration.
	// Supports managed (clusterRef) and brownfield (servers) modes; the
	// clusterRef/servers mutual-exclusivity rule lives on commonv1.CacheSpec.
	Cache commonv1.CacheSpec `json:"cache"`

	// KeystoneEndpoint is the Keystone endpoint URL Glance authenticates against.
	// It renders as [keystone_authtoken] auth_url in glance-api.conf. Glance
	// connects to this URL server-side (token validation on every API request),
	// so it must be reachable from the Glance pods — for a colocated control
	// plane that is the cluster-local Service URL, never an externally routable
	// address that only resolves outside the cluster. A plain URL field keeps the
	// operator decoupled from the keystone-operator; the c5c3 ControlPlane
	// operator projects it from its Keystone child by naming convention.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystoneEndpoint string `json:"keystoneEndpoint"`

	// KeystonePublicEndpoint is the browser/client-facing Keystone base URL Glance
	// advertises as [keystone_authtoken] www_authenticate_uri — the address a 401
	// response points unauthenticated clients at. Optional: when empty the
	// operator falls back to KeystoneEndpoint (see EffectiveKeystonePublicEndpoint),
	// which is correct only when the internal and public Keystone URLs coincide.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystonePublicEndpoint string `json:"keystonePublicEndpoint,omitempty"`

	// ServiceUser identifies the Keystone service account Glance authenticates as
	// and the Secret holding its password. The username/domain fields are
	// webhook-defaulted (glance / service / Default / Default) in a later commit;
	// the password Secret reference is required.
	ServiceUser ServiceUserSpec `json:"serviceUser"`

	// Region is the Keystone region Glance authenticates against
	// ([keystone_authtoken] region_name). Optional: when empty the option is
	// omitted and Glance uses the Keystone catalog's default region.
	// +optional
	Region string `json:"region,omitempty"`

	// APIServer tunes the Glance API server process. Its two knobs are
	// release-conditional: uwsgi applies only from 2026.1 (uWSGI launch mode) and
	// workers only below 2026.1 (eventlet launch mode). When nil the operator uses
	// hardcoded defaults for the active launch mode.
	// +optional
	APIServer *APIServerSpec `json:"apiServer,omitempty"`

	// Gateway configures external exposure of the Glance API via a Gateway API
	// HTTPRoute. When set, the operator creates an HTTPRoute targeting the {name}
	// Service and attaches it to the referenced pre-existing Gateway. When removed
	// (nil), the HTTPRoute is deleted. The Gateway and GatewayClass are
	// infrastructure concerns managed outside this operator.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`

	// NetworkPolicy configures network isolation for Glance API pods.
	// When set, a NetworkPolicy is created restricting ingress and egress traffic.
	// When removed (nil), the NetworkPolicy is deleted and traffic flows
	// unrestricted.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`

	// Autoscaling configures horizontal pod autoscaling for the Glance API
	// deployment. When set, a HorizontalPodAutoscaler is created targeting the
	// deployment. When removed, the HPA is deleted.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Logging configures oslo.log output for the Glance API container.
	// When unset, the defaulting webhook materializes a LoggingSpec with
	// Format=text, Level=INFO, Debug=false.
	// +optional
	Logging *LoggingSpec `json:"logging,omitempty"`

	// SecretStoreRef selects the External Secrets store the operator routes this
	// Glance's ExternalSecrets and PushSecrets through. When omitted the operator
	// uses the shared cluster-scoped openbao-cluster-store, so existing
	// deployments keep working unchanged. Set kind to SecretStore with the name of
	// a namespaced store in THIS Glance's namespace to reach OpenBao as a
	// per-tenant identity. The ControlPlane operator projects this field onto the
	// Glance it owns, so operators normally configure it there rather than here.
	// +optional
	SecretStoreRef *commonv1.SecretStoreRefSpec `json:"secretStoreRef,omitempty"`

	// PolicyOverrides defines custom oslo.policy rules for the service.
	// When set, the operator renders a policy.yaml and configures
	// oslo_policy.policy_file automatically.
	// +optional
	// +kubebuilder:validation:XValidation:rule="(has(self.rules) && size(self.rules) > 0) || self.configMapRef != null",message="at least one of rules or configMapRef must be set"
	// The empty rule-name and rule-value constraints are enforced by the
	// XValidation markers on commonv1.PolicySpec itself, so they apply to every
	// PolicySpec field across operators without per-field duplication.
	PolicyOverrides *commonv1.PolicySpec `json:"policyOverrides,omitempty"`

	// Middleware defines WSGI middleware filters for the api-paste.ini pipeline.
	// +optional
	Middleware []commonv1.MiddlewareSpec `json:"middleware,omitempty"`

	// Plugins defines service plugins/drivers to configure. Modeled as a
	// list-map keyed by configSection so the API server rejects duplicate sections
	// structurally and server-side apply merges entries by section instead of
	// replacing the whole list.
	// +optional
	// +listType=map
	// +listMapKey=configSection
	Plugins []commonv1.PluginSpec `json:"plugins,omitempty"`

	// ExtraConfig provides free-form INI sections for configuration not covered by
	// explicit CRD fields. It is the deliberate escape hatch for import/staging
	// tuning, which has no dedicated knobs of its own yet.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`
}

// DeploymentSpec, AutoscalingSpec, NetworkPolicySpec,
// NetworkPolicyIngressSource, LoggingSpec, GatewaySpec, and
// GatewayParentRefSpec are aliased to the shared commonv1 definitions —
// commonv1 carries the canonical per-field godoc and validation markers.
// The aliases keep call sites (glancev1alpha1.DeploymentSpec and bare
// DeploymentSpec{} literals alike) consistent with the sibling operators.
type (
	DeploymentSpec             = commonv1.DeploymentSpec
	AutoscalingSpec            = commonv1.AutoscalingSpec
	NetworkPolicySpec          = commonv1.NetworkPolicySpec
	NetworkPolicyIngressSource = commonv1.NetworkPolicyIngressSource
	LoggingSpec                = commonv1.LoggingSpec
	GatewaySpec                = commonv1.GatewaySpec
	GatewayParentRefSpec       = commonv1.GatewayParentRefSpec
)

// ServiceUserSpec identifies the Keystone service account Glance uses to validate
// tokens and call other services, and references the Secret holding its password.
// The name and domain fields are optional; the defaulting webhook materializes
// them (username glance, projectName service, userDomainName and
// projectDomainName Default) in a later commit, so a minimal CR need only supply
// the password Secret reference.
type ServiceUserSpec struct {
	// Username is the Keystone username Glance authenticates as
	// ([keystone_authtoken] username). Webhook-defaulted to "glance".
	// +optional
	Username string `json:"username,omitempty"`

	// ProjectName is the Keystone project the service user scopes to
	// ([keystone_authtoken] project_name). Webhook-defaulted to "service".
	// +optional
	ProjectName string `json:"projectName,omitempty"`

	// UserDomainName is the domain the service user lives in
	// ([keystone_authtoken] user_domain_name). Webhook-defaulted to "Default".
	// +optional
	UserDomainName string `json:"userDomainName,omitempty"`

	// ProjectDomainName is the domain the service project lives in
	// ([keystone_authtoken] project_domain_name). Webhook-defaulted to "Default".
	// +optional
	ProjectDomainName string `json:"projectDomainName,omitempty"`

	// SecretRef references the Secret holding the service user's password. The
	// key is webhook-defaulted to "password" in a later commit.
	SecretRef commonv1.SecretRefSpec `json:"secretRef"`
}

// APIServerSpec tunes the Glance API server process. Which field takes effect
// depends on spec.openStackRelease: uWSGI is the launch mode from 2026.1 (uwsgi
// applies), the eventlet glance-api server below 2026.1 (workers applies). The
// validating webhook warns on inert combinations (e.g. workers set on a uWSGI
// release); it lands in a later commit on this branch.
type APIServerSpec struct {
	// UWSGI configures the uWSGI application server parameters. Effective only
	// from release 2026.1, where Glance launches under uWSGI; ignored below
	// 2026.1 (eventlet launch mode).
	// +optional
	UWSGI *UWSGISpec `json:"uwsgi,omitempty"`

	// Workers is the number of eventlet API worker processes, rendered as
	// [DEFAULT] workers in glance-api.conf. Effective only below release 2026.1,
	// where Glance launches the eventlet glance-api server; ignored from 2026.1
	// (uWSGI launch mode, where uwsgi applies instead).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Workers *int32 `json:"workers,omitempty"`
}

// UWSGISpec defines the uWSGI application server parameters.
// Exposed as an optional pointer field on APIServerSpec (spec.apiServer.uwsgi) so
// that Glance CRs without it continue to work with hardcoded defaults in the
// reconciler. The cross-field CEL rule mirrors the validating webhook:
// httpKeepAliveTimeout is only meaningful when httpKeepAlive is true, otherwise
// the --http-keepalive-timeout flag is never emitted.
// +kubebuilder:validation:XValidation:rule="!has(self.httpKeepAliveTimeout) || !has(self.httpKeepAlive) || self.httpKeepAlive",message="httpKeepAliveTimeout may only be set when httpKeepAlive is true"
type UWSGISpec struct {
	// Processes is the number of uWSGI worker processes.
	// The default literal mirrors DefaultUWSGIProcesses in glance_webhook.go.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	Processes int32 `json:"processes,omitempty"`

	// Threads is the number of threads per uWSGI worker process.
	// The default literal mirrors DefaultUWSGIThreads in glance_webhook.go.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Threads int32 `json:"threads,omitempty"`

	// HTTPKeepAlive enables the --http-keepalive flag on the uWSGI process.
	// When false, the flag is omitted from the command. It is a nil-preserving
	// pointer so "unset" is representable: the defaulting webhook restores the
	// documented default (true, DefaultUWSGIHTTPKeepAlive in glance_webhook.go)
	// when the pointer is nil, and the reconciler falls back to the same default
	// for CRs that bypass the webhook. An explicit false is honored verbatim.
	// +optional
	HTTPKeepAlive *bool `json:"httpKeepAlive,omitempty"`

	// Harakiri caps the per-request worker lifetime (seconds) via the uWSGI
	// --harakiri flag. A request blocked longer than this bound is
	// killed so a single stuck DB lookup cannot prevent other in-flight
	// requests from completing cleanly before graceful shutdown ends. When
	// nil, the --harakiri flag is omitted from the uWSGI command entirely
	// (no hidden default is injected). The webhook additionally requires
	// harakiri < terminationGracePeriodSeconds - preStopSleepSeconds so the
	// shutdown envelope is consistent.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Harakiri *int32 `json:"harakiri,omitempty"`

	// HTTPKeepAliveTimeout bounds the idle timeout (seconds) of keep-alive
	// connections via the uWSGI --http-keepalive-timeout flag.
	// A bounded timeout forces clients to reconnect through the Service so
	// they never reuse a socket to a removed pod. When nil, the flag is
	// omitted from the uWSGI command. Zero is rejected to avoid the
	// unbounded-timeout interpretation. A value at or below
	// preStopSleepSeconds is recommended so idle sockets have closed before
	// SIGTERM reaches uWSGI.
	// +optional
	// +kubebuilder:validation:Minimum=1
	HTTPKeepAliveTimeout *int32 `json:"httpKeepAliveTimeout,omitempty"`
}

// GlanceStatus defines the observed state of Glance.
type GlanceStatus struct {
	// Conditions represent the latest available observations of the Glance state.
	// Each condition carries an ObservedGeneration so consumers can tell a stale
	// condition from one reflecting the current spec; use the conditions helper
	// (internal/common/conditions) to upsert them.
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

	// Endpoint is the Glance API endpoint URL clients use.
	Endpoint string `json:"endpoint,omitempty"`

	// InstalledRelease is the OpenStack release whose database schema is currently
	// installed, promoted to spec.openStackRelease after a successful db-sync.
	InstalledRelease string `json:"installedRelease,omitempty"`

	// TargetRelease is the spec.openStackRelease being converged to during an
	// active upgrade.
	TargetRelease string `json:"targetRelease,omitempty"`
}

// EffectiveKeystonePublicEndpoint resolves the [keystone_authtoken]
// www_authenticate_uri value: the explicit KeystonePublicEndpoint when set,
// otherwise KeystoneEndpoint. It is resolved at render time rather than
// webhook-defaulted so a later edit to keystoneEndpoint keeps being tracked by
// the fallback instead of freezing a once-defaulted value.
func (s *GlanceSpec) EffectiveKeystonePublicEndpoint() string {
	if s.KeystonePublicEndpoint != "" {
		return s.KeystonePublicEndpoint
	}
	return s.KeystoneEndpoint
}

func init() {
	SchemeBuilder.Register(&Glance{}, &GlanceList{})
}
