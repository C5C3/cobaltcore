// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/job"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The names of the FRRConfigurations the two fixture nodes get: the CR name,
// the component and the hash suffix every per-node child carries.
const (
	testFRRConfigurationNodeA = testOVNChassisName + "-fabric-" + pinNodeAHash
	testFRRConfigurationNodeB = testOVNChassisName + "-fabric-" + pinNodeBHash
)

// fabricChassis is the maintenance fixture with spec.fabric set. It carries a
// UID because the ownership labels and the finalizer writes need a CR the fake
// client knows.
func fabricChassis() *ovnv1alpha1.OVNChassis {
	cr := maintenanceChassis()
	cr.Spec.Fabric = testFabric()
	return cr
}

// fabricEntry is the rendered entry of a fabric node, the way renderNodeEntries
// leaves it for a node with a row in testFabric and the frr-k8s label when
// frrK8s is set.
func fabricEntry(frrK8s bool) nodeEntry {
	entry := chassisEntry(testFixedSystemID, false)
	entry.fabric = testNodeFabric()
	entry.asn = 4200000001
	entry.frrK8s = frrK8s
	entry.hostname = testNodeA
	return entry
}

// frrMapper serves the scheme's kinds, plus FRRConfiguration when served is
// set: the RESTMapper of a cluster with and without frr-k8s.
func frrMapper(scheme *runtime.Scheme, served bool) meta.RESTMapper {
	base := testrestmapper.TestOnlyStaticRESTMapper(scheme)
	if !served {
		return base
	}
	frr := meta.NewDefaultRESTMapper(nil)
	frr.Add(frrConfigurationGVK, meta.RESTScopeNamespace)
	return meta.MultiRESTMapper{base, frr}
}

// fabricClientBuilder is the chassis fake client builder for the Fabric step:
// Server-Side Apply of an unregistered kind needs the deduced type converter,
// and served decides whether the client's mapper knows FRRConfiguration.
func fabricClientBuilder(t *testing.T, served bool, objs ...client.Object) *fake.ClientBuilder {
	t.Helper()
	return ovnChassisFakeClientBuilder(t, objs...).
		WithRESTMapper(frrMapper(newTestScheme(t), served)).
		WithTypeConverters(managedfields.NewDeducedTypeConverter())
}

// frrCalls counts the requests a client receives for FRRConfiguration objects.
type frrCalls struct {
	lists, applies, deletes atomic.Int32
}

// any reports whether the client received any request for the kind.
func (c *frrCalls) any() bool {
	return c.lists.Load()+c.applies.Load()+c.deletes.Load() > 0
}

// isFRR reports whether obj is an FRRConfiguration or a list of them.
func isFRR(obj runtime.Object) bool {
	kind := obj.GetObjectKind().GroupVersionKind()
	return kind.Group == frrConfigurationGVK.Group
}

// countingFuncs records the FRRConfiguration requests into calls and delegates
// them. Each hook in override replaces the delegation for its verb, which is
// how a test injects an API error.
func countingFuncs(calls *frrCalls, override interceptor.Funcs) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if isFRR(list) {
				calls.lists.Add(1)
				if override.List != nil {
					return override.List(ctx, c, list, opts...)
				}
			}
			return c.List(ctx, list, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			calls.applies.Add(1)
			if override.Apply != nil {
				return override.Apply(ctx, c, obj, opts...)
			}
			return c.Apply(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if isFRR(obj) {
				calls.deletes.Add(1)
				if override.Delete != nil {
					return override.Delete(ctx, c, obj, opts...)
				}
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
}

// newFabricReconciler builds the reconciler over a local fake client that
// serves FRRConfiguration when served is set, with every FRR request counted.
func newFabricReconciler(t *testing.T, served bool, override interceptor.Funcs,
	objs ...client.Object,
) (*OVNChassisReconciler, *frrCalls) {
	t.Helper()
	calls := &frrCalls{}
	c := fabricClientBuilder(t, served, objs...).WithInterceptorFuncs(countingFuncs(calls, override)).Build()
	return &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(50)}, calls
}

// readFRRConfiguration reads one FRRConfiguration back, or nil when it is
// absent.
func readFRRConfiguration(ctx context.Context, t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(frrConfigurationGVK)
	err := c.Get(ctx, client.ObjectKey{Namespace: frrK8sNamespace, Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading FRRConfiguration %s back: %v", name, err)
	}
	return obj
}

// foreignFRRConfiguration is an FRRConfiguration of the name a fixture node
// wants that nobody's ownership labels mark.
func foreignFRRConfiguration(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"raw": map[string]any{
		"rawConfig": "! hand-written",
	}}}}
	obj.SetGroupVersionKind(frrConfigurationGVK)
	obj.SetNamespace(frrK8sNamespace)
	obj.SetName(name)
	return obj
}

