// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi/computeapitest"
)

// apiPass is a pass whose NovaRef step resolved the fake Nova.
func apiPass(api *computeapitest.Fake, siblings ...novav1alpha1.NovaCompute) *novaComputePass {
	return &novaComputePass{
		doer:        api,
		keystoneURL: "http://keystone.openstack.svc.cluster.local:5000/v3",
		computeURL:  "http://nova.openstack.svc.cluster.local:8774",
		siblings:    siblings,
		handover:    map[string]bool{},
	}
}

// runAggregates runs the Aggregates step for cr against api.
func runAggregates(t *testing.T, api *computeapitest.Fake, cr *novav1alpha1.NovaCompute,
	siblings ...novav1alpha1.NovaCompute,
) (ctrl.Result, *novaComputePass, *record.FakeRecorder) {
	t.Helper()
	r := newNovaComputeTestReconciler(api, cr)
	pass := apiPass(api, siblings...)
	result, err := r.reconcileNovaComputeAggregates(context.Background(), cr, pass)
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	return result, pass, r.Recorder.(*record.FakeRecorder)
}

// activePool is the fixture pool holding the given nodes as Active.
func activePool(nodes ...novav1alpha1.NovaComputeNodeStatus) *novav1alpha1.NovaCompute {
	cr := validNovaCompute()
	cr.Status.Nodes = nodes
	return cr
}

func activeIn(name, zone string) novav1alpha1.NovaComputeNodeStatus {
	return novav1alpha1.NovaComputeNodeStatus{Name: name, Phase: novav1alpha1.NovaComputeNodeActive, Zone: zone}
}

func TestReconcileNovaComputeAggregates_CreatesAndMarks(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := activePool(activeIn(testNodeName, testZone))

	result, pass, rec := runAggregates(t, api, cr)

	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(pass.aggregatesEnsured).To(BeTrue())
	zone, ok := api.Aggregate(testZone)
	g.Expect(ok).To(BeTrue())
	g.Expect(zone.AvailabilityZone).To(Equal(testZone))
	g.Expect(zone.Metadata).To(HaveKeyWithValue(aggregateMarkerKey, testPoolMarker))
	tenant, ok := api.Aggregate(tenantFilterTestsAggregate)
	g.Expect(ok).To(BeTrue())
	g.Expect(tenant.AvailabilityZone).To(BeEmpty())
	g.Expect(tenant.Metadata).To(HaveKeyWithValue(aggregateMarkerKey, testPoolMarker))

	cond := novaComputeCondition(cr, conditionTypeAggregatesReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNodesOutsideZoneAggregate),
		"the aggregates this pass created hold no host yet")
	g.Expect(collectEvents(rec)).To(ConsistOf(
		ContainSubstring("Normal AggregateCreated Created host aggregate az1 in availability zone az1"),
		ContainSubstring("Normal AggregateCreated Created host aggregate tenant_filter_tests"),
	))

	// A second pass finds both and creates nothing.
	before := len(api.CallsTo(http.MethodPost, "/v2.1/os-aggregates"))
	runAggregates(t, api, cr)
	g.Expect(api.CallsTo(http.MethodPost, "/v2.1/os-aggregates")).To(HaveLen(before))
}

// TestReconcileNovaComputeAggregates_ReportsANodeOutsideItsZone pins the
// membership report: the aggregates are in place, the Active node is in none
// of its zone, and AggregatesReady stays True without an event, as the pool
// does not own the membership.
func TestReconcileNovaComputeAggregates_ReportsANodeOutsideItsZone(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate(testZone, testZone, []string{}, nil)
	api.AddAggregate(tenantFilterTestsAggregate, "", []string{}, nil)
	cr := activePool(activeIn("node-a", testZone))

	result, pass, rec := runAggregates(t, api, cr)

	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(pass.aggregatesEnsured).To(BeTrue(), "the step's own work, the aggregates, is done")
	cond := novaComputeCondition(cr, conditionTypeAggregatesReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNodesOutsideZoneAggregate))
	g.Expect(cond.Message).To(Equal(
		"Host aggregates in place: az1, tenant_filter_tests; Active nodes in no aggregate of their zone: node-a (az1)"))
	g.Expect(collectEvents(rec)).To(BeEmpty())
}

