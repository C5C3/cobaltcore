// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
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

	// ImportFiltering constrains the URIs the web-download image-import method
	// may fetch from, rendered as the [import_filtering_opts] group in
	// glance-api.conf. The operator resolves the effective lists at render time —
	// HTTPS-only on port 443, plus a literal host denylist covering loopback, the
	// link-local metadata endpoint, and the in-cluster API server — rather than
	// materializing them into the CR, so a nil block resolves exactly like an
	// empty struct and both keep tracking the operator defaults.
	// +optional
	ImportFiltering *ImportFilteringSpec `json:"importFiltering,omitempty"`

	// ImportPlugins enables glance's image-import plugins, rendered as the
	// [image_import_opts] image_import_plugins list plus the section each enabled
	// plugin reads. Presence of a sub-block enables that plugin and nil leaves it
	// out, the same opt-in convention spec.imageCache follows. The operator
	// renders the list in the fixed order decompression, conversion,
	// inject_image_metadata — glance runs the plugins in list order and requires
	// decompression before conversion, which holds here by construction because
	// there is no ordering input. Every default resolves at render time rather
	// than in the defaulting webhook, so a field left unset keeps tracking the
	// operator defaults across upgrades instead of freezing today's values into
	// the stored CR.
	//
	// The plugins act in the interoperable image import flow alone
	// (POST /v2/images/{image_id}/import). A plain PUT /v2/images/{image_id}/file
	// upload never reaches them, and glance skips them for the copy-image import
	// method, so with the operator's pinned enabled_import_methods of
	// web-download and copy-image they take effect exactly on web-download
	// imports.
	// +optional
	ImportPlugins *ImportPluginsSpec `json:"importPlugins,omitempty"`

	// DBPurge tunes the recurring database purge that hard-deletes the rows
	// glance only ever soft-deletes. The operator resolves the effective
	// retention and schedule at reconcile time — DefaultDBPurgeRetentionDays and
	// DefaultDBPurgeSchedule — rather than materializing them into the CR, so a
	// nil block resolves exactly like an empty struct and both keep tracking the
	// operator defaults. The task-row sweep is scheduled on every Glance; the
	// destructive extras are opt-in (purgeImagesTable) or reversible (suspend).
	// +optional
	DBPurge *DBPurgeSpec `json:"dbPurge,omitempty"`

	// Staging bounds the node-local scratch space an image import may consume.
	// The operator resolves the effective size limit at reconcile time —
	// DefaultStagingSizeLimit — rather than materializing it into the CR, so a
	// nil block, an empty block, and a nil sizeLimit all mean the same thing:
	// use the operator default. Leaving it unset therefore keeps tracking that
	// default across upgrades instead of freezing today's value into the stored
	// CR. Setting staging.unbounded opts out of the bound altogether.
	// +optional
	Staging *StagingSpec `json:"staging,omitempty"`

	// ImageCache turns on the per-replica image cache: presence of the block
	// enables it, nil disables it, the same opt-in convention gateway,
	// networkPolicy and autoscaling follow. Both of its fields resolve at render
	// time — DefaultImageCacheSizeLimit and DefaultImageCacheMaintenanceInterval —
	// rather than being materialized into the CR, so an empty block and an unset
	// field both mean "use the operator default" and keep tracking it across
	// upgrades.
	//
	// Enabling, resizing, or retuning the cache rolls the Deployment exactly once,
	// through the existing content-hash mechanics. Every such rollout starts the
	// cache cold: it lives in a fresh emptyDir per replica, so after a roll the
	// first read of each image goes to the backing store again.
	// +optional
	ImageCache *ImageCacheSpec `json:"imageCache,omitempty"`

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
	// explicit CRD fields. It is the deliberate escape hatch for options that have
	// no dedicated knob of their own.
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
	// (uWSGI launch mode, where uwsgi applies instead). When nil below 2026.1 the
	// operator renders DefaultEventletWorkers rather than letting the eventlet
	// server fall back to one worker per host CPU, so the worker count — and thus
	// the memory footprint — stays deterministic regardless of node size.
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

