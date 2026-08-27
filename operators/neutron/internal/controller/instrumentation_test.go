// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the sub-reconciler instrumentation wiring.
package controller

import (
	"slices"
	"testing"

	. "github.com/onsi/gomega"
)

// TestSubReconcilerConditionTypesCoversAllNames is one half of the drift guard:
// every condition_type value in subReconcilerConditionTypes must be a member of
// subConditionTypes or of agentSubConditionTypes, otherwise an addition to one
// list without the other will silently produce metrics with a stale
// condition_type label. One map serves both pipelines, so the union of the two
// vocabularies is what it is checked against. The two overlap in SecretsReady by
// design: both kinds render config artefacts under that condition. The repo's
// condition-coverage audit keys on this exact test.
func TestSubReconcilerConditionTypesCoversAllNames(t *testing.T) {
	g := NewGomegaWithT(t)

	known := make(map[string]struct{}, len(subConditionTypes)+len(agentSubConditionTypes))
	for _, ct := range append(append([]string{}, subConditionTypes...), agentSubConditionTypes...) {
		known[ct] = struct{}{}
	}

	for name, condType := range subReconcilerConditionTypes {
		_, ok := known[condType]
		g.Expect(ok).To(BeTrue(),
			"sub_reconciler %q maps to condition_type %q which is in neither subConditionTypes "+
				"nor agentSubConditionTypes. update the owning list or fix the mapping", name, condType)
	}
}

// TestPipelineStepNamesAreMapped is the other half of the drift guard, walking
// the mapping in the opposite direction: every step name the Neutron pipeline
// actually runs must be a key in subReconcilerConditionTypes, otherwise its
// error series carries condition_type=UNKNOWN. The names come from the pipeline
// itself (pipelineSteps plus the parallel group's members), so a step added to
// Reconcile without a mapping entry fails here rather than in a Prometheus query.
//
// The map also serves the NeutronMetadataAgent pipeline, whose steps this
// reconciler does not run, so "Chassis" and "DaemonSet" are the only keys the
// reverse direction admits without a Neutron step behind them. The agent's third
// step, "Secrets", shares its name with the Neutron one and is covered here.
func TestPipelineStepNamesAreMapped(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &NeutronReconciler{}

	agentOnlySteps := []string{"Chassis", "DaemonSet"}

	var names []string
	// The parallel-group step carries no name of its own by design: it
	// self-instruments its members (see commonreconcile.Step), which are collected
	// from parallelSteps below. The arguments of both methods are the outputs of
	// earlier steps, and none of them is read here: only the step names and the
	// members' condition types are.
	for _, step := range r.pipelineSteps(r.Client, validNeutron()) {
		if step.Name != "" {
			names = append(names, step.Name)
		}
	}
	for _, sub := range r.parallelSteps(r.Client, resolvedOVNEndpoints{}, 0) {
		names = append(names, sub.Name)
		g.Expect(subReconcilerConditionTypes[sub.Name]).To(Equal(sub.ConditionType),
			"parallel member %q reports condition %q but the metrics map says %q",
			sub.Name, sub.ConditionType, subReconcilerConditionTypes[sub.Name])
	}

	for _, name := range names {
		g.Expect(subReconcilerConditionTypes).To(HaveKey(name),
			"pipeline step %q has no subReconcilerConditionTypes entry, so its errors "+
				"would be attributed to condition_type=UNKNOWN", name)
	}

	g.Expect(names).To(HaveLen(len(subReconcilerConditionTypes)-len(agentOnlySteps)),
		"every mapped sub_reconciler must correspond to exactly one pipeline step "+
			"of one of the two kinds")
	for name := range subReconcilerConditionTypes {
		if slices.Contains(agentOnlySteps, name) {
			continue
		}
		g.Expect(names).To(ContainElement(name),
			"subReconcilerConditionTypes maps sub_reconciler %q, which no Neutron "+
				"pipeline step and no NeutronMetadataAgent step runs", name)
	}
}
