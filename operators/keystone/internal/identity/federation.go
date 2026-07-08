// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/federation"
)

// IdentityProvider is the minimal keystone federation identity-provider
// representation the operator needs. The ID doubles as the resource name —
// keystone's federation API addresses identity providers by ID in the path
// (PUT /OS-FEDERATION/identity_providers/{id}).
type IdentityProvider struct {
	ID          string   `json:"id,omitempty"`
	DomainID    string   `json:"domain_id,omitempty"`
	Description string   `json:"description,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
	RemoteIDs   []string `json:"remote_ids,omitempty"`
}

// Protocol binds one federation protocol of an identity provider to a
// mapping.
type Protocol struct {
	ID        string `json:"id,omitempty"`
	MappingID string `json:"mapping_id"`
}

// Group is the minimal keystone group representation for the declarative
// federation target groups.
type Group struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	DomainID    string `json:"domain_id,omitempty"`
	Description string `json:"description,omitempty"`
}

// Role names one keystone role (resolved by name for role assignments).
type Role struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Project names one keystone project (resolved by name for project-scoped
// role assignments).
type Project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	DomainID string `json:"domain_id,omitempty"`
}

// GetIdentityProvider fetches one identity provider by ID via the local REST
// client (gophercloud v2 has no identity-provider surface; upstream
// contributions are tracked separately and gate nothing here).
func (c *httpClient) GetIdentityProvider(ctx context.Context, id string) (*IdentityProvider, error) {
	payload, err := c.do(ctx, http.MethodGet, "/OS-FEDERATION/identity_providers/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		IdentityProvider IdentityProvider `json:"identity_provider"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decoding identity provider: %w", err)
	}
	return &out.IdentityProvider, nil
}

// CreateIdentityProvider registers the identity provider under idp.ID via
// PUT /OS-FEDERATION/identity_providers/{id} (keystone's create verb for this
// resource — the ID lives in the path, not the body).
func (c *httpClient) CreateIdentityProvider(ctx context.Context, idp IdentityProvider) error {
	id := idp.ID
	body := IdentityProvider{
		DomainID:    idp.DomainID,
		Description: idp.Description,
		Enabled:     idp.Enabled,
		RemoteIDs:   idp.RemoteIDs,
	}
	_, err := c.do(ctx, http.MethodPut, "/OS-FEDERATION/identity_providers/"+url.PathEscape(id),
		map[string]IdentityProvider{"identity_provider": body})
	return err
}

// UpdateIdentityProvider patches the enabled flag, description, and/or remote
// IDs of the identity provider with the given ID; nil fields are left
// untouched (a nil remoteIDs slice is omitted from the patch).
func (c *httpClient) UpdateIdentityProvider(ctx context.Context, id string, enabled *bool, description *string, remoteIDs []string) error {
	patch := map[string]any{}
	if enabled != nil {
		patch["enabled"] = *enabled
	}
	if description != nil {
		patch["description"] = *description
	}
	if remoteIDs != nil {
		patch["remote_ids"] = remoteIDs
	}
	_, err := c.do(ctx, http.MethodPatch, "/OS-FEDERATION/identity_providers/"+url.PathEscape(id),
		map[string]any{"identity_provider": patch})
	return err
}

// DeleteIdentityProvider deletes the identity provider with the given ID.
func (c *httpClient) DeleteIdentityProvider(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/OS-FEDERATION/identity_providers/"+url.PathEscape(id), nil)
	return err
}

// protocolPath assembles the nested protocol resource path.
func protocolPath(idpID, id string) string {
	return "/OS-FEDERATION/identity_providers/" + url.PathEscape(idpID) + "/protocols/" + url.PathEscape(id)
}

