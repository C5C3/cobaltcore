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
		{name: "default uWSGI counts", perProcess: DefaultMemoryPerProcess(), processes: 2, threads: 1, want: "512Mi"},
		{name: "four processes", perProcess: DefaultMemoryPerProcess(), processes: 4, threads: 1, want: "800Mi"},
		{name: "four processes, two threads", perProcess: DefaultMemoryPerProcess(), processes: 4, threads: 2, want: "928Mi"},
		{name: "glance per-process figure", perProcess: resource.MustParse("400Mi"), processes: 2, threads: 1, want: "1Gi"},
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
// some figures ("1Gi") that a Quantity built by arithmetic does not carry.
func TestMemoryForProcesses_ReturnsTheCanonicalForm(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(MemoryForProcesses(resource.MustParse("400Mi"), 2, 1)).To(gomega.Equal(resource.MustParse("1Gi")))
	g.Expect(MemoryForProcesses(DefaultMemoryPerProcess(), 2, 1)).To(gomega.Equal(resource.MustParse("512Mi")))
}

func TestDefaultMemoryPerProcess_ReturnsACopy(t *testing.T) {
	g := gomega.NewWithT(t)

	q := DefaultMemoryPerProcess()
	q.Add(resource.MustParse("1Gi"))

	next := DefaultMemoryPerProcess()
	g.Expect(next.Cmp(resource.MustParse("144Mi"))).To(gomega.BeZero())
}

func TestWithResourceDefaults(t *testing.T) {
	q := resource.MustParse
	mem512 := q("512Mi")
	defaults := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: q("100m"), corev1.ResourceMemory: q("512Mi")},
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
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("100m")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("1Gi")},
			},
		},
		{
			name:   "memory request only gets no memory limit",
			in:     &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: q("2Gi")}},
			memory: mem512,
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: q("2Gi"), corev1.ResourceCPU: q("100m")},
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
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("100m"), corev1.ResourceMemory: q("512Mi")},
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
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("100m"), corev1.ResourceMemory: q("368Mi")},
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
	g.Expect(next.Requests[corev1.ResourceCPU]).To(gomega.Equal(resource.MustParse("100m")))
	g.Expect(next.Requests[corev1.ResourceMemory]).To(gomega.Equal(resource.MustParse("512Mi")))
	g.Expect(next.Limits[corev1.ResourceMemory]).To(gomega.Equal(resource.MustParse("512Mi")))
}
