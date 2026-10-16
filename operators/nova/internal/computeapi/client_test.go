// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package computeapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi/computeapitest"
)

const (
	testKeystoneURL = "http://keystone.openstack.svc.cluster.local:5000/v3"
	testComputeURL  = "http://nova.openstack.svc.cluster.local:8774"
)

var testCreds = Credentials{
	Username: "nova", Password: "s3cret", ProjectName: "service",
	UserDomainName: "Default", ProjectDomainName: "Default",
}

// stubDoer answers every request with the handler's response and records the
// requests it saw.
type stubDoer struct {
	handler  http.HandlerFunc
	requests []*http.Request
	bodies   []string
}

func (d *stubDoer) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	d.requests = append(d.requests, req)
	d.bodies = append(d.bodies, string(body))
	rec := httptest.NewRecorder()
	d.handler(rec, req)
	return rec.Result(), nil
}

// tokenThen answers the token request with a token and every later request
// with compute.
func tokenThen(compute http.HandlerFunc) *stubDoer {
	return tokenWithBodyThen(`{"token":{}}`, compute)
}

// tokenWithBodyThen answers the token request with a token and body, and every
// later request with next.
func tokenWithBodyThen(body string, next http.HandlerFunc) *stubDoer {
	return &stubDoer{handler: func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/tokens") {
			w.Header().Set("X-Subject-Token", "tok")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, body)
			return
		}
		next(w, r)
	}}
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func newClient(t *testing.T, doer Doer) *Client {
	t.Helper()
	c, err := New(context.Background(), doer, testKeystoneURL, testComputeURL, testCreds)
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	return c
}

func TestNew_TokenRequest(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := tokenThen(status(http.StatusOK, "{}"))
	c := newClient(t, doer)
	g.Expect(c.token).To(Equal("tok"))

	g.Expect(doer.requests).To(HaveLen(1))
	req := doer.requests[0]
	g.Expect(req.Method).To(Equal(http.MethodPost))
	g.Expect(req.URL.String()).To(Equal(testKeystoneURL + "/auth/tokens"))

	var body map[string]any
	g.Expect(json.Unmarshal([]byte(doer.bodies[0]), &body)).To(Succeed())
	auth := body["auth"].(map[string]any)
	user := auth["identity"].(map[string]any)["password"].(map[string]any)["user"].(map[string]any)
	g.Expect(user["name"]).To(Equal("nova"))
	g.Expect(user["password"]).To(Equal("s3cret"))
	project := auth["scope"].(map[string]any)["project"].(map[string]any)
	g.Expect(project["name"]).To(Equal("service"))
}

// TestNew_NormalizesTheKeystoneURL pins that a URL with or without the /v3
// suffix, and with or without a trailing slash, posts to /v3/auth/tokens.
func TestNew_NormalizesTheKeystoneURL(t *testing.T) {
	for _, keystoneURL := range []string{"http://k:5000", "http://k:5000/", "http://k:5000/v3", "http://k:5000/v3/"} {
		t.Run(keystoneURL, func(t *testing.T) {
			g := NewGomegaWithT(t)
			doer := tokenThen(status(http.StatusOK, "{}"))
			_, err := New(context.Background(), doer, keystoneURL, testComputeURL, testCreds)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(doer.requests[0].URL.Host).To(Equal("k:5000"))
			g.Expect(doer.requests[0].URL.Path).To(Equal("/v3/auth/tokens"))
		})
	}
}

func TestNew_CreatedWithoutSubjectTokenFails(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := &stubDoer{handler: status(http.StatusCreated, "{}")}
	_, err := New(context.Background(), doer, testKeystoneURL, testComputeURL, testCreds)
	g.Expect(err).To(MatchError("keystone returned no X-Subject-Token"))
}

// TestNew_BodyThatIsNotJSONFails pins that the token body is decoded: the
// client keeps its catalog for UsePlacement.
func TestNew_BodyThatIsNotJSONFails(t *testing.T) {
	g := NewGomegaWithT(t)
	for _, body := range []string{"nope", ""} {
		_, err := New(context.Background(), tokenWithBodyThen(body, status(http.StatusOK, "{}")),
			testKeystoneURL, testComputeURL, testCreds)
		g.Expect(err).To(MatchError(ContainSubstring("decoding token response")), "body %q", body)
	}
}