func TestReconcileNovaComputeAggregates_NodeInItsZoneAggregate(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate(testZone, testZone, []string{"node-a"}, nil)
	api.AddAggregate(tenantFilterTestsAggregate, "", []string{}, nil)
	cr := activePool(activeIn("node-a", testZone))

	runAggregates(t, api, cr)

	cond := novaComputeCondition(cr, conditionTypeAggregatesReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAggregatesEnsured))
	g.Expect(cond.Message).To(Equal("Host aggregates in place: az1, tenant_filter_tests"))
}

// TestReconcileNovaComputeAggregates_AnyAggregateOfTheZoneCounts pins that
// Nova derives a host's zone from every aggregate it is in, so membership in
// another aggregate carrying the zone places the node in it.
func TestReconcileNovaComputeAggregates_AnyAggregateOfTheZoneCounts(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate(testZone, testZone, []string{}, nil)
	api.AddAggregate("az1-extra", testZone, []string{"node-a"}, nil)
	cr := activePool(activeIn("node-a", testZone))

	runAggregates(t, api, cr)

	g.Expect(novaComputeCondition(cr, conditionTypeAggregatesReady).Reason).To(Equal(conditionReasonAggregatesEnsured))
}

// TestReconcileNovaComputeAggregates_TenantFilterTestsIsNoZone pins that the
// zone-less tenant_filter_tests places a host in no availability zone.
func TestReconcileNovaComputeAggregates_TenantFilterTestsIsNoZone(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate(testZone, testZone, []string{}, nil)
	api.AddAggregate(tenantFilterTestsAggregate, "", []string{"node-a"}, nil)
	cr := activePool(activeIn("node-a", testZone))

	runAggregates(t, api, cr)

	cond := novaComputeCondition(cr, conditionTypeAggregatesReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonNodesOutsideZoneAggregate))
	g.Expect(cond.Message).To(HaveSuffix("node-a (az1)"))
}

// TestReconcileNovaComputeAggregates_SkipsPendingAndDrainingNodes pins that
// only Active nodes are checked: openstack-hypervisor-operator onboards only
// mapped hosts, and a Draining node is leaving the pool.
func TestReconcileNovaComputeAggregates_SkipsPendingAndDrainingNodes(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := activePool(
		novav1alpha1.NovaComputeNodeStatus{Name: "node-a", Phase: novav1alpha1.NovaComputeNodePending, Zone: testZone},
		novav1alpha1.NovaComputeNodeStatus{Name: "node-b", Phase: novav1alpha1.NovaComputeNodeDraining, Zone: testZone},
	)

	_, pass, _ := runAggregates(t, api, cr)

	g.Expect(pass.aggregatesEnsured).To(BeTrue())
	cond := novaComputeCondition(cr, conditionTypeAggregatesReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonAggregatesEnsured))
	g.Expect(cond.Message).To(Equal("Host aggregates in place: az1, tenant_filter_tests"))
}

func TestReconcileNovaComputeAggregates_EmptyPool(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := activePool()

	_, pass, _ := runAggregates(t, api, cr)

	g.Expect(pass.aggregatesEnsured).To(BeTrue())
	cond := novaComputeCondition(cr, conditionTypeAggregatesReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAggregatesEnsured))
	g.Expect(cond.Message).To(Equal("Host aggregates in place: tenant_filter_tests"))
}

// TestReconcileNovaComputeAggregates_BoundsTheNodesNamed pins that a message
// names at most maxNamedNodes nodes and counts the rest, so a large pool stays
// within the CRD's 32768 bytes for a condition message.
func TestReconcileNovaComputeAggregates_BoundsTheNodesNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		zone string
		want string
	}{
		{name: "nodes outside their zone's aggregate", zone: testZone, want: "node-19 (az1) and 1 more"},
		{name: "nodes without a zone", want: "node-19 and 1 more"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			var nodes []novav1alpha1.NovaComputeNodeStatus
			for i := range maxNamedNodes + 1 {
				nodes = append(nodes, activeIn(fmt.Sprintf("node-%02d", i), tc.zone))
			}
			cr := activePool(nodes...)

			runAggregates(t, computeapitest.New(), cr)

			message := novaComputeCondition(cr, conditionTypeAggregatesReady).Message
			g.Expect(message).To(HaveSuffix(tc.want))
			g.Expect(message).NotTo(ContainSubstring("node-20"))
		})
	}
}

