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
