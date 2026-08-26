// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// watchOwnerKind is the owner kind every test below maps back to. Barbican
// stands in for the other operator projecting into the same target namespace.
const watchOwnerKind = "Keystone"

// ownerRequest is what a fully stamped child has to map to: the owner on the
// management cluster, never the cluster the child was seen on.
var ownerRequest = reconcile.Request{
	NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "example"},
}

// watchChildLabels is a complete ownership stamp, the one Claim writes onto a
// child of the owner named by ownerRequest.
func watchChildLabels() map[string]string {
	return map[string]string{
		OwnerKindLabel:      watchOwnerKind,
		OwnerNameLabel:      "example",
		OwnerNamespaceLabel: "openstack",
	}
}

// watchChild is an object on a target cluster carrying labels, in the namespace
// a child of ownerRequest's owner lands in. Its own name differs from its
// owner's on purpose: a map function reading the name off the object rather
// than off the labels would produce the wrong request.
func watchChild(labels map[string]string) *corev1.Secret {
	return watchChildIn("openstack", labels)
}

// watchChildIn is watchChild in a namespace of its own, for the case where the
// object's namespace and the namespace its labels claim disagree.
func watchChildIn(namespace string, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "example-config",
		Namespace: namespace,
		Labels:    labels,
	}}
}

// httpRouteGVK is a kind not every target cluster installs, which is the case
// ClusterServesKind exists for.
var httpRouteGVK = schema.GroupVersionKind{
	Group:   "gateway.networking.k8s.io",
	Version: "v1",
	Kind:    "HTTPRoute",
}

// mapperCluster answers about the kinds it serves and nothing else. Embedding
// the interface leaves every other method nil, so a filter reaching past the
// RESTMapper panics rather than passing quietly.
type mapperCluster struct {
	cluster.Cluster
	mapper meta.RESTMapper
}

func (m mapperCluster) GetRESTMapper() meta.RESTMapper { return m.mapper }

// erroringMapper fails every mapping with an error that is not a no-match: the
// throttled or briefly unreachable target cluster, or the one whose aggregated
// discovery an unrelated broken APIService fails, whose answer says nothing
// about whether it serves the kind.
type erroringMapper struct {
	meta.RESTMapper
}

func (erroringMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, errors.New("etcdserver: request timed out")
}

// watchTargetsFor builds the TargetClusterFunc a remote leg gates on over the
// CR stand-ins the fake client holds. A ConfigMap plays the CR — this package
// knows no service CR — and its targetCluster key plays spec.targetClusterRef,
// absent for a CR that keeps its children on the management cluster.
func watchTargetsFor(t *testing.T, crs ...client.Object) TargetClusterFunc {
	t.Helper()

	c := fake.NewClientBuilder().WithScheme(ownershipScheme(t)).WithObjects(crs...).Build()
	return TargetClusterOf(c, func(cr *corev1.ConfigMap) *commonv1.TargetClusterRefSpec {
		name, named := cr.Data["targetCluster"]
		if !named {
			return nil
		}
		return &commonv1.TargetClusterRefSpec{Name: name}
	})
}

// watchOwnerCR is the CR stand-in ownerRequest names, projecting onto cluster.
// An empty cluster is a CR that names none.
func watchOwnerCR(cluster string) *corev1.ConfigMap {
	cr := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      ownerRequest.Name,
		Namespace: ownerRequest.Namespace,
	}}
	if cluster != "" {
		cr.Data = map[string]string{"targetCluster": cluster}
	}
	return cr
}

// watchTargetSetsFor is watchTargetsFor for the set-valued gate, over CR
// stand-ins whose targetClusters key lists the names as one comma-separated
// value. A CR without the key names none.
func watchTargetSetsFor(t *testing.T, crs ...client.Object) TargetClustersFunc {
	t.Helper()

	c := fake.NewClientBuilder().WithScheme(ownershipScheme(t)).WithObjects(crs...).Build()
	return func(ctx context.Context, key types.NamespacedName) ([]string, error) {
		cr := &corev1.ConfigMap{}
		if err := c.Get(ctx, key, cr); err != nil {
			return nil, err
		}
		list, named := cr.Data["targetClusters"]
		if !named {
			return nil, nil
		}
		return strings.Split(list, ","), nil
	}
}

