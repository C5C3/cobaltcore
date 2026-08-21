// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
)

// OwnedKey identifies a single operator-owned configuration key and how the
// operator treats a user override of it. Section is the INI section name and is
// empty for flat Django settings (Horizon). OwnedBy names the source of the
// value ("operator-computed" or a typed field path such as
// "spec.federation.trustedDashboards"). Impact is an optional one-sentence
// consequence note included verbatim in condition and event messages. Rejected
// marks a key whose override is blocked at admission by the service's validating
// webhook; the zero value marks a key whose override is honored but surfaced
// through the ExtraConfigHealthy condition and a Warning event.
type OwnedKey struct {
	Section  string
	Key      string
	OwnedBy  string
	Impact   string
	Rejected bool
}

const (
	// ConditionTypeExtraConfigHealthy is the status condition type reporting
	// whether spec.extraConfig overrides any operator-owned key.
	ConditionTypeExtraConfigHealthy = "ExtraConfigHealthy"
	// ConditionReasonOwnedKeysOverridden is the ExtraConfigHealthy=False reason
	// used when spec.extraConfig overrides at least one operator-owned key.
	ConditionReasonOwnedKeysOverridden = "OwnedKeysOverridden"
	// ConditionReasonNoOwnedKeysOverridden is the ExtraConfigHealthy=True reason
	// used when spec.extraConfig overrides no operator-owned key.
	ConditionReasonNoOwnedKeysOverridden = "NoOwnedKeysOverridden"
	// EventReasonExtraConfigOwnedKeyOverride is the reason on the Warning event
	// emitted when spec.extraConfig first overrides an operator-owned key.
	EventReasonExtraConfigOwnedKeyOverride = "ExtraConfigOwnedKeyOverride"
)

// FindOwnedOverrides returns the registry entries, reported or rejected, whose
// (Section, Key) appears in extraConfig. The result is sorted by Section then
// Key so condition and event messages are deterministic and independent of
// registry order. It returns nil for a nil or empty extraConfig and nil when
// nothing matches.
func FindOwnedOverrides(extraConfig map[string]map[string]string, registry []OwnedKey) []OwnedKey {
	if len(extraConfig) == 0 {
		return nil
	}
	var matched []OwnedKey
	for _, entry := range registry {
		keys, ok := extraConfig[entry.Section]
		if !ok {
			continue
		}
		if _, ok := keys[entry.Key]; ok {
			matched = append(matched, entry)
		}
	}
	sortOwnedKeys(matched)
	return matched
}

// FindOwnedSettingOverrides is the flat variant of FindOwnedOverrides for
// Horizon: callers pass the keys of their map[string]apiextensionsv1.JSON
// extraConfig. It matches only registry entries with an empty Section and
// returns them sorted by Key. It returns nil for a nil or empty settingNames
// and nil when nothing matches.
func FindOwnedSettingOverrides(settingNames []string, registry []OwnedKey) []OwnedKey {
	if len(settingNames) == 0 {
		return nil
	}
	names := make(map[string]struct{}, len(settingNames))
	for _, name := range settingNames {
		names[name] = struct{}{}
	}
	var matched []OwnedKey
	for _, entry := range registry {
		if entry.Section != "" {
			continue
		}
		if _, ok := names[entry.Key]; ok {
			matched = append(matched, entry)
		}
	}
	sortOwnedKeys(matched)
	return matched
}

// OwnedSections returns the set of non-empty Section values present in the
// registry. Callers use it to decide which INI sections carry operator-owned
// keys.
func OwnedSections(registry []OwnedKey) map[string]struct{} {
	sections := make(map[string]struct{})
	for _, entry := range registry {
		if entry.Section != "" {
			sections[entry.Section] = struct{}{}
		}
	}
	return sections
}

// sortOwnedKeys orders keys by Section then Key in place so message rendering is
// deterministic regardless of the order registry entries were declared in.
func sortOwnedKeys(keys []OwnedKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Section != keys[j].Section {
			return keys[i].Section < keys[j].Section
		}
		return keys[i].Key < keys[j].Key
	})
}

// RecordExtraConfigHealth upserts the ExtraConfigHealthy condition on conds on
// every call and emits a Warning event only on a transition, mirroring the
// gated-event pattern service operators use for spec-derived health signals.
//
// With no overrides it sets ExtraConfigHealthy=True, Reason
// NoOwnedKeysOverridden and message "spec.extraConfig overrides no
// operator-owned keys", and emits nothing. With one or more overrides it sets
// ExtraConfigHealthy=False, Reason OwnedKeysOverridden and a message listing the
// overridden keys, and emits a single EventTypeWarning event with reason
// ExtraConfigOwnedKeyOverride carrying that same message when the previous
// condition is absent, not False, or carries a different Message. Gating on the
// Message (not the Reason) re-fires the event once when the overridden-key set
// changes while the reconcile poll never floods the event stream.
func RecordExtraConfigHealth(recorder record.EventRecorder, obj runtime.Object, conds *[]metav1.Condition, generation int64, overrides []OwnedKey) {
	if len(overrides) == 0 {
		conditions.SetCondition(conds, metav1.Condition{
			Type:               ConditionTypeExtraConfigHealthy,
			Status:             metav1.ConditionTrue,
			Reason:             ConditionReasonNoOwnedKeysOverridden,
			ObservedGeneration: generation,
			Message:            "spec.extraConfig overrides no operator-owned keys",
		})
		return
	}

	message := "spec.extraConfig overrides operator-owned keys: " + renderOwnedOverrides(overrides)

	prev := conditions.GetCondition(*conds, ConditionTypeExtraConfigHealthy)
	if prev == nil || prev.Status != metav1.ConditionFalse || prev.Message != message {
		recorder.Event(obj, corev1.EventTypeWarning, EventReasonExtraConfigOwnedKeyOverride, message)
	}

	conditions.SetCondition(conds, metav1.Condition{
		Type:               ConditionTypeExtraConfigHealthy,
		Status:             metav1.ConditionFalse,
		Reason:             ConditionReasonOwnedKeysOverridden,
		ObservedGeneration: generation,
		Message:            message,
	})
}

// renderOwnedOverrides renders overrides into the human-readable tail of the
// ExtraConfigHealthy message: an INI entry as "[section] key", a flat entry as
// "key", each followed by " (impact)" when Impact is non-empty, joined with
// "; ". Callers pass an already-sorted slice.
func renderOwnedOverrides(overrides []OwnedKey) string {
	parts := make([]string, 0, len(overrides))
	for _, o := range overrides {
		var entry string
		if o.Section != "" {
			entry = fmt.Sprintf("[%s] %s", o.Section, o.Key)
		} else {
			entry = o.Key
		}
		if o.Impact != "" {
			entry += fmt.Sprintf(" (%s)", o.Impact)
		}
		parts = append(parts, entry)
	}
	return strings.Join(parts, "; ")
}
