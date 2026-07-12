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
// +kubebuilder:printcolumn:name="Release",type="string",JSONPath=".spec.openStackRelease"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ControlPlane is the Schema for the controlplanes API. It is the
// top-level aggregate that projects an OpenStack control plane: it owns shared
// infrastructure references (database, cache) and a curated set of service
// specs (today: keystone) that the reconciler (L2) materializes into the
// per-service CRs.
type ControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ControlPlaneSpec   `json:"spec,omitempty"`
	Status ControlPlaneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ControlPlaneList contains a list of ControlPlane.
type ControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlane `json:"items"`
}

// ControlPlaneSpec defines the desired state of a ControlPlane.
type ControlPlaneSpec struct {
	// OpenStackRelease is the OpenStack release the control plane targets,
	// e.g. "2025.2". The reconciler (L2) projects this into each service CR's
	// image tag. The pattern matches the OpenStack date-based release scheme
	// (YYYY.N where N is 1 or 2 — the two-releases-per-year cadence, e.g.
	// 2024.1, 2025.2). The [12] minor class keeps this CRD pattern, the
	// webhook's controlPlaneReleaseRegexp, and release.ParseRelease in agreement
	// so a non-cadence minor (e.g. 2025.9) is rejected at every layer.
	//
	// The field stays required in both keystone modes. In External mode it is
	// ADVISORY: no images are deployed, so the value only needs to match the
	// external installation's release at the phase-3 managed takeover — until
	// then it is recorded but unused by the External-mode reconciler.
	// +kubebuilder:validation:Pattern=`^\d{4}\.[12]$`
	OpenStackRelease string `json:"openStackRelease"`

	// Region is the OpenStack region name applied across the control plane.
	// DECISION (plan decision #4): defaults to "RegionOne" via both the
	// CRD schema default (normal admission path) and the defaulting webhook
	// (callers that bypass the CRD default), mirroring BootstrapSpec.Region in the
	// keystone operator.
	// +kubebuilder:default="RegionOne"
	// +optional
	Region string `json:"region,omitempty"`

	// Infrastructure declares the shared backing services (database, cache)
	// that the control plane's services connect to.
	//
	// Required in Managed keystone mode (or when services.keystone is unset) and
	// forbidden in External keystone mode. Preserving today's contract, the
	// validating webhook rejects a non-External ControlPlane without it and the
	// defaulting webhook materializes the omitted block; an External-mode
	// ControlPlane manages identity against a pre-existing Keystone and provisions
	// no backing services, so infrastructure is forbidden (phase 2 will relax this
	// to optional). The Go field is a pointer (hence +optional at the CRD schema
	// layer) so External mode can omit it; the mode-conditional required/forbidden
	// rules live in the validating webhook because CEL cannot express a
	// cross-field rule spanning spec.infrastructure and spec.services.keystone.
	// +optional
	Infrastructure *InfrastructureSpec `json:"infrastructure,omitempty"`

	// SecretStoreRef selects the External Secrets store the ControlPlane and its
	// service children route ExternalSecrets and PushSecrets through. When
	// omitted the operator PROVISIONS a per-tenant namespaced store
	// (openbao-tenant-store) in the control plane's namespace and defaults the
	// control plane and its Keystone/Horizon children onto it, so every control
	// plane reaches OpenBao as its own tenant identity — the enforced default that
	// makes OpenBao itself, not a naming convention, isolate one control plane's
	// secret material from another. Set this field to override that default with an
	// explicit store (e.g. a namespaced store you manage, or the shared
	// cluster-scoped openbao-cluster-store); the operator then provisions nothing
	// and uses the store you name. The reference is projected onto the Keystone and
	// Horizon children, so setting it here is the single place operators configure
	// it. It is deliberately MUTABLE: switching stores re-points the identity while
	// the operator moves the fernet/credential key material in place, never
	// re-creating it.
	// +optional
	SecretStoreRef *commonv1.SecretStoreRefSpec `json:"secretStoreRef,omitempty"`

	// Services declares the per-service configuration projected into the
	// individual service CRs.
	Services ServicesSpec `json:"services"`

	// GlobalPolicyOverrides defines oslo.policy overrides applied across every
	// service in the control plane. Named to parallel
	// services.keystone.policyOverrides, whose per-service rules take precedence
	// over these global rules when both are set.
	// +optional
	GlobalPolicyOverrides *commonv1.PolicySpec `json:"globalPolicyOverrides,omitempty"`

	// KORC configures the K-ORC (OpenStack Resource Controller) integration used
	// to bootstrap and rotate the admin application credential and any declared
	// bootstrap resources.
	KORC KORCSpec `json:"korc"`
}

// InfrastructureSpec declares the shared backing services for the control
// plane. Both fields reuse the canonical commonv1 shapes so the
// ControlPlane and the per-service CRs validate the database/cache the same
// way.
type InfrastructureSpec struct {
	// Database defines the MariaDB connection parameters shared by the control
	// plane. Supports managed (clusterRef) and brownfield (host) modes; exactly
	// one must be set. That invariant is carried by the embedded commonv1.DatabaseSpec
	// type-level CEL rule (and the validating webhook), mirroring keystone — no
	// field-level marker is needed here, and duplicating it would emit the rule twice.
	Database commonv1.DatabaseSpec `json:"database"`

	// Cache defines the Memcached configuration shared by the control plane.
	// Supports managed (clusterRef) and brownfield (servers) modes; exactly one
	// must be set. That invariant is carried by the embedded commonv1.CacheSpec
	// type-level CEL rule (and the validating webhook), mirroring keystone — no
	// field-level marker is needed here, and duplicating it would emit the rule twice.
	Cache commonv1.CacheSpec `json:"cache"`
}