// watchOwnerCRAmong is watchOwnerCR for a CR placing services on several target
// clusters at once. No clusters is a CR that names none.
func watchOwnerCRAmong(clusters ...string) *corev1.ConfigMap {
	cr := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      ownerRequest.Name,
		Namespace: ownerRequest.Namespace,
	}}
	if len(clusters) > 0 {
		cr.Data = map[string]string{"targetClusters": strings.Join(clusters, ",")}
	}
	return cr
}

// mcQueue is the workqueue a multicluster event handler writes into.
type mcQueue = workqueue.TypedRateLimitingInterface[mcreconcile.Request]

// newMCQueue builds the real controller workqueue rather than a recording fake,
// so the deduplication a pinned cluster name buys is what the assertions read.
func newMCQueue() mcQueue {
	return workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[mcreconcile.Request]())
}

// A remote child carries no owner reference, so the labels are the only link
// back to the CR. This is the mapping the whole watch leg exists to make.
func TestChildToOwnerMapsALabelledChildToItsOwner(t *testing.T) {
	g := gomega.NewWithT(t)

	requests := ChildToOwner(watchOwnerKind)(context.Background(), watchChild(watchChildLabels()))

	g.Expect(requests).To(gomega.ConsistOf(ownerRequest))
}

// The children of every CR projecting into a shared target namespace arrive
// through the same watch, so the map function is constantly asked about objects
// it must say nothing about. A half-stamped object is one of them: a request
// naming an empty name or namespace would reconcile whatever CR answered to it.
func TestChildToOwnerIgnoresWhatItDoesNotOwn(t *testing.T) {
	without := func(key string) map[string]string {
		labels := watchChildLabels()
		delete(labels, key)
		return labels
	}
	anotherOwner := watchChildLabels()
	anotherOwner[OwnerKindLabel] = "Barbican"

	for name, labels := range map[string]map[string]string{
		"nothing stamped it at all": nil,
		"no owner kind":             without(OwnerKindLabel),
		"no owner name":             without(OwnerNameLabel),
		"no owner namespace":        without(OwnerNamespaceLabel),
		"another operator's child":  anotherOwner,
	} {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			requests := ChildToOwner(watchOwnerKind)(context.Background(), watchChild(labels))

			g.Expect(requests).To(gomega.BeEmpty())
		})
	}
}

// The operator stamps a child in its owner's own namespace and nowhere else, so
// an object claiming an owner somewhere else was stamped by somebody else. The
// labels are forgeable by anyone who can create an object, and a target cluster
// has namespaces this operator never projects into; trusting them there would
// let a principal with create access in any of them name any CR in the fleet and
// have it reconciled on their timing.
func TestChildToOwnerIgnoresAChildClaimingAnOwnerInAnotherNamespace(t *testing.T) {
	g := gomega.NewWithT(t)

	forged := watchChildIn("someone-elses-ns", watchChildLabels())

	requests := ChildToOwner(watchOwnerKind)(context.Background(), forged)

	g.Expect(requests).To(gomega.BeEmpty(),
		"a labelled object outside its claimed owner's namespace is not a child this operator wrote")
}

