// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Fixed data-key contract for the Secrets an OpenBao store references. The
// Secrets referenced by OpenBaoServerSpec MUST carry exactly these keys; the
// key names are pinned by contract (not configurable per CR) and match the
// AppRole credentials the operator mints in managed mode.
const (
	// OpenBaoRoleIDKey is the credentials Secret data key holding the AppRole
	// role ID (rendered into the vault plugin's approle_role_id).
	OpenBaoRoleIDKey = "role-id"
	// OpenBaoSecretIDKey is the credentials Secret data key holding the AppRole
	// secret ID (rendered into the vault plugin's approle_secret_id).
	OpenBaoSecretIDKey = "secret-id"
	// OpenBaoCAKey is the CA bundle Secret data key holding the PEM certificate
	// that authenticates the OpenBao server. The operator mounts the bundle into
	// the API pods and points the vault plugin's ssl_ca_crt_file at the mount
	// path.
	OpenBaoCAKey = "ca.crt"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Default",type="boolean",JSONPath=".spec.isDefault"
// +kubebuilder:printcolumn:name="Barbican",type="string",JSONPath=".spec.barbicanRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// BarbicanSecretStore is the Schema for the barbicansecretstores API. One CR
// attaches to a Barbican CR via spec.barbicanRef and describes one secret-store
// backend (Phase 1: OpenBao). A dedicated controller owns the store lifecycle —
// finalizer, credential resolution, per-store conditions — while the
// barbican-side sub-reconciler aggregates all attached, credential-ready stores
// into the rendered barbican.conf secret-store sections, promoting the one
// marked isDefault to the global default store.
//
// The metadata.name is part of the rendered configuration: it becomes the
// suffix of the [secretstore:<name>] section barbican reads, so the validating
// webhook rejects reserved names that would collide with the sections the
// operator owns. The name is also bounded, because the operator derives the
// AppRole credentials Secret by appending "-approle" to it, which caps the
// usable name at 55 characters. Both limits live in the webhook, not in this
// schema.
type BarbicanSecretStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BarbicanSecretStoreSpec   `json:"spec,omitempty"`
	Status BarbicanSecretStoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BarbicanSecretStoreList contains a list of BarbicanSecretStore.
type BarbicanSecretStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BarbicanSecretStore `json:"items"`
}

// BarbicanSecretStoreType enumerates the supported secret-store plugins. Phase 1
// ships OpenBao, driven by barbican's vault plugin. A brownfield HashiCorp Vault
// server is attached under this same type: the plugin talks the KV v2 API, which
// OpenBao and Vault both serve, so the two are interchangeable from barbican's
// side and do not warrant a second enum value.
// +kubebuilder:validation:Enum=OpenBao
type BarbicanSecretStoreType string

const (
	// BarbicanSecretStoreTypeOpenBao selects barbican's vault secret-store
	// plugin, backed by an OpenBao (or API-compatible HashiCorp Vault) server.
	BarbicanSecretStoreTypeOpenBao BarbicanSecretStoreType = "OpenBao"
)