// passFabric runs the Fabric step the way a converged pipeline reaches it, with
// the finalizer installed, so a test starts at the steady state.
func passFabric(ctx context.Context, t *testing.T, r *OVNChassisReconciler, cr *ovnv1alpha1.OVNChassis,
	nodes renderedNodes,
) (ctrl.Result, error) {
	t.Helper()
	res, err := r.reconcileFRRConfigurations(ctx, r.Client, cr, nodes)
	if err == nil && res.RequeueAfter == commonreconcile.RequeueNextPass {
		return r.reconcileFRRConfigurations(ctx, r.Client, cr, nodes)
	}
	return res, err
}

// A fabric node with the frr-k8s label gets an FRRConfiguration in
// frr-k8s-system, with the component and ownership labels and the node
// annotation; a fabric node without the label gets none, while it still counts
// as attached, because its devices are prepared all the same.
func TestReconcileFabric_DesiredSetFollowsTheFRRK8sLabel(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	r, _ := newFabricReconciler(t, true, interceptor.Funcs{}, cr)
	nodeB := fabricEntry(false)
	nodeB.hostname = testNodeB
	nodes := renderedNodes{testNodeA: fabricEntry(true), testNodeB: nodeB}

	res, err := passFabric(ctx, t, r, cr, nodes)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	obj := readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA)
	g.Expect(obj).NotTo(BeNil())
	g.Expect(obj.GetLabels()).To(HaveKeyWithValue(commonmulticluster.OwnerKindLabel, "OVNChassis"))
	g.Expect(obj.GetLabels()).To(HaveKeyWithValue(commonmulticluster.OwnerNameLabel, testOVNChassisName))
	g.Expect(obj.GetLabels()).To(HaveKeyWithValue(commonmulticluster.OwnerNamespaceLabel, testNamespace))
	g.Expect(obj.GetLabels()).To(HaveKeyWithValue("app.kubernetes.io/component", componentFabric))
	g.Expect(obj.GetAnnotations()).To(HaveKeyWithValue(fabricNodeAnnotation, testNodeA))
	g.Expect(obj.GetOwnerReferences()).To(BeEmpty(), "an owner reference cannot cross namespaces")
	g.Expect(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeB)).To(BeNil())
	g.Expect(controllerutil.ContainsFinalizer(getOVNChassis(t, r.Client, testOVNChassisName), fabricFinalizer)).
		To(BeTrue())

	cond := ovnChassisCondition(cr, conditionTypeFabricReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonFabricConfigured))
	g.Expect(cond.Message).To(Equal("Rendered 2 fabric nodes; 1 carry an FRRConfiguration in frr-k8s-system"))
}

// Every range of a family takes the next seq in every list of that family,
// starting at 100, and a range is printed masked.
func TestFabricRawConfig_SeqPerRangeAndFamily(t *testing.T) {
	g := NewGomegaWithT(t)
	fabric := testFabric()
	fabric.AnnounceableRanges = []string{"198.51.100.0/24", "2001:db8::/64", "203.0.113.0/26", "not-a-cidr"}

	raw := fabricRawConfig(fabric, 4200000001)

	g.Expect(raw).To(HaveSuffix("exit\n" +
		"ip prefix-list br-fab1-fabric-allowed-ipv4 seq 100 permit 198.51.100.0/24 ge 32\n" +
		"ip prefix-list br-fab1-fabric-allowed-ipv4 seq 101 permit 203.0.113.0/26 ge 32\n" +
		"ip prefix-list br-fab2-fabric-allowed-ipv4 seq 100 permit 198.51.100.0/24 ge 32\n" +
		"ip prefix-list br-fab2-fabric-allowed-ipv4 seq 101 permit 203.0.113.0/26 ge 32\n" +
		"ipv6 prefix-list br-fab1-fabric-allowed-ipv6 seq 100 permit 2001:db8::/64 ge 128\n" +
		"ipv6 prefix-list br-fab2-fabric-allowed-ipv6 seq 100 permit 2001:db8::/64 ge 128\n"))
	g.Expect(raw).NotTo(ContainSubstring("not-a-cidr"), "a range that does not parse never reaches FRR")
}

// Without an advertised id there is nothing to redistribute and no range to
// let through, so the object carries no raw block and no ovnvrf router of that
// kind.
func TestBuildFRRConfiguration_NoAdvertisedIDNoRawBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := fabricChassis()
	cr.Spec.Fabric.AdvertiseVRFIDs = nil

	obj, err := buildFRRConfiguration(cr, testNodeA, fabricEntry(true), nil)

	g.Expect(err).NotTo(HaveOccurred())
	_, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "raw")
	g.Expect(found).To(BeFalse())
	routers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "bgp", "routers")
	g.Expect(routers).To(HaveLen(2), "the fabric router and the learning router of VRF id 10")
}