// ImportFilteringSpec constrains the URIs the web-download image-import method
// may fetch from, rendered as the [import_filtering_opts] group in
// glance-api.conf. Each attribute — scheme, host, port — carries an allow-list
// and a deny-list, but glance only ever evaluates one of the two: a non-empty
// allow-list makes it ignore the matching deny-list entirely. Setting both for
// the same attribute would silently drop the deny-list, so the CEL rules below
// reject the combination instead of letting it look effective. Host matching is
// literal string membership; glance supports neither CIDR ranges nor wildcards,
// so an entry must spell out the host exactly as an import URI would.
//
// The operator resolves the effective lists at render time rather than in the
// defaulting webhook, so a field left unset keeps tracking the operator defaults
// (DefaultImportAllowedSchemes, DefaultImportAllowedPorts and
// DefaultImportDisallowedHosts in glance_webhook.go) across upgrades instead of
// freezing today's values into the stored CR. Only a nil field is defaulted: an
// explicitly empty list is honored as empty, which is how a deployment opts out
// of a default restriction and falls back to glance's own behavior. Setting one
// field never relaxes the default resolved for another — a deny-list entry keeps
// the sibling allow-list default, and host entries are unioned onto the operator
// denylist rather than replacing it — so no edit to this block can widen the
// filter except the explicit opt-out.
//
// The three messages below are mirrored verbatim by the webhook constants
// importFilteringSchemesExclusiveMsg, importFilteringHostsExclusiveMsg and
// importFilteringPortsExclusiveMsg — markers cannot reference a Go constant, so
// the literals must stay in sync.
// +kubebuilder:validation:XValidation:rule="!(has(self.allowedSchemes) && self.allowedSchemes.size() > 0 && has(self.disallowedSchemes) && self.disallowedSchemes.size() > 0)",message="allowedSchemes and disallowedSchemes are mutually exclusive: glance ignores the deny-list when the allow-list is non-empty"
// +kubebuilder:validation:XValidation:rule="!(has(self.allowedHosts) && self.allowedHosts.size() > 0 && has(self.disallowedHosts) && self.disallowedHosts.size() > 0)",message="allowedHosts and disallowedHosts are mutually exclusive: glance ignores the deny-list when the allow-list is non-empty"
// +kubebuilder:validation:XValidation:rule="!(has(self.allowedPorts) && self.allowedPorts.size() > 0 && has(self.disallowedPorts) && self.disallowedPorts.size() > 0)",message="allowedPorts and disallowedPorts are mutually exclusive: glance ignores the deny-list when the allow-list is non-empty"
type ImportFilteringSpec struct {
	// AllowedSchemes lists the URI schemes web-download may fetch from
	// ([import_filtering_opts] allowed_schemes). When non-empty, glance ignores
	// disallowedSchemes. Glance's own default is [http, https]; when this field
	// is nil the operator narrows it to DefaultImportAllowedSchemes.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Enum=http;https
	AllowedSchemes []string `json:"allowedSchemes,omitempty"`

	// DisallowedSchemes lists the URI schemes web-download must refuse
	// ([import_filtering_opts] disallowed_schemes). Ignored by glance whenever
	// allowedSchemes is non-empty, which is why the two are mutually exclusive
	// here — including when allowedSchemes is nil and resolves to
	// DefaultImportAllowedSchemes, so this list only becomes authoritative once
	// allowedSchemes is set to an explicitly empty list.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Enum=http;https
	DisallowedSchemes []string `json:"disallowedSchemes,omitempty"`

	// AllowedHosts lists the hosts web-download may fetch from
	// ([import_filtering_opts] allowed_hosts). When non-empty, glance ignores
	// disallowedHosts and every other host is refused — the tightest way to pin
	// imports to a known image mirror. Empty by default, since the operator
	// ships a host denylist instead.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	AllowedHosts []string `json:"allowedHosts,omitempty"`

	// DisallowedHosts lists the hosts web-download must refuse
	// ([import_filtering_opts] disallowed_hosts). When this field is nil the
	// operator renders DefaultImportDisallowedHosts, which blocks loopback, the
	// link-local metadata endpoint, and the in-cluster API server; entries set
	// here are unioned onto that denylist, so adding one extends the baseline
	// instead of replacing it. Ignored by glance whenever allowedHosts is
	// non-empty. An explicitly empty list drops the baseline — the opt-out.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	DisallowedHosts []string `json:"disallowedHosts,omitempty"`

	// AllowedPorts lists the ports web-download may connect to
	// ([import_filtering_opts] allowed_ports). When non-empty, glance ignores
	// disallowedPorts. Glance's own default is [80, 443]; when this field is nil
	// the operator narrows it to DefaultImportAllowedPorts.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	AllowedPorts []int32 `json:"allowedPorts,omitempty"`

	// DisallowedPorts lists the ports web-download must refuse
	// ([import_filtering_opts] disallowed_ports). Ignored by glance whenever
	// allowedPorts is non-empty, which is why the two are mutually exclusive
	// here — including when allowedPorts is nil and resolves to
	// DefaultImportAllowedPorts, so this list only becomes authoritative once
	// allowedPorts is set to an explicitly empty list.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	DisallowedPorts []int32 `json:"disallowedPorts,omitempty"`
}

