// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Release",type="string",JSONPath=".status.installedRelease"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Placement is the Schema for the placements API. It deploys the Placement
// service: the API server, its database and cache connections, and the Keystone
// integration. Placement tracks resource-provider inventories and allocations
// for the compute services; it owns no backing stores of its own, so its spec
// stays close to the plain API-server shape.
type Placement struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlacementSpec   `json:"spec,omitempty"`
	Status PlacementStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PlacementList contains a list of Placement.
type PlacementList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Placement `json:"items"`
}

// PlacementSpec defines the desired state of Placement.
//
// The targetClusterRef transition rules (evaluated only on UPDATE) freeze the
// ref: adding it, removing it, or renaming it after creation is rejected at the
// schema layer, so the guarantee holds even when the validating webhook is
// down. Moving a service between clusters is not a supported mutation.
// +kubebuilder:validation:XValidation:rule="has(self.targetClusterRef) == has(oldSelf.targetClusterRef)",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.targetClusterRef) || !has(oldSelf.targetClusterRef) || self.targetClusterRef.name == oldSelf.targetClusterRef.name",message="targetClusterRef is immutable"
type PlacementSpec struct {
	// OpenStackRelease names the OpenStack release this operator deploys and
	// drives. It governs install/upgrade release tracking:
	// status.installedRelease is promoted to this value after a successful
	// db-sync. It is deliberately kept separate from the image tag so
	// digest-pinned images keep working: pinning spec.image.digest disables
	// tag-based release tracking, but this field still tells the operator which
	// schema to converge to.
	//
	// The pattern matches the OpenStack date-based release scheme (YYYY.N where N
	// is 1 or 2, the two-releases-per-year cadence, e.g. 2025.2, 2026.1). The
	// [12] minor class keeps this CRD pattern, the validating webhook, and
	// release.ParseRelease in agreement so a non-cadence minor (e.g. 2025.9) is
	// rejected at every layer.
	// +kubebuilder:validation:Pattern=`^\d{4}\.[12]$`
	OpenStackRelease string `json:"openStackRelease"`

	// Deployment groups the pod-level knobs for the Placement API Deployment
	// (replicas, resources, rollout strategy, graceful-termination timings, and
	// scheduling constraints).
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`

	// Image defines the Placement container image reference. Like the sibling
	// operators, the field carries no immutability rule: image upgrades are
	// routine.
	Image commonv1.ImageSpec `json:"image"`

	// Database defines the MariaDB connection parameters.
	// Supports managed (clusterRef) and brownfield (host/port) modes. The
	// clusterRef/host mutual-exclusivity rule and the credentialsMode
	// (Static/Dynamic) contract are inherited from commonv1.DatabaseSpec, so they
	// hold here without per-field duplication. Placement reads the resolved URL
	// from the [placement_database] section rather than [database].
	Database commonv1.DatabaseSpec `json:"database"`

	// Cache defines the Memcached cache configuration.
	// Supports managed (clusterRef) and brownfield (servers) modes; the
	// clusterRef/servers mutual-exclusivity rule lives on commonv1.CacheSpec.
	Cache commonv1.CacheSpec `json:"cache"`

	// KeystoneEndpoint is the Keystone endpoint URL Placement authenticates
	// against. It renders as [keystone_authtoken] auth_url in placement.conf.
	// Placement connects to this URL server-side (token validation on every API
	// request), so it must be reachable from the Placement pods. For a colocated
	// control plane that is the cluster-local Service URL, never an externally
	// routable address that only resolves outside the cluster. A plain URL field
	// keeps the operator decoupled from the keystone-operator; the c5c3
	// ControlPlane operator projects it from its Keystone child by naming
	// convention.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystoneEndpoint string `json:"keystoneEndpoint"`

	// KeystonePublicEndpoint is the browser/client-facing Keystone base URL
	// Placement advertises as [keystone_authtoken] www_authenticate_uri, the
	// address a 401 response points unauthenticated clients at. Optional: when
	// empty the operator falls back to KeystoneEndpoint (see
	// EffectiveKeystonePublicEndpoint), which is correct only when the internal
	// and public Keystone URLs coincide.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystonePublicEndpoint string `json:"keystonePublicEndpoint,omitempty"`

	// ServiceUser identifies the Keystone service account Placement
	// authenticates as and the Secret holding its password. The defaulting
	// webhook fills the username/domain fields (placement / service / Default /
	// Default); the password Secret reference is required.
	ServiceUser ServiceUserSpec `json:"serviceUser"`

	// Region is the Keystone region Placement authenticates against
	// ([keystone_authtoken] region_name). Optional: when empty the option is
	// omitted and Placement uses the Keystone catalog's default region.
	// +optional
	Region string `json:"region,omitempty"`

	// APIServer tunes the Placement API server process. When nil the operator
	// uses hardcoded defaults.
	// +optional
	APIServer *APIServerSpec `json:"apiServer,omitempty"`

	// Gateway configures external exposure of the Placement API via a Gateway API
	// HTTPRoute. When set, the operator creates an HTTPRoute targeting the {name}
	// Service and attaches it to the referenced pre-existing Gateway. When removed
	// (nil), the HTTPRoute is deleted. The Gateway and GatewayClass are
	// infrastructure concerns managed outside this operator.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`

	// NetworkPolicy configures network isolation for Placement API pods.
	// When set, a NetworkPolicy is created restricting ingress and egress traffic.
	// When removed (nil), the NetworkPolicy is deleted and traffic flows
	// unrestricted.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`

	// Autoscaling configures horizontal pod autoscaling for the Placement API
	// deployment. When set, a HorizontalPodAutoscaler is created targeting the
	// deployment. When removed, the HPA is deleted.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Logging configures oslo.log output for the Placement API container.
	// When unset, the defaulting webhook materializes a LoggingSpec with
	// Format=text, Level=INFO, Debug=false.
	// +optional
	Logging *LoggingSpec `json:"logging,omitempty"`

	// SecretStoreRef selects the External Secrets store the operator routes this
	// Placement's ExternalSecrets and PushSecrets through. When omitted the
	// operator uses the shared cluster-scoped openbao-cluster-store, so existing
	// deployments keep working unchanged. Set kind to SecretStore with the name of
	// a namespaced store in THIS Placement's namespace to reach OpenBao as a
	// per-tenant identity. The ControlPlane operator projects this field onto the
	// Placement it owns, so operators normally configure it there rather than
	// here.
	// +optional
	SecretStoreRef *commonv1.SecretStoreRefSpec `json:"secretStoreRef,omitempty"`

	// TargetClusterRef names the registered target cluster that receives this
	// Placement's children: Deployments, ConfigMaps, Secrets, and the database
	// CRs. The CR itself stays on the management cluster, and so do its status
	// and its finalizers. When omitted, the children are created on the local
	// cluster (the management cluster the operator runs on) and the deployment
	// behaves exactly like a single-cluster one. The field is immutable (see the
	// transition rules on this spec).
	//
	// A child written to the target carries no owner reference, since nothing
	// on that cluster resolves one into the management cluster. Three labels
	// name the owner instead: openstack.c5c3.io/owner-kind,
	// openstack.c5c3.io/owner-name, and openstack.c5c3.io/owner-namespace.
	// Deleting the CR deletes the children that carry its labels, under a
	// finalizer the CR installs whenever the ref is set, so a target cluster
	// may have the service CRDs installed. A cluster deregistered past the
	// abandon window is the exception: its children cannot be reached and stay
	// behind on it.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`

	// PolicyOverrides defines custom oslo.policy rules for the service.
	// When set, the operator renders a policy.yaml and configures
	// oslo_policy.policy_file automatically.
	// +optional
	// +kubebuilder:validation:XValidation:rule="(has(self.rules) && size(self.rules) > 0) || self.configMapRef != null",message="at least one of rules or configMapRef must be set"
	// The empty rule-name and rule-value constraints are enforced by the
	// XValidation markers on commonv1.PolicySpec itself, so they apply to every
	// PolicySpec field across operators without per-field duplication.
	PolicyOverrides *commonv1.PolicySpec `json:"policyOverrides,omitempty"`

	// ExtraConfig provides free-form INI sections for configuration not covered by
	// explicit CRD fields. It is the deliberate escape hatch for options that have
	// no dedicated knob of their own.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`
}

