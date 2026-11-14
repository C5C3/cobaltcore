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
	"time"

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

// getDomains sends GET /v3/domains with the fake's token under ctx, outside
// the identity client, so a test sees the transport's own error.
func getDomains(ctx context.Context, srv *fake.Server) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.Endpoint()+"/domains", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Auth-Token", fake.Token)
	return http.DefaultClient.Do(req)
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
		hang    bool
		wantMsg string
	}{
		{name: "missing", status: 0, wantMsg: "fake: fault for GET /v3/domains has no valid Status (0)"},
		{name: "informational", status: http.StatusProcessing, wantMsg: "fake: fault for GET /v3/domains has no valid Status (102)"},
		{name: "informational hang", status: http.StatusProcessing, hang: true, wantMsg: "fake: fault for GET /v3/domains has no valid Status (102)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c, srv := newClient(t)
			srv.SeedDomain("corp", "", true)
			srv.InjectFault(fake.Fault{Method: http.MethodGet, Path: "/v3/domains", Status: tc.status, Hang: tc.hang})

			_, err := c.GetDomainByName(context.Background(), "corp")
			g.Expect(err).To(MatchError(ContainSubstring("unexpected HTTP 500")))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantMsg))
			g.Expect(errors.Is(err, io.EOF)).To(BeFalse(), "got %v", err)
		})
	}
}

// A Hang fault without a Status holds the request without a response byte
// until the client gives up; the request is recorded once, and the fake
// answers again once the fault is cleared.
func TestFault_HangHoldsTheRequestUntilTheClientLeaves(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := fake.NewServer(adminPassword)
	t.Cleanup(srv.Close)
	srv.InjectFault(fake.Fault{Method: http.MethodGet, Path: "/v3/domains", Hang: true})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	resp, err := getDomains(ctx, srv)
	if resp != nil {
		_ = resp.Body.Close()
	}
	g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
	g.Expect(srv.Requests()).To(Equal([]string{"GET /v3/domains"}))

	srv.ClearFaults()
	resp, err = getDomains(context.Background(), srv)
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
}

// A Hang fault with a Status sends the status line and the headers, and the
// body never ends: reading it fails once the client's context expires.
func TestFault_HangWithStatusSendsHeadersAndNoBodyEnd(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := fake.NewServer(adminPassword)
	t.Cleanup(srv.Close)
	srv.InjectFault(fake.Fault{Method: http.MethodGet, Path: "/v3/domains", Status: http.StatusOK, Body: `{"domains":`, Hang: true})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	resp, err := getDomains(ctx, srv)
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	g.Expect(resp.StatusCode).To(Equal(http.StatusOK))

	body, err := io.ReadAll(resp.Body)
	g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
	g.Expect(string(body)).To(Equal(`{"domains":`), "the flushed part of the body arrives")
}

// Close releases a request a Hang fault holds, even one sent without a
// deadline, and the request fails instead of finishing as an implicit 200.
func TestFault_HangIsReleasedByClose(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := fake.NewServer(adminPassword)
	t.Cleanup(srv.Close)
	srv.InjectFault(fake.Fault{Method: http.MethodGet, Path: "/v3/domains", Hang: true})

	result := make(chan error, 1)
	go func() {
		resp, err := getDomains(context.Background(), srv)
		if resp != nil {
			_ = resp.Body.Close()
		}
		result <- err
	}()
	g.Eventually(srv.Requests).Should(HaveLen(1), "the request reaches the fake")

	closed := make(chan struct{})
	go func() {
		srv.Close()
		close(closed)
	}()
	g.Eventually(closed).WithTimeout(5 * time.Second).Should(BeClosed())
	g.Eventually(result).WithTimeout(5 * time.Second).Should(Receive(HaveOccurred()))
}

// Hang and CloseConnection contradict each other; the fault answers 500 with
// a fake: error that names the mistake.
func TestFault_HangWithCloseConnectionAnswers500(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := fake.NewServer(adminPassword)
	t.Cleanup(srv.Close)
	srv.InjectFault(fake.Fault{Method: http.MethodGet, Path: "/v3/domains", Hang: true, CloseConnection: true})

	resp, err := getDomains(context.Background(), srv)
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	g.Expect(resp.StatusCode).To(Equal(http.StatusInternalServerError))
	body, err := io.ReadAll(resp.Body)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(body)).To(HavePrefix("fake:"))
	g.Expect(string(body)).To(ContainSubstring("sets both Hang and CloseConnection"))
}
