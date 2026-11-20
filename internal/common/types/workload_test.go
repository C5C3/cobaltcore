// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// The verticalAutoscaling block is an opt-in: Default never writes it, and an
// unset block leaves no key behind, so a server-side apply of a DeploymentSpec
// without it neither writes nor owns the field.
func TestDeploymentSpec_VerticalAutoscalingIsOptIn(t *testing.T) {
	g := gomega.NewWithT(t)

	d := &DeploymentSpec{}
	d.Default()
	g.Expect(d.VerticalAutoscaling).To(gomega.BeNil())

	raw, err := json.Marshal(d)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(string(raw)).NotTo(gomega.ContainSubstring("verticalAutoscaling"))

	d.VerticalAutoscaling = &VerticalAutoscalingSpec{UpdateMode: "Off"}
	raw, err = json.Marshal(d)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	var got map[string]any
	g.Expect(json.Unmarshal(raw, &got)).To(gomega.Succeed())
	g.Expect(got).To(gomega.HaveKeyWithValue("verticalAutoscaling", map[string]any{"updateMode": "Off"}))
}

// TestEffectiveLogging pins the render-time resolution of spec.logging: a CR
// that bypassed the defaulting webhook still renders the production baseline,
// and the resolver hands back a copy so nothing is written into the CR.
func TestEffectiveLogging(t *testing.T) {
	g := gomega.NewWithT(t)

	fromNil := EffectiveLogging(nil)
	g.Expect(fromNil.Format).To(gomega.Equal("text"))
	g.Expect(fromNil.Level).To(gomega.Equal("INFO"))
	g.Expect(fromNil.Debug).NotTo(gomega.BeNil())
	g.Expect(*fromNil.Debug).To(gomega.BeFalse())

	// A half-filled block keeps what it sets and defaults the rest.
	partial := &LoggingSpec{Level: "WARNING"}
	resolved := EffectiveLogging(partial)
	g.Expect(resolved.Format).To(gomega.Equal("text"))
	g.Expect(resolved.Level).To(gomega.Equal("WARNING"))
	g.Expect(partial.Format).To(gomega.BeEmpty(), "the input spec must be left untouched")
	g.Expect(partial.Debug).To(gomega.BeNil(), "the input spec must be left untouched")
}

// budgetValueError is the message of every value ResolveBudgetValue rejects.
const budgetValueError = "must be a non-negative integer or a percentage from 0% to 100%"

// ResolveBudgetValue returns an integer as it is and rounds a percentage up
// against the pod count, the way the disruption controller does, and rejects
// every other value with one message.
func TestResolveBudgetValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   intstr.IntOrString
		pods    int32
		want    int32
		wantErr bool
	}{
		{name: "zero", value: intstr.FromInt32(0), pods: 3, want: 0},
		{name: "integer", value: intstr.FromInt32(2), pods: 3, want: 2},
		{name: "integer above pods", value: intstr.FromInt32(5), pods: 3, want: 5},
		{name: "25% of 3 rounds up", value: intstr.FromString("25%"), pods: 3, want: 1},
		{name: "50% of 3 rounds up", value: intstr.FromString("50%"), pods: 3, want: 2},
		{name: "67% of 3 rounds up", value: intstr.FromString("67%"), pods: 3, want: 3},
		{name: "0%", value: intstr.FromString("0%"), pods: 3, want: 0},
		{name: "100%", value: intstr.FromString("100%"), pods: 3, want: 3},
		{name: "negative integer", value: intstr.FromInt32(-1), pods: 3, wantErr: true},
		{name: "above 100%", value: intstr.FromString("101%"), pods: 3, wantErr: true},
		{name: "negative percentage", value: intstr.FromString("-5%"), pods: 3, wantErr: true},
		{name: "word", value: intstr.FromString("half"), pods: 3, wantErr: true},
		{name: "number as string", value: intstr.FromString("5"), pods: 3, wantErr: true},
		{name: "empty string", value: intstr.FromString(""), pods: 3, wantErr: true},
		{name: "leading zero", value: intstr.FromString("05%"), pods: 3, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			got, err := ResolveBudgetValue(tc.value, tc.pods)
			if tc.wantErr {
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(budgetValueError)))
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(got).To(gomega.Equal(tc.want))
		})
	}
}

