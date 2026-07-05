// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
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