// A pass over an unchanged chassis costs the one list and writes nothing. A
// field edited by hand is written back on the next pass, and so is a list that
// lost an entry in the spec, which equality.Semantic.DeepDerivative alone would
// accept as unchanged.
func TestReconcileFabric_AppliesOnlyOnADifference(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	r, calls := newFabricReconciler(t, true, interceptor.Funcs{}, cr)
	nodes := renderedNodes{testNodeA: fabricEntry(true)}
	_, err := passFabric(ctx, t, r, cr, nodes)
	g.Expect(err).NotTo(HaveOccurred())

	calls.lists.Store(0)
	calls.applies.Store(0)
	_, err = r.reconcileFRRConfigurations(ctx, r.Client, cr, nodes)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(calls.lists.Load()).To(BeEquivalentTo(1))
	g.Expect(calls.applies.Load()).To(BeZero(), "an unchanged object is not applied again")

	edited := readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA)
	g.Expect(unstructured.SetNestedField(edited.Object, int64(999), "spec", "raw", "priority")).To(Succeed())
	g.Expect(r.Update(ctx, edited)).To(Succeed())
	_, err = r.reconcileFRRConfigurations(ctx, r.Client, cr, nodes)
	g.Expect(err).NotTo(HaveOccurred())
	priority, _, _ := unstructured.NestedInt64(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA).Object,
		"spec", "raw", "priority")
	g.Expect(priority).To(BeEquivalentTo(frrRawPriority), "a hand-edited field is written back")

	cr.Spec.Fabric.Uplinks = cr.Spec.Fabric.Uplinks[:1]
	calls.applies.Store(0)
	_, err = r.reconcileFRRConfigurations(ctx, r.Client, cr, nodes)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(calls.applies.Load()).To(BeEquivalentTo(1), "a removed uplink must reach the object")
	routers, _, _ := unstructured.NestedSlice(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA).Object,
		"spec", "bgp", "routers")
	neighbors, _, _ := unstructured.NestedSlice(routers[0].(map[string]any), "neighbors")
	g.Expect(neighbors).To(HaveLen(1))
}

// containsDesired ignores what the API server or another writer added to a map,
// and accepts no list of another length.
func TestContainsDesired(t *testing.T) {
	g := NewGomegaWithT(t)
	desired := map[string]any{"a": int64(1), "l": []any{map[string]any{"x": "y"}}}

	g.Expect(containsDesired(desired, map[string]any{
		"a": int64(1), "extra": true, "l": []any{map[string]any{"x": "y", "default": int64(0)}},
	})).To(BeTrue())
	g.Expect(containsDesired(desired, map[string]any{"a": int64(2), "l": []any{map[string]any{"x": "y"}}})).To(BeFalse())
	g.Expect(containsDesired(desired, map[string]any{
		"a": int64(1), "l": []any{map[string]any{"x": "y"}, map[string]any{"x": "z"}},
	})).To(BeFalse(), "a live list with an entry desired no longer names is a difference")
	g.Expect(containsDesired(desired, map[string]any{"l": []any{map[string]any{"x": "y"}}})).To(BeFalse())
	g.Expect(containsDesired(desired, nil)).To(BeFalse())
}

// The objects of nodes that no longer want one are deleted, those of another
// CR are left alone, and once the desired set is empty and nothing of this CR
// is left the finalizer goes.
func TestReconcileFabric_PrunesAndReleasesTheFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	other := &ovnv1alpha1.OVNChassis{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testNamespace}}
	othersObj := foreignFRRConfiguration("other-fabric-0000")
	labels, err := commonmulticluster.OwnerLabels(newTestScheme(t), other)
	g.Expect(err).NotTo(HaveOccurred())
	othersObj.SetLabels(labels)
	r, _ := newFabricReconciler(t, true, interceptor.Funcs{}, cr, othersObj)
	_, err = passFabric(ctx, t, r, cr, renderedNodes{testNodeA: fabricEntry(true)})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA)).NotTo(BeNil())

	// The node loses the frr-k8s label.
	_, err = r.reconcileFRRConfigurations(ctx, r.Client, cr, renderedNodes{testNodeA: fabricEntry(false)})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA)).To(BeNil())
	g.Expect(readFRRConfiguration(ctx, t, r.Client, "other-fabric-0000")).NotTo(BeNil())
	g.Expect(cr.Finalizers).NotTo(ContainElement(fabricFinalizer))
	g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).NotTo(ContainElement(fabricFinalizer))
	g.Expect(ovnChassisCondition(cr, conditionTypeFabricReady).Message).
		To(Equal("Rendered 1 fabric nodes; 0 carry an FRRConfiguration in frr-k8s-system"))
}

// Removing the whole block from a chassis that wrote an FRRConfiguration
// deletes the object and releases the finalizer, and FabricReady reports the
// block as unset.
func TestReconcileFabric_RemovedBlockPrunesAndReleasesTheFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	r, _ := newFabricReconciler(t, true, interceptor.Funcs{}, cr)
	_, err := passFabric(ctx, t, r, cr, renderedNodes{testNodeA: fabricEntry(true)})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA)).NotTo(BeNil())

	cr.Spec.Fabric = nil
	res, err := r.reconcileFabric(ctx, r.Client, cr, maintenanceCentral(),
		renderedNodes{testNodeA: chassisEntry(testFixedSystemID, false)})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA)).To(BeNil())
	g.Expect(cr.Finalizers).NotTo(ContainElement(fabricFinalizer))
	g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).NotTo(ContainElement(fabricFinalizer))
	cond := ovnChassisCondition(cr, conditionTypeFabricReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonFabricNotConfigured))
	g.Expect(ovnChassisCondition(cr, conditionTypeRoutesAdvertised)).To(BeNil())
}