// ImportPluginsSpec selects the image-import plugins glance runs. They are
// taskflow stages the import flow applies to the staged image before it reaches
// the backing store, and glance runs none of them unless [image_import_opts]
// image_import_plugins names them. A sub-block set here is that naming: the
// plugin joins the list, nil leaves it out.
//
// The rendered order is fixed — decompression, conversion,
// inject_image_metadata — and is not an input. Glance runs the plugins in list
// order, and conversion ahead of decompression would rewrite the archive rather
// than the disk image inside it, so the one ordering constraint upstream
// documents is satisfied by construction.
//
// The blocks only reach imports that go through the interoperable image import
// flow. An image uploaded with PUT /v2/images/{image_id}/file bypasses the
// plugins entirely, so a deployment that relies on conversion or on injected
// metadata has to keep that upload path out of its policy.
type ImportPluginsSpec struct {
	// Decompression enables the image_decompression plugin, which unpacks a
	// compressed image after staging so the later plugins and the backing store
	// see the disk image rather than the archive. It unpacks by an unbounded
	// ratio and glance's image_size_cap bounds only the bytes transferred, so
	// size spec.staging.sizeLimit against the largest UNPACKED image: breaching
	// that bound evicts the glance-api pod and every in-flight import on the
	// replica with it. Setting this block therefore requires spec.staging to
	// answer for that bound — either sizeLimit or unbounded — because the
	// operator default was sized against the largest download. See
	// ImportDecompressionSpec.
	// +optional
	Decompression *ImportDecompressionSpec `json:"decompression,omitempty"`

	// Conversion enables the image_conversion plugin, which rewrites the staged
	// image into one target disk format.
	// +optional
	Conversion *ImportConversionSpec `json:"conversion,omitempty"`

	// InjectMetadata enables the inject_image_metadata plugin, which stamps a
	// fixed set of image properties onto every image it applies to.
	// +optional
	InjectMetadata *ImportInjectMetadataSpec `json:"injectMetadata,omitempty"`
}

// ImportDecompressionSpec enables the image_decompression plugin. Glance
// identifies the staged file by its magic number and unpacks it in place, which
// is what lets an import fetch a compressed image over a slow link and still
// land an uncompressed disk image in the store. It recognizes gzip, zip, and
// LHA, the last only where the optional lhafile package is present. bzip2 and xz
// are not recognized, so an image compressed either way passes through untouched
// and reaches the store as the archive it arrived as.
//
// The plugin unpacks one compression layer and no more. A zip or LHA archive
// must hold exactly one file, and a multi-layer archive such as tar.gz is
// unsupported: unpacking the outer layer yields the tar, which is no more a disk
// image than the archive was.
//
// The expansion is UNBOUNDED, and it is the caller who picks the ratio. The
// plugin unpacks in place with no cap on the result and cannot know that size
// before writing it, and glance's image_size_cap is no help: it measures the
// bytes transferred, so a ~10 MiB gzip that expands past 10 GiB stays well
// under any cap the deployment sets. Nothing in this operator caps it either,
// which leaves spec.staging.sizeLimit as the only bound in the path — which is
// why admission refuses to let that bound stay an inherited default here: a CR
// setting this block must set spec.staging.sizeLimit, or spec.staging.unbounded
// to deliberately keep none. Breaching
// that bound evicts the glance-api pod rather than the offending import, and
// within one pod it is a single shared budget rather than per-import accounting
// (see StagingSpec), so every other in-flight import on that replica dies with
// it — the blast radius is per-replica and shared, and on a single-replica
// Glance one import is a full image-service outage. Enable this only where the
// import path is already restricted to trusted callers by oslo.policy, and size
// spec.staging.sizeLimit against the largest UNPACKED image rather than the
// largest download. Sibling ImportConversionSpec carries a bounded 2x cost by
// comparison: conversion works on a file the caller had to transfer in full.
//
// It carries no fields: upstream registers no options for the plugin, so the
// presence of the block is the entire configuration. It is a struct rather than
// a bool so a future upstream option can be added without changing the shape a
// stored CR carries.
type ImportDecompressionSpec struct{}