// TestReconcileNovaComputeAggregates_EarlierReasonsWin pins the precedence: a
// node without a zone and a zone mismatch are failures of the step's own work
// and outrank the membership report.
func TestReconcileNovaComputeAggregates_EarlierReasonsWin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(api *computeapitest.Fake)
		nodes []novav1alpha1.NovaComputeNodeStatus
		want  string
	}{
		{
			name:  "a node without a zone",
			setup: func(*computeapitest.Fake) {},
			nodes: []novav1alpha1.NovaComputeNodeStatus{activeIn("node-a", testZone), activeIn("node-2", "")},
			want:  conditionReasonNodesWithoutZone,
		},
		{
			name:  "an aggregate in another zone",
			setup: func(api *computeapitest.Fake) { api.AddAggregate(testZone, "az2", nil, nil) },
			nodes: []novav1alpha1.NovaComputeNodeStatus{activeIn("node-a", testZone)},
			want:  conditionReasonAggregateZoneMismatch,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			api := computeapitest.New()
			tc.setup(api)
			cr := activePool(tc.nodes...)

			_, pass, _ := runAggregates(t, api, cr)

			g.Expect(pass.aggregatesEnsured).To(BeFalse())
			cond := novaComputeCondition(cr, conditionTypeAggregatesReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.want))
		})
	}
}

// TestReconcileNovaComputeAggregates_ListsTheAggregatesOnce pins that the
// membership report reads the list the step already fetched: it adds no call
// to Nova.
func TestReconcileNovaComputeAggregates_ListsTheAggregatesOnce(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate(testZone, testZone, []string{}, nil)
	api.AddAggregate(tenantFilterTestsAggregate, "", []string{}, nil)
	cr := activePool(activeIn("node-a", testZone))

	runAggregates(t, api, cr)

	g.Expect(novaComputeCondition(cr, conditionTypeAggregatesReady).Reason).To(Equal(conditionReasonNodesOutsideZoneAggregate))
	g.Expect(api.CallsTo(http.MethodGet, "/v2.1/os-aggregates")).To(HaveLen(1))
}

func TestReconcileNovaComputeAggregates_UsesAnUnmarkedAggregate(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate(testZone, testZone, nil, nil)
	cr := activePool(activeIn(testNodeName, testZone))

	runAggregates(t, api, cr)

	zone, _ := api.Aggregate(testZone)
	g.Expect(zone.Metadata).NotTo(HaveKey(aggregateMarkerKey), "an aggregate someone else made is used as it is")
	g.Expect(novaComputeCondition(cr, conditionTypeAggregatesReady).Status).To(Equal(metav1.ConditionTrue))
}

func TestReconcileNovaComputeAggregates_ZoneMismatch(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate(testZone, "az2", nil, nil)
	cr := activePool(activeIn(testNodeName, testZone))

	result, pass, _ := runAggregates(t, api, cr)

	g.Expect(result.IsZero()).To(BeTrue(), "a mismatch is not a wait: the later steps still run")
	g.Expect(pass.aggregatesEnsured).To(BeFalse())
	cond := novaComputeCondition(cr, conditionTypeAggregatesReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonAggregateZoneMismatch))
	g.Expect(cond.Message).To(ContainSubstring(`aggregate az1 has availability zone "az2"`))
}

func TestReconcileNovaComputeAggregates_TenantFilterTestsWithAZoneIsAMismatch(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate(tenantFilterTestsAggregate, "az9", nil, nil)
	cr := activePool(activeIn(testNodeName, testZone))

	runAggregates(t, api, cr)

	g.Expect(novaComputeCondition(cr, conditionTypeAggregatesReady).Reason).To(Equal(conditionReasonAggregateZoneMismatch))
}