// The finalizer is installed before the first object, after the list, and the
// pass returns to let the next one observe it. The status this pass set so far
// survives the write: an Update decodes the persisted object into what it is
// handed.
func TestReconcileFabric_FinalizerFirstKeepsThePassStatus(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	r, calls := newFabricReconciler(t, true, interceptor.Funcs{}, cr)
	cr.Status.Nodes = []ovnv1alpha1.OVNChassisNodeStatus{{Name: testNodeA, Fabric: true, ConfigHash: "abcdef01"}}

	res, err := r.reconcileFRRConfigurations(ctx, r.Client, cr, renderedNodes{testNodeA: fabricEntry(true)})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueNextPass))
	g.Expect(cr.Finalizers).To(ContainElement(fabricFinalizer))
	g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).To(ContainElement(fabricFinalizer))
	g.Expect(cr.Status.Nodes).To(HaveLen(1), "the in-memory status of the pass must survive the finalizer write")
	g.Expect(cr.Status.Nodes[0].Fabric).To(BeTrue())
	g.Expect(calls.lists.Load()).To(BeEquivalentTo(1), "the list goes ahead of the finalizer")
	g.Expect(calls.applies.Load()).To(BeZero(), "nothing is written before the finalizer is persisted")
}

// A cluster that refuses the list, such as a target whose access chart grants
// no FRRConfiguration access, gets no finalizer: it would guard no object and
// hold the CR's deletion on the same refusal. The deletion then asks nothing.
func TestReconcileFabric_RefusedListAddsNoFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	r, calls := newFabricReconciler(t, true, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewForbidden(frrConfigurationGVK.GroupVersion().WithResource("frrconfigurations").GroupResource(),
				"", errors.New("no grant"))
		},
	}, cr)

	_, err := r.reconcileFRRConfigurations(ctx, r.Client, cr, renderedNodes{testNodeA: fabricEntry(true)})

	g.Expect(apierrors.IsForbidden(err)).To(BeTrue())
	g.Expect(ovnChassisCondition(cr, conditionTypeFabricReady).Reason).To(Equal(conditionReasonFabricError))
	g.Expect(cr.Finalizers).NotTo(ContainElement(fabricFinalizer))
	g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).NotTo(ContainElement(fabricFinalizer))

	calls.lists.Store(0)
	g.Expect(r.reconcileDeleteFabric(ctx, r.Client, cr)).To(Succeed())
	g.Expect(calls.any()).To(BeFalse())
}

// An object of the wanted name that this CR does not own is somebody else's:
// it is left untouched and the condition names it, without failing the pass.
func TestReconcileFabric_ForeignObjectIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	r, calls := newFabricReconciler(t, true, interceptor.Funcs{}, cr,
		foreignFRRConfiguration(testFRRConfigurationNodeA))

	_, err := passFabric(ctx, t, r, cr, renderedNodes{testNodeA: fabricEntry(true)})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(calls.applies.Load()).To(BeZero())
	g.Expect(calls.deletes.Load()).To(BeZero())
	raw, _, _ := unstructured.NestedString(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA).Object,
		"spec", "raw", "rawConfig")
	g.Expect(raw).To(Equal("! hand-written"))
	cond := ovnChassisCondition(cr, conditionTypeFabricReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonFabricError))
	g.Expect(cond.Message).To(Equal("FRRConfiguration " + testFRRConfigurationNodeA +
		" exists and is not owned by this OVNChassis"))
}

// A cluster that does not serve the kind gets no write and a condition that
// names the labelled nodes. A probe that cannot answer is an error, returned.
func TestReconcileFabric_KindNotServedAndProbeError(t *testing.T) {
	t.Run("not served", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		cr := fabricChassis()
		r, calls := newFabricReconciler(t, false, interceptor.Funcs{}, cr)

		res, err := r.reconcileFRRConfigurations(ctx, r.Client, cr, renderedNodes{testNodeA: fabricEntry(true)})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(calls.any()).To(BeFalse())
		g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).To(BeEmpty())
		cond := ovnChassisCondition(cr, conditionTypeFabricReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonFRRK8sNotInstalled))
		g.Expect(cond.Message).To(ContainSubstring(testNodeA))
	})

	t.Run("not served on the target", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		cr := fabricChassis()
		r, _ := newFabricReconciler(t, true, interceptor.Funcs{}, cr)
		children := mctestutil.RemoteChildren(t, r.Client, mctestutil.TargetFake(ovnChassisFakeClientBuilder(t)))

		_, err := r.reconcileFRRConfigurations(ctx, children, cr, renderedNodes{testNodeA: fabricEntry(true)})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ovnChassisCondition(cr, conditionTypeFabricReady).Reason).To(Equal(conditionReasonFRRK8sNotInstalled))
	})

	t.Run("probe error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		cr := fabricChassis()
		r, _ := newFabricReconciler(t, true, interceptor.Funcs{}, cr)
		children := mctestutil.UnprobeableChildren(ovnChassisFakeClientBuilder(t).Build())

		_, err := r.reconcileFRRConfigurations(ctx, children, cr, renderedNodes{testNodeA: fabricEntry(true)})

		g.Expect(err).To(MatchError(ContainSubstring("discovery is unavailable")))
		cond := ovnChassisCondition(cr, conditionTypeFabricReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(commonmulticluster.CapabilityProbeFailed))
	})
}

