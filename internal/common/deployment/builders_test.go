// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"testing"

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// A zero-valued replica count (webhook-bypassed spec) must normalize to the
// shared default so the Deployment is never scaled to zero pods.
func TestEffectiveReplicas_NormalizesZero(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(EffectiveReplicas(&commonv1.DeploymentSpec{})).To(gomega.Equal(commonv1.DefaultReplicas))
	g.Expect(EffectiveReplicas(&commonv1.DeploymentSpec{Replicas: 5})).To(gomega.Equal(int32(5)))
}

// When autoscaling is set the Deployment replicas field is left unmanaged
// (nil) so the HPA owns the count and the operator does not fight it.
func TestDeploymentReplicas_NilUnderAutoscaling(t *testing.T) {
	g := gomega.NewWithT(t)

	spec := &commonv1.DeploymentSpec{Replicas: 3}
	g.Expect(DeploymentReplicas(spec, &commonv1.AutoscalingSpec{MaxReplicas: 5})).To(gomega.BeNil())
	g.Expect(DeploymentReplicas(spec, nil)).To(gomega.HaveValue(gomega.Equal(int32(3))))
}

func TestBuildPDB_MultiReplicaUsesMinAvailable(t *testing.T) {
	g := gomega.NewWithT(t)

	labels := map[string]string{"app.kubernetes.io/name": "keystone"}
	selector := map[string]string{"app.kubernetes.io/instance": "ks"}
	pdb := BuildPDB("ns", "ks", labels, selector, &commonv1.DeploymentSpec{Replicas: 3})

	g.Expect(pdb.Name).To(gomega.Equal("ks"))
	g.Expect(pdb.Namespace).To(gomega.Equal("ns"))
	g.Expect(pdb.Labels).To(gomega.Equal(labels))
	g.Expect(pdb.Spec.Selector.MatchLabels).To(gomega.Equal(selector))
	g.Expect(pdb.Spec.MinAvailable).To(gomega.HaveValue(gomega.Equal(intstr.FromInt32(1))))
	g.Expect(pdb.Spec.MaxUnavailable).To(gomega.BeNil())
}

// A single-replica deployment must use maxUnavailable=1: minAvailable=1 would
// block every voluntary eviction and deadlock node drains.
func TestBuildPDB_SingleReplicaUsesMaxUnavailable(t *testing.T) {
	g := gomega.NewWithT(t)

	pdb := BuildPDB("ns", "ks", nil, nil, &commonv1.DeploymentSpec{Replicas: 1})

	g.Expect(pdb.Spec.MinAvailable).To(gomega.BeNil())
	g.Expect(pdb.Spec.MaxUnavailable).To(gomega.HaveValue(gomega.Equal(intstr.FromInt32(1))))
}

// A zero-valued replica count normalizes to the default (3) before the PDB
// strategy branch, so a webhook-bypassed spec still gets minAvailable=1.
func TestBuildPDB_ZeroReplicasNormalized(t *testing.T) {
	g := gomega.NewWithT(t)

	pdb := BuildPDB("ns", "ks", nil, nil, &commonv1.DeploymentSpec{})

	g.Expect(pdb.Spec.MinAvailable).To(gomega.HaveValue(gomega.Equal(intstr.FromInt32(1))))
}

func TestBuildHPA_MinReplicasDefaultsToEffectiveReplicas(t *testing.T) {
	g := gomega.NewWithT(t)

	// Zero replicas (webhook-bypassed): minReplicas must default to the
	// normalized count, never to an invalid 0 the API server would reject.
	hpa := BuildHPA("ns", "ks", nil, &commonv1.DeploymentSpec{}, &commonv1.AutoscalingSpec{
		MaxReplicas:          10,
		TargetCPUUtilization: ptr.To(int32(80)),
	})

	g.Expect(hpa.Spec.MinReplicas).To(gomega.HaveValue(gomega.Equal(commonv1.DefaultReplicas)))
	g.Expect(hpa.Spec.MaxReplicas).To(gomega.Equal(int32(10)))
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(gomega.Equal("ks"))
	g.Expect(hpa.Spec.Metrics).To(gomega.HaveLen(1))
	g.Expect(hpa.Spec.Metrics[0].Resource.Name).To(gomega.Equal(corev1.ResourceCPU))
}

