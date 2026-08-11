// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the sub-reconciler instrumentation wiring.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
)

// TestSubReconcilerConditionTypesCoversAllNames is one half of the drift guard:
// every condition_type value in subReconcilerConditionTypes must be a member of
// subConditionTypes, otherwise an addition to one list without the other will
// silently produce metrics with a stale condition_type label. The repo's
// condition-coverage audit keys on this exact test.
func TestSubReconcilerConditionTypesCoversAllNames(t *testing.T) {
	g := NewGomegaWithT(t)

	known := make(map[string]struct{}, len(subConditionTypes))
	for _, ct := range subConditionTypes {
		known[ct] = struct{}{}
	}

	for name, condType := range subReconcilerConditionTypes {
		_, ok := known[condType]
		g.Expect(ok).To(BeTrue(),
			"sub_reconciler %q maps to condition_type %q which is not in subConditionTypes — "+
				"update subConditionTypes or fix the mapping", name, condType)
	}
}

// TestPipelineStepNamesAreMapped is the other half of the drift guard, walking
// the mapping in the opposite direction: every step name the pipeline actually
// runs must be a key in subReconcilerConditionTypes, otherwise its error series
// carries condition_type=UNKNOWN. The names come from the pipeline itself
// (pipelineSteps plus the parallel group's members), so a step added to Reconcile
// without a mapping entry fails here rather than in a Prometheus query.
func TestPipelineStepNamesAreMapped(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &BarbicanReconciler{}

	var names []string
	// The parallel-group step carries no name of its own by design: it
	// self-instruments its members (see commonreconcile.Step), which are
	// collected from parallelSteps below. Its arguments are the outputs of
	// earlier steps, and neither of them is read here — only the member names and
	// condition types are.
	for _, step := range r.pipelineSteps(r.Client, testBarbican()) {
		if step.Name != "" {
			names = append(names, step.Name)
		}
	}
	for _, sub := range r.parallelSteps(r.Client, secretStoreProjection{}) {
		names = append(names, sub.Name)
		g.Expect(subReconcilerConditionTypes[sub.Name]).To(Equal(sub.ConditionType),
			"parallel member %q reports condition %q but the metrics map says %q",
			sub.Name, sub.ConditionType, subReconcilerConditionTypes[sub.Name])
	}

	g.Expect(names).To(HaveLen(len(subReconcilerConditionTypes)),
		"every mapped sub_reconciler must correspond to exactly one pipeline step")
	for _, name := range names {
		g.Expect(subReconcilerConditionTypes).To(HaveKey(name),
			"pipeline step %q has no subReconcilerConditionTypes entry, so its errors "+
				"would be attributed to condition_type=UNKNOWN", name)
	}
}
