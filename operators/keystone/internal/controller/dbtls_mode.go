// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — dbtls_mode.go provides the pure, deterministic mapping
// from a database TLS mode enum to the pymysql ssl_* DSN query parameters
// plus the typed condition/reason constants for the
// DatabaseTLSReady status condition.
//
// The mapping is intentionally isolated and unit-tested so that the
// verification strength implied by each mode can never be silently downgraded
// when the DSN is assembled in reconcile_dbconnection_secret.go (task 4.1).
package controller

import (
	"fmt"
	"net/url"
)

// dbTLSPaths holds the in-pod mount file paths of the client certificate
// material referenced by the ssl_ca/ssl_cert/ssl_key DSN parameters. The
// reconciler never reads these bytes; it only emits the paths.
type dbTLSPaths struct {
	CA   string
	Cert string
	Key  string
}

// Typed condition/reason constants for the DatabaseTLSReady status condition
// They are declared here alongside the mode
// mapping so the DB-TLS vocabulary lives in one place; the condition itself is
// wired into the sub-reconciler chain and the subConditionTypes drift map by
// task 3.2, which is the consumer of these constants.
//
//nolint:unused // declared by task 1.3; consumed by the reconcileDatabaseTLS sub-reconciler added in task 3.2.
const (
	conditionTypeDatabaseTLSReady = "DatabaseTLSReady"

	reasonCertificateIssued  = "CertificateIssued"
	reasonCertificatePending = "CertificatePending"
	reasonNotRequired        = "NotRequired"
	reasonExternallyManaged  = "ExternallyManaged"
	reasonMissingCertRefs    = "MissingCertificateRefs"
)

// modeToSSLParams maps a database TLS mode to the ordered pymysql ssl_* DSN
// query parameters. The exact mapping is:
//
//   - prefer, require:  ssl_ca + ssl_cert + ssl_key only (encrypt, no peer
//     verification).
//   - verify-ca:        the above plus ssl_verify_cert=true (verify the server
//     certificate chain against the trusted CA).
//   - verify-full:      the above plus ssl_verify_cert=true and
//     ssl_verify_identity=true (also verify the server hostname identity).
//
// Any other mode (including the empty string) returns a non-nil error and nil
// url.Values, so the caller never assembles a partially-formed DSN.
//
// The returned url.Values is consumed via url.Values.Encode(), which sorts
// keys lexically and therefore yields a deterministic, stable query-parameter
// ordering across calls.
//
// DECISION: helper signature — Chose (mode string, basePaths dbTLSPaths)
// (url.Values, error) returning url.Values + error rather than a raw query
// string, because the task 4.1 consumer (reconcile_dbconnection_secret.go)
// builds a *url.URL and will merge these into connURL.RawQuery; url.Values
// composes cleanly with the existing "charset=utf8" parameter and Encode()
// guarantees deterministic ordering. Reviewer: please verify this matches the
// task 4.1 DSN-assembly consumption.
func modeToSSLParams(mode string, basePaths dbTLSPaths) (url.Values, error) {
	switch mode {
	case "prefer", "require", "verify-ca", "verify-full":
		// Valid mode; fall through to build the parameters.
	default:
		return nil, fmt.Errorf("unknown database TLS mode %q: want one of prefer, require, verify-ca, verify-full", mode)
	}

	params := url.Values{}
	params.Set("ssl_ca", basePaths.CA)
	params.Set("ssl_cert", basePaths.Cert)
	params.Set("ssl_key", basePaths.Key)

	switch mode {
	case "verify-ca":
		params.Set("ssl_verify_cert", "true")
	case "verify-full":
		params.Set("ssl_verify_cert", "true")
		params.Set("ssl_verify_identity", "true")
	}

	return params, nil
}
