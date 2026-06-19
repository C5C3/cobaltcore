// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
)

// TestDetectOperatorNamespace_PodNamespaceEnvWins verifies that POD_NAMESPACE,
// when set, takes precedence over the ServiceAccount namespace file.
func TestDetectOperatorNamespace_PodNamespaceEnvWins(t *testing.T) {
	g := NewGomegaWithT(t)

	// Point the SA file at a value that must be ignored when the env is set.
	saFile := filepath.Join(t.TempDir(), "namespace")
	g.Expect(os.WriteFile(saFile, []byte("from-file\n"), 0o600)).To(Succeed())
	withServiceAccountNamespacePath(t, saFile)

	t.Setenv("POD_NAMESPACE", "keystone-system")

	g.Expect(DetectOperatorNamespace()).To(Equal("keystone-system"))
}

// TestDetectOperatorNamespace_FallsBackToServiceAccountFile verifies that the
// ServiceAccount namespace file is read when POD_NAMESPACE is unset.
func TestDetectOperatorNamespace_FallsBackToServiceAccountFile(t *testing.T) {
	g := NewGomegaWithT(t)
	t.Setenv("POD_NAMESPACE", "")

	saFile := filepath.Join(t.TempDir(), "namespace")
	g.Expect(os.WriteFile(saFile, []byte("openstack"), 0o600)).To(Succeed())
	withServiceAccountNamespacePath(t, saFile)

	g.Expect(DetectOperatorNamespace()).To(Equal("openstack"))
}

// TestDetectOperatorNamespace_TrimsWhitespace verifies that a trailing newline
// or surrounding whitespace in either source is trimmed.
func TestDetectOperatorNamespace_TrimsWhitespace(t *testing.T) {
	g := NewGomegaWithT(t)

	saFile := filepath.Join(t.TempDir(), "namespace")
	g.Expect(os.WriteFile(saFile, []byte("  spaced-ns\n"), 0o600)).To(Succeed())
	withServiceAccountNamespacePath(t, saFile)

	t.Setenv("POD_NAMESPACE", "  env-ns\n")
	g.Expect(DetectOperatorNamespace()).To(Equal("env-ns"))

	t.Setenv("POD_NAMESPACE", "")
	g.Expect(DetectOperatorNamespace()).To(Equal("spaced-ns"))
}

// TestDetectOperatorNamespace_NeitherAvailableReturnsEmpty verifies that the
// function returns "" when POD_NAMESPACE is unset and the ServiceAccount file
// is absent — the out-of-cluster case.
func TestDetectOperatorNamespace_NeitherAvailableReturnsEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	t.Setenv("POD_NAMESPACE", "")
	withServiceAccountNamespacePath(t, filepath.Join(t.TempDir(), "does-not-exist"))

	g.Expect(DetectOperatorNamespace()).To(Equal(""))
}

// withServiceAccountNamespacePath repoints the package-level SA namespace path
// at the given file for the duration of the test and restores it afterwards.
func withServiceAccountNamespacePath(t *testing.T, path string) {
	t.Helper()
	original := serviceAccountNamespacePath
	serviceAccountNamespacePath = path
	t.Cleanup(func() { serviceAccountNamespacePath = original })
}
