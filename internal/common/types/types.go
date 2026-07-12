// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package types

import (
	corev1 "k8s.io/api/core/v1"
)

// DefaultCacheBackend is the oslo.cache backend materialized when a Spec leaves
// CacheSpec.Backend empty. It lives here as the single source of truth shared by
// the keystone and c5c3 defaulting webhooks so the default cannot drift across
// operators (kubebuilder markers cannot reference Go constants, so leaf markers
// keep the literal in sync separately).
const DefaultCacheBackend = "dogpile.cache.pymemcache"

// DatabaseStorageSizeDefault is the per-replica managed-MariaDB volume size
// materialized when DatabaseSpec.StorageSize is left empty. It is the single Go
// source of truth shared by the c5c3 fresh-create projection (the fallback in
// reconcile_infrastructure.go) and the ControlPlane validating webhook (the
// one-time migration normalization for pre-existing CRs), so the two cannot
// drift. Keep it in lockstep with the +kubebuilder:default marker on
// DatabaseSpec.StorageSize below — kubebuilder markers cannot reference Go
// constants, so the leaf marker keeps the literal in sync separately.
const DatabaseStorageSizeDefault = "100Gi"

// Database credential modes select how the service DB credential referenced by
// DatabaseSpec.SecretRef is provisioned and consumed. They are shared by the
// keystone and c5c3 defaulting/validation webhooks so the contract cannot drift
// across operators (kubebuilder markers keep the enum literals in sync
// separately, since markers cannot reference Go constants).
const (
	// CredentialsModeStatic is the default: the operator provisions the MariaDB
	// User/Grant CRs and the DB password is a long-lived value carried in
	// SecretRef. It is the backwards-compatible mode and the explicit opt-out
	// used during (and after) migration to the dynamic engine.
	CredentialsModeStatic = "Static"
	// CredentialsModeDynamic issues short-lived credentials from an external
	// secrets engine (OpenBao database engine). SecretRef then carries both a
	// username and a password materialised on demand, and the operator does not
	// manage MariaDB User/Grant CRs — the engine owns the DB user lifecycle.
	// Only valid in managed mode (ClusterRef set).
	CredentialsModeDynamic = "Dynamic"
)

// ImageSpec defines a container image reference. Exactly one of Tag or Digest
// must be set (enforced by the type-level XValidation rule below), so a
// supply-chain-sensitive deployment can pin the image by immutable digest while
// the common case keeps a human-readable tag.
//
// Digest-mode (Tag empty, Digest set) disables release
// tracking/upgrades, which key on the tag; the managed ControlPlane path always
// projects a tag, so it is unaffected.
//
// +kubebuilder:validation:XValidation:rule="has(self.tag) != has(self.digest)",message="exactly one of image.tag or image.digest must be set"
type ImageSpec struct {
	// Repository is the OCI image repository, optionally including the registry
	// host (e.g. "ghcr.io/c5c3/<service>" or "c5c3/<service>"). The pattern is a
	// permissive OCI reference — lowercase alphanumeric components separated by
	// ".", "_", "-", "/", or ":" (registry port) — so mirror and host:port forms
	// are accepted while an empty string (which "required" alone admits) and
	// obvious garbage are rejected.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._:/-][a-z0-9]+)*$`
	Repository string `json:"repository"`
	// Tag is the OCI image tag (e.g. "2025.2"). It follows the OCI tag grammar:
	// up to 128 characters of word characters, dots, and dashes, and must not
	// begin with a dot or dash. Optional: exactly one of Tag or Digest must be
	// set. The pattern is enforced only when a tag is present.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`
	Tag string `json:"tag,omitempty"`
	// Digest pins the image by immutable content digest (e.g.
	// "sha256:<64 hex chars>"). Optional: exactly one of Tag or Digest must be
	// set. A pinned digest closes the supply-chain gap where a mutable tag can be
	// re-pushed behind a stable name, at the cost of disabling release tracking.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest,omitempty"`
}

// Reference returns the fully-qualified image reference the workloads consume:
// "repository@digest" when a digest is pinned, otherwise "repository:tag".
func (i ImageSpec) Reference() string {
	if i.Digest != "" {
		return i.Repository + "@" + i.Digest
	}
	return i.Repository + ":" + i.Tag
}

