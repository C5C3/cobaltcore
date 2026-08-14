// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"errors"
	"testing"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// probeClient answers with a mapper of the test's choosing, nil unless one was
// set, and records that it was asked for one at all. The recording is what
// tells an unasked mapper from one that answered nothing: every real client
// carries a mapper, the fake one included, so a local client that probed
// anyway would otherwise be indistinguishable from one that did not.
//
// Embedding the interface leaves every other method nil, so a probe reaching
// past the RESTMapper panics rather than passing quietly.
type probeClient struct {
	client.Client
	mapper meta.RESTMapper
	asked  bool
}

func (c *probeClient) RESTMapper() meta.RESTMapper {
	c.asked = true
	return c.mapper
}

// failingMapper fails every mapping with one error value, held rather than
// built per call so a test can assert the caller got that exact error back. A
// plain error is not a no-match: it is the throttled or briefly unreachable
// target cluster, whose answer says nothing about the kind.
type failingMapper struct {
	meta.RESTMapper
	err error
}

func (m failingMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, m.err
}

// remoteChildrenServing builds the children client the production path hands a
// reconciler: a target cluster's own client, resolved through the manager and
// marked remote, whose RESTMapper knows the kinds passed here and no others.
func remoteChildrenServing(t *testing.T, kinds ...schema.GroupVersionKind) client.Client {
	t.Helper()

	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range kinds {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}

	resolver := &fakeResolver{cl: fakeCluster{
		c: fake.NewClientBuilder().WithRESTMapper(mapper).Build(),
	}}

	children, err := ResolveChildrenClient(context.Background(), resolver, fake.NewClientBuilder().Build(),
		&commonv1.TargetClusterRefSpec{Name: "remote-a"})
	if err != nil {
		t.Fatalf("resolving the children client: %v", err)
	}
	return children
}

// A CR that keeps its children on the management cluster is answered by the
// latch its operator probed at setup, in both directions. Re-probing the
// management cluster on every reconcile is the cost single-cluster deployments
// must not pay for a feature they do not use.
func TestChildrenServeKindLocalClientAnswersFromTheLatch(t *testing.T) {
	for name, available := range map[string]bool{
		"the kind is installed locally":     true,
		"the kind is not installed locally": false,
	} {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			children := &probeClient{}

			serves, err := ChildrenServeKind(children, available, httpRouteGVK)

			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(serves).To(gomega.Equal(available))
			g.Expect(children.asked).To(gomega.BeFalse(),
				"a local client must be answered from the latch alone, without touching its RESTMapper")
		})
	}
}

// A client with no mapper cannot be asked what its cluster serves, and the
// local latch describes the wrong cluster. Neither answer is available, so the
// probe fails rather than reporting the negative verdict its callers skip
// deletes on.
func TestChildrenServeKindRemoteClientWithoutAMapperIsAProbeFailure(t *testing.T) {
	g := gomega.NewWithT(t)

	stub := &probeClient{}

	serves, err := ChildrenServeKind(Remote(stub), true, httpRouteGVK)

	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("carries no RESTMapper")),
		"a target that could not be asked must not be reported as one that answered no")
	g.Expect(serves).To(gomega.BeFalse())
	g.Expect(stub.asked).To(gomega.BeTrue(),
		"a remote client is the case this probe exists for; it has to be asked")
}

func TestChildrenServeKindSeesAKindTheTargetClusterServes(t *testing.T) {
	g := gomega.NewWithT(t)

	// The latch says the kind is absent on the management cluster, so only the
	// target's own mapper can produce this answer.
	serves, err := ChildrenServeKind(remoteChildrenServing(t, httpRouteGVK), false, httpRouteGVK)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(serves).To(gomega.BeTrue())
}

func TestChildrenServeKindSeesAKindTheTargetClusterDoesNotServe(t *testing.T) {
	g := gomega.NewWithT(t)

	// A no-match answer is a verdict, not a failure: the CRD is simply not
	// installed on that cluster, and the caller skips the projection.
	serves, err := ChildrenServeKind(remoteChildrenServing(t), true, httpRouteGVK)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(serves).To(gomega.BeFalse())
}

// A discovery failure says nothing about the kind, so it is reported rather
// than turned into a verdict. The caller wraps it once on its way into a status
// condition under CapabilityProbeFailed, and a prefix added here would show up
// in that message twice over.
func TestChildrenServeKindReturnsADiscoveryFailureUnwrapped(t *testing.T) {
	g := gomega.NewWithT(t)

	boom := errors.New("boom")
	children := Remote(&probeClient{mapper: failingMapper{err: boom}})

	serves, err := ChildrenServeKind(children, true, httpRouteGVK)

	g.Expect(serves).To(gomega.BeFalse())
	g.Expect(err).To(gomega.BeIdenticalTo(boom))
}
