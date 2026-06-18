// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — rotation output validation. The operator
// validates the contents of the staging Secret before copying its data onto
// the production keys Secret. Validation is defense-in-depth: even if the
// rotation CronJob is compromised via a supply-chain attack, malformed or
// adversarial keys are rejected by the operator before they reach production.
package controller

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
)

// ErrInvalidKeyFormat is returned when a staged key is not a 44-byte base64url
// string decoding to exactly 32 bytes (the format produced by generateFernetKey).
var ErrInvalidKeyFormat = errors.New("invalid key format")

// ErrDuplicateKeys is returned when two staged keys have identical bytes.
var ErrDuplicateKeys = errors.New("duplicate keys detected")

// ErrKeyCountOutOfRange is returned when the staged key count falls outside
// the configured [minKeys, maxKeys] inclusive range.
var ErrKeyCountOutOfRange = errors.New("key count out of range")

// validateRotationOutput enforces the rotation-output contract
//   - len(keys) must lie in [minKeys, maxKeys] inclusive;
//   - each value must be 44 bytes long and decode via base64.URLEncoding to
//     exactly 32 bytes (the format produced by generateFernetKey);
//   - all values must be pairwise distinct (byte-for-byte).
//
// Errors are returned wrapped so callers can match with errors.Is against
// ErrKeyCountOutOfRange, ErrInvalidKeyFormat, or ErrDuplicateKeys. Map keys
// are visited in sorted order so error messages are deterministic.
func validateRotationOutput(keys map[string][]byte, minKeys, maxKeys int) error {
	if len(keys) < minKeys || len(keys) > maxKeys {
		return fmt.Errorf("%w: got %d, want [%d, %d]", ErrKeyCountOutOfRange, len(keys), minKeys, maxKeys)
	}

	indices := make([]string, 0, len(keys))
	for k := range keys {
		indices = append(indices, k)
	}
	sort.Strings(indices)

	for _, idx := range indices {
		value := keys[idx]
		if len(value) != 44 {
			return fmt.Errorf("%w: key %q length=%d, want 44", ErrInvalidKeyFormat, idx, len(value))
		}
		decoded, err := base64.URLEncoding.DecodeString(string(value))
		if err != nil {
			return fmt.Errorf("%w: key %q base64 decode: %w", ErrInvalidKeyFormat, idx, err)
		}
		if len(decoded) != 32 {
			return fmt.Errorf("%w: key %q decoded length=%d, want 32", ErrInvalidKeyFormat, idx, len(decoded))
		}
	}

	for i := 0; i < len(indices); i++ {
		for j := i + 1; j < len(indices); j++ {
			if bytes.Equal(keys[indices[i]], keys[indices[j]]) {
				return fmt.Errorf("%w: keys %q and %q", ErrDuplicateKeys, indices[i], indices[j])
			}
		}
	}

	return nil
}
