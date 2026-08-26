// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// chassisControllerDaemonSet is the key of the ovn-controller DaemonSet of the
// shared fixture.
var chassisControllerDaemonSet = chassisKey(testOVNChassisName + "-ovn-controller")

// testResolvedCentral is what the central step resolves for the shared fixture:
// a control plane without a relay, so the chassis dial the Southbound database
// itself.
func testResolvedCentral() resolvedCentral {
	return resolvedCentral{
		ovnRemote:        testSouthboundAddress,
		nbAddress:        testNorthboundAddress,
		sbAddress:        testSouthboundAddress,
		clientSecretName: "ovn-client",
	}
}

// A rollout in flight polls, mirrors the counters it found, and leaves the
// installed image alone: the image belongs to the pods that run, and on two of
// the three nodes the previous ones still do.
func TestReconcileController_ProgressingMirrorsTheCounters(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, rollingOutDaemonSet(t, cr, chassisControllerDaemonSet.Name))

	res, err := r.reconcileController(ctx, r.Client, cr, testResolvedCentral())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))

	cond := ovnChassisCondition(cr, conditionTypeControllerReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetProgressing))
	g.Expect(cond.Message).To(ContainSubstring("1 of 3 nodes"))

	g.Expect(cr.Status.DesiredNumberScheduled).To(BeEquivalentTo(3))
	g.Expect(cr.Status.NumberReady).To(BeEquivalentTo(1))
	g.Expect(cr.Status.InstalledImage).To(BeEmpty(),
		"the installed image is the image the nodes run, and two of them still run the previous one")
}

// A rollout that reached every node records the image those nodes run. It is
// stamped on this arm only, which is what tells a chassis that has picked up a
// new image from one that has only been handed it.
func TestReconcileController_ReadyRecordsTheInstalledImage(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, rollingOutDaemonSet(t, cr, chassisControllerDaemonSet.Name))

	_, err := r.reconcileController(ctx, r.Client, cr, testResolvedCentral())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cr.Status.InstalledImage).To(BeEmpty(), "the rollout has reached one of three nodes")
	g.Expect(simulators.MarkDaemonSetReady(ctx, r.Client, chassisControllerDaemonSet)).To(Succeed())

	res, err := r.reconcileController(ctx, r.Client, cr, testResolvedCentral())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a rolled-out DaemonSet is not polled")

	cond := ovnChassisCondition(cr, conditionTypeControllerReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetReady))

	g.Expect(cr.Status.DesiredNumberScheduled).To(BeEquivalentTo(3))
	g.Expect(cr.Status.NumberReady).To(BeEquivalentTo(3))
	g.Expect(cr.Status.InstalledImage).To(Equal(defaultOVNRepository + ":" + defaultOVNVersion))
}

