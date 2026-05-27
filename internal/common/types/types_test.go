// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package types unit tests for the shared spec types. This file covers the
// CC-0106 DatabaseTLSSpec / DatabaseSpec.TLS deep-copy contract (REQ-001):
// a populated DatabaseTLSSpec deep-copies into an independent equal value and
// a nil TLS pointer deep-copies to nil, preserving pre-CC-0106 behavior.
package types

import "testing"

// TestDatabaseTLSSpec_DeepCopy backs REQ-001: DeepCopy of a populated
// DatabaseTLSSpec returns an independent, equal value, and DeepCopy of a nil
// *DatabaseTLSSpec returns nil.
func TestDatabaseTLSSpec_DeepCopy(t *testing.T) {
	original := &DatabaseTLSSpec{
		Enabled: true,
		Mode:    "verify-full",
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
	clone.Enabled = false
	clone.Mode = "prefer"
	clone.CABundleSecretRef.Name = "mutated"
	clone.ClientCertSecretRef.Key = "mutated"
	if !original.Enabled || original.Mode != "verify-full" ||
		original.CABundleSecretRef.Name != "db-ca-bundle" ||
		original.ClientCertSecretRef.Key != "tls.crt" {
		t.Errorf("mutating the clone altered the original: %+v", *original)
	}

	var nilTLS *DatabaseTLSSpec
	if nilTLS.DeepCopy() != nil {
		t.Errorf("DeepCopy of a nil *DatabaseTLSSpec must return nil")
	}
}

// TestDatabaseSpec_TLSField_OptionalPointer backs REQ-001: a DatabaseSpec with
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
			Enabled:             true,
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
