// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/job"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The names the three maintenance Jobs of the shared fixture take, spelled out
// so a test asserts against the name an operator would type rather than against
// the builder that produced it.
const (
	testApplyJobNodeA      = testOVNChassisName + "-apply-" + pinNodeAHash
	testEvacuateJobNodeA   = testOVNChassisName + "-evacuate-" + pinNodeAHash
	testChassisDelJobNodeB = testOVNChassisName + "-chassis-del-" + pinNodeBHash
)

// testStaleHash is a recorded hash no rendering produces, which is what makes a
// node look like one whose values have moved on since it last applied them.
const testStaleHash = "deadbeef"

// maintenanceChassis is the fixture the maintenance tests run against. It
// carries a UID, because the fake client mints none and the Jobs need an owner
// reference that resolves.
func maintenanceChassis() *ovnv1alpha1.OVNChassis {
	cr := testOVNChassis()
	cr.UID = "ovn-chassis-uid"
	return cr
}

// maintenanceCentral is the resolved OVNCentral the tests are parameterised by:
// both database addresses published and a client Secret to mount.
func maintenanceCentral() resolvedCentral {
	return resolvedCentral{
		ovnRemote:        testSouthboundAddress,
		nbAddress:        testNorthboundAddress,
		sbAddress:        testSouthboundAddress,
		clientSecretName: "ovn-client",
	}
}

// chassisEntry is one rendered node entry: the shape the node step produces for
// a selected node.
func chassisEntry(systemID string, gateway bool) nodeEntry {
	return nodeEntry{systemID: systemID, gateway: gateway, encapType: "geneve"}
}

// renderMaintenanceStatus does to the CR what the node step does right before
// the maintenance step runs: it snapshots the status as the pass read it and
// rebuilds status.nodes from the rendered entries. Calling it once per pass is
// what makes a multi-pass test see the state the pipeline would carry over,
// rather than a snapshot frozen at the first pass.
func renderMaintenanceStatus(cr *ovnv1alpha1.OVNChassis, nodes renderedNodes) *ovnv1alpha1.OVNChassisStatus {
	before := cr.Status.DeepCopy()
	cr.Status.Nodes = nodeStatuses(cr, nodes)
	return before
}

// maintenanceJobNames lists the Jobs in the fixture namespace by name.
func maintenanceJobNames(ctx context.Context, t *testing.T, c client.Client) []string {
	t.Helper()

	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("listing the maintenance Jobs: %v", err)
	}
	names := make([]string, 0, len(jobs.Items))
	for i := range jobs.Items {
		names = append(names, jobs.Items[i].Name)
	}
	return names
}

// readMaintenanceJob reads one Job back, failing the test when it is absent.
func readMaintenanceJob(ctx context.Context, t *testing.T, c client.Client, name string) *batchv1.Job {
	t.Helper()

	var jobObj batchv1.Job
	if err := c.Get(ctx, chassisKey(name), &jobObj); err != nil {
		t.Fatalf("reading Job %s back: %v", name, err)
	}
	return &jobObj
}

// nodeStatusByName returns one entry of status.nodes, or nil.
func nodeStatusByName(cr *ovnv1alpha1.OVNChassis, name string) *ovnv1alpha1.OVNChassisNodeStatus {
	for i := range cr.Status.Nodes {
		if cr.Status.Nodes[i].Name == name {
			return &cr.Status.Nodes[i]
		}
	}
	return nil
}

// containerEnv indexes the environment of a Job's single container.
func containerEnv(jobObj *batchv1.Job) map[string]string {
	env := make(map[string]string)
	for _, v := range jobObj.Spec.Template.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	return env
}

