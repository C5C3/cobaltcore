// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/federation"

	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
)

// crdToWireKeys maps the mapping-rule JSON keys the CRD spells differently
// from keystone's wire format. Every other key is the same on both sides.
var crdToWireKeys = map[string]string{
	"anyOneOf": "any_one_of",
	"notAnyOf": "not_any_of",
	"groupIds": "group_ids",
}

// FuzzToGophercloudMappingRules checks that the converter keeps every rule,
// that its output equals what json.Unmarshal makes of its own wire JSON (the
// drift compare in ensureMappingAndGroups), and that no field is lost or
// misnamed between the CRD shape and the wire shape.
func FuzzToGophercloudMappingRules(f *testing.F) {
	for _, rules := range [][]keystonev1alpha1.MappingRuleSpec{
		fullShapeMappingRules(),
		testOIDCBackend("corp-oidc", "corp").Spec.Mappings,
	} {
		seed, err := json.Marshal(rules)
		if err != nil {
			f.Fatalf("marshaling seed rules: %v", err)
		}
		f.Add(seed)
	}
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))
	// A project without roles and an empty anyOneOf, both omitted on the wire.
	f.Add([]byte(`[{"local":[{"projects":[{"name":"demo"}]}],"remote":[{"type":"HTTP_OIDC_ISS","anyOneOf":[]}]}]`))
	// The typed fields the seeds above leave unset, so the seed run checks
	// each rename and copy.
	f.Add([]byte(`[{"local":[{"domain":{"id":"d1"},"groupIds":"{0}","group":{"id":"g1","domain":{"id":"d1"}},` +
		`"user":{"id":"u1","name":"{1}"}}],"remote":[{"type":"A","notAnyOf":["x"],"blacklist":["y"]},` +
		`{"type":"B","whitelist":["z"]},{"type":"C","notAnyOf":[],"blacklist":[],"whitelist":[]}]}]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var rules []keystonev1alpha1.MappingRuleSpec
		if err := json.Unmarshal(data, &rules); err != nil {
			// The API server never stores a mapping that does not decode.
			t.Skip()
		}

		out := toGophercloudMappingRules(rules)
		if len(rules) == 0 {
			if out != nil {
				t.Fatalf("rule count: no rules must convert to nil, got %#v for %q", out, data)
			}
		} else {
			if len(out) != len(rules) {
				t.Fatalf("rule count: got %d rules, want %d for %q", len(out), len(rules), data)
			}
			for i := range rules {
				if len(out[i].Local) != len(rules[i].Local) || len(out[i].Remote) != len(rules[i].Remote) {
					t.Fatalf("rule count: rule %d has %d local and %d remote entries, want %d and %d for %q",
						i, len(out[i].Local), len(out[i].Remote), len(rules[i].Local), len(rules[i].Remote), data)
				}
			}
		}

		payload, err := json.Marshal(federation.CreateMappingOpts{Rules: out})
		if err != nil {
			t.Fatalf("wire round trip: marshaling %#v: %v for %q", out, err, data)
		}
		var back federation.CreateMappingOpts
		if err := json.Unmarshal(payload, &back); err != nil {
			t.Fatalf("wire round trip: unmarshaling %s: %v for %q", payload, err, data)
		}
		if !reflect.DeepEqual(back.Rules, out) {
			t.Fatalf("wire round trip: rules read back from %s are %#v, want %#v for %q", payload, back.Rules, out, data)
		}

		crdJSON, err := json.Marshal(rules)
		if err != nil {
			t.Fatalf("CRD-to-wire keys: marshaling the CRD rules: %v for %q", err, data)
		}
		var crdTree, wireTree any
		if err := json.Unmarshal(crdJSON, &crdTree); err != nil {
			t.Fatalf("CRD-to-wire keys: decoding %s: %v for %q", crdJSON, err, data)
		}
		if err := json.Unmarshal(payload, &wireTree); err != nil {
			t.Fatalf("CRD-to-wire keys: decoding %s: %v for %q", payload, err, data)
		}
		// The wire payload carries the rules under "rules"; wrapping the CRD
		// side the same way lets a top-level [] and null prune alike.
		want := normalizeMappingTree(map[string]any{"rules": crdTree}, crdToWireKeys)
		got := normalizeMappingTree(wireTree, nil)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CRD-to-wire keys: wire tree %#v differs from CRD tree %#v for %q", got, want, data)
		}
	})
}

// normalizeMappingTree returns a copy of a decoded JSON tree with every map key
// found in rename replaced by its value, and with every map entry dropped whose
// value is null, false, "", an empty list or an empty object once its own
// children are pruned. List elements are kept, so list lengths still compare.
// The pruning absorbs the different omitempty sets of the CRD and wire types.
func normalizeMappingTree(v any, rename map[string]string) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			child = normalizeMappingTree(child, rename)
			if isEmptyJSONValue(child) {
				continue
			}
			if wire, ok := rename[k]; ok {
				k = wire
			}
			out[k] = child
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = normalizeMappingTree(child, rename)
		}
		return out
	default:
		return v
	}
}

// isEmptyJSONValue reports whether a decoded JSON value is null, false, "",
// an empty list or an empty object.
func isEmptyJSONValue(v any) bool {
	switch val := v.(type) {
	case nil:
		return true
	case bool:
		return !val
	case string:
		return val == ""
	case []any:
		return len(val) == 0
	case map[string]any:
		return len(val) == 0
	default:
		return false
	}
}