// The event arrives from a provider cluster and the CR it names lives on the
// management cluster, so the request has to come out with an empty cluster
// name. Anything else gives the workqueue a second key for the same CR and lets
// a child event and a CR event reconcile it concurrently.
func TestLocalRequestsPinsEveryEventToTheLocalCluster(t *testing.T) {
	child := watchChild(watchChildLabels())
	ctx := context.Background()

	// A nil cluster proves the factory reaches for neither of its arguments,
	// and a non-empty cluster name that it does not carry one through.
	eventHandler := LocalRequests(ChildToOwner(watchOwnerKind))("remote-a", nil)

	for name, fire := range map[string]func(mcQueue){
		"create": func(q mcQueue) {
			eventHandler.Create(ctx, event.TypedCreateEvent[client.Object]{Object: child}, q)
		},
		"update": func(q mcQueue) {
			eventHandler.Update(ctx, event.TypedUpdateEvent[client.Object]{ObjectOld: child, ObjectNew: child}, q)
		},
		"delete": func(q mcQueue) {
			eventHandler.Delete(ctx, event.TypedDeleteEvent[client.Object]{Object: child}, q)
		},
		"generic": func(q mcQueue) {
			eventHandler.Generic(ctx, event.TypedGenericEvent[client.Object]{Object: child}, q)
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			queue := newMCQueue()
			defer queue.ShutDown()

			fire(queue)

			// The update case fires the map function twice, once per object,
			// so one item also says the two collapsed onto one key.
			g.Expect(queue.Len()).To(gomega.Equal(1))

			got, _ := queue.Get()
			g.Expect(got.Request).To(gomega.Equal(ownerRequest))
			g.Expect(got.ClusterName).To(gomega.BeEmpty(),
				"a request naming the cluster the event came from would be a second workqueue key for one CR")
		})
	}
}

// Most events on a shared target namespace map to nothing, and the handler has
// to leave the queue alone rather than enqueue an empty request that would
// reconcile a CR with no name.
func TestLocalRequestsEnqueuesNothingWhenNothingMaps(t *testing.T) {
	g := gomega.NewWithT(t)

	queue := newMCQueue()
	defer queue.ShutDown()

	LocalRequests(ChildToOwner(watchOwnerKind))("remote-a", nil).
		Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: watchChild(nil)}, queue)

	g.Expect(queue.Len()).To(gomega.BeZero())
}

// Every input mapper the converted controllers wrap fans out — one Secret or
// one SecretStore names as many CRs as reference it — so the conversion from
// reconcile.Request to the multicluster one has to carry all of them, not just
// the first.
func TestLocalRequestsEnqueuesEveryRequestAFanOutMapperNames(t *testing.T) {
	g := gomega.NewWithT(t)

	second := reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "second"},
	}
	fanOut := func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{ownerRequest, second}
	}

	queue := newMCQueue()
	defer queue.ShutDown()

	LocalRequests(fanOut)("remote-a", nil).
		Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: watchChild(nil)}, queue)

	g.Expect(queue.Len()).To(gomega.Equal(2), "a mapper naming several CRs must enqueue every one of them")

	var got []reconcile.Request
	for range 2 {
		item, _ := queue.Get()
		g.Expect(item.ClusterName).To(gomega.BeEmpty())
		got = append(got, item.Request)
	}
	g.Expect(got).To(gomega.ConsistOf(ownerRequest, second))
}

// A leg is engaged on every registered cluster, not on the ones a CR names, so
// the cluster an event arrived from is the half of the ownership claim the
// object itself cannot carry. Everything on a target cluster — the ownership
// labels included — is writable by whoever can write to that cluster, so
// without this gate anyone with create access in one shared namespace could
// name any CR in the fleet and have it reconciled on their timing, indefinitely
// and without the rate limiter noticing (its backoff applies to items that
// errored, and these reconciles succeed).
func TestRemoteRequestsDropsEventsFromAClusterTheCRDoesNotProjectOnto(t *testing.T) {
	child := watchChild(watchChildLabels())

	for name, tc := range map[string]struct {
		cr      *corev1.ConfigMap
		enqueue bool
	}{
		"the CR projects onto this cluster":    {cr: watchOwnerCR("remote-a"), enqueue: true},
		"the CR projects onto another cluster": {cr: watchOwnerCR("remote-b")},
		"the CR keeps its children local":      {cr: watchOwnerCR("")},
	} {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			queue := newMCQueue()
			defer queue.ShutDown()

			RemoteRequests(ChildToOwner(watchOwnerKind), watchTargetsFor(t, tc.cr))("remote-a", nil).
				Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: child}, queue)

			if !tc.enqueue {
				g.Expect(queue.Len()).To(gomega.BeZero(),
					"an event from a cluster the CR does not project onto must not reconcile it")
				return
			}

			g.Expect(queue.Len()).To(gomega.Equal(1))
			got, _ := queue.Get()
			g.Expect(got.Request).To(gomega.Equal(ownerRequest))
			g.Expect(got.ClusterName).To(gomega.BeEmpty())
		})
	}
}

