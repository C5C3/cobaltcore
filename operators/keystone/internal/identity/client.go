// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package identity implements the operator's own minimal Keystone identity
// client: domain and federation-object CRUD, authenticated per call with the
// bootstrap admin credentials against the cluster-local Keystone endpoint —
// no K-ORC, no clouds.yaml — so a standalone Keystone (no ControlPlane) works
// with zero extra configuration. Everything is stdlib (net/http +
// encoding/json) except federation mappings, which ride gophercloud v2 — the
// one federation resource its SDK covers (the Phase-0 client decision);
// identity providers, protocols, groups, roles, and role assignments stay on
// the local REST implementation in federation.go until the parallel upstream
// gophercloud contributions land (they gate nothing here).
package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/federation"

	"github.com/c5c3/forge/internal/common/healthcheck"
)

// Sentinel errors mapped from the identity API's HTTP status codes. Callers
// use errors.Is to branch on the recoverable classes without parsing bodies.
var (
	// ErrNotFound maps HTTP 404 (and an empty list on lookup-by-name).
	ErrNotFound = errors.New("not found")
	// ErrConflict maps HTTP 409 (e.g. creating a domain whose name exists).
	ErrConflict = errors.New("conflict")
	// ErrUnauthorized maps HTTP 401 (bad admin credentials).
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden maps HTTP 403 (e.g. deleting an enabled domain).
	ErrForbidden = errors.New("forbidden")
)

// HTTPDoer re-exports the shared client seam so tests can inject a stub
// transport, mirroring the health-check reconciler's injection point.
type HTTPDoer = healthcheck.HTTPDoer

// snippetLimit bounds how much of an unexpected response body is embedded in
// an error message: enough for a full keystone JSON error document, small
// enough for a status-condition message.
const snippetLimit = 256

// bodySnippet renders at most snippetLimit bytes of a response body for
// embedding in an "unexpected HTTP" error. The snippet identifies the
// responder when the status line alone cannot: keystone errors carry a JSON
// body ({"error": {...}}), while a foreign HTTP server that answered in its
// place typically returns HTML or nothing. %q escapes control characters so
// arbitrary bytes stay log- and condition-safe.
func bodySnippet(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if len(body) > snippetLimit {
		body = body[:snippetLimit]
	}
	return fmt.Sprintf(" (body: %q)", body)
}

// Credentials carries the password-method authentication inputs. Username is
// the bootstrap admin user; ProjectName / UserDomainName default to the
// bootstrap conventions ("admin" project, "Default" user domain) when empty.
type Credentials struct {
	Username string
	Password string
	// ProjectName scopes the token; defaults to "admin".
	ProjectName string
	// UserDomainName is the domain the admin user lives in; defaults to
	// "Default" (BootstrapSpec has no domain knob, so the bootstrap admin
	// always lives in the Default domain).
	UserDomainName string
}

// Domain is the minimal domain representation the operator needs.
type Domain struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

