// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the sub-reconciler instrumentation wiring.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"

	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// TestSubReconcilerConditionTypesCoversAllNames is one half of the drift guard:
// every condition_type value in subReconcilerConditionTypes must be a member of
// centralSubConditionTypes or of chassisSubConditionTypes, otherwise an addition
// to one list without the other will silently produce metrics with a stale
// condition_type label. One map serves both pipelines, so the union is what it
// is checked against. The repo's condition-coverage audit keys on this exact
// test.
func TestSubReconcilerConditionTypesCoversAllNames(t *testing.T) {
	g := NewGomegaWithT(t)

	known := make(map[string]struct{}, len(centralSubConditionTypes)+len(chassisSubConditionTypes))
	for _, ct := range append(append([]string{}, centralSubConditionTypes...), chassisSubConditionTypes...) {
		known[ct] = struct{}{}
	}

	for name, condType := range subReconcilerConditionTypes {
		_, ok := known[condType]
		g.Expect(ok).To(BeTrue(),
			"sub_reconciler %q maps to condition_type %q which is in neither centralSubConditionTypes "+
				"nor chassisSubConditionTypes. update the owning list or fix the mapping", name, condType)
	}

	// The two vocabularies have to stay disjoint. A condition type both CRs set
	// would be attributed to one sub_reconciler name in the map and to the other
	// pipeline's step in the metrics, and the reader of an alert could not tell
	// which CR kind is failing.
	g.Expect(known).To(HaveLen(len(centralSubConditionTypes)+len(chassisSubConditionTypes)),
		"the OVNCentral and OVNChassis sub-condition vocabularies must not overlap")
}

// TestPipelineStepNamesAreMapped is the other half of the drift guard, walking
// the mapping in the opposite direction: every step name either pipeline
// actually runs must be a key in subReconcilerConditionTypes, otherwise its
// error series carries condition_type=UNKNOWN. The names come from the pipelines
// themselves (the OVNCentral steps plus the parallel group's members, and the
// OVNChassis steps), so a step added to a Reconcile without a mapping entry
// fails here rather than in a Prometheus query.
func TestPipelineStepNamesAreMapped(t *testing.T) {
	g := NewGomegaWithT(t)
	central := &OVNCentralReconciler{}
	chassis := &OVNChassisReconciler{}

	var names []string
	// The parallel-group step carries no name of its own by design: it
	// self-instruments its members (see commonreconcile.Step), which are
	// collected from parallelSteps below. No call here runs a step, so the nil
	// children clients, the nil status snapshot and the bare fixtures are never
	// dereferenced: only the step names and condition types are read.
	for _, step := range central.pipelineSteps(central.Client, testOVNCentral()) {
		if step.Name != "" {
			names = append(names, step.Name)
		}
	}
	for _, sub := range central.parallelSteps(central.Client, &ovnv1alpha1.OVNCentral{}) {
		names = append(names, sub.Name)
		g.Expect(subReconcilerConditionTypes[sub.Name]).To(Equal(sub.ConditionType),
			"parallel member %q reports condition %q but the metrics map says %q",
			sub.Name, sub.ConditionType, subReconcilerConditionTypes[sub.Name])
	}
	for _, step := range chassis.pipelineSteps(chassis.Client, testOVNChassis(), nil) {
		g.Expect(step.Name).NotTo(BeEmpty(),
			"the OVNChassis pipeline runs no parallel group, so every step is instrumented by name")
		names = append(names, step.Name)
	}

	g.Expect(names).To(HaveLen(len(subReconcilerConditionTypes)),
		"every mapped sub_reconciler must correspond to exactly one pipeline step")
	for _, name := range names {
		g.Expect(subReconcilerConditionTypes).To(HaveKey(name),
			"pipeline step %q has no subReconcilerConditionTypes entry, so its errors "+
				"would be attributed to condition_type=UNKNOWN", name)
	}
}