// A failed list and a failed apply are returned wrapped, under FabricError.
func TestReconcileFabric_ListAndApplyErrors(t *testing.T) {
	boom := errors.New("etcd is unhappy")
	for _, tc := range []struct {
		name     string
		override interceptor.Funcs
		want     string
	}{
		{
			name: "list",
			override: interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			}},
			want: "listing FRRConfigurations in frr-k8s-system: etcd is unhappy",
		},
		{
			name: "apply",
			override: interceptor.Funcs{Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				return boom
			}},
			want: "applying FRRConfiguration " + testFRRConfigurationNodeA + ": ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cr := fabricChassis()
			r, _ := newFabricReconciler(t, true, tc.override, cr)

			_, err := passFabric(ctx, t, r, cr, renderedNodes{testNodeA: fabricEntry(true)})

			g.Expect(err).To(MatchError(ContainSubstring(tc.want)))
			g.Expect(errors.Is(err, boom)).To(BeTrue(), "the cause stays reachable")
			cond := ovnChassisCondition(cr, conditionTypeFabricReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonFabricError))
		})
	}
}

// A selected node without a usable row stays unattached and is named, in name
// order, ahead of the attached ones.
func TestReconcileFabric_NodeWithoutFabricEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	r, _ := newFabricReconciler(t, true, interceptor.Funcs{}, cr)
	leaving := chassisEntry(testFixedSystemID, false)
	leaving.leaving = true
	nodes := renderedNodes{
		testNodeA: fabricEntry(true),
		"node-z":  chassisEntry(testFixedSystemID, false),
		testNodeB: chassisEntry(testFixedSystemID, false),
		"node-l":  leaving,
	}

	_, err := passFabric(ctx, t, r, cr, nodes)

	g.Expect(err).NotTo(HaveOccurred())
	cond := ovnChassisCondition(cr, conditionTypeFabricReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonNodeWithoutFabricEntry))
	g.Expect(cond.Message).To(HavePrefix("Nodes node-b, node-z are selected"),
		"a leaving node is no candidate for the fabric")
	g.Expect(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA)).NotTo(BeNil(),
		"the attached node is configured regardless")
}

// A chassis without spec.fabric and without the finalizer asks nothing about
// the kind, which is every chassis that never had a fabric node on frr-k8s.
func TestReconcileFabric_NilBlockIssuesNoRequest(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := maintenanceChassis()
	r, calls := newFabricReconciler(t, true, interceptor.Funcs{}, cr)

	res, err := r.reconcileFRRConfigurations(ctx, r.Client, cr, renderedNodes{testNodeA: fabricEntry(true)})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(calls.any()).To(BeFalse())
	cond := ovnChassisCondition(cr, conditionTypeFabricReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonFabricNotConfigured))
}

// On a target cluster the object carries the ownership labels and no owner
// reference, and it lands on the target rather than beside the CR.
func TestReconcileFabric_TargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	r, _ := newFabricReconciler(t, false, interceptor.Funcs{}, cr)
	target := fabricClientBuilder(t, true).Build()
	children := mctestutil.RemoteChildren(t, r.Client, target)

	res, err := r.reconcileFRRConfigurations(ctx, children, cr, renderedNodes{testNodeA: fabricEntry(true)})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueNextPass))
	_, err = r.reconcileFRRConfigurations(ctx, children, cr, renderedNodes{testNodeA: fabricEntry(true)})

	g.Expect(err).NotTo(HaveOccurred())
	obj := readFRRConfiguration(ctx, t, target, testFRRConfigurationNodeA)
	g.Expect(obj).NotTo(BeNil())
	g.Expect(obj.GetOwnerReferences()).To(BeEmpty())
	g.Expect(obj.GetLabels()).To(HaveKeyWithValue(commonmulticluster.OwnerNameLabel, testOVNChassisName))
	g.Expect(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA)).To(BeNil())
}

