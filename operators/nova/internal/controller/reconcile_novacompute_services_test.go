// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi/computeapitest"
)

// runServices runs the Services step for cr against api, on a fake client
// holding objs.
func runServices(t *testing.T, api *computeapitest.Fake, cr *novav1alpha1.NovaCompute, pass *novaComputePass,
	objs ...client.Object,
) (ctrl.Result, *record.FakeRecorder) {
	t.Helper()
	r := newNovaComputeTestReconciler(api, append(objs, cr)...)
	if pass == nil {
		pass = apiPass(api)
	}
	result, err := r.reconcileNovaComputeServices(context.Background(), r.Client, cr, pass)
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	return result, r.Recorder.(*record.FakeRecorder)
}

// poolWith is the fixture pool holding one node in the given phase.
func poolWith(phase novav1alpha1.NovaComputeNodePhase) *novav1alpha1.NovaCompute {
	cr := validNovaCompute()
	cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{{Name: testNodeName, Phase: phase, Zone: testZone}}
	return cr
}

// nodePod is a pod of the fixture pool on node, in the given phase.
func nodePod(node string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pool-a-nova-compute-" + node, Namespace: testNamespace,
			Labels: novaComputeSelectorLabels(validNovaCompute()),
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func onlyEntry(g Gomega, cr *novav1alpha1.NovaCompute) novav1alpha1.NovaComputeNodeStatus {
	g.ExpectWithOffset(1, cr.Status.Nodes).To(HaveLen(1))
	return cr.Status.Nodes[0]
}

func TestReconcileNovaComputeServices_PendingAndActive(t *testing.T) {
	t.Run("a node without a service is pending", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := poolWith(novav1alpha1.NovaComputeNodePending)

		result, _ := runServices(t, computeapitest.New(), cr, nil)

		g.Expect(onlyEntry(g, cr).Phase).To(Equal(novav1alpha1.NovaComputeNodePending))
		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		cond := novaComputeCondition(cr, conditionTypeServicesReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForServices))
	})

	t.Run("a found service makes the node active", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		id := api.AddService(testNodeName, "enabled", "up")
		cr := poolWith(novav1alpha1.NovaComputeNodePending)

		result, _ := runServices(t, api, cr, nil)

		entry := onlyEntry(g, cr)
		g.Expect(entry.Phase).To(Equal(novav1alpha1.NovaComputeNodeActive))
		g.Expect(entry.ServiceID).To(Equal(id))
		g.Expect(entry.ServiceStatus).To(Equal("enabled"))
		g.Expect(entry.ServiceState).To(Equal("up"))
		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling), "the node was Pending when the pass began")
		cond := novaComputeCondition(cr, conditionTypeServicesReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonServicesUp))

		result, _ = runServices(t, api, cr, nil)
		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeServicePolling), "a settled pool polls at the steady pace")
	})

	t.Run("a down service is reported", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "enabled", "down")
		cr := poolWith(novav1alpha1.NovaComputeNodeActive)

		runServices(t, api, cr, nil)

		cond := novaComputeCondition(cr, conditionTypeServicesReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonServicesDown))
		g.Expect(cond.Message).To(ContainSubstring(testNodeName))
	})

	t.Run("no node polls at the steady pace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		result, _ := runServices(t, computeapitest.New(), validNovaCompute(), nil)
		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeServicePolling))
	})
}

