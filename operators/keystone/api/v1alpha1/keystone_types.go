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
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
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
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	Replicas int32 `json:"replicas,omitempty"`

	// Image defines the Keystone container image reference.
	Image commonv1.ImageSpec `json:"image"`

	// Database defines the MariaDB connection parameters.
	// Supports managed (clusterRef) and brownfield (host/port) modes.
	// +kubebuilder:validation:XValidation:rule="has(self.clusterRef) != has(self.host)",message="exactly one of clusterRef or host must be set"
	Database commonv1.DatabaseSpec `json:"database"`

	// Cache defines the Memcached cache configuration.
	// Supports managed (clusterRef) and brownfield (servers) modes.
	// +kubebuilder:validation:XValidation:rule="has(self.clusterRef) != (has(self.servers) && size(self.servers) > 0)",message="exactly one of clusterRef or servers must be set"
	Cache commonv1.CacheSpec `json:"cache"`

	// Fernet configures Fernet key rotation.
	Fernet FernetSpec `json:"fernet,omitempty"`

	// Federation configures Keystone federation (optional).
	// +optional
	Federation *FederationSpec `json:"federation,omitempty"`

	// Bootstrap configures the initial Keystone bootstrap.
	Bootstrap BootstrapSpec `json:"bootstrap"`

	// Middleware defines WSGI middleware filters for the api-paste.ini pipeline.
	// +optional
	Middleware []commonv1.MiddlewareSpec `json:"middleware,omitempty"`

	// Plugins defines service plugins/drivers to configure.
	// +optional
	Plugins []commonv1.PluginSpec `json:"plugins,omitempty"`

	// PolicyOverrides defines custom oslo.policy rules for the service.
	// When set, the operator renders a policy.yaml and configures
	// oslo_policy.policy_file automatically.
	// +optional
	// +kubebuilder:validation:XValidation:rule="(has(self.rules) && size(self.rules) > 0) || self.configMapRef != null",message="at least one of rules or configMapRef must be set"
	// +kubebuilder:validation:XValidation:rule="!has(self.rules) || self.rules.all(k, k != '')",message="policy rule name must not be empty"
	PolicyOverrides *commonv1.PolicySpec `json:"policyOverrides,omitempty"`

	// ExtraConfig provides free-form INI sections for configuration
	// not covered by explicit CRD fields.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`
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
}

// FederationSpec defines Keystone federation configuration.
type FederationSpec struct {
	// Enabled activates federation support.
	Enabled bool `json:"enabled"`
}

// BootstrapSpec defines Keystone bootstrap parameters.
type BootstrapSpec struct {
	// AdminUser is the admin username for the bootstrap.
	// +kubebuilder:default="admin"
	AdminUser string `json:"adminUser,omitempty"`

	// AdminPasswordSecretRef references the Secret containing the admin password.
	AdminPasswordSecretRef commonv1.SecretRefSpec `json:"adminPasswordSecretRef"`

	// Region is the Keystone region name.
	// +kubebuilder:default="RegionOne"
	Region string `json:"region,omitempty"`

	// PublicEndpoint is the externally routable Keystone endpoint URL used for
	// --bootstrap-public-url. When unset, the cluster-local service DNS is used
	// as a fallback. External clients (CLI users, Horizon, federation partners)
	// require a routable address here (CC-0013).
	// +optional
	PublicEndpoint string `json:"publicEndpoint,omitempty"`
}

// KeystoneStatus defines the observed state of Keystone.
type KeystoneStatus struct {
	// Conditions represent the latest available observations of the Keystone state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Endpoint is the Keystone API endpoint URL.
	Endpoint string `json:"endpoint,omitempty"`
}

func init() {
	SchemeBuilder.Register(&Keystone{}, &KeystoneList{})
}
