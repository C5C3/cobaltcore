// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// MergeSizing overlays override on base, one value at a time, and returns a
// new value that aliases neither input:
//
//   - replicas, processes, threads, workers, priorityClassName and
//     storageSize: a set override wins.
//   - resources: every request and limit the override names replaces or adds
//     the base entry of that resource; override claims replace when non-nil.
//   - nodeSelector, tolerations and spreadConstraints: a non-empty override
//     replaces the base. An empty one inherits, because the defaulting
//     webhook's JSON round trip drops an empty map or list, so "empty" cannot
//     mean "clear".
//   - autoscaling: a set override replaces the whole block, since its bounds
//     and targets only make sense together.
//   - blocks: a nil override keeps the base, a nil base takes the override,
//     and two set blocks merge recursively.
func MergeSizing(base, override SizingSpec) SizingSpec {
	out := base.DeepCopy()
	o := override.DeepCopy()
	mergePlacement(&out.PodPlacementSpec, &o.PodPlacementSpec)
	out.Database = mergeBlock(out.Database, o.Database, mergeDatabase)
	out.Cache = mergeBlock(out.Cache, o.Cache, mergeCache)
	out.Messaging = mergeBlock(out.Messaging, o.Messaging, mergeScaled)
	out.SecretStore = mergeBlock(out.SecretStore, o.SecretStore, mergeContainer)
	out.Keystone = mergeBlock(out.Keystone, o.Keystone, mergeKeystone)
	out.Horizon = mergeBlock(out.Horizon, o.Horizon, mergeHorizon)
	out.Glance = mergeBlock(out.Glance, o.Glance, mergeAPIService)
	out.Placement = mergeBlock(out.Placement, o.Placement, mergeAPIService)
	out.Barbican = mergeBlock(out.Barbican, o.Barbican, mergeAPIService)
	out.Neutron = mergeBlock(out.Neutron, o.Neutron, mergeNeutron)
	out.Cinder = mergeBlock(out.Cinder, o.Cinder, mergeCinder)
	out.Nova = mergeBlock(out.Nova, o.Nova, mergeNova)
	return *out
}

// EffectiveSizingBase returns the built-in profile a ControlPlane's sizing
// starts from: spec.sizing.profile when set, else the base of profile when it
// is non-nil, else Standard.
func EffectiveSizingBase(cp *ControlPlane, profile *SizingProfile) SizingProfileName {
	if s := cp.Spec.Sizing; s != nil && s.Profile != "" {
		return s.Profile
	}
	if profile != nil && profile.Spec.Base != "" {
		return profile.Spec.Base
	}
	return SizingProfileStandard
}

// ResolveSizing returns the effective sizing of cp: the built-in base
// profile (EffectiveSizingBase), overlaid by the values of profile when it is
// non-nil, overlaid by cp's own spec.sizing values. profile is the
// SizingProfile spec.sizing.profileRef names; a nil profile resolves cp's
// values over the base alone. A nil spec.sizing resolves to Standard.
func ResolveSizing(cp *ControlPlane, profile *SizingProfile) SizingSpec {
	var profileValues, cpValues SizingSpec
	if profile != nil {
		profileValues = profile.Spec.SizingSpec
	}
	if cp.Spec.Sizing != nil {
		cpValues = cp.Spec.Sizing.SizingSpec
	}
	return MergeSizing(MergeSizing(BuiltinSizing(EffectiveSizingBase(cp, profile)), profileValues), cpValues)
}

// mergeBlock merges two optional blocks that MergeSizing already owns: a nil
// override keeps the base, a nil base takes the override, and two set blocks
// merge into base.
func mergeBlock[T any](base, override *T, merge func(b, o *T)) *T {
	switch {
	case override == nil:
		return base
	case base == nil:
		return override
	}
	merge(base, override)
	return base
}

// mergeValue returns override when it is set, base otherwise.
func mergeValue[T any](base, override *T) *T {
	if override != nil {
		return override
	}
	return base
}

func mergeResources(base, override *corev1.ResourceRequirements) *corev1.ResourceRequirements {
	return mergeBlock(base, override, func(b, o *corev1.ResourceRequirements) {
		b.Requests = mergeResourceList(b.Requests, o.Requests)
		b.Limits = mergeResourceList(b.Limits, o.Limits)
		if o.Claims != nil {
			b.Claims = o.Claims
		}
	})
}

func mergeResourceList(base, override corev1.ResourceList) corev1.ResourceList {
	if len(override) == 0 {
		return base
	}
	if base == nil {
		base = corev1.ResourceList{}
	}
	for name, q := range override {
		base[name] = q
	}
	return base
}

func mergePlacement(b, o *PodPlacementSpec) {
	if len(o.NodeSelector) > 0 {
		b.NodeSelector = o.NodeSelector
	}
	if len(o.Tolerations) > 0 {
		b.Tolerations = o.Tolerations
	}
	b.PriorityClassName = mergeValue(b.PriorityClassName, o.PriorityClassName)
}

func mergeContainer(b, o *ContainerSizingSpec) {
	b.Resources = mergeResources(b.Resources, o.Resources)
}