// The deletion path: a CR without the finalizer issues nothing; a CR whose
// target cluster was given up on releases it without a request; otherwise every
// object this CR controls goes before the finalizer, and other objects stay.
func TestReconcileDeleteFabric(t *testing.T) {
	t.Run("no finalizer", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := fabricChassis()
		r, calls := newFabricReconciler(t, true, interceptor.Funcs{}, cr)

		g.Expect(r.reconcileDeleteFabric(context.Background(), r.Client, cr)).To(Succeed())
		g.Expect(calls.any()).To(BeFalse())
	})

	t.Run("abandoned target", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := fabricChassis()
		cr.Finalizers = []string{fabricFinalizer}
		r, calls := newFabricReconciler(t, true, interceptor.Funcs{}, cr)

		g.Expect(r.reconcileDeleteFabric(context.Background(), nil, cr)).To(Succeed())
		g.Expect(calls.any()).To(BeFalse())
		g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).To(BeEmpty())
	})

	t.Run("deletes what it controls", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		cr := fabricChassis()
		r, _ := newFabricReconciler(t, true, interceptor.Funcs{}, cr, foreignFRRConfiguration("someone-elses"))
		_, err := passFabric(ctx, t, r, cr, renderedNodes{testNodeA: fabricEntry(true)})
		g.Expect(err).NotTo(HaveOccurred())

		g.Expect(r.reconcileDeleteFabric(ctx, r.Client, cr)).To(Succeed())

		g.Expect(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA)).To(BeNil())
		g.Expect(readFRRConfiguration(ctx, t, r.Client, "someone-elses")).NotTo(BeNil())
		g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).To(BeEmpty())
	})

	t.Run("list error keeps the finalizer", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := fabricChassis()
		cr.Finalizers = []string{fabricFinalizer}
		r, _ := newFabricReconciler(t, true, interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return apierrors.NewForbidden(frrConfigurationGVK.GroupVersion().WithResource("frrconfigurations").GroupResource(),
					"", errors.New("no grant"))
			},
		}, cr)

		err := r.reconcileDeleteFabric(context.Background(), r.Client, cr)

		g.Expect(apierrors.IsForbidden(err)).To(BeTrue())
		g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).To(ContainElement(fabricFinalizer))
	})

	t.Run("kind no longer served releases the finalizer", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := fabricChassis()
		cr.Finalizers = []string{fabricFinalizer}
		r, calls := newFabricReconciler(t, true, interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return &meta.NoKindMatchError{
					GroupKind: frrConfigurationGVK.GroupKind(), SearchedVersions: []string{frrConfigurationGVK.Version},
				}
			},
		}, cr)

		g.Expect(r.reconcileDeleteFabric(context.Background(), r.Client, cr)).To(Succeed())
		g.Expect(calls.deletes.Load()).To(BeZero())
		g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).To(BeEmpty())
	})

	t.Run("failed delete keeps the finalizer, a gone object releases it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		cr := fabricChassis()
		boom := errors.New("etcd is unhappy")
		var deleteErr error
		r, _ := newFabricReconciler(t, true, interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return deleteErr
			},
		}, cr)
		_, err := passFabric(ctx, t, r, cr, renderedNodes{testNodeA: fabricEntry(true)})
		g.Expect(err).NotTo(HaveOccurred())

		deleteErr = boom
		err = r.reconcileDeleteFabric(ctx, r.Client, cr)

		g.Expect(errors.Is(err, boom)).To(BeTrue())
		g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).To(ContainElement(fabricFinalizer))
		g.Expect(readFRRConfiguration(ctx, t, r.Client, testFRRConfigurationNodeA)).NotTo(BeNil())

		deleteErr = apierrors.NewNotFound(frrConfigurationGVK.GroupVersion().WithResource("frrconfigurations").GroupResource(),
			testFRRConfigurationNodeA)
		g.Expect(r.reconcileDeleteFabric(ctx, r.Client, cr)).To(Succeed())
		g.Expect(getOVNChassis(t, r.Client, testOVNChassisName).Finalizers).To(BeEmpty())
	})
}

// A name that would not fit a label value or the kind's own name rules never
// leaves the builder: the CR name bound leaves room for the suffix.
func TestFabricFRRConfigurationName_FitsTheNameCap(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	cr.Name = strings.Repeat("c", ovnv1alpha1.MaxOVNChassisNameLength)

	g.Expect(len(fabricFRRConfigurationName(cr, strings.Repeat("n", 253)))).To(BeNumerically("<=", 63))
	g.Expect(fabricFRRConfigurationName(testOVNChassis(), testNodeA)).To(Equal(testFRRConfigurationNodeA))
}

// --- route probe ------------------------------------------------------------

// testRouteProbeJob is the name of the fixture's route probe Job.
const testRouteProbeJob = testOVNChassisName + "-route-probe"

// probeClock is a clock a test moves by hand.
type probeClock struct{ t time.Time }

func (c *probeClock) now() time.Time { return c.t }

// probeReconciler is newFabricReconciler for a chassis whose FRR half has
// nothing to do (no node runs frr-k8s), with the clock pinned to clock.
func probeReconciler(t *testing.T, clock *probeClock, override interceptor.Funcs,
	objs ...client.Object,
) *OVNChassisReconciler {
	t.Helper()
	r, _ := newFabricReconciler(t, true, override, objs...)
	r.Now = clock.now
	return r
}

