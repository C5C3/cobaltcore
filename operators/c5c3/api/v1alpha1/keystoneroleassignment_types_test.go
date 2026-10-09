// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestSchemeBuilderRegistersKeystoneRoleAssignment(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}
	for _, kind := range []string{"KeystoneRoleAssignment", "KeystoneRoleAssignmentList"} {
		gvk := schema.GroupVersionKind{Group: "c5c3.io", Version: "v1alpha1", Kind: kind}
		if _, err := s.New(gvk); err != nil {
			t.Fatalf("scheme.New(%v) failed: %v", gvk, err)
		}
	}
	// A kind the package does not define must not resolve, so the loop above
	// cannot pass on a scheme that answers every lookup.
	missing := schema.GroupVersionKind{Group: "c5c3.io", Version: "v1alpha1", Kind: "KeystoneGroup"}
	if _, err := s.New(missing); err == nil {
		t.Fatalf("scheme.New(%v) resolved a kind the package does not define", missing)
	}
}
