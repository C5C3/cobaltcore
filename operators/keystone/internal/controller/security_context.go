// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// openstackUID is the UID/GID of the "openstack" user created in
// images/python-base/Dockerfile and shared by all service images.
const openstackUID int64 = 42424

// restrictedSecurityContext returns a container-level SecurityContext that
// satisfies the Pod Security Standards Restricted profile. All Job and CronJob
// builders must use this helper to ensure a consistent security posture.
func restrictedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		RunAsNonRoot:             ptr.To(true),
		RunAsUser:                ptr.To(openstackUID),
		RunAsGroup:               ptr.To(openstackUID),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}
