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

	// KeystoneEndpoint is the Keystone public endpoint URL the dashboard
	// authenticates against (OPENSTACK_KEYSTONE_URL). A plain URL field keeps
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

	// ExtraConfig provides free-form Django settings rendered verbatim into
	// local_settings.py after the operator defaults, so user values win.
	// Keys are Django setting names (e.g. SESSION_TIMEOUT); values are
	// arbitrary JSON converted structurally to Python literals.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.all(k, k != '')",message="extraConfig setting name must not be empty"
	ExtraConfig map[string]apiextensionsv1.JSON `json:"extraConfig,omitempty"`
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