// The labels are the forgeable half of the pair and the CR is the trusted one,
// so an event whose CR cannot be read is dropped rather than admitted. A child
// outliving the CR that projected it is the ordinary case.
func TestRemoteRequestsDropsEventsNamingACRThatDoesNotExist(t *testing.T) {
	g := gomega.NewWithT(t)

	queue := newMCQueue()
	defer queue.ShutDown()

	RemoteRequests(ChildToOwner(watchOwnerKind), watchTargetsFor(t))("remote-a", nil).
		Create(context.Background(), event.TypedCreateEvent[client.Object]{
			Object: watchChild(watchChildLabels()),
		}, queue)

	g.Expect(queue.Len()).To(gomega.BeZero())
}

// The gate is applied per request, not per event: an input mapper names as many
// CRs as reference the object, and they need not all project onto the cluster
// the event came from.
func TestRemoteRequestsGatesEveryRequestOfAFanOutMapperOnItsOwnCR(t *testing.T) {
	g := gomega.NewWithT(t)

	elsewhere := reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "second"},
	}
	fanOut := func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{ownerRequest, elsewhere}
	}
	secondCR := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: elsewhere.Name, Namespace: elsewhere.Namespace},
		Data:       map[string]string{"targetCluster": "remote-b"},
	}

	queue := newMCQueue()
	defer queue.ShutDown()

	RemoteRequests(fanOut, watchTargetsFor(t, watchOwnerCR("remote-a"), secondCR))("remote-a", nil).
		Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: watchChild(nil)}, queue)

	g.Expect(queue.Len()).To(gomega.Equal(1))
	got, _ := queue.Get()
	g.Expect(got.Request).To(gomega.Equal(ownerRequest),
		"only the CR projecting onto the cluster the event came from may be enqueued")
}

// A CR placing several services on target clusters of their own is claimed by
// each of those clusters and by no other, so the gate is a membership test. The
// two ways of not being a member are the same answer: a cluster the CR never
// names, and a CR that names no cluster at all.
func TestRemoteRequestsAmongDropsEventsFromAClusterOutsideTheCRsTargets(t *testing.T) {
	child := watchChild(watchChildLabels())

	for name, tc := range map[string]struct {
		cr      *corev1.ConfigMap
		enqueue bool
	}{
		"the CR names this cluster among its targets": {cr: watchOwnerCRAmong("remote-a", "remote-b"), enqueue: true},
		"the CR names only other clusters":            {cr: watchOwnerCRAmong("remote-b", "remote-c")},
		"the CR names no target cluster at all":       {cr: watchOwnerCRAmong()},
	} {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			queue := newMCQueue()
			defer queue.ShutDown()

			RemoteRequestsAmong(ChildToOwner(watchOwnerKind), watchTargetSetsFor(t, tc.cr))("remote-a", nil).
				Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: child}, queue)

			if !tc.enqueue {
				g.Expect(queue.Len()).To(gomega.BeZero(),
					"an event from a cluster outside the CR's targets must not reconcile it")
				return
			}

			g.Expect(queue.Len()).To(gomega.Equal(1))
			got, _ := queue.Get()
			g.Expect(got.Request).To(gomega.Equal(ownerRequest))
			g.Expect(got.ClusterName).To(gomega.BeEmpty())
		})
	}
}

// The leg is engaged once per cluster, so one CR spread over two of them is
// gated separately on each and has to pass both times. A gate keeping only the
// first name would leave every service but one without drift correction, which
// is the whole reason the set-valued sibling exists.
func TestRemoteRequestsAmongEnqueuesForEveryClusterTheCRNames(t *testing.T) {
	targets := watchTargetSetsFor(t, watchOwnerCRAmong("cluster-a", "cluster-b"))
	child := watchChild(watchChildLabels())

	for _, engaged := range []mcruntime.ClusterName{"cluster-a", "cluster-b"} {
		t.Run(string(engaged), func(t *testing.T) {
			g := gomega.NewWithT(t)

			queue := newMCQueue()
			defer queue.ShutDown()

			RemoteRequestsAmong(ChildToOwner(watchOwnerKind), targets)(engaged, nil).
				Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: child}, queue)

			g.Expect(queue.Len()).To(gomega.Equal(1))
			got, _ := queue.Get()
			g.Expect(got.Request).To(gomega.Equal(ownerRequest))
			g.Expect(got.ClusterName).To(gomega.BeEmpty())
		})
	}
}

