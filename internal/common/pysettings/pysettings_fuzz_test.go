// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package pysettings

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// FuzzRender checks that Render rejects an invalid setting name or an empty
// value, renders an accepted setting as exactly one line (so no value injects
// a statement), keeps the value's Python literal faithful to the decoded JSON,
// and places the preamble verbatim in front of the settings.
func FuzzRender(f *testing.F) {
	// The raw values of TestRender_ScalarConversionTable.
	for _, raw := range []string{
		`true`, `false`, `null`, `"hello"`, `42`, `1.5`,
		`["a", 1, true]`, `{"b": 1, "a": {"c": null}}`, `"it\"s"`,
	} {
		f.Add("X", []byte(raw))
	}
	// The names of TestRender_InvalidSettingNameReturnsError.
	for _, name := range []string{
		"X\nimport os", "SECRET_KEY ", "bad name", "1X", "a.b", "X = 1; import",
	} {
		f.Add(name, []byte(`1`))
	}
	f.Add("BROKEN", []byte(`{not json`))
	f.Add("EMPTY", []byte(``))
	f.Add("", []byte(`1`))
	// A string whose decoded value holds a newline and a tab.
	f.Add("X", []byte(`"a\nb\tc"`))
	f.Add("X", []byte(`  42  `))

	f.Fuzz(func(t *testing.T, name string, raw []byte) {
		settings := map[string]apiextensionsv1.JSON{name: {Raw: raw}}
		out, err := Render(nil, settings)
		again, errAgain := Render(nil, settings)
		if again != out || (errAgain == nil) != (err == nil) {
			t.Fatalf("determinism: Render(%q, %q) returned (%q, %v), then (%q, %v)", name, raw, out, err, again, errAgain)
		}
		if !settingNameRE.MatchString(name) && (err == nil || out != "") {
			t.Fatalf("invalid name: Render(%q, %q) = (%q, %v), want an error and no output", name, raw, out, err)
		}
		if len(bytes.TrimSpace(raw)) == 0 && (err == nil || out != "") {
			t.Fatalf("empty value: Render(%q, %q) = (%q, %v), want an error and no output", name, raw, out, err)
		}
		if err != nil && out != "" {
			t.Fatalf("output on error: Render(%q, %q) = (%q, %v), want no output", name, raw, out, err)
		}

		withPreamble, errPreamble := Render([]string{"import os"}, settings)
		if (errPreamble == nil) != (err == nil) {
			t.Fatalf("preamble: Render with a preamble returned %v, without %v, for (%q, %q)", errPreamble, err, name, raw)
		}
		if err != nil {
			return
		}
		if want := "import os\n\n" + out; withPreamble != want {
			t.Fatalf("preamble: Render with a preamble = %q, want %q for (%q, %q)", withPreamble, want, name, raw)
		}

		prefix := name + " = "
		if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") || !strings.HasPrefix(out, prefix) {
			t.Fatalf("one line: Render(%q, %q) = %q, want one %q line", name, raw, out, prefix+"<literal>")
		}
		lit := strings.TrimSuffix(strings.TrimPrefix(out, prefix), "\n")
		if strings.ContainsAny(lit, "\n\r") {
			t.Fatalf("one line: literal %q of (%q, %q) contains a line break", lit, name, raw)
		}

		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			t.Fatalf("literal: Render accepted %q, which does not decode: %v", raw, err)
		}
		switch v := value.(type) {
		case string:
			if unquoted, err := strconv.Unquote(lit); err != nil || unquoted != v {
				t.Fatalf("literal: unquoting %q gave (%q, %v), want %q for %q", lit, unquoted, err, v, raw)
			}
		case []any:
			if !strings.HasPrefix(lit, "[") || !strings.HasSuffix(lit, "]") {
				t.Fatalf("literal: list %q rendered as %q, want [...]", raw, lit)
			}
		case map[string]any:
			if !strings.HasPrefix(lit, "{") || !strings.HasSuffix(lit, "}") {
				t.Fatalf("literal: object %q rendered as %q, want {...}", raw, lit)
			}
		case nil:
			if lit != "None" {
				t.Fatalf("literal: null %q rendered as %q, want None", raw, lit)
			}
		case bool:
			if want := map[bool]string{true: "True", false: "False"}[v]; lit != want {
				t.Fatalf("literal: boolean %q rendered as %q, want %s", raw, lit, want)
			}
		case json.Number:
			if lit != v.String() {
				t.Fatalf("literal: number %q rendered as %q, want %s", raw, lit, v.String())
			}
		default:
			t.Fatalf("literal: %q decoded to unexpected type %T", raw, value)
		}
	})
}
