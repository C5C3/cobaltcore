// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package computeapi is the operator's minimal client for the parts of the
// Nova compute API a NovaCompute drives: the nova-compute services, the host
// aggregates, and the server count on a host. It authenticates once per client
// with a password-scoped Keystone token and speaks microversion 2.53, where a
// service is addressed by its UUID; the service list alone asks for 2.69 (see
// ListComputeServices).
//
// It is stdlib only (net/http and encoding/json). Every request goes through a
// Doer, the same seam the operator's health check uses, so a placed Nova is
// reached through the target cluster's service proxy and tests inject an
// in-memory fake (see computeapitest).
package computeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Doer sends one HTTP request. *http.Client satisfies it, and so do the
// service-proxy doer of a placed Nova and the computeapitest fake.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Credentials identify the Keystone user the client authenticates as, and the
// project its token is scoped to.
type Credentials struct {
	Username          string
	Password          string
	ProjectName       string
	UserDomainName    string
	ProjectDomainName string
}

// Service is one nova-compute service as os-services reports it.
type Service struct {
	// ID is the service UUID.
	ID string `json:"id"`
	// Host is the host the service registered under, the node name.
	Host string `json:"host"`
	// Status is the administrative status, enabled or disabled.
	Status string `json:"status"`
	// State is the liveness, up or down.
	State string `json:"state"`
	// DisabledReason is the reason recorded with a disabled status. Nova
	// reports null on an enabled service, which decodes to "".
	DisabledReason string `json:"disabled_reason"`
}

// Aggregate is one host aggregate as os-aggregates reports it. A null
// availability_zone decodes to "".
type Aggregate struct {
	ID               int               `json:"id"`
	Name             string            `json:"name"`
	AvailabilityZone string            `json:"availability_zone"`
	Hosts            []string          `json:"hosts"`
	Metadata         map[string]string `json:"metadata"`
}

// ErrServiceHasInstances is what DeleteService returns when Nova refuses the
// delete (HTTP 409) because instances or migrations remain on the host.
var ErrServiceHasInstances = errors.New("compute service still hosts instances or migrations")

// ErrAggregateExists is what CreateAggregate returns when Nova refuses the
// create (HTTP 409) because an aggregate of that name exists.
var ErrAggregateExists = errors.New("aggregate name already exists")

// APIError is an unexpected HTTP status from Keystone or Nova.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

// maxErrorBody bounds how much of an error body an APIError message carries.
const maxErrorBody = 512

// Error renders "<method> <path>: HTTP <code>: <body>", the body cut to its
// first 512 bytes.
func (e *APIError) Error() string {
	body := e.Body
	if len(body) > maxErrorBody {
		body = body[:maxErrorBody]
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, body)
}

// requestTimeout bounds every request the client sends, the token request
// included, so an unreachable API costs a reconcile pass ten seconds rather
// than the transport's own timeout. It is a var only so the tests can exercise
// the timeout without waiting ten seconds.
var requestTimeout = 10 * time.Second

// MicroVersion is the compute API microversion every request but the service
// list asks for. 2.53 is the first that addresses a service by UUID.
const MicroVersion = "2.53"

// serviceListMicroVersion is what the service list asks for. From 2.69 on,
// Nova answers for a cell it cannot reach with one record of status UNKNOWN
// per host instead of leaving the cell's services out.
const serviceListMicroVersion = "2.69"

// serviceStatusUnknown is the status of such a record.
const serviceStatusUnknown = "UNKNOWN"

// maxResponseBody bounds how much of a response body the client decodes. A
// servers list at the default page limit of a thousand entries is a fraction
// of it.
const maxResponseBody = 8 << 20

// Client is an authenticated compute API client. It holds one token for its
// lifetime; callers build one per reconcile pass.
type Client struct {
	doer       Doer
	computeURL string
	token      string
}

