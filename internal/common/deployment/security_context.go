// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// OpenStackUID is the UID/GID of the "openstack" user created in
// images/python-base/Dockerfile and shared by all service images.
const OpenStackUID int64 = 42424

// RestrictedSecurityContext returns a container-level SecurityContext that
// satisfies the Pod Security Standards Restricted profile. All workload, Job,
// and CronJob builders across operators must use this helper to ensure a
// consistent security posture.
func RestrictedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		RunAsNonRoot:             ptr.To(true),
		RunAsUser:                ptr.To(OpenStackUID),
		RunAsGroup:               ptr.To(OpenStackUID),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// CapabilitySecurityContext returns a container-level SecurityContext that keeps
// every hardening field of the Restricted profile and adds the named Linux
// capabilities on top. It is the posture for a container that needs kernel
// access the Restricted profile forbids but still runs unprivileged:
// ovs-vswitchd and ovn-controller take CapabilitySecurityContext("NET_ADMIN"),
// while ovsdb-server beside them needs no capability and stays on
// RestrictedSecurityContext.
//
// The capabilities are added in the order given, on top of a Drop of ALL, so
// the container holds those and nothing else. Passing none returns a context
// with no Add list at all, which is RestrictedSecurityContext without the user
// pinning.
//
// RunAsUser, RunAsGroup and RunAsNonRoot are left unset, so the user the image
// declares applies. A container that runs as the shared openstack user takes
// RestrictedSecurityContext instead.
func CapabilitySecurityContext(caps ...corev1.Capability) *corev1.SecurityContext {
	sc := &corev1.SecurityContext{
		Privileged:               ptr.To(false),
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
	// The variadic slice belongs to the caller when it spreads one of its own,
	// so the capabilities are copied: two contexts built from the same slice
	// must not share it.
	if len(caps) > 0 {
		sc.Capabilities.Add = slices.Clone(caps)
	}
	return sc
}

// PrivilegedSecurityContext returns a container-level SecurityContext for a
// container that cannot run any other way. Its one caller in this tree is the
// host-prepare init container of the OVN chassis, which loads kernel modules
// from the host's module tree before ovs-vswitchd starts. A container that only
// needs a named capability takes CapabilitySecurityContext.
//
// Capabilities, SeccompProfile and RunAsNonRoot stay nil. A privileged
// container holds the full capability set, and spelling out a Drop list or a
// seccomp profile beside Privileged would describe a confinement that is not in
// force.
//
// ReadOnlyRootFilesystem stays true: a privileged container still has no reason
// to write to its own image, and the host paths it works on come in as mounts.
//
// A pod carrying this context needs a namespace that admits it, labelled
// pod-security.kubernetes.io/enforce: privileged on a cluster that enforces the
// Pod Security Standards.
func PrivilegedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		Privileged:               ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(true),
		ReadOnlyRootFilesystem:   ptr.To(true),
	}
}
