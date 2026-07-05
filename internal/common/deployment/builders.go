// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// EffectiveReplicas returns the desired API replica count, normalizing a
// zero-valued spec.deployment.replicas to commonv1.DefaultReplicas. The
// mutating webhooks fill replicas=3 on admission and the CRDs carry a
// +kubebuilder:default=3 on the nested field, but neither fires for a CR that
// omitted the spec.deployment block (Kubernetes does not descend into an
// absent object to materialize the nested leaf default) or for any path that
// bypasses the webhook. Left unnormalized, a zero would make
// DeploymentReplicas scale the Deployment to zero pods. Every replica
// consumer (DeploymentReplicas, BuildPDB, BuildHPA) routes through this
// single point so the default is applied consistently.
func EffectiveReplicas(spec *commonv1.DeploymentSpec) int32 {
	if spec.Replicas == 0 {
		return commonv1.DefaultReplicas
	}
	return spec.Replicas
}

// DeploymentReplicas returns the desired .spec.replicas for the API
// Deployment. When autoscaling is set, it returns nil so the field is left
// unmanaged and the HorizontalPodAutoscaler owns the replica count; otherwise
// it returns the effective replica count. Pinning replicas while an HPA also
// targets the Deployment causes the operator and the HPA to fight over the
// field, and each write re-triggers reconciliation in a scale-up/scale-down
// loop (issue #462). EnsureDeployment preserves the live count when this is
// nil.
func DeploymentReplicas(spec *commonv1.DeploymentSpec, autoscaling *commonv1.AutoscalingSpec) *int32 {
	if autoscaling != nil {
		return nil
	}
	return ptr.To(EffectiveReplicas(spec))
}

// BuildPDB constructs the desired PDB for the API deployment. It branches on
// the effective replica count — so a zero-valued spec.deployment.replicas
// normalizes to the default, matching the Deployment's own replica count —
// rather than the raw spec value. When the effective count is > 1,
// minAvailable=1 guarantees at least one pod remains during voluntary
// disruptions. When it is 1, maxUnavailable=1 is used instead to avoid drain
// deadlock (a PDB with minAvailable=1 on a single-replica deployment would
// block all evictions).
func BuildPDB(namespace, name string, labels, selector map[string]string, spec *commonv1.DeploymentSpec) *policyv1.PodDisruptionBudget {
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: selector,
			},
		},
	}

	if EffectiveReplicas(spec) > 1 {
		minAvailable := intstr.FromInt32(1)
		pdb.Spec.MinAvailable = &minAvailable
	} else {
		maxUnavailable := intstr.FromInt32(1)
		pdb.Spec.MaxUnavailable = &maxUnavailable
	}

	return pdb
}

// BuildHPA constructs the desired HorizontalPodAutoscaler for the API
// deployment. MinReplicas defaults to the effective spec.deployment.replicas
// when autoscaling.minReplicas is not set — routing through EffectiveReplicas
// normalizes a zero-valued (webhook-bypassed) count to the default, so a
// bypassed spec never yields an invalid minReplicas=0 the API server would
// reject. Metrics are added for CPU and/or memory utilization based on the
// autoscaling spec. name is used for both the HPA and its scale target
// Deployment (the shared sub-resource naming convention).
func BuildHPA(namespace, name string, labels map[string]string, spec *commonv1.DeploymentSpec, autoscaling *commonv1.AutoscalingSpec) *autoscalingv2.HorizontalPodAutoscaler {
	minReplicas := autoscaling.MinReplicas
	if minReplicas == nil {
		defaultMin := EffectiveReplicas(spec)
		minReplicas = &defaultMin
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       name,
			},
			MinReplicas: minReplicas,
			MaxReplicas: autoscaling.MaxReplicas,
		},
	}

	if autoscaling.TargetCPUUtilization != nil {
		hpa.Spec.Metrics = append(hpa.Spec.Metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: autoscaling.TargetCPUUtilization,
				},
			},
		})
	}

	if autoscaling.TargetMemoryUtilization != nil {
		hpa.Spec.Metrics = append(hpa.Spec.Metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceMemory,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: autoscaling.TargetMemoryUtilization,
				},
			},
		})
	}

	return hpa
}