func TestNew_UnauthorizedIsAnAPIError(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := &stubDoer{handler: status(http.StatusUnauthorized, `{"error":{"code":401}}`)}
	_, err := New(context.Background(), doer, testKeystoneURL, testComputeURL, testCreds)

	var apiErr *APIError
	g.Expect(errors.As(err, &apiErr)).To(BeTrue(), "got %v", err)
	g.Expect(apiErr.StatusCode).To(Equal(http.StatusUnauthorized))
	g.Expect(apiErr.Method).To(Equal(http.MethodPost))
	g.Expect(apiErr.Path).To(Equal("/v3/auth/tokens"))
	g.Expect(err.Error()).NotTo(ContainSubstring("s3cret"), "the password never reaches an error")
}

func TestNew_TransportErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	refused := errors.New("connection refused")
	doer := doerFunc(func(*http.Request) (*http.Response, error) { return nil, refused })
	_, err := New(context.Background(), doer, testKeystoneURL, testComputeURL, testCreds)
	g.Expect(err).To(MatchError(ContainSubstring("POST /v3/auth/tokens: connection refused")))
	g.Expect(errors.Is(err, refused)).To(BeTrue())
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// TestRequests_CarryTheMicroversionAndToken pins the three headers every
// compute request sends.
func TestRequests_CarryTheMicroversionAndToken(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := tokenThen(status(http.StatusOK, `{"services":[],"aggregates":[],"servers":[],"hypervisors":[]}`))
	c := newClient(t, doer)
	ctx := context.Background()

	_, _ = c.ListComputeServices(ctx)
	_, _ = c.ListMappedHosts(ctx)
	_, _ = c.ListAggregates(ctx)
	_, _ = c.CountServersOnHost(ctx, "node-1")
	_ = c.DisableService(ctx, "id", "reason")
	_ = c.DeleteService(ctx, "id")
	_, _ = c.CreateAggregate(ctx, "az1", "az1")
	_ = c.SetAggregateMetadata(ctx, 1, map[string]string{"k": "v"})
	_ = c.DeleteAggregate(ctx, 1)

	compute := doer.requests[1:]
	g.Expect(compute).To(HaveLen(9))
	for _, req := range compute {
		// The service list asks for 2.69, which reports a cell that did not
		// answer instead of leaving its services out, and the hypervisor list
		// for 2.88, whose entries drop cpu_info and the resource counters.
		version := "2.53"
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/v2.1/os-services":
			version = "2.69"
		case req.Method == http.MethodGet && req.URL.Path == "/v2.1/os-hypervisors/detail":
			version = "2.88"
		}
		g.Expect(req.Header.Get("X-Auth-Token")).To(Equal("tok"), "%s %s", req.Method, req.URL)
		g.Expect(req.Header.Get("X-OpenStack-Nova-API-Version")).To(Equal(version), "%s %s", req.Method, req.URL)
		g.Expect(req.Header.Get("OpenStack-API-Version")).To(Equal("compute "+version), "%s %s", req.Method, req.URL)
		g.Expect(req.URL.Host).To(Equal("nova.openstack.svc.cluster.local:8774"))
	}
}

