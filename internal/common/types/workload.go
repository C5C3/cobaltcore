// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package types

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// Graceful-termination effective defaults.
// These constants are the single source of truth used by the operator
// validating webhooks (for cross-field arithmetic when pointer fields are nil)
// and the reconcilers (which apply them when rendering the Deployment).
// Keeping them beside DeploymentSpec ensures webhook and reconciler cannot
// drift apart across operators.
const (
	// DefaultTerminationGracePeriodSeconds is applied when DeploymentSpec.TerminationGracePeriodSeconds is nil.
	DefaultTerminationGracePeriodSeconds int64 = 30
	// DefaultPreStopSleepSeconds is applied when DeploymentSpec.PreStopSleepSeconds is nil.
	DefaultPreStopSleepSeconds int64 = 5

	// DefaultReplicas is the desired API pod count materialized by the
	// defaulting webhooks (Default) when spec.deployment.replicas is zero, and
	// the fallback the reconcilers apply when they render the Deployment/PDB/HPA
	// for a CR that reached the controller with a zero-valued replica count — a
	// spec that bypassed the mutating webhook, or one that omitted the
	// spec.deployment block so the nested +kubebuilder:default never
	// materialized (Kubernetes does not descend into an absent object to apply
	// leaf defaults). Left unnormalized, a zero would scale the Deployment to
	// zero pods. It is the single source of truth so the webhooks and
	// reconcilers cannot drift; the +kubebuilder:default=3 marker on
	// DeploymentSpec.Replicas keeps the same literal in sync (markers cannot
	// reference Go constants).
	DefaultReplicas int32 = 3
)

// Default resource requests and limits for the service API container. These
// unexported vars are the single source of truth for the defaulting webhooks;
// they ensure Burstable QoS class and enable HPA utilization-based scaling. They
// are exposed only through the accessor functions below, which return a copy so
// no caller can mutate the shared default.
var (
	defaultMemoryRequest = resource.MustParse("256Mi")
	defaultCPURequest    = resource.MustParse("100m")
	defaultMemoryLimit   = resource.MustParse("512Mi")
	defaultCPULimit      = resource.MustParse("500m")
)

// DefaultMemoryRequest returns a copy of the default memory request for the
// service API container.
func DefaultMemoryRequest() resource.Quantity { return defaultMemoryRequest.DeepCopy() }

// DefaultCPURequest returns a copy of the default CPU request for the service
// API container.
func DefaultCPURequest() resource.Quantity { return defaultCPURequest.DeepCopy() }

// DefaultMemoryLimit returns a copy of the default memory limit for the service
// API container.
func DefaultMemoryLimit() resource.Quantity { return defaultMemoryLimit.DeepCopy() }

// DefaultCPULimit returns a copy of the default CPU limit for the service API
// container.
func DefaultCPULimit() resource.Quantity { return defaultCPULimit.DeepCopy() }

