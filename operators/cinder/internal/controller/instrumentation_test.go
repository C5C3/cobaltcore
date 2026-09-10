// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the sub-reconciler instrumentation wiring.
package controller

import (
	"slices"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/c5c3/cobaltcore/internal/common/instrumentation"
)

// TestSubReconcilerConditionTypesCoversAllNames is one half of the drift guard:
// every condition_type value in subReconcilerConditionTypes must be a member of
// subConditionTypes, otherwise an addition to one list without the other will
// silently produce metrics with a stale condition_type label. The repo's
// condition-coverage audit keys on this exact test.
func TestSubReconcilerConditionTypesCoversAllNames(t *testing.T) {
	g := NewGomegaWithT(t)

	for name, condType := range subReconcilerConditionTypes {
		g.Expect(subConditionTypes).To(ContainElement(condType),
			"sub_reconciler %q maps to condition_type %q, which subConditionTypes does not "+
				"aggregate. update the list or fix the mapping", name, condType)
	}
}

// TestPipelineStepNamesAreMapped is the other half of the drift guard, walking
// the mapping in the opposite direction: every step name the pipeline actually
// runs must be a key in subReconcilerConditionTypes, otherwise its error series
// carries condition_type=UNKNOWN. The names come from the pipeline itself, so a
// step added to Reconcile without a mapping entry fails here rather than in a
// Prometheus query.
func TestPipelineStepNamesAreMapped(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &CinderReconciler{}
	state := &pipelineState{}

	var names []string
	add := func(name string) {
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	// The parallel-group step carries no name of its own by design: it
	// self-instruments its members (see commonreconcile.Step), which are collected
	// from parallelSteps below. The state both methods read is the outputs of
	// earlier steps, and none of them is read here: only the step names and the
	// members' condition types are.
	for _, step := range r.pipelineSteps(r.Client, validCinder(), state) {
		add(step.Name)
	}
	for _, sub := range r.parallelSteps(r.Client, state) {
		add(sub.Name)
		g.Expect(subReconcilerConditionTypes[sub.Name]).To(Equal(sub.ConditionType),
			"parallel member %q reports condition %q but the metrics map says %q",
			sub.Name, sub.ConditionType, subReconcilerConditionTypes[sub.Name])
	}

	for _, name := range names {
		g.Expect(subReconcilerConditionTypes).To(HaveKey(name),
			"pipeline step %q has no subReconcilerConditionTypes entry, so its errors "+
				"would be attributed to condition_type=UNKNOWN", name)
	}

	g.Expect(names).To(HaveLen(len(subReconcilerConditionTypes)),
		"every mapped sub_reconciler must correspond to a pipeline step")
	for name := range subReconcilerConditionTypes {
		g.Expect(names).To(ContainElement(name),
			"subReconcilerConditionTypes maps sub_reconciler %q, which no Cinder pipeline step runs", name)
	}
}

// TestSubReconcilerConditionTypes_UnknownNameMapsToNothing pins the fallback the
// instrumenter relies on. A step name absent from the map resolves to no
// condition type, which is what makes the helper stamp
// instrumentation.ConditionTypeUnknown instead of an empty label, so the drift
// shows up as UNKNOWN in an alert rather than as a series that silently merges
// with a legitimate one.
func TestSubReconcilerConditionTypes_UnknownNameMapsToNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	condType, ok := subReconcilerConditionTypes["NoSuchStep"]

	g.Expect(ok).To(BeFalse())
	g.Expect(condType).To(BeEmpty())
	g.Expect(instrumentation.ConditionTypeUnknown).NotTo(BeEmpty(),
		"the fallback label must be a readable value, not an empty string")
}