// The labels are the forgeable half of the pair and the CR is the trusted one,
// so an event whose CR cannot be read is dropped rather than admitted. NotFound
// is the ordinary case, a child outliving the CR that projected it, and stays
// silent; any other error is dropped just as hard, because an unreadable CR
// names no clusters that anyone may act on.
func TestRemoteRequestsAmongDropsEventsWhoseCRCannotBeRead(t *testing.T) {
	unreadable := func(context.Context, types.NamespacedName) ([]string, error) {
		return nil, errors.New("etcdserver: request timed out")
	}

	for name, targets := range map[string]TargetClustersFunc{
		"the CR does not exist":   watchTargetSetsFor(t),
		"the lookup itself fails": unreadable,
	} {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			queue := newMCQueue()
			defer queue.ShutDown()

			RemoteRequestsAmong(ChildToOwner(watchOwnerKind), targets)("remote-a", nil).
				Create(context.Background(), event.TypedCreateEvent[client.Object]{
					Object: watchChild(watchChildLabels()),
				}, queue)

			g.Expect(queue.Len()).To(gomega.BeZero())
		})
	}
}

// The lookup every remote leg gates on reads the ref off the CR itself, which
// is the only party to the claim a target cluster cannot write to.
func TestTargetClusterOfReadsTheRefOffTheCR(t *testing.T) {
	g := gomega.NewWithT(t)

	local := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "openstack"}}
	targets := watchTargetsFor(t, watchOwnerCR("remote-b"), local)
	ctx := context.Background()

	named, err := targets(ctx, ownerRequest.NamespacedName)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(named).To(gomega.Equal("remote-b"))

	unnamed, err := targets(ctx, types.NamespacedName{Namespace: "openstack", Name: "local"})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(unnamed).To(gomega.BeEmpty(),
		"a CR naming no target cluster must match no engaged cluster")

	_, err = targets(ctx, types.NamespacedName{Namespace: "openstack", Name: "gone"})
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
}

// The adapter exists so a converted controller keeps its Reconcile signature.
// It therefore has to be transparent in both directions: the inner reconciler
// sees the request without the cluster name, and its result and error come back
// exactly as it returned them.
func TestLocalReconcilerForwardsTheRequestAndItsOutcome(t *testing.T) {
	g := gomega.NewWithT(t)

	failed := errors.New("the sub-reconciler chain stopped")
	var seen []reconcile.Request
	inner := reconcile.Func(func(_ context.Context, req reconcile.Request) (reconcile.Result, error) {
		seen = append(seen, req)
		return reconcile.Result{RequeueAfter: 42 * time.Second}, failed
	})

	request := mcreconcile.Request{Request: ownerRequest, ClusterName: "remote-a"}
	result, err := LocalReconciler(inner).Reconcile(context.Background(), request)

	g.Expect(seen).To(gomega.ConsistOf(ownerRequest))
	g.Expect(result).To(gomega.Equal(reconcile.Result{RequeueAfter: 42 * time.Second}))
	g.Expect(errors.Is(err, failed)).To(gomega.BeTrue())
}

// A target cluster is free to serve only part of the kind list an operator
// projects. The filter is what keeps that from failing the cluster's whole
// engagement, so it has to answer per cluster and not once for all of them.
func TestClusterServesKind(t *testing.T) {
	g := gomega.NewWithT(t)

	serving := meta.NewDefaultRESTMapper([]schema.GroupVersion{httpRouteGVK.GroupVersion()})
	serving.Add(httpRouteGVK, meta.RESTScopeNamespace)

	filter := ClusterServesKind(httpRouteGVK)

	g.Expect(filter("remote-a", mapperCluster{mapper: serving})).To(gomega.BeTrue())
	g.Expect(filter("remote-b", mapperCluster{mapper: meta.NewDefaultRESTMapper(nil)})).To(gomega.BeFalse(),
		"a cluster without the CRD must lose this leg alone")
	// A cluster that has not built a mapper is as unusable for this leg as one
	// missing the CRD, and must not panic the engagement.
	g.Expect(filter("remote-c", mapperCluster{})).To(gomega.BeFalse())
}