// DeploymentSpec groups the pod-level knobs for the Keystone API Deployment.
// Grouping them under spec.deployment keeps the KeystoneSpec root legible as
// further scheduling knobs (affinity/tolerations/nodeSelector) are added.
//
// The drain-window CEL rule mirrors the validating webhook: when one or both of
// the nil-preserving pointers is unset, the rule substitutes the same effective
// defaults the reconciler applies. The literals 5 and 30 must stay in sync with
// DefaultPreStopSleepSeconds and DefaultTerminationGracePeriodSeconds in
// keystone_webhook.go — kubebuilder/CEL rules cannot reference Go constants.
// +kubebuilder:validation:XValidation:rule="(has(self.preStopSleepSeconds) ? self.preStopSleepSeconds : 5) < (has(self.terminationGracePeriodSeconds) ? self.terminationGracePeriodSeconds : 30)",message="preStopSleepSeconds must be strictly less than terminationGracePeriodSeconds"
type DeploymentSpec struct {
	// Replicas is the desired number of Keystone API pods.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	Replicas int32 `json:"replicas,omitempty"`

	// Resources defines the CPU and memory requests and limits for the Keystone API
	// container. When unset, the defaulting webhook injects sensible defaults
	// (256Mi/512Mi memory, 100m/500m CPU) to ensure Burstable QoS class and
	// enable HPA utilization calculations.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Note (internal design decision — kept out of the user-facing CRD description): a +kubebuilder:default=30
	// marker on a pointer field would cause the API server to materialize the
	// value at admission, mutating pre-existing CRs on operator upgrade —
	// exactly what the nil-preserving contract forbids. The marker is therefore
	// omitted and the effective "default 30" is applied by the reconciler when the
	// pointer is nil, mirroring the AutoscalingSpec.MinReplicas pattern. This
	// comment group is separated from the field's godoc by a blank line so
	// controller-gen excludes it from `kubectl explain` output.

	// TerminationGracePeriodSeconds is the grace period (seconds) granted to
	// Keystone API pods between SIGTERM and SIGKILL during rolling updates
	// Extend this to cover slow upstream token validation (LDAP/DB)
	// so in-flight requests finish before the kubelet forcibly kills uWSGI.
	// When nil, the reconciler omits the field from the pod template and the
	// Kubernetes default of 30s applies. Must be at least 10s when set.
	// +optional
	// +kubebuilder:validation:Minimum=10
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// PreStopSleepSeconds is the sleep duration (seconds) of the preStop
	// lifecycle hook, configured independently of the overall grace period
	// This covers the window between EndpointSlice removal and
	// kube-proxy/ingress-controller propagation so new requests stop arriving
	// before SIGTERM reaches uWSGI. When nil, the reconciler applies a default
	// of 5s. Zero is permitted to disable the sleep. The cross-field rule
	// preStopSleepSeconds < terminationGracePeriodSeconds is enforced by the
	// validating webhook to guarantee a non-zero drain window.
	// +optional
	// +kubebuilder:validation:Minimum=0
	PreStopSleepSeconds *int64 `json:"preStopSleepSeconds,omitempty"`

	// Strategy overrides the Deployment rollout strategy for the Keystone API
	// Deployment. When nil, the reconciler applies RollingUpdate
	// with MaxUnavailable=0 and MaxSurge=1 to guarantee surge-before-remove
	// behavior — available capacity never dips below spec.deployment.replicas
	// during an image-tag patch. Set this to customize maxSurge/maxUnavailable,
	// or to switch the type to Recreate for site-specific rollout policies.
	// +optional
	Strategy *appsv1.DeploymentStrategy `json:"strategy,omitempty"`

	// TopologySpreadConstraints describes how pods should be spread across
	// topology domains (zones, nodes) to achieve high availability.
	// When nil (unset), the operator injects two default constraints:
	// zone-spread (topology.kubernetes.io/zone) and hostname-spread
	// (kubernetes.io/hostname), both MaxSkew=1 with ScheduleAnyway.
	// When set to a non-nil value (including an empty slice), the user-provided
	// constraints are used verbatim — an empty slice disables defaults.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// PriorityClassName sets the priority class for Keystone API pods.
	// When set, the operator passes the value through to the PodSpec, allowing
	// cluster administrators to control scheduling priority and preemption.
	// When unset, no priority class is configured and the cluster default applies.
	// +optional
	PriorityClassName *string `json:"priorityClassName,omitempty"`
}

// Default sets the shared-type defaults on a DeploymentSpec in place: a
// zero-valued Replicas becomes DefaultReplicas, and a nil-or-empty Resources
// block is filled with the default requests/limits so the container gets
// Burstable QoS and HPA utilization calculations work. It encodes exactly the
// defaults the keystone defaulting webhook previously applied inline; operator
// webhooks call it so the shared type defaults cannot drift across operators.
func (d *DeploymentSpec) Default() {
	if d.Replicas == 0 {
		d.Replicas = DefaultReplicas
	}
	// Default resource requests and limits for Burstable QoS and HPA
	// utilization calculations. Also defaults when Resources is non-nil but
	// empty (e.g. `resources: {}`), which would otherwise produce BestEffort
	// QoS and break HPA utilization calculations.
	if d.Resources == nil || (len(d.Resources.Requests) == 0 && len(d.Resources.Limits) == 0) {
		d.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: DefaultMemoryRequest(),
				corev1.ResourceCPU:    DefaultCPURequest(),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: DefaultMemoryLimit(),
				corev1.ResourceCPU:    DefaultCPULimit(),
			},
		}
	}
}

// AutoscalingSpec defines the parameters for horizontal pod autoscaling.
// +kubebuilder:validation:XValidation:rule="has(self.targetCPUUtilization) || has(self.targetMemoryUtilization)",message="at least one of targetCPUUtilization or targetMemoryUtilization must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
type AutoscalingSpec struct {
	// MinReplicas is the lower bound for the number of replicas.
	// Defaults to the current spec.replicas value if unset.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the upper bound for the number of replicas.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// TargetCPUUtilization is the target average CPU utilization (percentage).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetCPUUtilization *int32 `json:"targetCPUUtilization,omitempty"`

	// TargetMemoryUtilization is the target average memory utilization (percentage).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetMemoryUtilization *int32 `json:"targetMemoryUtilization,omitempty"`
}

