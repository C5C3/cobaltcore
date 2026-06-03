// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.target"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CredentialRotation requests a one-shot rotation of a control-plane credential
// (CC-0110). Today the only supported target is the K-ORC admin application
// credential. The reconciler (L2) re-mints the target credential and reports
// progress via status conditions.
type CredentialRotation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CredentialRotationSpec   `json:"spec,omitempty"`
	Status CredentialRotationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CredentialRotationList contains a list of CredentialRotation.
type CredentialRotationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CredentialRotation `json:"items"`
}

// RotationTarget selects which credential a CredentialRotation rotates
// (CC-0110).
// +kubebuilder:validation:Enum=adminApplicationCredential
type RotationTarget string

const (
	// RotationTargetAdminApplicationCredential rotates the K-ORC admin
	// application credential. This is the only target supported at L1.
	RotationTargetAdminApplicationCredential RotationTarget = "adminApplicationCredential"
)

// CredentialRotationSpec defines the desired state of a CredentialRotation
// (CC-0110).
type CredentialRotationSpec struct {
	// Target selects which credential to rotate. Today only
	// "adminApplicationCredential" is supported.
	Target RotationTarget `json:"target"`

	// Bootstrap, when true, requests an initial mint of the credential rather
	// than a rotation of an existing one. The reconciler treats a bootstrap as
	// idempotent: if the credential already exists it is a no-op.
	// +optional
	Bootstrap bool `json:"bootstrap,omitempty"`

	// ReMint, when true, forces the reconciler to discard the current credential
	// and mint a fresh one even if the existing credential is still valid.
	// +optional
	ReMint bool `json:"reMint,omitempty"`

	// DECISION (CC-0110, REQ-015): the scheduled-rotation fields below are
	// DEFERRED. They surface in the CRD schema now so the contract is stable, but
	// the L1 reconciler ignores them — scheduled rotation (and the two-credential
	// pre-rotation/grace overlap) is implemented in a later level. They are kept
	// here, rather than in a future breaking schema change, so dashboards and
	// GitOps manifests can be written against the final shape. Per REQ-015 the
	// deferral is NOT silent: when any of these fields is set the reconciler emits
	// a "ScheduledRotationDeferred" event (see the matching DECISION in
	// reconcile_credentialrotation.go) so an operator knows the loop is not yet
	// active.

	// IntervalDays is the rotation cadence in days for scheduled rotation.
	// Deferred — ignored by the L1 reconciler.
	// +optional
	// +kubebuilder:validation:Minimum=1
	IntervalDays *int32 `json:"intervalDays,omitempty"`

	// PreRotationDays is how many days before expiry a replacement credential is
	// minted (the overlap window). Deferred — ignored by the L1 reconciler.
	// +optional
	// +kubebuilder:validation:Minimum=0
	PreRotationDays *int32 `json:"preRotationDays,omitempty"`

	// GracePeriodDays is how long the superseded credential remains valid after a
	// rotation before it is revoked. Deferred — ignored by the L1 reconciler.
	// +optional
	// +kubebuilder:validation:Minimum=0
	GracePeriodDays *int32 `json:"gracePeriodDays,omitempty"`
}

// CredentialRotationStatus defines the observed state of a CredentialRotation
// (CC-0110).
type CredentialRotationStatus struct {
	// Conditions represent the latest available observations of the rotation
	// state. Upsert via the shared conditions helper.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the .metadata.generation the controller last
	// reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

func init() {
	SchemeBuilder.Register(&CredentialRotation{}, &CredentialRotationList{})
}
