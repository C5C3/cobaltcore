// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// testClientSecretName is the Secret the TLS step of an OVNCentral publishes
// and every chassis container mounts.
const testClientSecretName = "ovn-client"

// testRelayAddress is the address of a Southbound relay tier, which the chassis
// prefer over the database itself.
const testRelayAddress = "ssl:10.96.0.99:6642"

// resolvableOVNCentral is the OVNCentral fixture as a chassis needs it: both
// database addresses published, the client Secret named, and the image the
// central resolves already installed, which is what says its own rollout has
// landed.
func resolvableOVNCentral() *ovnv1alpha1.OVNCentral {
	central := publishEndpoints(testOVNCentral())
	central.Status.ClientSecretName = testClientSecretName
	central.Status.InstalledImage = effectiveImage(central.Spec.Image).Reference()
	return central
}

// An OVNChassis applied before its OVNCentral is an ordinary ordering of two
// objects in one manifest, so the step polls rather than failing the pass.
func TestReconcileCentral_NotFoundWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr)

	resolved, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(resolved).To(Equal(resolvedCentral{}))

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCentralNotFound))
	g.Expect(cond.Message).To(ContainSubstring(testOVNCentralName),
		"the message has to name the central somebody has to go and create")
}

// A Get that fails for any other reason is the operator's own problem, not a
// state to wait out: it is returned so the workqueue retries with backoff, and
// the condition flips so the aggregate Ready cannot stay stale-True.
func TestReconcileCentral_ReadErrorIsCentralReadError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()

	c := ovnChassisFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*ovnv1alpha1.OVNCentral); ok {
					return apierrors.NewForbidden(
						ovnv1alpha1.GroupVersion.WithResource("ovncentrals").GroupResource(), key.Name, nil)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	_, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).To(MatchError(ContainSubstring("reading OVNCentral openstack/ovn")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCentralReadError))
}

// testNorthboundNodeAddress and testSouthboundNodeAddress are the addresses a
// central publishes outside its cluster, and testRelayNodeAddress the node
// address of its published relay. They differ from the cluster-IP addresses so
// a test can tell which pair a chassis was handed.
const (
	testNorthboundNodeAddress = "ssl:172.18.0.5:30641"
	testSouthboundNodeAddress = "ssl:172.18.0.5:30651"
	testRelayNodeAddress      = "ssl:172.18.0.5:30661"
)

// crossClusterOVNCentral is resolvableOVNCentral projecting onto edge-1 with
// both databases published on node ports and their node addresses in status,
// as a chassis on another cluster needs it.
func crossClusterOVNCentral() *ovnv1alpha1.OVNCentral {
	central := publishOnNodePorts(resolvableOVNCentral())
	central.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	central.Status.Northbound.DbAddress = testNorthboundNodeAddress
	central.Status.Southbound.DbAddress = testSouthboundNodeAddress
	return central
}

// A central on another cluster is reached at the addresses it publishes on node
// ports. Without a relay ovn-controller dials the Southbound node address, and
// the client Secret is left to the next step, which copies it onto the
// chassis's cluster.
func TestReconcileCentral_CrossClusterUsesTheNodeAddresses(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, crossClusterOVNCentral())

	resolved, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(resolved).To(Equal(resolvedCentral{
		ovnRemote:               testSouthboundNodeAddress,
		nbAddress:               testNorthboundNodeAddress,
		sbAddress:               testSouthboundNodeAddress,
		sameCluster:             false,
		centralTargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge-1"},
		sourceClientSecretName:  testClientSecretName,
	}), "the pods mount no Secret by the source's name across a cluster boundary")

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonCentralResolved))
	g.Expect(cond.Message).To(ContainSubstring(testSouthboundNodeAddress))
}