// DatabaseSpec supports managed (ClusterRef) and brownfield (explicit) modes.
// Exactly one of ClusterRef or Host must be set; the first XValidation rule
// below enforces that invariant at the schema layer for every operator that
// embeds a DatabaseSpec, so it holds even when a validating webhook is
// bypassed. The second rule enforces that CredentialsMode Dynamic (engine-issued
// credentials) is only valid in managed mode, since the dynamic engine issues
// per-tenant DB users against a cluster the operator provisions.
//
// +kubebuilder:validation:XValidation:rule="has(self.clusterRef) != has(self.host)",message="exactly one of clusterRef or host must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.credentialsMode) || self.credentialsMode != 'Dynamic' || has(self.clusterRef)",message="credentialsMode Dynamic requires clusterRef (managed mode)"
type DatabaseSpec struct {
	// ClusterRef references a MariaDB CR in the cluster (managed mode).
	// +optional
	ClusterRef *corev1.LocalObjectReference `json:"clusterRef,omitempty"`
	// CredentialsMode selects how the service DB credential in SecretRef is
	// provisioned. "Static" (the default when empty) has the operator manage
	// the MariaDB User/Grant CRs with a long-lived password from SecretRef.
	// "Dynamic" has an external secrets engine issue short-lived credentials on
	// demand: SecretRef then carries both a username and password, no User/Grant
	// CRs are managed, and no long-lived DB password remains at rest. Dynamic is
	// only valid in managed mode (ClusterRef set).
	// +optional
	// +kubebuilder:validation:Enum=Static;Dynamic
	CredentialsMode string `json:"credentialsMode,omitempty"`
	// Host is the database hostname (brownfield mode). The pattern is a
	// permissive host matcher that accepts DNS names, IPv4, and IPv6 literals
	// while rejecting empty strings and shell/path metacharacters; it is not a
	// strict RFC-1123 validator, deliberately, so airgapped/mirror endpoints are
	// not rejected at admission.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._:-]+$`
	Host string `json:"host,omitempty"`
	// Port is the database port (brownfield mode, default 3306). Omitted (managed
	// mode) leaves it unset; an explicit value must be a valid TCP port.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
	// Database is the database name within the cluster. Constrained to the MySQL
	// identifier character set and the 64-character identifier limit.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_]+$`
	Database string `json:"database"`
	// SecretRef references the K8s Secret with credentials.
	SecretRef SecretRefSpec `json:"secretRef"`
	// TLS optionally enables TLS/mTLS for the database connection. The pointer keeps the field opt-in and non-mutating: a nil
	// TLS means plaintext TCP, preserving the pre-existing behavior for all
	// existing DatabaseSpec consumers.
	// +optional
	TLS *DatabaseTLSSpec `json:"tls,omitempty"`
	// Replicas is the number of managed MariaDB replicas provisioned in
	// fresh-create mode. It mirrors CacheSpec.Replicas: only the c5c3 operator's
	// managed-mode projection honours it (a single replica yields a
	// single-instance non-Galera MariaDB, three or more yield a Galera cluster),
	// so a constrained cluster such as a single-node kind can schedule the
	// fresh-create path. Operators that adopt an existing MariaDB (the c5c3 adopted-infra path,
	// and every service operator) ignore it. Only meaningful when ClusterRef is
	// set.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`
	// StorageSize is the persistent-volume size requested for each managed
	// MariaDB replica in fresh-create mode. Like Replicas, only the c5c3
	// operator's managed-mode projection honours it (it is written to the owned
	// MariaDB's spec.storage.size); operators that adopt an existing MariaDB
	// (the c5c3 adopted-infra path, and every service operator) ignore it. The default 100Gi
	// mirrors the production baseline (deploy/flux-system/infrastructure/
	// mariadb.yaml); a constrained cluster such as a single-node kind can pin a
	// far smaller value (e.g. 512Mi) so CI does not request a 100Gi volume it
	// never fills. Immutable after creation: the mariadb-operator rejects
	// changing spec.storage.size on a live CR, so the ControlPlane validating
	// webhook freezes it too. Only meaningful when ClusterRef is set. The pattern
	// admits binary IEC units (Mi/Gi/Ti) matching the Kubernetes quantity grammar
	// the operator parses with resource.ParseQuantity.
	// +optional
	// +kubebuilder:default="100Gi"
	// +kubebuilder:validation:Pattern=`^[0-9]+(Mi|Gi|Ti)$`
	StorageSize string `json:"storageSize,omitempty"`
}