// ImportConversionSpec enables the image_conversion plugin, which converts the
// staged image to a single disk format before it reaches the backing store.
// Uniform image formats are what make a Ceph/RBD backend clone an image
// copy-on-write instead of flattening a full copy per boot, and they close the
// gap where an image's declared disk_format disagrees with its actual content.
//
// The conversion runs on the node-local staging area and needs room for the
// source and the result at once, so enabling it raises the scratch space an
// import consumes — size spec.staging.sizeLimit accordingly.
type ImportConversionSpec struct {
	// OutputFormat is the disk format every imported image is converted to
	// ([image_conversion] output_format). When empty the operator resolves
	// DefaultImportConversionOutputFormat at render time rather than
	// materializing it into the CR, so leaving it unset keeps tracking that
	// default across upgrades. The accepted values are glance's own: raw, which
	// an RBD store can clone; qcow2, which stays sparse on a filesystem store;
	// and vmdk.
	// +optional
	// +kubebuilder:validation:Enum=qcow2;raw;vmdk
	OutputFormat string `json:"outputFormat,omitempty"`
}

// ImportInjectMetadataSpec enables the inject_image_metadata plugin, which
// stamps operator-chosen image properties onto imported images. The properties
// are how a deployment marks provenance or pins scheduling and driver behaviour
// on images it did not build itself, without trusting the importing user to set
// them.
//
// The properties are not immutable afterwards: the plugin writes them during
// the import, and whether the image owner may change them later is an
// oslo.policy question (modify_image / the protected-property rules), not one
// this block answers.
type ImportInjectMetadataSpec struct {
	// Properties are the image properties injected onto every image the plugin
	// applies to ([inject_metadata_properties] inject). At least one entry is
	// required: an empty map would enable a plugin with nothing to do. The
	// operator renders the pairs with the keys sorted alphabetically, so the
	// unordered map cannot reshuffle the rendered config and roll the Deployment
	// on a reconcile that changed nothing.
	//
	// The rendered value uses the oslo Dict syntax, which splits pairs on commas
	// and each pair on its FIRST colon. A key therefore carries neither
	// character; a value may carry a colon (a URL-shaped value works) but no
	// comma. The validating webhook enforces this — a map key has no marker of
	// its own.
	//
	// Both halves are bounded at 255 characters. Every pair renders verbatim into
	// the one inject line, so an unbounded value would let a CR that etcd accepts
	// render a glance-api.conf past the 1 MiB ConfigMap ceiling: admission would
	// pass the reconciler an object the API server then refuses on every attempt.
	// The CEL rule is the schema counterpart the map's halves otherwise lack; the
	// webhook repeats it as defense in depth.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 255 && size(self[k]) <= 255)",message="each injected property name and value must be at most 255 characters"
	Properties map[string]string `json:"properties"`

	// IgnoreUserRoles lists the Keystone roles exempt from the injection
	// ([inject_metadata_properties] ignore_user_roles): an import by a user
	// holding one of them leaves the image's properties untouched. When nil the
	// operator resolves DefaultImportInjectIgnoreUserRoles at render time —
	// glance's own default, which exempts admins — so an unset field keeps
	// tracking that default across upgrades.
	//
	// An explicitly empty list is honored verbatim and is the opt-in to injecting
	// for every role including admin, the same nil-versus-empty contract the
	// spec.importFiltering lists carry.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=255
	IgnoreUserRoles []string `json:"ignoreUserRoles,omitempty"`
}