// ContainerResources returns the ResourceRequirements for the API container.
// It dereferences spec.Resources if set, falling back to a zero value if nil
// (safe fallback for CRs that bypassed the webhook, e.g. pre-existing CRs
// during operator upgrade).
func ContainerResources(spec *commonv1.DeploymentSpec) corev1.ResourceRequirements {
	if spec.Resources != nil {
		return *spec.Resources
	}
	return corev1.ResourceRequirements{}
}

// TopologySpreadConstraints returns the topology spread constraints for the
// API pods. If spec.TopologySpreadConstraints is non-nil, those are used
// verbatim (an empty slice disables defaults). Otherwise, two default
// constraints are injected: one zone-spread and one hostname-spread, both
// with ScheduleAnyway to distribute pods across zones and nodes. selector is
// the pod selector label set the defaults target.
func TopologySpreadConstraints(spec *commonv1.DeploymentSpec, selector map[string]string) []corev1.TopologySpreadConstraint {
	if spec.TopologySpreadConstraints != nil {
		return spec.TopologySpreadConstraints
	}
	ls := &metav1.LabelSelector{MatchLabels: selector}
	return []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     ls,
		},
		{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     ls,
		},
	}
}

// PriorityClassName returns the priority class name for the API pods. If
// spec.PriorityClassName is set, that value is used. Otherwise, an empty
// string is returned, leaving the cluster default in effect.
func PriorityClassName(spec *commonv1.DeploymentSpec) string {
	if spec.PriorityClassName != nil {
		return *spec.PriorityClassName
	}
	return ""
}

// TerminationGracePeriodSeconds returns the PodSpec
// TerminationGracePeriodSeconds value. When
// spec.TerminationGracePeriodSeconds is nil (existing CR, pre-upgrade), it
// falls back to commonv1.DefaultTerminationGracePeriodSeconds, the shared
// constant that the validating webhooks also resolve against for cross-field
// arithmetic. Routing both sides through the same constant prevents silent
// drift.
func TerminationGracePeriodSeconds(spec *commonv1.DeploymentSpec) int64 {
	if spec.TerminationGracePeriodSeconds != nil {
		return *spec.TerminationGracePeriodSeconds
	}
	return commonv1.DefaultTerminationGracePeriodSeconds
}

// PreStopSleepCommand returns the preStop exec command. When
// spec.PreStopSleepSeconds is nil, it falls back to
// commonv1.DefaultPreStopSleepSeconds, the shared constant that the
// validating webhooks also resolve against for cross-field arithmetic. Zero
// is a permitted opt-out value and emits "sleep 0" verbatim. Routing both
// sides through the same constant prevents silent drift.
func PreStopSleepCommand(spec *commonv1.DeploymentSpec) []string {
	seconds := commonv1.DefaultPreStopSleepSeconds
	if spec.PreStopSleepSeconds != nil {
		seconds = *spec.PreStopSleepSeconds
	}
	return []string{"/bin/sh", "-c", fmt.Sprintf("sleep %d", seconds)}
}

// Strategy returns the Deployment rollout strategy. When spec.Strategy is
// non-nil, it is returned verbatim (a deep copy, so callers cannot mutate the
// CR). Otherwise, a RollingUpdate strategy with MaxUnavailable=0 and
// MaxSurge=1 is synthesized so available capacity never drops below
// spec.replicas during a rolling image-tag patch — the default
// surge-before-remove behavior.
func Strategy(spec *commonv1.DeploymentSpec) appsv1.DeploymentStrategy {
	if spec.Strategy != nil {
		return *spec.Strategy.DeepCopy()
	}
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		},
	}
}