// DatabaseTLSSpec configures opt-in TLS (and mutual TLS) for a database
// connection. It is referenced as an optional pointer from
// DatabaseSpec so the canonical shape can be reused by sibling operators.
//
// The single Mode enum is the on/off discriminator: a present tls block means
// "on" (the defaulting webhook materializes an empty mode to "require"), and TLS
// is enabled exactly when mode is neither empty nor "disabled". The "disabled"
// value lets an operator keep the certificate references while turning
// verification off, without deleting the block.
type DatabaseTLSSpec struct {
	// Mode selects the TLS verification strength applied to the connection:
	//   - disabled:       TLS is off; certificate references are ignored.
	//   - prefer/require: encrypt the connection only (no peer verification).
	//   - verify-ca:      additionally verify the server certificate chain
	//                      against the trusted CA bundle.
	//   - verify-full:    additionally verify the server certificate chain
	//                      and that the server hostname matches the
	//                      certificate identity.
	// +kubebuilder:validation:Enum=disabled;prefer;require;verify-ca;verify-full
	// +optional
	Mode string `json:"mode,omitempty"`
	// CABundleSecretRef references the K8s Secret holding the server CA
	// bundle the client trusts when verifying the database endpoint.
	CABundleSecretRef SecretRefSpec `json:"caBundleSecretRef"`
	// ClientCertSecretRef references the K8s Secret holding the client
	// keypair presented to the database for mutual TLS.
	ClientCertSecretRef SecretRefSpec `json:"clientCertSecretRef"`
}

// IsEnabled reports whether the database TLS block requests an encrypted
// connection. A nil receiver (no tls block) is disabled; a present block is
// enabled unless its mode is empty or "disabled". The defaulting webhook
// materializes an empty mode to "require", so an enabled block reaching the
// reconciler always carries a concrete mode.
func (t *DatabaseTLSSpec) IsEnabled() bool {
	return t != nil && t.Mode != "" && t.Mode != "disabled"
}

// CacheSpec supports managed (ClusterRef) and brownfield (explicit) modes.
// Exactly one of ClusterRef or Servers must be set; the XValidation rule below
// enforces that invariant at the schema layer for every operator that embeds a
// CacheSpec, so it holds even when a validating webhook is bypassed.
//
// +kubebuilder:validation:XValidation:rule="has(self.clusterRef) != (has(self.servers) && size(self.servers) > 0)",message="exactly one of clusterRef or servers must be set"
type CacheSpec struct {
	// ClusterRef references a Memcached CR in the cluster (managed mode).
	// +optional
	ClusterRef *corev1.LocalObjectReference `json:"clusterRef,omitempty"`
	// Backend is the cache backend (e.g. dogpile.cache.pymemcache).
	Backend string `json:"backend"`
	// Servers is the list of cache server endpoints (brownfield mode).
	// +optional
	Servers []string `json:"servers,omitempty"`
	// Replicas is the number of Memcached pod replicas in the referenced cluster
	// (managed mode). Used to generate the correct number of StatefulSet pod
	// endpoints. Only used when ClusterRef is set.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`
}

// SecretRefSpec references a Kubernetes Secret.
type SecretRefSpec struct {
	// Name is the referenced Secret's name. It must be a non-empty DNS-1123
	// subdomain (the Kubernetes object-name grammar). Tightening the shared type
	// fixes every consumer at once — a service operator adminPasswordSecretRef /
	// database.secretRef / messaging / TLS cert refs and the c5c3
	// passwordSecretRef — so an empty name no longer slips through "required".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
}

// SecretStoreRefKind names the External Secrets store scope a
// SecretStoreRefSpec selects: the cluster-scoped ClusterSecretStore or the
// namespaced SecretStore.
// +kubebuilder:validation:Enum=ClusterSecretStore;SecretStore
type SecretStoreRefKind string

const (
	// SecretStoreKindCluster selects a cluster-scoped ClusterSecretStore,
	// resolved by name alone. This is today's shared default.
	SecretStoreKindCluster SecretStoreRefKind = "ClusterSecretStore"
	// SecretStoreKindNamespaced selects a namespaced SecretStore, resolved in
	// the consuming CR's own namespace — the per-tenant identity path.
	SecretStoreKindNamespaced SecretStoreRefKind = "SecretStore"
)