// A node seen for the first time has no recorded hash, and its values are
// applied by the init container of its own ovn-controller pod. Running a Job for
// it would duplicate that work on every node the CR ever selects.
func TestReconcileMaintenance_FirstRenderRunsNoApplyJob(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := maintenanceChassis()
	entry := chassisEntry(testFixedSystemID, false)
	nodes := renderedNodes{testNodeA: entry}
	before := renderMaintenanceStatus(cr, nodes)
	r := newTestOVNChassisReconciler(t, cr)

	res, err := r.reconcileMaintenance(ctx, r.Client, cr, before, maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(maintenanceJobNames(ctx, t, r.Client)).To(BeEmpty())
	g.Expect(nodeStatusByName(cr, testNodeA).ConfigHash).To(Equal(entry.hash()),
		"a first entry counts as applied, because the init container applies it before ovn-controller starts")

	cond := ovnChassisCondition(cr, conditionTypeMaintenanceReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonMaintenanceIdle))
}

// A recorded hash that no longer matches the rendered one is a node running
// values the operator has moved on from. The Job is pinned to that node, because
// what it writes is the node's own database.
func TestReconcileMaintenance_ChangedHashRunsPinnedApplyJob(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := maintenanceChassis()
	entry := chassisEntry(testFixedSystemID, false)
	nodes := renderedNodes{testNodeA: entry}
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{
		{Name: testNodeA, SystemID: testFixedSystemID, ConfigHash: testStaleHash},
	}
	r := newTestOVNChassisReconciler(t, cr)

	res, err := r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	g.Expect(nodeStatusByName(cr, testNodeA).ConfigHash).To(Equal(testStaleHash),
		"the drift has to survive the pass, or the node would count as applied while the Job still runs")

	applyJobObj := readMaintenanceJob(ctx, t, r.Client, testApplyJobNodeA)
	g.Expect(applyJobObj.Spec.Template.Spec.NodeName).To(Equal(testNodeA))
	g.Expect(applyJobObj.Spec.Template.Spec.HostNetwork).To(BeTrue())
	g.Expect(applyJobObj.Annotations).To(HaveKeyWithValue(job.PodSpecHashAnnotation, entry.hash()),
		"the rerun key is the hash the Job applies, so a further change reruns it")

	cond := ovnChassisCondition(cr, conditionTypeMaintenanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonMaintenanceRunning))
	g.Expect(cond.Message).To(ContainSubstring(testNodeA))

	// The Job finishes, and only then does the node count as running the values
	// that were rendered for it.
	g.Expect(simulators.SimulateJobComplete(ctx, r.Client, chassisKey(testApplyJobNodeA))).To(Succeed())

	res, err = r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(nodeStatusByName(cr, testNodeA).ConfigHash).To(Equal(entry.hash()))
	g.Expect(ovnChassisCondition(cr, conditionTypeMaintenanceReady).Reason).
		To(Equal(conditionReasonMaintenanceIdle))
}

