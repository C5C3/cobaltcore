// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
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
	AppName          = "keystone"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Release",type="string",JSONPath=".status.installedRelease"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Keystone is the Schema for the keystones API.
type Keystone struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeystoneSpec   `json:"spec,omitempty"`
	Status KeystoneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeystoneList contains a list of Keystone.
type KeystoneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Keystone `json:"items"`
}

// KeystoneSpec defines the desired state of Keystone.
type KeystoneSpec struct {
	// Deployment groups the pod-level knobs for the Keystone API Deployment
	// (replicas, resources, rollout strategy, graceful-termination timings, and
	// scheduling constraints). Future affinity/tolerations/nodeSelector knobs
	// land here too, keeping the spec root legible.
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`

	// Image defines the Keystone container image reference.
	//
	// Unlike the db/bootstrap fields below — whose CEL transition rules
	// (self == oldSelf) make them immutable even when the validating webhook is
	// unavailable, because the API server enforces them (#466) — spec.image
	// carries no immutability rule, since image upgrades are routine and the field
	// must stay mutable. The ControlPlane operator projects spec.openStackRelease
	// into this image tag and rejects release downgrades in its validating webhook
	// (validateReleaseNotDowngraded); that guard is webhook-only and has no
	// API-server CEL backstop, so a direct edit of this Keystone child's spec.image
	// can still point an already-migrated deployment at an older release if the
	// ControlPlane webhook is bypassed.
	Image commonv1.ImageSpec `json:"image"`

	// Database defines the MariaDB connection parameters.
	// Supports managed (clusterRef) and brownfield (host/port) modes.
	// TLS/mTLS is opt-in via database.tls when enabled, both the
	// CA bundle and client keypair Secret references must be supplied so the
	// reconciler can establish a verified, mutually-authenticated connection.
	//
	// DECISION no CEL rule is added for tls.mode — the out-of-enum
	// case is already rejected by the leaf +kubebuilder:validation:Enum marker
	// on DatabaseTLSSpec.Mode (disabled;prefer;require;verify-ca;verify-full).
	// Adding a mode CEL here would be redundant with that schema-level enum and
	// risks drifting from it. TLS is enabled exactly when mode is neither empty
	// nor "disabled" (DatabaseTLSSpec.IsEnabled).
	//
	// The clusterRef/host mutual-exclusivity rule lives on commonv1.DatabaseSpec
	// itself, so it is inherited here without per-field duplication. The TLS
	// rule below stays field-level because it is keystone-specific.
	//
	// The database name and the connection mode are immutable after create.
	// Renaming spec.database.database re-points db_sync at a fresh, empty schema:
	// Keystone silently "loses" every user/project while the old data is orphaned
	// (data-loss class). Flipping the managed (clusterRef) ↔ brownfield (host) mode
	// likewise re-targets the whole connection. The transition rules below
	// (self == oldSelf, evaluated only on UPDATE) are enforced by the API server
	// itself, so they keep protecting the field even when the validating webhook
	// is unavailable (#466).
	// +kubebuilder:validation:XValidation:rule="!has(self.tls) || self.tls.mode == '' || self.tls.mode == 'disabled' || (self.tls.caBundleSecretRef.name != '' && self.tls.clientCertSecretRef.name != '')",message="when database.tls is enabled (mode is neither empty nor 'disabled'), both database.tls.caBundleSecretRef.name and database.tls.clientCertSecretRef.name must be set"
	// +kubebuilder:validation:XValidation:rule="self.database == oldSelf.database",message="database name is immutable"
	// +kubebuilder:validation:XValidation:rule="has(self.clusterRef) == has(oldSelf.clusterRef)",message="database mode (managed clusterRef vs brownfield host) is immutable"
	Database commonv1.DatabaseSpec `json:"database"`

	// Cache defines the Memcached cache configuration.
	// Supports managed (clusterRef) and brownfield (servers) modes. The
	// clusterRef/servers mutual-exclusivity rule lives on commonv1.CacheSpec.
	Cache commonv1.CacheSpec `json:"cache"`

	// Fernet configures Fernet key rotation.
	Fernet FernetSpec `json:"fernet,omitempty"`

	// CredentialKeys configures credential key rotation.
	CredentialKeys CredentialKeysSpec `json:"credentialKeys,omitempty"`

	// PasswordRotation optionally enables scheduled rotation of the admin
	// password. Day-2 admin-password rotation is not a bootstrap concern, so it
	// lives at the spec root beside the fernet/credential key rotation
	// configuration. Nil (the default) leaves the feature off and the
	// sub-reconciler is a clean no-op. See PasswordRotationSpec for the opt-in
	// and per-CR semantics.
	// +optional
	PasswordRotation *PasswordRotationSpec `json:"passwordRotation,omitempty"`

	// TrustFlush configures periodic purging of expired trust delegations
	// The defaulting webhook materializes a populated
	// TrustFlushSpec when the field is unset so that the operator runs
	// keystone-manage trust_flush hourly by default — there is no nil-back
	// path on a webhook-enabled cluster, because admission re-defaults the
	// pointer if a user patches it to null. To pause the schedule without
	// removing the CronJob, set suspend: true.
	// +optional
	TrustFlush *TrustFlushSpec `json:"trustFlush,omitempty"`

	// Federation configures Keystone federation (optional).
	// +optional
	Federation *FederationSpec `json:"federation,omitempty"`

	// Bootstrap configures the initial Keystone bootstrap.
	Bootstrap BootstrapSpec `json:"bootstrap"`

	// Middleware defines WSGI middleware filters for the api-paste.ini pipeline.
	// +optional
	Middleware []commonv1.MiddlewareSpec `json:"middleware,omitempty"`

	// Plugins defines service plugins/drivers to configure. Modeled as a
	// list-map keyed by configSection so the API server rejects duplicate
	// sections structurally (parity with the validating webhook's duplicate
	// check) and server-side apply merges entries by section instead of
	// replacing the whole list.
	// +optional
	// +listType=map
	// +listMapKey=configSection
	Plugins []commonv1.PluginSpec `json:"plugins,omitempty"`

	// PolicyOverrides defines custom oslo.policy rules for the service.
	// When set, the operator renders a policy.yaml and configures
	// oslo_policy.policy_file automatically.
	// +optional
	// +kubebuilder:validation:XValidation:rule="(has(self.rules) && size(self.rules) > 0) || self.configMapRef != null",message="at least one of rules or configMapRef must be set"
	// The empty rule-name and rule-value constraints are enforced by the
	// XValidation markers on commonv1.PolicySpec itself, so they apply to every
	// PolicySpec field across operators without per-field duplication.
	PolicyOverrides *commonv1.PolicySpec `json:"policyOverrides,omitempty"`

	// Autoscaling configures horizontal pod autoscaling for the Keystone API deployment.
	// When set, a HorizontalPodAutoscaler is created targeting the deployment.
	// When removed, the HPA is deleted.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// NetworkPolicy configures network isolation for Keystone API pods.
	// When set, a NetworkPolicy is created restricting ingress and egress traffic.
	// When removed (nil), the NetworkPolicy is deleted and traffic flows unrestricted.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`

	// Gateway configures external exposure of the Keystone API via a Gateway API
	// HTTPRoute. When set, the operator creates an HTTPRoute targeting
	// the {name} Service on port 5000 and attaches it to the referenced
	// pre-existing Gateway. When removed (nil), the HTTPRoute is deleted.
	// The Gateway and GatewayClass are infrastructure concerns managed outside
	// this operator.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`

	// UWSGI configures the uWSGI application server parameters.
	// When set, the operator uses these values for the uWSGI command in the
	// Deployment. When nil, hardcoded defaults (processes=2, threads=1,
	// httpKeepAlive=true) are used.
	// +optional
	UWSGI *UWSGISpec `json:"uwsgi,omitempty"`

	// Logging configures oslo.log output for the Keystone API container.
	// When unset, the defaulting webhook materializes a LoggingSpec with
	// Format=text, Level=INFO, Debug=false — equivalent to the documented
	// production baseline (stdout/stderr, oslo.log format, no debug noise).
	// +optional
	Logging *LoggingSpec `json:"logging,omitempty"`

	// ExtraConfig provides free-form INI sections for configuration
	// not covered by explicit CRD fields.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`
}

