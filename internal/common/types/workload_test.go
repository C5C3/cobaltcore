// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
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
}

// Default must never write Resources: the reconcilers resolve the container
// resources when they render the pod, so a nil block stays nil and an empty one
// stays empty.
func TestDeploymentSpecDefault_LeavesResourcesUnset(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resources *corev1.ResourceRequirements
		want      *corev1.ResourceRequirements
	}{
		{name: "nil", resources: nil, want: nil},
		{name: "empty", resources: &corev1.ResourceRequirements{}, want: &corev1.ResourceRequirements{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			d := &DeploymentSpec{Resources: tc.resources}
			d.Default()

			g.Expect(d.Replicas).To(gomega.Equal(DefaultReplicas))
			g.Expect(d.Resources).To(gomega.Equal(tc.want))
		})
	}
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

// The placement fields sit inline in spec.deployment, beside replicas, so the
// CR paths are spec.deployment.nodeSelector, .tolerations and .affinity.
func TestDeploymentSpec_NodePlacementIsInline(t *testing.T) {
	g := gomega.NewWithT(t)

	raw, err := json.Marshal(DeploymentSpec{
		Replicas: 1,
		NodePlacementSpec: NodePlacementSpec{
			NodeSelector: map[string]string{"a": "b"},
			Tolerations:  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Affinity:     &corev1.Affinity{},
		},
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var got map[string]any
	g.Expect(json.Unmarshal(raw, &got)).To(gomega.Succeed())
	g.Expect(got).To(gomega.HaveKey("nodeSelector"))
	g.Expect(got).To(gomega.HaveKey("tolerations"))
	g.Expect(got).To(gomega.HaveKey("affinity"))
	g.Expect(got).NotTo(gomega.HaveKey("NodePlacementSpec"))
}

// A JobSpec is one flat block, and an empty one marshals to {}: every field is
// omitempty, so a server-side apply that sets none of them owns none.
func TestJobSpec_IsFlat(t *testing.T) {
	g := gomega.NewWithT(t)

	raw, err := json.Marshal(JobSpec{
		JobBaseSpec: JobBaseSpec{
			Resources:         &corev1.ResourceRequirements{},
			PriorityClassName: ptr.To(""),
		},
		NodePlacementSpec: NodePlacementSpec{NodeSelector: map[string]string{"a": "b"}, Affinity: &corev1.Affinity{}},
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(string(raw)).To(gomega.MatchJSON(`{"resources":{},"priorityClassName":"","nodeSelector":{"a":"b"},"affinity":{}}`))

	empty, err := json.Marshal(JobSpec{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(string(empty)).To(gomega.MatchJSON(`{}`))
}
