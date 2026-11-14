// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	"github.com/c5c3/cobaltcore/operators/keystone/internal/identity"
	"github.com/c5c3/cobaltcore/operators/keystone/internal/identity/fake"
)

const adminPassword = "s3cret"

func newClient(t *testing.T) (identity.Client, *fake.Server) {
	t.Helper()
	srv := fake.NewServer(adminPassword)
	t.Cleanup(srv.Close)
	c := identity.NewHTTPClient(srv.Endpoint(), identity.Credentials{
		Username: "admin",
		Password: adminPassword,
	}, nil)
	return c, srv
}

// newClientWithTimeout is newClient with a shorter per-request timeout.
func newClientWithTimeout(t *testing.T, timeout time.Duration) (identity.Client, *fake.Server) {
	t.Helper()
	srv := fake.NewServer(adminPassword)
	t.Cleanup(srv.Close)
	c := identity.NewHTTPClientWithRequestTimeout(srv.Endpoint(), identity.Credentials{
		Username: "admin",
		Password: adminPassword,
	}, nil, timeout)
	return c, srv
}

func TestCreateAndGetDomain_RoundTrip(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	ctx := context.Background()

	created, err := c.CreateDomain(ctx, identity.Domain{Name: "corp", Description: "corporate directory"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.ID).NotTo(BeEmpty())
	g.Expect(created.Name).To(Equal("corp"))

	got, err := c.GetDomainByName(ctx, "corp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.ID).To(Equal(created.ID))
	g.Expect(got.Description).To(Equal("corporate directory"))

	// The auth request precedes every API call (per-call authentication).
	reqs := srv.Requests()
	g.Expect(reqs[0]).To(Equal("POST /v3/auth/tokens"))
}

func TestGetDomainByName_NotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	c, _ := newClient(t)

	_, err := c.GetDomainByName(context.Background(), "missing")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())
}

func TestCreateDomain_ConflictOnDuplicateName(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	srv.SeedDomain("corp", "", true)

	_, err := c.CreateDomain(context.Background(), identity.Domain{Name: "corp"})
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, identity.ErrConflict)).To(BeTrue())
}

func TestAuthenticate_BadPasswordIsUnauthorized(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := fake.NewServer(adminPassword)
	t.Cleanup(srv.Close)
	c := identity.NewHTTPClient(srv.Endpoint(), identity.Credentials{
		Username: "admin",
		Password: "wrong",
	}, nil)

	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, identity.ErrUnauthorized)).To(BeTrue())
}

// A non-keystone responder (LB default backend, SPA fallback, misrouted
// Service) answers with a status keystone never uses for auth; the error must
// carry a bounded body snippet so the responder is identifiable from the
// DomainReady condition alone.
func TestAuthenticate_UnexpectedStatusIncludesBoundedBodySnippet(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no route: auth-tokens " + strings.Repeat("x", 4096)))
	}))
	t.Cleanup(srv.Close)
	c := identity.NewHTTPClient(srv.URL+"/v3", identity.Credentials{
		Username: "admin",
		Password: adminPassword,
	}, nil)

	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("unexpected HTTP 404"))
	g.Expect(err.Error()).To(ContainSubstring("no route: auth-tokens"))
	// The 4KB body must be truncated to the snippet limit, not embedded whole.
	g.Expect(len(err.Error())).To(BeNumerically("<", 512))
}