func TestBuildHPA_ExplicitMinAndBothMetrics(t *testing.T) {
	g := gomega.NewWithT(t)

	hpa := BuildHPA("ns", "ks", nil, &commonv1.DeploymentSpec{Replicas: 3}, &commonv1.AutoscalingSpec{
		MinReplicas:             ptr.To(int32(2)),
		MaxReplicas:             6,
		TargetCPUUtilization:    ptr.To(int32(70)),
		TargetMemoryUtilization: ptr.To(int32(85)),
	})

	g.Expect(hpa.Spec.MinReplicas).To(gomega.HaveValue(gomega.Equal(int32(2))))
	g.Expect(hpa.Spec.Metrics).To(gomega.HaveLen(2))
}

// The nil-spec fallbacks keep webhook-bypassed CRs safe: zero resources, the
// shared graceful-termination defaults, and the surge-before-remove strategy.
func TestPodKnobDefaults(t *testing.T) {
	g := gomega.NewWithT(t)
	spec := &commonv1.DeploymentSpec{}

	g.Expect(ContainerResources(spec)).To(gomega.Equal(corev1.ResourceRequirements{}))
	g.Expect(PriorityClassName(spec)).To(gomega.Equal(""))
	g.Expect(TerminationGracePeriodSeconds(spec)).To(gomega.Equal(commonv1.DefaultTerminationGracePeriodSeconds))
	g.Expect(PreStopSleepCommand(spec)).To(gomega.Equal([]string{"/bin/sh", "-c", "sleep 5"}))

	strategy := Strategy(spec)
	g.Expect(strategy.Type).To(gomega.Equal(appsv1.RollingUpdateDeploymentStrategyType))
	g.Expect(strategy.RollingUpdate.MaxUnavailable).To(gomega.HaveValue(gomega.Equal(intstr.FromInt32(0))))
	g.Expect(strategy.RollingUpdate.MaxSurge).To(gomega.HaveValue(gomega.Equal(intstr.FromInt32(1))))

	tscs := TopologySpreadConstraints(spec, map[string]string{"a": "b"})
	g.Expect(tscs).To(gomega.HaveLen(2))
	g.Expect(tscs[0].TopologyKey).To(gomega.Equal("topology.kubernetes.io/zone"))
	g.Expect(tscs[1].TopologyKey).To(gomega.Equal("kubernetes.io/hostname"))
}

// An explicit empty TSC slice disables the defaults; a custom strategy is
// deep-copied so callers cannot mutate the CR through the returned value.
func TestPodKnobOverrides(t *testing.T) {
	g := gomega.NewWithT(t)

	spec := &commonv1.DeploymentSpec{
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{},
		Strategy:                  &appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
		PreStopSleepSeconds:       ptr.To(int64(0)),
	}

	g.Expect(TopologySpreadConstraints(spec, nil)).To(gomega.BeEmpty())
	g.Expect(PreStopSleepCommand(spec)).To(gomega.Equal([]string{"/bin/sh", "-c", "sleep 0"}))

	strategy := Strategy(spec)
	g.Expect(strategy.Type).To(gomega.Equal(appsv1.RecreateDeploymentStrategyType))
	strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	g.Expect(spec.Strategy.Type).To(gomega.Equal(appsv1.RecreateDeploymentStrategyType),
		"mutating the returned strategy must not write through to the spec")
}

// TestRestrictedSecurityContext verifies the helper returns a SecurityContext
// with every PSS Restricted profile field set — including Capabilities
// dropping ALL, without which clusters enforcing the profile reject the pod.
func TestRestrictedSecurityContext(t *testing.T) {
	g := gomega.NewWithT(t)

	sc := RestrictedSecurityContext()

	g.Expect(sc.AllowPrivilegeEscalation).To(gomega.HaveValue(gomega.BeFalse()))
	g.Expect(sc.RunAsNonRoot).To(gomega.HaveValue(gomega.BeTrue()))
	g.Expect(sc.RunAsUser).To(gomega.HaveValue(gomega.Equal(OpenStackUID)))
	g.Expect(sc.RunAsGroup).To(gomega.HaveValue(gomega.Equal(OpenStackUID)))
	g.Expect(sc.ReadOnlyRootFilesystem).To(gomega.HaveValue(gomega.BeTrue()))
	g.Expect(sc.Capabilities).NotTo(gomega.BeNil())
	g.Expect(sc.Capabilities.Drop).To(gomega.ContainElement(corev1.Capability("ALL")))
	g.Expect(sc.SeccompProfile).NotTo(gomega.BeNil())
	g.Expect(sc.SeccompProfile.Type).To(gomega.Equal(corev1.SeccompProfileTypeRuntimeDefault))
}
