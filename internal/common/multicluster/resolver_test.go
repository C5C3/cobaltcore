// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// fakeResolver stands in for the multicluster manager. It records what it was
// asked for so a test can assert the resolver was skipped entirely.
type fakeResolver struct {
	cl    cluster.Cluster
	err   error
	calls []mcruntime.ClusterName
}

func (f *fakeResolver) GetCluster(_ context.Context, name mcruntime.ClusterName) (cluster.Cluster, error) {
	f.calls = append(f.calls, name)
	return f.cl, f.err
}

// fakeCluster implements only GetClient and GetAPIReader. Embedding the
// interface leaves every other method nil, which panics if the resolver ever
// reaches for one.
type fakeCluster struct {
	cluster.Cluster
	c      client.Client
	reader client.Reader
}

func (f fakeCluster) GetClient() client.Client { return f.c }

func (f fakeCluster) GetAPIReader() client.Reader { return f.reader }

func TestResolveChildrenClientNilResolverReturnsLocal(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()

	// A ref is present, so only the missing resolver can select local: an
	// operator built without a multicluster manager must not fail here.
	got, err := ResolveChildrenClient(context.Background(), nil, local,
		&commonv1.TargetClusterRefSpec{Name: "remote-a"})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.BeIdenticalTo(local))
	g.Expect(IsRemote(got)).To(gomega.BeFalse(),
		"a client on the management cluster must keep claiming its children by owner reference")
}

func TestResolveChildrenClientNilRefSkipsResolver(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	resolver := &fakeResolver{cl: fakeCluster{c: fake.NewClientBuilder().Build()}}

	got, err := ResolveChildrenClient(context.Background(), resolver, local, nil)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.BeIdenticalTo(local))
	g.Expect(IsRemote(got)).To(gomega.BeFalse())
	// Not merely "local came back": a CR without a ref must not cost a lookup.
	g.Expect(resolver.calls).To(gomega.BeEmpty())
}

func TestResolveChildrenClientPropagatesResolverError(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	resolver := &fakeResolver{err: mcruntime.ErrClusterNotFound}

	got, err := ResolveChildrenClient(context.Background(), resolver, local,
		&commonv1.TargetClusterRefSpec{Name: "unregistered"})

	g.Expect(got).To(gomega.BeNil())
	g.Expect(errors.Is(err, mcruntime.ErrClusterNotFound)).To(gomega.BeTrue())
	// The text goes into a status condition verbatim, so wrapping would show
	// up here as a prefix.
	g.Expect(err.Error()).To(gomega.Equal("cluster not found"))
}

func TestResolveChildrenClientReturnsTargetClusterClient(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	onTheTarget := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "there", Namespace: "openstack"}}
	remote := fake.NewClientBuilder().WithObjects(onTheTarget).Build()
	resolver := &fakeResolver{cl: fakeCluster{c: remote}}

	got, err := ResolveChildrenClient(context.Background(), resolver, local,
		&commonv1.TargetClusterRefSpec{Name: "remote-a"})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	// The mark is what tells every write site to claim its children by label
	// rather than by an owner reference the target cluster cannot resolve.
	g.Expect(IsRemote(got)).To(gomega.BeTrue())
	g.Expect(got).NotTo(gomega.BeIdenticalTo(local))
	// It is a wrapper, so the calls still land on the target cluster.
	g.Expect(got.Get(context.Background(), client.ObjectKeyFromObject(onTheTarget), &corev1.ConfigMap{})).
		To(gomega.Succeed())
	g.Expect(resolver.calls).To(gomega.Equal([]mcruntime.ClusterName{"remote-a"}))
}

// seedUnresolvableSince backdates this process's first-failure record for name,
// so a test can reach past the in-process half of the abandon window without
// sitting it out. The entry is dropped again afterwards to keep the
// package-level map from leaking between tests.
func seedUnresolvableSince(t *testing.T, name string, since time.Time) {
	t.Helper()

	unresolvableMu.Lock()
	unresolvableSince[name] = since
	unresolvableMu.Unlock()
	t.Cleanup(func() { forgetUnresolvable(name) })
}