// BarbicanSecretStoreSpec defines the desired state of BarbicanSecretStore.
//
// The barbicanRef transition rule (evaluated only on UPDATE) makes the
// attachment immutable: re-pointing a store at a different Barbican would leave
// the old deployment with a store nothing manages anymore and race the config
// projection on the new one. Delete and recreate instead. The type is immutable
// for the same reason plus a harder one: the secret material already written
// through a store cannot be read back through a different plugin. The
// type/openBao union rule enforces "exactly one store block matching spec.type"
// at the schema layer so it holds even when the validating webhook is down.
// +kubebuilder:validation:XValidation:rule="self.barbicanRef.name == oldSelf.barbicanRef.name",message="barbicanRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="type is immutable"
// +kubebuilder:validation:XValidation:rule="(self.type == 'OpenBao') == has(self.openBao)",message="exactly one store block matching spec.type must be set (type OpenBao requires spec.openBao)"
type BarbicanSecretStoreSpec struct {
	// BarbicanRef names the Barbican CR in the same namespace this store attaches
	// to. The referenced CR does not have to exist at admission time (GitOps
	// ordering: the store may be applied before the Barbican CR); a dangling
	// reference surfaces as Ready=False.
	BarbicanRef BarbicanRefSpec `json:"barbicanRef"`

	// Type selects the secret-store plugin. Phase 1 supports OpenBao only.
	Type BarbicanSecretStoreType `json:"type"`

	// OpenBao configures the OpenBao-backed store. Required exactly when type is
	// OpenBao (union rule above).
	// +optional
	OpenBao *OpenBaoStoreSpec `json:"openBao,omitempty"`

	// IsDefault marks this store as the Barbican global default secret store.
	// Mutable: exactly one attached, ready store must be the default, and
	// flipping it re-renders the parent Barbican's config
	// (global_default). The barbican-side sub-reconciler and the validating
	// webhook enforce the single-default invariant.
	// +optional
	IsDefault bool `json:"isDefault,omitempty"`

	// ExtraOptions provides free-form [vault_plugin] options not covered by the
	// typed fields, keyed by bare option name. The entries merge into the
	// process-global [vault_plugin] section, which every OpenBao store on the
	// same Barbican shares. The validating webhook applies an allowlist of
	// options that may be set at all, a denylist of the options the operator
	// owns through the typed fields, and a control-character guard, so the escape
	// hatch cannot silently contradict the typed spec or inject INI syntax.
	//
	// MaxProperties and the per-entry key/value length bound the aggregate
	// rendered config at admission so this free-form map cannot bloat the plugin
	// section past reasonable size.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 256 && size(self[k]) <= 1024)",message="each extraOptions key must be <=256 characters and each value <=1024 characters"
	// +optional
	ExtraOptions map[string]string `json:"extraOptions,omitempty"`
}

