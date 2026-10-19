// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package celhost

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// crdPath is the generated CELHost CRD the envtest installs.
var crdPath = filepath.Join("crd", "test.c5c3.io_celhosts.yaml")

// typesDir is the shared types package whose CEL rules CELHostSpec embeds.
var typesDir = ".."

// markerMessage captures the message of one XValidation marker.
var markerMessage = regexp.MustCompile(`XValidation:rule=".*",message="([^"]*)"`)

// markerCELMessages returns the message of every XValidation marker in the
// non-test, non-generated Go sources of dir, with the number of markers that
// declare it. Every source is read, so a rule in a new file cannot escape the
// drift lock.
func markerCELMessages(dir string) (map[string]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	msgs := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" ||
			strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "zz_generated") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		for _, m := range markerMessage.FindAllSubmatch(data, -1) {
			msgs[string(m[1])]++
		}
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no CEL rules found in %s", dir)
	}
	return msgs, nil
}

// crdCELMessages returns the message of every x-kubernetes-validations rule in
// the schema of the CRD at path.
func crdCELMessages(path string) (map[string]struct{}, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	msgs := map[string]struct{}{}
	for _, v := range crd.Spec.Versions {
		if v.Schema != nil {
			collectCELMessages(v.Schema.OpenAPIV3Schema, msgs)
		}
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no CEL rules found in %s", path)
	}
	return msgs, nil
}

// collectCELMessages walks schema and every schema nested under it, adding
// each validation rule's message to msgs.
func collectCELMessages(schema *apiextensionsv1.JSONSchemaProps, msgs map[string]struct{}) {
	if schema == nil {
		return
	}
	for _, rule := range schema.XValidations {
		msgs[rule.Message] = struct{}{}
	}
	for _, prop := range schema.Properties {
		collectCELMessages(&prop, msgs)
	}
	if schema.AdditionalProperties != nil {
		collectCELMessages(schema.AdditionalProperties.Schema, msgs)
	}
	if schema.Items != nil {
		collectCELMessages(schema.Items.Schema, msgs)
	}
}

// TestCELHostCRD_CarriesEverySharedTypeRule locks the CELHost CRD to the CEL
// rules of the shared types: a rule declared on a shared type that CELHostSpec
// does not embed, or a CRD left stale after a marker changed, fails here before
// any envtest runs. The lock compares messages, so it also fails when two
// markers share one: the CRD entry of the embedded type would hide a type that
// is not embedded.
func TestCELHostCRD_CarriesEverySharedTypeRule(t *testing.T) {
	crd, err := crdCELMessages(crdPath)
	if err != nil {
		t.Fatal(err)
	}
	markers, err := markerCELMessages(typesDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, msg := range slices.Sorted(maps.Keys(markers)) {
		if n := markers[msg]; n > 1 {
			t.Errorf("marker message %q is declared %d times in the shared types: give each rule its own message", msg, n)
		}
		if _, ok := crd[msg]; !ok {
			t.Errorf("marker message %q is missing from the CRD: embed its type in CELHostSpec and run make manifests-common", msg)
		}
	}
	for _, msg := range slices.Sorted(maps.Keys(crd)) {
		if _, ok := markers[msg]; !ok {
			t.Errorf("CRD message %q has no marker in the shared types: run make manifests-common", msg)
		}
	}
	t.Logf("%d marker messages, %d CRD messages", len(markers), len(crd))
}

// TestCELMessages_Errors pins the failure modes of the drift lock: an input
// that cannot be read or parsed, and an input without a rule, fail with an
// error naming the path instead of comparing two empty sets. Rules in test
// files and generated files do not count.
func TestCELMessages_Errors(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return path
	}
	expectError := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil {
			t.Fatalf("expected an error starting with %q, got nil", want)
		}
		if !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("expected an error starting with %q, got %q", want, err)
		}
	}

	t.Run("marker sources without rules fail with no CEL rules found", func(t *testing.T) {
		rule := "package x\n\n// +kubebuilder:validation:XValidation:rule=\"true\",message=\"skipped\"\n"
		write(t, "norules.go", "package x\n\n// Spec has a marker but no CEL rule.\n// +kubebuilder:validation:MinLength=1\n")
		write(t, "norules_test.go", rule)
		write(t, "zz_generated.deepcopy.go", rule)
		_, err := markerCELMessages(dir)
		expectError(t, err, "no CEL rules found in "+dir)
	})
	t.Run("an unreadable marker directory fails with the read error", func(t *testing.T) {
		path := filepath.Join(dir, "missing")
		_, err := markerCELMessages(path)
		expectError(t, err, "reading "+path+":")
	})
	t.Run("a CRD without rules fails with no CEL rules found", func(t *testing.T) {
		path := write(t, "norules.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
`)
		_, err := crdCELMessages(path)
		expectError(t, err, "no CEL rules found in "+path)
	})
	t.Run("an unreadable CRD fails with the read error", func(t *testing.T) {
		path := filepath.Join(dir, "missing.yaml")
		_, err := crdCELMessages(path)
		expectError(t, err, "reading "+path+":")
	})
	t.Run("a CRD that is no YAML fails with the parse error", func(t *testing.T) {
		path := write(t, "broken.yaml", "spec: [unclosed\n")
		_, err := crdCELMessages(path)
		expectError(t, err, "parsing "+path+":")
	})
}
