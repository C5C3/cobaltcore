// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package fake_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	. "github.com/onsi/gomega"

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

// TestFault_TimesAndClear pins the fault bookkeeping of the fake: a fault
// with Times fires for that many matching requests and is then spent, a fault
// without Times fires until ClearFaults, and every request is recorded once
// whether a fault or the handler answered it.
func TestFault_TimesAndClear(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	srv.SeedDomain("corp", "", true)
	ctx := context.Background()

	srv.InjectFault(fake.Fault{Method: http.MethodGet, Path: "/v3/domains", Status: http.StatusServiceUnavailable, Times: 2})
	for i := range 2 {
		_, err := c.GetDomainByName(ctx, "corp")
		g.Expect(err).To(MatchError(ContainSubstring("unexpected HTTP 503")), "call %d must hit the fault", i+1)
	}
	d, err := c.GetDomainByName(ctx, "corp")
	g.Expect(err).NotTo(HaveOccurred(), "the fault is spent after two requests")
	g.Expect(d.Name).To(Equal("corp"))

	srv.InjectFault(fake.Fault{Method: http.MethodGet, Path: "/v3/domains", Status: http.StatusServiceUnavailable})
	for i := range 2 {
		_, err := c.GetDomainByName(ctx, "corp")
		g.Expect(err).To(MatchError(ContainSubstring("unexpected HTTP 503")), "call %d must hit the unbounded fault", i+1)
	}
	srv.ClearFaults()
	d, err = c.GetDomainByName(ctx, "corp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(d.Name).To(Equal("corp"))

	var lookups int
	for _, req := range srv.Requests() {
		if req == "GET /v3/domains" {
			lookups++
		}
	}
	g.Expect(lookups).To(Equal(6), "faulted and served requests are recorded once each")
}

// A fault with a Query fires only for the lookup whose query string it names.
func TestFault_QueryMatchesRawQuery(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	srv.SeedDomain("corp", "", true)
	srv.SeedDomain("shared", "", true)
	ctx := context.Background()
	srv.InjectFault(fake.Fault{Method: http.MethodGet, Path: "/v3/domains", Query: "name=shared", Status: http.StatusServiceUnavailable})

	d, err := c.GetDomainByName(ctx, "corp")
	g.Expect(err).NotTo(HaveOccurred(), "another query string passes the fault")
	g.Expect(d.Name).To(Equal("corp"))
	_, err = c.GetDomainByName(ctx, "shared")
	g.Expect(err).To(MatchError(ContainSubstring("unexpected HTTP 503")))
}

// A fault armed without a Status, or with an informational 1xx one, answers
// 500 with a fake: error. A forgotten Status cannot pass for a CloseConnection
// fault, and a 1xx typo cannot finish the response as 200 OK.
func TestFault_InvalidStatusAnswers500(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantMsg string
	}{
		{name: "missing", status: 0, wantMsg: "fake: fault for GET /v3/domains has no valid Status (0)"},
		{name: "informational", status: http.StatusProcessing, wantMsg: "fake: fault for GET /v3/domains has no valid Status (102)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c, srv := newClient(t)
			srv.SeedDomain("corp", "", true)
			srv.InjectFault(fake.Fault{Method: http.MethodGet, Path: "/v3/domains", Status: tc.status})

			_, err := c.GetDomainByName(context.Background(), "corp")
			g.Expect(err).To(MatchError(ContainSubstring("unexpected HTTP 500")))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantMsg))
			g.Expect(errors.Is(err, io.EOF)).To(BeFalse(), "got %v", err)
		})
	}
}
