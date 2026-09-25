// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
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
	g.Expect(cond.Reason).To(Equal(conditionReasonAggregatesEnsured))
	g.Expect(collectEvents(rec)).To(ConsistOf(
		ContainSubstring("Normal AggregateCreated Created host aggregate az1 in availability zone az1"),
		ContainSubstring("Normal AggregateCreated Created host aggregate tenant_filter_tests"),
	))

	// A second pass finds both and creates nothing.
	before := len(api.CallsTo(http.MethodPost, "/v2.1/os-aggregates"))
	runAggregates(t, api, cr)
	g.Expect(api.CallsTo(http.MethodPost, "/v2.1/os-aggregates")).To(HaveLen(before))
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