func TestReconcileNovaComputeServices_Draining(t *testing.T) {
	t.Run("an enabled service is disabled once, with the pool in the reason", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "enabled", "up")
		api.SetServers(testNodeName, 2)
		cr := poolWith(novav1alpha1.NovaComputeNodeDraining)

		result, rec := runServices(t, api, cr, nil)

		puts := api.CallsTo(http.MethodPut, "/v2.1/os-services/")
		g.Expect(puts).To(HaveLen(1))
		var body map[string]string
		g.Expect(json.Unmarshal([]byte(puts[0].Body), &body)).To(Succeed())
		g.Expect(body).To(Equal(map[string]string{
			"status": "disabled", "disabled_reason": "c5c3.io: leaving NovaCompute openstack/pool-a",
		}))
		entry := onlyEntry(g, cr)
		g.Expect(entry.Phase).To(Equal(novav1alpha1.NovaComputeNodeDraining))
		g.Expect(entry.Instances).To(Equal(ptr.To(int32(2))))
		g.Expect(entry.ServiceStatus).To(Equal("disabled"))
		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		g.Expect(collectEvents(rec)).To(ConsistOf(ContainSubstring("Normal ComputeServiceDisabled")))
		cond := novaComputeCondition(cr, conditionTypeServicesReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonDraining))
		g.Expect(cond.Message).To(Equal("Draining node-1: 2 instances"))

		// The next pass finds it disabled and does not disable it again.
		runServices(t, api, cr, nil)
		g.Expect(api.CallsTo(http.MethodPut, "/v2.1/os-services/")).To(HaveLen(1))
	})

	t.Run("a service someone else disabled keeps its reason", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		id := api.AddService(testNodeName, "enabled", "up")
		api.SetServers(testNodeName, 1)
		outside, _ := apiPass(api).computeClient(context.Background())
		g.Expect(outside.DisableService(context.Background(), id, "maintenance window")).To(Succeed())
		cr := poolWith(novav1alpha1.NovaComputeNodeDraining)

		runServices(t, api, cr, nil)

		g.Expect(api.CallsTo(http.MethodPut, "/v2.1/os-services/")).To(HaveLen(1), "only the outside disable")
		g.Expect(onlyEntry(g, cr).DisabledReason).To(Equal("maintenance window"))
	})

	t.Run("no servers left moves the node to releasing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "disabled", "up")
		cr := poolWith(novav1alpha1.NovaComputeNodeDraining)
		cr.Status.Nodes[0].Instances = ptr.To(int32(1))

		result, _ := runServices(t, api, cr, nil)

		entry := onlyEntry(g, cr)
		g.Expect(entry.Phase).To(Equal(novav1alpha1.NovaComputeNodeReleasing))
		g.Expect(entry.Instances).To(BeNil())
		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeReleasePolling))
	})

	t.Run("a cell that did not answer holds the drain", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "disabled", "up")
		api.SetServers(testNodeName, 3)
		api.SetCellDown(testNodeName)
		cr := poolWith(novav1alpha1.NovaComputeNodeDraining)
		cr.Status.Nodes[0].Instances = ptr.To(int32(3))

		result, _ := runServices(t, api, cr, nil)

		entry := onlyEntry(g, cr)
		g.Expect(entry.Phase).To(Equal(novav1alpha1.NovaComputeNodeDraining), "the host's servers are hidden, not gone")
		g.Expect(entry.Instances).To(Equal(ptr.To(int32(3))))
		g.Expect(api.CallsTo(http.MethodGet, "/v2.1/servers")).To(BeEmpty())
		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		cond := novaComputeCondition(cr, conditionTypeServicesReady)
		g.Expect(cond.Reason).To(Equal(conditionReasonComputeAPIError))
		g.Expect(cond.Message).To(ContainSubstring("the cell of host node-1 did not answer"))
	})

	t.Run("without a service the count still runs", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.SetServers(testNodeName, 3)
		cr := poolWith(novav1alpha1.NovaComputeNodeDraining)

		runServices(t, api, cr, nil)

		g.Expect(onlyEntry(g, cr).Instances).To(Equal(ptr.To(int32(3))))
		g.Expect(api.CallsTo(http.MethodPut, "/v2.1/os-services/")).To(BeEmpty())
	})
}

