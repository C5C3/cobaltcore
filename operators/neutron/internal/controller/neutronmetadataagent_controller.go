// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

// agentSubConditionTypes lists the condition types set by the individual
// NeutronMetadataAgent sub-reconcilers. The aggregate Ready condition is True
// only when all of these are True. The agent runs no optional step: every one of
// the three is set on every pass.
//
// It is a separate list from subConditionTypes because the two kinds carry
// separate status contracts, while one instrumenter serves both pipelines. The
// drift guard in instrumentation_test.go checks the metrics map against the
// union of the two.
var agentSubConditionTypes = []string{
	"ChassisReady",
	"SecretsReady",
	"DaemonSetReady",
}
