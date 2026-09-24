// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package types

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Memory sizing constants for the service containers. They are estimates that
// the VPA run of #1097 calibrates. They start from the working sets of CI job
// 107440936441 (e2e-controlplane): API pods at two processes ran at 156 MiB
// (Placement) to 383 MiB (Neutron), single-process Neutron workers at 266 and
// 281 MiB. A nova-api worker settles near 130 MiB. At the default two
// processes the formula keeps the former 512Mi limit. The vars are exposed
// only through DefaultMemoryPerProcess and MemoryForProcesses, which return
// copies, so no caller can mutate a shared default.
var (
	// memoryBase is the part of the figure that does not grow with the
	// process count: the interpreter, the imported service code and the
	// master process.
	memoryBase = resource.MustParse("224Mi")
	// memoryPerExtraThread is what each thread beyond the first adds to a
	// process.
	memoryPerExtraThread = resource.MustParse("32Mi")
	// defaultMemoryPerProcess is what one single-threaded service process
	// adds on top of memoryBase.
	defaultMemoryPerProcess = resource.MustParse("144Mi")
)

// DefaultMemoryPerProcess returns a copy of the memory one service process adds (144Mi).
func DefaultMemoryPerProcess() resource.Quantity { return defaultMemoryPerProcess.DeepCopy() }

// MemoryForProcesses returns 224Mi + processes × (perProcess + (threads-1) × 32Mi),
// clamping processes and threads below 1 to 1.
//
// The result is in canonical form, the same value resource.MustParse returns
// for its String(), so it compares equal to a parsed literal such as "1Gi"
// and renders the same way in a Pod template.
func MemoryForProcesses(perProcess resource.Quantity, processes, threads int32) resource.Quantity {
	processes = max(processes, 1)
	threads = max(threads, 1)
	perProcessBytes := perProcess.Value() + int64(threads-1)*memoryPerExtraThread.Value()
	total := memoryBase.Value() + int64(processes)*perProcessBytes
	return resource.MustParse(resource.NewQuantity(total, resource.BinarySI).String())
}

// WithResourceDefaults applies the per-resource default rule to a copy of rr
// (nil is empty) and returns the copy:
//
//   - a CPU the block names neither as request nor as limit gets a request of
//     DefaultCPURequest() and no limit;
//   - a memory the block names neither way gets memory as both request and
//     limit;
//   - anything else is kept as written: a resource the block names in either
//     map (a zero quantity included), other resources such as
//     ephemeral-storage and hugepages-*, and Claims.
//
// A request is never added beside a limit the block sets for the same
// resource. Kubernetes already defaults that request to the limit, and an
// added request would silently lower it.
//
// A zero memory falls back to MemoryForProcesses(DefaultMemoryPerProcess(), 1, 1).
func WithResourceDefaults(rr *corev1.ResourceRequirements, memory resource.Quantity) corev1.ResourceRequirements {
	var out corev1.ResourceRequirements
	if rr != nil {
		out = *rr.DeepCopy()
	}
	if memory.IsZero() {
		memory = MemoryForProcesses(DefaultMemoryPerProcess(), 1, 1)
	}
	if !NamesResource(out, corev1.ResourceCPU) {
		if out.Requests == nil {
			out.Requests = corev1.ResourceList{}
		}
		out.Requests[corev1.ResourceCPU] = DefaultCPURequest()
	}
	if !NamesResource(out, corev1.ResourceMemory) {
		if out.Requests == nil {
			out.Requests = corev1.ResourceList{}
		}
		if out.Limits == nil {
			out.Limits = corev1.ResourceList{}
		}
		out.Requests[corev1.ResourceMemory] = memory.DeepCopy()
		out.Limits[corev1.ResourceMemory] = memory.DeepCopy()
	}
	return out
}

// NamesResource reports whether rr names the resource as a request or as a
// limit. A zero quantity counts as named. A default for a resource belongs
// only in a block that does not name it: a request added beside a limit the
// block sets would silently lower the request the API server defaults to that
// limit.
func NamesResource(rr corev1.ResourceRequirements, name corev1.ResourceName) bool {
	_, inRequests := rr.Requests[name]
	_, inLimits := rr.Limits[name]
	return inRequests || inLimits
}
