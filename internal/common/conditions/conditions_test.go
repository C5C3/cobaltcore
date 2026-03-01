package conditions

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetCondition(t *testing.T) {
	tests := []struct {
		name               string
		initial            []metav1.Condition
		condType           string
		status             metav1.ConditionStatus
		reason             string
		message            string
		expectLen          int
		expectStatus       metav1.ConditionStatus
		expectTimeChanged  bool
		otherTypeUnchanged string
	}{
		{
			name:              "insert into empty slice",
			initial:           []metav1.Condition{},
			condType:          "Ready",
			status:            metav1.ConditionTrue,
			reason:            "AllGood",
			message:           "everything is fine",
			expectLen:         1,
			expectStatus:      metav1.ConditionTrue,
			expectTimeChanged: true,
		},
		{
			name: "update with new status",
			initial: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionFalse,
					Reason:             "NotReady",
					Message:            "not ready yet",
					LastTransitionTime: metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
				},
			},
			condType:          "Ready",
			status:            metav1.ConditionTrue,
			reason:            "NowReady",
			message:           "now ready",
			expectLen:         1,
			expectStatus:      metav1.ConditionTrue,
			expectTimeChanged: true,
		},
		{
			name: "no-op when status unchanged preserves LastTransitionTime",
			initial: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "AllGood",
					Message:            "fine",
					LastTransitionTime: metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
				},
			},
			condType:          "Ready",
			status:            metav1.ConditionTrue,
			reason:            "StillGood",
			message:           "still fine",
			expectLen:         1,
			expectStatus:      metav1.ConditionTrue,
			expectTimeChanged: false,
		},
		{
			name: "multiple conditions, only target type updated",
			initial: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "OK",
					Message:            "ok",
					LastTransitionTime: metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
				},
				{
					Type:               "Available",
					Status:             metav1.ConditionFalse,
					Reason:             "NotAvailable",
					Message:            "not available",
					LastTransitionTime: metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
				},
			},
			condType:           "Available",
			status:             metav1.ConditionTrue,
			reason:             "NowAvailable",
			message:            "available now",
			expectLen:          2,
			expectStatus:       metav1.ConditionTrue,
			expectTimeChanged:  true,
			otherTypeUnchanged: "Ready",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			conditions := make([]metav1.Condition, len(tc.initial))
			copy(conditions, tc.initial)

			var originalTime metav1.Time
			for _, c := range conditions {
				if c.Type == tc.condType {
					originalTime = c.LastTransitionTime
				}
			}

			var otherOriginalTime metav1.Time
			if tc.otherTypeUnchanged != "" {
				for _, c := range conditions {
					if c.Type == tc.otherTypeUnchanged {
						otherOriginalTime = c.LastTransitionTime
					}
				}
			}

			SetCondition(&conditions, tc.condType, tc.status, tc.reason, tc.message)

			g.Expect(conditions).To(HaveLen(tc.expectLen))

			found := GetCondition(conditions, tc.condType)
			g.Expect(found).NotTo(BeNil())
			g.Expect(found.Status).To(Equal(tc.expectStatus))
			g.Expect(found.Reason).To(Equal(tc.reason))
			g.Expect(found.Message).To(Equal(tc.message))

			if tc.expectTimeChanged {
				g.Expect(found.LastTransitionTime.Time).NotTo(Equal(originalTime.Time))
			} else {
				g.Expect(found.LastTransitionTime.Time).To(Equal(originalTime.Time))
			}

			if tc.otherTypeUnchanged != "" {
				other := GetCondition(conditions, tc.otherTypeUnchanged)
				g.Expect(other).NotTo(BeNil())
				g.Expect(other.LastTransitionTime.Time).To(Equal(otherOriginalTime.Time))
			}
		})
	}
}

func TestSetCondition_NilPointer(t *testing.T) {
	g := NewGomegaWithT(t)

	// Should not panic on nil pointer.
	g.Expect(func() {
		SetCondition(nil, "Ready", metav1.ConditionTrue, "OK", "ok")
	}).NotTo(Panic())
}

func TestGetCondition(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		condType   string
		expectNil  bool
	}{
		{
			name: "returns matching condition",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
				{Type: "Available", Status: metav1.ConditionFalse},
			},
			condType:  "Ready",
			expectNil: false,
		},
		{
			name: "returns nil when not found",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
			condType:  "Missing",
			expectNil: true,
		},
		{
			name:       "returns nil on empty slice",
			conditions: []metav1.Condition{},
			condType:   "Ready",
			expectNil:  true,
		},
		{
			name:       "returns nil on nil slice",
			conditions: nil,
			condType:   "Ready",
			expectNil:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			result := GetCondition(tc.conditions, tc.condType)
			if tc.expectNil {
				g.Expect(result).To(BeNil())
			} else {
				g.Expect(result).NotTo(BeNil())
				g.Expect(result.Type).To(Equal(tc.condType))
			}
		})
	}
}

func TestGetCondition_MutationUpdatesOriginalSlice(t *testing.T) {
	g := NewGomegaWithT(t)
	conditions := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse}}

	c := GetCondition(conditions, "Ready")
	g.Expect(c).NotTo(BeNil())
	c.Status = metav1.ConditionTrue

	g.Expect(conditions[0].Status).To(Equal(metav1.ConditionTrue))
}

func TestIsReady(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		expected   bool
	}{
		{
			name: "true when Ready=True",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
			expected: true,
		},
		{
			name: "false when Ready=False",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionFalse},
			},
			expected: false,
		},
		{
			name: "false when no Ready condition",
			conditions: []metav1.Condition{
				{Type: "Available", Status: metav1.ConditionTrue},
			},
			expected: false,
		},
		{
			name:       "false on nil slice",
			conditions: nil,
			expected:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(IsReady(tc.conditions)).To(Equal(tc.expected))
		})
	}
}

func TestAllTrue(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		types      []string
		expected   bool
	}{
		{
			name: "all requested types True",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
				{Type: "Available", Status: metav1.ConditionTrue},
			},
			types:    []string{"Ready", "Available"},
			expected: true,
		},
		{
			name: "one type False",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
				{Type: "Available", Status: metav1.ConditionFalse},
			},
			types:    []string{"Ready", "Available"},
			expected: false,
		},
		{
			name: "missing type",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
			types:    []string{"Ready", "Missing"},
			expected: false,
		},
		{
			name: "no types requested (vacuous truth)",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionFalse},
			},
			types:    []string{},
			expected: true,
		},
		{
			name:       "empty conditions slice with types requested",
			conditions: []metav1.Condition{},
			types:      []string{"Ready"},
			expected:   false,
		},
		{
			name:       "nil conditions with no types (vacuous truth)",
			conditions: nil,
			types:      []string{},
			expected:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(AllTrue(tc.conditions, tc.types...)).To(Equal(tc.expected))
		})
	}
}