// New authenticates against keystoneURL and returns a client for computeURL.
//
// keystoneURL is the Identity v3 base; a URL whose path does not end in /v3
// gets it appended, so "http://keystone:5000" and "http://keystone:5000/v3/"
// both post to /v3/auth/tokens. A 201 without an X-Subject-Token header is an
// error, and any other status comes back as an *APIError.
func New(ctx context.Context, doer Doer, keystoneURL, computeURL string, creds Credentials) (*Client, error) {
	base := strings.TrimRight(keystoneURL, "/")
	if !strings.HasSuffix(base, "/v3") {
		base += "/v3"
	}
	tokenURL := base + "/auth/tokens"

	payload, err := json.Marshal(tokenRequest(creds))
	if err != nil {
		return nil, fmt.Errorf("marshaling token request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	path := req.URL.Path
	resp, err := doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", http.MethodPost, path, err)
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusCreated {
		return nil, newAPIError(http.MethodPost, path, resp)
	}
	token := resp.Header.Get("X-Subject-Token")
	if token == "" {
		return nil, errors.New("keystone returned no X-Subject-Token")
	}

	return &Client{doer: doer, computeURL: strings.TrimRight(computeURL, "/"), token: token}, nil
}

// tokenRequest is the password-method, project-scoped token request body.
func tokenRequest(creds Credentials) map[string]any {
	return map[string]any{
		"auth": map[string]any{
			"identity": map[string]any{
				"methods": []string{"password"},
				"password": map[string]any{
					"user": map[string]any{
						"name":     creds.Username,
						"domain":   map[string]string{"name": creds.UserDomainName},
						"password": creds.Password,
					},
				},
			},
			"scope": map[string]any{
				"project": map[string]any{
					"name":   creds.ProjectName,
					"domain": map[string]string{"name": creds.ProjectDomainName},
				},
			},
		},
	}
}

// ListComputeServices lists the nova-compute services. A service in a cell
// that did not answer fails the list: below 2.69 Nova would leave it out, and
// the servers list skips that cell too, so its hosts would read as having
// neither a service nor a server.
func (c *Client) ListComputeServices(ctx context.Context) ([]Service, error) {
	var out struct {
		Services []Service `json:"services"`
	}
	if err := c.doVersion(ctx, serviceListMicroVersion, http.MethodGet,
		"/v2.1/os-services?binary=nova-compute", nil, &out); err != nil {
		return nil, err
	}
	for _, svc := range out.Services {
		if svc.Status == serviceStatusUnknown {
			return nil, fmt.Errorf("the cell of host %s did not answer, so the service list is incomplete", svc.Host)
		}
	}
	return out.Services, nil
}

// DisableService disables the service with the given id and records reason.
func (c *Client) DisableService(ctx context.Context, id, reason string) error {
	body := map[string]string{"status": "disabled", "disabled_reason": reason}
	return c.do(ctx, http.MethodPut, "/v2.1/os-services/"+url.PathEscape(id), body, nil)
}

// DeleteService deletes the service with the given id. Nova removes the host
// from every aggregate, deletes its resource providers and destroys its host
// mapping with it. A service that is already gone is success, and a host that
// still has instances or migrations yields ErrServiceHasInstances.
func (c *Client) DeleteService(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/v2.1/os-services/"+url.PathEscape(id), nil, nil)
	switch statusOf(err) {
	case http.StatusNotFound:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("%w: %w", ErrServiceHasInstances, err)
	}
	return err
}

// CountServersOnHost counts the servers of every project on host. The host
// filter is admin-only under Nova's default policies.
func (c *Client) CountServersOnHost(ctx context.Context, host string) (int, error) {
	var out struct {
		Servers []json.RawMessage `json:"servers"`
	}
	path := "/v2.1/servers?all_tenants=1&host=" + url.QueryEscape(host)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return 0, err
	}
	return len(out.Servers), nil
}

// ListAggregates lists the host aggregates.
func (c *Client) ListAggregates(ctx context.Context) ([]Aggregate, error) {
	var out struct {
		Aggregates []Aggregate `json:"aggregates"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2.1/os-aggregates", nil, &out); err != nil {
		return nil, err
	}
	return out.Aggregates, nil
}

// CreateAggregate creates an aggregate. An empty zone creates one without an
// availability zone. A name that exists yields ErrAggregateExists.
func (c *Client) CreateAggregate(ctx context.Context, name, zone string) (Aggregate, error) {
	aggregate := map[string]string{"name": name}
	if zone != "" {
		aggregate["availability_zone"] = zone
	}
	var out struct {
		Aggregate Aggregate `json:"aggregate"`
	}
	err := c.do(ctx, http.MethodPost, "/v2.1/os-aggregates", map[string]any{"aggregate": aggregate}, &out)
	if statusOf(err) == http.StatusConflict {
		return Aggregate{}, fmt.Errorf("%w: %w", ErrAggregateExists, err)
	}
	if err != nil {
		return Aggregate{}, err
	}
	return out.Aggregate, nil
}

// SetAggregateMetadata sets the given metadata keys on the aggregate. Keys it
// does not name are left as they are.
func (c *Client) SetAggregateMetadata(ctx context.Context, id int, md map[string]string) error {
	body := map[string]any{"set_metadata": map[string]any{"metadata": md}}
	return c.do(ctx, http.MethodPost, "/v2.1/os-aggregates/"+strconv.Itoa(id)+"/action", body, nil)
}

// DeleteAggregate deletes the aggregate. One that is already gone is success.
func (c *Client) DeleteAggregate(ctx context.Context, id int) error {
	err := c.do(ctx, http.MethodDelete, "/v2.1/os-aggregates/"+strconv.Itoa(id), nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// do sends one authenticated compute request at MicroVersion; see doVersion.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.doVersion(ctx, MicroVersion, method, path, in, out)
}

// doVersion sends one authenticated compute request at version under
// requestTimeout, decodes a 2xx body into out when out is non-nil, and returns
// an *APIError for any other status. A transport error is wrapped as
// "<method> <path>: <err>", so a deadline stays detectable with errors.Is.
func (c *Client) doVersion(ctx context.Context, version, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshaling %s %s request: %w", method, path, err)
		}
		body = bytes.NewReader(payload)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, c.computeURL+path, body)
	if err != nil {
		return fmt.Errorf("building %s %s request: %w", method, path, err)
	}
	req.Header.Set("X-Auth-Token", c.token)
	req.Header.Set("X-OpenStack-Nova-API-Version", version)
	req.Header.Set("OpenStack-API-Version", "compute "+version)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.doer.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer closeBody(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIError(method, path, resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return nil
}

// newAPIError reads the first bytes of resp's body into an *APIError.
func newAPIError(method, path string, resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: string(body)}
}

// statusOf returns the HTTP status an *APIError in err's chain carries, or 0.
func statusOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// closeBody drains and closes a response body so the transport can reuse the
// connection.
func closeBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	_ = resp.Body.Close()
}
