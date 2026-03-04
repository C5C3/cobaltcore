// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
)

// Feature: CC-0002

func TestFakeCRDsDirs_returnsSubdirectories(t *testing.T) {
	g := NewGomegaWithT(t)

	dirs := fakeCRDsDirs()
	g.Expect(dirs).NotTo(BeEmpty(), "expected at least one CRD subdirectory")

	for _, dir := range dirs {
		g.Expect(dir).To(BeADirectory())
	}
}

func TestFakeCRDsDirs_eachSubdirContainsYAML(t *testing.T) {
	g := NewGomegaWithT(t)

	dirs := fakeCRDsDirs()
	g.Expect(dirs).NotTo(BeEmpty())

	for _, dir := range dirs {
		matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(matches).NotTo(BeEmpty(), "expected at least one YAML file in %s", dir)
	}
}

func TestSetupEnvTest(t *testing.T) {
	SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := SetupEnvTest(t)
	defer cancel()

	g.Expect(c).NotTo(BeNil())
	g.Expect(ctx).NotTo(BeNil())
}