func TestListComputeServices(t *testing.T) {
	t.Run("an empty list is an empty slice", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := newClient(t, tokenThen(status(http.StatusOK, `{"services":[]}`)))
		services, err := c.ListComputeServices(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(services).NotTo(BeNil())
		g.Expect(services).To(BeEmpty())
	})

	t.Run("fields are decoded and a null reason is empty", func(t *testing.T) {
		g := NewGomegaWithT(t)
		doer := tokenThen(status(http.StatusOK, `{"services":[`+
			`{"id":"a","host":"node-1","status":"enabled","state":"up","disabled_reason":null},`+
			`{"id":"b","host":"node-2","status":"disabled","state":"down","disabled_reason":"maintenance"}]}`))
		c := newClient(t, doer)
		services, err := c.ListComputeServices(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(services).To(Equal([]Service{
			{ID: "a", Host: "node-1", Status: "enabled", State: "up"},
			{ID: "b", Host: "node-2", Status: "disabled", State: "down", DisabledReason: "maintenance"},
		}))
		g.Expect(doer.requests[1].URL.RequestURI()).To(Equal("/v2.1/os-services?binary=nova-compute"))
	})

	t.Run("a service of a cell that did not answer fails the list", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := newClient(t, tokenThen(status(http.StatusOK, `{"services":[`+
			`{"id":"a","host":"node-1","status":"enabled","state":"up","disabled_reason":null},`+
			`{"binary":"nova-compute","host":"node-2","status":"UNKNOWN","zone":"nova"}]}`)))
		services, err := c.ListComputeServices(context.Background())
		g.Expect(err).To(MatchError("the cell of host node-2 did not answer, so the service list is incomplete"))
		g.Expect(services).To(BeNil())
	})

	t.Run("the fake reports a down cell at 2.69 only", func(t *testing.T) {
		g := NewGomegaWithT(t)
		fake := computeapitest.New()
		fake.AddService("node-1", "enabled", "up")
		fake.SetServers("node-1", 2)
		fake.SetCellDown("node-1")
		c := newClient(t, fake)

		_, err := c.ListComputeServices(context.Background())
		g.Expect(err).To(MatchError(ContainSubstring("the cell of host node-1 did not answer")))

		var below struct {
			Services []Service `json:"services"`
		}
		g.Expect(c.do(context.Background(), http.MethodGet, "/v2.1/os-services?binary=nova-compute", nil, &below)).To(Succeed())
		g.Expect(below.Services).To(BeEmpty(), "below 2.69 Nova leaves the cell's services out")
		count, err := c.CountServersOnHost(context.Background(), "node-1")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(count).To(BeZero(), "the servers list skips the cell too")
	})

	t.Run("a server error is an APIError naming the call", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := newClient(t, tokenThen(status(http.StatusInternalServerError, "boom")))
		_, err := c.ListComputeServices(context.Background())
		g.Expect(err).To(MatchError("GET /v2.1/os-services?binary=nova-compute: HTTP 500: boom"))
	})

	t.Run("an undecodable body is an error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := newClient(t, tokenThen(status(http.StatusOK, "<html>")))
		_, err := c.ListComputeServices(context.Background())
		g.Expect(err).To(MatchError(ContainSubstring("decoding GET /v2.1/os-services")))
	})
}

