// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package openbao wraps the OpenBao HTTP API behind the narrow interface the
// Barbican operator needs to provision a managed secret store: authenticate,
// check the token's reach, and mint AppRole credentials. The interface is the
// seam the controller tests fake, so no unit test needs a live server.
package openbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	api "github.com/openbao/openbao/api/v2"
)

// Config addresses one OpenBao server.
type Config struct {
	// URL is the server's API base URL, e.g. "https://openbao.example.com:8200".
	URL string

	// CACertPEM is the PEM-encoded certificate bundle that authenticates the
	// server. When it is non-empty, verification runs against a pool built from
	// it alone; when it is empty, the system trust store applies. Verification is
	// never skipped.
	CACertPEM []byte

	// Namespace scopes every request to an OpenBao/Vault namespace (the
	// enterprise-style multi-tenancy header). Empty means the root namespace.
	// Without it the operator's own login would run against the root namespace
	// while the rendered configuration points the pods at a namespaced one, so a
	// brownfield store whose AppRole lives in a namespace could never validate.
	Namespace string

	// Timeout bounds every call made through the Client, retries included. It is
	// applied both as the request context deadline and as the underlying HTTP
	// client timeout, so a server that accepts a connection and then stalls
	// cannot wedge a reconcile.
	Timeout time.Duration
}

// Client is the subset of the OpenBao API the Barbican operator calls. A Client
// carries at most one session: the login methods retain the token they receive,
// and RevokeSelfToken hands it back.
type Client interface {
	// LoginKubernetes authenticates against the kubernetes auth method at
	// auth/kubernetes/login with the projected service-account token jwt and the
	// named role. On success the session token is retained on the client and every
	// subsequent call carries it.
	LoginKubernetes(ctx context.Context, jwt, role string) error

	// ReadRoleID returns the role ID of the AppRole roleName, read from
	// auth/approle/role/<roleName>/role-id. The role ID is stable for the
	// lifetime of the AppRole.
	ReadRoleID(ctx context.Context, roleName string) (string, error)

	// GenerateSecretID mints a fresh secret ID for the AppRole roleName at
	// auth/approle/role/<roleName>/secret-id and returns it together with the
	// secret_id_ttl the response reports. A ttlSeconds of 0 means the response
	// carried no TTL, which the caller reads as "does not expire".
	//
	// The sibling custom-secret-id endpoint, which would let the caller pick the
	// secret ID, is deliberately absent: the provisioner policy this operator
	// authenticates under denies it, and minting through the generate endpoint is
	// the settled contract. Do not add it.
	GenerateSecretID(ctx context.Context, roleName string) (secretID string, ttlSeconds int64, err error)

	// LoginAppRole authenticates against the approle auth method at
	// auth/approle/login with the given credentials. On success the session token
	// is retained on the client and every subsequent call carries it.
	LoginAppRole(ctx context.Context, roleID, secretID string) error

	// CapabilitiesSelf returns the capabilities the current token holds on path,
	// read from sys/capabilities-self. It is how the operator tells a policy gap
	// apart from a server fault before it attempts a write.
	CapabilitiesSelf(ctx context.Context, path string) ([]string, error)

	// MountExists reports whether the secrets engine mount is mounted, read from
	// sys/mounts/<mount>. An absent mount is not an error: it returns
	// (false, nil), so the caller can branch on the boolean alone.
	MountExists(ctx context.Context, mount string) (bool, error)

	// RevokeSelfToken revokes the retained session token at
	// auth/token/revoke-self and drops it from the client. It is a no-op
	// returning nil when no token is set, and every controller session must end
	// with it, error paths included, so a failed reconcile does not leave a live
	// token behind.
	RevokeSelfToken(ctx context.Context) error
}

// New returns a Client talking to the server described by cfg.
func New(cfg Config) (Client, error) {
	// NewConfig (not DefaultConfig) is deliberate: it sets DisableEnvironment, so
	// a stray BAO_* variable in the operator pod cannot redirect the client or
	// inject a token behind the caller's back.
	apiCfg := api.NewConfig()
	if apiCfg.Error != nil {
		return nil, fmt.Errorf("preparing the OpenBao client configuration: %w", apiCfg.Error)
	}
	apiCfg.Address = cfg.URL
	apiCfg.Timeout = cfg.Timeout
	apiCfg.HttpClient.Timeout = cfg.Timeout
	if err := apiCfg.ConfigureTLS(&api.TLSConfig{CACertBytes: cfg.CACertPEM}); err != nil {
		return nil, fmt.Errorf("configuring the OpenBao TLS trust: %w", err)
	}

	apiClient, err := api.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("creating the OpenBao client: %w", err)
	}
	// Set unconditionally: an empty namespace clears the header, which is the
	// root namespace every managed store runs against.
	apiClient.SetNamespace(cfg.Namespace)
	return &client{api: apiClient}, nil
}

// IsPermissionDenied reports whether err is an OpenBao response carrying HTTP
// 403. The operator branches on it to report a policy gap as a terminal
// configuration problem instead of retrying it forever.
func IsPermissionDenied(err error) bool {
	return hasStatusCode(err, http.StatusForbidden)
}