// The whole gateway hand-over, pass by pass. The apply runs first so the chassis
// stops announcing itself as a gateway before the bindings are taken off it; an
// evacuation that ran the other way round would hand them straight back.
func TestReconcileMaintenance_GatewayFlipAppliesBeforeItEvacuates(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := maintenanceChassis()
	gateway := chassisEntry(testFixedSystemID, true)
	plain := chassisEntry(testFixedSystemID, false)
	nodes := renderedNodes{testNodeA: plain}
	// The node as the previous pass left it: a gateway running its own values.
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{
		{Name: testNodeA, SystemID: testFixedSystemID, Gateway: true, ConfigHash: gateway.hash()},
	}
	r := newTestOVNChassisReconciler(t, cr)

	// Pass one: the apply Job alone.
	_, err := r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(maintenanceJobNames(ctx, t, r.Client)).To(ConsistOf(testApplyJobNodeA),
		"the evacuation must wait for the node to stop announcing the role")
	g.Expect(nodeStatusByName(cr, testNodeA).Gateway).To(BeTrue(),
		"the node still announces the role until the evacuation has landed")

	// Pass two: the apply has landed, so the evacuation may start.
	g.Expect(simulators.SimulateJobComplete(ctx, r.Client, chassisKey(testApplyJobNodeA))).To(Succeed())

	res, err := r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	g.Expect(maintenanceJobNames(ctx, t, r.Client)).
		To(ConsistOf(testApplyJobNodeA, testEvacuateJobNodeA))
	g.Expect(nodeStatusByName(cr, testNodeA).ConfigHash).To(Equal(plain.hash()))
	g.Expect(nodeStatusByName(cr, testNodeA).GatewayEvacuated).To(BeFalse())

	evacuate := readMaintenanceJob(ctx, t, r.Client, testEvacuateJobNodeA)
	g.Expect(evacuate.Spec.Template.Spec.NodeName).To(BeEmpty(),
		"the evacuation edits the logical model, so it runs wherever there is room")
	g.Expect(containerEnv(evacuate)).To(Equal(map[string]string{
		"NB_ADDR": testNorthboundAddress,
		"CHASSIS": testFixedSystemID,
	}))
	g.Expect(evacuate.Annotations).To(HaveKeyWithValue(job.PodSpecHashAnnotation,
		testFixedSystemID+":gateway-off:"+plain.hash()))

	// Pass three: the evacuation succeeded, so the node is drained.
	g.Expect(simulators.SimulateJobComplete(ctx, r.Client, chassisKey(testEvacuateJobNodeA))).To(Succeed())

	res, err = r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(nodeStatusByName(cr, testNodeA).GatewayEvacuated).To(BeTrue())
	g.Expect(nodeStatusByName(cr, testNodeA).Gateway).To(BeFalse())
	g.Expect(ovnChassisCondition(cr, conditionTypeMaintenanceReady).Reason).
		To(Equal(conditionReasonMaintenanceIdle))

	// Pass four: nothing is outstanding, and no Job is scheduled a second time.
	_, err = r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(maintenanceJobNames(ctx, t, r.Client)).
		To(ConsistOf(testApplyJobNodeA, testEvacuateJobNodeA))
}

// A node that takes the gateway role back is no longer drained. Leaving the flag
// set would make a re-promoted node look like one whose bindings are still off
// it, and the next demotion would skip its evacuation.
func TestReconcileMaintenance_GatewayReturnResetsEvacuated(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := maintenanceChassis()
	gateway := chassisEntry(testFixedSystemID, true)
	nodes := renderedNodes{testNodeA: gateway}
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{{
		Name:             testNodeA,
		SystemID:         testFixedSystemID,
		ConfigHash:       gateway.hash(),
		GatewayEvacuated: true,
	}}
	r := newTestOVNChassisReconciler(t, cr)

	res, err := r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(maintenanceJobNames(ctx, t, r.Client)).To(BeEmpty())
	g.Expect(nodeStatusByName(cr, testNodeA).GatewayEvacuated).To(BeFalse())
	g.Expect(ovnChassisCondition(cr, conditionTypeMaintenanceReady).Reason).
		To(Equal(conditionReasonMaintenanceIdle))
}

