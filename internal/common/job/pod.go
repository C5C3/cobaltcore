// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package job

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// PodSettings is the resolved pod-level configuration of a Job or CronJob pod.
// The zero value renders no resources, no priority class and no placement.
type PodSettings struct {
	// Resources is set on every container and init container of the pod.
	Resources corev1.ResourceRequirements
	// PriorityClassName is the pod priority class; empty renders none.
	PriorityClassName string
	// Placement is rendered through deployment.ApplyNodePlacement.
	Placement commonv1.NodePlacementSpec
}

// ResolvePodSettings resolves the settings of a Job or CronJob pod from the
// CR's spec.jobs block (nil is empty) and its API Deployment block (nil means
// no fallback), per the rule commonv1.JobSpec documents:
//
//   - Resources: the per-resource rule of commonv1.WithResourceDefaults with
//     the figure of one single-threaded process (a 100m CPU request, 368Mi
//     memory as request and limit);
//   - PriorityClassName: spec's value when the pointer is non-nil, "" included,
//     else the fallback's, else none;
//   - NodeSelector and Tolerations: spec's value when non-nil, an empty map
//     or slice included, else a copy of the fallback's;
//   - Affinity: spec's value when non-nil, else only the fallback's
//     nodeAffinity. The pod (anti-)affinity terms target the API pods and do
//     not carry over.
//
// Every returned value is a copy, so writing to it never reaches either CR
// block.
func ResolvePodSettings(spec *commonv1.JobSpec, fallback *commonv1.DeploymentSpec) PodSettings {
	s := resolvePodSettings(spec, fallback)
	s.Resources = commonv1.WithResourceDefaults(jobResources(spec), resource.Quantity{})
	return s
}

// ResolvePodSettingsWithRequestFloor applies the rule of ResolvePodSettings,
// but resolves Resources through commonv1.WithRequestFloor: a 100m CPU and a
// 256Mi memory request and no limit. It is for the Jobs whose working set
// grows with the data they process (the OVN backup, Neutron's
// ovn-db-sync-util), which a default limit would OOM-kill once the logical
// model outgrew it.
func ResolvePodSettingsWithRequestFloor(spec *commonv1.JobSpec, fallback *commonv1.DeploymentSpec) PodSettings {
	s := resolvePodSettings(spec, fallback)
	s.Resources = commonv1.WithRequestFloor(jobResources(spec))
	return s
}

// jobResources returns the resources block of spec, nil when spec is nil.
func jobResources(spec *commonv1.JobSpec) *corev1.ResourceRequirements {
	if spec == nil {
		return nil
	}
	return spec.Resources
}

// resolvePodSettings resolves the priority class and the placement, which both
// resolvers share.
func resolvePodSettings(spec *commonv1.JobSpec, fallback *commonv1.DeploymentSpec) PodSettings {
	if spec == nil {
		spec = &commonv1.JobSpec{}
	}
	if fallback == nil {
		fallback = &commonv1.DeploymentSpec{}
	}
	var s PodSettings

	switch {
	case spec.PriorityClassName != nil:
		s.PriorityClassName = *spec.PriorityClassName
	case fallback.PriorityClassName != nil:
		s.PriorityClassName = *fallback.PriorityClassName
	}

	nodeSelector := spec.NodeSelector
	if nodeSelector == nil {
		nodeSelector = fallback.NodeSelector
	}
	tolerations := spec.Tolerations
	if tolerations == nil {
		tolerations = fallback.Tolerations
	}
	affinity := spec.Affinity
	if affinity == nil && fallback.Affinity != nil && fallback.Affinity.NodeAffinity != nil {
		affinity = &corev1.Affinity{NodeAffinity: fallback.Affinity.NodeAffinity}
	}
	placement := commonv1.NodePlacementSpec{
		NodeSelector: nodeSelector,
		Tolerations:  tolerations,
		Affinity:     affinity,
	}
	s.Placement = *placement.DeepCopy()
	return s
}

// Apply sets PriorityClassName and the placement on ps, and a copy of
// Resources on every container and init container. A nil ps is a no-op.
func (s PodSettings) Apply(ps *corev1.PodSpec) {
	if ps == nil {
		return
	}
	ps.PriorityClassName = s.PriorityClassName
	deployment.ApplyNodePlacement(ps, &s.Placement)
	for i := range ps.InitContainers {
		ps.InitContainers[i].Resources = *s.Resources.DeepCopy()
	}
	for i := range ps.Containers {
		ps.Containers[i].Resources = *s.Resources.DeepCopy()
	}
}
