// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for validateRotationOutput.
package controller

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

// TestValidateRotationOutput_CC0081 drives validateRotationOutput through its
// documented success and failure paths.
func TestValidateRotationOutput_CC0081(t *testing.T) {
	mustGenKey := func(t *testing.T) string {
		t.Helper()
		k, err := generateFernetKey()
		if err != nil {
			t.Fatalf("generateFernetKey: %v", err)
		}
		return k
	}

	genKeys := func(t *testing.T, n int) map[string][]byte {
		t.Helper()
		out := make(map[string][]byte, n)
		for i := 0; i < n; i++ {
			out[strconv.Itoa(i)] = []byte(mustGenKey(t))
		}
		return out
	}

	tests := []struct {
		name       string
		keys       func(t *testing.T) map[string][]byte
		minKeys    int
		maxKeys    int
		wantErrIs  error
		wantNilErr bool
	}{
		{
			name:       "accepts_valid_keys",
			keys:       func(t *testing.T) map[string][]byte { return genKeys(t, 3) },
			minKeys:    3,
			maxKeys:    4,
			wantNilErr: true,
		},
		{
			name: "rejects_wrong_length",
			keys: func(t *testing.T) map[string][]byte {
				k := genKeys(t, 3)
				// Replace one value with 32 raw bytes (wrong length, not 44).
				k["1"] = bytes.Repeat([]byte{0xAB}, 32)
				return k
			},
			minKeys:   3,
			maxKeys:   4,
			wantErrIs: ErrInvalidKeyFormat,
		},
		{
			name: "rejects_non_base64",
			keys: func(t *testing.T) map[string][]byte {
				k := genKeys(t, 3)
				// Replace one value with a 44-char string that is not valid base64url.
				k["0"] = []byte(strings.Repeat("!", 44))
				return k
			},
			minKeys:   3,
			maxKeys:   4,
			wantErrIs: ErrInvalidKeyFormat,
		},
		{
			name: "rejects_duplicate_keys",
			keys: func(t *testing.T) map[string][]byte {
				k := genKeys(t, 3)
				// Force two keys to be byte-identical.
				k["2"] = append([]byte(nil), k["1"]...)
				return k
			},
			minKeys:   3,
			maxKeys:   4,
			wantErrIs: ErrDuplicateKeys,
		},
		{
			name:      "rejects_too_few_keys",
			keys:      func(t *testing.T) map[string][]byte { return genKeys(t, 2) },
			minKeys:   3,
			maxKeys:   4,
			wantErrIs: ErrKeyCountOutOfRange,
		},
		{
			name:      "rejects_too_many_keys",
			keys:      func(t *testing.T) map[string][]byte { return genKeys(t, 5) },
			minKeys:   3,
			maxKeys:   4,
			wantErrIs: ErrKeyCountOutOfRange,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			err := validateRotationOutput(tc.keys(t), tc.minKeys, tc.maxKeys)
			if tc.wantNilErr {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(errors.Is(err, tc.wantErrIs)).To(BeTrue(), "expected err to wrap %v, got %v", tc.wantErrIs, err)
		})
	}
}

// TestValidateRotationOutput_AcceptsRealFernetKeys_CC0081 confirms that
// keys produced by the real generateFernetKey() helper pass validation
// across the supported count range.
func TestValidateRotationOutput_AcceptsRealFernetKeys_CC0081(t *testing.T) {
	g := NewGomegaWithT(t)

	const maxKeys = 4

	for _, n := range []int{3, 4, maxKeys} {
		data := make(map[string][]byte, n)
		for i := 0; i < n; i++ {
			k, err := generateFernetKey()
			g.Expect(err).NotTo(HaveOccurred())

			// Sanity: the helper must produce the 44-char base64url format we validate.
			g.Expect(k).To(HaveLen(44))
			decoded, derr := base64.URLEncoding.DecodeString(k)
			g.Expect(derr).NotTo(HaveOccurred())
			g.Expect(decoded).To(HaveLen(32))

			data[strconv.Itoa(i)] = []byte(k)
		}

		g.Expect(validateRotationOutput(data, 3, maxKeys)).To(Succeed(), "n=%d", n)
	}
}
