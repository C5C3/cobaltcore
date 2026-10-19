// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package celhost

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// CELHostSpec holds one optional block per shared type that declares a CEL
// rule. Every block is a pointer, so a test case sets only the block whose
// rule it drives and every other rule stays out of play.
type CELHostSpec struct {
	// Image carries the ImageSpec rules.
	// +optional
	Image *commonv1.ImageSpec `json:"image,omitempty"`

	// Database carries the DatabaseSpec rules.
	// +optional
	Database *commonv1.DatabaseSpec `json:"database,omitempty"`

	// Cache carries the CacheSpec rules.
	// +optional
	Cache *commonv1.CacheSpec `json:"cache,omitempty"`

	// Messaging carries the MessagingSpec rule.
	// +optional
	Messaging *commonv1.MessagingSpec `json:"messaging,omitempty"`

	// Policy carries the PolicySpec rules.
	// +optional
	Policy *commonv1.PolicySpec `json:"policy,omitempty"`

	// Deployment carries the DeploymentSpec rule.
	// +optional
	Deployment *commonv1.DeploymentSpec `json:"deployment,omitempty"`

	// Autoscaling carries the AutoscalingSpec rules.
	// +optional
	Autoscaling *commonv1.AutoscalingSpec `json:"autoscaling,omitempty"`

	// VerticalAutoscaling carries the VerticalAutoscalingSpec rules.
	// +optional
	VerticalAutoscaling *commonv1.VerticalAutoscalingSpec `json:"verticalAutoscaling,omitempty"`

	// NetworkPolicy carries the NetworkPolicySpec rule.
	// +optional
	NetworkPolicy *commonv1.NetworkPolicySpec `json:"networkPolicy,omitempty"`

	// Logging carries the rules of LoggingSpec.PerLoggerLevels.
	// +optional
	Logging *commonv1.LoggingSpec `json:"logging,omitempty"`

	// UWSGI carries the UWSGISpec rule.
	// +optional
	UWSGI *commonv1.UWSGISpec `json:"uwsgi,omitempty"`
}

// CELHost is the envtest-only host kind of the shared types' CEL rules.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
type CELHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CELHostSpec `json:"spec,omitempty"`
}

func init() {
	SchemeBuilder.Register(&CELHost{})
}