func TestListMappedHosts(t *testing.T) {
	t.Run("every page is read, each after the previous page's last id", func(t *testing.T) {
		g := NewGomegaWithT(t)
		previous := hypervisorPageLimit
		hypervisorPageLimit = 2
		t.Cleanup(func() { hypervisorPageLimit = previous })

		fake := computeapitest.New()
		for _, host := range []string{"node-1", "node-2", "node-3"} {
			fake.AddService(host, "enabled", "up")
		}
		c := newClient(t, fake)

		hosts, err := c.ListMappedHosts(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(hosts).To(Equal(map[string]bool{"node-1": true, "node-2": true, "node-3": true}))

		calls := fake.CallsTo(http.MethodGet, "/v2.1/os-hypervisors/detail")
		g.Expect(calls).To(HaveLen(3), "two pages with entries and the empty page that ends the list")
		g.Expect(calls[0].Query).To(Equal("limit=2"))
		g.Expect(calls[1].Query).To(Equal("limit=2&marker=10000000-0000-0000-0000-000000000002"))
		g.Expect(calls[2].Query).To(Equal("limit=2&marker=10000000-0000-0000-0000-000000000003"))
	})

	t.Run("a page an unmapped node shortens does not end the list", func(t *testing.T) {
		g := NewGomegaWithT(t)
		previous := hypervisorPageLimit
		hypervisorPageLimit = 2
		t.Cleanup(func() { hypervisorPageLimit = previous })

		fake := computeapitest.New()
		for _, host := range []string{"node-1", "node-2", "node-3"} {
			fake.AddService(host, "enabled", "up")
		}
		fake.SetHostMapped("node-2", false)
		c := newClient(t, fake)

		hosts, err := c.ListMappedHosts(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(hosts).To(Equal(map[string]bool{"node-1": true, "node-3": true}))

		calls := fake.CallsTo(http.MethodGet, "/v2.1/os-hypervisors/detail")
		g.Expect(calls).To(HaveLen(3), "a page holding node-1 alone, one holding node-3 and the empty end")
		g.Expect(calls[1].Query).To(Equal("limit=2&marker=10000000-0000-0000-0000-000000000001"))
		g.Expect(calls[2].Query).To(Equal("limit=2&marker=10000000-0000-0000-0000-000000000003"))
	})

	t.Run("an empty list is an empty set", func(t *testing.T) {
		g := NewGomegaWithT(t)
		doer := tokenThen(status(http.StatusOK, `{"hypervisors": []}`))
		c := newClient(t, doer)
		hosts, err := c.ListMappedHosts(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(hosts).NotTo(BeNil())
		g.Expect(hosts).To(BeEmpty())
		g.Expect(doer.requests[1].URL.RequestURI()).To(Equal("/v2.1/os-hypervisors/detail?limit=500"))
	})

	t.Run("an entry without a service or a host is skipped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		pages := []string{
			`{"hypervisors":[{"id":"a","service":{"host":"node-1"}},{"id":"b"},{"id":"c","service":{"host":""}}]}`,
			`{"hypervisors":[]}`,
		}
		c := newClient(t, tokenThen(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, pages[0])
			pages = pages[1:]
		}))
		hosts, err := c.ListMappedHosts(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(hosts).To(Equal(map[string]bool{"node-1": true}))
	})

	t.Run("an unmapped host is absent while its service is listed", func(t *testing.T) {
		g := NewGomegaWithT(t)
		fake := computeapitest.New()
		fake.AddService("node-1", "enabled", "up")
		fake.AddService("node-2", "enabled", "up")
		fake.SetHostMapped("node-2", false)
		c := newClient(t, fake)

		hosts, err := c.ListMappedHosts(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(hosts).To(Equal(map[string]bool{"node-1": true}))
		services, err := c.ListComputeServices(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(services).To(HaveLen(2))

		fake.SetHostMapped("node-2", true)
		hosts, err = c.ListMappedHosts(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(hosts).To(HaveKey("node-2"))
	})

	t.Run("a server error is an APIError", func(t *testing.T) {
		g := NewGomegaWithT(t)
		fake := computeapitest.New()
		fake.AddService("node-1", "enabled", "up")
		fake.FailNext(http.MethodGet, "/v2.1/os-hypervisors", http.StatusInternalServerError)
		c := newClient(t, fake)

		hosts, err := c.ListMappedHosts(context.Background())
		g.Expect(hosts).To(BeNil())
		var apiErr *APIError
		g.Expect(errors.As(err, &apiErr)).To(BeTrue(), "got %v", err)
		g.Expect(apiErr.StatusCode).To(Equal(http.StatusInternalServerError))
		g.Expect(apiErr.Method).To(Equal(http.MethodGet))
		g.Expect(apiErr.Path).To(Equal("/v2.1/os-hypervisors/detail?limit=500"))
	})

	t.Run("a list that never ends is an error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		doer := tokenThen(status(http.StatusOK, `{"hypervisors":[{"id":"a","service":{"host":"node-1"}}]}`))
		c := newClient(t, doer)
		hosts, err := c.ListMappedHosts(context.Background())
		g.Expect(err).To(MatchError("the hypervisor list did not end after 100 pages"))
		g.Expect(hosts).To(BeNil())
		g.Expect(doer.requests[1:]).To(HaveLen(100))
	})
}

func TestDisableService(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := tokenThen(status(http.StatusOK, "{}"))
	c := newClient(t, doer)
	g.Expect(c.DisableService(context.Background(), "abc", "c5c3.io: leaving")).To(Succeed())

	req := doer.requests[1]
	g.Expect(req.Method).To(Equal(http.MethodPut))
	g.Expect(req.URL.Path).To(Equal("/v2.1/os-services/abc"))
	g.Expect(doer.bodies[1]).To(MatchJSON(`{"status":"disabled","disabled_reason":"c5c3.io: leaving"}`))
}

func TestDeleteService(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		wantErr error
		wantNil bool
	}{
		{name: "204 is success", code: http.StatusNoContent, wantNil: true},
		{name: "404 is success", code: http.StatusNotFound, wantNil: true},
		{name: "409 is ErrServiceHasInstances", code: http.StatusConflict, wantErr: ErrServiceHasInstances},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c := newClient(t, tokenThen(status(tc.code, "")))
			err := c.DeleteService(context.Background(), "abc")
			if tc.wantNil {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(errors.Is(err, tc.wantErr)).To(BeTrue(), "got %v", err)
		})
	}

	t.Run("403 stays an APIError", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := newClient(t, tokenThen(status(http.StatusForbidden, "policy")))
		err := c.DeleteService(context.Background(), "abc")
		var apiErr *APIError
		g.Expect(errors.As(err, &apiErr)).To(BeTrue())
		g.Expect(apiErr.StatusCode).To(Equal(http.StatusForbidden))
	})
}