// A target cluster that grants the operator no daemonsets verb fails here. The
// condition has to flip on that pass: the aggregate Ready is re-derived from the
// sub-conditions at the new observedGeneration, so a condition left untouched
// would report the failed pass as ready.
func TestReconcileController_ApplyErrorIsDaemonSetError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()

	c := ovnChassisFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if kinded, ok := obj.(kindedApplyConfiguration); ok && kinded.GetKind() == "DaemonSet" {
					return apierrors.NewForbidden(appsv1.Resource("daemonsets"), chassisControllerDaemonSet.Name, nil)
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileController(ctx, r.Client, cr, testResolvedCentral())

	g.Expect(err).To(MatchError(ContainSubstring("ensuring ovn-controller DaemonSet")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnChassisCondition(cr, conditionTypeControllerReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetError))
	g.Expect(cr.Status.DesiredNumberScheduled).To(BeEquivalentTo(0),
		"a pass that could not apply the DaemonSet has no counters to mirror")
	g.Expect(cr.Status.InstalledImage).To(BeEmpty())
}

// The four variables the init container writes into the node's local database
// and ovn-controller reads back from it. The node's name and address come from
// the downward API: both are per-pod facts that no ConfigMap could carry without
// being rewritten whenever a node's address changes.
func TestChassisEnv_CarriesTheNodeIdentityAndTheRemote(t *testing.T) {
	g := NewGomegaWithT(t)

	env := chassisEnv(testOVNChassis(), testResolvedCentral())

	g.Expect(env).To(HaveLen(4))
	g.Expect(env[0].Name).To(Equal("NODE_NAME"))
	g.Expect(env[0].ValueFrom.FieldRef.FieldPath).To(Equal("spec.nodeName"))
	g.Expect(env[1].Name).To(Equal("NODE_IP"))
	g.Expect(env[1].ValueFrom.FieldRef.FieldPath).To(Equal("status.hostIP"))
	g.Expect(env[2]).To(Equal(corev1.EnvVar{Name: "OVN_REMOTE", Value: testSouthboundAddress}))
	g.Expect(env[3]).To(Equal(corev1.EnvVar{Name: "OVN_REMOTE_PROBE_INTERVAL_MS", Value: "60000"}))
}

// A probe interval of zero disables the Southbound probe, which is what a
// chassis behind a connection-tracking middlebox needs. It has to reach the node
// as "0": an empty value would leave the node on the OVN default of five
// seconds, the opposite of what the CR asks for.
func TestChassisEnv_ZeroProbeIntervalReachesTheNode(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	cr.Spec.RemoteProbeIntervalMs = 0

	env := chassisEnv(cr, testResolvedCentral())

	g.Expect(env[3]).To(Equal(corev1.EnvVar{Name: "OVN_REMOTE_PROBE_INTERVAL_MS", Value: "0"}))
}

// The client Secret is mounted under the name the central published rather than
// one the chassis derives: the two CRs may carry unrelated names, and a chassis
// that guessed would wait on a volume that never mounts.
func TestBuildControllerDaemonSet_MountsTheCentralsClientSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	central := testResolvedCentral()
	central.clientSecretName = "some-other-client"

	ds := buildControllerDaemonSet(testOVNChassis(), central)

	var mounted string
	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Name == tlsVolumeName {
			mounted = volume.Secret.SecretName
		}
	}
	g.Expect(mounted).To(Equal("some-other-client"))
}

// The remote the chassis dial is the central's, relay or database. It reaches
// both containers, because the init container writes it into the local database
// and ovn-controller is what connects to it.
func TestBuildControllerDaemonSet_CarriesTheRemoteInBothContainers(t *testing.T) {
	g := NewGomegaWithT(t)
	central := testResolvedCentral()
	central.ovnRemote = "ssl:10.96.0.77:6642"

	ds := buildControllerDaemonSet(testOVNChassis(), central)

	remote := corev1.EnvVar{Name: "OVN_REMOTE", Value: "ssl:10.96.0.77:6642"}
	g.Expect(ds.Spec.Template.Spec.InitContainers[0].Env).To(ContainElement(remote))
	g.Expect(ds.Spec.Template.Spec.Containers[0].Env).To(ContainElement(remote))
}

// The two DaemonSets of one CR must not select each other's pods. Their
// selectors are immutable once applied, so a component label missing here would
// wedge both of them for the life of the CR.
func TestChassisSelectorLabels_NarrowTheTwoDaemonSetsApart(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()

	ovs := buildOVSDaemonSet(cr).Spec.Selector.MatchLabels
	controller := buildControllerDaemonSet(cr, testResolvedCentral()).Spec.Selector.MatchLabels

	g.Expect(ovs).NotTo(Equal(controller))
	g.Expect(ovs).To(HaveKeyWithValue("app.kubernetes.io/component", componentOVS))
	g.Expect(controller).To(HaveKeyWithValue("app.kubernetes.io/component", componentOVNController))
	g.Expect(ovs).To(HaveKeyWithValue("app.kubernetes.io/instance", cr.Name))
}

// A CR that names no resources renders none rather than a zero-valued block, so
// the containers land in the BestEffort class the cluster's own defaults may
// then override.
func TestChassisResources_UnsetRendersNone(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(chassisResources(nil)).To(Equal(corev1.ResourceRequirements{}))
	g.Expect(chassisResources(&ovnv1alpha1.OVNChassisContainerSpec{})).To(Equal(corev1.ResourceRequirements{}))
}