// finishProbe completes the probe Job and leaves the pod a termination message,
// the way the kubelet does for a container that wrote /dev/termination-log.
func finishProbe(ctx context.Context, t *testing.T, c client.Client, message string) {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(simulators.SimulateJobComplete(ctx, c, chassisKey(testRouteProbeJob))).To(Succeed())
	probe := readMaintenanceJob(ctx, t, c, testRouteProbeJob)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: testRouteProbeJob + "-pod", Namespace: testNamespace,
		Labels: map[string]string{
			"batch.kubernetes.io/job-name":       testRouteProbeJob,
			"batch.kubernetes.io/controller-uid": string(probe.UID),
		},
	}}
	g.Expect(c.Create(ctx, pod)).To(Succeed())
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodSucceeded,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  componentMaintenance,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
		}},
	}
	g.Expect(c.Status().Update(ctx, pod)).To(Succeed())
}

// probeFabric runs the whole Fabric step for a fabric chassis whose only node
// does not run frr-k8s, so the FRR half writes nothing and the pass reaches the
// probe.
func probeFabric(ctx context.Context, r *OVNChassisReconciler, cr *ovnv1alpha1.OVNChassis,
	central resolvedCentral,
) (ctrl.Result, error) {
	return r.reconcileFabric(ctx, r.Client, cr, central, renderedNodes{testNodeA: fabricEntry(false)})
}

// The report of a finished probe Job becomes RoutesAdvertised: True with the
// counts when anything is advertised, False with the same message when nothing
// is, and Unknown when the message is not the probe's report. None of them
// touches Ready, and every pass of a fabric chassis requeues after the probe
// interval.
func TestProbeRoutes_Report(t *testing.T) {
	for _, tc := range []struct {
		name, message, status, reason, text string
	}{
		{
			"advertised", "advertised=3 learned=4", "True", conditionReasonRoutesAdvertised,
			"The Southbound database of OVNCentral ovn holds 3 advertised and 4 learned routes",
		},
		{
			"nothing advertised", "advertised=0 learned=0", "False", conditionReasonNoAdvertisedRoutes,
			"The Southbound database of OVNCentral ovn holds 0 advertised and 0 learned routes",
		},
		{"empty message", "", "Unknown", conditionReasonRouteProbeUnreadable, ""},
		{"garbage", "garbage", "Unknown", conditionReasonRouteProbeUnreadable, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cr := fabricChassis()
			r := probeReconciler(t, &probeClock{t: time.Unix(3000, 0)}, interceptor.Funcs{}, cr)
			_, err := probeFabric(ctx, r, cr, maintenanceCentral())
			g.Expect(err).NotTo(HaveOccurred())
			finishProbe(ctx, t, r.Client, tc.message)

			res, err := probeFabric(ctx, r, cr, maintenanceCentral())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(5 * time.Minute))
			cond := ovnChassisCondition(cr, conditionTypeRoutesAdvertised)
			g.Expect(string(cond.Status)).To(Equal(tc.status))
			g.Expect(cond.Reason).To(Equal(tc.reason))
			if tc.text != "" {
				g.Expect(cond.Message).To(Equal(tc.text))
			}

			// RoutesAdvertised is no sub-condition of Ready.
			for _, ct := range chassisSubConditionTypes {
				conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
					Type: ct, Status: metav1.ConditionTrue, Reason: "OK",
				})
			}
			setChassisReadyCondition(cr)
			g.Expect(ovnChassisCondition(cr, "Ready").Status).To(Equal(metav1.ConditionTrue))
		})
	}
}

// A failed probe Job and a report that cannot be read are reported as
// RouteProbeFailed, and neither fails the pass: the next window runs a new Job.
func TestProbeRoutes_Failures(t *testing.T) {
	t.Run("failed Job", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		cr := fabricChassis()
		clock := &probeClock{t: time.Unix(3000, 0)}
		failed := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: testRouteProbeJob, Namespace: testNamespace,
				Annotations: map[string]string{job.PodSpecHashAnnotation: routeProbeRerunKey(clock.t)},
			},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now(),
			}}},
		}
		r := probeReconciler(t, clock, interceptor.Funcs{}, cr, failed)

		res, err := probeFabric(ctx, r, cr, maintenanceCentral())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(RequeueRouteProbe))
		cond := ovnChassisCondition(cr, conditionTypeRoutesAdvertised)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonRouteProbeFailed))
		g.Expect(cond.Message).To(ContainSubstring(job.ErrJobFailed.Error()))
	})

	t.Run("unreadable report", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		cr := fabricChassis()
		// countingFuncs hands non-FRR lists straight through, so the pod list
		// fails on a client of its own.
		c := fabricClientBuilder(t, true, cr).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, pods := list.(*corev1.PodList); pods {
					return errors.New("pods are not listable")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
		r := &OVNChassisReconciler{
			Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10),
			Now: (&probeClock{t: time.Unix(3000, 0)}).now,
		}
		_, err := probeFabric(ctx, r, cr, maintenanceCentral())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(simulators.SimulateJobComplete(ctx, r.Client, chassisKey(testRouteProbeJob))).To(Succeed())

		_, err = probeFabric(ctx, r, cr, maintenanceCentral())

		g.Expect(err).NotTo(HaveOccurred())
		cond := ovnChassisCondition(cr, conditionTypeRoutesAdvertised)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonRouteProbeFailed))
		g.Expect(cond.Message).To(ContainSubstring("pods are not listable"))
	})
}

