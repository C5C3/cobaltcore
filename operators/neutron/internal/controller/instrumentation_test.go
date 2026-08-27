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