// DeploymentSpec, AutoscalingSpec, NetworkPolicySpec,
// NetworkPolicyIngressSource, LoggingSpec, GatewaySpec, and
// GatewayParentRefSpec are aliased to the shared commonv1 definitions.
// commonv1 carries the canonical per-field godoc and validation markers.
// The aliases keep call sites (placementv1alpha1.DeploymentSpec and bare
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

// ServiceUserSpec identifies the Keystone service account Placement uses to
// validate tokens and call other services, and references the Secret holding its
// password. The name and domain fields are optional; the defaulting webhook
// materializes them (username placement, projectName service, userDomainName and
// projectDomainName Default), so a minimal CR need only supply the password
// Secret reference.
type ServiceUserSpec struct {
	// Username is the Keystone username Placement authenticates as
	// ([keystone_authtoken] username). Webhook-defaulted to "placement".
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
	// key is webhook-defaulted to "password".
	SecretRef commonv1.SecretRefSpec `json:"secretRef"`
}

// APIServerSpec tunes the Placement API server process. It carries the uWSGI
// parameters alone: placement has only ever shipped a WSGI application and no
// eventlet server, so there is no launch mode to select and no worker count to
// configure outside uWSGI. That is why the sibling glance operator's workers
// field has no counterpart here.
type APIServerSpec struct {
	// UWSGI configures the uWSGI application server parameters.
	// +optional
	UWSGI *UWSGISpec `json:"uwsgi,omitempty"`
}