// A CR that was created before its cluster was deregistered still carries the
// finalizers the cleanup path has to release. Once both abandon windows have
// passed, the deletion resolver therefore swallows the resolver error and hands
// back nil, instead of failing the pass and leaving the CR Terminating forever.
func TestResolveChildrenClientForDeletionUnresolvablePastBothWindowsYieldsNil(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	resolver := &fakeResolver{err: mcruntime.ErrClusterNotFound}
	deletedAt := metav1.NewTime(time.Now().Add(-AbandonAfter - time.Minute))
	seedUnresolvableSince(t, "deregistered", time.Now().Add(-AbandonAfter-time.Minute))

	got, wait := ResolveChildrenClientForDeletion(context.Background(), resolver, local,
		&commonv1.TargetClusterRefSpec{Name: "deregistered"}, deletedAt)

	g.Expect(wait).To(gomega.BeFalse())
	g.Expect(got).To(gomega.BeNil(),
		"an unresolvable target must not fall back to the management cluster: "+
			"a Delete issued there would hit unrelated objects of the same name")
}

// The window measures how long this process has been unable to resolve the
// cluster, not how long the CR has been terminating. Deciding from the deletion
// timestamp alone abandoned on the very first unresolvable pass whenever the
// operator restarted — or the provider dropped the cluster to engage a rotated
// kubeconfig — under a CR that had been terminating longer than the window,
// which a blocked OpenBao cleanup makes ordinary. The children were then left
// running on a reachable cluster with no CR to retry from: the exact race the
// window exists to prevent.
func TestResolveChildrenClientForDeletionFirstUnresolvablePassWaitsEvenPastTheDeletionWindow(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	resolver := &fakeResolver{err: mcruntime.ErrClusterNotFound}
	// Terminating for far longer than the window, and this process has never
	// failed to resolve the cluster before — an operator that just came back up.
	deletedAt := metav1.NewTime(time.Now().Add(-10 * AbandonAfter))
	t.Cleanup(func() { forgetUnresolvable("restarted-under") })

	got, wait := ResolveChildrenClientForDeletion(context.Background(), resolver, local,
		&commonv1.TargetClusterRefSpec{Name: "restarted-under"}, deletedAt)

	g.Expect(wait).To(gomega.BeTrue(),
		"a cluster this process has not yet had a chance to resolve must be waited for")
	g.Expect(got).To(gomega.BeNil())
}

// A cluster that resolves again clears its record, so a later flap gets a full
// window rather than inheriting an expired one and abandoning on its first pass.
func TestResolveChildrenClientForDeletionResolvingAgainRestartsTheWindow(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	seedUnresolvableSince(t, "flapping", time.Now().Add(-AbandonAfter-time.Minute))
	deletedAt := metav1.NewTime(time.Now().Add(-10 * AbandonAfter))

	// The cluster comes back...
	resolvable := &fakeResolver{cl: fakeCluster{c: fake.NewClientBuilder().Build()}}
	_, wait := ResolveChildrenClientForDeletion(context.Background(), resolvable, local,
		&commonv1.TargetClusterRefSpec{Name: "flapping"}, deletedAt)
	g.Expect(wait).To(gomega.BeFalse())

	// ...and drops out again, which has to start the window over.
	unresolvable := &fakeResolver{err: mcruntime.ErrClusterNotFound}
	got, wait := ResolveChildrenClientForDeletion(context.Background(), unresolvable, local,
		&commonv1.TargetClusterRefSpec{Name: "flapping"}, deletedAt)

	g.Expect(wait).To(gomega.BeTrue(),
		"the stale record of an earlier outage must not shorten the next one's window")
	g.Expect(got).To(gomega.BeNil())
}

// Cluster engagement is asynchronous, so right after an operator restart a
// registered cluster does not resolve either. Giving up on the first such pass
// would release the finalizers of every CR terminating in that window and leave
// its children running on a cluster that is perfectly reachable, with no CR left
// to retry from. Within AbandonAfter the caller is told to wait instead.
func TestResolveChildrenClientForDeletionUnresolvableInsideTheWindowWaits(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	resolver := &fakeResolver{err: mcruntime.ErrClusterNotFound}
	deletedAt := metav1.NewTime(time.Now().Add(-AbandonAfter / 2))
	t.Cleanup(func() { forgetUnresolvable("not-engaged-yet") })

	got, wait := ResolveChildrenClientForDeletion(context.Background(), resolver, local,
		&commonv1.TargetClusterRefSpec{Name: "not-engaged-yet"}, deletedAt)

	g.Expect(wait).To(gomega.BeTrue(),
		"a target that may still be engaging must be requeued for, not abandoned")
	g.Expect(got).To(gomega.BeNil())
}