// Across a cluster boundary both databases have to be published: a chassis
// dials the Southbound one, and the evacuation Job writes the Northbound one. A
// central publishing neither, or the Southbound alone, is refused with the two
// fields to set, and without a requeue: the fix is an edit to the central,
// which the central watch delivers.
func TestReconcileCentral_CrossClusterWithoutNodePortsIsNotExternallyReachable(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		northbound, southbound bool
	}{
		{"neither database published", false, false},
		{"only the Southbound database published", false, true},
		{"only the Northbound database published", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cr := testOVNChassis()
			cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
			central := resolvableOVNCentral()
			central.Spec.Northbound.ExternallyReachable = tc.northbound
			central.Spec.Southbound.ExternallyReachable = tc.southbound
			central.Status.Southbound.DbAddress = testSouthboundNodeAddress
			r := newTestOVNChassisReconciler(t, cr, central)

			resolved, res, err := r.reconcileCentral(ctx, cr)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue(), "an edit to the central is what repairs it, not a retry")
			g.Expect(resolved).To(Equal(resolvedCentral{}))

			cond := ovnChassisCondition(cr, conditionTypeCentralReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonCentralNotExternallyReachable))
			g.Expect(cond.Message).To(And(
				ContainSubstring("target cluster edge-1"),
				ContainSubstring("the management cluster"),
				ContainSubstring("spec.northbound.externallyReachable"),
				ContainSubstring("spec.southbound.externallyReachable"),
			), "the message has to name both clusters and both fields to be actionable")
		})
	}
}

// A published central whose node addresses have not been assembled yet is an
// ordinary wait, polled at the Raft cadence like the same wait inside one
// cluster. Two target clusters that are not the same one take the same path as
// a target and the management cluster.
func TestReconcileCentral_CrossClusterPublishedButPendingWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-2"}
	central := crossClusterOVNCentral()
	central.Status.Northbound.DbAddress = ""
	central.Status.Southbound.DbAddress = ""
	r := newTestOVNChassisReconciler(t, cr, central)

	resolved, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	g.Expect(resolved).To(Equal(resolvedCentral{}))

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCentralNotReady))
	g.Expect(cond.Message).To(Equal("Waiting for OVNCentral " + testOVNCentralName +
		" to publish its Southbound address outside its cluster and its client Secret"))
}

// A relay published outside its cluster keeps its purpose for a chassis on
// another cluster: ovn-controller dials it, while the deregistration Job still
// addresses the Southbound database itself.
func TestReconcileCentral_CrossClusterPrefersThePublishedRelay(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	central := crossClusterOVNCentral()
	central.Spec.Relay = &ovnv1alpha1.OVNRelaySpec{Replicas: 1, ExternallyReachable: true}
	central.Status.RelayAddress = testRelayAddress
	central.Status.RelayDbAddress = testRelayNodeAddress
	r := newTestOVNChassisReconciler(t, cr, central)

	resolved, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(resolved.ovnRemote).To(Equal(testRelayNodeAddress))
	g.Expect(resolved.sbAddress).To(Equal(testSouthboundNodeAddress))
	g.Expect(ovnChassisCondition(cr, conditionTypeCentralReady).Message).To(ContainSubstring(testRelayNodeAddress))
}

// A relay the central publishes but whose node address is not assembled yet is
// waited for rather than bypassed: falling back to the Southbound database for
// one pass would put every chassis on the Raft members and roll them all again
// when the relay address arrives.
func TestReconcileCentral_CrossClusterWaitsForTheRelayNodeAddress(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	central := crossClusterOVNCentral()
	central.Spec.Relay = &ovnv1alpha1.OVNRelaySpec{Replicas: 1, ExternallyReachable: true}
	central.Status.RelayAddress = testRelayAddress
	r := newTestOVNChassisReconciler(t, cr, central)

	resolved, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	g.Expect(resolved).To(Equal(resolvedCentral{}))

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCentralNotReady))
	g.Expect(cond.Message).To(Equal("Waiting for OVNCentral " + testOVNCentralName +
		" to publish its relay's node address"))
}

// A relay that is not published outside its cluster has a cluster IP alone,
// which a chassis on another cluster cannot reach, so it dials the Southbound
// node address instead.
func TestReconcileCentral_CrossClusterUnpublishedRelayFallsBackToTheSouthbound(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	central := crossClusterOVNCentral()
	central.Spec.Relay = &ovnv1alpha1.OVNRelaySpec{Replicas: 1}
	central.Status.RelayAddress = testRelayAddress
	r := newTestOVNChassisReconciler(t, cr, central)

	resolved, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(resolved.ovnRemote).To(Equal(testSouthboundNodeAddress))
}

