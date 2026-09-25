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

// defaultCPURequest is exposed only through DefaultCPURequest, which returns a
// copy so no caller can mutate the shared default.
var defaultCPURequest = resource.MustParse("100m")

// DefaultCPURequest returns a copy of the 100m CPU request WithResourceDefaults
// gives a container whose block names no CPU. It is also the CPU half of the
// OVN Raft request floor.
func DefaultCPURequest() resource.Quantity { return defaultCPURequest.DeepCopy() }

// NodePlacementSpec groups the fields that pick the nodes a pod may run on.
// Each field is a nil-able map, slice or pointer with omitempty, so a CR that
// sets none of them renders none and a server-side apply that omits them
// neither writes nor owns them.
type NodePlacementSpec struct {
	// NodeSelector restricts the pods to nodes that carry every listed label.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations let the pods onto nodes with matching taints.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity holds node affinity and pod (anti-)affinity rules, applied
	// beside the topology spread constraints.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

// JobBaseSpec sizes and prioritizes the pods of a CR's Jobs and CronJobs.
type JobBaseSpec struct {
	// Resources defines the CPU and memory requests and limits of every
	// container and init container of the CR's Job and CronJob pods. The
	// operator resolves them per resource when it renders the pod: a CPU the
	// block names neither as request nor as limit gets a 100m request and no
	// limit, and a memory the block names neither way gets 368Mi as both
	// request and limit. Jobs that size with their data rather than with a
	// process count (the OVN backup and Neutron's ovn-db-sync) get a 100m CPU
	// and 256Mi memory request and no limit instead. A resource the block
	// names is used as written.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// PriorityClassName sets the priority class of the Job and CronJob pods.
	// When unset, the pods take the priority class of the CR's API
	// Deployment, if any. An empty string opts out of that fallback and
	// renders no priority class.
	// +optional
	PriorityClassName *string `json:"priorityClassName,omitempty"`
}

// JobSpec is JobBaseSpec plus node placement. It configures every Job and
// CronJob pod of a CR, and each field it leaves unset falls back to the CR's
// API Deployment:
//
//   - Resources has no fallback; the operator defaults it per resource (see
//     JobBaseSpec.Resources).
//   - PriorityClassName: nil takes the Deployment's priorityClassName, and ""
//     opts out.
//   - NodeSelector and Tolerations: nil copies the Deployment's value, and an
//     empty map or list opts out.
//   - Affinity: nil copies only the Deployment's nodeAffinity, because the
//     pod (anti-)affinity terms target the API pods; an empty affinity opts
//     out.
//
// An empty value opts out of the fallback, the same way an empty
// topologySpreadConstraints list disables the Deployment's default spread.
type JobSpec struct {
	JobBaseSpec       `json:",inline"`
	NodePlacementSpec `json:",inline"`
}

// DeploymentSpec groups the pod-level knobs for the service API Deployment.
// Grouping them under spec.deployment keeps the CR spec root legible.
//
// The drain-window CEL rule mirrors the validating webhook: when one or both of
// the nil-preserving pointers is unset, the rule substitutes the same effective
// defaults the reconciler applies. The literals 5 and 30 must stay in sync with
// DefaultPreStopSleepSeconds and DefaultTerminationGracePeriodSeconds — the
// operator webhook applies the same effective defaults; kubebuilder/CEL rules
// cannot reference Go constants.
// +kubebuilder:validation:XValidation:rule="(has(self.preStopSleepSeconds) ? self.preStopSleepSeconds : 5) < (has(self.terminationGracePeriodSeconds) ? self.terminationGracePeriodSeconds : 30)",message="preStopSleepSeconds must be strictly less than terminationGracePeriodSeconds"
type DeploymentSpec struct {
	// Replicas is the desired number of service API pods.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	Replicas int32 `json:"replicas,omitempty"`

	// Resources defines the CPU and memory requests and limits for the
	// container. The operator never writes defaults into this field; it resolves
	// them when it renders the pod, per resource: a CPU the block names neither
	// as request nor as limit gets a 100m request and no limit, and a memory the
	// block names neither way gets one figure as both request and limit, sized
	// from the process and thread count the container runs (see the operator's
	// CRD reference). A resource the block names is used as written, and
	// anything else it sets is kept.
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
	// service API pods between SIGTERM and SIGKILL during rolling updates
	// Extend this to cover slow upstream token validation (LDAP/DB)
	// so in-flight requests finish before the kubelet forcibly kills the API process.
	// When nil, the reconciler omits the field from the pod template and the
	// Kubernetes default of 30s applies. Must be at least 10s when set.
	// +optional
	// +kubebuilder:validation:Minimum=10
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// PreStopSleepSeconds is the sleep duration (seconds) of the preStop
	// lifecycle hook, configured independently of the overall grace period
	// This covers the window between EndpointSlice removal and
	// kube-proxy/ingress-controller propagation so new requests stop arriving
	// before SIGTERM reaches the API process. When nil, the reconciler applies a default
	// of 5s. Zero is permitted to disable the sleep. The cross-field rule
	// preStopSleepSeconds < terminationGracePeriodSeconds is enforced by the
	// validating webhook to guarantee a non-zero drain window.
	// +optional
	// +kubebuilder:validation:Minimum=0
	PreStopSleepSeconds *int64 `json:"preStopSleepSeconds,omitempty"`

	// Strategy overrides the Deployment rollout strategy for the service API
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

	// PriorityClassName sets the priority class for service API pods.
	// When set, the operator passes the value through to the PodSpec, allowing
	// cluster administrators to control scheduling priority and preemption.
	// When unset, no priority class is configured and the cluster default applies.
	// +optional
	PriorityClassName *string `json:"priorityClassName,omitempty"`

	// NodePlacementSpec adds nodeSelector, tolerations and affinity, which the
	// operator renders onto the pod template verbatim.
	NodePlacementSpec `json:",inline"`
}

// Default sets the shared-type defaults on a DeploymentSpec in place: a
// zero-valued Replicas becomes DefaultReplicas. It never writes Resources: the
// reconcilers resolve container resources per resource when they render the
// pod (WithResourceDefaults), so a later change of the process count moves the
// memory with it. Operator webhooks call it so the replica default cannot drift
// across operators.
func (d *DeploymentSpec) Default() {
	if d.Replicas == 0 {
		d.Replicas = DefaultReplicas
	}
}

// AutoscalingSpec defines the parameters for horizontal pod autoscaling.
// +kubebuilder:validation:XValidation:rule="has(self.targetCPUUtilization) || has(self.targetMemoryUtilization)",message="at least one of targetCPUUtilization or targetMemoryUtilization must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
type AutoscalingSpec struct {
	// MinReplicas is the lower bound for the number of replicas.
	// Defaults to the API block's replica count if unset. The API
	// PodDisruptionBudget follows this bound: maxUnavailable: 1 at one
	// replica, so a drain can evict the last pod, and minAvailable: 1 above.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the upper bound for the number of replicas.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// TargetCPUUtilization is the target average CPU utilization (percentage).
	// The HPA measures it against the summed CPU requests of every container
	// in the API pod, so while it is set the webhook rejects a zero or
	// negative CPU request, or a limit the request would be copied from.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetCPUUtilization *int32 `json:"targetCPUUtilization,omitempty"`

	// TargetMemoryUtilization is the target average memory utilization (percentage).
	// The HPA measures it against the summed memory requests of every
	// container in the API pod, so while it is set the webhook rejects a zero
	// or negative memory request, or a limit the request would be copied from.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetMemoryUtilization *int32 `json:"targetMemoryUtilization,omitempty"`
}

// NetworkPolicySpec defines network isolation for the service API pods.
// When applied, the operator creates a NetworkPolicy that restricts ingress
// to the service API port from the specified sources and auto-derives egress rules for
// DNS, MariaDB (from database.ClusterRef), and Memcached (from cache.ClusterRef).
// +kubebuilder:validation:XValidation:rule="size(self.ingress) > 0",message="at least one ingress source must be specified"
type NetworkPolicySpec struct {
	// Ingress defines the sources allowed to reach the service API on its API port.
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
// to reach the service API pods on the API port.
type NetworkPolicyIngressSource struct {
	// NamespaceSelector selects namespaces from which traffic is allowed.
	// All pods in matching namespaces can reach the service on its API port
	// unless PodSelector further restricts the set. It is a full
	// metav1.LabelSelector, so set-based matchExpressions are supported in
	// addition to matchLabels.
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// PodSelector optionally restricts allowed traffic to pods matching
	// these labels within the selected namespaces. When set, only pods
	// matching both NamespaceSelector AND PodSelector can reach the service
	// (AND logic within a single peer). It is a full metav1.LabelSelector,
	// so set-based matchExpressions are supported in addition to matchLabels.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`
}

// LoggingSpec configures oslo.log output for the service API container.
// Exposed as an optional pointer field on the CR spec; the defaulting webhook
// materializes a baseline LoggingSpec when the pointer is nil so downstream
// reconciler code never sees a nil pointer (mirrors the UWSGISpec precedent).
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
	// {"sqlalchemy.engine": "WARNING", "myservice.middleware": "DEBUG"}.
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