// BarbicanRefSpec references a Barbican CR by name in the same namespace.
// Modeled as a dedicated struct (rather than corev1.LocalObjectReference) so the
// name carries the same MinLength schema guard as the shared
// commonv1.SecretRefSpec. The reference is inverted attachment: the store points
// at the Barbican, not the other way round, so stores can be added and removed
// without editing the Barbican CR.
type BarbicanRefSpec struct {
	// Name is the referenced Barbican CR's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SecretNameRefSpec references a Kubernetes Secret by name in the store's
// namespace. Unlike commonv1.SecretRefSpec it carries no key field: the data
// keys these Secrets must expose are fixed by contract (see OpenBaoRoleIDKey /
// OpenBaoSecretIDKey / OpenBaoCAKey), so there is nothing to select.
type SecretNameRefSpec struct {
	// Name is the referenced Secret's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// InstanceRefSpec references an OpenBaoCluster (openbao.org/v1alpha1) by name in
// the store's namespace. It is kept separate from SecretNameRefSpec so the
// generated schema describes what the name actually addresses: the OpenBao
// instance the operator provisions against, not a Secret.
type InstanceRefSpec struct {
	// Name is the referenced OpenBaoCluster's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// OpenBaoStoreSpec configures barbican's vault secret-store plugin against one
// OpenBao server, in one of two modes.
//
// Managed mode (instanceRef) points at an OpenBaoCluster this cluster runs: the
// operator mints the AppRole credentials at runtime and owns the KV mount.
// Brownfield mode (server) points at an OpenBao or HashiCorp Vault server
// somewhere else and is validate-only: the operator reads the referenced
// Secrets and renders the configuration, and never writes to the server.
//
// The second rule freezes the mount layout in managed mode. The delivered
// self-init contract provisions exactly one mount, barbican/, at the root
// namespace, so a managed store that asks for anything else would render a
// configuration pointing at a path nothing provisions. Brownfield stores are
// free to name any mount and any namespace, since their server is provisioned
// outside this operator.
//
// The two transition rules carry the same data-safety argument that freezes
// spec.type: the secret material already written through a store is addressed by
// mount and by mode, so material written under the old mount is not reachable
// under a new one, and switching between a managed instance and a brownfield
// server re-points the plugin at a different server entirely. Delete and
// recreate the store instead.
// +kubebuilder:validation:XValidation:rule="has(self.instanceRef) != has(self.server)",message="exactly one of instanceRef or server must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.instanceRef) || (self.kvMountpoint == 'barbican' && !has(self.namespace))",message="managed stores (instanceRef) must keep kvMountpoint barbican and leave namespace unset: the self-init contract provisions only the barbican/ mount at the root namespace"
// +kubebuilder:validation:XValidation:rule="self.kvMountpoint == oldSelf.kvMountpoint",message="kvMountpoint is immutable: the secret material already written under the old mount is not reachable under a new one"
// +kubebuilder:validation:XValidation:rule="has(self.instanceRef) == has(oldSelf.instanceRef)",message="the store mode (instanceRef vs server) is immutable: delete and recreate the store instead"
type OpenBaoStoreSpec struct {
	// InstanceRef names the OpenBaoCluster (openbao.org/v1alpha1) in this store's
	// namespace to provision against (managed mode). Everything else is derived
	// from that name by convention: the server URL, the CA Secret
	// "<instance>-tls-ca", and the provisioner ServiceAccount
	// "<instance>-provisioner" the operator authenticates as to mint the store's
	// AppRole credentials.
	// +optional
	InstanceRef *InstanceRefSpec `json:"instanceRef,omitempty"`

	// Server describes an OpenBao or HashiCorp Vault server provisioned outside
	// this operator (brownfield mode). The operator only validates the referenced
	// credentials and renders them into the configuration; it never creates a
	// mount, a policy, or an AppRole on such a server.
	// +optional
	Server *OpenBaoServerSpec `json:"server,omitempty"`

	// KVMountpoint is the path the KV v2 secrets engine holding barbican's secret
	// material is mounted at. Managed stores are pinned to the delivered default
	// (rule above); brownfield stores name whatever mount their server provides.
	// The default only fills an absent field, so the MinLength guard is what
	// rejects an explicitly empty mount path.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default=barbican
	// +optional
	KVMountpoint string `json:"kvMountpoint,omitempty"`

	// Namespace scopes every request to an OpenBao/Vault namespace (the
	// enterprise-style multi-tenancy header). Brownfield only: the delivered
	// self-init contract provisions at the root namespace, so a managed store
	// leaves this unset.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// OpenBaoServerSpec addresses an existing OpenBao or HashiCorp Vault server for
// a brownfield store. Both Secret references resolve in the store's namespace.
type OpenBaoServerSpec struct {
	// URL is the server's API base URL, e.g. "https://openbao.example.com:8200".
	//
	// TLS is mandatory. Both the operator's own AppRole login and every secret
	// barbican stores travel this URL, so a plaintext scheme would put the role
	// ID, the secret ID, and the private keys and certificates the service exists
	// to protect on the wire in the clear — and it would silently ignore a
	// supplied caBundleSecretRef, leaving a CR that looks TLS-configured but is
	// not.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url"`

	// CABundleSecretRef references the Secret holding the PEM CA bundle that
	// authenticates the server, under the fixed data key "ca.crt" (see
	// OpenBaoCAKey). Omit it when the server presents a certificate the pods
	// already trust through their system store.
	// +optional
	CABundleSecretRef *SecretNameRefSpec `json:"caBundleSecretRef,omitempty"`

	// CredentialsSecretRef references the Secret holding the AppRole credentials
	// barbican authenticates with, under the fixed data keys "role-id" and
	// "secret-id" (see OpenBaoRoleIDKey / OpenBaoSecretIDKey). The AppRole is
	// created on the server outside this operator, which only reads the Secret.
	CredentialsSecretRef SecretNameRefSpec `json:"credentialsSecretRef"`
}

// BarbicanSecretStoreStatus defines the observed state of BarbicanSecretStore.
// The dedicated BarbicanSecretStore controller is the single writer of this
// status; the barbican-side sub-reconciler only reads it (a ready store gates
// config projection) and writes an aggregated condition onto the Barbican CR
// instead.
type BarbicanSecretStoreStatus struct {
	// Conditions represent the latest available observations of the store state:
	// CredentialsReady (the AppRole credentials Secret exists and carries the
	// contract data keys), ProvisioningReady (managed mode only: the KV mount,
	// the policy, and the AppRole exist on the referenced OpenBaoCluster),
	// ConfigProjected (the rendered secret-store section is wired into the
	// running Barbican Deployment), and the aggregate Ready.
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
}

func init() {
	SchemeBuilder.Register(&BarbicanSecretStore{}, &BarbicanSecretStoreList{})
}