func TestReconcileNovaComputeServices_Releasing(t *testing.T) {
	t.Run("a pod still on the node blocks the delete", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "disabled", "up")
		cr := poolWith(novav1alpha1.NovaComputeNodeReleasing)

		result, _ := runServices(t, api, cr, nil, nodePod(testNodeName, corev1.PodRunning))

		g.Expect(api.CallsTo(http.MethodDelete, "/v2.1/os-services/")).To(BeEmpty())
		g.Expect(onlyEntry(g, cr).Phase).To(Equal(novav1alpha1.NovaComputeNodeReleasing))
		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeReleasePolling))
	})

	t.Run("a finished pod or a pod on another node does not block", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "disabled", "up")
		cr := poolWith(novav1alpha1.NovaComputeNodeReleasing)

		runServices(t, api, cr, nil, nodePod(testNodeName, corev1.PodSucceeded), nodePod("node-2", corev1.PodRunning))

		g.Expect(api.CallsTo(http.MethodDelete, "/v2.1/os-services/")).To(HaveLen(1))
	})

	t.Run("with no pod left the service is deleted and the entry dropped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "disabled", "up")
		cr := poolWith(novav1alpha1.NovaComputeNodeReleasing)

		result, rec := runServices(t, api, cr, nil)

		g.Expect(cr.Status.Nodes).To(BeEmpty())
		g.Expect(api.Services()).To(BeEmpty())
		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeReleasePolling), "a dropped node counts as Releasing")
		g.Expect(collectEvents(rec)).To(ConsistOf(ContainSubstring("Normal ComputeServiceDeleted")))
		g.Expect(novaComputeCondition(cr, conditionTypeServicesReady).Reason).To(Equal(conditionReasonServicesUp))
	})

	t.Run("a delete answered 404 drops the entry too", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "disabled", "up")
		api.FailNext(http.MethodDelete, "/v2.1/os-services/", http.StatusNotFound)
		cr := poolWith(novav1alpha1.NovaComputeNodeReleasing)

		runServices(t, api, cr, nil)

		g.Expect(cr.Status.Nodes).To(BeEmpty())
	})

	t.Run("instances that came back return the node to draining", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "disabled", "up")
		api.SetServers(testNodeName, 1)
		cr := poolWith(novav1alpha1.NovaComputeNodeReleasing)

		runServices(t, api, cr, nil)

		g.Expect(onlyEntry(g, cr).Phase).To(Equal(novav1alpha1.NovaComputeNodeDraining))
		g.Expect(api.Services()).To(HaveLen(1))
	})

	t.Run("a handover drops the entry with no delete", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "enabled", "up")
		cr := poolWith(novav1alpha1.NovaComputeNodeReleasing)
		pass := apiPass(api)
		pass.handover[testNodeName] = true

		runServices(t, api, cr, pass)

		g.Expect(cr.Status.Nodes).To(BeEmpty())
		g.Expect(api.CallsTo(http.MethodDelete, "/v2.1/os-services/")).To(BeEmpty())
		g.Expect(api.Services()).To(HaveLen(1), "the pool taking the node keeps its service")
	})

	t.Run("several releasing nodes share one pod list", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "disabled", "up")
		api.AddService("node-2", "disabled", "up")
		api.AddService("node-3", "disabled", "up")
		cr := validNovaCompute()
		cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{
			entry(testNodeName, novav1alpha1.NovaComputeNodeReleasing),
			entry("node-2", novav1alpha1.NovaComputeNodeReleasing),
			entry("node-3", novav1alpha1.NovaComputeNodeReleasing),
		}
		podLists := 0
		c := novaFakeClientBuilder(cr, nodePod("node-2", corev1.PodRunning), nodePod("node-3", corev1.PodSucceeded)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.PodList); ok {
						podLists++
					}
					return cl.List(ctx, list, opts...)
				},
			}).Build()
		r := &NovaComputeReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(10), HTTPClient: api}

		_, err := r.reconcileNovaComputeServices(context.Background(), c, cr, apiPass(api))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(podLists).To(Equal(1))
		g.Expect(cr.Status.Nodes).To(ConsistOf(HaveField("Name", "node-2")), "only the node a pod still runs on stays")
		g.Expect(api.Services()).To(ConsistOf(HaveField("Host", "node-2")))
	})

	t.Run("no registered service drops the entry with no call", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		cr := poolWith(novav1alpha1.NovaComputeNodeReleasing)

		runServices(t, api, cr, nil)

		g.Expect(cr.Status.Nodes).To(BeEmpty())
		g.Expect(api.CallsTo(http.MethodDelete, "/v2.1/os-services/")).To(BeEmpty())
	})
}

// TestReconcileNovaComputeServices_OffboardedNode pins what the pool reports
// once openstack-hypervisor-operator deleted the service of a node the pool
// still selects, and that the drain then drops the entry without a write to
// Nova.
func TestReconcileNovaComputeServices_OffboardedNode(t *testing.T) {
	t.Run("an active node whose service is gone waits for it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := poolWith(novav1alpha1.NovaComputeNodeActive)

		runServices(t, computeapitest.New(), cr, nil)

		g.Expect(onlyEntry(g, cr).Phase).To(Equal(novav1alpha1.NovaComputeNodePending))
		cond := novaComputeCondition(cr, conditionTypeServicesReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForServices))
		g.Expect(cond.Message).To(ContainSubstring(testNodeName))
	})

	t.Run("a draining node with no service and no server is dropped with no write", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		cr := poolWith(novav1alpha1.NovaComputeNodeDraining)

		runServices(t, api, cr, nil)
		g.Expect(onlyEntry(g, cr).Phase).To(Equal(novav1alpha1.NovaComputeNodeReleasing))
		runServices(t, api, cr, nil)

		g.Expect(cr.Status.Nodes).To(BeEmpty())
		g.Expect(api.CallsTo(http.MethodPut, "/v2.1/os-services/")).To(BeEmpty())
		g.Expect(api.CallsTo(http.MethodDelete, "/v2.1/os-services/")).To(BeEmpty())
	})
}

