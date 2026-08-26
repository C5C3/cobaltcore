// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// dockerfileOVNVersionPattern captures the version off the single
// ARG OVN_VERSION line of images/ovn/Dockerfile. The image records the tag with
// its upstream "v" prefix, the operator constant without it, so the prefix is
// matched rather than captured.
var dockerfileOVNVersionPattern = regexp.MustCompile(`(?m)^ARG OVN_VERSION=v(.+)$`)

func TestEffectiveImage_NilResolvesDefault(t *testing.T) {
	g := NewWithT(t)

	image := effectiveImage(nil)

	g.Expect(image.Repository).To(Equal(defaultOVNRepository))
	g.Expect(image.Tag).To(Equal(defaultOVNVersion))
	g.Expect(image.Digest).To(BeEmpty())
	g.Expect(image.Reference()).To(Equal("ghcr.io/c5c3/ovn:" + defaultOVNVersion))
}

// An override reaches the workloads unchanged, digest included: a CR that pins a
// digest has closed the supply-chain gap a mutable tag leaves open, and a
// resolver that re-attached the default tag would reopen it.
func TestEffectiveImage_DigestPreserved(t *testing.T) {
	g := NewWithT(t)
	digest := "sha256:" + strings.Repeat("a", 64)

	image := effectiveImage(&commonv1.ImageSpec{Repository: "registry.example.com/ovn", Digest: digest})

	g.Expect(image.Tag).To(BeEmpty(), "a digest-pinned override must not gain the default tag")
	g.Expect(image.Digest).To(Equal(digest))
	g.Expect(image.Reference()).To(Equal("registry.example.com/ovn@" + digest))
}

func TestEffectiveShifterImage_NilResolvesLatest(t *testing.T) {
	g := NewWithT(t)

	image := effectiveShifterImage(nil)

	g.Expect(image.Repository).To(Equal(defaultBackupShifterRepository))
	g.Expect(image.Tag).To(Equal("latest"))
	g.Expect(image.Reference()).To(Equal("ghcr.io/c5c3/backup-shifter:latest"))
}

func TestEffectiveShifterImage_OverrideVerbatim(t *testing.T) {
	g := NewWithT(t)
	override := commonv1.ImageSpec{Repository: "registry.example.com/shifter", Tag: "2026.1"}

	g.Expect(effectiveShifterImage(&override)).To(Equal(override))
	g.Expect(effectiveShifterImage(&override).Reference()).To(Equal("registry.example.com/shifter:2026.1"))
}

// The operator default and the image it runs are two records of one version. A
// bump that lands in only one of them ships an operator whose default image tag
// names a tag the build never pushed, so the two are compared here rather than
// left to a reviewer.
func TestDefaultOVNVersionMatchesDockerfilePin(t *testing.T) {
	g := NewWithT(t)

	_, thisFile, _, ok := runtime.Caller(0)
	g.Expect(ok).To(BeTrue(), "runtime.Caller must resolve this test file to locate the Dockerfile")
	dockerfile := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "images", "ovn", "Dockerfile")

	content, err := os.ReadFile(filepath.Clean(dockerfile))
	g.Expect(err).NotTo(HaveOccurred(), "images/ovn/Dockerfile must be readable at %s", dockerfile)

	match := dockerfileOVNVersionPattern.FindSubmatch(content)
	g.Expect(match).NotTo(BeNil(),
		"images/ovn/Dockerfile carries no line matching %q; defaultOVNVersion has nothing to be checked against",
		dockerfileOVNVersionPattern.String())
	g.Expect(string(match[1])).To(Equal(defaultOVNVersion),
		"defaultOVNVersion and ARG OVN_VERSION in images/ovn/Dockerfile must name the same OVN version")
}
