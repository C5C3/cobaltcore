// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package openbao

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "s.session-token"

// recordedRequest is one request the fake server saw.
type recordedRequest struct {
	Method, Path, Token string
	Body                map[string]any
}

// fakeServer answers the login with testToken and every other path with the
// handler routes holds for it, recording each request.
type fakeServer struct {
	t      *testing.T
	mu     sync.Mutex
	seen   []recordedRequest
	routes map[string]func(w http.ResponseWriter)
}

func newFakeServer(t *testing.T, routes map[string]func(w http.ResponseWriter)) (*fakeServer, *httptest.Server) {
	t.Helper()
	f := &fakeServer{t: t, routes: routes}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Token: r.Header.Get("X-Vault-Token")}
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		if err := json.Unmarshal(data, &rec.Body); err != nil {
			f.t.Errorf("request body of %s %s is not JSON: %v", r.Method, r.URL.Path, err)
		}
	}
	f.mu.Lock()
	f.seen = append(f.seen, rec)
	f.mu.Unlock()

	if strings.HasSuffix(r.URL.Path, "/login") {
		_, _ = io.WriteString(w, `{"auth":{"client_token":"`+testToken+`"}}`)
		return
	}
	if route, ok := f.routes[r.Method+" "+r.URL.Path]; ok {
		route(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeServer) requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.seen...)
}

func login(t *testing.T, srv *httptest.Server) Client {
	t.Helper()
	c, err := Login(context.Background(), Config{
		Server: srv.URL, KubernetesMount: "kubernetes/management", Role: "c5c3-operator", JWT: "sa-jwt",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	return c
}

func status(code int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func TestLogin_PostsRoleAndJWTAndCarriesTheToken(t *testing.T) {
	f, srv := newFakeServer(t, map[string]func(http.ResponseWriter){
		"GET /v1/database/mariadb/roles/r": status(http.StatusOK, `{"data":{"db_name":"keystone-ks"}}`),
	})
	c := login(t, srv)
	if _, _, err := c.ReadDatabaseRole(context.Background(), "database/mariadb", "r"); err != nil {
		t.Fatalf("ReadDatabaseRole: %v", err)
	}

	seen := f.requests()
	if len(seen) != 2 {
		t.Fatalf("got %d requests, want the login and the read: %+v", len(seen), seen)
	}
	got := seen[0]
	if got.Method != http.MethodPost || got.Path != "/v1/auth/kubernetes/management/login" {
		t.Errorf("login went to %s %s", got.Method, got.Path)
	}
	if got.Body["role"] != "c5c3-operator" || got.Body["jwt"] != "sa-jwt" {
		t.Errorf("login body = %v, want the role and the jwt", got.Body)
	}
	if got.Token != "" {
		t.Errorf("the login carried a token %q before it had one", got.Token)
	}
	if seen[1].Token != testToken {
		t.Errorf("the read carried token %q, want the login's %q", seen[1].Token, testToken)
	}
}

func TestLogin_AnswerWithoutATokenIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"auth":null}`)
	}))
	t.Cleanup(srv.Close)

	_, err := Login(context.Background(), Config{Server: srv.URL, KubernetesMount: "kubernetes/management"})
	if err == nil || !strings.Contains(err.Error(), "carries no client token") {
		t.Fatalf("Login err = %v, want the missing-token error", err)
	}
}

func TestLogin_403IsPermissionDeniedAndNamesNoSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"errors":["permission denied"]}`)
	}))
	t.Cleanup(srv.Close)

	_, err := Login(context.Background(), Config{
		Server: srv.URL, KubernetesMount: "kubernetes/management", Role: "c5c3-operator", JWT: "sa-jwt",
	})
	if !IsPermissionDenied(err) {
		t.Fatalf("Login err = %v, want a permission denial", err)
	}
	if strings.Contains(err.Error(), "sa-jwt") {
		t.Errorf("the error %q carries the JWT", err)
	}
}

