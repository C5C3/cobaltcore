// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// The order the caller passes is the order the container gets: a capability set
// read back out of a live pod is compared against the builder's, and a helper
// that sorted or deduplicated would make that comparison drift.
func TestCapabilitySecurityContext_AddsExactlyTheCapsInOrder(t *testing.T) {
	g := gomega.NewWithT(t)

	sc := CapabilitySecurityContext("NET_ADMIN", "SYS_NICE", "NET_RAW")

	g.Expect(sc.Capabilities).NotTo(gomega.BeNil())
	g.Expect(sc.Capabilities.Add).To(gomega.Equal([]corev1.Capability{"NET_ADMIN", "SYS_NICE", "NET_RAW"}))
	g.Expect(sc.Capabilities.Drop).To(gomega.Equal([]corev1.Capability{"ALL"}))

	// Everything the Restricted profile hardens stays hardened. Only the user
	// pinning is left to the image.
	g.Expect(sc.Privileged).To(gomega.HaveValue(gomega.BeFalse()))
	g.Expect(sc.AllowPrivilegeEscalation).To(gomega.HaveValue(gomega.BeFalse()))
	g.Expect(sc.ReadOnlyRootFilesystem).To(gomega.HaveValue(gomega.BeTrue()))
	g.Expect(sc.SeccompProfile).NotTo(gomega.BeNil())
	g.Expect(sc.SeccompProfile.Type).To(gomega.Equal(corev1.SeccompProfileTypeRuntimeDefault))
	g.Expect(sc.RunAsUser).To(gomega.BeNil())
	g.Expect(sc.RunAsGroup).To(gomega.BeNil())
	g.Expect(sc.RunAsNonRoot).To(gomega.BeNil())
}

// No capability at all still drops ALL, and renders no add list rather than an
// empty one: an empty list would show up as `add: []` in every applied pod spec
// and in every golden beside it.
func TestCapabilitySecurityContext_NoCaps(t *testing.T) {
	g := gomega.NewWithT(t)

	sc := CapabilitySecurityContext()

	g.Expect(sc.Capabilities).NotTo(gomega.BeNil())
	g.Expect(sc.Capabilities.Add).To(gomega.BeNil())
	g.Expect(sc.Capabilities.Drop).To(gomega.Equal([]corev1.Capability{"ALL"}))
}

// Two containers of one pod each call the helper, so a shared slice or a shared
// struct would let a mutation on one container's context reach the other.
func TestCapabilitySecurityContext_FreshValuePerCall(t *testing.T) {
	g := gomega.NewWithT(t)

	caps := []corev1.Capability{"NET_ADMIN"}

	first := CapabilitySecurityContext(caps...)
	first.Capabilities.Add[0] = "SYS_ADMIN"
	first.Capabilities.Drop = append(first.Capabilities.Drop, "CHOWN")
	first.Privileged = ptr.To(true)

	second := CapabilitySecurityContext(caps...)

	g.Expect(second.Capabilities.Add).To(gomega.Equal([]corev1.Capability{"NET_ADMIN"}))
	g.Expect(second.Capabilities.Drop).To(gomega.Equal([]corev1.Capability{"ALL"}))
	g.Expect(second.Privileged).To(gomega.HaveValue(gomega.BeFalse()))
	// The caller's own slice is copied, not adopted.
	g.Expect(caps).To(gomega.Equal([]corev1.Capability{"NET_ADMIN"}))
}

// The privileged escape is the one posture nothing else in the tree may take by
// accident, so every field is pinned, including the three that stay nil.
func TestPrivilegedSecurityContext(t *testing.T) {
	g := gomega.NewWithT(t)

	sc := PrivilegedSecurityContext()

	g.Expect(sc.Privileged).To(gomega.HaveValue(gomega.BeTrue()))
	g.Expect(sc.AllowPrivilegeEscalation).To(gomega.HaveValue(gomega.BeTrue()))
	g.Expect(sc.ReadOnlyRootFilesystem).To(gomega.HaveValue(gomega.BeTrue()))
	g.Expect(sc.Capabilities).To(gomega.BeNil())
	g.Expect(sc.SeccompProfile).To(gomega.BeNil())
	g.Expect(sc.RunAsNonRoot).To(gomega.BeNil())
	g.Expect(sc.RunAsUser).To(gomega.BeNil())
	g.Expect(sc.RunAsGroup).To(gomega.BeNil())

	// A second call must not hand back the first one's value.
	sc.Privileged = ptr.To(false)
	g.Expect(PrivilegedSecurityContext().Privileged).To(gomega.HaveValue(gomega.BeTrue()))
}