// NetworkPolicySpec defines network isolation for Keystone API pods.
// When applied, the operator creates a NetworkPolicy that restricts ingress
// to TCP 5000 from the specified sources and auto-derives egress rules for
// DNS, MariaDB (from database.ClusterRef), and Memcached (from cache.ClusterRef).
// +kubebuilder:validation:XValidation:rule="size(self.ingress) > 0",message="at least one ingress source must be specified"
type NetworkPolicySpec struct {
	// Ingress defines the sources allowed to reach Keystone API on TCP 5000.
	// Each source specifies a namespace selector and an optional pod selector.
	// Multiple sources produce multiple From peers in a single ingress rule
	// (OR across peers, AND within a peer's selectors).
	Ingress []NetworkPolicyIngressSource `json:"ingress"`

	// AdditionalEgress defines extra egress rules appended after auto-derived
	// rules (DNS, MariaDB, Memcached). Use this for brownfield backends,
	// external APIs, or any target not covered by ClusterRef auto-derivation.
	// +optional
	AdditionalEgress []networkingv1.NetworkPolicyEgressRule `json:"additionalEgress,omitempty"`
}

// NetworkPolicyIngressSource defines a source from which traffic is allowed
// to reach the Keystone API pods on TCP 5000.
type NetworkPolicyIngressSource struct {
	// NamespaceSelector selects namespaces from which traffic is allowed.
	// All pods in matching namespaces can reach Keystone on port 5000
	// unless PodSelector further restricts the set. It is a full
	// metav1.LabelSelector, so set-based matchExpressions are supported in
	// addition to matchLabels.
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// PodSelector optionally restricts allowed traffic to pods matching
	// these labels within the selected namespaces. When set, only pods
	// matching both NamespaceSelector AND PodSelector can reach Keystone
	// (AND logic within a single peer). It is a full metav1.LabelSelector,
	// so set-based matchExpressions are supported in addition to matchLabels.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`
}

// LoggingSpec configures oslo.log output for the Keystone API container.
// Exposed as an optional pointer field on KeystoneSpec; the defaulting webhook
// materializes a baseline LoggingSpec when the pointer is nil so downstream
// reconciler code never sees a nil pointer (mirrors UWSGISpec / Resources precedent).
type LoggingSpec struct {
	// Format selects the on-wire layout of oslo.log records.
	// "text" emits the standard oslo.log line format; "json" emits one
	// JSON object per record for direct ingest by Loki/OpenSearch.
	// +kubebuilder:validation:Enum=text;json
	// +kubebuilder:default=text
	Format string `json:"format,omitempty"`

	// Level is the root logger level applied to oslo.log.
	// +kubebuilder:validation:Enum=DEBUG;INFO;WARNING;ERROR;CRITICAL
	// +kubebuilder:default=INFO
	Level string `json:"level,omitempty"`

	// Debug toggles oslo.log [DEFAULT] debug=true. Independent of Level
	// because oslo.log gates several extra-verbose code paths on the
	// debug flag specifically (SQL echo, auth-backend tracing). It is a
	// nil-preserving pointer so "unset" is representable: the defaulting webhook
	// restores the documented default (false) when the pointer is nil, and the
	// reconciler falls back to the same default for CRs that bypass the webhook.
	// +optional
	Debug *bool `json:"debug,omitempty"`

	// PerLoggerLevels overrides the level of named loggers, mirroring
	// oslo.log's `default_log_levels`. Example:
	// {"sqlalchemy.engine": "WARNING", "keystone.middleware": "DEBUG"}.
	// Each value must be one of DEBUG/INFO/WARNING/ERROR/CRITICAL and every
	// logger name must be non-empty. These are now enforced by the CRD CEL
	// XValidation rules below as well as by the validating webhook (a plain
	// enum on additionalProperties is still not expressible in CRD v1, so the
	// value constraint is written as an `in [...]` CEL rule rather than an enum).
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.all(k, k != '')",message="logger name must not be empty"
	// +kubebuilder:validation:XValidation:rule="self.all(k, self[k] in ['DEBUG','INFO','WARNING','ERROR','CRITICAL'])",message="per-logger level must be one of DEBUG, INFO, WARNING, ERROR, CRITICAL"
	PerLoggerLevels map[string]string `json:"perLoggerLevels,omitempty"`
}

// Default sets the shared-type defaults on a LoggingSpec in place: an empty
// Format becomes "text", an empty Level becomes "INFO", and a nil Debug
// pointer is materialized as an explicit false. Materializing the parent
// pointer when the whole block is absent remains each operator webhook's
// decision — it calls Default() on the freshly materialized (or the present)
// struct so the leaf defaults cannot drift across operators.
func (l *LoggingSpec) Default() {
	if l.Format == "" {
		l.Format = "text"
	}
	if l.Level == "" {
		l.Level = "INFO"
	}
	if l.Debug == nil {
		l.Debug = ptr.To(false)
	}
}