func mergePinned(b, o *PinnedSizingSpec) {
	mergeContainer(&b.ContainerSizingSpec, &o.ContainerSizingSpec)
	mergePlacement(&b.PodPlacementSpec, &o.PodPlacementSpec)
}

func mergeScaled(b, o *ScaledSizingSpec) {
	b.Replicas = mergeValue(b.Replicas, o.Replicas)
	mergePinned(&b.PinnedSizingSpec, &o.PinnedSizingSpec)
}

func mergeDeployment(b, o *DeploymentSizingSpec) {
	mergeScaled(&b.ScaledSizingSpec, &o.ScaledSizingSpec)
	if len(o.SpreadConstraints) > 0 {
		b.SpreadConstraints = o.SpreadConstraints
	}
}

func mergeProcess(b, o *ProcessSizingSpec) {
	b.Processes = mergeValue(b.Processes, o.Processes)
	b.Threads = mergeValue(b.Threads, o.Threads)
}

func mergeAPI(b, o *APISizingSpec) {
	mergeDeployment(&b.DeploymentSizingSpec, &o.DeploymentSizingSpec)
	mergeProcess(&b.ProcessSizingSpec, &o.ProcessSizingSpec)
	b.Autoscaling = mergeValue(b.Autoscaling, o.Autoscaling)
}

func mergeHorizonAPI(b, o *HorizonAPISizingSpec) {
	mergeDeployment(&b.DeploymentSizingSpec, &o.DeploymentSizingSpec)
	b.Autoscaling = mergeValue(b.Autoscaling, o.Autoscaling)
}

func mergeMetadataAPI(b, o *MetadataAPISizingSpec) {
	mergeDeployment(&b.DeploymentSizingSpec, &o.DeploymentSizingSpec)
	mergeProcess(&b.ProcessSizingSpec, &o.ProcessSizingSpec)
}

func mergeWorker(b, o *WorkerSizingSpec) {
	mergeDeployment(&b.DeploymentSizingSpec, &o.DeploymentSizingSpec)
	b.Workers = mergeValue(b.Workers, o.Workers)
}

func mergeJob(b, o *JobSizingSpec) {
	mergeContainer(&b.ContainerSizingSpec, &o.ContainerSizingSpec)
	b.PriorityClassName = mergeValue(b.PriorityClassName, o.PriorityClassName)
}

func mergeDatabase(b, o *DatabaseSizingSpec) {
	b.Replicas = mergeValue(b.Replicas, o.Replicas)
	if o.StorageSize != "" {
		b.StorageSize = o.StorageSize
	}
	mergePinned(&b.PinnedSizingSpec, &o.PinnedSizingSpec)
}

func mergeCache(b, o *CacheSizingSpec) {
	b.Replicas = mergeValue(b.Replicas, o.Replicas)
	mergeContainer(&b.ContainerSizingSpec, &o.ContainerSizingSpec)
}

func mergeKeystone(b, o *KeystoneSizingSpec) {
	b.API = mergeBlock(b.API, o.API, mergeAPI)
	b.Jobs = mergeBlock(b.Jobs, o.Jobs, mergeJob)
	b.FederationProxy = mergeBlock(b.FederationProxy, o.FederationProxy, mergeContainer)
}

func mergeHorizon(b, o *HorizonSizingSpec) {
	b.API = mergeBlock(b.API, o.API, mergeHorizonAPI)
}

func mergeAPIService(b, o *APIServiceSizingSpec) {
	b.API = mergeBlock(b.API, o.API, mergeAPI)
	b.Jobs = mergeBlock(b.Jobs, o.Jobs, mergeJob)
}

func mergeNeutron(b, o *NeutronSizingSpec) {
	b.API = mergeBlock(b.API, o.API, mergeAPI)
	b.Workers = mergeBlock(b.Workers, o.Workers, mergeScaled)
	b.Jobs = mergeBlock(b.Jobs, o.Jobs, mergeJob)
}

func mergeCinder(b, o *CinderSizingSpec) {
	b.API = mergeBlock(b.API, o.API, mergeAPI)
	b.Scheduler = mergeBlock(b.Scheduler, o.Scheduler, mergeDeployment)
	b.Volume = mergeBlock(b.Volume, o.Volume, mergePinned)
	b.Backup = mergeBlock(b.Backup, o.Backup, mergePinned)
	b.Jobs = mergeBlock(b.Jobs, o.Jobs, mergeJob)
}

func mergeNova(b, o *NovaSizingSpec) {
	b.API = mergeBlock(b.API, o.API, mergeAPI)
	b.Metadata = mergeBlock(b.Metadata, o.Metadata, mergeMetadataAPI)
	b.Scheduler = mergeBlock(b.Scheduler, o.Scheduler, mergeWorker)
	b.Conductor = mergeBlock(b.Conductor, o.Conductor, mergeWorker)
	b.ConsoleProxy = mergeBlock(b.ConsoleProxy, o.ConsoleProxy, mergeDeployment)
	b.Jobs = mergeBlock(b.Jobs, o.Jobs, mergeJob)
}
