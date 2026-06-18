// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestSchemeBuilderRegistersCredentialRotation(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}
	for _, kind := range []string{"CredentialRotation", "CredentialRotationList"} {
		gvk := schema.GroupVersionKind{Group: "c5c3.io", Version: "v1alpha1", Kind: kind}
		if _, err := s.New(gvk); err != nil {
			t.Fatalf("scheme.New(%v) failed: %v", gvk, err)
		}
	}
}

// TestCredentialRotationSpecDeferredFields verifies the deferred scheduled
// fields are optional pointers that round-trip through DeepCopy.
func TestCredentialRotationSpecDeferredFields(t *testing.T) {
	interval := int32(90)
	pre := int32(7)
	grace := int32(3)
	spec := CredentialRotationSpec{
		Target:          RotationTargetAdminApplicationCredential,
		Bootstrap:       true,
		IntervalDays:    &interval,
		PreRotationDays: &pre,
		GracePeriodDays: &grace,
	}

	clone := spec.DeepCopy()
	if clone.IntervalDays == spec.IntervalDays {
		t.Errorf("DeepCopy did not allocate a new *int32 for IntervalDays")
	}
	if *clone.IntervalDays != 90 || *clone.PreRotationDays != 7 || *clone.GracePeriodDays != 3 {
		t.Errorf("DeepCopy altered deferred fields: %+v", clone)
	}
	if !clone.Bootstrap {
		t.Errorf("expected Bootstrap true")
	}
}