// TestReconcileNovaComputeServices_NeverEnables pins that a node relabelled
// back into the pool mid-drain gets no service update: the satellite only ever
// disables.
func TestReconcileNovaComputeServices_NeverEnables(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	id := api.AddService(testNodeName, "enabled", "up")
	earlier, _ := apiPass(api).computeClient(context.Background())
	g.Expect(earlier.DisableService(context.Background(), id, "c5c3.io: leaving NovaCompute openstack/pool-a")).To(Succeed())
	cr := poolWith(novav1alpha1.NovaComputeNodeActive)
	cr.Status.Nodes[0].ServiceID = id

	runServices(t, api, cr, nil)

	g.Expect(api.CallsTo(http.MethodPut, "/v2.1/os-services/")).To(HaveLen(1), "only the disable above")
	entry := onlyEntry(g, cr)
	g.Expect(entry.Phase).To(Equal(novav1alpha1.NovaComputeNodeActive))
	g.Expect(entry.ServiceStatus).To(Equal("disabled"))
}

func TestReconcileNovaComputeServices_APIErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(api *computeapitest.Fake)
		want  string
	}{
		{name: "keystone 401", setup: func(api *computeapitest.Fake) { api.RejectAuth = true }, want: "POST /v3/auth/tokens: HTTP 401"},
		{
			name: "compute 500",
			setup: func(api *computeapitest.Fake) {
				api.FailNext(http.MethodGet, "/v2.1/os-services", http.StatusInternalServerError)
			},
			want: "GET /v2.1/os-services?binary=nova-compute: HTTP 500",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			api := computeapitest.New()
			tc.setup(api)
			cr := poolWith(novav1alpha1.NovaComputeNodeActive)

			result, _ := runServices(t, api, cr, nil)

			g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
			cond := novaComputeCondition(cr, conditionTypeServicesReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonComputeAPIError))
			g.Expect(cond.Message).To(ContainSubstring(tc.want))
		})
	}

	t.Run("a failed disable keeps the walk's progress", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "disabled", "up")
		api.AddService("node-2", "enabled", "up")
		api.SetServers("node-2", 4)
		api.AddService("node-3", "enabled", "up")
		api.FailNext(http.MethodPut, "/v2.1/os-services/", http.StatusForbidden)
		cr := validNovaCompute()
		draining := entry("node-2", novav1alpha1.NovaComputeNodeDraining)
		draining.Instances = ptr.To(int32(4))
		cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{
			entry(testNodeName, novav1alpha1.NovaComputeNodeReleasing),
			draining,
			entry("node-3", novav1alpha1.NovaComputeNodeDraining),
		}

		result, _ := runServices(t, api, cr, nil)

		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		g.Expect(cr.Status.Nodes).To(HaveExactElements(
			And(
				HaveField("Name", "node-2"),
				HaveField("Phase", novav1alpha1.NovaComputeNodeDraining),
				HaveField("Instances", ptr.To(int32(4))),
			),
			Equal(entry("node-3", novav1alpha1.NovaComputeNodeDraining)),
		), "node-1 was released before the failure; the failed entry and the ones after it are kept")
		g.Expect(api.Services()).NotTo(ContainElement(HaveField("Host", testNodeName)))
		for _, call := range api.CallsTo(http.MethodGet, "/v2.1/servers") {
			g.Expect(call.Query).NotTo(ContainSubstring("node-3"), "the walk stops at the failed call")
		}
		g.Expect(novaComputeCondition(cr, conditionTypeServicesReady).Message).To(ContainSubstring("HTTP 403"))
	})
}