func TestReadDatabaseRole_AbsentRoleIsNotAnError(t *testing.T) {
	_, srv := newFakeServer(t, map[string]func(http.ResponseWriter){
		"GET /v1/database/mariadb/roles/missing": status(http.StatusNotFound, `{"errors":[]}`),
	})
	role, ok, err := login(t, srv).ReadDatabaseRole(context.Background(), "database/mariadb", "missing")
	if err != nil || ok || role != nil {
		t.Fatalf("ReadDatabaseRole = (%v, %v, %v), want (nil, false, nil)", role, ok, err)
	}
}

func TestReadDatabaseRole_DecodesIntegerTTLs(t *testing.T) {
	cases := []struct {
		name                 string
		defaultTTL, maxTTL   int64
		wantDefault, wantMax time.Duration
	}{
		{name: "whole hours", defaultTTL: 172800, maxTTL: 259200, wantDefault: 48 * time.Hour, wantMax: 72 * time.Hour},
		{name: "not whole hours", defaultTTL: 90, maxTTL: 5400, wantDefault: 90 * time.Second, wantMax: 90 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"data": map[string]any{
				"db_name":               "keystone-ks",
				"creation_statements":   []string{"CREATE USER x;", "GRANT y;"},
				"revocation_statements": []string{"DROP USER x;"},
				"default_ttl":           tc.defaultTTL,
				"max_ttl":               tc.maxTTL,
			}})
			_, srv := newFakeServer(t, map[string]func(http.ResponseWriter){
				"GET /v1/database/mariadb/roles/r": status(http.StatusOK, string(body)),
			})
			role, ok, err := login(t, srv).ReadDatabaseRole(context.Background(), "database/mariadb", "r")
			if err != nil || !ok {
				t.Fatalf("ReadDatabaseRole = (%v, %v, %v)", role, ok, err)
			}
			want := &DatabaseRole{
				DBName:               "keystone-ks",
				CreationStatements:   []string{"CREATE USER x;", "GRANT y;"},
				RevocationStatements: []string{"DROP USER x;"},
				DefaultTTL:           tc.wantDefault,
				MaxTTL:               tc.wantMax,
			}
			if !reflect.DeepEqual(role, want) {
				t.Errorf("role = %+v, want %+v", role, want)
			}
		})
	}
}

func TestReadDatabaseRole_500IsAnAPIErrorWithTheServerErrors(t *testing.T) {
	_, srv := newFakeServer(t, map[string]func(http.ResponseWriter){
		"GET /v1/database/mariadb/roles/r": status(http.StatusInternalServerError, `{"errors":["one","two"]}`),
	})
	_, _, err := login(t, srv).ReadDatabaseRole(context.Background(), "database/mariadb", "r")

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an APIError", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError || !reflect.DeepEqual(apiErr.Errors, []string{"one", "two"}) {
		t.Errorf("APIError = %+v", apiErr)
	}
	if want := "openbao: 500 on GET /v1/database/mariadb/roles/r: one; two"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if IsPermissionDenied(err) {
		t.Errorf("a 500 reads as a permission denial")
	}
}

func TestAPIError_NonJSONBodyAndEmptyBody(t *testing.T) {
	_, srv := newFakeServer(t, map[string]func(http.ResponseWriter){
		"GET /v1/database/mariadb/roles/text":  status(http.StatusBadGateway, "upstream down\n"),
		"GET /v1/database/mariadb/roles/empty": status(http.StatusBadGateway, ""),
	})
	c := login(t, srv)

	_, _, err := c.ReadDatabaseRole(context.Background(), "database/mariadb", "text")
	if err == nil || !strings.HasSuffix(err.Error(), ": upstream down") {
		t.Errorf("non-JSON body err = %v, want the body as the error", err)
	}
	_, _, err = c.ReadDatabaseRole(context.Background(), "database/mariadb", "empty")
	if err == nil || !strings.HasSuffix(err.Error(), ": <no error body>") {
		t.Errorf("empty body err = %v, want the no-body marker", err)
	}
}