// SecretStoreRefSpec selects the External Secrets store the operators route a
// CR's ExternalSecrets and PushSecrets through. It is the per-CR replacement
// for the compile-time openbao-cluster-store constant: when omitted the
// operators default to the shared cluster-scoped store, so existing
// deployments keep working unchanged; when set to a namespaced SecretStore the
// CR reaches OpenBao as its own tenant identity.
//
// A namespaced store (Kind SecretStore) is always resolved in the consuming
// CR's own namespace — there is deliberately no namespace field, matching ESO
// SecretStoreRef semantics where a SecretStore is looked up locally.
type SecretStoreRefSpec struct {
	// Kind is the store scope: ClusterSecretStore (cluster-scoped, the default)
	// or SecretStore (resolved in the CR's namespace).
	// +optional
	// +kubebuilder:default=ClusterSecretStore
	Kind SecretStoreRefKind `json:"kind,omitempty"`
	// Name is the referenced store's name. It must be a non-empty DNS-1123
	// subdomain (the Kubernetes object-name grammar), matching SecretRefSpec.Name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// PolicySpec defines oslo.policy override configuration for an OpenStack service.
// The two XValidation rules below reject empty rule names and empty rule values
// at the schema layer, mirroring policy.ValidatePolicyRules in the admission
// webhooks so the invariant still holds when a webhook is bypassed.
// The empty checks use size() rather than a string literal so the marker
// carries no embedded quotes.
//
// +kubebuilder:validation:XValidation:rule="!has(self.rules) || self.rules.all(k, size(k) > 0)",message="policy rule name must not be empty"
// +kubebuilder:validation:XValidation:rule="!has(self.rules) || self.rules.all(k, size(self.rules[k]) > 0)",message="policy rule value must not be empty"
type PolicySpec struct {
	// Rules contains inline policy rule overrides.
	// Keys are oslo.policy rule names (e.g., "compute:create").
	// Values are oslo.policy rule definitions (e.g., "role:admin").
	// Inline rules take precedence over ConfigMap rules.
	// +optional
	Rules map[string]string `json:"rules,omitempty"`
	// ConfigMapRef references a user-provided ConfigMap containing a
	// "policy.yaml" key with rule overrides.
	// +optional
	ConfigMapRef *corev1.LocalObjectReference `json:"configMapRef,omitempty"`
}

// PluginSpec defines a service plugin/driver configuration.
type PluginSpec struct {
	// Name of the plugin (e.g., "myservice-keycloak-backend")
	Name string `json:"name"`
	// ConfigSection is the INI section name (e.g., "keycloak")
	ConfigSection string `json:"configSection"`
	// Config contains key-value pairs for the plugin's INI section
	Config map[string]string `json:"config,omitempty"`
}

// PipelinePosition defines where middleware is inserted in api-paste.ini.
// +kubebuilder:validation:Enum=before;after
type PipelinePosition string

const (
	// PipelinePositionBefore inserts middleware before the base filters in the pipeline.
	PipelinePositionBefore PipelinePosition = "before"
	// PipelinePositionAfter inserts middleware after the base filters but before
	// the terminal application in the pipeline.
	PipelinePositionAfter PipelinePosition = "after"
)

// MiddlewareSpec defines a WSGI middleware filter for api-paste.ini.
type MiddlewareSpec struct {
	// Name of the filter (e.g., "audit")
	Name string `json:"name"`
	// FilterFactory is the Python entry point (e.g., "audit_middleware:filter_factory")
	FilterFactory string `json:"filterFactory"`
	// Position defines where in the pipeline this filter is inserted
	Position PipelinePosition `json:"position"`
	// Config contains key-value pairs for the filter section
	Config map[string]string `json:"config,omitempty"`
}

// GatewaySpec configures the Gateway API HTTPRoute used to expose an OpenStack
// service externally. It is the single source of truth for the shared Gateway
// shape: the service operators and the c5c3 operator reuse this commonv1 type instead
// of maintaining their own field-for-field copies.
//
// The operator plays the application-developer role in the Gateway API model:
// it only manages the HTTPRoute. The referenced Gateway (and GatewayClass) must
// be pre-provisioned by the platform team.
type GatewaySpec struct {
	// ParentRef identifies the pre-existing Gateway that the HTTPRoute attaches
	// to.
	ParentRef GatewayParentRefSpec `json:"parentRef"`

	// Hostname is the externally reachable host (SNI / Host header) that the
	// HTTPRoute matches. Required.
	// +kubebuilder:validation:MinLength=1
	Hostname string `json:"hostname"`

	// Path is the URL path prefix matched by the HTTPRoute. Defaults to "/" when
	// empty.
	// +optional
	Path string `json:"path,omitempty"`

	// Annotations are passed through to the generated HTTPRoute metadata
	// verbatim, allowing implementation-specific configuration (rate limits,
	// timeouts, CORS) without extending the CRD.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// GatewayParentRefSpec references a pre-existing Gateway that the operator
// attaches the HTTPRoute to. It is shared across the service operators and the
// c5c3 operator as the single source of truth for the Gateway parent reference.
type GatewayParentRefSpec struct {
	// Name is the Gateway resource name. Required.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the namespace of the referenced Gateway. When empty, the
	// Gateway is assumed to live in the referencing CR's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// SectionName targets a specific listener on the Gateway (e.g. "https") when
	// the Gateway defines multiple listeners. When empty, the HTTPRoute attaches
	// to all compatible listeners.
	// +optional
	SectionName string `json:"sectionName,omitempty"`
}
