// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func TestDeploymentSpecDefault_FillsZeroValues(t *testing.T) {
	g := gomega.NewWithT(t)

	d := &DeploymentSpec{}
	d.Default()

	g.Expect(d.Replicas).To(gomega.Equal(DefaultReplicas))
	g.Expect(d.Resources).NotTo(gomega.BeNil())
	g.Expect(d.Resources.Requests[corev1.ResourceMemory]).To(gomega.Equal(DefaultMemoryRequest()))
	g.Expect(d.Resources.Requests[corev1.ResourceCPU]).To(gomega.Equal(DefaultCPURequest()))
	g.Expect(d.Resources.Limits[corev1.ResourceMemory]).To(gomega.Equal(DefaultMemoryLimit()))
	g.Expect(d.Resources.Limits[corev1.ResourceCPU]).To(gomega.Equal(DefaultCPULimit()))
}

// An empty-but-non-nil Resources block (`resources: {}`) would produce
// BestEffort QoS and break HPA utilization calculations, so Default must fill
// it exactly like the nil case.
func TestDeploymentSpecDefault_FillsEmptyResources(t *testing.T) {
	g := gomega.NewWithT(t)

	d := &DeploymentSpec{Resources: &corev1.ResourceRequirements{}}
	d.Default()

	g.Expect(d.Resources.Requests).NotTo(gomega.BeEmpty())
	g.Expect(d.Resources.Limits).NotTo(gomega.BeEmpty())
}

func TestDeploymentSpecDefault_PreservesExplicitValues(t *testing.T) {
	g := gomega.NewWithT(t)

	custom := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	}
	d := &DeploymentSpec{Replicas: 5, Resources: custom}
	d.Default()

	g.Expect(d.Replicas).To(gomega.Equal(int32(5)))
	g.Expect(d.Resources).To(gomega.BeIdenticalTo(custom))
	g.Expect(d.Resources.Requests[corev1.ResourceCPU]).To(gomega.Equal(resource.MustParse("2")))
}

func TestLoggingSpecDefault_FillsZeroValues(t *testing.T) {
	g := gomega.NewWithT(t)

	l := &LoggingSpec{}
	l.Default()

	g.Expect(l.Format).To(gomega.Equal("text"))
	g.Expect(l.Level).To(gomega.Equal("INFO"))
	g.Expect(l.Debug).To(gomega.HaveValue(gomega.BeFalse()))
}

// An explicit Debug=true must survive Default — the nil-preserving pointer is
// what distinguishes "unset" from an explicit user choice.
func TestLoggingSpecDefault_PreservesExplicitValues(t *testing.T) {
	g := gomega.NewWithT(t)

	l := &LoggingSpec{Format: "json", Level: "DEBUG", Debug: ptr.To(true)}
	l.Default()

	g.Expect(l.Format).To(gomega.Equal("json"))
	g.Expect(l.Level).To(gomega.Equal("DEBUG"))
	g.Expect(l.Debug).To(gomega.HaveValue(gomega.BeTrue()))
}
