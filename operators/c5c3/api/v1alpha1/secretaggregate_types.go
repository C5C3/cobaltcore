// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SecretAggregate aggregates the Secrets produced by a control plane into a
// single materialized Secret.
//
// DECISION: this is TYPES ONLY at L1 — there is no controller. The
// reconciler is deferred, and the operator RBAC for this kind will
// be READ-ONLY (get/list/watch) until that reconciler lands, so the operator
// can observe SecretAggregate CRs without being granted write access to a kind
// it does not yet manage. The Spec/Status below are intentionally minimal
// placeholders.
type SecretAggregate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SecretAggregateSpec   `json:"spec,omitempty"`
	Status SecretAggregateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SecretAggregateList contains a list of SecretAggregate.
type SecretAggregateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SecretAggregate `json:"items"`
}

// SecretAggregateSpec defines the desired state of a SecretAggregate.
// Minimal placeholder — see the DECISION on SecretAggregate.
type SecretAggregateSpec struct {
	// TargetSecretName is the name of the materialized aggregate Secret the
	// (deferred) reconciler will produce.
	// +optional
	TargetSecretName string `json:"targetSecretName,omitempty"`
}

// SecretAggregateStatus defines the observed state of a SecretAggregate.
// Minimal placeholder.
type SecretAggregateStatus struct {
	// Conditions represent the latest available observations of the aggregate
	// state. Upsert via the shared conditions helper.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

func init() {
	SchemeBuilder.Register(&SecretAggregate{}, &SecretAggregateList{})
}