// matchesSentinel reports whether err matches one of the identity sentinel
// errors callers branch on.
func matchesSentinel(err error) bool {
	for _, sentinel := range identity.Sentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// A 5xx from an API call is no sentinel: the error carries the status and a
// snippet of the body, and the faulted request is recorded once.
func TestGetDomainByName_ServerErrorIsNotASentinel(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	srv.InjectFault(fake.Fault{
		Method: http.MethodGet,
		Path:   "/v3/domains",
		Status: http.StatusServiceUnavailable,
		Body:   `{"error":{"code":503}}`,
	})

	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(err).To(HaveOccurred())
	g.Expect(matchesSentinel(err)).To(BeFalse(), "a 5xx must not read as a recoverable class")
	g.Expect(err.Error()).To(ContainSubstring("unexpected HTTP 503"))
	// The snippet is %q-quoted, so the body's quotes arrive escaped.
	g.Expect(err.Error()).To(ContainSubstring(`(body: "{\"error\":{\"code\":503}}")`))
	g.Expect(srv.Requests()).To(Equal([]string{"POST /v3/auth/tokens", "GET /v3/domains"}))
}

// A 5xx at the token endpoint fails the call before any API request is sent.
func TestAuthenticate_ServerErrorIsNotASentinel(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	srv.InjectFault(fake.Fault{Method: http.MethodPost, Path: "/v3/auth/tokens", Status: http.StatusServiceUnavailable})

	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(err).To(HaveOccurred())
	g.Expect(matchesSentinel(err)).To(BeFalse())
	g.Expect(err.Error()).To(ContainSubstring("authenticating against"))
	g.Expect(err.Error()).To(ContainSubstring("unexpected HTTP 503"))
	g.Expect(srv.Requests()).To(HaveLen(1))
}

// A connection the server drops on the token request surfaces as io.EOF. The
// token POST is not replayable, so net/http reports the failure instead of
// retrying it on a new connection.
func TestAuthenticate_ClosedConnectionSurfacesEOF(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	srv.SeedDomain("corp", "", true)
	ctx := context.Background()
	srv.InjectFault(fake.Fault{Method: http.MethodPost, Path: "/v3/auth/tokens", CloseConnection: true, Times: 1})

	_, err := c.GetDomainByName(ctx, "corp")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, io.EOF)).To(BeTrue(), "got %v", err)
	g.Expect(err.Error()).To(ContainSubstring("authenticating against"))
	g.Expect(matchesSentinel(err)).To(BeFalse())

	d, err := c.GetDomainByName(ctx, "corp")
	g.Expect(err).NotTo(HaveOccurred(), "the spent fault no longer fires")
	g.Expect(d.Name).To(Equal("corp"))
	g.Expect(srv.Requests()).To(Equal([]string{"POST /v3/auth/tokens", "POST /v3/auth/tokens", "GET /v3/domains"}),
		"the dropped token request reached the server once")
}

// A connection the server drops on an authenticated API request surfaces as
// io.EOF behind the method and path. The PATCH is not replayable, so net/http
// reports the failure instead of retrying it.
func TestUpdateDomain_ClosedConnectionSurfacesEOF(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	id := srv.SeedDomain("corp", "", true)
	srv.InjectFault(fake.Fault{Method: http.MethodPatch, Path: "/v3/domains/" + id, CloseConnection: true, Times: 1})

	err := c.UpdateDomain(context.Background(), id, ptr.To(false), nil)
	g.Expect(errors.Is(err, io.EOF)).To(BeTrue(), "got %v", err)
	g.Expect(err.Error()).To(HavePrefix("PATCH /domains/" + id))
	g.Expect(err.Error()).NotTo(ContainSubstring("authenticating against"))
	g.Expect(matchesSentinel(err)).To(BeFalse())
	g.Expect(srv.GetDomain(id).Enabled).To(BeTrue(), "the dropped PATCH wrote nothing")
}

func TestUpdateDomain_PatchesEnabledAndDescription(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	id := srv.SeedDomain("corp", "old", true)

	g.Expect(c.UpdateDomain(context.Background(), id, ptr.To(false), ptr.To("new"))).To(Succeed())

	d := srv.GetDomain(id)
	g.Expect(d.Enabled).To(BeFalse())
	g.Expect(d.Description).To(Equal("new"))

	// Nil fields must leave state untouched.
	g.Expect(c.UpdateDomain(context.Background(), id, nil, ptr.To("newer"))).To(Succeed())
	d = srv.GetDomain(id)
	g.Expect(d.Enabled).To(BeFalse(), "nil enabled must not re-enable the domain")
	g.Expect(d.Description).To(Equal("newer"))
}