func TestCountServersOnHost(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := tokenThen(status(http.StatusOK, `{"servers":[{"id":"1"},{"id":"2"}]}`))
	c := newClient(t, doer)
	n, err := c.CountServersOnHost(context.Background(), "node 1")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(n).To(Equal(2))
	g.Expect(doer.requests[1].URL.Query().Get("host")).To(Equal("node 1"))
	g.Expect(doer.requests[1].URL.Query().Get("all_tenants")).To(Equal("1"))
}

func TestListAggregates(t *testing.T) {
	t.Run("an empty list is an empty slice", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := newClient(t, tokenThen(status(http.StatusOK, `{"aggregates":[]}`)))
		aggregates, err := c.ListAggregates(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(aggregates).NotTo(BeNil())
		g.Expect(aggregates).To(BeEmpty())
	})

	t.Run("a null zone is empty", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := newClient(t, tokenThen(status(http.StatusOK, `{"aggregates":[`+
			`{"id":3,"name":"tenant_filter_tests","availability_zone":null,"hosts":["n1"],"metadata":{"k":"v"}}]}`)))
		aggregates, err := c.ListAggregates(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(aggregates).To(Equal([]Aggregate{
			{ID: 3, Name: "tenant_filter_tests", Hosts: []string{"n1"}, Metadata: map[string]string{"k": "v"}},
		}))
	})
}

func TestCreateAggregate(t *testing.T) {
	t.Run("an empty zone sends no availability_zone key", func(t *testing.T) {
		g := NewGomegaWithT(t)
		doer := tokenThen(status(http.StatusOK, `{"aggregate":{"id":1,"name":"tenant_filter_tests","availability_zone":null}}`))
		c := newClient(t, doer)
		agg, err := c.CreateAggregate(context.Background(), "tenant_filter_tests", "")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(agg.ID).To(Equal(1))
		g.Expect(doer.bodies[1]).To(MatchJSON(`{"aggregate":{"name":"tenant_filter_tests"}}`))
	})

	t.Run("a zone is sent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		doer := tokenThen(status(http.StatusOK, `{"aggregate":{"id":2,"name":"az1","availability_zone":"az1"}}`))
		c := newClient(t, doer)
		agg, err := c.CreateAggregate(context.Background(), "az1", "az1")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(agg.AvailabilityZone).To(Equal("az1"))
		g.Expect(doer.bodies[1]).To(MatchJSON(`{"aggregate":{"name":"az1","availability_zone":"az1"}}`))
	})

	t.Run("409 is ErrAggregateExists", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := newClient(t, tokenThen(status(http.StatusConflict, "exists")))
		_, err := c.CreateAggregate(context.Background(), "az1", "az1")
		g.Expect(errors.Is(err, ErrAggregateExists)).To(BeTrue(), "got %v", err)
	})
}

func TestSetAggregateMetadata(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := tokenThen(status(http.StatusOK, "{}"))
	c := newClient(t, doer)
	g.Expect(c.SetAggregateMetadata(context.Background(), 7, map[string]string{"c5c3.io:nova": "ns/nova"})).To(Succeed())
	g.Expect(doer.requests[1].URL.Path).To(Equal("/v2.1/os-aggregates/7/action"))
	g.Expect(doer.bodies[1]).To(MatchJSON(`{"set_metadata":{"metadata":{"c5c3.io:nova":"ns/nova"}}}`))
}

func TestDeleteAggregate(t *testing.T) {
	t.Run("404 is success", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := newClient(t, tokenThen(status(http.StatusNotFound, "")))
		g.Expect(c.DeleteAggregate(context.Background(), 7)).To(Succeed())
	})

	t.Run("400 is an APIError", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := newClient(t, tokenThen(status(http.StatusBadRequest, "not empty")))
		err := c.DeleteAggregate(context.Background(), 7)
		g.Expect(err).To(MatchError("DELETE /v2.1/os-aggregates/7: HTTP 400: not empty"))
	})
}

