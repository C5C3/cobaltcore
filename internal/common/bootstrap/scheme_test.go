// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestNewScheme_RegistersClientGoBaseline verifies that a call without extras
// still carries the client-go types every operator needs. Without the baseline
// the manager could not even read a Secret.
func TestNewScheme_RegistersClientGoBaseline(t *testing.T) {
	g := NewGomegaWithT(t)

	scheme := NewScheme()

	g.Expect(scheme.Recognizes(corev1.SchemeGroupVersion.WithKind("Pod"))).To(BeTrue(),
		"the client-go baseline must be registered even when no extras are passed")
}

// TestNewScheme_AppliesExtrasInOrder verifies that every extra runs, and runs
// in argument order. Order matters because an operator may register a group
// whose types reference an earlier one.
func TestNewScheme_AppliesExtrasInOrder(t *testing.T) {
	g := NewGomegaWithT(t)

	var applied []string
	scheme := NewScheme(
		func(*runtime.Scheme) error {
			applied = append(applied, "first")
			return nil
		},
		func(*runtime.Scheme) error {
			applied = append(applied, "second")
			return nil
		},
	)

	g.Expect(scheme).NotTo(BeNil())
	g.Expect(applied).To(Equal([]string{"first", "second"}),
		"extras must be applied in argument order")
}

// TestNewScheme_PanicsOnFailingAdder pins the panic contract: a registration
// error kills startup instead of yielding a half-populated scheme that would
// fail much later with "no kind is registered for the type ...".
func TestNewScheme_PanicsOnFailingAdder(t *testing.T) {
	g := NewGomegaWithT(t)

	failing := func(*runtime.Scheme) error {
		return errors.New("cannot register type")
	}

	g.Expect(func() { NewScheme(failing) }).To(Panic(),
		"a failing registration function must panic through utilruntime.Must")
}