// A node that left is deregistered, and only once that has succeeded do its
// ConfigMap key and its status entry go. Dropping them earlier would lose the
// system-id the deletion addresses.
func TestReconcileMaintenance_LeavingNodeIsDeregistered(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := maintenanceChassis()
	staying := chassisEntry("aaaaaaaa-1111-2222-3333-444444444444", false)
	leaving := chassisEntry(testFixedSystemID, false)
	leaving.leaving = true
	nodes := renderedNodes{testNodeA: staying, testNodeB: leaving}
	r := newTestOVNChassisReconciler(t, cr)
	// The ConfigMap has to be applied by the shared field manager first, which is
	// what lets the re-apply below shed the key it no longer asserts.
	g.Expect(r.ensureNodeConfigMaps(ctx, r.Client, cr, nodes)).To(Succeed())

	res, err := r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	g.Expect(maintenanceJobNames(ctx, t, r.Client)).To(ConsistOf(testChassisDelJobNodeB))
	g.Expect(readNodesConfigMap(ctx, t, r).Data).To(HaveKey(testNodeB),
		"the record of what to deregister must outlive the pass that started the deletion")

	chassisDel := readMaintenanceJob(ctx, t, r.Client, testChassisDelJobNodeB)
	g.Expect(containerEnv(chassisDel)).To(Equal(map[string]string{
		"SB_ADDR": testSouthboundAddress,
		"CHASSIS": testFixedSystemID,
	}))
	g.Expect(chassisDel.Annotations).To(HaveKeyWithValue(job.PodSpecHashAnnotation,
		testFixedSystemID+":leave"))

	g.Expect(simulators.SimulateJobComplete(ctx, r.Client, chassisKey(testChassisDelJobNodeB))).To(Succeed())

	res, err = r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	data := readNodesConfigMap(ctx, t, r).Data
	g.Expect(data).To(HaveKey(testNodeA))
	g.Expect(data).NotTo(HaveKey(testNodeB))
	g.Expect(cr.Status.Nodes).To(HaveLen(1))
	g.Expect(cr.Status.Nodes[0].Name).To(Equal(testNodeA))
	g.Expect(ovnChassisCondition(cr, conditionTypeMaintenanceReady).Reason).
		To(Equal(conditionReasonMaintenanceIdle))
}

// A failed Job is a state no further pass can improve on: its rerun key is
// unchanged, so it stays failed. Returning an error would put the CR on the
// workqueue's backoff and hot-loop a pass that cannot help, so it reports
// through the condition instead, and the Warning fires once per Job.
func TestReconcileMaintenance_FailedJobReportsWithoutHotLooping(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := maintenanceChassis()
	entry := chassisEntry(testFixedSystemID, false)
	nodes := renderedNodes{testNodeA: entry}
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{
		{Name: testNodeA, SystemID: testFixedSystemID, ConfigHash: testStaleHash},
	}
	failed := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testApplyJobNodeA,
			Namespace:   testNamespace,
			UID:         types.UID(testApplyJobNodeA + "-uid"),
			Annotations: map[string]string{job.PodSpecHashAnnotation: entry.hash()},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type:               batchv1.JobFailed,
			Status:             corev1.ConditionTrue,
			Reason:             "BackoffLimitExceeded",
			LastTransitionTime: metav1.Now(),
		}}},
	}
	r := newTestOVNChassisReconciler(t, cr, failed)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	res, err := r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred(),
		"a failed Job is a status signal, not a reconcile error: retrying the pass cannot fix it")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnChassisCondition(cr, conditionTypeMaintenanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonMaintenanceJobFailed))
	g.Expect(cond.Message).To(ContainSubstring(testApplyJobNodeA))
	g.Expect(cond.Message).To(ContainSubstring(testNodeA))
	g.Expect(cr.Annotations).To(HaveKey(job.JobUIDAnnotationKey(componentMaintenance)))

	// A second pass observes the same Job. Nothing new may come of it.
	_, err = r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(recorder.Events).To(Receive(And(
		ContainSubstring(corev1.EventTypeWarning),
		ContainSubstring(eventReasonMaintenanceJobFailed),
		ContainSubstring(testApplyJobNodeA),
		ContainSubstring(testNodeA),
	)))
	g.Expect(recorder.Events).NotTo(Receive(),
		"the Warning must fire once per Job, not once per pass")
}

// Everything that is not a failed Job is the caller's to return: a cluster that
// refuses the create is a pass that has to be retried, and the condition has to
// flip so the aggregate Ready is not left stale-True at the new generation.
func TestReconcileMaintenance_JobCreateErrorIsMaintenanceError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := maintenanceChassis()
	nodes := renderedNodes{testNodeA: chassisEntry(testFixedSystemID, false)}
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{
		{Name: testNodeA, SystemID: testFixedSystemID, ConfigHash: testStaleHash},
	}

	c := ovnChassisFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*batchv1.Job); ok {
					return apierrors.NewForbidden(batchv1.Resource("jobs"), obj.GetName(), nil)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).To(MatchError(ContainSubstring("running apply Job " + testApplyJobNodeA)))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnChassisCondition(cr, conditionTypeMaintenanceReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonMaintenanceError))
}