// ServicesSpec declares the per-service configuration of the control plane.
// Keystone and Horizon are modeled today; additional services are added as
// optional pointer fields as the operator grows.
type ServicesSpec struct {
	// Keystone configures the Keystone service projected by the reconciler.
	// Optional: a ControlPlane with services.keystone unset manages no Keystone
	// service (staged adoption, or an externally-managed Keystone), and the
	// reconciler reports KeystoneReady as not-managed. Flipping this from set to
	// nil deletes the previously-projected Keystone child.
	// +optional
	Keystone *ServiceKeystoneSpec `json:"keystone,omitempty"`

	// Horizon configures the Horizon dashboard projected by the reconciler.
	// Optional: a ControlPlane with services.horizon unset manages no dashboard
	// and the reconciler reports HorizonReady as not-managed. The projection is
	// gated on KeystoneReady — the dashboard authenticates against the
	// ControlPlane's Keystone child, so it is only created once that child is
	// ready.
	// +optional
	Horizon *ServiceHorizonSpec `json:"horizon,omitempty"`
}

// ServiceKeystoneSpec is a CURATED LOCAL subset of the knobs the ControlPlane
// exposes for the Keystone service.
//
// DECISION (plan decision #2): this is intentionally NOT an import of
// keystonev1alpha1.KeystoneSpec. The reconciler (L2) PROJECTS this
// struct into a Keystone CR; the database, cache, and Fernet rotation schedule
// of that Keystone CR are DERIVED from the ControlPlane (infrastructure.* and
// operator policy) rather than set by the user here. Keeping a curated subset
// avoids leaking every Keystone knob through the aggregate and keeps the L1 api
// package free of a dependency on the keystone module (see DECISION on L2
// dependency coordinates below).
//
// DECISION (plan decision #3 — L2 dependency coordinates): the L1 api
// package imports ONLY commonv1, k8s.io/apimachinery/*, k8s.io/api/core/v1, and
// sigs.k8s.io/controller-runtime/* (all already in go.mod). `go mod tidy`
// therefore prunes any service-module require because nothing here imports
// them. The L2 reconciler will need these coordinates (recorded here so the
// orchestrator does not have to re-resolve them):
//   - keystone           => ../keystone (local replace directive)
//   - mariadb-operator    => github.com/mariadb-operator/mariadb-operator v0.38.1
//   - external-secrets    => github.com/external-secrets/external-secrets/apis
//     (match the pin in operators/keystone/go.mod)
//   - K-ORC               => github.com/k-orc/openstack-resource-controller/v2 v2.5.0
//   - memcached.c5c3.io   => NO public Go module; L2 uses unstructured.Unstructured
//
// Mode is the Managed|External discriminator (default Managed). In Managed mode
// (or unset) the reconciler projects a full Keystone service exactly as before.
// In External mode the ControlPlane manages identity against a pre-existing,
// externally-operated Keystone (external.authURL) and deploys no Keystone
// workload; the managed-only knobs below are forbidden and the typed external
// block is required. The intra-struct invariants are expressed as type-level CEL
// rules so they hold at the CRD schema layer even when the validating webhook is
// bypassed; the validating webhook mirrors them (and enforces the cross-field
// rules CEL cannot express: External forbids spec.infrastructure and
// services.horizon, and Managed requires spec.infrastructure).
//
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || has(self.external)",message="services.keystone.external is required when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="(has(self.mode) && self.mode == 'External') || !has(self.external)",message="services.keystone.external may only be set when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.replicas)",message="services.keystone.replicas is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.image)",message="services.keystone.image is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.policyOverrides)",message="services.keystone.policyOverrides is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.rotationInterval)",message="services.keystone.rotationInterval is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.gateway)",message="services.keystone.gateway is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.publicEndpoint)",message="services.keystone.publicEndpoint is forbidden when services.keystone.mode is External"
// +kubebuilder:validation:XValidation:rule="!(has(self.mode) && self.mode == 'External') || !has(self.federationProxyImage)",message="services.keystone.federationProxyImage is forbidden when services.keystone.mode is External"
type ServiceKeystoneSpec struct {
	// Mode selects whether the Keystone service is Managed (the reconciler
	// deploys and owns a full Keystone workload, today's behavior) or External
	// (identity is managed against a pre-existing, externally-operated Keystone
	// reachable at external.authURL and no Keystone workload is deployed).
	// Defaults to Managed via both the CRD schema default and the defaulting
	// webhook. In External mode the typed external block is required and every
	// managed-only field below is forbidden (CEL + webhook enforced).
	// +kubebuilder:default=Managed
	// +optional
	Mode KeystoneMode `json:"mode,omitempty"`

	// External carries the connection parameters for an externally-operated
	// Keystone. Required when mode is External and forbidden otherwise (CEL +
	// webhook enforced).
	// +optional
	External *ExternalKeystoneSpec `json:"external,omitempty"`

	// Replicas overrides the number of Keystone API replicas. When nil the
	// reconciler applies the Keystone operator's own default (3).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Image optionally overrides the Keystone container image. When nil the
	// reconciler derives the image from spec.openStackRelease.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// PolicyOverrides defines per-service oslo.policy overrides for Keystone.
	// When set, these take precedence over spec.global for the Keystone service.
	// +optional
	PolicyOverrides *commonv1.PolicySpec `json:"policyOverrides,omitempty"`

	// RotationInterval optionally overrides the Fernet key rotation interval the
	// reconciler derives for the projected Keystone CR. When nil the reconciler
	// derives a default schedule.
	// +optional
	RotationInterval *metav1.Duration `json:"rotationInterval,omitempty"`

	// Gateway optionally exposes the projected Keystone API externally via a
	// Gateway API HTTPRoute. When nil (the default) the reconciler does NOT
	// project a gateway and the Keystone API is reachable in-cluster only (its
	// ClusterIP Service); when set, the reconciler projects this onto the Keystone
	// CR's spec.gateway so the keystone-operator attaches an HTTPRoute to the
	// referenced Gateway.
	//
	// this is the shared commonv1.GatewaySpec — the curated local copy
	// was consolidated into internal/common/types so both operators reuse one
	// source of truth.
	// +optional
	Gateway *commonv1.GatewaySpec `json:"gateway,omitempty"`

	// PublicEndpoint is the externally routable Keystone identity endpoint URL
	// (e.g. "https://keystone.127-0-0-1.nip.io:8443/v3"). The reconciler projects
	// it into the Keystone bootstrap (--bootstrap-public-url) and uses it for the
	// K-ORC identity catalog Endpoint, so external clients resolve the same URL
	// Keystone advertises. When empty and Gateway is set, the reconciler derives
	// "https://{gateway.hostname}/v3" (the default-443 form); set it explicitly
	// when the externally reachable port differs (e.g. a kind host-port mapping
	// like :8443), since the port cannot be derived from the hostname alone.
	// The pattern enforces an HTTP(S) URL shape so a malformed endpoint is
	// rejected at admission rather than wedging the projected Keystone CR (the
	// keystone webhook later rejects a non-URL publicEndpoint post-admission).
	// The 512-character bound mirrors the Horizon child's bound on
	// websso.keystoneURL, which the reconciler projects this value onto: a
	// longer value would be schema-legal here and rejected on the child.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https?://`
	PublicEndpoint string `json:"publicEndpoint,omitempty"`

	// FederationProxyImage optionally overrides the mod_auth_openidc sidecar
	// image the reconciler projects onto the Keystone child's
	// spec.federation.proxyImage. When nil the reconciler projects
	// "ghcr.io/c5c3/keystone-federation-proxy:latest".
	//
	// That default is a MUTABLE tag: every node re-pulls it on each pod start,
	// and there is no way to exercise a locally built sidecar. Override it with
	// a digest-carrying ImageSpec for the immutable pin published images are
	// expected to carry, or with a locally loaded tag to test a sidecar under
	// review. The image is inert until a federation-typed
	// KeystoneIdentityBackend attaches — only then does the keystone-operator
	// project the sidecar.
	//
	// Forbidden in External mode (CEL + webhook enforced): no Keystone workload
	// is deployed, so there is no sidecar to image.
	// +optional
	FederationProxyImage *commonv1.ImageSpec `json:"federationProxyImage,omitempty"`
}

