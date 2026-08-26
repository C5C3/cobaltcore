// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The addresses the fixtures below publish. The cluster IPs differ per database
// so a step that read the wrong database's Services would produce a visibly
// wrong list; the host IPs do not, because both databases run on the same nodes.
const (
	nbClusterIPPrefix = "10.0.0."
	sbClusterIPPrefix = "10.0.1."
	hostIPPrefix      = "192.168.1.1"
)

// memberService returns the per-member Service the endpoint step reads, with a
// cluster IP already assigned. An empty clusterIP stands for a Service the API
// server has not allocated one for yet.
func memberService(cr *ovnv1alpha1.OVNCentral, db raftDB, ordinal int32, clusterIP string) *corev1.Service {
	svc := raftPerPodService(cr, db, ordinal)
	svc.Spec.ClusterIP = clusterIP
	return svc
}

// memberPod returns the pod of one member. An empty hostIP stands for a pod that
// is not scheduled onto a node yet.
func memberPod(cr *ovnv1alpha1.OVNCentral, db raftDB, ordinal int32, hostIP string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      raftMemberName(cr, db, ordinal),
			Namespace: cr.Namespace,
		},
		Status: corev1.PodStatus{HostIP: hostIP},
	}
}

// publishedFixtures returns the Services and pods of a fully published control
// plane: every member has a Service with a cluster IP and a pod on a node.
func publishedFixtures(cr *ovnv1alpha1.OVNCentral) []client.Object {
	var objs []client.Object
	for _, db := range []raftDB{northboundDB(cr), southboundDB(cr)} {
		prefix := nbClusterIPPrefix
		if db.suffix == suffixSouthbound {
			prefix = sbClusterIPPrefix
		}
		for ordinal := int32(0); ordinal < db.spec.Replicas; ordinal++ {
			objs = append(objs,
				memberService(cr, db, ordinal, fmt.Sprintf("%s%d", prefix, ordinal+1)),
				memberPod(cr, db, ordinal, fmt.Sprintf("%s%d", hostIPPrefix, ordinal)))
		}
	}
	return objs
}

func TestReconcileEndpoints_PublishesAddressesInOrdinalOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := publishOnNodePorts(testOVNCentral())
	r := newTestOVNCentralReconciler(t, append(publishedFixtures(cr), cr)...)

	res, err := r.reconcileEndpoints(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "published endpoints are not polled")

	g.Expect(cr.Status.Northbound.InternalDbAddress).To(Equal(
		"ssl:10.0.0.1:6641,ssl:10.0.0.2:6641,ssl:10.0.0.3:6641"))
	g.Expect(cr.Status.Northbound.DbAddress).To(Equal(
		"ssl:192.168.1.10:30641,ssl:192.168.1.11:30642,ssl:192.168.1.12:30643"))
	g.Expect(cr.Status.Southbound.InternalDbAddress).To(Equal(
		"ssl:10.0.1.1:6642,ssl:10.0.1.2:6642,ssl:10.0.1.3:6642"))
	g.Expect(cr.Status.Southbound.DbAddress).To(Equal(
		"ssl:192.168.1.10:30651,ssl:192.168.1.11:30652,ssl:192.168.1.12:30653"))

	cond := ovnCentralCondition(cr, conditionTypeEndpointsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointsPublished))
}

// A Service the API server has not allocated a cluster IP for yet holds the
// whole step: clients inside the cluster have no substitute for that address.
func TestReconcileEndpoints_ServiceWithoutClusterIPIsPending(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()

	var objs []client.Object
	for _, obj := range publishedFixtures(cr) {
		if svc, isService := obj.(*corev1.Service); isService && svc.Name == "ovn-nb-1" {
			objs = append(objs, memberService(cr, northboundDB(cr), 1, ""))
			continue
		}
		objs = append(objs, obj)
	}
	r := newTestOVNCentralReconciler(t, append(objs, cr)...)

	res, err := r.reconcileEndpoints(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	expectNoPublishedAddresses(g, cr)

	cond := ovnCentralCondition(cr, conditionTypeEndpointsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointsPending))
}