func TestWriteDatabaseRole_SendsTheRoleToTheRolePath(t *testing.T) {
	f, srv := newFakeServer(t, nil)
	role := DatabaseRole{
		DBName:               "keystone-ks",
		CreationStatements:   []string{"CREATE USER '{{name}}';", "GRANT ALL;"},
		RevocationStatements: []string{"DROP USER '{{name}}';"},
		DefaultTTL:           48 * time.Hour,
		MaxTTL:               72 * time.Hour,
	}
	if err := login(t, srv).WriteDatabaseRole(context.Background(), "database/mariadb", "order.ns.x", role); err != nil {
		t.Fatalf("WriteDatabaseRole: %v", err)
	}

	got := f.requests()[1]
	if got.Method != http.MethodPost || got.Path != "/v1/database/mariadb/roles/order.ns.x" {
		t.Fatalf("write went to %s %s", got.Method, got.Path)
	}
	want := map[string]any{
		"db_name":               "keystone-ks",
		"creation_statements":   []any{"CREATE USER '{{name}}';", "GRANT ALL;"},
		"revocation_statements": []any{"DROP USER '{{name}}';"},
		"default_ttl":           float64(172800),
		"max_ttl":               float64(259200),
	}
	if !reflect.DeepEqual(got.Body, want) {
		t.Errorf("write body = %v, want %v", got.Body, want)
	}
}

// TestDatabaseRole_ReadsBackWhatWasWritten serves the body a write sent as the
// role a read gets, the way OpenBao stores it, and expects the role that was
// written, so a caller's equality check holds whatever durations it uses.
func TestDatabaseRole_ReadsBackWhatWasWritten(t *testing.T) {
	role := DatabaseRole{
		DBName:               "keystone-ks",
		CreationStatements:   []string{"CREATE USER '{{name}}';"},
		RevocationStatements: []string{"DROP USER '{{name}}';"},
		DefaultTTL:           2880 * time.Minute,
		MaxTTL:               72*time.Hour + 30*time.Minute,
	}
	f, srv := newFakeServer(t, nil)
	if err := login(t, srv).WriteDatabaseRole(context.Background(), "database/mariadb", "r", role); err != nil {
		t.Fatalf("WriteDatabaseRole: %v", err)
	}
	stored, err := json.Marshal(map[string]any{"data": f.requests()[1].Body})
	if err != nil {
		t.Fatalf("encoding the stored role: %v", err)
	}
	_, srv = newFakeServer(t, map[string]func(http.ResponseWriter){
		"GET /v1/database/mariadb/roles/r": status(http.StatusOK, string(stored)),
	})

	got, ok, err := login(t, srv).ReadDatabaseRole(context.Background(), "database/mariadb", "r")

	if err != nil || !ok {
		t.Fatalf("ReadDatabaseRole = (%v, %v, %v)", got, ok, err)
	}
	if !reflect.DeepEqual(*got, role) {
		t.Errorf("read back %+v, want %+v", *got, role)
	}
}

func TestWriteDatabaseRole_403IsPermissionDenied(t *testing.T) {
	_, srv := newFakeServer(t, map[string]func(http.ResponseWriter){
		"POST /v1/database/mariadb/roles/r": status(http.StatusForbidden, `{"errors":["1 error occurred:\n\t* permission denied\n\n"]}`),
	})
	err := login(t, srv).WriteDatabaseRole(context.Background(), "database/mariadb", "r", DatabaseRole{})

	if !IsPermissionDenied(err) {
		t.Fatalf("err = %v, want a permission denial", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("err = %#v, want an APIError with 403", err)
	}
}

func TestDeleteAndRevoke_HitTheirPathsAndTreat404AsSuccess(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{name: "204", code: http.StatusNoContent},
		{name: "404", code: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeServer(t, map[string]func(http.ResponseWriter){
				"DELETE /v1/database/mariadb/roles/order.ns.x":                        status(tc.code, ""),
				"POST /v1/sys/leases/revoke-prefix/database/mariadb/creds/order.ns.x": status(tc.code, ""),
			})
			c := login(t, srv)
			if err := c.RevokeLeasePrefix(context.Background(), "database/mariadb/creds/order.ns.x"); err != nil {
				t.Errorf("RevokeLeasePrefix: %v", err)
			}
			if err := c.DeleteDatabaseRole(context.Background(), "database/mariadb", "order.ns.x"); err != nil {
				t.Errorf("DeleteDatabaseRole: %v", err)
			}
			seen := f.requests()
			if len(seen) != 3 ||
				seen[1].Path != "/v1/sys/leases/revoke-prefix/database/mariadb/creds/order.ns.x" ||
				seen[2].Method != http.MethodDelete || seen[2].Path != "/v1/database/mariadb/roles/order.ns.x" {
				t.Errorf("requests = %+v", seen)
			}
		})
	}
}

