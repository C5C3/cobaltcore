// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// agentCentral is the OVNCentral the chassis fixture attaches to, publishing the
// two values the agent cannot start without.
func agentCentral() *ovnv1alpha1.OVNCentral {
	return readyOVNCentral(testOVNCentralName, testNamespace,
		testNorthboundAddress, testSouthboundAddress, "ovn-client")
}

// An agent applied before its OVNChassis polls rather than failing the pass: the
// two objects commonly arrive in one manifest, in whichever order the apply
// walked it.
func TestReconcileChassis_ChassisNotFoundPolls(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	r := newAgentTestReconciler(cr) // no OVNChassis seeded

	chassis, res, err := r.reconcileChassis(context.Background(), cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(chassis).To(Equal(resolvedChassis{}))

	cond := agentCondition(cr, conditionTypeChassisReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonChassisNotFound))
	g.Expect(cond.Message).To(ContainSubstring(testOVNChassisName))
}

// A chassis on another cluster is a spec error rather than a wait: the agent
// shares the chassis's nodes and mounts a Secret from its cluster, and both refs
// are immutable, so polling would never repair it.
func TestReconcileChassis_ChassisOnAnotherClusterDoesNotRequeue(t *testing.T) {
	tests := []struct {
		name         string
		agentRef     *commonv1.TargetClusterRefSpec
		chassisRef   *commonv1.TargetClusterRefSpec
		wantMessages []string
	}{
		{
			name:         "the agent is local while the chassis is placed",
			chassisRef:   &commonv1.TargetClusterRefSpec{Name: "edge-1"},
			wantMessages: []string{"target cluster edge-1", "the management cluster"},
		},
		{
			name:         "both are placed, on different clusters",
			agentRef:     &commonv1.TargetClusterRefSpec{Name: "edge-2"},
			chassisRef:   &commonv1.TargetClusterRefSpec{Name: "edge-1"},
			wantMessages: []string{"target cluster edge-1", "target cluster edge-2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := validAgent()
			cr.Spec.TargetClusterRef = tc.agentRef
			chassisCR := readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName)
			chassisCR.Spec.TargetClusterRef = tc.chassisRef
			r := newAgentTestReconciler(cr, chassisCR, agentCentral())

			chassis, res, err := r.reconcileChassis(context.Background(), cr)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue(), "a mismatched pair is not something to poll")
			g.Expect(chassis).To(Equal(resolvedChassis{}))

			cond := agentCondition(cr, conditionTypeChassisReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonChassisOnAnotherCluster))
			for _, want := range tc.wantMessages {
				g.Expect(cond.Message).To(ContainSubstring(want))
			}
		})
	}
}

// Two CRs naming the same cluster, and two naming none, both pass the gate: the
// rule is that they agree, not that they are placed.
func TestReconcileChassis_MatchingTargetClustersResolve(t *testing.T) {
	tests := []struct {
		name string
		ref  *commonv1.TargetClusterRefSpec
	}{
		{name: "both keep their children on the management cluster"},
		{name: "both project onto the same target cluster", ref: &commonv1.TargetClusterRefSpec{Name: "edge-1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := validAgent()
			cr.Spec.TargetClusterRef = tc.ref
			chassisCR := readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName)
			chassisCR.Spec.TargetClusterRef = tc.ref
			r := newAgentTestReconciler(cr, chassisCR, agentCentral())

			chassis, res, err := r.reconcileChassis(context.Background(), cr)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue())
			g.Expect(chassis.sbAddress).To(Equal(testSouthboundAddress))
			g.Expect(agentCondition(cr, conditionTypeChassisReady).Reason).To(Equal(conditionReasonChassisResolved))
		})
	}
}

// The central the chassis attaches to is read in the agent's own namespace. It
// may not exist yet, which polls the same way a missing chassis does.
func TestReconcileChassis_CentralNotFoundPolls(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	r := newAgentTestReconciler(cr, readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName))

	chassis, res, err := r.reconcileChassis(context.Background(), cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(chassis).To(Equal(resolvedChassis{}))

	cond := agentCondition(cr, conditionTypeChassisReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCentralNotFound))
	g.Expect(cond.Message).To(ContainSubstring(testOVNCentralName))
}