func TestResolveChildrenClientForDeletionResolvableYieldsTheTargetClient(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	onTheTarget := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "there", Namespace: "openstack"}}
	remote := fake.NewClientBuilder().WithObjects(onTheTarget).Build()
	resolver := &fakeResolver{cl: fakeCluster{c: remote}}

	got, wait := ResolveChildrenClientForDeletion(context.Background(), resolver, local,
		&commonv1.TargetClusterRefSpec{Name: "remote-a"}, metav1.Now())

	g.Expect(wait).To(gomega.BeFalse())
	// The deletion path resolves through ResolveChildrenClient, so the teardown
	// gets the same marked client the projection did and recognizes the children
	// it has to delete by the same labels it stamped them with.
	g.Expect(IsRemote(got)).To(gomega.BeTrue())
	g.Expect(got.Get(context.Background(), client.ObjectKeyFromObject(onTheTarget), &corev1.ConfigMap{})).
		To(gomega.Succeed())
}

func TestResolveChildrenClientForDeletionNilRefYieldsLocal(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	resolver := &fakeResolver{cl: fakeCluster{c: fake.NewClientBuilder().Build()}}

	got, wait := ResolveChildrenClientForDeletion(context.Background(), resolver, local, nil, metav1.Now())

	g.Expect(wait).To(gomega.BeFalse())
	g.Expect(got).To(gomega.BeIdenticalTo(local))
	g.Expect(IsRemote(got)).To(gomega.BeFalse())
	g.Expect(resolver.calls).To(gomega.BeEmpty())
}

func TestResolveChildrenAPIReaderNilRefSkipsResolver(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	resolver := &fakeResolver{cl: fakeCluster{reader: fake.NewClientBuilder().Build()}}

	got, err := ResolveChildrenAPIReader(context.Background(), resolver, local, nil)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.BeIdenticalTo(client.Reader(local)))
	g.Expect(resolver.calls).To(gomega.BeEmpty())
}

// A reconciler built without a manager passes a nil reader. It has to come back
// unchanged rather than as an error, so the caller can fall back to its
// children client instead of failing the pass.
func TestResolveChildrenAPIReaderNilLocalStaysNil(t *testing.T) {
	g := gomega.NewWithT(t)

	got, err := ResolveChildrenAPIReader(context.Background(), nil, nil,
		&commonv1.TargetClusterRefSpec{Name: "remote-a"})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.BeNil())
}

// The latch that reads through this reader must never be decided from the
// target cluster's cache: a relist or a just-issued write can leave it behind
// the API server, and the latch would then reverse itself.
func TestResolveChildrenAPIReaderReturnsTargetClusterAPIReader(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	cached := fake.NewClientBuilder().Build()
	direct := fake.NewClientBuilder().Build()
	resolver := &fakeResolver{cl: fakeCluster{c: cached, reader: direct}}

	got, err := ResolveChildrenAPIReader(context.Background(), resolver, local,
		&commonv1.TargetClusterRefSpec{Name: "remote-a"})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.BeIdenticalTo(client.Reader(direct)))
	g.Expect(got).NotTo(gomega.BeIdenticalTo(client.Reader(cached)),
		"the cache-backed client must not be used as the latch source")
	g.Expect(resolver.calls).To(gomega.Equal([]mcruntime.ClusterName{"remote-a"}))
}

func TestResolveChildrenAPIReaderPropagatesResolverError(t *testing.T) {
	g := gomega.NewWithT(t)

	local := fake.NewClientBuilder().Build()
	resolver := &fakeResolver{err: mcruntime.ErrClusterNotFound}

	got, err := ResolveChildrenAPIReader(context.Background(), resolver, local,
		&commonv1.TargetClusterRefSpec{Name: "unregistered"})

	g.Expect(got).To(gomega.BeNil())
	g.Expect(errors.Is(err, mcruntime.ErrClusterNotFound)).To(gomega.BeTrue())
}