func TestDeleteDatabaseRole_OtherFailuresAreErrors(t *testing.T) {
	_, srv := newFakeServer(t, map[string]func(http.ResponseWriter){
		"DELETE /v1/database/mariadb/roles/r": status(http.StatusForbidden, `{"errors":["permission denied"]}`),
	})
	if err := login(t, srv).DeleteDatabaseRole(context.Background(), "database/mariadb", "r"); !IsPermissionDenied(err) {
		t.Fatalf("err = %v, want a permission denial", err)
	}
}

func TestClose_PostsRevokeSelf(t *testing.T) {
	f, srv := newFakeServer(t, nil)
	if err := login(t, srv).Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := f.requests()[1]
	if got.Method != http.MethodPost || got.Path != "/v1/auth/token/revoke-self" || got.Token != testToken {
		t.Errorf("Close sent %+v", got)
	}
}

func TestLogin_RefusesIncompleteTLSMaterial(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "a CA alone", cfg: Config{CACert: []byte("ca")}, want: "must be set together"},
		{name: "a keypair without a CA", cfg: Config{ClientCert: []byte("c"), ClientKey: []byte("k")}, want: "must be set together"},
		{name: "a CA that is not PEM", cfg: Config{CACert: []byte("ca"), ClientCert: []byte("c"), ClientKey: []byte("k")}, want: "no PEM certificate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Server = "https://127.0.0.1:1"
			_, err := Login(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Login err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLogin_PresentsTheClientCertificateOverMTLS(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, "openbao", x509.ExtKeyUsageServerAuth)
	clientCert := ca.issue(t, "order-db-openbao-client", x509.ExtKeyUsageClientAuth)

	var peer string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer = r.TLS.PeerCertificates[0].Subject.CommonName
		_, _ = io.WriteString(w, `{"auth":{"client_token":"`+testToken+`"}}`)
	}))
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{server.keypair(t)},
		ClientCAs:    ca.pool(),
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	c, err := Login(context.Background(), Config{
		Server: srv.URL, KubernetesMount: "kubernetes/management",
		CACert: ca.certPEM, ClientCert: clientCert.certPEM, ClientKey: clientCert.keyPEM,
	})
	if err != nil {
		t.Fatalf("Login over mTLS: %v", err)
	}
	if peer != "order-db-openbao-client" {
		t.Errorf("the server saw client certificate %q", peer)
	}
	_ = c.Close(context.Background())
}

func TestRequest_ContextDeadlinePropagates(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Login(ctx, Config{Server: srv.URL, KubernetesMount: "kubernetes/management"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if !strings.HasPrefix(err.Error(), "openbao: POST /v1/auth/kubernetes/management/login: ") {
		t.Errorf("err = %q, want the method and the path", err)
	}
}

// --- test PKI ---

type testCert struct {
	cert            *x509.Certificate
	key             *ecdsa.PrivateKey
	certPEM, keyPEM []byte
}

func (c testCert) keypair(t *testing.T) tls.Certificate {
	t.Helper()
	kp, err := tls.X509KeyPair(c.certPEM, c.keyPEM)
	if err != nil {
		t.Fatalf("loading keypair: %v", err)
	}
	return kp
}

type testCA struct{ testCert }

func newTestCA(t *testing.T) testCA {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "openbao-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	return testCA{signCert(t, tmpl, nil)}
}

func (ca testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

func (ca testCA) issue(t *testing.T, cn string, usage x509.ExtKeyUsage) testCert {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	return signCert(t, tmpl, &ca.testCert)
}

// signCert creates a key and a certificate from tmpl, signed by parent or
// self-signed when parent is nil.
func signCert(t *testing.T, tmpl *x509.Certificate, parent *testCert) testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	signer, signerCert := key, tmpl
	if parent != nil {
		signer, signerCert = parent.key, parent.cert
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, &key.PublicKey, signer)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	return testCert{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
}
