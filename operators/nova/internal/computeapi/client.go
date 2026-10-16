// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package computeapi is the operator's minimal client for the parts of the
// Nova compute API a NovaCompute drives: the nova-compute services, the hosts
// the hypervisor list names, the host aggregates, and the server count on a
// host. It authenticates once per client
// with a password-scoped Keystone token and speaks microversion 2.53, where a
// service is addressed by its UUID; the service list asks for 2.69 (see
// ListComputeServices) and the hypervisor list for 2.88 (see ListMappedHosts).
//
// The same client creates and deletes custom traits in Placement (EnsureTrait,
// DeleteTrait) at Placement microversion 1.6, the first that serves /traits.
// It finds Placement in the token's service catalog or at an override (see
// UsePlacement).
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

// ErrPlacementEndpointNotFound is what UsePlacement returns when the token's
// catalog lists no internal Placement endpoint for the region, and what the
// trait calls return before UsePlacement selected one.
var ErrPlacementEndpointNotFound = errors.New("no internal placement endpoint in the service catalog")

// ErrTraitInUse is what DeleteTrait returns when Placement refuses the delete
// (HTTP 409) because a resource provider carries the trait.
var ErrTraitInUse = errors.New("trait is associated with a resource provider")

// APIError is an unexpected HTTP status from Keystone, Nova or Placement.
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
// and hypervisor lists asks for. 2.53 is the first that addresses a service by
// UUID.
const MicroVersion = "2.53"

// serviceListMicroVersion is what the service list asks for. From 2.69 on,
// Nova answers for a cell it cannot reach with one record of status UNKNOWN
// per host instead of leaving the cell's services out.
const serviceListMicroVersion = "2.69"

// serviceStatusUnknown is the status of such a record.
const serviceStatusUnknown = "UNKNOWN"

// hypervisorListMicroVersion is what the hypervisor list asks for. From 2.88
// on, a detail entry no longer carries cpu_info and the resource counters, the
// bulk of a page; the id and the service host stay.
const hypervisorListMicroVersion = "2.88"

// placementMicroVersion is the Placement microversion the trait calls ask
// for. Below 1.6 Placement answers /traits with 404.
const placementMicroVersion = "1.6"

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

	// catalog is the service catalog of the token, and placementURL the
	// Placement base UsePlacement selected from it, "" until then.
	catalog      []catalogEntry
	placementURL string
}

// catalogEntry is one service of a Keystone v3 token's catalog.
type catalogEntry struct {
	Type      string            `json:"type"`
	Endpoints []catalogEndpoint `json:"endpoints"`
}

// catalogEndpoint is one endpoint of a catalog entry.
type catalogEndpoint struct {
	Interface string `json:"interface"`
	Region    string `json:"region"`
	URL       string `json:"url"`
}