// Both CRs naming the same target cluster is the multi-cluster pairing that
// works, so the step resolves rather than refusing.
func TestReconcileCentral_SameTargetNameIsAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	central := resolvableOVNCentral()
	central.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	r := newTestOVNChassisReconciler(t, cr, central)

	resolved, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(resolved.sbAddress).To(Equal(testSouthboundAddress))
	g.Expect(resolved.sameCluster).To(BeTrue())
	g.Expect(resolved.clientSecretName).To(Equal(testClientSecretName),
		"a chassis on its central's cluster mounts the central's Secret directly")
	g.Expect(ovnChassisCondition(cr, conditionTypeCentralReady).Status).To(Equal(metav1.ConditionTrue))
}

// Without a Southbound address there is nothing for ovn-controller to dial, so
// the step waits at the Raft cadence rather than projecting a chassis pointed
// at the empty string.
func TestReconcileCentral_EmptySouthboundAddressWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	central := resolvableOVNCentral()
	central.Status.Southbound.InternalDbAddress = ""
	r := newTestOVNChassisReconciler(t, cr, central)

	resolved, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	g.Expect(resolved).To(Equal(resolvedCentral{}))

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCentralNotReady))
}

// Without the client Secret the chassis containers have nothing to authenticate
// with, which is the same wait.
func TestReconcileCentral_EmptyClientSecretWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	central := resolvableOVNCentral()
	central.Status.ClientSecretName = ""
	r := newTestOVNChassisReconciler(t, cr, central)

	_, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	g.Expect(ovnChassisCondition(cr, conditionTypeCentralReady).Reason).
		To(Equal(conditionReasonCentralNotReady))
}

// A central that runs relays hands them to the chassis. Every chassis holds an
// open Southbound connection, and taking those off the Raft leader is the whole
// reason the relay tier exists.
func TestReconcileCentral_RelayPreferredOverTheSouthboundAddress(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	central := resolvableOVNCentral()
	central.Status.RelayAddress = testRelayAddress
	r := newTestOVNChassisReconciler(t, cr, central)

	resolved, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(resolved).To(Equal(resolvedCentral{
		ovnRemote:              testRelayAddress,
		nbAddress:              testNorthboundAddress,
		sbAddress:              testSouthboundAddress,
		clientSecretName:       testClientSecretName,
		sameCluster:            true,
		sourceClientSecretName: testClientSecretName,
	}))
	g.Expect(resolved.sbAddress).NotTo(Equal(resolved.ovnRemote),
		"the deregistration Job still addresses the database itself")

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonCentralResolved))
	g.Expect(cond.Message).To(ContainSubstring(testRelayAddress))
}

// Without relays the chassis connect to the Southbound database directly.
func TestReconcileCentral_WithoutARelayUsesTheSouthboundAddress(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, resolvableOVNCentral())

	resolved, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(resolved.ovnRemote).To(Equal(testSouthboundAddress))
	g.Expect(resolved.sameCluster).To(BeTrue())
	g.Expect(resolved.clientSecretName).To(Equal(testClientSecretName))
	g.Expect(ovnChassisCondition(cr, conditionTypeCentralReady).Message).
		To(ContainSubstring(testSouthboundAddress))
}

// A central whose own rollout has not reached northd yet holds the whole chassis
// pipeline. OVN upgrades central first and hypervisors second, and the two
// controllers reconcile independently: without this gate an operator upgrade
// that moves both CRs onto a new default image lets the DaemonSets finish their
// rolling update while the databases are still going member by member, leaving
// every hypervisor on an ovn-controller newer than the Southbound schema it
// reads.
func TestReconcileCentral_HoldsWhileTheCentralIsStillRollingOut(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	central := resolvableOVNCentral()
	central.Status.InstalledImage = "ghcr.io/c5c3/ovn:previous"
	r := newTestOVNChassisReconciler(t, cr, central)

	resolved, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	g.Expect(resolved).To(Equal(resolvedCentral{}))

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCentralUpgrading))
	g.Expect(cond.Message).To(ContainSubstring(effectiveImage(nil).Reference()))
}

// A chassis pinned to an older image than the central is the direction OVN
// supports, so the gate compares the central against itself rather than against
// the chassis.
func TestReconcileCentral_ChassisMayLagTheCentralImage(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	cr.Spec.Image = &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/ovn", Tag: "previous"}
	r := newTestOVNChassisReconciler(t, cr, resolvableOVNCentral())

	_, res, err := r.reconcileCentral(ctx, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(ovnChassisCondition(cr, conditionTypeCentralReady).Reason).
		To(Equal(conditionReasonCentralResolved))
}
