// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// RenderedResourceDefaults is the block a container gets when its CR names
// neither CPU nor memory: a 100m CPU request, no CPU limit, and memory as both
// request and limit. The 100m is a literal on purpose, so a test comparing a
// rendered container against it does not follow a change of
// commonv1.DefaultCPURequest.
func RenderedResourceDefaults(memory string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse(memory)},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)},
	}
}
