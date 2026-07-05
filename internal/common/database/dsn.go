// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// TLSPaths carries the in-pod file paths of the projected DB-TLS material —
// the CA bundle plus the client certificate and key presented for mutual TLS.
type TLSPaths struct {
	CA   string
	Cert string
	Key  string
}

// SSLParams maps a database TLS mode to the pymysql ssl_* DSN query
// parameters, using the given in-pod file paths.
//
// Any mode outside prefer/require/verify-ca/verify-full (including the empty
// string) returns a non-nil error and nil url.Values, so the caller never
// assembles a partially-formed DSN.
//
// The returned url.Values is consumed via url.Values.Encode(), which sorts
// keys lexically and therefore yields a deterministic, stable query-parameter
// ordering across calls.
func SSLParams(mode string, paths TLSPaths) (url.Values, error) {
	switch mode {
	case "prefer", "require", "verify-ca", "verify-full":
		// Valid mode; fall through to build the parameters.
	default:
		return nil, fmt.Errorf("unknown database TLS mode %q: want one of prefer, require, verify-ca, verify-full", mode)
	}

	params := url.Values{}
	params.Set("ssl_ca", paths.CA)
	params.Set("ssl_cert", paths.Cert)
	params.Set("ssl_key", paths.Key)

	switch mode {
	case "verify-ca":
		params.Set("ssl_verify_cert", "true")
	case "verify-full":
		params.Set("ssl_verify_cert", "true")
		params.Set("ssl_verify_identity", "true")
	}

	return params, nil
}

// AppendTLSParams merges the pymysql ssl_* DSN parameters into query when the
// given database TLS block is enabled. It is a no-op when TLS is nil, its
// mode is empty, or its mode is "disabled" (DatabaseTLSSpec.IsEnabled),
// preserving the plaintext DSN. The mode is validated by SSLParams; an
// unknown mode (which the webhooks + CRD enum reject earlier) is surfaced as
// an error rather than silently producing a partial DSN.
func AppendTLSParams(tls *commonv1.DatabaseTLSSpec, paths TLSPaths, query url.Values) error {
	if !tls.IsEnabled() {
		return nil
	}
	sslParams, err := SSLParams(tls.Mode, paths)
	if err != nil {
		return err
	}
	for key, values := range sslParams {
		query[key] = values
	}
	return nil
}

// BuildDSN assembles the pymysql SQLAlchemy connection URL and returns it
// together with its digest (see Digest).
//
// url.UserPassword percent-encodes reserved characters in the userinfo
// component per RFC 3986, matching the encoding pymysql expects.
// url.Values.Encode percent-encodes "/" to "%2F" in the ssl_ca/ssl_cert/
// ssl_key file paths; keystone-manage db_sync hands the DSN to alembic's
// ConfigParser, which interprets "%" as interpolation syntax and aborts with
// "invalid interpolation syntax". RFC 3986 allows literal "/" in the query
// component, and the values contain neither "&" nor "=", so the "/" is kept
// literal — it also round-trips cleanly through urllib.parse_qs in the
// operators' embedded Python scripts.
func BuildDSN(username, password, host, dbName string, query url.Values) (dsn, digest string) {
	connURL := &url.URL{
		Scheme:   "mysql+pymysql",
		User:     url.UserPassword(username, password),
		Host:     host,
		Path:     dbName,
		RawQuery: strings.ReplaceAll(query.Encode(), "%2F", "/"),
	}
	dsn = connURL.String()
	return dsn, Digest(dsn)
}

// Digest returns the SHA-256 of the assembled DSN as a lowercase hex string.
// The deployment reconcilers stamp it into a pod-template annotation in
// Dynamic credentials mode so a rotated engine-issued credential rolls the
// Deployment (the DSN is consumed via an env var, which — unlike hot-reloaded
// volumes — only takes effect on a Pod restart).
func Digest(dsn string) string {
	sum := sha256.Sum256([]byte(dsn))
	return hex.EncodeToString(sum[:])
}
