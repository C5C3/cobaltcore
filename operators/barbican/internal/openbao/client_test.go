// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package openbao

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/onsi/gomega"
)

// response is one canned answer the fake server returns for one path.
type response struct {
	status int
	body   string
}

// fakeServer is a TLS OpenBao stand-in. It answers a fixed route table and
// records every request it receives, so a test can assert both what the client
// sent and that it sent nothing at all.
type fakeServer struct {
	*httptest.Server

	mu    sync.Mutex
	calls []call
}

// call is one request the fake server saw, reduced to what the tests assert on.
type call struct {
	methodAndPath string
	token         string
	namespace     string
}

// newFakeServer starts a TLS server answering routes, keyed by request path.
// A path outside the table answers 404, which is what OpenBao returns for an
// unmounted path, so an unexpected request fails the test it belongs to instead
// of hanging it.
func newFakeServer(t *testing.T, routes map[string]response) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, call{
			methodAndPath: r.Method + " " + r.URL.Path,
			token:         r.Header.Get("X-Vault-Token"),
			namespace:     r.Header.Get("X-Vault-Namespace"),
		})
		f.mu.Unlock()

		res, ok := routes[r.URL.Path]
		if !ok {
			res = response{status: http.StatusNotFound, body: `{"errors":["no handler for this path"]}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.status)
		_, _ = io.WriteString(w, res.body)
	}))
	t.Cleanup(f.Close)
	return f
}

// recorded returns the requests the server saw, oldest first.
func (f *fakeServer) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// methodsAndPaths reduces the recorded requests to "METHOD /path" strings.
func (f *fakeServer) methodsAndPaths() []string {
	var out []string
	for _, c := range f.recorded() {
		out = append(out, c.methodAndPath)
	}
	return out
}

// caPEM returns the fake server's self-signed certificate PEM, the bundle a
// client has to carry in Config.CACertPEM to trust it.
func (f *fakeServer) caPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.Certificate().Raw})
}

// newTestClient builds a Client trusting f through Config.CACertPEM.
func newTestClient(t *testing.T, f *fakeServer) Client {
	t.Helper()
	g := gomega.NewWithT(t)
	c, err := New(Config{URL: f.URL, CACertPEM: f.caPEM(), Timeout: 5 * time.Second})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return c
}

// secretIDResult bundles the two values GenerateSecretID returns so the table
// below can compare them as one.
type secretIDResult struct {
	secretID string
	ttl      int64
}

// TestClientCallsReachTheExpectedPaths walks every Client method against a fake
// serving the JSON shapes the real API returns. Because every case trusts the
// fake through Config.CACertPEM, it is also the CA success path: a client
// carrying the server's certificate completes the handshake.
func TestClientCallsReachTheExpectedPaths(t *testing.T) {
	const roleName = "barbican-store"

	tests := []struct {
		name      string
		routes    map[string]response
		invoke    func(ctx context.Context, c Client) (any, error)
		want      any
		wantCalls []string
	}{
		{
			name: "kubernetes login",
			routes: map[string]response{
				"/v1/auth/kubernetes/login": {http.StatusOK, `{"auth":{"client_token":"kubernetes.token"}}`},
			},
			invoke: func(ctx context.Context, c Client) (any, error) {
				return nil, c.LoginKubernetes(ctx, "projected-jwt", "barbican-provisioner")
			},
			wantCalls: []string{"PUT /v1/auth/kubernetes/login"},
		},
		{
			name: "approle login",
			routes: map[string]response{
				"/v1/auth/approle/login": {http.StatusOK, `{"auth":{"client_token":"approle.token"}}`},
			},
			invoke: func(ctx context.Context, c Client) (any, error) {
				return nil, c.LoginAppRole(ctx, "role-id", "secret-id")
			},
			wantCalls: []string{"PUT /v1/auth/approle/login"},
		},
		{
			name: "read role id",
			routes: map[string]response{
				"/v1/auth/approle/role/barbican-store/role-id": {
					http.StatusOK, `{"data":{"role_id":"6a1b3f2c-role"}}`,
				},
			},
			invoke: func(ctx context.Context, c Client) (any, error) {
				return c.ReadRoleID(ctx, roleName)
			},
			want:      "6a1b3f2c-role",
			wantCalls: []string{"GET /v1/auth/approle/role/barbican-store/role-id"},
		},
		{
			name: "generate secret id",
			routes: map[string]response{
				"/v1/auth/approle/role/barbican-store/secret-id": {
					http.StatusOK, `{"data":{"secret_id":"9c1e-secret","secret_id_ttl":600}}`,
				},
			},
			invoke: func(ctx context.Context, c Client) (any, error) {
				secretID, ttl, err := c.GenerateSecretID(ctx, roleName)
				return secretIDResult{secretID: secretID, ttl: ttl}, err
			},
			want:      secretIDResult{secretID: "9c1e-secret", ttl: 600},
			wantCalls: []string{"PUT /v1/auth/approle/role/barbican-store/secret-id"},
		},
		{
			name: "capabilities of the current token",
			routes: map[string]response{
				"/v1/sys/capabilities-self": {
					http.StatusOK, `{"data":{"capabilities":["create","read","update"]}}`,
				},
			},
			invoke: func(ctx context.Context, c Client) (any, error) {
				return c.CapabilitiesSelf(ctx, "barbican/data/stores")
			},
			want:      []string{"create", "read", "update"},
			wantCalls: []string{"POST /v1/sys/capabilities-self"},
		},
		{
			name: "mount exists",
			routes: map[string]response{
				"/v1/sys/mounts/barbican": {
					http.StatusOK, `{"data":{"type":"kv","options":{"version":"2"}}}`,
				},
			},
			invoke: func(ctx context.Context, c Client) (any, error) {
				return c.MountExists(ctx, "barbican")
			},
			want:      true,
			wantCalls: []string{"GET /v1/sys/mounts/barbican"},
		},
		{
			name: "revoke the session token",
			routes: map[string]response{
				"/v1/auth/approle/login":     {http.StatusOK, `{"auth":{"client_token":"approle.token"}}`},
				"/v1/auth/token/revoke-self": {http.StatusNoContent, ""},
			},
			invoke: func(ctx context.Context, c Client) (any, error) {
				if err := c.LoginAppRole(ctx, "role-id", "secret-id"); err != nil {
					return nil, err
				}
				return nil, c.RevokeSelfToken(ctx)
			},
			wantCalls: []string{"PUT /v1/auth/approle/login", "PUT /v1/auth/token/revoke-self"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			f := newFakeServer(t, tc.routes)
			c := newTestClient(t, f)

			got, err := tc.invoke(t.Context(), c)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			if tc.want == nil {
				g.Expect(got).To(gomega.BeNil())
			} else {
				g.Expect(got).To(gomega.Equal(tc.want))
			}
			g.Expect(f.methodsAndPaths()).To(gomega.Equal(tc.wantCalls))
		})
	}
}

// TestLoginRetainsTheSessionToken pins the contract both login methods promise:
// the token from the login response travels on every later request. Without it
// the controller would authenticate and then act unauthenticated.
func TestLoginRetainsTheSessionToken(t *testing.T) {
	tests := []struct {
		name      string
		loginPath string
		token     string
		login     func(ctx context.Context, c Client) error
	}{
		{
			name:      "kubernetes",
			loginPath: "/v1/auth/kubernetes/login",
			token:     "kubernetes.token",
			login: func(ctx context.Context, c Client) error {
				return c.LoginKubernetes(ctx, "projected-jwt", "barbican-provisioner")
			},
		},
		{
			name:      "approle",
			loginPath: "/v1/auth/approle/login",
			token:     "approle.token",
			login: func(ctx context.Context, c Client) error {
				return c.LoginAppRole(ctx, "role-id", "secret-id")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			f := newFakeServer(t, map[string]response{
				tc.loginPath:                {http.StatusOK, fmt.Sprintf(`{"auth":{"client_token":%q}}`, tc.token)},
				"/v1/sys/capabilities-self": {http.StatusOK, `{"data":{"capabilities":["read"]}}`},
			})
			c := newTestClient(t, f)

			g.Expect(tc.login(t.Context(), c)).To(gomega.Succeed())
			_, err := c.CapabilitiesSelf(t.Context(), "barbican/data/stores")
			g.Expect(err).NotTo(gomega.HaveOccurred())

			recorded := f.recorded()
			g.Expect(recorded).To(gomega.HaveLen(2))
			g.Expect(recorded[0].token).To(gomega.BeEmpty(), "the login itself must not carry a token")
			g.Expect(recorded[1].token).To(gomega.Equal(tc.token))
		})
	}
}

// Config.Namespace must reach the wire. A brownfield store may point at a
// namespaced Vault, where the AppRole lives in that namespace: without the
// header the operator's own login runs against the root namespace and is
// rejected, which surfaces as InvalidCredentials against credentials that are
// correct — while the rendered configuration points the pods at the namespace
// the CR asked for.
func TestConfigNamespaceIsSentOnEveryRequest(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newFakeServer(t, map[string]response{
		"/v1/auth/approle/login":    {http.StatusOK, `{"auth":{"client_token":"approle.token"}}`},
		"/v1/sys/capabilities-self": {http.StatusOK, `{"data":{"capabilities":["read"]}}`},
	})

	c, err := New(Config{URL: f.URL, CACertPEM: f.caPEM(), Namespace: "tenant-a", Timeout: 5 * time.Second})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(c.LoginAppRole(t.Context(), "role-id", "secret-id")).To(gomega.Succeed())
	_, err = c.CapabilitiesSelf(t.Context(), "barbican/data/probe")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	for _, recorded := range f.recorded() {
		g.Expect(recorded.namespace).To(gomega.Equal("tenant-a"), "%s", recorded.methodAndPath)
	}
}

// An empty Config.Namespace clears the header, which is the root namespace every
// managed store runs against.
func TestEmptyConfigNamespaceSendsNoHeader(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newFakeServer(t, map[string]response{
		"/v1/auth/approle/login": {http.StatusOK, `{"auth":{"client_token":"approle.token"}}`},
	})
	c := newTestClient(t, f)

	g.Expect(c.LoginAppRole(t.Context(), "role-id", "secret-id")).To(gomega.Succeed())
	g.Expect(f.recorded()).To(gomega.HaveLen(1))
	g.Expect(f.recorded()[0].namespace).To(gomega.BeEmpty())
}

// TestLoginWithoutAClientTokenFails covers the malformed-response path: a 200
// that carries no token must not leave the client looking logged in.
func TestLoginWithoutAClientTokenFails(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newFakeServer(t, map[string]response{
		"/v1/auth/kubernetes/login": {http.StatusOK, `{"auth":{"client_token":""}}`},
	})
	c := newTestClient(t, f)

	err := c.LoginKubernetes(t.Context(), "projected-jwt", "barbican-provisioner")
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("no client token")))
	g.Expect(c.RevokeSelfToken(t.Context())).To(gomega.Succeed())
	g.Expect(f.methodsAndPaths()).To(gomega.Equal([]string{"PUT /v1/auth/kubernetes/login"}),
		"a failed login must leave no token to revoke")
}

// TestPermissionDeniedIsClassified proves the helper the controller branches on:
// a 403 is a policy gap, and it must not read as a missing object.
func TestPermissionDeniedIsClassified(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newFakeServer(t, map[string]response{
		"/v1/auth/approle/role/barbican-store/role-id": {
			http.StatusForbidden, `{"errors":["1 error occurred:\n\t* permission denied\n\n"]}`,
		},
	})
	c := newTestClient(t, f)

	_, err := c.ReadRoleID(t.Context(), "barbican-store")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(IsPermissionDenied(err)).To(gomega.BeTrue())
	g.Expect(IsNotFound(err)).To(gomega.BeFalse())
}

// TestMountExistsOnMissingMount pins the one method that swallows a 404: an
// absent mount is the normal pre-provisioning state, not a failure.
func TestMountExistsOnMissingMount(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newFakeServer(t, map[string]response{
		"/v1/sys/mounts/barbican": {http.StatusNotFound, `{"errors":[]}`},
	})
	c := newTestClient(t, f)

	exists, err := c.MountExists(t.Context(), "barbican")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(exists).To(gomega.BeFalse())
}

// TestGenerateSecretIDWithoutTTL covers an AppRole configured without a secret-ID
// TTL: the response omits secret_id_ttl and the caller must see 0, its
// "does not expire" value, rather than a decode failure.
func TestGenerateSecretIDWithoutTTL(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newFakeServer(t, map[string]response{
		"/v1/auth/approle/role/barbican-store/secret-id": {
			http.StatusOK, `{"data":{"secret_id":"9c1e-secret"}}`,
		},
	})
	c := newTestClient(t, f)

	secretID, ttl, err := c.GenerateSecretID(t.Context(), "barbican-store")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(secretID).To(gomega.Equal("9c1e-secret"))
	g.Expect(ttl).To(gomega.BeZero())
}

// TestRevokeSelfTokenWithoutLoginIsANoOp guards the contract that every
// controller session may end with RevokeSelfToken, error paths included: a
// session that never got a token must not fire a request, let alone fail.
func TestRevokeSelfTokenWithoutLoginIsANoOp(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newFakeServer(t, map[string]response{})
	c := newTestClient(t, f)

	g.Expect(c.RevokeSelfToken(t.Context())).To(gomega.Succeed())
	g.Expect(f.recorded()).To(gomega.BeEmpty())
}

// TestUnreachableServerIsNotClassified covers a closed server: the client must
// surface the transport error, and neither classifier may claim it, so the
// controller retries instead of reporting a policy or provisioning problem.
func TestUnreachableServerIsNotClassified(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newFakeServer(t, map[string]response{})
	c := newTestClient(t, f)
	f.Close()

	_, err := c.MountExists(t.Context(), "barbican")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(IsPermissionDenied(err)).To(gomega.BeFalse())
	g.Expect(IsNotFound(err)).To(gomega.BeFalse())
}

// TestUntrustedServerCertificateIsRejected is the negative half of the CA path:
// without Config.CACertPEM the fake's self-signed certificate fails
// verification, and the request never reaches the handler.
func TestUntrustedServerCertificateIsRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newFakeServer(t, map[string]response{
		"/v1/sys/mounts/barbican": {http.StatusOK, `{"data":{"type":"kv"}}`},
	})

	untrusting, err := New(Config{URL: f.URL, Timeout: 5 * time.Second})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = untrusting.MountExists(t.Context(), "barbican")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(f.recorded()).To(gomega.BeEmpty(), "the handshake must fail before the request is served")

	trusting := newTestClient(t, f)
	exists, err := trusting.MountExists(t.Context(), "barbican")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(exists).To(gomega.BeTrue())
}

// TestNewRejectsAnUnparseableURL covers the constructor's error path.
func TestNewRejectsAnUnparseableURL(t *testing.T) {
	g := gomega.NewWithT(t)

	_, err := New(Config{URL: "://openbao", Timeout: time.Second})
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("creating the OpenBao client")))
}
