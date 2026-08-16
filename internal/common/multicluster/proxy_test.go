// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onsi/gomega"
	"k8s.io/client-go/rest"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// recordedRequest is what the stub API server saw.
type recordedRequest struct {
	path  string
	query string
}

// stubAPIServer stands in for the target cluster's API server. It answers every
// request with 200 and records the path the rewrite produced. Plain HTTP is
// enough even for the https cases: the scheme of the ORIGINAL URL only decides
// the <scheme>: segment of the proxy path, never how the API server itself is
// dialled.
func stubAPIServer(t *testing.T) (*httptest.Server, *[]recordedRequest) {
	t.Helper()

	var seen []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, recordedRequest{path: r.URL.Path, query: r.URL.RawQuery})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	return srv, &seen
}

func TestServiceProxyDoerRewritesClusterLocalServiceURLs(t *testing.T) {
	tests := []struct {
		name      string
		request   string
		wantPath  string
		wantQuery string
	}{
		{
			name:     "http on the short Service form",
			request:  "http://keystone.openstack.svc:5000/v3",
			wantPath: "/api/v1/namespaces/openstack/services/http:keystone:5000/proxy/v3",
		},
		{
			name:     "http on the fully qualified Service form",
			request:  "http://keystone.openstack.svc.cluster.local:5000/v3",
			wantPath: "/api/v1/namespaces/openstack/services/http:keystone:5000/proxy/v3",
		},
		{
			name:     "https keeps its scheme in the proxy path",
			request:  "https://glance.openstack.svc:9292/healthcheck",
			wantPath: "/api/v1/namespaces/openstack/services/https:glance:9292/proxy/healthcheck",
		},
		{
			name:     "https without a port defaults to 443",
			request:  "https://glance.openstack.svc.cluster.local/healthcheck",
			wantPath: "/api/v1/namespaces/openstack/services/https:glance:443/proxy/healthcheck",
		},
		{
			name:     "http without a port defaults to 80",
			request:  "http://horizon.openstack.svc/auth/login/",
			wantPath: "/api/v1/namespaces/openstack/services/http:horizon:80/proxy/auth/login/",
		},
		{
			name:      "the query survives the rewrite",
			request:   "http://placement.openstack.svc:8778/resource_providers?name=cell1",
			wantPath:  "/api/v1/namespaces/openstack/services/http:placement:8778/proxy/resource_providers",
			wantQuery: "name=cell1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			srv, seen := stubAPIServer(t)

			doer, err := NewServiceProxyDoer(&rest.Config{Host: srv.URL})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, tc.request, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())

			resp, err := doer.Do(req)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(resp.Body.Close()).To(gomega.Succeed())
			g.Expect(resp.StatusCode).To(gomega.Equal(http.StatusOK))

			g.Expect(*seen).To(gomega.HaveLen(1))
			g.Expect((*seen)[0].path).To(gomega.Equal(tc.wantPath))
			g.Expect((*seen)[0].query).To(gomega.Equal(tc.wantQuery))
		})
	}
}

func TestServiceProxyDoerRefusesHostsThatAreNoService(t *testing.T) {
	tests := []struct {
		name    string
		request string
	}{
		{name: "a public host", request: "http://keystone.example.com/v3"},
		{name: "a bare Service name", request: "http://keystone/v3"},
		{name: "a namespaced name without the svc label", request: "http://keystone.openstack/v3"},
		{name: "a search-domain form that is not cluster.local", request: "http://keystone.openstack.svc.example.com/v3"},
		{name: "a scheme the API server does not proxy", request: "ftp://keystone.openstack.svc:5000/v3"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			srv, seen := stubAPIServer(t)

			doer, err := NewServiceProxyDoer(&rest.Config{Host: srv.URL})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, tc.request, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())

			resp, err := doer.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.request),
				"the refusal must name the URL it refused")
			g.Expect(*seen).To(gomega.BeEmpty(),
				"a URL that is no Service must never reach the API server")
		})
	}
}

func TestNewServiceProxyDoerRejectsAConfigWithoutAHost(t *testing.T) {
	g := gomega.NewWithT(t)

	_, err := NewServiceProxyDoer(&rest.Config{})
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("names no host")))

	_, err = NewServiceProxyDoer(nil)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("no REST config")))
}

func TestResolveHTTPDoerNilResolverReturnsLocal(t *testing.T) {
	g := gomega.NewWithT(t)

	local := &http.Client{}

	// A ref is present, so only the missing resolver can select local: an
	// operator built without a multicluster manager must keep probing directly.
	got, err := ResolveHTTPDoer(context.Background(), nil,
		&commonv1.TargetClusterRefSpec{Name: "remote-a"}, local)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.BeIdenticalTo(local))
}

func TestResolveHTTPDoerNilRefSkipsResolver(t *testing.T) {
	g := gomega.NewWithT(t)

	local := &http.Client{}
	resolver := &fakeResolver{cl: fakeCluster{cfg: &rest.Config{Host: "https://unused.example.com"}}}

	got, err := ResolveHTTPDoer(context.Background(), resolver, nil, local)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.BeIdenticalTo(local))
	g.Expect(resolver.calls).To(gomega.BeEmpty(),
		"an unplaced CR must not consult the resolver at all")
}

func TestResolveHTTPDoerPropagatesTheResolverError(t *testing.T) {
	g := gomega.NewWithT(t)

	resolver := &fakeResolver{err: mcruntime.ErrClusterNotFound}

	got, err := ResolveHTTPDoer(context.Background(), resolver,
		&commonv1.TargetClusterRefSpec{Name: "nowhere"}, &http.Client{})

	g.Expect(got).To(gomega.BeNil())
	g.Expect(errors.Is(err, mcruntime.ErrClusterNotFound)).To(gomega.BeTrue(),
		"the resolver's error goes onto a status condition unwrapped")
	g.Expect(resolver.calls).To(gomega.ConsistOf(mcruntime.ClusterName("nowhere")))
}

func TestResolveHTTPDoerProxiesThroughTheResolvedCluster(t *testing.T) {
	g := gomega.NewWithT(t)
	srv, seen := stubAPIServer(t)

	resolver := &fakeResolver{cl: fakeCluster{cfg: &rest.Config{Host: srv.URL}}}
	local := &http.Client{}

	got, err := ResolveHTTPDoer(context.Background(), resolver,
		&commonv1.TargetClusterRefSpec{Name: "remote-a"}, local)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).NotTo(gomega.BeIdenticalTo(local))

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://keystone.openstack.svc.cluster.local:5000/v3", nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	resp, err := got.Do(req)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(resp.Body.Close()).To(gomega.Succeed())

	g.Expect(*seen).To(gomega.HaveLen(1))
	g.Expect((*seen)[0].path).To(gomega.Equal("/api/v1/namespaces/openstack/services/http:keystone:5000/proxy/v3"))
	g.Expect(req.URL.String()).To(gomega.Equal("http://keystone.openstack.svc.cluster.local:5000/v3"),
		"the caller's request must keep the Service URL its conditions are derived from")
}