func TestReconcileNovaComputeAggregates_ConcurrentCreateRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.FailNext(http.MethodPost, "/v2.1/os-aggregates", http.StatusConflict)
	cr := activePool(activeIn(testNodeName, testZone))

	result, _, _ := runAggregates(t, api, cr)

	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
	g.Expect(novaComputeCondition(cr, conditionTypeAggregatesReady).Reason).To(Equal(conditionReasonWaitingForAggregate))
}

func TestReconcileNovaComputeAggregates_NodesWithoutZone(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := activePool(activeIn(testNodeName, testZone), activeIn("node-2", ""))

	result, _, _ := runAggregates(t, api, cr)

	g.Expect(result.IsZero()).To(BeTrue())
	_, ok := api.Aggregate(testZone)
	g.Expect(ok).To(BeTrue(), "the zones the other nodes carry are still ensured")
	cond := novaComputeCondition(cr, conditionTypeAggregatesReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonNodesWithoutZone))
	g.Expect(cond.Message).To(ContainSubstring("node-2"))
}

// TestReconcileNovaComputeAggregates_Removal pins which aggregates the step
// deletes: its own, once empty, and never one it did not create.
func TestReconcileNovaComputeAggregates_Removal(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	marker := map[string]string{aggregateMarkerKey: testPoolMarker}
	api.AddAggregate("az-old-empty", "az-old-empty", nil, marker)
	api.AddAggregate("az-old-busy", "az-old-busy", []string{"node-7"}, marker)
	api.AddAggregate("az-foreign", "az-foreign", nil, nil)
	api.AddAggregate("az-other-nova", "az-other-nova", nil, map[string]string{aggregateMarkerKey: testNamespace + "/nova-2"})
	cr := activePool(activeIn(testNodeName, testZone))

	_, _, rec := runAggregates(t, api, cr)

	_, ok := api.Aggregate("az-old-empty")
	g.Expect(ok).To(BeFalse(), "a marked, empty, unneeded aggregate is deleted")
	for _, kept := range []string{"az-old-busy", "az-foreign", "az-other-nova", testZone, tenantFilterTestsAggregate} {
		_, ok := api.Aggregate(kept)
		g.Expect(ok).To(BeTrue(), "%s is kept", kept)
	}
	g.Expect(collectEvents(rec)).To(ContainElement(ContainSubstring("Normal AggregateDeleted Deleted host aggregate az-old-empty")))
}

// TestReconcileNovaComputeAggregates_FailedMarkDeletesTheAggregate pins that an
// aggregate the step created but could not mark does not stay behind: unmarked
// it would read as someone else's and never be deleted.
func TestReconcileNovaComputeAggregates_FailedMarkDeletesTheAggregate(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.FailNext(http.MethodPost, "/v2.1/os-aggregates/", http.StatusInternalServerError)
	cr := activePool(activeIn(testNodeName, testZone))

	result, pass, _ := runAggregates(t, api, cr)

	g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
	g.Expect(pass.aggregatesEnsured).To(BeFalse())
	_, ok := api.Aggregate(testZone)
	g.Expect(ok).To(BeFalse(), "the unmarked aggregate is deleted again")
	cond := novaComputeCondition(cr, conditionTypeAggregatesReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonComputeAPIError))
	g.Expect(cond.Message).To(ContainSubstring("marking aggregate az1"))

	runAggregates(t, api, cr)
	zone, ok := api.Aggregate(testZone)
	g.Expect(ok).To(BeTrue(), "the next pass creates it anew")
	g.Expect(zone.Metadata).To(HaveKeyWithValue(aggregateMarkerKey, testPoolMarker))
}