// IsInvalidCredentials reports whether err is an OpenBao response rejecting the
// credentials that were presented, rather than a server the client could not
// reach. It covers HTTP 400 and HTTP 403, because an AppRole login is answered
// with 400 ("invalid role or secret ID") for a role ID or secret ID the server
// does not accept — an expired or revoked secret ID above all — while a policy
// gap on a path answers 403. The AppRole login probes branch on it: a rejected
// credential is re-minted or reported as invalid, an unreachable server is
// retried.
func IsInvalidCredentials(err error) bool {
	return hasStatusCode(err, http.StatusBadRequest) || IsPermissionDenied(err)
}

// IsNotFound reports whether err is an OpenBao response carrying HTTP 404,
// which OpenBao returns both for an unmounted path and for a missing entry
// under a mounted one.
func IsNotFound(err error) bool {
	return hasStatusCode(err, http.StatusNotFound)
}

// hasStatusCode reports whether err wraps an OpenBao response error with the
// given HTTP status.
func hasStatusCode(err error, code int) bool {
	var respErr *api.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == code
}

// client is the api/v2-backed Client implementation.
type client struct {
	api *api.Client
}

func (c *client) LoginKubernetes(ctx context.Context, jwt, role string) error {
	data := map[string]any{"jwt": jwt, "role": role}
	if err := c.login(ctx, "auth/kubernetes/login", data); err != nil {
		return fmt.Errorf("logging in to OpenBao with the kubernetes auth method: %w", err)
	}
	return nil
}

func (c *client) LoginAppRole(ctx context.Context, roleID, secretID string) error {
	data := map[string]any{"role_id": roleID, "secret_id": secretID}
	if err := c.login(ctx, "auth/approle/login", data); err != nil {
		return fmt.Errorf("logging in to OpenBao with the approle auth method: %w", err)
	}
	return nil
}

func (c *client) ReadRoleID(ctx context.Context, roleName string) (string, error) {
	secret, err := c.api.Logical().ReadWithContext(ctx, fmt.Sprintf("auth/approle/role/%s/role-id", roleName))
	if err != nil {
		return "", fmt.Errorf("reading the role ID of AppRole %q: %w", roleName, err)
	}
	if secret == nil {
		return "", fmt.Errorf("reading the role ID of AppRole %q: the role does not exist", roleName)
	}
	roleID, _ := secret.Data["role_id"].(string)
	if roleID == "" {
		return "", fmt.Errorf("reading the role ID of AppRole %q: the response carried no role_id", roleName)
	}
	return roleID, nil
}

func (c *client) GenerateSecretID(ctx context.Context, roleName string) (string, int64, error) {
	path := fmt.Sprintf("auth/approle/role/%s/secret-id", roleName)
	secret, err := c.api.Logical().WriteWithContext(ctx, path, nil)
	if err != nil {
		return "", 0, fmt.Errorf("generating a secret ID for AppRole %q: %w", roleName, err)
	}
	if secret == nil {
		return "", 0, fmt.Errorf("generating a secret ID for AppRole %q: the role does not exist", roleName)
	}
	secretID, _ := secret.Data["secret_id"].(string)
	if secretID == "" {
		return "", 0, fmt.Errorf("generating a secret ID for AppRole %q: the response carried no secret_id", roleName)
	}
	ttl, err := ttlSeconds(secret.Data["secret_id_ttl"])
	if err != nil {
		return "", 0, fmt.Errorf("generating a secret ID for AppRole %q: %w", roleName, err)
	}
	return secretID, ttl, nil
}

func (c *client) CapabilitiesSelf(ctx context.Context, path string) ([]string, error) {
	capabilities, err := c.api.Sys().CapabilitiesSelfWithContext(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("reading the token capabilities on %q: %w", path, err)
	}
	return capabilities, nil
}

func (c *client) MountExists(ctx context.Context, mount string) (bool, error) {
	if _, err := c.api.Sys().MountInfoWithContext(ctx, mount); err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading the OpenBao mount %q: %w", mount, err)
	}
	return true, nil
}

func (c *client) RevokeSelfToken(ctx context.Context) error {
	if c.api.Token() == "" {
		return nil
	}
	// The token argument is ignored by the API client; the retained token is the
	// one revoked.
	if err := c.api.Auth().Token().RevokeSelfWithContext(ctx, ""); err != nil {
		return fmt.Errorf("revoking the OpenBao session token: %w", err)
	}
	c.api.ClearToken()
	return nil
}

// login writes the credentials to an auth-method login path and retains the
// token the response carries. Errors are returned unwrapped so each caller can
// name its own auth method.
func (c *client) login(ctx context.Context, path string, data map[string]any) error {
	secret, err := c.api.Logical().WriteWithContext(ctx, path, data)
	if err != nil {
		return err
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return errors.New("the response carried no client token")
	}
	c.api.SetToken(secret.Auth.ClientToken)
	return nil
}

// ttlSeconds reads a secret_id_ttl field as a whole number of seconds. The API
// client decodes JSON numbers as json.Number, so anything else (an absent field
// above all) yields 0, which the caller reads as "does not expire".
func ttlSeconds(raw any) (int64, error) {
	number, ok := raw.(json.Number)
	if !ok {
		return 0, nil
	}
	ttl, err := number.Int64()
	if err != nil {
		return 0, fmt.Errorf("parsing secret_id_ttl %q: %w", number, err)
	}
	return ttl, nil
}
