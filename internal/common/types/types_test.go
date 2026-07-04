// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package types unit tests for the shared spec types. This file covers the
// DatabaseTLSSpec / DatabaseSpec.TLS deep-copy contract
// a populated DatabaseTLSSpec deep-copies into an independent equal value and
// a nil TLS pointer deep-copies to nil, preserving pre-existing behavior.
package types

import (
	"reflect"
	"testing"
)

// TestImageSpec_Reference backs the fully-qualified reference helper: a pinned
// digest yields "repository@digest"; otherwise "repository:tag".
func TestImageSpec_Reference(t *testing.T) {
	cases := []struct {
		name string
		spec ImageSpec
		want string
	}{
		{
			name: "tag",
			spec: ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			want: "ghcr.io/c5c3/keystone:2025.2",
		},
		{
			name: "digest",
			spec: ImageSpec{Repository: "ghcr.io/c5c3/keystone", Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"},
			want: "ghcr.io/c5c3/keystone@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		},
		{
			name: "digest wins over tag",
			spec: ImageSpec{Repository: "repo", Tag: "2025.2", Digest: "sha256:abc"},
			want: "repo@sha256:abc",
		},
	}
	for _, tc := range cases {
		if got := tc.spec.Reference(); got != tc.want {
			t.Errorf("%s: Reference() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestDatabaseTLSSpec_DeepCopy backs: DeepCopy of a populated
// DatabaseTLSSpec returns an independent, equal value, and DeepCopy of a nil
// *DatabaseTLSSpec returns nil.
func TestDatabaseTLSSpec_DeepCopy(t *testing.T) {
	original := &DatabaseTLSSpec{
		Mode: "verify-full",
		CABundleSecretRef: SecretRefSpec{
			Name: "db-ca-bundle",
			Key:  "ca.crt",
		},
		ClientCertSecretRef: SecretRefSpec{
			Name: "keystone-db-client",
			Key:  "tls.crt",
		},
	}

	clone := original.DeepCopy()

	if clone == original {
		t.Fatal("DeepCopy did not allocate a new *DatabaseTLSSpec")
	}
	if *clone != *original {
		t.Errorf("DeepCopy produced an unequal value: got %+v, want %+v", *clone, *original)
	}

	// Mutating the clone must not affect the original (no aliasing).
	clone.Mode = "prefer"
	clone.CABundleSecretRef.Name = "mutated"
	clone.ClientCertSecretRef.Key = "mutated"
	if original.Mode != "verify-full" ||
		original.CABundleSecretRef.Name != "db-ca-bundle" ||
		original.ClientCertSecretRef.Key != "tls.crt" {
		t.Errorf("mutating the clone altered the original: %+v", *original)
	}

	var nilTLS *DatabaseTLSSpec
	if nilTLS.DeepCopy() != nil {
		t.Errorf("DeepCopy of a nil *DatabaseTLSSpec must return nil")
	}
}

// TestDatabaseTLSSpec_IsEnabled backs the on/off predicate: a nil receiver, an
// empty mode, and mode "disabled" are all off; every real verification mode is
// on.
func TestDatabaseTLSSpec_IsEnabled(t *testing.T) {
	var nilTLS *DatabaseTLSSpec
	if nilTLS.IsEnabled() {
		t.Errorf("nil *DatabaseTLSSpec must not be enabled")
	}

	cases := []struct {
		mode string
		want bool
	}{
		{mode: "", want: false},
		{mode: "disabled", want: false},
		{mode: "prefer", want: true},
		{mode: "require", want: true},
		{mode: "verify-ca", want: true},
		{mode: "verify-full", want: true},
	}
	for _, tc := range cases {
		got := (&DatabaseTLSSpec{Mode: tc.mode}).IsEnabled()
		if got != tc.want {
			t.Errorf("IsEnabled() for mode %q = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// TestDatabaseSpec_TLSField_OptionalPointer backs: a DatabaseSpec with
// TLS == nil deep-copies to TLS == nil (unchanged plaintext behavior), and a
// DatabaseSpec with TLS set round-trips through DeepCopy without aliasing the
// TLS pointer.
func TestDatabaseSpec_TLSField_OptionalPointer(t *testing.T) {
	withoutTLS := DatabaseSpec{
		Database:  "keystone",
		SecretRef: SecretRefSpec{Name: "keystone-db", Key: "password"},
	}
	cloneWithout := withoutTLS.DeepCopy()
	if cloneWithout.TLS != nil {
		t.Errorf("DeepCopy of a DatabaseSpec with nil TLS must keep TLS nil, got %+v", cloneWithout.TLS)
	}

	withTLS := DatabaseSpec{
		Database:  "keystone",
		SecretRef: SecretRefSpec{Name: "keystone-db", Key: "password"},
		TLS: &DatabaseTLSSpec{
			Mode:                "verify-ca",
			CABundleSecretRef:   SecretRefSpec{Name: "db-ca-bundle", Key: "ca.crt"},
			ClientCertSecretRef: SecretRefSpec{Name: "keystone-db-client", Key: "tls.crt"},
		},
	}
	cloneWith := withTLS.DeepCopy()
	if cloneWith.TLS == nil {
		t.Fatal("DeepCopy dropped a non-nil TLS pointer")
	}
	if cloneWith.TLS == withTLS.TLS {
		t.Errorf("DeepCopy did not allocate a new *DatabaseTLSSpec for DatabaseSpec.TLS")
	}
	if *cloneWith.TLS != *withTLS.TLS {
		t.Errorf("DeepCopy produced an unequal TLS value: got %+v, want %+v", *cloneWith.TLS, *withTLS.TLS)
	}

	cloneWith.TLS.Mode = "prefer"
	if withTLS.TLS.Mode != "verify-ca" {
		t.Errorf("mutating clone.TLS altered the original: %+v", *withTLS.TLS)
	}
}

// TestGatewaySpec_DeepCopy backs for the shared commonv1 Gateway shape:
// DeepCopy of a populated GatewaySpec (non-empty Annotations map, populated
// nested ParentRef) returns an independent, equal value, and DeepCopy of a nil
// *GatewaySpec returns nil. GatewaySpec contains a map, so it is not comparable
// with == — compare via reflect.DeepEqual and field-by-field.
func TestGatewaySpec_DeepCopy(t *testing.T) {
	original := &GatewaySpec{
		ParentRef: GatewayParentRefSpec{
			Name:        "shared-gateway",
			Namespace:   "gateway-system",
			SectionName: "https",
		},
		Hostname: "keystone.127-0-0-1.nip.io",
		Path:     "/v3",
		Annotations: map[string]string{
			"haproxy.org/timeout-client": "30s",
			"haproxy.org/rate-limit":     "100",
		},
	}

	clone := original.DeepCopy()

	if clone == original {
		t.Fatal("DeepCopy did not allocate a new *GatewaySpec")
	}
	if !reflect.DeepEqual(clone, original) {
		t.Errorf("DeepCopy produced an unequal value: got %+v, want %+v", *clone, *original)
	}

	// Mutating the clone's Annotations map must not affect the original (no
	// aliasing of the map).
	clone.Annotations["haproxy.org/rate-limit"] = "mutated"
	clone.Annotations["added"] = "mutated"
	if original.Annotations["haproxy.org/rate-limit"] != "100" {
		t.Errorf("mutating the clone's Annotations altered the original: %+v", original.Annotations)
	}
	if _, ok := original.Annotations["added"]; ok {
		t.Errorf("adding to the clone's Annotations altered the original: %+v", original.Annotations)
	}

	// Mutating the clone's nested ParentRef fields must not affect the original.
	clone.ParentRef.Name = "mutated"
	clone.ParentRef.SectionName = "mutated"
	if original.ParentRef.Name != "shared-gateway" || original.ParentRef.SectionName != "https" {
		t.Errorf("mutating the clone's ParentRef altered the original: %+v", original.ParentRef)
	}

	var nilGateway *GatewaySpec
	if nilGateway.DeepCopy() != nil {
		t.Errorf("DeepCopy of a nil *GatewaySpec must return nil")
	}
}

// TestGatewayParentRefSpec_DeepCopy backs: GatewayParentRefSpec holds
// only scalar fields, so DeepCopy returns an independent, equal value, and
// DeepCopy of a nil *GatewayParentRefSpec returns nil.
func TestGatewayParentRefSpec_DeepCopy(t *testing.T) {
	original := &GatewayParentRefSpec{
		Name:        "shared-gateway",
		Namespace:   "gateway-system",
		SectionName: "https",
	}

	clone := original.DeepCopy()

	if clone == original {
		t.Fatal("DeepCopy did not allocate a new *GatewayParentRefSpec")
	}
	if *clone != *original {
		t.Errorf("DeepCopy produced an unequal value: got %+v, want %+v", *clone, *original)
	}

	clone.Name = "mutated"
	clone.Namespace = "mutated"
	clone.SectionName = "mutated"
	if original.Name != "shared-gateway" || original.Namespace != "gateway-system" ||
		original.SectionName != "https" {
		t.Errorf("mutating the clone altered the original: %+v", *original)
	}

	var nilRef *GatewayParentRefSpec
	if nilRef.DeepCopy() != nil {
		t.Errorf("DeepCopy of a nil *GatewayParentRefSpec must return nil")
	}
}
