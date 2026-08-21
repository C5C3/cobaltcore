// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Placement operator entrypoint wiring. They assert against the
// package-level scheme this binary hands to bootstrap.Run, without standing up
// a cluster or envtest.
package main

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/operators/placement/internal/controller"
)

// The remote child kinds drive two things at once: SetupWithManager builds a
// watch object per entry from this scheme, and the teardown sweep lists through
// the same list. A kind this binary's scheme does not know therefore fails
// SetupWithManager and the operator exits at startup.
//
// Nothing else in the tree catches that. The controller package's own tests run
// against a scheme they build themselves, so a kind added to the list without
// its AddToScheme in main.go passes every unit and integration test and only
// surfaces on a real deploy.
func TestRemoteChildKindsAreRegisteredInTheScheme(t *testing.T) {
	for _, gvk := range controller.PlacementRemoteChildKinds {
		t.Run(gvk.String(), func(t *testing.T) {
			g := NewGomegaWithT(t)

			obj, err := scheme.New(gvk)

			g.Expect(err).NotTo(HaveOccurred(),
				"%s is swept and watched on target clusters, so this binary's scheme has to know it", gvk)

			_, isObject := obj.(client.Object)
			g.Expect(isObject).To(BeTrue(),
				"%s must carry object metadata for the watch leg and the ownership labels", gvk)
		})
	}
}