// DisruptionsAllowed is the count of healthy pods the budget lets voluntary
// evictions take. A pod count below one counts as one, and a block that does
// not set exactly one field, or sets a value ResolveBudgetValue rejects, is an
// error.
func TestPodDisruptionBudgetSpec_DisruptionsAllowed(t *testing.T) {
	intVal := func(v int32) *intstr.IntOrString { return ptr.To(intstr.FromInt32(v)) }
	strVal := func(v string) *intstr.IntOrString { return ptr.To(intstr.FromString(v)) }
	for _, tc := range []struct {
		name    string
		budget  *PodDisruptionBudgetSpec
		pods    int32
		want    int32
		wantErr string
	}{
		{name: "maxUnavailable 1", budget: &PodDisruptionBudgetSpec{MaxUnavailable: intVal(1)}, pods: 3, want: 1},
		{name: "maxUnavailable 25%", budget: &PodDisruptionBudgetSpec{MaxUnavailable: strVal("25%")}, pods: 3, want: 1},
		{name: "minAvailable 2", budget: &PodDisruptionBudgetSpec{MinAvailable: intVal(2)}, pods: 3, want: 1},
		{name: "minAvailable 50%", budget: &PodDisruptionBudgetSpec{MinAvailable: strVal("50%")}, pods: 3, want: 1},
		{name: "minAvailable 3", budget: &PodDisruptionBudgetSpec{MinAvailable: intVal(3)}, pods: 3, want: 0},
		{name: "minAvailable 67%", budget: &PodDisruptionBudgetSpec{MinAvailable: strVal("67%")}, pods: 3, want: 0},
		{name: "maxUnavailable 0", budget: &PodDisruptionBudgetSpec{MaxUnavailable: intVal(0)}, pods: 3, want: 0},
		{name: "maxUnavailable 0%", budget: &PodDisruptionBudgetSpec{MaxUnavailable: strVal("0%")}, pods: 3, want: 0},
		{name: "maxUnavailable above pods", budget: &PodDisruptionBudgetSpec{MaxUnavailable: intVal(5)}, pods: 3, want: 3},
		{name: "minAvailable 0", budget: &PodDisruptionBudgetSpec{MinAvailable: intVal(0)}, pods: 3, want: 3},
		{name: "zero pods count as one, maxUnavailable", budget: &PodDisruptionBudgetSpec{MaxUnavailable: intVal(1)}, pods: 0, want: 1},
		{name: "zero pods count as one, minAvailable", budget: &PodDisruptionBudgetSpec{MinAvailable: intVal(1)}, pods: 0, want: 0},
		{name: "negative pods count as one, maxUnavailable", budget: &PodDisruptionBudgetSpec{MaxUnavailable: intVal(1)}, pods: -1, want: 1},
		{name: "negative pods count as one, minAvailable", budget: &PodDisruptionBudgetSpec{MinAvailable: intVal(1)}, pods: -1, want: 0},
		{name: "nil block", budget: nil, pods: 3, wantErr: "exactly one of minAvailable or maxUnavailable must be set"},
		{name: "empty block", budget: &PodDisruptionBudgetSpec{}, pods: 3, wantErr: "exactly one of minAvailable or maxUnavailable must be set"},
		{
			name:    "both fields",
			budget:  &PodDisruptionBudgetSpec{MinAvailable: intVal(1), MaxUnavailable: intVal(1)},
			pods:    3,
			wantErr: "exactly one of minAvailable or maxUnavailable must be set",
		},
		{name: "malformed maxUnavailable", budget: &PodDisruptionBudgetSpec{MaxUnavailable: strVal("half")}, pods: 3, wantErr: "maxUnavailable: " + budgetValueError},
		{name: "malformed minAvailable", budget: &PodDisruptionBudgetSpec{MinAvailable: intVal(-1)}, pods: 3, wantErr: "minAvailable: " + budgetValueError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			got, err := tc.budget.DisruptionsAllowed(tc.pods)
			if tc.wantErr != "" {
				g.Expect(err).To(gomega.MatchError(tc.wantErr))
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(got).To(gomega.Equal(tc.want))
		})
	}
}

// A value error keeps the ResolveBudgetValue error as its cause.
func TestPodDisruptionBudgetSpec_DisruptionsAllowedWrapsValueErrors(t *testing.T) {
	g := gomega.NewWithT(t)

	_, err := (&PodDisruptionBudgetSpec{MaxUnavailable: ptr.To(intstr.FromString("half"))}).DisruptionsAllowed(3)
	_, cause := ResolveBudgetValue(intstr.FromString("half"), 3)

	g.Expect(errors.Unwrap(err)).To(gomega.MatchError(cause.Error()))
}

// The lower replica bound is the HPA minimum while one is set, taken as it
// is, and otherwise the replica count with zero normalized to the default.
func TestLowerReplicaBound(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replicas    int32
		autoscaling *AutoscalingSpec
		want        int32
	}{
		{name: "replicas without autoscaling", replicas: 3, want: 3},
		{name: "zero replicas without autoscaling", replicas: 0, want: DefaultReplicas},
		{name: "HPA minimum below replicas", replicas: 3, autoscaling: &AutoscalingSpec{MinReplicas: ptr.To[int32](1), MaxReplicas: 5}, want: 1},
		{name: "HPA without a minimum", replicas: 2, autoscaling: &AutoscalingSpec{MaxReplicas: 5}, want: 2},
		{name: "HPA without a minimum beside zero replicas", replicas: 0, autoscaling: &AutoscalingSpec{MaxReplicas: 5}, want: DefaultReplicas},
		{name: "zero HPA minimum", replicas: 3, autoscaling: &AutoscalingSpec{MinReplicas: ptr.To[int32](0), MaxReplicas: 5}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(LowerReplicaBound(tc.replicas, tc.autoscaling)).To(gomega.Equal(tc.want))
		})
	}
}
