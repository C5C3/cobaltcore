// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package openbao

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultTimeout bounds every request of a Client.
const defaultTimeout = 30 * time.Second

// maxResponseBytes caps the response body a Client reads. The answers it
// expects (a login, a role, an error list) are a few hundred bytes.
const maxResponseBytes = 1 << 20

// Config is what Login needs to reach OpenBao and authenticate.
type Config struct {
	// Server is the OpenBao base URL, for example
	// https://openbao.shared-services.svc:8200.
	Server string
	// KubernetesMount is the path of the Kubernetes auth mount, for example
	// kubernetes/management.
	KubernetesMount string
	// Role is the Kubernetes-auth role the login names.
	Role string
	// JWT is the ServiceAccount token the login presents.
	JWT string
	// CACert is the PEM bundle the server certificate is verified against, and
	// ClientCert and ClientKey the PEM keypair presented to the listener. All
	// three are set together; all three empty means plain HTTP, which only the
	// tests use.
	CACert, ClientCert, ClientKey []byte
}

// DatabaseRole is a role of the OpenBao database secrets engine. OpenBao keeps
// the TTLs in whole seconds.
type DatabaseRole struct {
	DBName               string
	CreationStatements   []string
	RevocationStatements []string
	DefaultTTL           time.Duration
	MaxTTL               time.Duration
}

// Client is an authenticated OpenBao session.
type Client interface {
	// ReadDatabaseRole reads the role name of the database engine at mount. An
	// absent role returns (nil, false, nil).
	ReadDatabaseRole(ctx context.Context, mount, name string) (*DatabaseRole, bool, error)
	// WriteDatabaseRole creates or replaces the role name at mount.
	WriteDatabaseRole(ctx context.Context, mount, name string, role DatabaseRole) error
	// DeleteDatabaseRole deletes the role name at mount. An absent role is not
	// an error.
	DeleteDatabaseRole(ctx context.Context, mount, name string) error
	// RevokeLeasePrefix revokes every lease whose path starts with prefix.
	RevokeLeasePrefix(ctx context.Context, prefix string) error
	// Close revokes the session's token and releases its connections.
	Close(ctx context.Context) error
}

// APIError is an answer of OpenBao with a status outside 2xx.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	// Errors is the server's errors list, or the raw body when it was not one.
	Errors []string
}

// Error renders "openbao: <status> on <method> <path>: <errors>".
func (e *APIError) Error() string {
	detail := strings.Join(e.Errors, "; ")
	if detail == "" {
		detail = "<no error body>"
	}
	return fmt.Sprintf("openbao: %d on %s %s: %s", e.StatusCode, e.Method, e.Path, detail)
}

// IsPermissionDenied reports whether err carries a 403 answer: the token's
// policy does not grant the path, or the login role does not admit the caller.
func IsPermissionDenied(err error) bool {
	return hasStatus(err, http.StatusForbidden)
}

// hasStatus reports whether err carries an APIError with status code.
func hasStatus(err error, code int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}

// client is the Client Login returns.
type client struct {
	http   *http.Client
	server string
	token  string
}