// GetProtocol fetches one protocol of an identity provider.
func (c *httpClient) GetProtocol(ctx context.Context, idpID, id string) (*Protocol, error) {
	payload, err := c.do(ctx, http.MethodGet, protocolPath(idpID, id), nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Protocol Protocol `json:"protocol"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decoding protocol: %w", err)
	}
	return &out.Protocol, nil
}

// CreateProtocol binds the protocol id of the identity provider to mappingID
// via PUT (keystone's create verb for this resource).
func (c *httpClient) CreateProtocol(ctx context.Context, idpID, id, mappingID string) error {
	_, err := c.do(ctx, http.MethodPut, protocolPath(idpID, id),
		map[string]Protocol{"protocol": {MappingID: mappingID}})
	return err
}

// UpdateProtocol re-points the protocol at a different mapping.
func (c *httpClient) UpdateProtocol(ctx context.Context, idpID, id, mappingID string) error {
	_, err := c.do(ctx, http.MethodPatch, protocolPath(idpID, id),
		map[string]Protocol{"protocol": {MappingID: mappingID}})
	return err
}

// DeleteProtocol deletes the protocol from the identity provider.
func (c *httpClient) DeleteProtocol(ctx context.Context, idpID, id string) error {
	_, err := c.do(ctx, http.MethodDelete, protocolPath(idpID, id), nil)
	return err
}

// mappingServiceClient bridges the per-call password authentication into a
// gophercloud ServiceClient bound to the cluster-local endpoint, bypassing
// catalog lookup entirely (the operator talks to the Service DNS name, never
// to a catalog-published URL). Mappings go through gophercloud — the one
// federation resource its v2 SDK covers — per the Phase-0 client decision;
// identity providers and protocols stay on the local REST client above.
func (c *httpClient) mappingServiceClient(ctx context.Context) (*gophercloud.ServiceClient, error) {
	token, err := c.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	pc := &gophercloud.ProviderClient{}
	pc.SetToken(token)
	// gophercloud carries an http.Client by value; reuse the injected doer
	// when it is a real *http.Client (production and the httptest-backed
	// fake) so transport settings are shared. A non-http.Client HTTPDoer stub
	// cannot be bridged — such tests exercise the stdlib methods only.
	if hc, ok := c.doer.(*http.Client); ok {
		pc.HTTPClient = *hc
	}
	return &gophercloud.ServiceClient{ProviderClient: pc, Endpoint: c.endpoint + "/"}, nil
}

// mapGophercloudError translates gophercloud's unexpected-response-code errors
// into the package sentinel errors so callers branch with errors.Is exactly as
// they do for the stdlib methods.
func mapGophercloudError(err error, op string) error {
	switch {
	case err == nil:
		return nil
	case gophercloud.ResponseCodeIs(err, http.StatusNotFound):
		return fmt.Errorf("%w: %s", ErrNotFound, op)
	case gophercloud.ResponseCodeIs(err, http.StatusConflict):
		return fmt.Errorf("%w: %s", ErrConflict, op)
	case gophercloud.ResponseCodeIs(err, http.StatusUnauthorized):
		return fmt.Errorf("%w: %s", ErrUnauthorized, op)
	case gophercloud.ResponseCodeIs(err, http.StatusForbidden):
		return fmt.Errorf("%w: %s", ErrForbidden, op)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// GetMapping fetches one federation mapping by ID via gophercloud.
func (c *httpClient) GetMapping(ctx context.Context, id string) (*federation.Mapping, error) {
	sc, err := c.mappingServiceClient(ctx)
	if err != nil {
		return nil, err
	}
	m, err := federation.GetMapping(ctx, sc, id).Extract()
	if err != nil {
		return nil, mapGophercloudError(err, "GET mapping "+id)
	}
	return m, nil
}

// CreateMapping creates the federation mapping with the given ID and rules.
func (c *httpClient) CreateMapping(ctx context.Context, id string, rules []federation.MappingRule) error {
	sc, err := c.mappingServiceClient(ctx)
	if err != nil {
		return err
	}
	_, err = federation.CreateMapping(ctx, sc, id, federation.CreateMappingOpts{Rules: rules}).Extract()
	return mapGophercloudError(err, "PUT mapping "+id)
}

// UpdateMapping replaces the rules of the federation mapping with the given ID.
func (c *httpClient) UpdateMapping(ctx context.Context, id string, rules []federation.MappingRule) error {
	sc, err := c.mappingServiceClient(ctx)
	if err != nil {
		return err
	}
	_, err = federation.UpdateMapping(ctx, sc, id, federation.UpdateMappingOpts{Rules: rules}).Extract()
	return mapGophercloudError(err, "PATCH mapping "+id)
}

// DeleteMapping deletes the federation mapping with the given ID.
func (c *httpClient) DeleteMapping(ctx context.Context, id string) error {
	sc, err := c.mappingServiceClient(ctx)
	if err != nil {
		return err
	}
	err = federation.DeleteMapping(ctx, sc, id).ExtractErr()
	return mapGophercloudError(err, "DELETE mapping "+id)
}

// GetGroupByName resolves a group by exact name inside a domain, returning
// ErrNotFound (wrapped) when no such group exists.
func (c *httpClient) GetGroupByName(ctx context.Context, name, domainID string) (*Group, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("domain_id", domainID)
	payload, err := c.do(ctx, http.MethodGet, "/groups?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Groups []Group `json:"groups"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decoding group list: %w", err)
	}
	if len(out.Groups) == 0 {
		return nil, fmt.Errorf("%w: group %q in domain %s", ErrNotFound, name, domainID)
	}
	return &out.Groups[0], nil
}