// Client is the identity-API surface the KeystoneIdentityBackend controller
// consumes: domain CRUD (Phase 1) plus the federation-object CRUD the OIDC
// backends need (identity providers, protocols, mappings, groups, roles, role
// assignments). The interface is defined here (producer side) because the
// fake test double in identity/fake and the controller both bind to it.
type Client interface {
	// GetDomainByName resolves a domain by exact name, returning ErrNotFound
	// (wrapped) when no domain with that name exists.
	GetDomainByName(ctx context.Context, name string) (*Domain, error)
	// CreateDomain creates the given domain and returns the server-side
	// representation (with the assigned ID).
	CreateDomain(ctx context.Context, domain Domain) (*Domain, error)
	// UpdateDomain patches the enabled flag and/or description of the domain
	// with the given ID; nil fields are left untouched.
	UpdateDomain(ctx context.Context, id string, enabled *bool, description *string) error
	// DeleteDomain deletes the domain with the given ID. Keystone requires
	// the domain to be disabled first (ErrForbidden otherwise).
	DeleteDomain(ctx context.Context, id string) error

	// GetIdentityProvider fetches one identity provider by ID.
	GetIdentityProvider(ctx context.Context, id string) (*IdentityProvider, error)
	// CreateIdentityProvider registers the identity provider under idp.ID.
	CreateIdentityProvider(ctx context.Context, idp IdentityProvider) error
	// UpdateIdentityProvider patches enabled/description/remoteIDs; nil
	// fields (and a nil remoteIDs slice) are left untouched.
	UpdateIdentityProvider(ctx context.Context, id string, enabled *bool, description *string, remoteIDs []string) error
	// DeleteIdentityProvider deletes the identity provider with the given ID.
	DeleteIdentityProvider(ctx context.Context, id string) error

	// GetProtocol fetches one protocol of an identity provider.
	GetProtocol(ctx context.Context, idpID, id string) (*Protocol, error)
	// CreateProtocol binds protocol id of idpID to mappingID.
	CreateProtocol(ctx context.Context, idpID, id, mappingID string) error
	// UpdateProtocol re-points the protocol at a different mapping.
	UpdateProtocol(ctx context.Context, idpID, id, mappingID string) error
	// DeleteProtocol deletes the protocol from the identity provider.
	DeleteProtocol(ctx context.Context, idpID, id string) error

	// GetMapping fetches one federation mapping by ID.
	GetMapping(ctx context.Context, id string) (*federation.Mapping, error)
	// CreateMapping creates the mapping with the given ID and rules.
	CreateMapping(ctx context.Context, id string, rules []federation.MappingRule) error
	// UpdateMapping replaces the rules of the mapping with the given ID.
	UpdateMapping(ctx context.Context, id string, rules []federation.MappingRule) error
	// DeleteMapping deletes the mapping with the given ID.
	DeleteMapping(ctx context.Context, id string) error

	// GetGroupByName resolves a group by exact name inside a domain,
	// returning ErrNotFound (wrapped) when no such group exists.
	GetGroupByName(ctx context.Context, name, domainID string) (*Group, error)
	// CreateGroup creates the given group and returns the server-side
	// representation (with the assigned ID).
	CreateGroup(ctx context.Context, group Group) (*Group, error)
	// GetRoleByName resolves a role by exact name.
	GetRoleByName(ctx context.Context, name string) (*Role, error)
	// GetProjectByName resolves a project by exact name inside a domain.
	GetProjectByName(ctx context.Context, name, domainID string) (*Project, error)
	// HasRoleForGroupOnDomain reports whether the assignment already exists.
	HasRoleForGroupOnDomain(ctx context.Context, domainID, groupID, roleID string) (bool, error)
	// HasRoleForGroupOnProject reports whether the assignment already exists.
	HasRoleForGroupOnProject(ctx context.Context, projectID, groupID, roleID string) (bool, error)
	// AssignRoleToGroupOnDomain grants the role to the group on the domain.
	AssignRoleToGroupOnDomain(ctx context.Context, domainID, groupID, roleID string) error
	// AssignRoleToGroupOnProject grants the role to the group on the project.
	AssignRoleToGroupOnProject(ctx context.Context, projectID, groupID, roleID string) error
}

// httpClient is the production Client implementation. It authenticates per
// call via POST /v3/auth/tokens (password method, project-scoped) — no token
// caching, because domain operations are rare (provisioning and deletion
// only) and a cached token would add expiry/invalidation state for no
// measurable gain.
type httpClient struct {
	// endpoint is the cluster-local Keystone API URL including the /v3
	// suffix, e.g. http://keystone.openstack.svc.cluster.local:5000/v3.
	endpoint string
	creds    Credentials
	doer     HTTPDoer
}

// NewHTTPClient builds a Client against the given /v3 endpoint. A nil doer
// falls back to http.DefaultClient.
func NewHTTPClient(endpoint string, creds Credentials, doer HTTPDoer) Client {
	if doer == nil {
		doer = http.DefaultClient
	}
	if creds.ProjectName == "" {
		creds.ProjectName = "admin"
	}
	if creds.UserDomainName == "" {
		creds.UserDomainName = "Default"
	}
	return &httpClient{endpoint: endpoint, creds: creds, doer: doer}
}

