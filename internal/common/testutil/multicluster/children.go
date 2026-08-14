// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package multicluster provides the children-client fixtures a unit test needs
// to drive a sub-reconciler as a CR with spec.targetClusterRef drives it: a
// target cluster's client with a RESTMapper of the test's choosing, that client
// resolved the production way so it carries the remote mark, and a target that
// cannot be probed at all.
//
// The fixtures live here because every service operator needs the same three
// and none of them owns the shape: the children client is resolved by
// internal/common/multicluster, and a copy per operator drifts the moment the
// resolution changes. The package internal/common/multicluster tests itself
// with its own fixtures, since importing this one back would be a cycle.
package multicluster

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// TargetFake builds a target cluster's client from builder — the operator's own
// scheme and seeded objects — behind a RESTMapper serving exactly the given
// kinds. Passing none is the target cluster that carries neither the Gateway
// API nor the cert-manager CRDs, which is what the capability probe has to tell
// from one that does.
func TargetFake(builder *fake.ClientBuilder, serves ...schema.GroupVersionKind) client.Client {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range serves {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return builder.WithRESTMapper(mapper).Build()
}

// RemoteChildren resolves target as the children client of a CR naming a target
// cluster, through the same ResolveChildrenClient the reconcilers call, so it
// carries the remote mark the capability probe and the ownership claim read.
func RemoteChildren(t *testing.T, local, target client.Client) client.Client {
	t.Helper()

	children, err := commonmulticluster.ResolveChildrenClient(context.Background(),
		&oneClusterResolver{cl: fakeCluster{c: target}}, local,
		&commonv1.TargetClusterRefSpec{Name: "remote-a"})
	if err != nil {
		t.Fatalf("resolving the children client: %v", err)
	}
	return children
}

// UnprobeableChildren is a target cluster that cannot say what it serves: its
// mapper fails with an error that is not a no-match, which is what a throttled
// or briefly unreachable API server looks like from the probe. It is marked
// remote, because a local client is never probed at all.
func UnprobeableChildren(inner client.Client) client.Client {
	return commonmulticluster.Remote(brokenMapperClient{Client: inner})
}

// oneClusterResolver and fakeCluster stand in for the multicluster manager,
// registering the one target cluster under every name.
type oneClusterResolver struct{ cl cluster.Cluster }

func (r *oneClusterResolver) GetCluster(context.Context, mcruntime.ClusterName) (cluster.Cluster, error) {
	return r.cl, nil
}

type fakeCluster struct {
	cluster.Cluster
	c client.Client
}

func (f fakeCluster) GetClient() client.Client    { return f.c }
func (f fakeCluster) GetAPIReader() client.Reader { return f.c }

type brokenMapperClient struct {
	client.Client
}

func (brokenMapperClient) RESTMapper() meta.RESTMapper { return failingMapper{} }

type failingMapper struct {
	meta.RESTMapper
}

func (failingMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, errors.New("discovery is unavailable")
}
