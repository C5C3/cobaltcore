// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	. "github.com/onsi/gomega"
)

// testDBTLSPaths is the canonical client-cert mount layout used across the
// modeToSSLParams tests (mirrors the /etc/keystone/db-tls/ mount established by
// CC-0106 task 4.2).
var testDBTLSPaths = dbTLSPaths{
	CA:   "/etc/keystone/db-tls/ca.crt",
	Cert: "/etc/keystone/db-tls/tls.crt",
	Key:  "/etc/keystone/db-tls/tls.key",
}

// TestModeToSSLParams_AllFourModes backs REQ-004: every enum mode produces the
// exact ssl_* parameter set — ssl_ca/ssl_cert/ssl_key always present and equal
// to the supplied paths; ssl_verify_cert only for verify-ca/verify-full;
// ssl_verify_identity only for verify-full.
func TestModeToSSLParams_AllFourModes(t *testing.T) {
	cases := []struct {
		mode           string
		wantVerifyCert bool
		wantVerifyID   bool
	}{
		{"prefer", false, false},
		{"require", false, false},
		{"verify-ca", true, false},
		{"verify-full", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			g := NewWithT(t)

			params, err := modeToSSLParams(tc.mode, testDBTLSPaths)
			g.Expect(err).NotTo(HaveOccurred())

			g.Expect(params.Get("ssl_ca")).To(Equal(testDBTLSPaths.CA))
			g.Expect(params.Get("ssl_cert")).To(Equal(testDBTLSPaths.Cert))
			g.Expect(params.Get("ssl_key")).To(Equal(testDBTLSPaths.Key))

			if tc.wantVerifyCert {
				g.Expect(params.Get("ssl_verify_cert")).To(Equal("true"))
			} else {
				g.Expect(params.Has("ssl_verify_cert")).To(BeFalse(),
					"ssl_verify_cert must be absent for mode %q", tc.mode)
			}

			if tc.wantVerifyID {
				g.Expect(params.Get("ssl_verify_identity")).To(Equal("true"))
			} else {
				g.Expect(params.Has("ssl_verify_identity")).To(BeFalse(),
					"ssl_verify_identity must be absent for mode %q", tc.mode)
			}
		})
	}
}

// TestModeToSSLParams_DeterministicOrdering backs REQ-004: repeated calls with
// identical input produce byte-identical encoded query strings, so the
// assembled DSN is stable across reconciles.
func TestModeToSSLParams_DeterministicOrdering(t *testing.T) {
	g := NewWithT(t)

	first, err := modeToSSLParams("verify-full", testDBTLSPaths)
	g.Expect(err).NotTo(HaveOccurred())
	second, err := modeToSSLParams("verify-full", testDBTLSPaths)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(first.Encode()).To(Equal(second.Encode()))
	// Encode() sorts keys lexically; assert the exact stable ordering.
	g.Expect(first.Encode()).To(Equal(
		"ssl_ca=%2Fetc%2Fkeystone%2Fdb-tls%2Fca.crt" +
			"&ssl_cert=%2Fetc%2Fkeystone%2Fdb-tls%2Ftls.crt" +
			"&ssl_key=%2Fetc%2Fkeystone%2Fdb-tls%2Ftls.key" +
			"&ssl_verify_cert=true" +
			"&ssl_verify_identity=true",
	))
}

// TestModeToSSLParams_UnknownModeErrors backs REQ-004: an out-of-enum mode
// (including the empty string) returns a non-nil error and nil params so no
// partially-formed DSN is ever assembled.
func TestModeToSSLParams_UnknownModeErrors(t *testing.T) {
	for _, mode := range []string{"", "bogus", "VERIFY-FULL", "disable"} {
		t.Run("mode="+mode, func(t *testing.T) {
			g := NewWithT(t)

			params, err := modeToSSLParams(mode, testDBTLSPaths)
			g.Expect(err).To(HaveOccurred())
			g.Expect(params).To(BeNil())
		})
	}
}