// The two Jobs that talk to a central database need an address the OVNCentral
// publishes. Starting one without it would leave a pod retrying against an empty
// target, so the step waits and says which node it is waiting for.
func TestReconcileMaintenance_MissingAddressDefersTheJob(t *testing.T) {
	plain := chassisEntry(testFixedSystemID, false)
	leaving := chassisEntry(testFixedSystemID, false)
	leaving.leaving = true

	cases := []struct {
		name    string
		central resolvedCentral
		entry   nodeEntry
		status  ovnv1alpha1.OVNChassisNodeStatus
	}{
		{
			name:    "evacuate without a Northbound address",
			central: resolvedCentral{sbAddress: testSouthboundAddress, clientSecretName: "ovn-client"},
			entry:   plain,
			status: ovnv1alpha1.OVNChassisNodeStatus{
				Name: testNodeA, SystemID: testFixedSystemID, Gateway: true, ConfigHash: plain.hash(),
			},
		},
		{
			name:    "chassis-del without a Southbound address",
			central: resolvedCentral{nbAddress: testNorthboundAddress, clientSecretName: "ovn-client"},
			entry:   leaving,
			status: ovnv1alpha1.OVNChassisNodeStatus{
				Name: testNodeA, SystemID: testFixedSystemID, ConfigHash: leaving.hash(),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cr := maintenanceChassis()
			nodes := renderedNodes{testNodeA: tc.entry}
			cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{tc.status}
			r := newTestOVNChassisReconciler(t, cr)

			res, err := r.reconcileMaintenance(ctx, r.Client, cr,
				renderMaintenanceStatus(cr, nodes), tc.central, nodes)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
			g.Expect(maintenanceJobNames(ctx, t, r.Client)).To(BeEmpty())

			cond := ovnChassisCondition(cr, conditionTypeMaintenanceReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(conditionReasonMaintenanceDeferred))
			g.Expect(cond.Message).To(ContainSubstring(testNodeA))
			g.Expect(cond.Message).To(ContainSubstring(testOVNCentralName))
		})
	}
}

// One node waiting on a Job must not hold the rest of the cluster up: a chassis
// that left keeps claiming the ports of workloads that have moved on, and a slow
// apply somewhere else is no reason to leave it registered.
func TestReconcileMaintenance_PendingJobDoesNotBlockAnotherNode(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := maintenanceChassis()
	drifted := chassisEntry(testFixedSystemID, false)
	leaving := chassisEntry("bbbbbbbb-1111-2222-3333-444444444444", false)
	leaving.leaving = true
	nodes := renderedNodes{testNodeA: drifted, testNodeB: leaving}
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{
		{Name: testNodeA, SystemID: testFixedSystemID, ConfigHash: testStaleHash},
		{Name: testNodeB, SystemID: leaving.systemID, ConfigHash: leaving.hash(), Leaving: true},
	}
	r := newTestOVNChassisReconciler(t, cr)
	g.Expect(r.ensureNodeConfigMaps(ctx, r.Client, cr, nodes)).To(Succeed())

	res, err := r.reconcileMaintenance(ctx, r.Client, cr,
		renderMaintenanceStatus(cr, nodes), maintenanceCentral(), nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))
	g.Expect(maintenanceJobNames(ctx, t, r.Client)).
		To(ConsistOf(testApplyJobNodeA, testChassisDelJobNodeB))
	g.Expect(ovnChassisCondition(cr, conditionTypeMaintenanceReady).Message).
		To(ContainSubstring(testNodeA + ", " + testNodeB))
}
