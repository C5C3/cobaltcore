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
	return &stubDoer{handler: func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/tokens") {
			w.Header().Set("X-Subject-Token", "tok")
			w.WriteHeader(http.StatusCreated)
			return
		}
		compute(w, r)
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
	doer := tokenThen(status(http.StatusOK, `{"services":[],"aggregates":[],"servers":[]}`))
	c := newClient(t, doer)
	ctx := context.Background()

	_, _ = c.ListComputeServices(ctx)
	_, _ = c.ListAggregates(ctx)
	_, _ = c.CountServersOnHost(ctx, "node-1")
	_ = c.DisableService(ctx, "id", "reason")
	_ = c.DeleteService(ctx, "id")
	_, _ = c.CreateAggregate(ctx, "az1", "az1")
	_ = c.SetAggregateMetadata(ctx, 1, map[string]string{"k": "v"})
	_ = c.DeleteAggregate(ctx, 1)

	compute := doer.requests[1:]
	g.Expect(compute).To(HaveLen(8))
	for _, req := range compute {
		// The service list alone asks for 2.69, which reports a cell that did
		// not answer instead of leaving its services out.
		version := "2.53"
		if req.Method == http.MethodGet && req.URL.Path == "/v2.1/os-services" {
			version = "2.69"
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

	t.Run("compute request", func(t *testing.T) {
		g := NewGomegaWithT(t)
		authenticated := false
		doer := doerFunc(func(req *http.Request) (*http.Response, error) {
			if !authenticated {
				authenticated = true
				rec := httptest.NewRecorder()
				rec.Header().Set("X-Subject-Token", "tok")
				rec.WriteHeader(http.StatusCreated)
				return rec.Result(), nil
			}
			return blocking(req)
		})
		c := newClient(t, doer)
		_, err := c.ListComputeServices(context.Background())
		g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
		g.Expect(err.Error()).To(HavePrefix("GET /v2.1/os-services?binary=nova-compute: "))
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

	fake.RejectAuth = true
	_, err = New(ctx, fake, testKeystoneURL, testComputeURL, testCreds)
	var apiErr *APIError
	g.Expect(errors.As(err, &apiErr)).To(BeTrue())
	g.Expect(apiErr.StatusCode).To(Equal(http.StatusUnauthorized))
}
