// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"k8s.io/client-go/rest"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// HTTPDoer is the client seam an operator's HTTP calls run through. It is
// declared here rather than imported from internal/common/healthcheck so this
// package does not depend on that one; the two shapes are identical, so an
// *http.Client satisfies both.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// NewServiceProxyDoer builds the client that sends requests addressed to a
// cluster-local Service through the API server the cluster cfg addresses, under
// the credentials of the kubeconfig that cluster is registered with.
//
// A CR placed on a target cluster runs its workload there, so the Service DNS
// name the operator dials (http://<name>.<namespace>.svc.cluster.local:<port>)
// resolves on that cluster and nowhere else. The API server's services/proxy
// subresource is the one route into it every registration already carries.
//
// The rewrite happens inside the transport, so callers keep building the
// Service URL they always built: status conditions, certificate SANs, and the
// probe cache key stay what an unplaced CR produces, and only who executes the
// request moves. The transport refuses anything that is not a cluster-local
// Service, so the only host this ever dials is the API server of cfg.
//
// The result is an *http.Client rather than a bare HTTPDoer because the
// keystone identity client bridges its doer into gophercloud only when it is a
// real *http.Client.
func NewServiceProxyDoer(cfg *rest.Config) (*http.Client, error) {
	if cfg == nil {
		return nil, errors.New("building a service-proxy transport: the target cluster has no REST config")
	}

	apiServer, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("parsing the API server URL %q of the target cluster: %w", cfg.Host, err)
	}
	if apiServer.Host == "" {
		return nil, fmt.Errorf("the API server URL %q of the target cluster names no host", cfg.Host)
	}

	// rest.TransportFor resolves through client-go's transport cache, which
	// hands back the same transport for an identical config. Building a doer per
	// reconcile pass therefore does not build a connection pool per pass.
	base, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("building the transport for the target cluster API server: %w", err)
	}

	return &http.Client{
		Transport: &serviceProxyTransport{base: base, apiServer: apiServer},
	}, nil
}

// serviceProxyTransport rewrites a cluster-local Service URL onto the API
// server's services/proxy subresource and sends it over base, the transport
// built from that cluster's REST config.
type serviceProxyTransport struct {
	base      http.RoundTripper
	apiServer *url.URL
}

// RoundTrip rewrites req's URL and forwards it. A URL that does not address a
// cluster-local Service is refused rather than dialled directly: this transport
// exists for the Service URLs a placed CR cannot reach, and anything else
// reaching it is a call site that never meant to be proxied.
func (t *serviceProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	svc, err := parseServiceURL(req.URL)
	if err != nil {
		return nil, err
	}

	proxied := req.Clone(req.Context())
	proxied.URL = &url.URL{
		Scheme: t.apiServer.Scheme,
		Host:   t.apiServer.Host,
		Path: fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s:%s:%s/proxy%s",
			strings.TrimSuffix(t.apiServer.Path, "/"),
			svc.namespace, svc.scheme, svc.name, svc.port, req.URL.Path),
		RawQuery: req.URL.RawQuery,
	}
	// The Host override would otherwise travel to the API server as the Service
	// DNS name; clearing it lets the transport derive the header from the
	// rewritten URL.
	proxied.Host = ""

	return t.base.RoundTrip(proxied)
}

// serviceTarget is the Service a cluster-local URL addresses.
type serviceTarget struct {
	namespace string
	name      string
	scheme    string
	port      string
}

// parseServiceURL reads the Service a cluster-local URL addresses. Only the two
// in-cluster DNS forms are accepted — <name>.<namespace>.svc and the fully
// qualified <name>.<namespace>.svc.cluster.local — because the proxy path needs
// both the Service name and its namespace, and no other host shape carries the
// two.
func parseServiceURL(u *url.URL) (serviceTarget, error) {
	if u.Scheme != "http" && u.Scheme != "https" {
		return serviceTarget{}, fmt.Errorf(
			"cannot proxy %q: only http and https URLs go through the target API server", u)
	}

	labels := strings.Split(u.Hostname(), ".")
	clusterLocal := len(labels) == 5 && labels[2] == "svc" && labels[3] == "cluster" && labels[4] == "local"
	short := len(labels) == 3 && labels[2] == "svc"
	if !clusterLocal && !short {
		return serviceTarget{}, fmt.Errorf(
			"cannot proxy %q: only cluster-local Service URLs "+
				"(<name>.<namespace>.svc or <name>.<namespace>.svc.cluster.local) "+
				"go through the target API server", u)
	}

	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}

	return serviceTarget{namespace: labels[1], name: labels[0], scheme: u.Scheme, port: port}, nil
}

// ResolveHTTPDoer returns the doer an operator runs a CR's HTTP calls through.
// A nil resolver (the operator runs without a multicluster manager) or a nil
// ref (the CR names no target cluster) both select local, so a single-cluster
// deployment keeps dialling the Service directly.
//
// A named cluster gets a service-proxy doer built from that cluster's REST
// config. The caller's URL is untouched — it stays the Service URL — so the
// conditions and cache keys derived from it do not change when a CR is placed.
//
// The resolver's error is returned unwrapped, for the reason
// ResolveChildrenClient returns it unwrapped: callers put its text straight
// into a status condition, where the upstream message ("cluster not found" for
// an unregistered name) is the part an operator needs.
func ResolveHTTPDoer(
	ctx context.Context,
	resolver ClusterResolver,
	ref *commonv1.TargetClusterRefSpec,
	local HTTPDoer,
) (HTTPDoer, error) {
	if resolver == nil || ref == nil {
		return local, nil
	}

	cl, err := resolver.GetCluster(ctx, mcruntime.ClusterName(ref.Name))
	if err != nil {
		return nil, err
	}

	doer, err := NewServiceProxyDoer(cl.GetConfig())
	if err != nil {
		return nil, err
	}
	return doer, nil
}