// Login authenticates against the Kubernetes auth mount cfg names and returns
// a session carrying the issued token. Neither the JWT nor the token appears
// in an error.
func Login(ctx context.Context, cfg Config) (Client, error) {
	httpClient, err := newHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	c := &client{http: httpClient, server: strings.TrimSuffix(cfg.Server, "/")}

	path := "/v1/auth/" + cfg.KubernetesMount + "/login"
	var resp struct {
		Auth *struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := c.do(ctx, http.MethodPost, path, map[string]string{"role": cfg.Role, "jwt": cfg.JWT}, &resp); err != nil {
		httpClient.CloseIdleConnections()
		return nil, err
	}
	if resp.Auth == nil || resp.Auth.ClientToken == "" {
		httpClient.CloseIdleConnections()
		return nil, fmt.Errorf("openbao: POST %s: the answer carries no client token", path)
	}
	c.token = resp.Auth.ClientToken
	return c, nil
}

// newHTTPClient builds the HTTP client of one session: mTLS when cfg carries
// the CA and the keypair, plain HTTP when it carries none of the three.
func newHTTPClient(cfg Config) (*http.Client, error) {
	set := 0
	for _, b := range [][]byte{cfg.CACert, cfg.ClientCert, cfg.ClientKey} {
		if len(b) > 0 {
			set++
		}
	}
	switch set {
	case 0:
		return &http.Client{Timeout: defaultTimeout, Transport: http.DefaultTransport.(*http.Transport).Clone()}, nil
	case 3:
	default:
		return nil, errors.New("openbao: CACert, ClientCert and ClientKey must be set together")
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cfg.CACert) {
		return nil, errors.New("openbao: CACert holds no PEM certificate")
	}
	keypair, err := tls.X509KeyPair(cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("openbao: loading the client keypair: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      pool,
		Certificates: []tls.Certificate{keypair},
	}
	return &http.Client{Timeout: defaultTimeout, Transport: transport}, nil
}

// do sends one request with body encoded as JSON, decodes a 2xx answer into
// out when both are non-nil, and returns an APIError for any other status.
// Transport failures are wrapped with the method and the path, so a
// context.DeadlineExceeded stays visible to errors.Is.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("openbao: %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, reader)
	if err != nil {
		return fmt.Errorf("openbao: %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("X-Vault-Token", c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openbao: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("openbao: %s %s: reading the answer: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{StatusCode: resp.StatusCode, Method: method, Path: path}
		var errBody struct {
			Errors []string `json:"errors"`
		}
		if json.Unmarshal(data, &errBody) == nil {
			apiErr.Errors = errBody.Errors
		} else if text := strings.TrimSpace(string(data)); text != "" {
			apiErr.Errors = []string{text}
		}
		return apiErr
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("openbao: %s %s: decoding the answer: %w", method, path, err)
	}
	return nil
}

// databaseRoleBody is the role's wire shape, the TTLs in integer seconds both
// ways.
type databaseRoleBody struct {
	DBName               string   `json:"db_name"`
	CreationStatements   []string `json:"creation_statements"`
	RevocationStatements []string `json:"revocation_statements"`
	DefaultTTL           int64    `json:"default_ttl"`
	MaxTTL               int64    `json:"max_ttl"`
}

// rolePath is the API path of the role name of the database engine at mount.
func rolePath(mount, name string) string {
	return "/v1/" + mount + "/roles/" + name
}

func (c *client) ReadDatabaseRole(ctx context.Context, mount, name string) (*DatabaseRole, bool, error) {
	var resp struct {
		Data databaseRoleBody `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, rolePath(mount, name), nil, &resp); err != nil {
		if hasStatus(err, http.StatusNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &DatabaseRole{
		DBName:               resp.Data.DBName,
		CreationStatements:   resp.Data.CreationStatements,
		RevocationStatements: resp.Data.RevocationStatements,
		DefaultTTL:           time.Duration(resp.Data.DefaultTTL) * time.Second,
		MaxTTL:               time.Duration(resp.Data.MaxTTL) * time.Second,
	}, true, nil
}

func (c *client) WriteDatabaseRole(ctx context.Context, mount, name string, role DatabaseRole) error {
	body := databaseRoleBody{
		DBName:               role.DBName,
		CreationStatements:   role.CreationStatements,
		RevocationStatements: role.RevocationStatements,
		DefaultTTL:           int64(role.DefaultTTL / time.Second),
		MaxTTL:               int64(role.MaxTTL / time.Second),
	}
	return c.do(ctx, http.MethodPost, rolePath(mount, name), body, nil)
}

func (c *client) DeleteDatabaseRole(ctx context.Context, mount, name string) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, rolePath(mount, name), nil, nil))
}

func (c *client) RevokeLeasePrefix(ctx context.Context, prefix string) error {
	return ignoreNotFound(c.do(ctx, http.MethodPost, "/v1/sys/leases/revoke-prefix/"+prefix, nil, nil))
}

func (c *client) Close(ctx context.Context) error {
	defer c.http.CloseIdleConnections()
	return c.do(ctx, http.MethodPost, "/v1/auth/token/revoke-self", nil, nil)
}

// ignoreNotFound treats a 404 answer as success.
func ignoreNotFound(err error) error {
	if hasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
