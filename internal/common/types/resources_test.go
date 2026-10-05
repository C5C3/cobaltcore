// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestMemoryForProcesses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		perProcess resource.Quantity
		processes  int32
		threads    int32
		want       string
	}{
		{name: "one process, one thread", perProcess: DefaultMemoryPerProcess(), processes: 1, threads: 1, want: "368Mi"},
		{name: "default uWSGI counts", perProcess: DefaultMemoryPerProcess(), processes: 2, threads: 1, want: "720Mi"},
		{name: "four processes", perProcess: DefaultMemoryPerProcess(), processes: 4, threads: 1, want: "1424Mi"},
		{name: "four processes, two threads", perProcess: DefaultMemoryPerProcess(), processes: 4, threads: 2, want: "1552Mi"},
		{name: "glance per-process figure", perProcess: resource.MustParse("1Gi"), processes: 2, threads: 1, want: "2064Mi"},
		{name: "zero counts clamp to one", perProcess: DefaultMemoryPerProcess(), processes: 0, threads: 0, want: "368Mi"},
		{name: "negative counts clamp to one", perProcess: DefaultMemoryPerProcess(), processes: -3, threads: -1, want: "368Mi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			got := MemoryForProcesses(tc.perProcess, tc.processes, tc.threads)
			g.Expect(got.Cmp(resource.MustParse(tc.want))).To(gomega.BeZero(), "got %s, want %s", got.String(), tc.want)
		})
	}
}

// The result must be structurally equal to the parsed literal, not only
// semantically: reconciler tests compare rendered containers with Equal
// (reflect.DeepEqual), and resource.MustParse caches its input string for
// some figures ("1Gi") that a Quantity built by arithmetic does not carry. No
// default figure is a whole Gi, so the first case picks a per-process figure
// that makes one: 16Mi + 2 × 504Mi = 1Gi.
func TestMemoryForProcesses_ReturnsTheCanonicalForm(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(MemoryForProcesses(resource.MustParse("504Mi"), 2, 1)).To(gomega.Equal(resource.MustParse("1Gi")))
	g.Expect(MemoryForProcesses(DefaultMemoryPerProcess(), 2, 1)).To(gomega.Equal(resource.MustParse("720Mi")))
}

func TestDefaultMemoryPerProcess_ReturnsACopy(t *testing.T) {
	g := gomega.NewWithT(t)

	q := DefaultMemoryPerProcess()
	q.Add(resource.MustParse("1Gi"))

	next := DefaultMemoryPerProcess()
	g.Expect(next.Cmp(resource.MustParse("352Mi"))).To(gomega.BeZero())
}

func TestMemoryRequestFloor_ReturnsACopy(t *testing.T) {
	g := gomega.NewWithT(t)

	q := MemoryRequestFloor()
	g.Expect(q.Cmp(resource.MustParse("256Mi"))).To(gomega.BeZero())
	q.Add(resource.MustParse("1Gi"))

	next := MemoryRequestFloor()
	g.Expect(next.Cmp(resource.MustParse("256Mi"))).To(gomega.BeZero())
}

