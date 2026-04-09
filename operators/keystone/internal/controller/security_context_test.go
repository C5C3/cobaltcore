// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
)

// Feature: CC-0045

// findContainerByName returns the container with the given name from a slice,
// or nil if no such container exists. Using this helper instead of direct index
// access makes tests resilient to future container additions (CC-0045).
func findContainerByName(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

// expectRestrictedSecurityContext asserts that the given container has a
// SecurityContext satisfying all five PSS Restricted profile fields
// (CC-0045: REQ-001 through REQ-005). This centralises the assertion logic
// so every Job/CronJob security-context test stays consistent.
func expectRestrictedSecurityContext(g Gomega, container *corev1.Container) {
	g.Expect(container).NotTo(BeNil(), "container must exist")

	sc := container.SecurityContext
	g.Expect(sc).NotTo(BeNil(), "SecurityContext must be set on container %q", container.Name)

	// REQ-001: AllowPrivilegeEscalation must be explicitly set to false.
	g.Expect(sc.AllowPrivilegeEscalation).NotTo(BeNil())
	g.Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())

	// REQ-002: RunAsNonRoot must be explicitly set to true.
	g.Expect(sc.RunAsNonRoot).NotTo(BeNil())
	g.Expect(*sc.RunAsNonRoot).To(BeTrue())

	// REQ-003: ReadOnlyRootFilesystem must be explicitly set to true.
	g.Expect(sc.ReadOnlyRootFilesystem).NotTo(BeNil())
	g.Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())

	// REQ-004: SeccompProfile must use RuntimeDefault.
	g.Expect(sc.SeccompProfile).NotTo(BeNil())
	g.Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))

	// REQ-005: Capabilities must drop ALL.
	g.Expect(sc.Capabilities).NotTo(BeNil(), "Capabilities must be set on container %q", container.Name)
	g.Expect(sc.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))

	// REQ-006: RunAsUser must be set to the openstack user UID (42424).
	g.Expect(sc.RunAsUser).NotTo(BeNil())
	g.Expect(*sc.RunAsUser).To(Equal(openstackUID))

	// REQ-007: RunAsGroup must be set to the openstack group GID (42424).
	g.Expect(sc.RunAsGroup).NotTo(BeNil())
	g.Expect(*sc.RunAsGroup).To(Equal(openstackUID))
}

// TestRestrictedSecurityContext verifies that restrictedSecurityContext() returns
// a *corev1.SecurityContext with all five PSS Restricted profile fields set
// correctly (CC-0045: REQ-001 through REQ-005).
func TestRestrictedSecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)

	expectRestrictedSecurityContext(g, &corev1.Container{
		Name:            "test",
		SecurityContext: restrictedSecurityContext(),
	})
}
