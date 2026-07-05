// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package pysettings

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func jsonVal(s string) apiextensionsv1.JSON {
	return apiextensionsv1.JSON{Raw: []byte(s)}
}

func TestRender_ScalarConversionTable(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "true becomes True", raw: `true`, want: "X = True\n"},
		{name: "false becomes False", raw: `false`, want: "X = False\n"},
		{name: "null becomes None", raw: `null`, want: "X = None\n"},
		{name: "string keeps quotes", raw: `"hello"`, want: "X = \"hello\"\n"},
		{name: "integer stays integral", raw: `42`, want: "X = 42\n"},
		{name: "float keeps representation", raw: `1.5`, want: "X = 1.5\n"},
		{name: "list renders as python list", raw: `["a", 1, true]`, want: "X = [\"a\", 1, True]\n"},
		{name: "nested dict renders sorted", raw: `{"b": 1, "a": {"c": null}}`, want: "X = {\"a\": {\"c\": None}, \"b\": 1}\n"},
		{name: "string with quote is escaped", raw: `"it\"s"`, want: "X = \"it\\\"s\"\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(nil, map[string]apiextensionsv1.JSON{"X": jsonVal(tc.raw)})
			if err != nil {
				t.Fatalf("Render returned error: %v", err)
			}
			if got != tc.want {
				t.Errorf("Render = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRender_SortsSettingsByKey(t *testing.T) {
	got, err := Render(nil, map[string]apiextensionsv1.JSON{
		"ZULU":  jsonVal(`1`),
		"ALPHA": jsonVal(`2`),
		"MIKE":  jsonVal(`3`),
	})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	want := "ALPHA = 2\nMIKE = 3\nZULU = 1\n"
	if got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

func TestRender_PreambleEmittedVerbatimWithSeparator(t *testing.T) {
	got, err := Render(
		[]string{"import os", `SECRET_KEY = os.environ["HORIZON_SECRET_KEY"]`},
		map[string]apiextensionsv1.JSON{"DEBUG": jsonVal(`false`)},
	)
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	want := "import os\nSECRET_KEY = os.environ[\"HORIZON_SECRET_KEY\"]\n\nDEBUG = False\n"
	if got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

func TestRender_PreambleOnlyOmitsSeparator(t *testing.T) {
	got, err := Render([]string{"import os"}, nil)
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if got != "import os\n" {
		t.Errorf("Render = %q, want %q", got, "import os\n")
	}
}

func TestRender_GoldenFullModule(t *testing.T) {
	got, err := Render(
		[]string{"import os", `SECRET_KEY = os.environ["HORIZON_SECRET_KEY"]`},
		map[string]apiextensionsv1.JSON{
			"SESSION_ENGINE": jsonVal(`"django.contrib.sessions.backends.signed_cookies"`),
			"ALLOWED_HOSTS":  jsonVal(`["*"]`),
			"CACHES":         jsonVal(`{"default": {"BACKEND": "django.core.cache.backends.memcached.PyMemcacheCache", "LOCATION": ["memcached:11211"]}}`),
			"DEBUG":          jsonVal(`false`),
		},
	)
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	want := `import os
SECRET_KEY = os.environ["HORIZON_SECRET_KEY"]

ALLOWED_HOSTS = ["*"]
CACHES = {"default": {"BACKEND": "django.core.cache.backends.memcached.PyMemcacheCache", "LOCATION": ["memcached:11211"]}}
DEBUG = False
SESSION_ENGINE = "django.contrib.sessions.backends.signed_cookies"
`
	if got != want {
		t.Errorf("Render mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRender_InvalidJSONReturnsError(t *testing.T) {
	_, err := Render(nil, map[string]apiextensionsv1.JSON{"BROKEN": jsonVal(`{not json`)})
	if err == nil {
		t.Fatal("Render accepted invalid JSON, want error")
	}
}

func TestRender_EmptyValueReturnsError(t *testing.T) {
	_, err := Render(nil, map[string]apiextensionsv1.JSON{"EMPTY": jsonVal(``)})
	if err == nil {
		t.Fatal("Render accepted empty raw value, want error")
	}
}

func TestRender_EmptySettingNameReturnsError(t *testing.T) {
	_, err := Render(nil, map[string]apiextensionsv1.JSON{"": jsonVal(`1`)})
	if err == nil {
		t.Fatal("Render accepted empty setting name, want error")
	}
}

// TestRender_InvalidSettingNameReturnsError asserts that keys which are not
// valid Python identifiers are rejected. Setting names are emitted verbatim
// as assignment targets, so an unescaped key (an embedded newline, a space,
// or a leading digit) would inject arbitrary statements into the module.
func TestRender_InvalidSettingNameReturnsError(t *testing.T) {
	names := []string{
		"X\nimport os",  // newline injects a statement at import time
		"SECRET_KEY ",   // trailing space evades an exact-match key guard
		"bad name",      // embedded space
		"1X",            // leading digit
		"a.b",           // attribute access is not an assignment target
		"X = 1; import", // operators and separators
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			_, err := Render(nil, map[string]apiextensionsv1.JSON{name: jsonVal(`1`)})
			if err == nil {
				t.Fatalf("Render accepted invalid setting name %q, want error", name)
			}
		})
	}
}