func TestWithResourceDefaults(t *testing.T) {
	q := resource.MustParse
	mem512 := q("512Mi")
	defaults := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: q("70m"), corev1.ResourceMemory: q("512Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: q("512Mi")},
	}

	for _, tc := range []struct {
		name   string
		in     *corev1.ResourceRequirements
		memory resource.Quantity
		want   corev1.ResourceRequirements
	}{
		{name: "nil block", in: nil, memory: mem512, want: defaults},
		{name: "empty block", in: &corev1.ResourceRequirements{}, memory: mem512, want: defaults},
		{
			name:   "empty maps",
			in:     &corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}},
			memory: mem512,
			want:   defaults,
		},
		{
			name:   "memory limit only gets no memory request",
			in:     &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: q("1Gi")}},
			memory: mem512,
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("70m")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("1Gi")},
			},
		},
		{
			name:   "memory request only gets no memory limit",
			in:     &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: q("2Gi")}},
			memory: mem512,
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: q("2Gi"), corev1.ResourceCPU: q("70m")},
			},
		},
		{
			name:   "CPU limit only gets no CPU request",
			in:     &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: q("2")}},
			memory: mem512,
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: q("512Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("2"), corev1.ResourceMemory: q("512Mi")},
			},
		},
		{
			name:   "zero CPU request is kept",
			in:     &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: q("0")}},
			memory: mem512,
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("0"), corev1.ResourceMemory: q("512Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("512Mi")},
			},
		},
		{
			name:   "other resources are kept beside the defaults",
			in:     &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceEphemeralStorage: q("1Gi")}},
			memory: mem512,
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("70m"), corev1.ResourceMemory: q("512Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceEphemeralStorage: q("1Gi"), corev1.ResourceMemory: q("512Mi")},
			},
		},
		{
			name:   "claims are kept beside the defaults",
			in:     &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}},
			memory: mem512,
			want: corev1.ResourceRequirements{
				Requests: defaults.Requests,
				Limits:   defaults.Limits,
				Claims:   []corev1.ResourceClaim{{Name: "gpu"}},
			},
		},
		{
			name:   "zero memory falls back to one process",
			in:     nil,
			memory: resource.Quantity{},
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("70m"), corev1.ResourceMemory: q("368Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("368Mi")},
			},
		},
		{
			name: "block naming CPU and memory is used as written",
			in: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("250m"), corev1.ResourceMemory: q("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("2Gi")},
			},
			memory: mem512,
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("250m"), corev1.ResourceMemory: q("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("2Gi")},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			got := WithResourceDefaults(tc.in, tc.memory)
			g.Expect(got).To(gomega.Equal(tc.want))
		})
	}
}

// The nil-block result must carry exactly two requests and one limit: the
// Equal comparison above already pins it, this names the absent CPU limit.
func TestWithResourceDefaults_SetsNoCPULimit(t *testing.T) {
	g := gomega.NewWithT(t)

	got := WithResourceDefaults(nil, resource.MustParse("512Mi"))

	g.Expect(got.Requests).To(gomega.HaveLen(2))
	g.Expect(got.Limits).To(gomega.HaveLen(1))
	g.Expect(got.Limits).NotTo(gomega.HaveKey(corev1.ResourceCPU))
}

func TestWithResourceDefaults_ReturnsACopy(t *testing.T) {
	g := gomega.NewWithT(t)

	in := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
		Limits:   corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
	}
	want := in.DeepCopy()

	got := WithResourceDefaults(in, resource.MustParse("512Mi"))
	got.Requests[corev1.ResourceCPU] = resource.MustParse("4")
	got.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("8Gi")
	got.Limits[corev1.ResourceMemory] = resource.MustParse("8Gi")

	g.Expect(in).To(gomega.Equal(want), "writing to the result must leave the input block unchanged")

	first := WithResourceDefaults(nil, resource.MustParse("512Mi"))
	first.Requests[corev1.ResourceCPU] = resource.MustParse("4")
	first.Requests[corev1.ResourceMemory] = resource.MustParse("8Gi")
	first.Limits[corev1.ResourceMemory] = resource.MustParse("8Gi")

	next := WithResourceDefaults(nil, resource.MustParse("512Mi"))
	g.Expect(next.Requests[corev1.ResourceCPU]).To(gomega.Equal(resource.MustParse("70m")))
	g.Expect(next.Requests[corev1.ResourceMemory]).To(gomega.Equal(resource.MustParse("512Mi")))
	g.Expect(next.Limits[corev1.ResourceMemory]).To(gomega.Equal(resource.MustParse("512Mi")))
}

func TestWithGivenResourceDefaults(t *testing.T) {
	q := resource.MustParse
	cpu, memory := q("130m"), q("2Gi")
	for _, tc := range []struct {
		name string
		in   *corev1.ResourceRequirements
		want corev1.ResourceRequirements
	}{
		{
			name: "nil block gets the given CPU request and the given memory as request and limit",
			in:   nil,
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("130m"), corev1.ResourceMemory: q("2Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("2Gi")},
			},
		},
		{
			name: "empty block is a nil block",
			in:   &corev1.ResourceRequirements{},
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("130m"), corev1.ResourceMemory: q("2Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("2Gi")},
			},
		},
		{
			name: "a named CPU request is kept and the memory is defaulted",
			in:   &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: q("50m")}},
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("50m"), corev1.ResourceMemory: q("2Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("2Gi")},
			},
		},
		{
			name: "a CPU limit gets no request beside it",
			in:   &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: q("1")}},
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: q("2Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("2Gi")},
			},
		},
		{
			name: "a named memory is kept and the CPU is defaulted",
			in:   &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: q("4Gi")}},
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("130m")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("4Gi")},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			got := WithGivenResourceDefaults(tc.in, cpu, memory)

			g.Expect(got).To(gomega.Equal(tc.want))
		})
	}
}