// KeystoneMode selects whether the ControlPlane's Keystone service is deployed
// and owned by the operator (Managed) or backed by a pre-existing, externally-
// operated Keystone (External). It mirrors the managed-vs-brownfield split of
// the infrastructure specs at the service level.
// +kubebuilder:validation:Enum=Managed;External
type KeystoneMode string

const (
	// KeystoneModeManaged (the default) deploys and owns a full Keystone
	// workload — today's behavior, byte-identical.
	KeystoneModeManaged KeystoneMode = "Managed"
	// KeystoneModeExternal manages identity against a pre-existing, externally-
	// operated Keystone (external.authURL) and deploys no Keystone workload.
	KeystoneModeExternal KeystoneMode = "External"
)

// ExternalEndpointType selects which Keystone catalog interface the control
// plane authenticates against. It maps to the clouds.yaml `endpoint_type` key —
// deliberately named endpointType rather than interface because K-ORC drops
// gophercloud's Interface field and only honours endpoint_type; the authoritative
// note lives on buildAppCredCloudsYAML in the reconciler's korc_cloudsyaml.go.
// +kubebuilder:validation:Enum=public;internal;admin
type ExternalEndpointType string

const (
	// ExternalEndpointTypePublic is the default: the public catalog interface.
	ExternalEndpointTypePublic ExternalEndpointType = "public"
	// ExternalEndpointTypeInternal selects the internal catalog interface.
	ExternalEndpointTypeInternal ExternalEndpointType = "internal"
	// ExternalEndpointTypeAdmin selects the admin catalog interface.
	ExternalEndpointTypeAdmin ExternalEndpointType = "admin"
)

// ExternalKeystoneSpec declares how the control plane reaches a pre-existing,
// externally-operated Keystone in External mode. It mirrors the brownfield
// infrastructure shape at the identity level: the endpoint and, optionally, a
// private-CA bundle are supplied here, and the reconciler manages identity
// against that endpoint rather than deploying a Keystone workload.
type ExternalKeystoneSpec struct {
	// AuthURL is the identity endpoint of the external Keystone (e.g.
	// "https://keystone.example.com/v3"). Required in External mode. The pattern
	// enforces an HTTP(S) URL shape with a non-empty host so a malformed or
	// hostless endpoint is rejected at admission; the validating webhook mirrors
	// it with a full net/url parse as defense-in-depth. Neither gate is an SSRF
	// control — admission cannot resolve where the host points, so the reconciler
	// that dials this endpoint must still enforce network egress restrictions.
	//
	// maxLength bounds the ONE unbounded input the reconciler interpolates into
	// status.conditions[].message. The pattern is end-unanchored, so without a cap
	// a multi-kilobyte path is admissible and the assembled message can exceed the
	// apiserver's 32768-byte message cap — which fails the WHOLE status.conditions
	// write, so no condition persists and the reconciler spins in a backoff loop.
	// 2048 is the conventional practical URL ceiling and far above any real
	// identity endpoint. Callers that bypass both gates are caught by
	// truncateConditionMessage at every interpolation site.
	// +kubebuilder:validation:Pattern=`^https?://[^\s/]+`
	// +kubebuilder:validation:MaxLength=2048
	AuthURL string `json:"authURL"`

	// EndpointType selects which Keystone catalog interface to authenticate
	// against. Defaults to public via both the CRD schema default and the
	// defaulting webhook. It is rendered as the clouds.yaml `endpoint_type` key
	// in both generated credentials Secrets (see ExternalEndpointType). The
	// selected interface must exist in the external Keystone's service catalog
	// for spec.region, otherwise the control plane fails loud with
	// KORCReady=False/CatalogEndpointMismatch.
	// +kubebuilder:default=public
	// +optional
	EndpointType ExternalEndpointType `json:"endpointType,omitempty"`

	// CABundleSecretRef optionally references a Secret carrying a private CA
	// bundle the client trusts when verifying the external Keystone endpoint.
	// The referenced bundle is projected verbatim as the inline `cacert` key
	// into BOTH generated K-ORC credentials Secrets — K-ORC reads that key
	// natively from the same Secret that carries clouds.yaml, so no mount and no
	// upstream change are needed. Key defaults to "ca.crt"; the default is
	// webhook-only because the shared SecretRefSpec carries no c5c3-specific
	// marker (the same discipline as passwordSecretRef.Key).
	//
	// Rotating or removing the bundle converges the Secrets immediately, but
	// K-ORC's provider-client cache keys on the parsed cloud struct only —
	// `cacert` is not part of the key — so the new trust store only takes effect
	// once the cached client expires (~token lifetime / 2).
	// +optional
	CABundleSecretRef *commonv1.SecretRefSpec `json:"caBundleSecretRef,omitempty"`

	// Catalog tunes how the control plane stewards the external Keystone's
	// service catalog. It is optional and defaults to the conservative posture:
	// the identity service and all three of its endpoint interfaces are IMPORTED
	// as unmanaged K-ORC CRs and ZERO catalog entries are created.
	// +optional
	Catalog *ExternalCatalogSpec `json:"catalog,omitempty"`
}

