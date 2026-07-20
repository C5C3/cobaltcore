// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/c5c3/forge/internal/common/conditions"
)

// drainEvents returns every event currently buffered on a FakeRecorder channel
// without blocking, so tests can assert on how many events an operation emitted.
func drainEvents(events chan string) []string {
	var out []string
	for {
		select {
		case e := <-events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestFindOwnedOverrides(t *testing.T) {
	// Registry entries are deliberately out of Section order to prove the
	// result ordering comes from the sort, not the registry declaration.
	registry := []OwnedKey{
		{Section: "token", Key: "provider", OwnedBy: "operator-computed"},
		{Section: "fernet_tokens", Key: "max_active_keys", OwnedBy: "operator-computed"},
		{Section: "database", Key: "connection", Rejected: true, OwnedBy: "operator-computed"},
	}

	tests := []struct {
		name        string
		extraConfig map[string]map[string]string
		want        []OwnedKey
	}{
		{
			name:        "nil extraConfig",
			extraConfig: nil,
			want:        nil,
		},
		{
			name:        "empty extraConfig",
			extraConfig: map[string]map[string]string{},
			want:        nil,
		},
		{
			name: "no matching key",
			extraConfig: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
				"token":   {"expiration": "3600"},
			},
			want: nil,
		},
		{
			name: "multiple matches sorted by section then key regardless of registry order",
			extraConfig: map[string]map[string]string{
				"token":         {"provider": "fernet"},
				"fernet_tokens": {"max_active_keys": "5"},
			},
			want: []OwnedKey{
				{Section: "fernet_tokens", Key: "max_active_keys", OwnedBy: "operator-computed"},
				{Section: "token", Key: "provider", OwnedBy: "operator-computed"},
			},
		},
		{
			name: "rejected-class entry is matched and returned too",
			extraConfig: map[string]map[string]string{
				"database": {"connection": "mysql://x"},
			},
			want: []OwnedKey{
				{Section: "database", Key: "connection", Rejected: true, OwnedBy: "operator-computed"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(FindOwnedOverrides(tt.extraConfig, registry)).To(Equal(tt.want))
		})
	}
}

func TestFindOwnedSettingOverrides(t *testing.T) {
	// A registry mixing INI (Section != "") and flat (Section == "") entries;
	// only the flat ones are eligible for the Horizon-style settings lookup.
	registry := []OwnedKey{
		{Section: "", Key: "STATIC_ROOT", OwnedBy: "operator-computed"},
		{Section: "federation", Key: "trusted_dashboard", OwnedBy: "spec.federation.trustedDashboards"},
		{Section: "", Key: "ALLOWED_HOSTS", OwnedBy: "operator-computed"},
	}

	tests := []struct {
		name         string
		settingNames []string
		want         []OwnedKey
	}{
		{
			name:         "nil names",
			settingNames: nil,
			want:         nil,
		},
		{
			name:         "empty names",
			settingNames: []string{},
			want:         nil,
		},
		{
			name:         "no matching name",
			settingNames: []string{"SESSION_TIMEOUT"},
			want:         nil,
		},
		{
			name: "matches only flat entries, sorted by key",
			// "trusted_dashboard" collides with the INI entry's Key but must
			// not match because that entry carries a non-empty Section.
			settingNames: []string{"STATIC_ROOT", "ALLOWED_HOSTS", "trusted_dashboard"},
			want: []OwnedKey{
				{Section: "", Key: "ALLOWED_HOSTS", OwnedBy: "operator-computed"},
				{Section: "", Key: "STATIC_ROOT", OwnedBy: "operator-computed"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(FindOwnedSettingOverrides(tt.settingNames, registry)).To(Equal(tt.want))
		})
	}
}

func TestOwnedSections(t *testing.T) {
	g := NewGomegaWithT(t)

	registry := []OwnedKey{
		{Section: "token", Key: "provider"},
		{Section: "fernet_tokens", Key: "max_active_keys"},
		{Section: "", Key: "STATIC_ROOT"},
		{Section: "token", Key: "expiration"},
	}

	// The two "token" entries collapse to a single set member and the flat
	// entry (empty Section) is excluded.
	g.Expect(OwnedSections(registry)).To(Equal(map[string]struct{}{
		"token":         {},
		"fernet_tokens": {},
	}))
}

func TestRecordExtraConfigHealth_NoOverrides(t *testing.T) {
	g := NewGomegaWithT(t)
	recorder := record.NewFakeRecorder(10)
	obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"}}
	var conds []metav1.Condition

	RecordExtraConfigHealth(recorder, obj, &conds, 7, nil)

	cond := conditions.GetCondition(conds, ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ConditionReasonNoOwnedKeysOverridden))
	g.Expect(cond.Message).To(Equal("spec.extraConfig overrides no operator-owned keys"))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))

	// No event on the healthy path.
	g.Expect(drainEvents(recorder.Events)).To(BeEmpty())
}

func TestRecordExtraConfigHealth_GatedEvent(t *testing.T) {
	g := NewGomegaWithT(t)
	recorder := record.NewFakeRecorder(10)
	obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"}}
	var conds []metav1.Condition

	impact := "the operator projects this many fernet keys into the pod; the override desynchronizes the rendered config from the mounted key material"
	overrides := []OwnedKey{
		{
			Section: "fernet_tokens",
			Key:     "max_active_keys",
			OwnedBy: "operator-computed",
			Impact:  impact,
		},
		{
			Section: "",
			Key:     "STATIC_ROOT",
			OwnedBy: "operator-computed",
		},
	}

	// Pins all three rendering forms: an INI entry with impact, a bare flat
	// entry without impact, joined with "; " after the fixed prefix.
	wantMessage := "spec.extraConfig overrides operator-owned keys: " +
		"[fernet_tokens] max_active_keys (" + impact + ")" +
		"; STATIC_ROOT"

	// First reconcile transitions ExtraConfigHealthy into False and emits one
	// Warning event.
	RecordExtraConfigHealth(recorder, obj, &conds, 1, overrides)

	cond := conditions.GetCondition(conds, ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(ConditionReasonOwnedKeysOverridden))
	g.Expect(cond.Message).To(Equal(wantMessage))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(1)))

	events := drainEvents(recorder.Events)
	g.Expect(events).To(HaveLen(1))
	g.Expect(events[0]).To(Equal("Warning " + EventReasonExtraConfigOwnedKeyOverride + " " + wantMessage))

	// Identical overrides on a later reconcile: the condition is upserted again
	// (ObservedGeneration advances) but no second event fires.
	RecordExtraConfigHealth(recorder, obj, &conds, 2, overrides)
	cond = conditions.GetCondition(conds, ConditionTypeExtraConfigHealthy)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(2)))
	g.Expect(drainEvents(recorder.Events)).To(BeEmpty())

	// A changed override set produces a different message and therefore fires
	// exactly one more event.
	changed := []OwnedKey{
		{Section: "fernet_tokens", Key: "max_active_keys", OwnedBy: "operator-computed"},
	}
	RecordExtraConfigHealth(recorder, obj, &conds, 3, changed)
	cond = conditions.GetCondition(conds, ConditionTypeExtraConfigHealthy)
	g.Expect(cond.Message).To(Equal("spec.extraConfig overrides operator-owned keys: [fernet_tokens] max_active_keys"))
	g.Expect(drainEvents(recorder.Events)).To(HaveLen(1))
}