// DBPurgeSpec tunes the recurring database purge. Glance never hard-deletes on
// its own: deleting an image only flips its row to deleted, and every image
// import leaves a task row behind, so both grow for the lifetime of the
// deployment. The operator projects a CronJob running glance-manage db purge,
// which sweeps the tasks table and the image child tables — the unbounded growth
// this block exists to bound. The images table itself is a separate, opt-in pass
// (PurgeImagesTable) because db purge deliberately skips it: while the row
// survives, a deleted image's UUID can never be handed out again to a different
// image (OSSN-0075).
//
// The operator resolves the knobs at reconcile time rather than in the
// defaulting webhook, so a field left unset keeps tracking the operator defaults
// (DefaultDBPurgeRetentionDays and DefaultDBPurgeSchedule in glance_webhook.go)
// across upgrades instead of freezing today's values into the stored CR. A nil
// block therefore resolves exactly like an empty struct: the task sweep runs
// daily on every Glance, since an unbounded soft-delete backlog is a deferred
// outage rather than a posture worth offering.
type DBPurgeSpec struct {
	// RetentionDays is how long a soft-deleted row survives before the purge
	// hard-deletes it — the --age_in_days argument both commands take. When unset
	// the operator resolves DefaultDBPurgeRetentionDays; the lower bound of one
	// day keeps the purge from racing the rows an in-flight import just wrote.
	// Lowering it applies retroactively at the next firing, so the validating
	// webhook warns on a reduction.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RetentionDays *int32 `json:"retentionDays,omitempty"`

	// Schedule is the standard cron expression the purge CronJob runs on. When
	// empty the operator resolves DefaultDBPurgeSchedule. The value is checked by
	// the validating webhook rather than by a CRD pattern — as with the keystone
	// schedules, the accepted grammar includes descriptors such as @daily, which
	// no regex expresses without also rejecting valid expressions.
	//
	// It is also the only lever over purge throughput: each run deletes at most a
	// fixed number of rows per table (the operator-owned --max_rows cap), so a
	// deployment whose daily soft-deletes outgrow one run's ceiling needs a more
	// frequent schedule to keep up.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// PurgeImagesTable enables the second glance-manage db purge_images_table
	// pass, which hard-deletes the images rows themselves. It defaults to false:
	// the row is what keeps a deleted image's UUID reserved, and glance's v2
	// image-create accepts a client-supplied id, so once the row is gone any
	// project member can claim the freed UUID for an image of their own and every
	// Heat template, Terraform state, or server image_ref still pinning it boots
	// that image instead (OSSN-0075). Enable it only where policy denies
	// client-supplied image IDs. The images table grows far more slowly than the
	// tasks table db purge already covers.
	// +optional
	PurgeImagesTable *bool `json:"purgeImagesTable,omitempty"`

	// Suspend pauses the purge CronJob without deleting it, matching the keystone
	// TrustFlushSpec.Suspend semantics. It is the escape hatch for a brownfield
	// deployment onboarding onto this operator: the first run applies the
	// retention window retroactively to a backlog that has never been purged, so
	// an operator who wants to stage that can suspend the CronJob, raise
	// RetentionDays to cover the deployment's full history, and step it down while
	// watching glance_operator_db_purge_total. The condition stays True while
	// suspended — a paused purge is a deliberate posture, not a failure.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// StagingSpec bounds the node-local scratch space image imports consume. Glance
// stages every import on local disk before the data reaches the backing store —
// the reserved staging store takes the uploaded image, the reserved tasks-work
// store the async task's working copy — and both are emptyDirs on the node
// filesystem. Left unbounded, one oversized web-download import fills the node's
// disk.
//
// The resolved value is stamped as the SizeLimit of BOTH emptyDirs, so one
// glance-api pod is expected to occupy at most twice the configured value.
//
// The enforcement is best-effort, not a filesystem quota. emptyDir.sizeLimit is
// an eviction threshold the kubelet evaluates on its periodic local-storage
// housekeeping pass (~10s), so actual peak usage is the bound plus whatever a
// writer appends within one pass — a full-speed in-cluster download overshoots
// by that much, and a download shorter than one pass can finish before the
// kubelet ever looks. The bound is also not a scheduling reservation: no
// ephemeral-storage request is derived from it, so co-scheduled replicas each
// get their own budget and the sum is bounded by the node's disk, not by this
// field. Size nodes against replicas x 2 x sizeLimit plus that overshoot.
//
// Within one pod the bound is a single shared budget, not per-import accounting:
// concurrent imports draw from it together, and breaching it evicts the pod
// rather than the offending import — every in-flight transfer on that replica
// dies with it. That contains a runaway import to one glance-api pod; it does
// not isolate tenants from each other.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.unbounded) && self.unbounded && has(self.sizeLimit))",message="unbounded and sizeLimit are mutually exclusive: an unbounded staging area has no size limit to configure"
type StagingSpec struct {
	// SizeLimit caps each of the two scratch emptyDirs, staging and tasks-work.
	// It must be at least 1Mi. When unset the operator resolves
	// DefaultStagingSizeLimit (10Gi) at reconcile time.
	// +optional
	SizeLimit *resource.Quantity `json:"sizeLimit,omitempty"`

	// Unbounded removes the bound entirely: the operator renders both scratch
	// emptyDirs without a sizeLimit, so an import may consume node disk until
	// the node itself runs out. It is the escape hatch for a deployment whose
	// largest import cannot be expressed as a number worth committing to —
	// notably one that ran before this field existed, since an operator upgrade
	// otherwise stamps DefaultStagingSizeLimit onto its Deployment and starts
	// evicting on imports that used to succeed. Prefer raising SizeLimit; reach
	// for this only to deliberately restore the pre-bound behaviour, and size
	// the node accordingly, because nothing then stops one import from filling
	// the disk out from under every other pod on it.
	//
	// It is a separate field rather than a sentinel SizeLimit so the 1Mi floor
	// keeps rejecting the sub-byte quantities it exists to catch. Setting both
	// is a contradiction and rejected at admission.
	// +optional
	Unbounded bool `json:"unbounded,omitempty"`
}

// ImageCacheSpec configures the local image cache. Glance keeps a copy of the
// image data it has served on the API pod's own filesystem, so a repeat download
// of the same image — a popular image booting across a fleet — is served from
// there instead of from the backing store.
//
// The cache is per replica, not shared: every pod fills its own copy, an image
// is cached once per replica that served it, and a request hits the cache only
// when it lands on a replica that already holds the image. Sizing therefore
// multiplies by replicas, and hit rates rise as replicas fall.
//
// The operator fixes the parts not worth a knob. The cache directory is an
// emptyDir bounded by SizeLimit, and the driver is pinned to sqlite, which keeps
// its metadata inside that same directory: cache state then shares the volume's
// lifecycle and the volume the pod's, so a replaced pod leaves nothing behind.
// Glance's own default driver, centralized_db, would instead key that state in
// the Glance database per worker_self_reference_url and strand a dead pod's rows
// there on every replacement. Eviction is not glance's business either: a
// maintenance sidecar runs glance-cache-pruner and glance-cache-cleaner, both
// every MaintenanceInterval and as soon as its size poll finds the cache above
// image_cache_max_size.
type ImageCacheSpec struct {
	// SizeLimit bounds the per-replica cache emptyDir. When unset the operator
	// resolves DefaultImageCacheSizeLimit (10Gi) at render time. It must be at
	// least 1Mi, enforced by the validating webhook.
	//
	// The rendered image_cache_max_size is 80% of this bound (Value()/10*8), not
	// the bound itself. Glance's pruner only prunes DOWN TO image_cache_max_size
	// and only when it runs, so the cache legitimately sits above that mark
	// between two maintenance passes; the 20% band is the headroom that keeps
	// those writes from crossing the emptyDir bound and tripping the kubelet's
	// eviction. A single image larger than image_cache_max_size is still cached in
	// full — the size check happens after the download — and pruned on the next
	// pass.
	// +optional
	SizeLimit *resource.Quantity `json:"sizeLimit,omitempty"`

	// MaintenanceInterval is the scheduled cadence of the cache-maintenance
	// sidecar loop: glance-cache-pruner, which evicts cache entries back down to
	// image_cache_max_size, and glance-cache-cleaner, which removes the stalled
	// entries an interrupted download leaves behind. When unset the operator
	// resolves DefaultImageCacheMaintenanceInterval (5m) at render time. It must
	// be at least 1m, enforced by the validating webhook.
	//
	// It is the lever over how long a stalled entry survives, NOT over how far a
	// download burst may overshoot: the same loop also polls the cache directory's
	// size and prunes as soon as it crosses image_cache_max_size, whatever this
	// interval says. That poll is what defends the 20% headroom, because glance
	// consults image_cache_max_size in the pruner alone and never on the write
	// path.
	// +optional
	MaintenanceInterval *metav1.Duration `json:"maintenanceInterval,omitempty"`
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

	// UpgradePhase is the current expand-migrate-contract phase during an active
	// release upgrade (Expanding, Migrating, RollingUpdate, Contracting); empty
	// when no upgrade is in flight.
	UpgradePhase commonv1.UpgradePhase `json:"upgradePhase,omitempty"`
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
