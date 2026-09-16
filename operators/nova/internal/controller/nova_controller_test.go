// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
)

// TestSubConditionTypes_PinsTheAggregatedVocabulary keeps the Ready contract
// deliberate: every entry is a condition some sub-reconciler sets, and
// ExtraConfigHealthy stays out because a user-owned overlay must not depool an
// API that serves.
func TestSubConditionTypes_PinsTheAggregatedVocabulary(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(subConditionTypes).To(ConsistOf(
		"SecretsReady",
		"ComputeConfigReady",
		"DatabaseReady",
		"ConductorReady",
		"SchedulerReady",
		"MetadataReady",
		"ConsoleProxyReady",
		"DeploymentReady",
		"DBArchiveReady",
		"NovaAPIReady",
		"HPAReady",
		"NetworkPolicyReady",
		"HTTPRouteReady",
		"MetadataHTTPRouteReady",
		"ConsoleHTTPRouteReady",
	))
	g.Expect(subConditionTypes).NotTo(ContainElement("ExtraConfigHealthy"),
		"the extraConfig overlay is informational and must not gate Ready")
}

func TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	// Every sub-condition True -> aggregate Ready True.
	for _, ct := range subConditionTypes {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:   ct,
			Status: metav1.ConditionTrue,
			Reason: "OK",
		})
	}
	setReadyCondition(nova)
	ready := conditions.GetCondition(nova.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

	// Flip one sub-condition False -> aggregate Ready flips False.
	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:   "ConsoleProxyReady",
		Status: metav1.ConditionFalse,
		Reason: "Degraded",
	})
	setReadyCondition(nova)
	ready = conditions.GetCondition(nova.Status.Conditions, "Ready")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
}
