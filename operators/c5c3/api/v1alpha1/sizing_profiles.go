// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// minimalServiceCPURequest is the CPU request of every service pod and Job
// under the Minimal profile: the median of the services' CPU targets in the
// e2e-controlplane job of CI run 36339033005, rounded up to 5m.
const minimalServiceCPURequest = "15m"

// BuiltinSizing returns the sizing of the built-in profile name, as a fresh
// value on every call so a caller can never change the next caller's result.
// An empty or unknown name returns Standard's sizing.
//
// Standard reproduces the replica counts and the database volume size a
// ControlPlane without sizing always projected, and sets nothing else: every
// other value stays with the child operators' render-time defaults and the
// backing-service operators' defaults. Minimal sizes every component for a
// single small node. Its figures come from the VPA recommendations of CI run
// 36339033005 (2026-09-27, docs/reference/testing/sizing-calibration.md) and
// keep the full stack within one 4 vCPU / 16 GiB node, which the node budget
// gate of the e2e-controlplane job (hack/ci-check-node-budget.sh) checks.
func BuiltinSizing(name SizingProfileName) SizingSpec {
	if name == SizingProfileMinimal {
		return minimalSizing()
	}
	return standardSizing()
}

// standardSizing is the Standard profile: today's replica counts and database
// volume size, nothing else.
func standardSizing() SizingSpec {
	api := func() *APISizingSpec {
		return &APISizingSpec{DeploymentSizingSpec: deploymentReplicas(commonv1.DefaultReplicas)}
	}
	component := func() *DeploymentSizingSpec {
		d := deploymentReplicas(novav1alpha1.DefaultComponentReplicas)
		return &d
	}
	worker := func() *WorkerSizingSpec {
		return &WorkerSizingSpec{DeploymentSizingSpec: deploymentReplicas(novav1alpha1.DefaultComponentReplicas)}
	}
	cinderScheduler := deploymentReplicas(cinderv1alpha1.DefaultSchedulerReplicas)
	return SizingSpec{
		Database: &DatabaseSizingSpec{
			Replicas:    ptr.To(commonv1.DefaultReplicas),
			StorageSize: commonv1.DatabaseStorageSizeDefault,
		},
		Cache:     &CacheSizingSpec{Replicas: ptr.To(commonv1.DefaultReplicas)},
		Messaging: &ScaledSizingSpec{Replicas: ptr.To(commonv1.DefaultReplicas)},
		Keystone:  &KeystoneSizingSpec{API: api()},
		Horizon: &HorizonSizingSpec{
			API: &HorizonAPISizingSpec{DeploymentSizingSpec: deploymentReplicas(commonv1.DefaultReplicas)},
		},
		Glance:    &APIServiceSizingSpec{API: api()},
		Placement: &APIServiceSizingSpec{API: api()},
		Barbican:  &APIServiceSizingSpec{API: api()},
		Neutron: &NeutronSizingSpec{
			API:     api(),
			Workers: &WorkersSizingSpec{ScaledSizingSpec: ScaledSizingSpec{Replicas: ptr.To(commonv1.DefaultReplicas)}},
		},
		Cinder: &CinderSizingSpec{API: api(), Scheduler: &cinderScheduler},
		Nova: &NovaSizingSpec{
			API:          api(),
			Metadata:     &MetadataAPISizingSpec{DeploymentSizingSpec: *component()},
			Scheduler:    worker(),
			Conductor:    worker(),
			ConsoleProxy: component(),
		},
	}
}

