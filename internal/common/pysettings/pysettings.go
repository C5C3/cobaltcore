// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package pysettings renders Django-style Python settings files from
// structured values. It is the Python-settings sibling of
// internal/common/config (which renders oslo.config INI): Django services
// such as Horizon consume a local_settings.py module, not an INI file, so
// their operators render assignments like `NAME = <python-literal>` instead
// of INI sections.
//
// Values are modeled as apiextensionsv1.JSON so a CRD can carry free-form
// typed settings (spec.extraConfig) without widening the API to
// interface{}. JSON scalars and containers map onto Python literals
// structurally: true/false/null become True/False/None, strings and numbers
// keep their JSON forms, and lists/objects render as Python lists/dicts with
// deterministically sorted keys so the rendered file — and therefore any
// content-addressed ConfigMap name derived from it — is stable across
// renders.
package pysettings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// settingNameRE matches a valid Python identifier. Setting names become the
// left-hand side of a bare `NAME = <literal>` assignment, so a name outside
// this set (an embedded newline, space, or operator) would inject arbitrary
// statements at import time rather than assign a value. Values are safely
// escaped; keys are not, so the renderer validates them as the last line of
// defense even though callers such as the Horizon webhook validate first.
var settingNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Render produces a Python settings module from the given preamble lines and
// settings map. Preamble lines are emitted verbatim (imports, env lookups)
// followed by a blank line, then one `NAME = <literal>` assignment per
// settings entry, sorted by key. It returns an error when a value does not
// parse as JSON or a setting name is not a valid Python identifier.
func Render(preamble []string, settings map[string]apiextensionsv1.JSON) (string, error) {
	var b strings.Builder
	for _, line := range preamble {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if len(preamble) > 0 && len(settings) > 0 {
		b.WriteString("\n")
	}

	keys := make([]string, 0, len(settings))
	for k := range settings {
		if !settingNameRE.MatchString(k) {
			return "", fmt.Errorf("setting name %q is not a valid Python identifier", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		literal, err := jsonToPythonLiteral(settings[k].Raw)
		if err != nil {
			return "", fmt.Errorf("rendering setting %s: %w", k, err)
		}
		b.WriteString(k)
		b.WriteString(" = ")
		b.WriteString(literal)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// jsonToPythonLiteral converts a raw JSON value into the equivalent Python
// literal. Numbers pass through verbatim (json.Number preserves the source
// representation, so integers never gain a trailing .0), and object keys are
// sorted for deterministic output.
func jsonToPythonLiteral(raw []byte) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", fmt.Errorf("value is empty")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("parsing JSON value: %w", err)
	}
	return valueToPythonLiteral(v)
}

// valueToPythonLiteral recursively converts a decoded JSON value into a
// Python literal.
func valueToPythonLiteral(v any) (string, error) {
	switch val := v.(type) {
	case nil:
		return "None", nil
	case bool:
		if val {
			return "True", nil
		}
		return "False", nil
	case json.Number:
		return val.String(), nil
	case string:
		// strconv.Quote emits a double-quoted string whose escape forms
		// (\", \\, \n, \xHH, \uHHHH, \UHHHHHHHH) are all valid Python 3
		// string-literal escapes.
		return strconv.Quote(val), nil
	case []any:
		items := make([]string, 0, len(val))
		for _, item := range val {
			lit, err := valueToPythonLiteral(item)
			if err != nil {
				return "", err
			}
			items = append(items, lit)
		}
		return "[" + strings.Join(items, ", ") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, k := range keys {
			lit, err := valueToPythonLiteral(val[k])
			if err != nil {
				return "", err
			}
			pairs = append(pairs, strconv.Quote(k)+": "+lit)
		}
		return "{" + strings.Join(pairs, ", ") + "}", nil
	default:
		return "", fmt.Errorf("unsupported JSON value type %T", v)
	}
}