// Writing to a result must change neither the caller's quantities nor the
// next result.
func TestWithGivenResourceDefaults_ReturnsACopy(t *testing.T) {
	g := gomega.NewWithT(t)
	cpu, memory := resource.MustParse("130m"), resource.MustParse("2Gi")

	first := WithGivenResourceDefaults(nil, cpu, memory)
	first.Requests[corev1.ResourceCPU] = resource.MustParse("4")
	first.Limits[corev1.ResourceMemory] = resource.MustParse("8Gi")

	next := WithGivenResourceDefaults(nil, cpu, memory)
	g.Expect(next.Requests[corev1.ResourceCPU]).To(gomega.Equal(resource.MustParse("130m")))
	g.Expect(next.Limits[corev1.ResourceMemory]).To(gomega.Equal(resource.MustParse("2Gi")))
	g.Expect(cpu).To(gomega.Equal(resource.MustParse("130m")))
	g.Expect(memory).To(gomega.Equal(resource.MustParse("2Gi")))
}

func TestWithSidecarResourceDefaults_EmptyBlock(t *testing.T) {
	want := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
	for _, tc := range []struct {
		name string
		in   *corev1.ResourceRequirements
	}{
		{name: "nil block", in: nil},
		{name: "empty block", in: &corev1.ResourceRequirements{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			got := WithSidecarResourceDefaults(tc.in)

			g.Expect(got).To(gomega.Equal(want))
			g.Expect(got.Limits).NotTo(gomega.HaveKey(corev1.ResourceCPU))
		})
	}
}

// A block that names only a memory limit keeps it, and gains no memory request
// the API server would otherwise default to that limit.
func TestWithSidecarResourceDefaults_KeepsUserLimit(t *testing.T) {
	g := gomega.NewWithT(t)

	got := WithSidecarResourceDefaults(&corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	})

	g.Expect(got.Limits).To(gomega.Equal(corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}))
	g.Expect(got.Requests).To(gomega.Equal(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m")}))
}

func TestWithSidecarResourceDefaults_ReturnsACopy(t *testing.T) {
	g := gomega.NewWithT(t)

	first := WithSidecarResourceDefaults(nil)
	first.Requests[corev1.ResourceCPU] = resource.MustParse("4")
	first.Limits[corev1.ResourceMemory] = resource.MustParse("8Gi")

	next := WithSidecarResourceDefaults(nil)
	g.Expect(next.Requests[corev1.ResourceCPU]).To(gomega.Equal(resource.MustParse("25m")))
	g.Expect(next.Limits[corev1.ResourceMemory]).To(gomega.Equal(resource.MustParse("256Mi")))
}

func TestWithRequestFloor_EmptyBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *corev1.ResourceRequirements
	}{
		{name: "nil block", in: nil},
		{name: "empty block", in: &corev1.ResourceRequirements{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			got := WithRequestFloor(tc.in)

			g.Expect(got.Requests).To(gomega.Equal(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("70m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			}))
			g.Expect(got.Limits).To(gomega.BeNil())
		})
	}
}

// A memory limit the block sets decides the memory request (the API server
// defaults it to the limit), so the floor adds only the CPU request. The result
// is a copy: writing to it leaves the input and the floor unchanged.
func TestWithRequestFloor_LimitOnlyAndCopy(t *testing.T) {
	g := gomega.NewWithT(t)

	in := &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	want := in.DeepCopy()

	got := WithRequestFloor(in)
	g.Expect(got.Requests).To(gomega.Equal(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("70m")}))
	g.Expect(got.Limits).To(gomega.Equal(corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}))

	got.Requests[corev1.ResourceCPU] = resource.MustParse("4")
	got.Limits[corev1.ResourceMemory] = resource.MustParse("8Gi")
	g.Expect(in).To(gomega.Equal(want), "writing to the result must leave the input block unchanged")

	floor := WithRequestFloor(nil)
	floor.Requests[corev1.ResourceMemory] = resource.MustParse("8Gi")
	g.Expect(WithRequestFloor(nil).Requests[corev1.ResourceMemory]).To(gomega.Equal(resource.MustParse("256Mi")))
}