// TestReconcileNovaComputeServices_Failures pins the failure branches of the
// walk: every one keeps the entry as it was, and a placed pool reads its pods
// on its own cluster.
func TestReconcileNovaComputeServices_Failures(t *testing.T) {
	ctx := context.Background()
	placed := func(phase novav1alpha1.NovaComputeNodePhase) *novav1alpha1.NovaCompute {
		cr := poolWith(phase)
		cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "compute-a"}
		return cr
	}

	t.Run("a failed pod list is an error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "disabled", "up")
		cr := poolWith(novav1alpha1.NovaComputeNodeReleasing)
		c := novaFakeClientBuilder(cr).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.PodList); ok {
					return errors.New("etcd is down")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
		r := &NovaComputeReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(10), HTTPClient: api}

		_, err := r.reconcileNovaComputeServices(ctx, c, cr, apiPass(api))

		g.Expect(err).To(MatchError(ContainSubstring("etcd is down")))
		g.Expect(novaComputeCondition(cr, conditionTypeServicesReady).Reason).To(Equal(conditionReasonPodListError))
		g.Expect(onlyEntry(g, cr).Phase).To(Equal(novav1alpha1.NovaComputeNodeReleasing))
		g.Expect(api.CallsTo(http.MethodDelete, "/v2.1/os-services/")).To(BeEmpty(), "a failed list is not an empty node")
	})

	t.Run("an unresolvable pod reader waits", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		cr := placed(novav1alpha1.NovaComputeNodeReleasing)
		r := newNovaComputeTestReconciler(api, cr)
		r.Resolver = unresolvableResolver{}

		result, err := r.reconcileNovaComputeServices(ctx, r.Client, cr, apiPass(api))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		g.Expect(novaComputeCondition(cr, conditionTypeServicesReady).Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
		g.Expect(onlyEntry(g, cr).Phase).To(Equal(novav1alpha1.NovaComputeNodeReleasing))
	})

	t.Run("a placed pool reads its pods on the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "disabled", "up")
		cr := placed(novav1alpha1.NovaComputeNodeReleasing)
		r := newNovaComputeTestReconciler(api, cr)
		target := novaFakeClientBuilder(nodePod(testNodeName, corev1.PodRunning)).Build()
		r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{Client: target})

		_, err := r.reconcileNovaComputeServices(ctx, target, cr, apiPass(api))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(api.CallsTo(http.MethodDelete, "/v2.1/os-services/")).To(BeEmpty(),
			"nova-compute still runs on the target, whatever the management cluster holds")
		g.Expect(onlyEntry(g, cr).Phase).To(Equal(novav1alpha1.NovaComputeNodeReleasing))
	})

	for _, tc := range []struct {
		name  string
		phase novav1alpha1.NovaComputeNodePhase
		fail  func(api *computeapitest.Fake)
		want  string
	}{
		{
			name:  "a failed server count keeps the node draining",
			phase: novav1alpha1.NovaComputeNodeDraining,
			fail: func(api *computeapitest.Fake) {
				api.FailNext(http.MethodGet, "/v2.1/servers", http.StatusInternalServerError)
			},
			want: "counting the servers on node-1",
		},
		{
			name:  "a failed delete keeps the node releasing",
			phase: novav1alpha1.NovaComputeNodeReleasing,
			fail: func(api *computeapitest.Fake) {
				api.FailNext(http.MethodDelete, "/v2.1/os-services/", http.StatusInternalServerError)
			},
			want: "deleting the compute service of node-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			api := computeapitest.New()
			api.AddService(testNodeName, "disabled", "up")
			tc.fail(api)
			cr := poolWith(tc.phase)

			result, _ := runServices(t, api, cr, nil)

			g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
			g.Expect(onlyEntry(g, cr).Phase).To(Equal(tc.phase))
			g.Expect(api.Services()).To(HaveLen(1))
			cond := novaComputeCondition(cr, conditionTypeServicesReady)
			g.Expect(cond.Reason).To(Equal(conditionReasonComputeAPIError))
			g.Expect(cond.Message).To(ContainSubstring(tc.want))
		})
	}
}

// servicesRun is what one Services step run with the host discovery returned,
// with the cluster it ran against and the events it recorded.
type servicesRun struct {
	result ctrl.Result
	err    error
	client client.Client
	events []string
}

