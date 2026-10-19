// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// fuzzINIKeyPattern is the option-name charset the webhooks enforce on
// CR-set keys (validation.DefaultExtraOptionKeyPattern), restated here so the
// config tests do not import the validation package.
var fuzzINIKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// FuzzRenderINIMulti checks that the INI renderer is deterministic, that a
// well-formed section with two keys renders its keys in sorted order to lines
// a minimal INI reader recovers byte-equal, and that CheckNoControlChars fires
// exactly when a key or value carries a line break.
func FuzzRenderINIMulti(f *testing.F) {
	f.Add("DEFAULT", "debug", "verbose", "true", "false")
	// The special-character rows of TestRenderINI.
	f.Add("database", "connection", "backend",
		"mysql+pymysql://user:p@ss=word@host:3306/db?charset=utf8",
		"mysql+pymysql://user:p%25ssword@host:3306/db")
	// The repeated-key row of TestRenderINIMulti.
	f.Add("federation", "trusted_dashboard", "remote_id_attribute",
		"https://b.example.com/auth/websso/", "https://a.example.com/auth/websso/")
	f.Add("DEFAULT", "keystone_user", "keystone_password", "", "")
	f.Add("", "", "", "", "")
	// The injection row of TestCacheNoControlChars.
	f.Add("cache", "servers", "backend", "mc-0:11211\nauth_url = http://attacker.example/v3", "x")

	f.Fuzz(func(t *testing.T, section, key, key2, v1, v2 string) {
		sections := map[string]map[string][]string{section: {key: {v1, v2}}}
		if key2 != key {
			sections[section][key2] = []string{v1}
		}
		out := RenderINIMulti(sections)
		if again := RenderINIMulti(sections); again != out {
			t.Fatalf("determinism: RenderINIMulti(%q) returned %q, then %q", sections, out, again)
		}
		single := map[string]map[string]string{section: {key: v1}}
		if got, want := RenderINI(single), RenderINIMulti(LiftSections(single)); got != want {
			t.Fatalf("single-valued adapter: RenderINI(%q) = %q, want %q", single, got, want)
		}

		wellFormed := section != "" && !strings.ContainsAny(section, "\n\r[]") &&
			fuzzINIKeyPattern.MatchString(key) && fuzzINIKeyPattern.MatchString(key2) && key != key2 &&
			!strings.ContainsAny(v1, "\n\r") && !strings.ContainsAny(v2, "\n\r")
		if wellFormed {
			lines := map[string]string{
				key:  key + " = " + v1 + "\n" + key + " = " + v2 + "\n",
				key2: key2 + " = " + v1 + "\n",
			}
			if want := "[" + section + "]\n" + lines[min(key, key2)] + lines[max(key, key2)]; out != want {
				t.Fatalf("parse back: RenderINIMulti(%q) = %q, want %q", sections, out, want)
			}
			if parsed := parseINILines(out); !reflect.DeepEqual(parsed, sections) {
				t.Fatalf("parse back: %q parses to %q, want %q", out, parsed, sections)
			}
		}

		err := CheckNoControlChars(section, map[string]string{key: v1})
		if hasLineBreak := strings.ContainsAny(key, "\n\r") || strings.ContainsAny(v1, "\n\r"); (err != nil) != hasLineBreak {
			t.Fatalf("control chars: CheckNoControlChars(%q, {%q: %q}) = %v, want an error %t", section, key, v1, err, hasLineBreak)
		}
		if err == nil {
			return
		}
		if !IsControlCharError(err) {
			t.Fatalf("control chars: CheckNoControlChars(%q, {%q: %q}) returned %T, want *ControlCharError", section, key, v1, err)
		}
		if want := "[" + section + "] option name or value contains a newline or carriage-return character"; err.Error() != want {
			t.Fatalf("control chars: error %q, want %q for section %q", err.Error(), want, section)
		}
	})
}

// parseINILines is a minimal INI reader for the renderer's output: a "[name]"
// line opens a section, a blank line is skipped, and any other line splits at
// its first " = " into key and value. A repeated key appends its value.
func parseINILines(out string) map[string]map[string][]string {
	sections := map[string]map[string][]string{}
	var current string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			current = line[1 : len(line)-1]
		default:
			key, value, _ := strings.Cut(line, " = ")
			if sections[current] == nil {
				sections[current] = map[string][]string{}
			}
			sections[current][key] = append(sections[current][key], value)
		}
	}
	return sections
}
