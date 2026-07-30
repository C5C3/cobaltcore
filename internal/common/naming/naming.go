// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package naming

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Selector label keys used by the workload pod selectors, webhook TSC
// validation, and CommonLabels. Exported so webhooks and controllers across
// operators reference the same constants — prevents silent drift.
const (
	LabelKeyName      = "app.kubernetes.io/name"
	LabelKeyInstance  = "app.kubernetes.io/instance"
	LabelKeyManagedBy = "app.kubernetes.io/managed-by"
	LabelKeyComponent = "app.kubernetes.io/component"
)

// LabelKeyJobName is the label the Job controller stamps on every pod it
// creates. ExcludeJobPods keys the API PodDisruptionBudget selector on its
// absence.
const LabelKeyJobName = "batch.kubernetes.io/job-name"

// ComponentAPI is the component value carried by the API pods, the ones the
// API Service is allowed to route to. Maintenance workloads (trust flush, key
// and password rotation, database purge) carry their own component value so
// they never become endpoints of that Service.
const ComponentAPI = "api"

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

// APISelectorLabels returns the selector of the API Service. It narrows
// SelectorLabels by the API component so that pods of other components cannot
// become endpoints of the Service. Deployment.spec.selector must keep using
// SelectorLabels: Deployment selectors are immutable after creation, so adding
// a key there would wedge every existing workload.
//
// The Service must not apply this selector while pods from a template without
// the component label are still running: a Service that matches none of the
// pods currently serving is an outage. See the two-phase narrowing in each
// operator's reconcileDeployment. The API PodDisruptionBudget does not use this
// selector at all — see ExcludeJobPods.
func APISelectorLabels(appName, instanceName string) map[string]string {
	labels := SelectorLabels(appName, instanceName)
	labels[LabelKeyComponent] = ComponentAPI
	return labels
}

// ExcludeJobPods returns the label-selector requirement that keeps every
// Job-created pod out of a selector. Together with SelectorLabels it is the
// selector of the API PodDisruptionBudget, deliberately not APISelectorLabels:
// every maintenance workload is a Job, so the absence of the Job name label
// separates the API pods from them without depending on the component label —
// which pods built by an operator predating it do not carry.
//
// A budget must not miss the pods it protects. The eviction API only consults
// budgets whose selector matches the pod being evicted, so a pod outside every
// selector has no budget at all: a node drain, an autoscaler consolidation, or
// a node upgrade may take all replicas at once. Keying on the component label
// would open exactly that gap for the whole one-time migration off the
// unlabelled template, and indefinitely whenever that rollout wedges.
func ExcludeJobPods() []metav1.LabelSelectorRequirement {
	return []metav1.LabelSelectorRequirement{{
		Key:      LabelKeyJobName,
		Operator: metav1.LabelSelectorOpDoesNotExist,
	}}
}

// ComponentLabels returns the pod-template labels for a workload of the given
// component. The result is always a strict superset of SelectorLabels, so the
// Deployment pod selector and the NetworkPolicy pod selector keep matching
// every component flavor — maintenance pods must stay covered by the egress
// rules. The API Service and PDB selectors deliberately do not: they use
// APISelectorLabels.
func ComponentLabels(appName, instanceName, component string) map[string]string {
	labels := CommonLabels(appName, instanceName)
	labels[LabelKeyComponent] = component
	return labels
}