// TestReconcileNovaComputeAggregates_KeepsForeignMetadata pins that an empty,
// marked, unneeded aggregate someone added metadata to is kept: deleting it
// would drop that metadata, such as a tenant isolation, without a trace.
func TestReconcileNovaComputeAggregates_KeepsForeignMetadata(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate("az-isolated", "az-isolated", nil, map[string]string{
		aggregateMarkerKey: testPoolMarker, "filter_tenant_id": "p1", "pinned": "true",
	})
	cr := activePool(activeIn(testNodeName, testZone))

	_, pass, rec := runAggregates(t, api, cr)

	_, ok := api.Aggregate("az-isolated")
	g.Expect(ok).To(BeTrue())
	g.Expect(pass.aggregatesEnsured).To(BeTrue(), "a kept aggregate does not hold the pool up")
	g.Expect(collectEvents(rec)).To(ContainElement(ContainSubstring(
		"Warning AggregateKept Kept empty host aggregate az-isolated: it carries metadata the operator did not set (filter_tenant_id, pinned)")))
}

// TestReconcileNovaComputeAggregates_ListsTheSiblingsItself pins the required
// set of a pass whose Nodes step did not run, the teardown of a pool that holds
// no node: the other pools' zones and tenant_filter_tests still count.
func TestReconcileNovaComputeAggregates_ListsTheSiblingsItself(t *testing.T) {
	marker := map[string]string{aggregateMarkerKey: testPoolMarker}
	sibling := rivalPool("pool-b", time.Hour, testPoolLabel, "b")
	sibling.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{
		{Name: "node-b", Phase: novav1alpha1.NovaComputeNodePending, Zone: "az2"},
	}

	t.Run("a live sibling keeps its aggregates", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddAggregate("az2", "az2", nil, marker)
		api.AddAggregate(tenantFilterTestsAggregate, "", nil, marker)
		cr := deletingNovaCompute()
		r := newNovaComputeTestReconciler(api, cr, sibling.DeepCopy())

		_, err := r.reconcileNovaComputeAggregates(context.Background(), cr, apiPass(api))

		g.Expect(err).NotTo(HaveOccurred())
		for _, kept := range []string{"az2", tenantFilterTestsAggregate} {
			_, ok := api.Aggregate(kept)
			g.Expect(ok).To(BeTrue(), "%s is kept", kept)
		}
	})

	t.Run("a failed list fails the step", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		cr := deletingNovaCompute()
		c := novaFakeClientBuilder(cr, sibling.DeepCopy()).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*novav1alpha1.NovaComputeList); ok {
					return errors.New("cache not synced")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
		r := &NovaComputeReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(10), HTTPClient: api}

		_, err := r.reconcileNovaComputeAggregates(context.Background(), cr, apiPass(api))

		g.Expect(err).To(MatchError(ContainSubstring("cache not synced")))
		g.Expect(novaComputeCondition(cr, conditionTypeAggregatesReady).Reason).To(Equal(conditionReasonNovaComputeListError))
		g.Expect(api.Calls()).To(BeEmpty(), "nothing is deleted on a guess")
	})
}

// TestReconcileNovaComputeAggregates_SiblingZonesStay pins the required set: a
// zone another pool of the Nova holds nodes in stays, whatever cluster that
// pool runs on.
func TestReconcileNovaComputeAggregates_SiblingZonesStay(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate("az2", "az2", nil, map[string]string{aggregateMarkerKey: testPoolMarker})
	sibling := validNovaCompute()
	sibling.Name = "pool-b"
	sibling.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{activeIn("node-b", "az2")}
	cr := activePool(activeIn(testNodeName, testZone))

	runAggregates(t, api, cr, *sibling)

	_, ok := api.Aggregate("az2")
	g.Expect(ok).To(BeTrue())
}

// TestReconcileNovaComputeAggregates_LastPoolRemovesTenantFilterTests pins the
// teardown of the last pool of a Nova: nothing requires tenant_filter_tests
// any more, so the marked, empty one goes.
func TestReconcileNovaComputeAggregates_LastPoolRemovesTenantFilterTests(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddAggregate(tenantFilterTestsAggregate, "", nil, map[string]string{aggregateMarkerKey: testPoolMarker})
	cr := deletingNovaCompute()

	_, pass, _ := runAggregates(t, api, cr)

	_, ok := api.Aggregate(tenantFilterTestsAggregate)
	g.Expect(ok).To(BeFalse())
	g.Expect(pass.aggregatesEnsured).To(BeTrue())
}