// TestRequestTimeout pins that a doer blocking past the timeout yields an error
// that still carries context.DeadlineExceeded, for the token request and for a
// compute request alike.
func TestRequestTimeout(t *testing.T) {
	previous := requestTimeout
	requestTimeout = 50 * time.Millisecond
	t.Cleanup(func() { requestTimeout = previous })

	blocking := func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}

	t.Run("token request", func(t *testing.T) {
		g := NewGomegaWithT(t)
		_, err := New(context.Background(), doerFunc(blocking), testKeystoneURL, testComputeURL, testCreds)
		g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
	})

	t.Run("compute and placement requests", func(t *testing.T) {
		g := NewGomegaWithT(t)
		authenticated := false
		doer := doerFunc(func(req *http.Request) (*http.Response, error) {
			if !authenticated {
				authenticated = true
				rec := httptest.NewRecorder()
				rec.Header().Set("X-Subject-Token", "tok")
				rec.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(rec, `{"token":{}}`)
				return rec.Result(), nil
			}
			return blocking(req)
		})
		c := newClient(t, doer)
		_, err := c.ListComputeServices(context.Background())
		g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
		g.Expect(err.Error()).To(HavePrefix("GET /v2.1/os-services?binary=nova-compute: "))

		_, err = c.ListMappedHosts(context.Background())
		g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
		g.Expect(err.Error()).To(HavePrefix("GET /v2.1/os-hypervisors/detail?limit=500: "))

		g.Expect(c.UsePlacement(testPlacementURL, "")).To(Succeed())
		_, err = c.EnsureTrait(context.Background(), "CUSTOM_A")
		g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
		g.Expect(err.Error()).To(HavePrefix("PUT /traits/CUSTOM_A: "))
	})
}

// testPlacementURL is a Placement override, outside any catalog.
const testPlacementURL = "http://p.example:8778"

// fakeCatalogToken is a token body in the shape Keystone sends, with
// placement in two regions and one public endpoint the client must not pick.
const fakeCatalogToken = `{"token":{"catalog":[` +
	`{"type":"compute","endpoints":[{"interface":"internal","region":"RegionOne","url":"http://nova:8774"}]},` +
	`{"type":"placement","endpoints":[` +
	`{"interface":"public","region":"RegionOne","url":"https://placement.example.com"},` +
	`{"interface":"internal","region":"RegionTwo","url":"http://placement.two.svc:8778/"},` +
	`{"interface":"internal","region":"RegionOne","url":"http://placement.openstack.svc:8778"}]}]}}`

func TestNew_DecodesTheCatalog(t *testing.T) {
	g := NewGomegaWithT(t)
	c := newClient(t, computeapitest.New())
	g.Expect(c.UsePlacement("", "RegionOne")).To(Succeed())
	g.Expect(c.placementURL).To(Equal(computeapitest.PlacementURL))
}

func TestUsePlacement(t *testing.T) {
	for _, tc := range []struct {
		name, override, region, want string
	}{
		{name: "the override wins", override: testPlacementURL, region: "RegionOne", want: testPlacementURL},
		{name: "the internal endpoint of the region", region: "RegionOne", want: "http://placement.openstack.svc:8778"},
		{name: "a trailing slash is trimmed", region: "RegionTwo", want: "http://placement.two.svc:8778"},
		{name: "any region when none is named", want: "http://placement.two.svc:8778"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c := newClient(t, tokenWithBodyThen(fakeCatalogToken, status(http.StatusOK, "{}")))
			g.Expect(c.UsePlacement(tc.override, tc.region)).To(Succeed())
			g.Expect(c.placementURL).To(Equal(tc.want))
		})
	}

	t.Run("a region without placement is ErrPlacementEndpointNotFound", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := newClient(t, tokenWithBodyThen(fakeCatalogToken, status(http.StatusOK, "{}")))
		err := c.UsePlacement("", "RegionThree")
		g.Expect(errors.Is(err, ErrPlacementEndpointNotFound)).To(BeTrue(), "got %v", err)
		g.Expect(err.Error()).To(ContainSubstring("RegionThree"))
	})

	t.Run("a catalog without placement is ErrPlacementEndpointNotFound", func(t *testing.T) {
		g := NewGomegaWithT(t)
		fake := computeapitest.New()
		fake.OmitPlacementFromCatalog = true
		c := newClient(t, fake)
		g.Expect(c.UsePlacement("", "")).To(MatchError(ErrPlacementEndpointNotFound))
	})
}