// CreateGroup creates the given group and returns the server-side
// representation (with the assigned ID).
func (c *httpClient) CreateGroup(ctx context.Context, group Group) (*Group, error) {
	payload, err := c.do(ctx, http.MethodPost, "/groups", map[string]Group{"group": group})
	if err != nil {
		return nil, err
	}
	var out struct {
		Group Group `json:"group"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decoding created group: %w", err)
	}
	return &out.Group, nil
}

// GetRoleByName resolves a role by exact name, returning ErrNotFound (wrapped)
// when no such role exists.
func (c *httpClient) GetRoleByName(ctx context.Context, name string) (*Role, error) {
	q := url.Values{}
	q.Set("name", name)
	payload, err := c.do(ctx, http.MethodGet, "/roles?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Roles []Role `json:"roles"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decoding role list: %w", err)
	}
	if len(out.Roles) == 0 {
		return nil, fmt.Errorf("%w: role %q", ErrNotFound, name)
	}
	return &out.Roles[0], nil
}

// GetProjectByName resolves a project by exact name inside a domain,
// returning ErrNotFound (wrapped) when no such project exists.
func (c *httpClient) GetProjectByName(ctx context.Context, name, domainID string) (*Project, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("domain_id", domainID)
	payload, err := c.do(ctx, http.MethodGet, "/projects?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Projects []Project `json:"projects"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decoding project list: %w", err)
	}
	if len(out.Projects) == 0 {
		return nil, fmt.Errorf("%w: project %q in domain %s", ErrNotFound, name, domainID)
	}
	return &out.Projects[0], nil
}

// AssignRoleToGroupOnDomain grants the role to the group on the domain
// (PUT is idempotent — re-asserting an existing assignment succeeds).
func (c *httpClient) AssignRoleToGroupOnDomain(ctx context.Context, domainID, groupID, roleID string) error {
	path := "/domains/" + url.PathEscape(domainID) + "/groups/" + url.PathEscape(groupID) + "/roles/" + url.PathEscape(roleID)
	_, err := c.do(ctx, http.MethodPut, path, nil)
	return err
}

// AssignRoleToGroupOnProject grants the role to the group on the project
// (PUT is idempotent — re-asserting an existing assignment succeeds).
func (c *httpClient) AssignRoleToGroupOnProject(ctx context.Context, projectID, groupID, roleID string) error {
	path := "/projects/" + url.PathEscape(projectID) + "/groups/" + url.PathEscape(groupID) + "/roles/" + url.PathEscape(roleID)
	_, err := c.do(ctx, http.MethodPut, path, nil)
	return err
}