func TestDeleteDomain_ForbiddenWhileEnabled(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	id := srv.SeedDomain("corp", "", true)
	ctx := context.Background()

	// Enabled domain: keystone (and the fake) forbid the delete.
	err := c.DeleteDomain(ctx, id)
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, identity.ErrForbidden)).To(BeTrue())

	// Disable, then delete succeeds.
	g.Expect(c.UpdateDomain(ctx, id, ptr.To(false), nil)).To(Succeed())
	g.Expect(c.DeleteDomain(ctx, id)).To(Succeed())
	g.Expect(srv.GetDomain(id)).To(BeNil())

	// Deleting again is a NotFound.
	err = c.DeleteDomain(ctx, id)
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())
}

// A pure lookup sequence must never issue a mutating call — the request-log
// contract the adopt path relies on.
func TestGetDomainByName_IssuesNoMutatingRequests(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	srv.SeedDomain("corp", "", true)

	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(srv.MutatingRequests()).To(BeEmpty())
}

// A token request that gets no answer ends at the request timeout with an
// error that names the timeout, and no API request follows it.
func TestAuthenticate_UnansweredTokenRequestTimesOut(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClientWithTimeout(t, 100*time.Millisecond)
	srv.SeedDomain("corp", "", true)
	srv.InjectFault(fake.Fault{Method: http.MethodPost, Path: "/v3/auth/tokens", Hang: true})

	start := time.Now()
	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(time.Since(start)).To(BeNumerically("<", 5*time.Second))
	g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
	g.Expect(err.Error()).To(ContainSubstring("authenticating against"))
	g.Expect(err.Error()).To(ContainSubstring("request did not complete within 100ms"))
	g.Expect(matchesSentinel(err)).To(BeFalse())
	g.Expect(srv.Requests()).To(Equal([]string{"POST /v3/auth/tokens"}))
}

// A response whose body never ends fails at the request timeout while the
// body is read, behind the method and path.
func TestGetDomainByName_UnfinishedResponseBodyTimesOut(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClientWithTimeout(t, 100*time.Millisecond)
	srv.InjectFault(fake.Fault{Method: http.MethodGet, Path: "/v3/domains", Status: http.StatusOK, Hang: true})

	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
	g.Expect(err.Error()).To(HavePrefix("reading GET /domains?name=corp response: request did not complete within 100ms"))
	g.Expect(matchesSentinel(err)).To(BeFalse())
}