// placementClient is a client pointed at testPlacementURL that answers the
// Placement requests with next.
func placementClient(t *testing.T, next http.HandlerFunc) (*Client, *stubDoer) {
	t.Helper()
	doer := tokenThen(next)
	c := newClient(t, doer)
	NewGomegaWithT(t).Expect(c.UsePlacement(testPlacementURL, "")).To(Succeed())
	return c, doer
}

func TestEnsureTrait(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int
		wantCreated bool
	}{
		{name: "201 is created", code: http.StatusCreated, wantCreated: true},
		{name: "204 is an existing trait", code: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c, doer := placementClient(t, status(tc.code, ""))
			created, err := c.EnsureTrait(context.Background(), "CUSTOM_GPU")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(created).To(Equal(tc.wantCreated))

			req := doer.requests[1]
			g.Expect(req.Method).To(Equal(http.MethodPut))
			g.Expect(req.URL.String()).To(Equal(testPlacementURL + "/traits/CUSTOM_GPU"))
			g.Expect(req.Header.Get("OpenStack-API-Version")).To(Equal("placement 1.6"))
			g.Expect(req.Header.Get("X-Auth-Token")).To(Equal("tok"))
			g.Expect(req.Header.Get("X-OpenStack-Nova-API-Version")).To(BeEmpty())
		})
	}

	t.Run("400 is an APIError", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c, _ := placementClient(t, status(http.StatusBadRequest, "not a custom trait"))
		created, err := c.EnsureTrait(context.Background(), "HW_CPU_X86_AVX2")
		g.Expect(created).To(BeFalse())
		var apiErr *APIError
		g.Expect(errors.As(err, &apiErr)).To(BeTrue(), "got %v", err)
		g.Expect(apiErr.StatusCode).To(Equal(http.StatusBadRequest))
		g.Expect(apiErr.Path).To(Equal("/traits/HW_CPU_X86_AVX2"))
	})

	t.Run("a 2xx other than 201 and 204 is an APIError", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c, _ := placementClient(t, status(http.StatusOK, ""))
		_, err := c.EnsureTrait(context.Background(), "CUSTOM_GPU")
		g.Expect(err).To(MatchError("PUT /traits/CUSTOM_GPU: HTTP 200: "))
	})

	t.Run("before UsePlacement nothing is sent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		doer := tokenThen(status(http.StatusCreated, ""))
		c := newClient(t, doer)
		_, err := c.EnsureTrait(context.Background(), "CUSTOM_GPU")
		g.Expect(err).To(MatchError(ErrPlacementEndpointNotFound))
		g.Expect(c.DeleteTrait(context.Background(), "CUSTOM_GPU")).To(MatchError(ErrPlacementEndpointNotFound))
		g.Expect(doer.requests).To(HaveLen(1), "the token request alone")
	})
}

func TestDeleteTrait(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		wantErr error
	}{
		{name: "204 is success", code: http.StatusNoContent},
		{name: "404 is success", code: http.StatusNotFound},
		{name: "409 is ErrTraitInUse", code: http.StatusConflict, wantErr: ErrTraitInUse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c, doer := placementClient(t, status(tc.code, ""))
			err := c.DeleteTrait(context.Background(), "CUSTOM_GPU")
			if tc.wantErr == nil {
				g.Expect(err).NotTo(HaveOccurred())
			} else {
				g.Expect(errors.Is(err, tc.wantErr)).To(BeTrue(), "got %v", err)
			}
			g.Expect(doer.requests[1].Method).To(Equal(http.MethodDelete))
			g.Expect(doer.requests[1].Header.Get("OpenStack-API-Version")).To(Equal("placement 1.6"))
		})
	}

	t.Run("500 is an APIError", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c, _ := placementClient(t, status(http.StatusInternalServerError, "boom"))
		err := c.DeleteTrait(context.Background(), "CUSTOM_GPU")
		var apiErr *APIError
		g.Expect(errors.As(err, &apiErr)).To(BeTrue(), "got %v", err)
		g.Expect(apiErr.StatusCode).To(Equal(http.StatusInternalServerError))
		g.Expect(errors.Is(err, ErrTraitInUse)).To(BeFalse())
	})
}

