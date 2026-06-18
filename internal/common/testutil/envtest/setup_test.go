// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

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

func TestSharedScheme_registersExternalOperatorTypes(t *testing.T) {
	s := SharedScheme()

	tests := []struct {
		name string
		gvk  schema.GroupVersionKind
	}{
		// MariaDB operator types
		{"MariaDB", schema.GroupVersionKind{Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "MariaDB"}},
		{"Database", schema.GroupVersionKind{Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Database"}},
		{"User", schema.GroupVersionKind{Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "User"}},
		{"Grant", schema.GroupVersionKind{Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Grant"}},
		// ESO types (v1)
		{"ExternalSecret", schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}},
		// ESO types (v1alpha1)
		{"PushSecret", schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1alpha1", Kind: "PushSecret"}},
		// cert-manager types
		{"Certificate", schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			recognized := s.Recognizes(tt.gvk)
			g.Expect(recognized).To(BeTrue(), "SharedScheme should recognize %s", tt.gvk)
		})
	}
}