// UWSGISpec is aliased to the shared commonv1 definition (see the
// DeploymentSpec alias block above for the rationale).
type UWSGISpec = commonv1.UWSGISpec

// PlacementStatus defines the observed state of Placement.
type PlacementStatus struct {
	// Conditions represent the latest available observations of the Placement
	// state. Each condition carries an ObservedGeneration so consumers can tell a
	// stale condition from one reflecting the current spec; use the conditions
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

	// Endpoint is the Placement API endpoint URL clients use.
	Endpoint string `json:"endpoint,omitempty"`

	// InstalledRelease is the OpenStack release whose database schema is currently
	// installed, promoted to spec.openStackRelease after a successful db-sync.
	InstalledRelease string `json:"installedRelease,omitempty"`

	// InstalledImage is the spec.image reference that migrated the schema
	// InstalledRelease names. It is what keeps release tracking honest for a
	// digest-pinned image: a digest carries no parseable release, so a
	// spec.openStackRelease bump that leaves spec.image untouched leaves the
	// db-sync Job's pod template unchanged, runs no migration at all, and would
	// still promote InstalledRelease. The release gate compares against this field
	// to refuse that transition.
	InstalledImage string `json:"installedImage,omitempty"`

	// TargetRelease is the spec.openStackRelease a release bump is converging to.
	// It is set while the db-sync for that release runs and cleared once
	// InstalledRelease equals the spec release.
	TargetRelease string `json:"targetRelease,omitempty"`
}

// EffectiveKeystonePublicEndpoint resolves the [keystone_authtoken]
// www_authenticate_uri value: the explicit KeystonePublicEndpoint when set,
// otherwise KeystoneEndpoint. It is resolved at render time rather than
// webhook-defaulted so a later edit to keystoneEndpoint keeps being tracked by
// the fallback instead of freezing a once-defaulted value.
func (s *PlacementSpec) EffectiveKeystonePublicEndpoint() string {
	if s.KeystonePublicEndpoint != "" {
		return s.KeystonePublicEndpoint
	}
	return s.KeystoneEndpoint
}

func init() {
	SchemeBuilder.Register(&Placement{}, &PlacementList{})
}