// IdentityCatalogServiceType is the OpenStack service type of the Keystone
// catalog entry. It is the `type` filter of the External-mode identity Service
// import, and therefore the one entry type the managed-entry opt-in forbids —
// the single source of truth both the validating webhook and the reconciler
// reference so the rule and the import can never drift apart.
const IdentityCatalogServiceType = "identity"

// ExternalCatalogSpec tunes External-mode catalog stewardship. Both of its
// fields are optional, and the zero value is the conservative default: import
// the existing identity service (and its public/internal/admin endpoints),
// create nothing.
type ExternalCatalogSpec struct {
	// IdentityServiceName disambiguates the identity Service import when the
	// external catalog carries more than one `identity`-type service. When empty
	// the import filters on type alone; a filter matching zero entries surfaces
	// CatalogReady=False/ImportStalled, and a filter matching several surfaces
	// CatalogReady=False/CatalogFailed naming this field — the reconciler never
	// guesses and never imports all matches.
	//
	// Disambiguation is by NAME only, deliberately: K-ORC's ServiceImport.ID
	// carries a `Format:=uuid` marker (the RFC-4122 dashed form) while Keystone
	// mints service IDs as dashless `uuid4().hex`, so an ID-based import is
	// rejected by K-ORC's own CRD schema and cannot be offered here. A catalog
	// holding two identically NAMED identity services therefore cannot be
	// disambiguated from the spec; the condition says so and the external catalog
	// must be repaired.
	//
	// The pattern and the caps mirror K-ORC's own OpenStackName, which the name is
	// cast to on the Service import filter — exactly as managedEntries[].name is on
	// the child Service CR. A comma is not exotic input here (OpenStack list filters
	// are comma-separated, which is why OpenStackName forbids it), and admitting one
	// would only move the rejection to the K-ORC CRD, where it wedges the reconcile
	// in an exponential backoff no ControlPlane field error explains. The validating
	// webhook mirrors the pattern.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	IdentityServiceName string `json:"identityServiceName,omitempty"`

	// ManagedEntries is the EXPLICIT opt-in for creating genuinely new catalog
	// entries against the external Keystone. Absent (the default), External mode
	// creates zero catalog entries — creation is impossible to trigger by
	// accident. Each declared entry is projected as one managed K-ORC Service and
	// one managed Endpoint per declared interface; removing an entry deletes
	// exactly those resources and nothing else.
	//
	// The `identity` type is forbidden here: it is owned by the imports above.
	//
	// maxItems bounds the child-CR and external-API amplification of one admission:
	// every entry projects one managed K-ORC Service plus up to one managed Endpoint
	// per interface, and K-ORC turns each of those into a write against a
	// third-party production Keystone. Without a cap the only bound is the ~1.5 MiB
	// etcd object limit.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	ManagedEntries []ExternalCatalogEntrySpec `json:"managedEntries,omitempty"`
}

// ExternalCatalogEntrySpec declares one genuinely new catalog entry the control
// plane creates (and owns) in the external Keystone.
type ExternalCatalogEntrySpec struct {
	// Type is the OpenStack service type (e.g. "image", "compute"). It keys the
	// listType=map ManagedEntries list, so the apiserver rejects duplicates. It is
	// embedded verbatim in the names of the child K-ORC CRs, hence the DNS-1123
	// label shape. "identity" is rejected: the identity entry is import-owned in
	// External mode.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self != 'identity'",message="the identity catalog entry is owned by the External-mode imports and must not be declared as a managed entry"
	Type string `json:"type"`

	// Name optionally overrides the catalog service name. When empty K-ORC names
	// the service after the child CR.
	//
	// The pattern and the caps mirror K-ORC's own OpenStackName, which the name is
	// cast to on the child Service CR: a name admitted here can therefore never be
	// rejected downstream by the K-ORC CRD, which would wedge the reconcile in an
	// exponential backoff loop that no ControlPlane field error explains. The
	// validating webhook mirrors the pattern.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	Name string `json:"name,omitempty"`

	// Endpoints declares the endpoint rows registered for this entry, at most one
	// per interface (apiserver-enforced via listType=map). An entry with no
	// endpoints registers the service row alone.
	// +optional
	// +listType=map
	// +listMapKey=interface
	Endpoints []ExternalCatalogEndpointSpec `json:"endpoints,omitempty"`
}

// ExternalCatalogEndpointSpec declares one endpoint row of a managed catalog
// entry.
type ExternalCatalogEndpointSpec struct {
	// Interface is the catalog interface this endpoint is published under. It
	// keys the listType=map Endpoints list.
	Interface ExternalEndpointType `json:"interface"`

	// URL is the endpoint URL registered in the catalog. maxLength mirrors
	// K-ORC's own EndpointResourceSpec.URL cap, so a URL admitted here can never
	// be rejected downstream by the K-ORC CRD; the validating webhook mirrors both
	// the shape and the cap with a full net/url parse.
	// +kubebuilder:validation:Pattern=`^https?://[^\s/]+`
	// +kubebuilder:validation:MaxLength=1024
	URL string `json:"url"`
}