// DeploymentSpec, AutoscalingSpec, NetworkPolicySpec,
// NetworkPolicyIngressSource, and LoggingSpec (below) are aliased to the
// shared commonv1 definitions. The generic workload spec types were
// consolidated into internal/common/types so every operator shares one source
// of truth; commonv1 carries the canonical per-field godoc and validation
// markers. These aliases keep existing references — keystonev1alpha1.DeploymentSpec
// and bare DeploymentSpec{} literals alike — compiling unchanged.
type (
	DeploymentSpec             = commonv1.DeploymentSpec
	AutoscalingSpec            = commonv1.AutoscalingSpec
	NetworkPolicySpec          = commonv1.NetworkPolicySpec
	NetworkPolicyIngressSource = commonv1.NetworkPolicyIngressSource
)

// UWSGISpec defines the uWSGI application server parameters.
// Exposed as an optional pointer field on KeystoneSpec so that existing CRs
// without spec.uwsgi continue to work with hardcoded defaults in the reconciler.
// The cross-field CEL rule mirrors the validating webhook: httpKeepAliveTimeout
// is only meaningful when httpKeepAlive is true, otherwise the
// --http-keepalive-timeout flag is never emitted.
// +kubebuilder:validation:XValidation:rule="!has(self.httpKeepAliveTimeout) || !has(self.httpKeepAlive) || self.httpKeepAlive",message="httpKeepAliveTimeout may only be set when httpKeepAlive is true"
type UWSGISpec struct {
	// Processes is the number of uWSGI worker processes.
	// The default literal mirrors DefaultUWSGIProcesses in keystone_webhook.go.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	Processes int32 `json:"processes,omitempty"`

	// Threads is the number of threads per uWSGI worker process.
	// The default literal mirrors DefaultUWSGIThreads in keystone_webhook.go.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Threads int32 `json:"threads,omitempty"`

	// HTTPKeepAlive enables the --http-keepalive flag on the uWSGI process.
	// When false, the flag is omitted from the command. It is a nil-preserving
	// pointer so "unset" is representable: the defaulting webhook restores the
	// documented default (true, DefaultUWSGIHTTPKeepAlive in keystone_webhook.go)
	// when the pointer is nil, and the reconciler falls back to the same default
	// for CRs that bypass the webhook. An explicit false is honored verbatim.
	// +optional
	HTTPKeepAlive *bool `json:"httpKeepAlive,omitempty"`

	// Harakiri caps the per-request worker lifetime (seconds) via the uWSGI
	// --harakiri flag. A request blocked longer than this bound is
	// killed so a single stuck LDAP/DB lookup cannot prevent other in-flight
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

// FernetSpec defines Fernet key rotation configuration.
type FernetSpec struct {
	// RotationSchedule is a cron expression for key rotation.
	// +kubebuilder:default="0 0 * * 0"
	RotationSchedule string `json:"rotationSchedule,omitempty"`

	// MaxActiveKeys is the maximum number of active Fernet keys.
	// +kubebuilder:validation:Minimum=3
	// +kubebuilder:default=3
	MaxActiveKeys int32 `json:"maxActiveKeys,omitempty"`

	// Suspend pauses the Fernet key rotation CronJob without deleting it,
	// letting an SRE halt rotation during an incident. Matches
	// TrustFlushSpec.Suspend semantics.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`
}

// CredentialKeysSpec defines credential key rotation configuration.
type CredentialKeysSpec struct {
	// RotationSchedule is a cron expression for credential key rotation.
	// +kubebuilder:default="0 0 * * 0"
	RotationSchedule string `json:"rotationSchedule,omitempty"`

	// MaxActiveKeys is the maximum number of active credential keys.
	// +kubebuilder:validation:Minimum=3
	// +kubebuilder:default=3
	MaxActiveKeys int32 `json:"maxActiveKeys,omitempty"`

	// Suspend pauses the credential key rotation CronJob without deleting it,
	// letting an SRE halt rotation during an incident. Matches
	// TrustFlushSpec.Suspend semantics.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`
}

// TrustFlushSpec configures periodic purging of expired trust delegations
// The defaulting webhook materializes the parent struct on
// KeystoneSpec.TrustFlush when unset, so the leaf +kubebuilder:default markers
// on Schedule and Suspend below fire deterministically and the trust-flush
// CronJob is created with the documented hourly schedule by default. The
// markers are kept as defense-in-depth for callers that bypass the webhook
// (e.g. envtest without the defaulter wired up). To pause without removing
// the CronJob, set Suspend: true — the condition reason remains TrustFlushReady.
type TrustFlushSpec struct {
	// The kubebuilder default literal below must stay in sync with
	// DefaultTrustFlushSchedule in keystone_webhook.go (kubebuilder markers
	// require a string literal and cannot reference Go constants).

	// Schedule is a cron expression controlling when keystone-manage trust_flush runs.
	// +kubebuilder:default="0 * * * *"
	Schedule string `json:"schedule,omitempty"`

	// Suspend pauses the CronJob without deleting it.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`

	// Args provides additional CLI flags passed to keystone-manage trust_flush.
	// +optional
	Args []string `json:"args,omitempty"`
}

// FederationSpec carries the Keystone-side federation knobs. Federation
// itself is activated by attaching a federation-typed KeystoneIdentityBackend
// (e.g. type OIDC) — not by this block: when at least one OIDC backend is
// projected, the operator injects the mod_auth_openidc reverse-proxy sidecar,
// binds uWSGI to localhost, and switches the Service to the proxy port. This
// spec only configures how that sidecar runs.
type FederationSpec struct {
	// ProxyImage is the Apache/mod_auth_openidc sidecar image projected when
	// a federation backend is attached. Standalone Keystone installations must
	// set it (mirroring the required spec.image); the managed ControlPlane
	// path projects the ghcr.io/c5c3/keystone-federation-proxy default. When a
	// federation backend is attached and no proxy image is configured, the
	// backends stay pending with a FederationProxyImageMissing warning — no
	// hidden default is assumed.
	// +optional
	ProxyImage *commonv1.ImageSpec `json:"proxyImage,omitempty"`
}

// PasswordRotationSpec configures scheduled rotation of the Keystone admin
// password (Model B / Part 2 of #381). When enabled, the operator runs
// a CronJob that periodically generates a fresh strong password and delivers it
// into OpenBao via a PushSecret; the existing keystone-admin ExternalSecret then
// round-trips it back into the cluster Secret and Part 1 re-bootstraps
// Keystone with the new credential.
//
// Unlike TrustFlushSpec, the defaulting webhook does NOT materialize this block
// when it is absent: scheduled rotation is strictly opt-in, so upgrading a CR
// that never set passwordRotation must never silently enable it. The defaulting
// webhook only fills the leaf defaults (Schedule, PasswordLength) once Enabled is
// true; when the pointer is nil the sub-reconciler is a clean no-op. The leaf
// +kubebuilder:default markers below remain as defense-in-depth for callers that
// bypass the webhook (e.g. envtest without the defaulter wired up).
//
// The admin-password backup is scoped per Keystone CR: the push path is the
// per-CR OpenBao key bootstrap/{namespace}/{name}/admin, so
// enabling rotation on multiple CRs no longer collides on a shared object.
type PasswordRotationSpec struct {
	// Enabled turns on scheduled admin-password rotation. Default false: the
	// feature is opt-in, and disabling it tears down every Model B resource.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// The kubebuilder default literal below must stay in sync with
	// DefaultPasswordRotationSchedule in keystone_webhook.go (kubebuilder markers
	// require a string literal and cannot reference Go constants).

	// Schedule is a cron expression controlling when a new admin password is
	// generated. Defaults to monthly at midnight on the 1st.
	// +kubebuilder:default="0 0 1 * *"
	Schedule string `json:"schedule,omitempty"`

	// Suspend pauses the CronJob without deleting it or any sibling resource,
	// matching TrustFlushSpec.Suspend semantics.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`

	// PasswordLength is the length of the generated password. The kubebuilder
	// default literal below must stay in sync with DefaultPasswordRotationLength,
	// and the Minimum literal with DefaultAdminPasswordMinLength, both in
	// keystone_webhook.go.
	// +kubebuilder:validation:Minimum=24
	// +kubebuilder:default=32
	PasswordLength int32 `json:"passwordLength,omitempty"`
}

// BootstrapSpec defines Keystone bootstrap parameters.
type BootstrapSpec struct {
	// AdminUser is the admin username for the bootstrap. Immutable after create:
	// re-bootstrapping against an existing deployment with a different admin user
	// duplicates or strands catalog entries (known duplicate-admin failure class).
	// The transition rule (self == oldSelf) is evaluated only on UPDATE and is
	// enforced by the API server, so it holds even when the webhook is down (#466).
	// +kubebuilder:default="admin"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="bootstrap.adminUser is immutable"
	AdminUser string `json:"adminUser,omitempty"`

	// AdminPasswordSecretRef references the Secret containing the admin password.
	AdminPasswordSecretRef commonv1.SecretRefSpec `json:"adminPasswordSecretRef"`

	// Region is the Keystone region name. Immutable after create: re-bootstrapping
	// against an existing deployment in a different region strands catalog entries
	// under the old region. The transition rule (self == oldSelf) is evaluated only
	// on UPDATE and is enforced by the API server, so it holds even when the webhook
	// is down (#466).
	// +kubebuilder:default="RegionOne"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="bootstrap.region is immutable"
	Region string `json:"region,omitempty"`

	// PublicEndpoint is the externally routable Keystone endpoint URL used for
	// --bootstrap-public-url. When unset, the cluster-local service DNS is used
	// as a fallback. External clients (CLI users, Horizon, federation partners)
	// require a routable address here. The pattern enforces an HTTP(S)
	// URL shape unconditionally; the webhook additionally cross-checks the host
	// against spec.gateway.hostname when a gateway is configured.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	PublicEndpoint string `json:"publicEndpoint,omitempty"`
}

// GatewaySpec and GatewayParentRefSpec are aliased to the shared commonv1
// definitions. The Gateway API HTTPRoute exposure types (originally) were consolidated into internal/common/types so every operator
// shares one source of truth; commonv1 carries the canonical per-field godoc
// and validation markers. These aliases keep existing references —
// keystonev1alpha1.GatewaySpec and bare GatewaySpec{} literals alike —
// compiling unchanged.
type (
	GatewaySpec          = commonv1.GatewaySpec
	GatewayParentRefSpec = commonv1.GatewayParentRefSpec
)

// UpgradePhase represents the current phase of a database upgrade.
// +kubebuilder:validation:Enum=Expanding;Migrating;RollingUpdate;Contracting
type UpgradePhase string

const (
	UpgradePhaseExpanding     UpgradePhase = "Expanding"
	UpgradePhaseMigrating     UpgradePhase = "Migrating"
	UpgradePhaseRollingUpdate UpgradePhase = "RollingUpdate"
	UpgradePhaseContracting   UpgradePhase = "Contracting"
)

// KeystoneStatus defines the observed state of Keystone.
type KeystoneStatus struct {
	// Conditions represent the latest available observations of the Keystone
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

	// Endpoint is the Keystone API endpoint URL.
	Endpoint string `json:"endpoint,omitempty"`

	// InstalledRelease is the OpenStack release version currently deployed.
	InstalledRelease string `json:"installedRelease,omitempty"`

	// TargetRelease is the upgrade target release during an active upgrade.
	TargetRelease string `json:"targetRelease,omitempty"`

	// UpgradePhase is the current phase of a database upgrade.
	UpgradePhase UpgradePhase `json:"upgradePhase,omitempty"`
}

// LoggingSpec is aliased to the shared commonv1 definition (see the
// DeploymentSpec alias block above for the rationale).
type LoggingSpec = commonv1.LoggingSpec

func init() {
	SchemeBuilder.Register(&Keystone{}, &KeystoneList{})
}
