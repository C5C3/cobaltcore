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
	if memory.IsZero() {
		memory = MemoryForProcesses(DefaultMemoryPerProcess(), 1, 1)
	}
	return withDefaults(rr, DefaultCPURequest(), memory)
}

// Sidecar sizing constants: the figures WithSidecarResourceDefaults gives a
// fixed-budget sidecar (the Keystone federation proxy, the Glance cache
// maintenance loop). They are exposed only through that function, which hands
// out copies.
var (
	sidecarCPURequest = resource.MustParse("25m")
	sidecarMemory     = resource.MustParse("256Mi")
)

// WithSidecarResourceDefaults applies the per-resource rule of
// WithResourceDefaults to a copy of rr (nil is empty) with the sidecar
// figures: a CPU the block names neither way gets a 25m request and no limit,
// and a memory the block names neither way gets 256Mi as both request and
// limit. Anything the block names is kept as written.
func WithSidecarResourceDefaults(rr *corev1.ResourceRequirements) corev1.ResourceRequirements {
	return withDefaults(rr, sidecarCPURequest.DeepCopy(), sidecarMemory.DeepCopy())
}

// withDefaults is the per-resource rule WithResourceDefaults documents, with
// the CPU request and the memory figure given.
func withDefaults(rr *corev1.ResourceRequirements, cpu, memory resource.Quantity) corev1.ResourceRequirements {
	var out corev1.ResourceRequirements
	if rr != nil {
		out = *rr.DeepCopy()
	}
	if !NamesResource(out, corev1.ResourceCPU) {
		if out.Requests == nil {
			out.Requests = corev1.ResourceList{}
		}
		out.Requests[corev1.ResourceCPU] = cpu
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

// memoryRequestFloor is the memory request WithRequestFloor gives a block that
// names no memory. WithRequestFloor hands out copies only.
var memoryRequestFloor = resource.MustParse("256Mi")

// WithRequestFloor resolves the requests of a container whose working set
// grows with its data rather than with a process count (an OVN Raft member,
// the OVN backup, Neutron's ovn-db-sync), per resource, on a copy of rr (nil
// is empty). A CPU the block names neither as request nor as limit gets a
// request of DefaultCPURequest(), a memory it names neither way gets a 256Mi
// request, and neither gets a limit. Anything else the block sets is kept, so
// a block that names both CPU and memory is used as written. The floor is
// never written into the CR, so an unset field keeps following the floor
// across upgrades.
//
// The floor sets no limit because the working set grows with the logical
// model: a request does not cap that growth, and a default limit would
// OOM-kill the container that outgrew it. A request is never added beside a
// user-set limit of the same resource: the API server defaults an unset
// request to its limit, and an added request would silently lower it. Other
// resources, such as an ephemeral-storage limit, decide neither the QoS class
// nor a CPU or memory request, so they keep the floor beside them.
func WithRequestFloor(rr *corev1.ResourceRequirements) corev1.ResourceRequirements {
	var out corev1.ResourceRequirements
	if rr != nil {
		out = *rr.DeepCopy()
	}
	for _, floor := range []struct {
		name     corev1.ResourceName
		quantity resource.Quantity
	}{
		{name: corev1.ResourceCPU, quantity: DefaultCPURequest()},
		{name: corev1.ResourceMemory, quantity: memoryRequestFloor.DeepCopy()},
	} {
		if NamesResource(out, floor.name) {
			continue
		}
		if out.Requests == nil {
			out.Requests = corev1.ResourceList{}
		}
		out.Requests[floor.name] = floor.quantity
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