func TestAPIError_TruncatesTheBody(t *testing.T) {
	g := NewGomegaWithT(t)
	err := &APIError{Method: "GET", Path: "/x", StatusCode: 500, Body: strings.Repeat("a", 600)}
	g.Expect(err.Error()).To(Equal("GET /x: HTTP 500: " + strings.Repeat("a", 512)))

	c := newClient(t, tokenThen(status(http.StatusInternalServerError, strings.Repeat("b", 4096))))
	_, listErr := c.ListAggregates(context.Background())
	var apiErr *APIError
	g.Expect(errors.As(listErr, &apiErr)).To(BeTrue())
	g.Expect(apiErr.Body).To(HaveLen(512), "no more than the message carries is read")
}

// TestAgainstTheFake round-trips the client through computeapitest.Fake, which
// pins the fake to the wire shapes the client sends and reads.
func TestAgainstTheFake(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	fake := computeapitest.New()
	id := fake.AddService("node-1", "enabled", "up")
	fake.SetServers("node-1", 1)

	c, err := New(ctx, fake, testKeystoneURL, testComputeURL, testCreds)
	g.Expect(err).NotTo(HaveOccurred())

	services, err := c.ListComputeServices(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(services).To(Equal([]Service{{ID: id, Host: "node-1", Status: "enabled", State: "up"}}))

	g.Expect(c.DisableService(ctx, id, "leaving")).To(Succeed())
	services, _ = c.ListComputeServices(ctx)
	g.Expect(services[0].Status).To(Equal("disabled"))
	g.Expect(services[0].DisabledReason).To(Equal("leaving"))

	agg, err := c.CreateAggregate(ctx, "az1", "az1")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(c.SetAggregateMetadata(ctx, agg.ID, map[string]string{"c5c3.io:nova": "ns/nova"})).To(Succeed())
	_, err = c.CreateAggregate(ctx, "az1", "az1")
	g.Expect(errors.Is(err, ErrAggregateExists)).To(BeTrue())

	n, err := c.CountServersOnHost(ctx, "node-1")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(n).To(Equal(1))
	g.Expect(errors.Is(c.DeleteService(ctx, id), ErrServiceHasInstances)).To(BeTrue())

	fake.SetServers("node-1", 0)
	g.Expect(c.DeleteService(ctx, id)).To(Succeed())
	g.Expect(c.DeleteService(ctx, id)).To(Succeed(), "a second delete is a 404, which is success")

	aggregates, err := c.ListAggregates(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(aggregates).To(HaveLen(1))
	g.Expect(aggregates[0].Metadata).To(HaveKeyWithValue("c5c3.io:nova", "ns/nova"))
	g.Expect(c.DeleteAggregate(ctx, agg.ID)).To(Succeed())
	g.Expect(c.DeleteAggregate(ctx, agg.ID)).To(Succeed())

	g.Expect(c.UsePlacement("", "RegionOne")).To(Succeed())
	created, err := c.EnsureTrait(ctx, "CUSTOM_GPU")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created).To(BeTrue())
	created, err = c.EnsureTrait(ctx, "CUSTOM_GPU")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created).To(BeFalse(), "a second create is a 204")
	g.Expect(fake.Traits()).To(Equal([]string{"CUSTOM_GPU"}))
	fake.SetTraitInUse("CUSTOM_GPU", true)
	g.Expect(errors.Is(c.DeleteTrait(ctx, "CUSTOM_GPU"), ErrTraitInUse)).To(BeTrue())
	fake.SetTraitInUse("CUSTOM_GPU", false)
	g.Expect(c.DeleteTrait(ctx, "CUSTOM_GPU")).To(Succeed())
	g.Expect(c.DeleteTrait(ctx, "CUSTOM_GPU")).To(Succeed(), "a second delete is a 404, which is success")
	g.Expect(fake.Traits()).To(BeEmpty())
	g.Expect(fake.CallsTo(http.MethodPut, "/traits/")).To(HaveLen(2))
	_, err = c.send(ctx, computeapitest.PlacementURL, http.MethodPut, "/traits/CUSTOM_OLD", nil, nil, nil)
	g.Expect(statusOf(err)).To(Equal(http.StatusNotFound), "below 1.6 Placement serves no /traits")

	fake.RejectAuth = true
	_, err = New(ctx, fake, testKeystoneURL, testComputeURL, testCreds)
	var apiErr *APIError
	g.Expect(errors.As(err, &apiErr)).To(BeTrue())
	g.Expect(apiErr.StatusCode).To(Equal(http.StatusUnauthorized))
}
