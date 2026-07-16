// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Fixed data-key contract for the S3 credentials Secret. The Secret referenced
// by S3BackendSpec.CredentialsSecretRef MUST carry exactly these two keys; the
// key names are pinned by contract (not configurable per CR) and match the
// repo's Garage S3 seeding.
const (
	// S3AccessKeyIDKey is the credentials Secret data key holding the S3 access
	// key ID (rendered into s3_store_access_key).
	S3AccessKeyIDKey = "access-key-id"
	// S3SecretAccessKeyKey is the credentials Secret data key holding the S3
	// secret access key (rendered into s3_store_secret_key).
	S3SecretAccessKeyKey = "secret-access-key"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Default",type="boolean",JSONPath=".spec.isDefault"
// +kubebuilder:printcolumn:name="Glance",type="string",JSONPath=".spec.glanceRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// GlanceBackend is the Schema for the glancebackends API. One CR attaches to a
// Glance CR via spec.glanceRef and describes one image store (Phase 1: an
// S3-compatible object store). A dedicated controller owns the backend lifecycle
// — finalizer, credential resolution, per-backend conditions — while the
// glance-side sub-reconciler aggregates all attached, credential-ready backends
// into the rendered glance-api.conf store sections, promoting the one marked
// isDefault to the default store.
type GlanceBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GlanceBackendSpec   `json:"spec,omitempty"`
	Status GlanceBackendStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GlanceBackendList contains a list of GlanceBackend.
type GlanceBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GlanceBackend `json:"items"`
}

// GlanceBackendType enumerates the supported image-store drivers. Phase 1 ships
// S3; the Ceph/Cinder enum values arrive with their follow-ups. The File store
// is deliberately never supported — a shared object store is required so every
// Glance replica reads and writes the same images.
// +kubebuilder:validation:Enum=S3
type GlanceBackendType string

const (
	// GlanceBackendTypeS3 selects the Glance S3 store driver.
	GlanceBackendTypeS3 GlanceBackendType = "S3"
)

// GlanceBackendSpec defines the desired state of GlanceBackend.
//
// The glanceRef transition rule (evaluated only on UPDATE) makes the attachment
// immutable: re-pointing a backend at a different Glance would leave the old
// deployment with a store nothing manages anymore and race the config projection
// on the new one. Delete and recreate instead. The type/s3 union rule enforces
// "exactly one backend block matching spec.type" at the schema layer so it holds
// even when the validating webhook is down.
// +kubebuilder:validation:XValidation:rule="self.glanceRef.name == oldSelf.glanceRef.name",message="glanceRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="type is immutable"
// +kubebuilder:validation:XValidation:rule="(self.type == 'S3') == has(self.s3)",message="exactly one backend block matching spec.type must be set (type S3 requires spec.s3)"
type GlanceBackendSpec struct {
	// GlanceRef names the Glance CR in the same namespace this backend attaches
	// to. The referenced CR does not have to exist at admission time (GitOps
	// ordering: the backend may be applied before the Glance CR); a dangling
	// reference surfaces as Ready=False.
	GlanceRef GlanceRefSpec `json:"glanceRef"`

	// Type selects the image-store driver. Phase 1 supports S3 only.
	Type GlanceBackendType `json:"type"`

	// S3 configures the S3-compatible object store. Required exactly when type is
	// S3 (union rule above).
	// +optional
	S3 *S3BackendSpec `json:"s3,omitempty"`

	// IsDefault marks this backend as the Glance default store. Mutable: exactly
	// one attached, credential-ready backend must be the default, and flipping it
	// re-renders the parent Glance's config (default_backend). The glance-side
	// sub-reconciler and the validating webhook enforce the single-default
	// invariant.
	// +optional
	IsDefault bool `json:"isDefault,omitempty"`

	// ExtraOptions provides free-form [<name>] store-section options not covered
	// by the typed fields, keyed by bare option name. The validating webhook
	// rejects options already owned by the typed fields (a denylist) so the escape
	// hatch cannot silently contradict the typed spec.
	//
	// MaxProperties and the per-entry key/value length bound the aggregate
	// rendered config at admission so this free-form map cannot bloat the store
	// section past reasonable size.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 256 && size(self[k]) <= 1024)",message="each extraOptions key must be <=256 characters and each value <=1024 characters"
	// +optional
	ExtraOptions map[string]string `json:"extraOptions,omitempty"`
}