// runServicesWithDiscovery runs the Services step for cr against api on a fake
// cluster that holds the Nova API Deployment, so the step can start the host
// discovery Job. funcs intercepts the cluster's calls.
func runServicesWithDiscovery(api *computeapitest.Fake, cr *novav1alpha1.NovaCompute,
	funcs interceptor.Funcs,
) servicesRun {
	c := novaFakeClientBuilder(cr, novaAPIDeployment(validNova(), testNovaConfigMap)).
		WithInterceptorFuncs(funcs).Build()
	r := &NovaComputeReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100), HTTPClient: api}
	result, err := r.reconcileNovaComputeServices(context.Background(), c, cr, discoveryPass(api, c))
	return servicesRun{result: result, err: err, client: c, events: collectEvents(r.Recorder.(*record.FakeRecorder))}
}

func TestReconcileNovaComputeServices_HostMapping(t *testing.T) {
	t.Run("a registered, mapped host makes the node active", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		id := api.AddService(testNodeName, "enabled", "up")
		cr := poolWith(novav1alpha1.NovaComputeNodePending)

		run := runServicesWithDiscovery(api, cr, interceptor.Funcs{})

		g.Expect(run.err).NotTo(HaveOccurred())
		entry := onlyEntry(g, cr)
		g.Expect(entry.Phase).To(Equal(novav1alpha1.NovaComputeNodeActive))
		g.Expect(entry.ServiceID).To(Equal(id))
		cond := novaComputeCondition(cr, conditionTypeServicesReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonServicesUp))
		g.Expect(api.CallsTo(http.MethodGet, "/v2.1/os-hypervisors")).NotTo(BeEmpty())
		g.Expect(discoveryJobExists(g, run.client)).To(BeFalse())
	})

	t.Run("a registered, unmapped host keeps the node pending and starts the discovery", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		id := api.AddService(testNodeName, "enabled", "up")
		api.SetHostMapped(testNodeName, false)
		cr := poolWith(novav1alpha1.NovaComputeNodePending)

		run := runServicesWithDiscovery(api, cr, interceptor.Funcs{})

		g.Expect(run.err).NotTo(HaveOccurred())
		entry := onlyEntry(g, cr)
		g.Expect(entry.Phase).To(Equal(novav1alpha1.NovaComputeNodePending))
		g.Expect(entry.ServiceID).To(Equal(id))
		cond := novaComputeCondition(cr, conditionTypeServicesReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForHostMapping))
		g.Expect(cond.Message).To(Equal(
			"Waiting for the host mapping of node-1: started discovery Job " + testDiscoveryJob.String()))
		g.Expect(run.result.RequeueAfter).To(Equal(RequeueHostDiscoveryPolling))
		g.Expect(RequeueHostDiscoveryPolling).To(Equal(10 * time.Second))
		g.Expect(discoveryJobExists(g, run.client)).To(BeTrue())
		g.Expect(run.events).To(HaveLen(1))

		// Once the discovery mapped the host, the next pass makes the node
		// Active.
		api.SetHostMapped(testNodeName, true)
		run = runServicesWithDiscovery(api, cr, interceptor.Funcs{})
		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(onlyEntry(g, cr).Phase).To(Equal(novav1alpha1.NovaComputeNodeActive))
		g.Expect(novaComputeCondition(cr, conditionTypeServicesReady).Reason).To(Equal(conditionReasonServicesUp))
	})

	t.Run("a node without a service waits for it and reads no mapping", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		cr := poolWith(novav1alpha1.NovaComputeNodePending)

		run := runServicesWithDiscovery(api, cr, interceptor.Funcs{})

		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(novaComputeCondition(cr, conditionTypeServicesReady).Reason).To(Equal(conditionReasonWaitingForServices))
		g.Expect(api.CallsTo(http.MethodGet, "/v2.1/os-hypervisors")).To(BeEmpty())
	})

	t.Run("a node without a service wins over an unmapped one", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService("node-2", "enabled", "up")
		api.SetHostMapped("node-2", false)
		cr := validNovaCompute()
		cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{
			{Name: testNodeName, Phase: novav1alpha1.NovaComputeNodePending, Zone: testZone},
			{Name: "node-2", Phase: novav1alpha1.NovaComputeNodePending, Zone: testZone},
		}

		run := runServicesWithDiscovery(api, cr, interceptor.Funcs{})

		g.Expect(run.err).NotTo(HaveOccurred())
		cond := novaComputeCondition(cr, conditionTypeServicesReady)
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForServices))
		g.Expect(cond.Message).To(Equal("Waiting for a compute service to register on node-1"))
		g.Expect(discoveryJobExists(g, run.client)).To(BeTrue(), "node-2 still gets its discovery")
	})

	t.Run("an active node is not checked again", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "enabled", "up")
		api.SetHostMapped(testNodeName, false)
		cr := poolWith(novav1alpha1.NovaComputeNodeActive)

		run := runServicesWithDiscovery(api, cr, interceptor.Funcs{})

		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(onlyEntry(g, cr).Phase).To(Equal(novav1alpha1.NovaComputeNodeActive))
		g.Expect(novaComputeCondition(cr, conditionTypeServicesReady).Reason).To(Equal(conditionReasonServicesUp))
		g.Expect(api.CallsTo(http.MethodGet, "/v2.1/os-hypervisors")).To(BeEmpty())
		g.Expect(discoveryJobExists(g, run.client)).To(BeFalse())
	})

	t.Run("a failed hypervisor list keeps the node pending", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		id := api.AddService(testNodeName, "enabled", "up")
		api.FailNext(http.MethodGet, "/v2.1/os-hypervisors", http.StatusInternalServerError)
		cr := poolWith(novav1alpha1.NovaComputeNodePending)

		run := runServicesWithDiscovery(api, cr, interceptor.Funcs{})

		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		entry := onlyEntry(g, cr)
		g.Expect(entry.Phase).To(Equal(novav1alpha1.NovaComputeNodePending))
		g.Expect(entry.ServiceID).To(Equal(id))
		cond := novaComputeCondition(cr, conditionTypeServicesReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonComputeAPIError))
		// The client names the path with its query, as for the service list.
		g.Expect(cond.Message).To(ContainSubstring("listing hypervisors: GET /v2.1/os-hypervisors/detail"))
		g.Expect(cond.Message).To(ContainSubstring(": HTTP 500"))
		g.Expect(discoveryJobExists(g, run.client)).To(BeFalse(), "no discovery runs on an unknown mapping")
	})

	// A node replacement: one node leaves while its successor joins.
	t.Run("a failed hypervisor list still moves the pool's other nodes", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddService(testNodeName, "enabled", "up")
		api.AddService("node-2", "disabled", "up")
		api.FailNext(http.MethodGet, "/v2.1/os-hypervisors", http.StatusInternalServerError)
		cr := validNovaCompute()
		cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{
			{Name: testNodeName, Phase: novav1alpha1.NovaComputeNodePending, Zone: testZone},
			{Name: "node-2", Phase: novav1alpha1.NovaComputeNodeDraining, Zone: testZone},
		}

		run := runServicesWithDiscovery(api, cr, interceptor.Funcs{})

		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(cr.Status.Nodes).To(HaveLen(2))
		g.Expect(cr.Status.Nodes[0].Phase).To(Equal(novav1alpha1.NovaComputeNodePending))
		g.Expect(cr.Status.Nodes[1].Phase).To(Equal(novav1alpha1.NovaComputeNodeReleasing),
			"the drained node moves on although the mapping is unknown")
		g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeReleasePolling))
		g.Expect(novaComputeCondition(cr, conditionTypeServicesReady).Reason).To(Equal(conditionReasonComputeAPIError))
	})

	t.Run("a refused Job create keeps the walked nodes and the pool's poll", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		id := api.AddService(testNodeName, "enabled", "up")
		api.SetHostMapped(testNodeName, false)
		cr := poolWith(novav1alpha1.NovaComputeNodePending)
		forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"},
			testDiscoveryJob.Name, errors.New("jobs.batch is forbidden"))

		run := runServicesWithDiscovery(api, cr, interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return forbidden
			},
		})

		// A returned error would put the pool on the controller's growing
		// backoff; the requeue keeps the poll its nodes need.
		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		cond := novaComputeCondition(cr, conditionTypeServicesReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonHostDiscoveryError))
		g.Expect(cond.Message).To(ContainSubstring("creating discovery Job " + testDiscoveryJob.String()))
		g.Expect(cond.Message).To(ContainSubstring("jobs.batch is forbidden"))
		entry := onlyEntry(g, cr)
		g.Expect(entry.Phase).To(Equal(novav1alpha1.NovaComputeNodePending))
		g.Expect(entry.ServiceID).To(Equal(id), "the walked entry is kept")
	})
}