// New authenticates against keystoneURL and returns a client for computeURL.
//
// keystoneURL is the Identity v3 base; a URL whose path does not end in /v3
// gets it appended, so "http://keystone:5000" and "http://keystone:5000/v3/"
// both post to /v3/auth/tokens. A 201 without an X-Subject-Token header is an
// error, and so is a 201 whose body does not decode as a token, whose catalog
// the client keeps for UsePlacement. Any other status comes back as an
// *APIError.
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
	var body struct {
		Token struct {
			Catalog []catalogEntry `json:"catalog"`
		} `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding token response: %w", err)
	}

	return &Client{
		doer: doer, computeURL: strings.TrimRight(computeURL, "/"), token: token, catalog: body.Token.Catalog,
	}, nil
}

// UsePlacement selects the Placement base URL the trait calls go to: override
// when it is not empty, else the internal endpoint of the catalog's placement
// service in region, or in any region when region is empty. Without one it
// returns ErrPlacementEndpointNotFound.
func (c *Client) UsePlacement(override, region string) error {
	if override != "" {
		c.placementURL = strings.TrimRight(override, "/")
		return nil
	}
	for _, entry := range c.catalog {
		if entry.Type != "placement" {
			continue
		}
		for _, ep := range entry.Endpoints {
			if ep.Interface == "internal" && (region == "" || ep.Region == region) && ep.URL != "" {
				c.placementURL = strings.TrimRight(ep.URL, "/")
				return nil
			}
		}
	}
	if region == "" {
		return ErrPlacementEndpointNotFound
	}
	return fmt.Errorf("%w for region %s", ErrPlacementEndpointNotFound, region)
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

// hypervisorPageLimit is the page size of the hypervisor list. It is a var
// only so the tests can page through a few entries.
var hypervisorPageLimit = 500

// hypervisorMaxPages bounds the pages ListMappedHosts reads.
const hypervisorMaxPages = 100

// ListMappedHosts returns the hosts of the compute nodes Nova lists as
// hypervisors. Nova leaves a compute node out of that list while its host has
// no host mapping, so a registered service whose host is absent is not mapped
// into its cell yet.
//
// It reads the list page by page, each page after the id of the previous
// page's last entry, until a page comes back empty. A short page does not end
// the list: Nova counts its page limit before it drops the unmapped nodes. An
// entry without a service host is skipped.
func (c *Client) ListMappedHosts(ctx context.Context) (map[string]bool, error) {
	hosts := map[string]bool{}
	marker := ""
	for range hypervisorMaxPages {
		path := "/v2.1/os-hypervisors/detail?limit=" + strconv.Itoa(hypervisorPageLimit)
		if marker != "" {
			path += "&marker=" + url.QueryEscape(marker)
		}
		var out struct {
			Hypervisors []struct {
				ID      string `json:"id"`
				Service *struct {
					Host string `json:"host"`
				} `json:"service"`
			} `json:"hypervisors"`
		}
		if err := c.doVersion(ctx, hypervisorListMicroVersion, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		if len(out.Hypervisors) == 0 {
			return hosts, nil
		}
		for _, hv := range out.Hypervisors {
			if hv.Service != nil && hv.Service.Host != "" {
				hosts[hv.Service.Host] = true
			}
		}
		marker = out.Hypervisors[len(out.Hypervisors)-1].ID
	}
	return nil, fmt.Errorf("the hypervisor list did not end after %d pages", hypervisorMaxPages)
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

// EnsureTrait creates the custom trait name in Placement. created is true when
// Placement created it (201) and false when it existed already (204). Placement
// refuses a name without the CUSTOM_ prefix with a 400.
func (c *Client) EnsureTrait(ctx context.Context, name string) (created bool, err error) {
	path := "/traits/" + url.PathEscape(name)
	code, err := c.doPlacement(ctx, http.MethodPut, path)
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusCreated:
		return true, nil
	case http.StatusNoContent:
		return false, nil
	}
	return false, &APIError{Method: http.MethodPut, Path: path, StatusCode: code}
}

// DeleteTrait deletes the custom trait name from Placement. A trait that is
// already gone is success, and one a resource provider carries yields
// ErrTraitInUse.
func (c *Client) DeleteTrait(ctx context.Context, name string) error {
	_, err := c.doPlacement(ctx, http.MethodDelete, "/traits/"+url.PathEscape(name))
	switch statusOf(err) {
	case http.StatusNotFound:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("%w: %w", ErrTraitInUse, err)
	}
	return err
}

// do sends one authenticated compute request at MicroVersion; see doVersion.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.doVersion(ctx, MicroVersion, method, path, in, out)
}

// doVersion sends one authenticated compute request at version; see send.
func (c *Client) doVersion(ctx context.Context, version, method, path string, in, out any) error {
	_, err := c.send(ctx, c.computeURL, method, path, map[string]string{
		"X-OpenStack-Nova-API-Version": version,
		"OpenStack-API-Version":        "compute " + version,
	}, in, out)
	return err
}

// doPlacement sends one authenticated Placement request without a body at
// placementMicroVersion and returns the 2xx status; see send. Before
// UsePlacement selected an endpoint it sends nothing and returns
// ErrPlacementEndpointNotFound.
func (c *Client) doPlacement(ctx context.Context, method, path string) (int, error) {
	if c.placementURL == "" {
		return 0, ErrPlacementEndpointNotFound
	}
	return c.send(ctx, c.placementURL, method, path, map[string]string{
		"OpenStack-API-Version": "placement " + placementMicroVersion,
	}, nil, nil)
}

// send sends one authenticated request to baseURL+path with headers under
// requestTimeout, decodes a 2xx body into out when out is non-nil, and returns
// the status. Any other status is an *APIError. A transport error is wrapped
// as "<method> <path>: <err>", so a deadline stays detectable with errors.Is.
func (c *Client) send(ctx context.Context, baseURL, method, path string, headers map[string]string,
	in, out any,
) (int, error) {
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("marshaling %s %s request: %w", method, path, err)
		}
		body = bytes.NewReader(payload)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return 0, fmt.Errorf("building %s %s request: %w", method, path, err)
	}
	req.Header.Set("X-Auth-Token", c.token)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.doer.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer closeBody(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, newAPIError(method, path, resp)
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		return 0, fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return resp.StatusCode, nil
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
