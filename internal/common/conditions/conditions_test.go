// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package conditions

import (
	"testing"

	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetCondition(t *testing.T) {
	tests := []struct {
		name       string
		initial    []metav1.Condition
		condition  metav1.Condition
		wantLen    int
		wantStatus metav1.ConditionStatus
	}{
		{
			name:    "add to empty slice",
			initial: []metav1.Condition{},
			condition: metav1.Condition{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "AllGood",
			},
			wantLen:    1,
			wantStatus: metav1.ConditionTrue,
		},
		{
			name: "update existing condition",
			initial: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionFalse,
					Reason: "NotReady",
				},
			},
			condition: metav1.Condition{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "AllGood",
			},
			wantLen:    1,
			wantStatus: metav1.ConditionTrue,
		},
		{
			name: "add new type alongside existing",
			initial: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "AllGood",
				},
			},
			condition: metav1.Condition{
				Type:   "Progressing",
				Status: metav1.ConditionTrue,
				Reason: "InProgress",
			},
			wantLen:    2,
			wantStatus: metav1.ConditionTrue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			conditions := make([]metav1.Condition, len(tt.initial))
			copy(conditions, tt.initial)
			SetCondition(&conditions, tt.condition)
			g.Expect(conditions).To(HaveLen(tt.wantLen))
			found := GetCondition(conditions, tt.condition.Type)
			g.Expect(found).NotTo(BeNil())
			g.Expect(found.Status).To(Equal(tt.wantStatus))
		})
	}
}

func TestGetCondition(t *testing.T) {
	tests := []struct {
		name          string
		conditions    []metav1.Condition
		conditionType string
		wantNil       bool
		wantStatus    metav1.ConditionStatus
	}{
		{
			name:          "nil conditions",
			conditions:    nil,
			conditionType: "Ready",
			wantNil:       true,
		},
		{
			name:          "empty conditions",
			conditions:    []metav1.Condition{},
			conditionType: "Ready",
			wantNil:       true,
		},
		{
			name: "type not found",
			conditions: []metav1.Condition{
				{Type: "Progressing", Status: metav1.ConditionTrue},
			},
			conditionType: "Ready",
			wantNil:       true,
		},
		{
			name: "type found",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
			conditionType: "Ready",
			wantNil:       false,
			wantStatus:    metav1.ConditionTrue,
		},
		{
			name: "correct type returned among multiple",
			conditions: []metav1.Condition{
				{Type: "Progressing", Status: metav1.ConditionFalse},
				{Type: "Ready", Status: metav1.ConditionTrue},
				{Type: "Degraded", Status: metav1.ConditionUnknown},
			},
			conditionType: "Ready",
			wantNil:       false,
			wantStatus:    metav1.ConditionTrue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result := GetCondition(tt.conditions, tt.conditionType)
			if tt.wantNil {
				g.Expect(result).To(BeNil())
			} else {
				g.Expect(result).NotTo(BeNil())
				g.Expect(result.Status).To(Equal(tt.wantStatus))
			}
		})
	}
}

func TestIsReady(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       bool
	}{
		{
			name:       "nil conditions",
			conditions: nil,
			want:       false,
		},
		{
			name:       "empty conditions",
			conditions: []metav1.Condition{},
			want:       false,
		},
		{
			name: "Ready condition is True",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
			want: true,
		},
		{
			name: "Ready condition is False",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionFalse},
			},
			want: false,
		},
		{
			name: "Ready condition is Unknown",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionUnknown},
			},
			want: false,
		},
		{
			name: "no Ready condition present",
			conditions: []metav1.Condition{
				{Type: "Progressing", Status: metav1.ConditionTrue},
			},
			want: false,
		},
		{
			name: "Ready True among multiple conditions",
			conditions: []metav1.Condition{
				{Type: "Progressing", Status: metav1.ConditionTrue},
				{Type: "Ready", Status: metav1.ConditionTrue},
				{Type: "Degraded", Status: metav1.ConditionFalse},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(IsReady(tt.conditions)).To(Equal(tt.want))
		})
	}
}

func TestAllTrue(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		types      []string
		want       bool
	}{
		{
			name:       "empty types list (vacuous truth)",
			conditions: nil,
			types:      []string{},
			want:       true,
		},
		{
			name:       "nil conditions with non-empty types",
			conditions: nil,
			types:      []string{"Ready"},
			want:       false,
		},
		{
			name:       "empty conditions with non-empty types",
			conditions: []metav1.Condition{},
			types:      []string{"Ready"},
			want:       false,
		},
		{
			name: "single type True",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
			types: []string{"Ready"},
			want:  true,
		},
		{
			name: "single type False",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionFalse},
			},
			types: []string{"Ready"},
			want:  false,
		},
		{
			name: "single type Unknown",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionUnknown},
			},
			types: []string{"Ready"},
			want:  false,
		},
		{
			name: "all specified types True",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
				{Type: "Progressing", Status: metav1.ConditionTrue},
				{Type: "Degraded", Status: metav1.ConditionFalse},
			},
			types: []string{"Ready", "Progressing"},
			want:  true,
		},
		{
			name: "one specified type missing",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
			types: []string{"Ready", "Progressing"},
			want:  false,
		},
		{
			name: "one specified type not True",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
				{Type: "Progressing", Status: metav1.ConditionFalse},
			},
			types: []string{"Ready", "Progressing"},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(AllTrue(tt.conditions, tt.types...)).To(Equal(tt.want))
		})
	}
}