// minimalSizing is the Minimal profile: one replica, one process and one
// thread per component, the measured Minimal CPU request for the service pods,
// and the measured CPU and memory of the backing services, never below the
// memory floor each one's configuration depends on (1Gi database, 96Mi cache,
// 1Gi broker, 64Mi secret store). Service memory stays with the child
// operators' per-process formula, so a lower process count lowers it.
func minimalSizing() SizingSpec {
	api := func() *APISizingSpec {
		return &APISizingSpec{DeploymentSizingSpec: minimalDeployment(), ProcessSizingSpec: singleProcess()}
	}
	worker := func() *WorkerSizingSpec {
		return &WorkerSizingSpec{DeploymentSizingSpec: minimalDeployment(), Workers: ptr.To[int32](1)}
	}
	deployment := func() *DeploymentSizingSpec {
		d := minimalDeployment()
		return &d
	}
	pinned := func() *PinnedDeploymentSizingSpec {
		return &PinnedDeploymentSizingSpec{PinnedSizingSpec: PinnedSizingSpec{ContainerSizingSpec: cpuRequest(minimalServiceCPURequest)}}
	}
	jobs := func() *JobSizingSpec {
		return &JobSizingSpec{ContainerSizingSpec: cpuRequest(minimalServiceCPURequest)}
	}
	return SizingSpec{
		Database: &DatabaseSizingSpec{
			Replicas:         ptr.To[int32](1),
			StorageSize:      "512Mi",
			PinnedSizingSpec: PinnedSizingSpec{ContainerSizingSpec: memoryBound("65m", "1Gi")},
		},
		Cache: &CacheSizingSpec{
			Replicas:            ptr.To[int32](1),
			ContainerSizingSpec: memoryBound("15m", "96Mi"),
		},
		// The RabbitMQ Cluster Operator (v2.23.0, removeHeadroom) writes
		// total_memory_available_override_value = limit - limit/5 for a
		// broker with a memory limit, and RabbitMQ 4.3.4 raises its memory
		// alarm at 0.6 of that value. 512Mi gave a 245.8 MiB watermark against
		// an idle footprint of 236 MiB (lab, 2026-10-07, 32 empty queues) and
		// blocked every publisher without a backlog (#1298). 1Gi gives
		// 491.5 MiB, twice the idle footprint. The figure must keep the
		// watermark above twice the idle footprint
		// (TestBuiltinSizing_MinimalBrokerHeadroom).
		Messaging: &ScaledSizingSpec{
			Replicas:         ptr.To[int32](1),
			PinnedSizingSpec: PinnedSizingSpec{ContainerSizingSpec: memoryBound("815m", "1Gi")},
		},
		SecretStore: ptr.To(memoryBound("35m", "64Mi")),
		Keystone:    &KeystoneSizingSpec{API: api(), Jobs: jobs()},
		Horizon:     &HorizonSizingSpec{API: &HorizonAPISizingSpec{DeploymentSizingSpec: minimalDeployment()}},
		Glance:      &APIServiceSizingSpec{API: api(), Jobs: jobs()},
		Placement:   &APIServiceSizingSpec{API: api(), Jobs: jobs()},
		Barbican:    &APIServiceSizingSpec{API: api(), Jobs: jobs()},
		Neutron: &NeutronSizingSpec{
			API:     api(),
			Workers: &WorkersSizingSpec{ScaledSizingSpec: minimalDeployment().ScaledSizingSpec},
			Jobs:    jobs(),
		},
		Cinder: &CinderSizingSpec{
			API:       api(),
			Scheduler: deployment(),
			Volume:    pinned(),
			Backup:    pinned(),
			Jobs:      jobs(),
		},
		Nova: &NovaSizingSpec{
			API: api(),
			Metadata: &MetadataAPISizingSpec{
				DeploymentSizingSpec: minimalDeployment(),
				ProcessSizingSpec:    singleProcess(),
			},
			Scheduler:    worker(),
			Conductor:    worker(),
			ConsoleProxy: deployment(),
			Jobs:         jobs(),
		},
	}
}

// deploymentReplicas returns a Deployment sizing that sets the replica count
// alone.
func deploymentReplicas(replicas int32) DeploymentSizingSpec {
	return DeploymentSizingSpec{ScaledSizingSpec: ScaledSizingSpec{Replicas: ptr.To(replicas)}}
}

// minimalDeployment is the Minimal sizing of a service Deployment: one
// replica with the Minimal service CPU request.
func minimalDeployment() DeploymentSizingSpec {
	return DeploymentSizingSpec{ScaledSizingSpec: ScaledSizingSpec{
		Replicas:         ptr.To[int32](1),
		PinnedSizingSpec: PinnedSizingSpec{ContainerSizingSpec: cpuRequest(minimalServiceCPURequest)},
	}}
}

// singleProcess is one uWSGI process with one thread.
func singleProcess() ProcessSizingSpec {
	return ProcessSizingSpec{Processes: ptr.To[int32](1), Threads: ptr.To[int32](1)}
}

// cpuRequest returns a container sizing that requests cpu and nothing else.
func cpuRequest(cpu string) ContainerSizingSpec {
	return ContainerSizingSpec{Resources: &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
	}}
}

// memoryBound returns a container sizing that requests cpu and memory and
// limits memory to its request.
func memoryBound(cpu, memory string) ContainerSizingSpec {
	return ContainerSizingSpec{Resources: &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)},
	}}
}