// GlanceRefSpec references a Glance CR by name in the same namespace. Modeled as
// a dedicated struct (rather than corev1.LocalObjectReference) so the name
// carries the same MinLength schema guard as the shared commonv1.SecretRefSpec.
// The reference is inverted attachment: the backend points at the Glance, not the
// other way round, so stores can be added and removed without editing the Glance
// CR.
type GlanceRefSpec struct {
	// Name is the referenced Glance CR's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SecretNameRefSpec references a Kubernetes Secret by name in the backend's
// namespace. Unlike commonv1.SecretRefSpec it carries no key field: the data
// keys these Secrets must expose are fixed by contract (see S3AccessKeyIDKey /
// S3SecretAccessKeyKey), so there is nothing to select.
type SecretNameRefSpec struct {
	// Name is the referenced Secret's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// S3BackendSpec configures the Glance S3 store driver for one backend. Optional
// fields are only rendered into the store section when set, so upstream Glance
// defaults apply for everything left unset.
type S3BackendSpec struct {
	// Host is the S3 endpoint URL (s3_store_host), e.g.
	// "https://s3.example.com".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	Host string `json:"host"`

	// Bucket is the S3 bucket images are stored in (s3_store_bucket).
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// CredentialsSecretRef references the Secret holding the S3 credentials under
	// the fixed data keys "access-key-id" and "secret-access-key" (see
	// S3AccessKeyIDKey / S3SecretAccessKeyKey). The key names are pinned by
	// contract and match the repo's Garage S3 seeding.
	CredentialsSecretRef SecretNameRefSpec `json:"credentialsSecretRef"`

	// BucketURLFormat selects how the bucket is addressed in request URLs
	// (s3_store_bucket_url_format): "path" (https://host/bucket) or "virtual"
	// (https://bucket.host). Defaults to "path".
	// +kubebuilder:validation:Enum=path;virtual
	// +kubebuilder:default=path
	// +optional
	BucketURLFormat string `json:"bucketURLFormat,omitempty"`

	// Region is the S3 region the bucket lives in (s3_store_region_name).
	// +optional
	Region string `json:"region,omitempty"`

	// CreateBucketOnPut lets Glance create the bucket on first write when it does
	// not already exist (s3_store_create_bucket_on_put).
	// +optional
	CreateBucketOnPut bool `json:"createBucketOnPut,omitempty"`

	// LargeObjectSize is the image size threshold, in MiB, above which Glance
	// switches to multipart upload (s3_store_large_object_size). Only rendered
	// when set.
	// +kubebuilder:validation:Minimum=1
	// +optional
	LargeObjectSize *int32 `json:"largeObjectSize,omitempty"`

	// LargeObjectChunkSize is the multipart chunk size, in MiB, used for uploads
	// above LargeObjectSize (s3_store_large_object_chunk_size). Only rendered when
	// set.
	// +kubebuilder:validation:Minimum=1
	// +optional
	LargeObjectChunkSize *int32 `json:"largeObjectChunkSize,omitempty"`
}

// GlanceBackendStatus defines the observed state of GlanceBackend. The dedicated
// GlanceBackend controller is the single writer of this status; the glance-side
// sub-reconciler only reads it (a credential-ready backend gates config
// projection) and writes an aggregated condition onto the Glance CR instead.
type GlanceBackendStatus struct {
	// Conditions represent the latest available observations of the backend state:
	// CredentialsReady (the referenced credentials Secret exists and carries the
	// contract data keys), ConfigProjected (the rendered store section is wired
	// into the running Glance Deployment), and the aggregate Ready.
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
	SchemeBuilder.Register(&GlanceBackend{}, &GlanceBackendList{})
}