// ServiceHorizonSpec is a CURATED LOCAL subset of the knobs the ControlPlane
// exposes for the Horizon dashboard, mirroring the ServiceKeystoneSpec
// DECISION above: the reconciler (L2) PROJECTS this struct into a Horizon CR;
// the cache and the Keystone endpoint of that Horizon CR are DERIVED from the
// ControlPlane (infrastructure.cache and the Keystone child's naming
// convention) rather than set by the user here, and the L1 api package stays
// free of a dependency on the horizon module.
type ServiceHorizonSpec struct {
	// Replicas overrides the number of dashboard replicas. When nil the
	// reconciler applies the Horizon operator's own default (3).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Image optionally overrides the Horizon container image. When nil the
	// reconciler derives the image from spec.openStackRelease.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// Gateway optionally exposes the projected dashboard externally via a
	// Gateway API HTTPRoute. When nil (the default) the reconciler does NOT
	// project a gateway and the dashboard is reachable in-cluster only.
	// +optional
	Gateway *commonv1.GatewaySpec `json:"gateway,omitempty"`

	// SecretKeyRef optionally overrides the Secret holding the Django
	// SECRET_KEY the dashboard replicas share. When nil the reconciler defaults
	// to the kind-infrastructure shim Secret "horizon-secret-key" (key
	// "secret-key"), which is pinned to the default ControlPlane identity —
	// multi-ControlPlane deployments MUST set this field explicitly so each
	// dashboard reads its own key material.
	// +optional
	SecretKeyRef *commonv1.SecretRefSpec `json:"secretKeyRef,omitempty"`

	// PublicEndpoint is the BROWSER-observed dashboard base URL, without a
	// trailing slash and INCLUDING a non-default port
	// (e.g. "https://horizon.127-0-0-1.nip.io" or
	// "https://horizon.example.com:8443"). The reconciler derives the WebSSO
	// origin from it — publicEndpoint + "/auth/websso/" — and projects that
	// onto the Keystone child's spec.federation.trustedDashboards.
	//
	// Keystone matches the origin the dashboard sends VERBATIM, so the value
	// must reproduce exactly what the browser's address bar shows. When empty
	// and Gateway is set, the reconciler derives "https://{gateway.hostname}",
	// the default-443 form; any deployment publishing the dashboard on another
	// port MUST set this field explicitly, since the port cannot be derived
	// from the hostname alone and the WebSSO hand-off would be rejected.
	//
	// NOTE: Django derives the origin it sends from the request's Host header,
	// i.e. from gateway.hostname — not from this field. Setting a publicEndpoint
	// whose host differs from gateway.hostname therefore produces an origin
	// Keystone will reject, so whenever a gateway is configured the validating
	// webhook enforces that the two hostnames agree (and that the scheme is
	// https, since the Gateway listener terminates TLS).
	//
	// The 499-character bound is the Keystone child's 512-character bound on
	// spec.federation.trustedDashboards[] minus the 13 characters the derived
	// origin appends ("/auth/websso/"). Without it a schema-legal value here
	// would be rejected on the projected child, wedging the whole ControlPlane
	// behind an error naming a field the operator never wrote.
	//
	// This mirrors ServiceKeystoneSpec.PublicEndpoint. It needs no External-mode
	// forbid-rule: the validating webhook already forbids services.horizon
	// entirely when services.keystone.mode is External.
	// +optional
	// +kubebuilder:validation:MaxLength=499
	// +kubebuilder:validation:Pattern=`^https?://`
	PublicEndpoint string `json:"publicEndpoint,omitempty"`
}

// KORCSpec configures the K-ORC (OpenStack Resource Controller) integration of
// the control plane. It declares how the admin application credential
// is bootstrapped and rotated and which bootstrap resources are reconciled.
type KORCSpec struct {
	// AdminCredential declares the admin OpenStack credential K-ORC uses to
	// reconcile resources, plus the application-credential rotation policy.
	AdminCredential AdminCredentialSpec `json:"adminCredential"`

	// ServiceAccounts declares the composite OpenStack service accounts the
	// control plane manages for other OpenStack services (nova, glance, …). Each
	// entry projects one K-ORC User and one Project (role assignments follow as a
	// deferred fast-follow — see ServiceAccountSpec.Roles), with an
	// operator-generated password delivered to Keystone via K-ORC's passwordRef,
	// mirrored to a per-CR OpenBao path, and materialized as a stable consumer
	// Secret. The field is mode-independent: the same declaration works on a
	// Managed and an External ControlPlane.
	//
	// maxItems bounds the child-CR and external-API amplification of one
	// admission, mirroring the managedEntries cap: every entry projects a K-ORC
	// User and Project (each a write against Keystone) plus the OpenBao
	// round-trip.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=32
	ServiceAccounts []ServiceAccountSpec `json:"serviceAccounts,omitempty"`
}