func TestReconcileNovaComputeAggregates_APIErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(api *computeapitest.Fake)
		want  string
	}{
		{name: "keystone 401", setup: func(api *computeapitest.Fake) { api.RejectAuth = true }, want: "HTTP 401"},
		{
			name: "list 500",
			setup: func(api *computeapitest.Fake) {
				api.FailNext(http.MethodGet, "/v2.1/os-aggregates", http.StatusInternalServerError)
			},
			want: "listing aggregates: GET /v2.1/os-aggregates: HTTP 500",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			api := computeapitest.New()
			tc.setup(api)
			cr := activePool(activeIn(testNodeName, testZone))

			result, _, _ := runAggregates(t, api, cr)

			g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
			cond := novaComputeCondition(cr, conditionTypeAggregatesReady)
			g.Expect(cond.Reason).To(Equal(conditionReasonComputeAPIError))
			g.Expect(cond.Message).To(ContainSubstring(tc.want))
		})
	}
}

func TestNodesOutsideZoneAggregate(t *testing.T) {
	inZone := func(hosts ...string) computeapi.Aggregate {
		return computeapi.Aggregate{Name: testZone, AvailabilityZone: testZone, Hosts: hosts}
	}
	withPhase := func(name string, phase novav1alpha1.NovaComputeNodePhase) novav1alpha1.NovaComputeNodeStatus {
		return novav1alpha1.NovaComputeNodeStatus{Name: name, Phase: phase, Zone: testZone}
	}
	for _, tc := range []struct {
		name       string
		nodes      []novav1alpha1.NovaComputeNodeStatus
		aggregates []computeapi.Aggregate
		want       []string
	}{
		{name: "no nodes and no aggregates"},
		{
			name:  "only Pending nodes",
			nodes: []novav1alpha1.NovaComputeNodeStatus{withPhase("node-a", novav1alpha1.NovaComputeNodePending)},
		},
		{
			name:  "an Active node without a zone",
			nodes: []novav1alpha1.NovaComputeNodeStatus{activeIn("node-a", "")},
		},
		{
			name: "Draining and Releasing nodes",
			nodes: []novav1alpha1.NovaComputeNodeStatus{
				withPhase("node-a", novav1alpha1.NovaComputeNodeDraining),
				withPhase("node-b", novav1alpha1.NovaComputeNodeReleasing),
			},
		},
		{
			name:       "an Active node in its zone's aggregate",
			nodes:      []novav1alpha1.NovaComputeNodeStatus{activeIn("node-a", testZone)},
			aggregates: []computeapi.Aggregate{inZone("node-a")},
		},
		{
			name:       "an aggregate whose hosts are null",
			nodes:      []novav1alpha1.NovaComputeNodeStatus{activeIn("node-a", testZone)},
			aggregates: []computeapi.Aggregate{inZone()},
			want:       []string{"node-a (az1)"},
		},
		{
			name:  "a member and a non-member in two zones",
			nodes: []novav1alpha1.NovaComputeNodeStatus{activeIn("node-a", testZone), activeIn("node-b", "az2")},
			aggregates: []computeapi.Aggregate{
				inZone("node-a", "node-b"),
				{Name: "az2", AvailabilityZone: "az2", Hosts: []string{"node-c"}},
			},
			want: []string{"node-b (az2)"},
		},
		{
			name:  "a host in an aggregate of another zone",
			nodes: []novav1alpha1.NovaComputeNodeStatus{activeIn("node-a", testZone)},
			aggregates: []computeapi.Aggregate{
				{Name: "az2", AvailabilityZone: "az2", Hosts: []string{"node-a"}},
			},
			want: []string{"node-a (az1)"},
		},
		{
			name:       "unordered nodes come back sorted",
			nodes:      []novav1alpha1.NovaComputeNodeStatus{activeIn("node-b", testZone), activeIn("node-a", testZone)},
			aggregates: []computeapi.Aggregate{inZone()},
			want:       []string{"node-a (az1)", "node-b (az1)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			got := nodesOutsideZoneAggregate(tc.nodes, tc.aggregates)
			if tc.want == nil {
				g.Expect(got).To(BeEmpty())
				return
			}
			g.Expect(got).To(Equal(tc.want))
		})
	}
}