// A caller that cancelled its context keeps the cancellation error, without
// the timeout's name, and no request leaves the client.
func TestRequestTimeout_CancelledCallerIsNotATimeout(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClientWithTimeout(t, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.GetDomainByName(ctx, "corp")
	g.Expect(errors.Is(err, context.Canceled)).To(BeTrue(), "got %v", err)
	g.Expect(err.Error()).NotTo(ContainSubstring("request did not complete within"))
	g.Expect(srv.Requests()).To(BeEmpty())
}

// A caller whose own deadline fires before the request timeout keeps the
// deadline error without the timeout's name.
func TestRequestTimeout_CallerDeadlineIsNotATimeout(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClientWithTimeout(t, 5*time.Second)
	srv.InjectFault(fake.Fault{Method: http.MethodPost, Path: "/v3/auth/tokens", Hang: true})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := c.GetDomainByName(ctx, "corp")
	g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
	g.Expect(err.Error()).NotTo(ContainSubstring("request did not complete within"))
}

// deadlineDoer is an HTTPDoer that answers the token request with a token and
// every other request with an empty domain list. It records how far away
// each request's deadline was when the request arrived, and whether the token
// request's context was done when the API request arrived.
type deadlineDoer struct {
	// unendingTokenBody gives the token response a body that ends only when
	// the token request's context is done.
	unendingTokenBody     bool
	deadlines             []time.Duration
	tokenCtx              context.Context
	tokenCtxDoneBeforeAPI bool
}

func (d *deadlineDoer) Do(req *http.Request) (*http.Response, error) {
	left := time.Duration(-1)
	if deadline, ok := req.Context().Deadline(); ok {
		left = time.Until(deadline)
	}
	d.deadlines = append(d.deadlines, left)
	if req.URL.Path == "/v3/auth/tokens" {
		d.tokenCtx = req.Context()
		var body io.Reader = strings.NewReader("")
		if d.unendingTokenBody {
			body = ctxReader{ctx: req.Context()}
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"X-Subject-Token": []string{"stub-token"}},
			Body:       io.NopCloser(body),
		}, nil
	}
	d.tokenCtxDoneBeforeAPI = d.tokenCtx != nil && d.tokenCtx.Err() != nil
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"domains":[]}`)),
	}, nil
}

var stubCreds = identity.Credentials{Username: "admin", Password: adminPassword}

// The token request and the API request each carry a deadline of their own
// that the default timeout sets, and the token request's deadline is
// released before the API request is sent.
func TestRequestTimeout_EachRequestGetsItsOwnDeadline(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := &deadlineDoer{}
	c := identity.NewHTTPClient("http://keystone.test/v3", stubCreds, doer)

	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue(), "got %v", err)
	g.Expect(doer.deadlines).To(HaveLen(2))
	for i, left := range doer.deadlines {
		g.Expect(left).To(BeNumerically(">", 5*time.Second), "request %d", i+1)
		g.Expect(left).To(BeNumerically("<=", 10*time.Second), "request %d", i+1)
	}
	g.Expect(doer.tokenCtxDoneBeforeAPI).To(BeTrue(), "the token request's context is done before the API request")
}

// ctxReader blocks every Read until ctx is done and then returns ctx's error,
// as the body of a response that never ends does.
type ctxReader struct{ ctx context.Context }

func (r ctxReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// A token response whose body never ends holds the call only until the token
// request's deadline, and the API request follows it.
func TestAuthenticate_UnfinishedTokenResponseBodyEndsAtTheDeadline(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := &deadlineDoer{unendingTokenBody: true}
	c := identity.NewHTTPClientWithRequestTimeout("http://keystone.test/v3", stubCreds, doer, 100*time.Millisecond)

	start := time.Now()
	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(time.Since(start)).To(BeNumerically("<", time.Second))
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue(), "got %v", err)
	g.Expect(doer.deadlines).To(HaveLen(2), "the API request follows the token request")
}

// A request timeout of zero or less falls back to RequestTimeout.
func TestNewHTTPClient_ZeroRequestTimeoutFallsBackToDefault(t *testing.T) {
	NewGomegaWithT(t).Expect(identity.RequestTimeout).To(Equal(10 * time.Second))
	for _, timeout := range []time.Duration{0, -1} {
		t.Run(timeout.String(), func(t *testing.T) {
			g := NewGomegaWithT(t)
			doer := &deadlineDoer{}
			c := identity.NewHTTPClientWithRequestTimeout("http://keystone.test/v3", stubCreds, doer, timeout)

			_, err := c.GetDomainByName(context.Background(), "corp")
			g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue(), "got %v", err)
			g.Expect(doer.deadlines).To(HaveLen(2))
			for i, left := range doer.deadlines {
				g.Expect(left).To(BeNumerically(">", 5*time.Second), "request %d", i+1)
				g.Expect(left).To(BeNumerically("<=", 10*time.Second), "request %d", i+1)
			}
		})
	}
}

// A client with a nil doer bounds its requests through the context, not by
// setting a Timeout on the process-wide http.DefaultClient.
func TestNewHTTPClient_NilDoerLeavesDefaultClientUntouched(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClientWithTimeout(t, 100*time.Millisecond)
	srv.InjectFault(fake.Fault{Method: http.MethodPost, Path: "/v3/auth/tokens", Hang: true})

	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
	g.Expect(http.DefaultClient.Timeout).To(BeZero())
}