// ServiceAccountSpec declares one composite OpenStack service account: a K-ORC
// User with an operator-generated, OpenBao-backed, rotatable password, its
// project (referenced or created), and the roles bound to it (deferred).
type ServiceAccountSpec struct {
	// Name keys the listType=map ServiceAccounts list (the apiserver rejects
	// duplicates) and is embedded verbatim in the names of every child CR and
	// Secret the entry projects, hence the DNS-1123 label shape. It is NOT the
	// OpenStack user name — that is userName, which defaults to this value.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// UserName is the OpenStack user name managed in Keystone. Defaults to Name
	// via the defaulting webhook. The pattern and caps mirror K-ORC's own
	// OpenStackName (a comma would only move the rejection to the K-ORC CRD, which
	// wedges the reconcile in an exponential backoff no ControlPlane field error
	// explains).
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	UserName string `json:"userName,omitempty"`

	// DomainName is the OpenStack domain the user and project live in. When empty
	// the reconciler resolves it to the effective admin domain
	// (spec.korc.adminCredential.domainName). The pattern and caps mirror K-ORC's
	// KeystoneName/OpenStackName filters.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	DomainName string `json:"domainName,omitempty"`

	// Adopt is the explicit consent that a pre-existing Keystone user of this
	// name may be taken over. The collision posture is conservative and
	// fail-loudly by default: a declared account whose user already exists in
	// Keystone surfaces ServiceAccountsReady=False/ServiceAccountCollision and is
	// never touched. Setting adopt=true opts into a PASSWORD TAKEOVER of that
	// account — the operator overwrites its password with a generated one — AND
	// into operator ownership of its lifecycle: an adopted user is a managed
	// K-ORC User, so it is DELETED from Keystone when the ControlPlane is torn
	// down, exactly like one the operator created. Adopt only what the control
	// plane should own.
	// +optional
	Adopt bool `json:"adopt,omitempty"`

	// Project is the OpenStack project the service user is associated with,
	// either referenced (the default) or created and owned by the control plane.
	Project ServiceAccountProjectSpec `json:"project"`

	// Roles are the OpenStack role names bound to the user on the project. They
	// are ACCEPTED but not yet projected: K-ORC v2.6.0 — the pinned and newest
	// release — ships no RoleAssignment kind, so role-assignment projection is a
	// sanctioned fast-follow once upstream releases it. The deferral is NOT
	// silent: when roles is non-empty the reconciler emits a
	// RoleAssignmentsDeferred event so an operator knows the bindings are not yet
	// applied. The field is carried now so the CRD schema is stable across levels.
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

// ServiceAccountProjectSpec declares the OpenStack project a service account is
// associated with.
type ServiceAccountProjectSpec struct {
	// Name is the OpenStack project name. The pattern and caps mirror K-ORC's
	// KeystoneName filter.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^,]+$`
	Name string `json:"name"`

	// Create selects whether the project is referenced or managed. false (the
	// default) REFERENCES a pre-existing project via an unmanaged K-ORC import —
	// the control plane never creates or deletes it. true CREATES and OWNS a
	// managed K-ORC Project, gated by the same fail-loudly collision probe as the
	// user: a project of that name already existing in Keystone surfaces
	// ServiceAccountCollision rather than silently adopting it.
	// +optional
	Create bool `json:"create,omitempty"`
}

// ServiceAccountRotationMode selects how a service account's password is rotated.
// It is deliberately NOT the admin RotationMode: there is no external password
// source, so PasswordDriven does not apply.
// +kubebuilder:validation:Enum=Manual;Scheduled
type ServiceAccountRotationMode string

const (
	// ServiceAccountRotationModeManual (the default) rotates the password only
	// when a CredentialRotation CR requests it.
	ServiceAccountRotationModeManual ServiceAccountRotationMode = "Manual"
	// ServiceAccountRotationModeScheduled rotates on a schedule. DECISION:
	// surfaced in the enum now so the CRD schema is stable, but the scheduled
	// rotation logic is deferred to a later level; the deferral is NOT silent
	// (the reconciler emits a ScheduledRotationDeferred event), mirroring
	// RotationModeScheduled on the admin credential.
	ServiceAccountRotationModeScheduled ServiceAccountRotationMode = "Scheduled"
)

// ServiceAccountRotationSpec declares the rotation policy for a service account's
// password.
type ServiceAccountRotationSpec struct {
	// Mode selects the rotation strategy. Defaults to Manual via both the CRD
	// schema default and the defaulting webhook.
	// +kubebuilder:validation:Enum=Manual;Scheduled
	// +kubebuilder:default=Manual
	// +optional
	Mode ServiceAccountRotationMode `json:"mode,omitempty"`
}

// AdminCredentialSpec declares the admin OpenStack credential and the
// application-credential rotation policy for the control plane.
type AdminCredentialSpec struct {
	// CloudCredentialsRef references the clouds.yaml Secret K-ORC reads the
	// admin cloud entry from.
	CloudCredentialsRef CloudCredentialsRef `json:"cloudCredentialsRef"`

	// PasswordSecretRef references the Secret holding the admin password used to
	// (re-)mint the application credential. Reuses the canonical commonv1 shape.
	PasswordSecretRef commonv1.SecretRefSpec `json:"passwordSecretRef"`

	// UserName is the OpenStack admin user name the control plane authenticates
	// as. Defaults to "admin" via both the CRD schema default and the defaulting
	// webhook. Valid in both Managed and External modes.
	//
	// It is rendered as the clouds.yaml `username` AND used as the K-ORC admin
	// User import filter the application credential's UserRef resolves to. Those
	// two MUST name the same user: Keystone's default policy only lets a token
	// mint an application credential for its OWN user. Editing this field on a
	// live ControlPlane updates the import filter in place, but K-ORC imports
	// resolve once — the stale resolved id surfaces as
	// KORCReady=False/CredentialDrift rather than silently repointing.
	// +kubebuilder:default=admin
	// +optional
	UserName string `json:"userName,omitempty"`

	// ProjectName is the OpenStack admin project name, rendered as the clouds.yaml
	// `project_name`. Defaults to "admin" via both the CRD schema default and the
	// defaulting webhook. Valid in both modes.
	// +kubebuilder:default=admin
	// +optional
	ProjectName string `json:"projectName,omitempty"`

	// DomainName is the OpenStack admin domain name. Defaults to "Default" via
	// both the CRD schema default and the defaulting webhook. Valid in both
	// modes. Phase-1 nuance: the single DomainName sets BOTH user_domain_name
	// and project_domain_name in the generated clouds.yaml, and is the K-ORC
	// admin Domain import filter, so the admin user and project must live in the
	// same domain; a later userDomainName/projectDomainName split is a
	// compatible extension.
	// +kubebuilder:default=Default
	// +optional
	DomainName string `json:"domainName,omitempty"`

	// ApplicationCredential declares the policy for the K-ORC admin application
	// credential (restriction, access rules, rotation mode).
	ApplicationCredential ApplicationCredentialSpec `json:"applicationCredential"`

	// BootstrapResources declares the OpenStack resources K-ORC bootstraps
	// alongside the admin credential (e.g. the projects/roles a fresh control
	// plane needs). The element shape is intentionally minimal at L1; the
	// reconciler (L2) interprets it.
	//
	// RESERVED, unreconciled: no controller reads this field today. For service
	// users of other OpenStack services, declare a composite service account via
	// spec.korc.serviceAccounts instead — it owns the full user + project +
	// password lifecycle. This field stays reserved for a later bootstrap use
	// case.
	// +optional
	BootstrapResources []BootstrapResourceSpec `json:"bootstrapResources,omitempty"`
}

// CloudCredentialsRef references the clouds.yaml Secret and the cloud entry
// within it that K-ORC authenticates as.
type CloudCredentialsRef struct {
	// CloudName is the entry in clouds.yaml K-ORC authenticates as.
	// DECISION defaults to "admin" via both the CRD schema default and
	// the defaulting webhook (for callers that bypass the CRD default), mirroring
	// the sibling SecretName field. The webhook is the load-bearing mechanism when
	// the whole korc block is omitted (the marker only fires when the parent
	// cloudCredentialsRef object is present), so cloudName is safe to drop from the
	// CRD's required list.
	// +kubebuilder:default="admin"
	// +optional
	CloudName string `json:"cloudName,omitempty"`

	// SecretName is the name of the Secret holding the clouds.yaml document.
	// DECISION defaults to "k-orc-clouds-yaml" via both the CRD schema
	// default and the defaulting webhook (for callers that bypass the CRD default),
	// mirroring the region defaulting discipline.
	// +kubebuilder:default="k-orc-clouds-yaml"
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// ApplicationCredentialSpec declares the K-ORC admin application-credential
// policy.
type ApplicationCredentialSpec struct {
	// Restricted controls whether the application credential is unrestricted
	// (able to create further application credentials) or restricted. Defaults
	// to true (the safe, least-privilege baseline) via both the CRD schema default
	// and the defaulting webhook.
	// +kubebuilder:default=true
	// +optional
	Restricted *bool `json:"restricted,omitempty"`

	// AccessRules optionally narrows the application credential to a specific set
	// of service/method/path rules. When empty the credential is not constrained
	// by access rules.
	// +optional
	AccessRules []AccessRule `json:"accessRules,omitempty"`

	// Rotation declares how the application credential is rotated.
	Rotation RotationSpec `json:"rotation"`
}

// AccessRule narrows an application credential to a specific service endpoint
// and method, mirroring the Keystone application-credential access
// rule shape (service / method / path).
type AccessRule struct {
	// Service is the OpenStack service type the rule applies to (e.g. "compute").
	Service string `json:"service"`

	// Method is the HTTP method the rule allows (e.g. "GET", "POST"). Optional:
	// projectAccessRules omits it from the projected K-ORC rule when empty. The
	// enum mirrors K-ORC's HTTPMethod type (the value is cast to it), so a value
	// the downstream ApplicationCredentialAccessRule would reject is caught at
	// admission instead.
	// +optional
	// +kubebuilder:validation:Enum=CONNECT;DELETE;GET;HEAD;OPTIONS;PATCH;POST;PUT;TRACE
	Method string `json:"method,omitempty"`

	// Path is the request path the rule allows (e.g. "/v2.1/servers"). Optional:
	// projectAccessRules omits it when empty. When set it must be an absolute
	// path (leading slash).
	// +optional
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path,omitempty"`
}

// RotationMode selects how the K-ORC admin application credential is rotated
// +kubebuilder:validation:Enum=PasswordDriven;Scheduled;Manual
type RotationMode string

const (
	// RotationModePasswordDriven re-mints the application credential whenever the
	// underlying admin password changes. This is the default.
	RotationModePasswordDriven RotationMode = "PasswordDriven"
	// RotationModeScheduled rotates the application credential on a schedule.
	// DECISION surfaced in the enum now so the CRD schema is stable,
	// but the scheduled rotation logic is deferred to a later level.
	RotationModeScheduled RotationMode = "Scheduled"
	// RotationModeManual rotates only when a CredentialRotation CR requests it.
	RotationModeManual RotationMode = "Manual"
)

// RotationSpec declares the rotation policy for the admin application
// credential.
type RotationSpec struct {
	// Mode selects the rotation strategy. Defaults to PasswordDriven via both the
	// CRD schema default and the defaulting webhook.
	// +kubebuilder:default=PasswordDriven
	// +optional
	Mode RotationMode `json:"mode,omitempty"`
}

// BootstrapResourceSpec declares an OpenStack resource K-ORC bootstraps with
// the control plane. The shape is intentionally minimal at L1 — the
// reconciler (L2) interprets the kind/name and applies it.
type BootstrapResourceSpec struct {
	// Kind is the K-ORC resource kind to bootstrap. Constrained to the kinds the
	// control plane bootstraps today; widen the enum when the L2 reconciler
	// learns to interpret additional kinds.
	// +kubebuilder:validation:Enum=Project;Role
	Kind string `json:"kind"`

	// Name is the name of the bootstrapped resource.
	Name string `json:"name"`
}

// UpdatePhase represents the current phase of a control-plane update.
//
// DECISION the enum surfaces the FUTURE phases (UpdatingServices,
// Verifying, RollingBack) alongside the active ones so the CRD schema is stable
// across levels and does not need a breaking change when the update state
// machine is implemented. The phases marked "not yet implemented" below are
// reserved values that the L1 reconciler never sets; they are documented here
// so consumers (dashboards, kubectl) see the full vocabulary.
// +kubebuilder:validation:Enum=Idle;Updating;UpdatingServices;Verifying;RollingBack
type UpdatePhase string

const (
	// UpdatePhaseIdle indicates no update is in progress.
	UpdatePhaseIdle UpdatePhase = "Idle"
	// UpdatePhaseUpdating indicates a release update has started.
	UpdatePhaseUpdating UpdatePhase = "Updating"
	// UpdatePhaseUpdatingServices indicates per-service CRs are being updated.
	// DECISION reserved; not yet implemented.
	UpdatePhaseUpdatingServices UpdatePhase = "UpdatingServices"
	// UpdatePhaseVerifying indicates the control plane is verifying an update.
	// DECISION reserved; not yet implemented.
	UpdatePhaseVerifying UpdatePhase = "Verifying"
	// UpdatePhaseRollingBack indicates a failed update is being rolled back.
	// DECISION reserved; not yet implemented.
	UpdatePhaseRollingBack UpdatePhase = "RollingBack"
)

// ControlPlaneStatus defines the observed state of a ControlPlane.
type ControlPlaneStatus struct {
	// Conditions represent the latest available observations of the control
	// plane state. Each condition carries an ObservedGeneration so consumers can
	// tell a stale condition from one reflecting the current spec; use the
	// conditions helper (internal/common/conditions) to upsert them.
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

	// UpdatePhase is the current phase of a control-plane release update.
	// +optional
	UpdatePhase UpdatePhase `json:"updatePhase,omitempty"`

	// Services reports the per-service readiness of the projected service CRs,
	// as a list keyed by service name (e.g. "keystone"). A listType=map list so
	// per-service entries merge under server-side apply and can grow
	// per-service conditions cleanly.
	// +optional
	// +listType=map
	// +listMapKey=name
	Services []ServiceStatus `json:"services,omitempty"`

	// AdminApplicationCredential reports the observed state of the K-ORC admin
	// application credential.
	// +optional
	AdminApplicationCredential *AdminApplicationCredentialStatus `json:"adminApplicationCredential,omitempty"`

	// Catalog reports the observed state of the External-mode catalog imports. It
	// is nil in Managed mode, where the control plane creates the catalog entries
	// rather than importing them.
	// +optional
	Catalog *CatalogStatus `json:"catalog,omitempty"`

	// ServiceAccounts reports the observed state of the declared service
	// accounts, keyed by name. It is the discoverability half of the consumption
	// contract: SecretName names the materialized Secret each account's password
	// is read from.
	// +optional
	// +listType=map
	// +listMapKey=name
	ServiceAccounts []ServiceAccountStatus `json:"serviceAccounts,omitempty"`
}

// ServiceAccountStatus reports the observed state of one declared service
// account.
type ServiceAccountStatus struct {
	// Name is the service account name; it keys the listType=map ServiceAccounts
	// list.
	Name string `json:"name"`

	// Ready reports whether the user, project, and materialized password Secret
	// are all converged for the current password generation.
	Ready bool `json:"ready"`

	// UserID is the OpenStack user id K-ORC resolved (or created). Empty until the
	// User is Available.
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

	// SecretName is the name of the materialized Secret carrying the account's
	// credentials (key "password" and a ready-to-use "clouds.yaml"). It is the
	// documented, stable handle consumers read the credentials from.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// CatalogStatus reports how the External-mode identity catalog imports resolved.
// It is the operator-visible answer to "did the ControlPlane find the catalog it
// was pointed at?" — the aggregate CatalogReady condition says whether they all
// resolved, this list says which ones did.
type CatalogStatus struct {
	// Imports lists the unmanaged K-ORC CRs importing the external identity
	// service and its endpoint interfaces, keyed by CR name.
	// +optional
	// +listType=map
	// +listMapKey=name
	Imports []CatalogImportStatus `json:"imports,omitempty"`
}

// CatalogImportStatus reports the observed state of a single unmanaged catalog
// import.
type CatalogImportStatus struct {
	// Name is the K-ORC CR name; it keys the listType=map Imports list.
	Name string `json:"name"`

	// Kind is the imported K-ORC kind, "Service" or "Endpoint".
	// +kubebuilder:validation:Enum=Service;Endpoint
	Kind string `json:"kind"`

	// Interface is the catalog interface of an imported Endpoint; empty for the
	// Service import.
	// +optional
	Interface ExternalEndpointType `json:"interface,omitempty"`

	// Resolved reports whether K-ORC has matched this import against a live
	// catalog entry (its Available condition is True for the CR's current
	// generation).
	Resolved bool `json:"resolved"`

	// ID is the OpenStack id K-ORC resolved the import to. Empty while the import
	// is unresolved.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	ID string `json:"id,omitempty"`
}

// ServiceStatus reports the observed readiness of a single projected service
// CR.
type ServiceStatus struct {
	// Name is the service name (e.g. "keystone"); it keys the listType=map
	// Services list.
	Name string `json:"name"`

	// Ready reports whether the projected service CR is Ready.
	Ready bool `json:"ready"`

	// Release is the OpenStack release the service currently reports installed.
	// +optional
	Release string `json:"release,omitempty"`
}

// AdminApplicationCredentialStatus reports the observed state of the K-ORC
// admin application credential.
type AdminApplicationCredentialStatus struct {
	// ID is the OpenStack application-credential ID currently in use.
	// +optional
	ID string `json:"id,omitempty"`

	// Restricted reports whether the active credential is restricted.
	// +optional
	Restricted bool `json:"restricted,omitempty"`

	// LastRotation is the timestamp of the last successful rotation.
	// +optional
	LastRotation *metav1.Time `json:"lastRotation,omitempty"`
}

// IsExternalKeystone reports whether the ControlPlane's Keystone service is in
// External mode: services.keystone is set and its mode is External. It is the
// single, nil-safe discriminator read shared by the webhook (transition gating)
// and the reconciler, so no call site re-implements the mode check. A nil
// services.keystone (no Keystone at all) is not External.
func (cp *ControlPlane) IsExternalKeystone() bool {
	ks := cp.Spec.Services.Keystone
	return ks != nil && ks.Mode == KeystoneModeExternal
}

func init() {
	SchemeBuilder.Register(&ControlPlane{}, &ControlPlaneList{})
}
