// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package naming

// Selector label keys used by the workload pod selectors, webhook TSC
// validation, and CommonLabels. Exported so webhooks and controllers across
// operators reference the same constants — prevents silent drift.
const (
	LabelKeyName      = "app.kubernetes.io/name"
	LabelKeyInstance  = "app.kubernetes.io/instance"
	LabelKeyManagedBy = "app.kubernetes.io/managed-by"
)

// CommonLabels returns the standard Kubernetes labels applied to all
// resources owned by a service CR instance. The managed-by value is derived
// from the app name by convention (<app>-operator), matching the operator
// binary that projects the workload.
func CommonLabels(appName, instanceName string) map[string]string {
	return map[string]string{
		LabelKeyName:      appName,
		LabelKeyInstance:  instanceName,
		LabelKeyManagedBy: appName + "-operator",
	}
}

// SelectorLabels returns the minimal label set used as the Deployment pod
// selector. It is a subset of CommonLabels and must remain stable for the
// lifetime of a Deployment (selectors are immutable after creation).
func SelectorLabels(appName, instanceName string) map[string]string {
	return map[string]string{
		LabelKeyName:     appName,
		LabelKeyInstance: instanceName,
	}
}

// SubResourceName returns the canonical name for operator-managed
// sub-resources (Deployment, HPA, Service, PodDisruptionBudget,
// NetworkPolicy, HTTPRoute) of the given CR instance. Centralised here so
// the naming convention is defined in one place. It returns the bare CR name
// with no suffix — the historical `-api` suffix was dropped to align internal
// Service DNS with the public hostname posture.
func SubResourceName(instanceName string) string {
	return instanceName
}