// An error that is not a no-match says nothing about the kind, and the filter
// has to answer anyway. It skips the leg, because engaging on a kind the
// cluster does not serve fails the engagement of the whole cluster, and a
// discovery failure that outlives the provider's backoff — an aggregated
// APIService stuck on Available=False fails the group lookup for every group
// the mapper has not cached — would make that permanent: every CR projecting
// onto the cluster would lose every watch it has, over one optional kind.
// Skipping costs this one leg until the cluster is engaged again.
func TestClusterServesKindSkipsTheLegOnADiscoveryFailure(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(ClusterServesKind(httpRouteGVK)("remote-d", mapperCluster{mapper: erroringMapper{}})).
		To(gomega.BeFalse(), "an ambiguous discovery answer must cost this leg alone, not the whole cluster")
}

// Each of the three options a remote leg carries is silent on its own: the
// first two left off watch the wrong side of the fleet, and the filter left off
// fails the whole engagement of a cluster missing the kind rather than just this
// leg. Nothing else in the tree would notice one going missing.
func TestRemoteWatchOptionsCarriesEveryOptionARemoteLegNeeds(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(RemoteWatchOptions(httpRouteGVK)).To(gomega.HaveLen(3),
		"a remote leg needs all three of: no local cluster, provider clusters, and the kind filter")
}

// The kind driving the remote leg's cluster filter is resolved from the object
// the leg watches, so the two cannot describe different kinds. A kind the
// scheme does not know is a wiring mistake and has to fail setup rather than
// register a leg filtered on nothing.
func TestAddInputWatchResolvesTheWatchedKindThroughTheScheme(t *testing.T) {
	g := gomega.NewWithT(t)

	got, err := AddInputWatch(mcbuilder.ControllerManagedBy(nil), ownershipScheme(t), watchTargetsFor(t),
		&corev1.Secret{}, ChildToOwner(watchOwnerKind))

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).NotTo(gomega.BeNil())

	got, err = AddInputWatch(mcbuilder.ControllerManagedBy(nil), runtime.NewScheme(), watchTargetsFor(t),
		&corev1.Secret{}, ChildToOwner(watchOwnerKind))

	g.Expect(got).To(gomega.BeNil())
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("resolving input kind for remote watch")))
}

// An input nobody narrows is one thing; a Node is another. The chassis watches
// Nodes for a label change, and a predicate that reached the local leg alone
// would leave the remote one waking every CR in the fleet on every kubelet
// heartbeat, which is why extra goes to both.
//
// What an assertion can reach here is the call and its outcome. The builder
// keeps its registered watches in an unexported field and offers no accessor,
// so no test in this package reads an option back off a leg (see
// TestRemoteWatchOptionsCarriesEveryOptionARemoteLegNeeds, which counts the
// remote option set instead). The options are applied at registration, though,
// so an option the builder cannot apply fails here rather than in production.
func TestAddInputWatch_ExtraOptionsReachBothLegs(t *testing.T) {
	g := gomega.NewWithT(t)

	got, err := AddInputWatch(mcbuilder.ControllerManagedBy(nil), ownershipScheme(t), watchTargetsFor(t),
		&corev1.Node{}, ChildToOwner(watchOwnerKind),
		mcbuilder.WithPredicates(predicate.LabelChangedPredicate{}))

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).NotTo(gomega.BeNil())

	// The failure the resolution owns keeps its diagnosis with an extra option
	// in hand: a leg is never registered on a kind the scheme cannot name, no
	// matter what is passed beside it.
	got, err = AddInputWatch(mcbuilder.ControllerManagedBy(nil), runtime.NewScheme(), watchTargetsFor(t),
		&corev1.Node{}, ChildToOwner(watchOwnerKind),
		mcbuilder.WithPredicates(predicate.LabelChangedPredicate{}))

	g.Expect(got).To(gomega.BeNil())
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("resolving input kind for remote watch")))

	// Every caller in the tree passes none, and the option set each leg carries
	// on its own has to be unchanged for them.
	got, err = AddInputWatch(mcbuilder.ControllerManagedBy(nil), ownershipScheme(t), watchTargetsFor(t),
		&corev1.Secret{}, ChildToOwner(watchOwnerKind))

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).NotTo(gomega.BeNil())
}