// authenticate obtains a project-scoped token via the password method and
// returns the X-Subject-Token value.
func (c *httpClient) authenticate(ctx context.Context) (string, error) {
	body := map[string]any{
		"auth": map[string]any{
			"identity": map[string]any{
				"methods": []string{"password"},
				"password": map[string]any{
					"user": map[string]any{
						"name":     c.creds.Username,
						"domain":   map[string]string{"name": c.creds.UserDomainName},
						"password": c.creds.Password,
					},
				},
			},
			"scope": map[string]any{
				"project": map[string]any{
					"name":   c.creds.ProjectName,
					"domain": map[string]string{"name": c.creds.UserDomainName},
				},
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshaling auth request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/auth/tokens", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("building auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.doer.Do(req)
	if err != nil {
		return "", fmt.Errorf("authenticating against %s: %w", c.endpoint, err)
	}
	defer func() {
		// Drain before closing so the transport can reuse the connection.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("%w: authenticating user %q", ErrUnauthorized, c.creds.Username)
	}
	if resp.StatusCode != http.StatusCreated {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, snippetLimit))
		return "", fmt.Errorf("authenticating against %s: unexpected HTTP %d%s", c.endpoint, resp.StatusCode, bodySnippet(snippet))
	}

	token := resp.Header.Get("X-Subject-Token")
	if token == "" {
		return "", fmt.Errorf("authenticating against %s: response carries no X-Subject-Token", c.endpoint)
	}
	return token, nil
}

// do issues an authenticated request, maps the error-class status codes to
// the sentinel errors, and returns the fully-read response body. Reading (and
// closing) the body here keeps the connection reusable and centralizes the
// lifecycle so no caller can leak it.
func (c *httpClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	token, err := c.authenticate(ctx)
	if err != nil {
		return nil, err
	}

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling %s %s request: %w", method, path, err)
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reader)
	if err != nil {
		return nil, fmt.Errorf("building %s %s request: %w", method, path, err)
	}
	req.Header.Set("X-Auth-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, readErr := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s %s", ErrNotFound, method, path)
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: %s %s", ErrConflict, method, path)
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: %s %s", ErrUnauthorized, method, path)
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s %s", ErrForbidden, method, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: unexpected HTTP %d%s", method, path, resp.StatusCode, bodySnippet(payload))
	}
	if readErr != nil {
		return nil, fmt.Errorf("reading %s %s response: %w", method, path, readErr)
	}
	return payload, nil
}

// GetDomainByName implements Client via GET /v3/domains?name=<name>.
func (c *httpClient) GetDomainByName(ctx context.Context, name string) (*Domain, error) {
	payload, err := c.do(ctx, http.MethodGet, "/domains?name="+url.QueryEscape(name), nil)
	if err != nil {
		return nil, err
	}

	var out struct {
		Domains []Domain `json:"domains"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decoding domain list: %w", err)
	}
	if len(out.Domains) == 0 {
		return nil, fmt.Errorf("%w: domain %q", ErrNotFound, name)
	}
	return &out.Domains[0], nil
}

// CreateDomain implements Client via POST /v3/domains.
func (c *httpClient) CreateDomain(ctx context.Context, domain Domain) (*Domain, error) {
	payload, err := c.do(ctx, http.MethodPost, "/domains", map[string]Domain{"domain": domain})
	if err != nil {
		return nil, err
	}

	var out struct {
		Domain Domain `json:"domain"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decoding created domain: %w", err)
	}
	return &out.Domain, nil
}

// UpdateDomain implements Client via PATCH /v3/domains/<id>. Nil fields are
// omitted from the patch body so the server leaves them untouched.
func (c *httpClient) UpdateDomain(ctx context.Context, id string, enabled *bool, description *string) error {
	patch := map[string]any{}
	if enabled != nil {
		patch["enabled"] = *enabled
	}
	if description != nil {
		patch["description"] = *description
	}
	_, err := c.do(ctx, http.MethodPatch, "/domains/"+url.PathEscape(id), map[string]any{"domain": patch})
	return err
}

// DeleteDomain implements Client via DELETE /v3/domains/<id>.
func (c *httpClient) DeleteDomain(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/domains/"+url.PathEscape(id), nil)
	return err
}