// A central that has not published its Southbound address defers the probe,
// and no Job is created for it.
func TestProbeRoutes_DeferredWithoutAddress(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	r := probeReconciler(t, &probeClock{t: time.Unix(3000, 0)}, interceptor.Funcs{}, cr)
	central := maintenanceCentral()
	central.sbAddress = ""

	_, err := probeFabric(ctx, r, cr, central)

	g.Expect(err).NotTo(HaveOccurred())
	cond := ovnChassisCondition(cr, conditionTypeRoutesAdvertised)
	g.Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
	g.Expect(cond.Reason).To(Equal(conditionReasonRouteProbeDeferred))
	g.Expect(maintenanceJobNames(ctx, t, r.Client)).To(BeEmpty())
}

// Before the first Job finishes the condition is Pending; while a later Job
// runs it keeps the finding of the one before.
func TestProbeRoutes_PendingKeepsThePreviousFinding(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	clock := &probeClock{t: time.Unix(3000, 0)}
	r := probeReconciler(t, clock, interceptor.Funcs{}, cr)

	_, err := probeFabric(ctx, r, cr, maintenanceCentral())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ovnChassisCondition(cr, conditionTypeRoutesAdvertised).Reason).To(Equal(conditionReasonRouteProbePending))

	finishProbe(ctx, t, r.Client, "advertised=1 learned=2")
	_, err = probeFabric(ctx, r, cr, maintenanceCentral())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ovnChassisCondition(cr, conditionTypeRoutesAdvertised).Reason).To(Equal(conditionReasonRoutesAdvertised))

	// The next window re-creates the Job, which is still running.
	clock.t = clock.t.Add(RequeueRouteProbe)
	_, err = probeFabric(ctx, r, cr, maintenanceCentral())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ovnChassisCondition(cr, conditionTypeRoutesAdvertised).Reason).To(Equal(conditionReasonRoutesAdvertised))
}

// The rerun key is the probe window: two passes inside one window read one
// Job, and the first pass of the next window runs a new one.
func TestProbeRoutes_OneJobPerWindow(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	clock := &probeClock{t: time.Unix(3000, 0)}
	r := probeReconciler(t, clock, interceptor.Funcs{}, cr)

	_, err := probeFabric(ctx, r, cr, maintenanceCentral())
	g.Expect(err).NotTo(HaveOccurred())
	first := readMaintenanceJob(ctx, t, r.Client, testRouteProbeJob)
	g.Expect(first.Annotations).To(HaveKeyWithValue(job.PodSpecHashAnnotation, "10"))
	g.Expect(simulators.SimulateJobComplete(ctx, r.Client, chassisKey(testRouteProbeJob))).To(Succeed())

	clock.t = time.Unix(3299, 0)
	_, err = probeFabric(ctx, r, cr, maintenanceCentral())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(readMaintenanceJob(ctx, t, r.Client, testRouteProbeJob).Annotations).
		To(HaveKeyWithValue(job.PodSpecHashAnnotation, "10"), "the same window reads the same Job")

	clock.t = time.Unix(3300, 0)
	_, err = probeFabric(ctx, r, cr, maintenanceCentral())
	g.Expect(err).NotTo(HaveOccurred())
	_, err = probeFabric(ctx, r, cr, maintenanceCentral())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(readMaintenanceJob(ctx, t, r.Client, testRouteProbeJob).Annotations).
		To(HaveKeyWithValue(job.PodSpecHashAnnotation, "11"), "the next window runs a new Job")
}

// Removing spec.fabric removes RoutesAdvertised and runs no probe.
func TestProbeRoutes_RemovedBlockRemovesTheCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := fabricChassis()
	r := probeReconciler(t, &probeClock{t: time.Unix(3000, 0)}, interceptor.Funcs{}, cr)
	_, err := probeFabric(ctx, r, cr, maintenanceCentral())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ovnChassisCondition(cr, conditionTypeRoutesAdvertised)).NotTo(BeNil())
	g.Expect(r.Delete(ctx, readMaintenanceJob(ctx, t, r.Client, testRouteProbeJob))).To(Succeed())

	cr.Spec.Fabric = nil
	res, err := probeFabric(ctx, r, cr, maintenanceCentral())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(ovnChassisCondition(cr, conditionTypeRoutesAdvertised)).To(BeNil())
	g.Expect(maintenanceJobNames(ctx, t, r.Client)).To(BeEmpty())
}