// A CR that projects nothing onto a target cluster still runs through this, and
// has to come back with the builder it handed in: a watch leg registered for an
// empty list would engage provider clusters for nothing.
func TestAddRemoteChildWatchesWithoutKindsLeavesTheBuilderAlone(t *testing.T) {
	for name, kinds := range map[string][]schema.GroupVersionKind{
		"nil kinds":   nil,
		"empty kinds": {},
	} {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			builder := mcbuilder.ControllerManagedBy(nil)

			// A nil extra map is the ordinary call: most operators need no
			// per-kind options beyond the three this sets itself.
			got, err := AddRemoteChildWatches(builder, ownershipScheme(t), testOwner(), watchTargetsFor(t), kinds, nil)

			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(got).To(gomega.BeIdenticalTo(builder))
		})
	}
}

// The owner kind the child labels are matched against is the same value the
// write side stamps, so it is resolved from the owner object rather than typed
// out again. An owner the scheme does not know would otherwise be matched
// against an empty kind, and every child would map to nothing.
func TestAddRemoteChildWatchesRejectsAnOwnerTheSchemeDoesNotKnow(t *testing.T) {
	g := gomega.NewWithT(t)

	got, err := AddRemoteChildWatches(mcbuilder.ControllerManagedBy(nil), runtime.NewScheme(), testOwner(),
		watchTargetsFor(t), []schema.GroupVersionKind{corev1.SchemeGroupVersion.WithKind("Secret")}, nil)

	g.Expect(got).To(gomega.BeNil())
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("resolving GVK for owner openstack/example")))
}

// The extra options are keyed by kind and applied inside the loop over kinds, so
// a key naming a kind outside that list is simply never read. Dropping a
// predicate silently is the worst outcome available: setup succeeds, every test
// passes, and the only symptom is reconcile churn in production.
func TestAddRemoteChildWatchesRejectsExtraOptionsForAKindItDoesNotWatch(t *testing.T) {
	g := gomega.NewWithT(t)

	got, err := AddRemoteChildWatches(mcbuilder.ControllerManagedBy(nil), ownershipScheme(t), testOwner(),
		watchTargetsFor(t), []schema.GroupVersionKind{corev1.SchemeGroupVersion.WithKind("Secret")},
		map[schema.GroupVersionKind][]mcbuilder.WatchesOption{
			corev1.SchemeGroupVersion.WithKind("ConfigMap"): {mcbuilder.WithEngageWithLocalCluster(true)},
		})

	g.Expect(got).To(gomega.BeNil())
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("is not a projected child kind")))
}

// The kind list is written by hand next to the operator's sweep list, so a kind
// its scheme never registered is a wiring mistake. It has to surface while the
// controller is being set up, not as a watch that quietly never fires.
func TestAddRemoteChildWatchesRejectsAKindTheSchemeDoesNotKnow(t *testing.T) {
	g := gomega.NewWithT(t)

	got, err := AddRemoteChildWatches(mcbuilder.ControllerManagedBy(nil), ownershipScheme(t), testOwner(),
		watchTargetsFor(t), []schema.GroupVersionKind{httpRouteGVK}, nil)

	g.Expect(got).To(gomega.BeNil())
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("building remote watch object for")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(httpRouteGVK.String())))
	// The scheme's own diagnosis survives the wrap, so the message still names
	// the kind nobody registered.
	g.Expect(runtime.IsNotRegisteredError(errors.Unwrap(err))).To(gomega.BeTrue())
}