// Either published value missing is enough to hold the pipeline: the agent has
// nothing to read the logical model from without the address, and nothing to
// authenticate with without the Secret.
func TestReconcileChassis_CentralNotReadyPolls(t *testing.T) {
	tests := []struct {
		name             string
		sbAddress        string
		clientSecretName string
	}{
		{name: "the Southbound address is not published yet", clientSecretName: "ovn-client"},
		{name: "the client Secret is not named yet", sbAddress: testSouthboundAddress},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := validAgent()
			central := readyOVNCentral(testOVNCentralName, testNamespace,
				testNorthboundAddress, tc.sbAddress, tc.clientSecretName)
			r := newAgentTestReconciler(cr,
				readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName), central)

			chassis, res, err := r.reconcileChassis(context.Background(), cr)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
			g.Expect(chassis).To(Equal(resolvedChassis{}))

			cond := agentCondition(cr, conditionTypeChassisReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonCentralNotReady))
		})
	}
}

// A read the API server refuses is not a wait: it fails the pass so the
// workqueue retries with backoff, and the condition has to flip on that pass
// because the aggregate Ready is re-derived at the new observedGeneration.
func TestReconcileChassis_ReadErrorsAreWrapped(t *testing.T) {
	tests := []struct {
		name       string
		refuseKind string
		wantReason string
		wantMsg    string
	}{
		{
			name:       "the OVNChassis read fails",
			refuseKind: "OVNChassis",
			wantReason: conditionReasonChassisReadError,
			wantMsg:    "reading OVNChassis " + testNamespace + "/" + testOVNChassisName,
		},
		{
			name:       "the OVNCentral read fails",
			refuseKind: "OVNCentral",
			wantReason: conditionReasonCentralReadError,
			wantMsg:    "reading OVNCentral " + testNamespace + "/" + testOVNCentralName,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := validAgent()
			c := neutronFakeClientBuilder(cr,
				readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName), agentCentral()).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
						obj client.Object, opts ...client.GetOption,
					) error {
						switch obj.(type) {
						case *ovnv1alpha1.OVNChassis:
							if tc.refuseKind == "OVNChassis" {
								return apierrors.NewServiceUnavailable("cache not started")
							}
						case *ovnv1alpha1.OVNCentral:
							if tc.refuseKind == "OVNCentral" {
								return apierrors.NewServiceUnavailable("cache not started")
							}
						}
						return cl.Get(ctx, key, obj, opts...)
					},
				}).Build()
			r := &NeutronMetadataAgentReconciler{
				Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10),
			}

			chassis, res, err := r.reconcileChassis(context.Background(), cr)

			g.Expect(err).To(MatchError(ContainSubstring(tc.wantMsg)))
			g.Expect(apierrors.IsServiceUnavailable(err)).To(BeTrue(), "the API error must stay unwrappable")
			g.Expect(res.IsZero()).To(BeTrue())
			g.Expect(chassis).To(Equal(resolvedChassis{}))

			cond := agentCondition(cr, conditionTypeChassisReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}

// The node selection is the chassis's, copied verbatim: the agent answers the
// instances on the nodes that chassis programs, and there is no shared label
// constant it could rebuild the selection from.
func TestReconcileChassis_ResolvedCopiesTheNodeSelection(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	chassisCR := readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName)
	r := newAgentTestReconciler(cr, chassisCR, agentCentral())

	chassis, res, err := r.reconcileChassis(context.Background(), cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(chassis.nodeSelector).To(Equal(map[string]string{testChassisNodeLabel: "true"}))
	g.Expect(chassis.tolerations).To(Equal([]corev1.Toleration{{
		Key:      "openstack.c5c3.io/network",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}))
	g.Expect(chassis.sbAddress).To(Equal(testSouthboundAddress))
	g.Expect(chassis.clientSecretName).To(Equal("ovn-client"))

	cond := agentCondition(cr, conditionTypeChassisReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonChassisResolved))

	// The copies are deep: a caller editing what it got back must not reach into
	// the OVNChassis the informer cache holds.
	chassis.nodeSelector["injected"] = "true"
	chassis.tolerations[0].Key = "somewhere-else"
	var stored ovnv1alpha1.OVNChassis
	g.Expect(r.Get(context.Background(), client.ObjectKey{
		Namespace: testNamespace, Name: testOVNChassisName,
	}, &stored)).To(Succeed())
	g.Expect(stored.Spec.NodeSelector).To(Equal(map[string]string{testChassisNodeLabel: "true"}))
	g.Expect(stored.Spec.Tolerations[0].Key).To(Equal("openstack.c5c3.io/network"))
}

// A chassis that tolerates nothing renders no toleration list rather than an
// empty one, which is what a pod spec that tolerates nothing carries.
func TestCopyTolerations_EmptyIsNil(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(copyTolerations(nil)).To(BeNil())
	g.Expect(copyTolerations([]corev1.Toleration{})).To(BeNil())
}