// Not one member of a database is on a node: nothing outside the cluster can
// reach it, so the CR publishes no address at all rather than the half that
// clients inside the cluster could use.
func TestReconcileEndpoints_NoPodWithHostIPIsPending(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := publishOnNodePorts(testOVNCentral())

	var objs []client.Object
	for _, db := range []raftDB{northboundDB(cr), southboundDB(cr)} {
		for ordinal := int32(0); ordinal < db.spec.Replicas; ordinal++ {
			objs = append(objs,
				memberService(cr, db, ordinal, fmt.Sprintf("%s%d", nbClusterIPPrefix, ordinal+1)),
				memberPod(cr, db, ordinal, ""))
		}
	}
	r := newTestOVNCentralReconciler(t, append(objs, cr)...)

	res, err := r.reconcileEndpoints(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	expectNoPublishedAddresses(g, cr)
	g.Expect(ovnCentralCondition(cr, conditionTypeEndpointsReady).Reason).To(Equal(conditionReasonEndpointsPending))
}

// The very first pass, before the database step's Services have been observed.
func TestReconcileEndpoints_MissingServiceIsPending(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)

	res, err := r.reconcileEndpoints(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	expectNoPublishedAddresses(g, cr)
	g.Expect(ovnCentralCondition(cr, conditionTypeEndpointsReady).Reason).To(Equal(conditionReasonEndpointsPending))
}

// A member whose pod is gone is skipped in the node-facing list while the
// members beside it keep publishing: a rescheduling member has no node to name,
// and holding the whole address list for it would take a reachable control
// plane's addresses away from every client.
func TestReconcileEndpoints_MissingPodIsSkipped(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := publishOnNodePorts(testOVNCentral())

	var objs []client.Object
	for _, obj := range publishedFixtures(cr) {
		pod, isPod := obj.(*corev1.Pod)
		if isPod && pod.Name == "ovn-nb-1" {
			continue
		}
		objs = append(objs, obj)
	}
	r := newTestOVNCentralReconciler(t, append(objs, cr)...)

	res, err := r.reconcileEndpoints(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(cr.Status.Northbound.InternalDbAddress).To(Equal(
		"ssl:10.0.0.1:6641,ssl:10.0.0.2:6641,ssl:10.0.0.3:6641"),
		"a missing pod costs no cluster-internal address: the Service keeps it")
	g.Expect(cr.Status.Northbound.DbAddress).To(Equal(
		"ssl:192.168.1.10:30641,ssl:192.168.1.12:30643"))
	g.Expect(ovnCentralCondition(cr, conditionTypeEndpointsReady).Status).To(Equal(metav1.ConditionTrue))
}

// A read the target cluster refuses is not a wait: it says nothing about where
// the members are, so the pass fails and the workqueue backs off.
func TestReconcileEndpoints_ServiceReadErrorIsReturned(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()

	c := ovnCentralFakeClientBuilder(t, append(publishedFixtures(cr), cr)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isService := obj.(*corev1.Service); isService {
					return apierrors.NewForbidden(corev1.Resource("services"), key.Name, nil)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &OVNCentralReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileEndpoints(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("reading endpoint Service ovn-nb-0")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())
	expectNoPublishedAddresses(g, cr)
	g.Expect(ovnCentralCondition(cr, conditionTypeEndpointsReady).Reason).To(Equal(conditionReasonEndpointsPending))
}

// The same for the pods, which are read right behind the Services.
func TestReconcileEndpoints_PodReadErrorIsReturned(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := publishOnNodePorts(testOVNCentral())

	c := ovnCentralFakeClientBuilder(t, append(publishedFixtures(cr), cr)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isPod := obj.(*corev1.Pod); isPod {
					return apierrors.NewForbidden(corev1.Resource("pods"), key.Name, nil)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &OVNCentralReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	_, err := r.reconcileEndpoints(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("reading endpoint pod ovn-nb-0")))
	expectNoPublishedAddresses(g, cr)
}

// A published address that no longer holds is worse than none: a client dials
// it and waits out its own timeout instead of failing over.
func TestReconcileEndpoints_PendingClearsPreviouslyPublishedAddresses(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	cr.Status.Northbound.InternalDbAddress = "ssl:10.0.0.1:6641"
	cr.Status.Northbound.DbAddress = "ssl:192.168.1.10:30641"
	cr.Status.Southbound.InternalDbAddress = "ssl:10.0.1.1:6642"
	cr.Status.Southbound.DbAddress = "ssl:192.168.1.10:30651"
	r := newTestOVNCentralReconciler(t, cr)

	res, err := r.reconcileEndpoints(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	expectNoPublishedAddresses(g, cr)
}

// expectNoPublishedAddresses asserts that all four address fields are empty.
func expectNoPublishedAddresses(g *WithT, cr *ovnv1alpha1.OVNCentral) {
	g.Expect(cr.Status.Northbound.InternalDbAddress).To(BeEmpty())
	g.Expect(cr.Status.Northbound.DbAddress).To(BeEmpty())
	g.Expect(cr.Status.Southbound.InternalDbAddress).To(BeEmpty())
	g.Expect(cr.Status.Southbound.DbAddress).To(BeEmpty())
}

// A control plane that publishes neither database on node ports reaches Ready
// on the cluster-internal addresses alone. Without this the node-facing list
// would be the gate on a posture that never produces one, and every OVNCentral
// with the default settings would sit at EndpointsPending forever.
func TestReconcileEndpoints_ClusterInternalOnlyPublishesNoNodeAddress(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()

	// Services with cluster IPs, and no pod on any node at all: the node-facing
	// half has nothing to build from and must not be waited for.
	var objs []client.Object
	for _, db := range []raftDB{northboundDB(cr), southboundDB(cr)} {
		prefix := nbClusterIPPrefix
		if db.suffix == suffixSouthbound {
			prefix = sbClusterIPPrefix
		}
		for ordinal := int32(0); ordinal < db.spec.Replicas; ordinal++ {
			objs = append(objs, memberService(cr, db, ordinal, fmt.Sprintf("%s%d", prefix, ordinal+1)))
		}
	}
	r := newTestOVNCentralReconciler(t, append(objs, cr)...)

	res, err := r.reconcileEndpoints(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(cr.Status.Northbound.InternalDbAddress).To(Equal(
		"ssl:10.0.0.1:6641,ssl:10.0.0.2:6641,ssl:10.0.0.3:6641"))
	g.Expect(cr.Status.Southbound.InternalDbAddress).To(Equal(
		"ssl:10.0.1.1:6642,ssl:10.0.1.2:6642,ssl:10.0.1.3:6642"))
	g.Expect(cr.Status.Northbound.DbAddress).To(BeEmpty())
	g.Expect(cr.Status.Southbound.DbAddress).To(BeEmpty())
	g.Expect(ovnCentralCondition(cr, conditionTypeEndpointsReady).Status).To(Equal(metav1.ConditionTrue))
}
