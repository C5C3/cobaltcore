// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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

// hypervisor is the Hypervisor of node, carrying spec and status as
// openstack-hypervisor-operator writes them. The kind is addressed
// unstructured, so no scheme registration is involved, which is how the step
// reads it.
func hypervisor(node string, spec, status map[string]any) *unstructured.Unstructured {
	hv := &unstructured.Unstructured{Object: map[string]any{}}
	hv.SetGroupVersionKind(hypervisorGVK)
	hv.SetName(node)
	if spec != nil {
		hv.Object["spec"] = spec
	}
	if status != nil {
		hv.Object["status"] = status
	}
	return hv
}

// hvSpec is a Hypervisor spec with the lifecycle enabled and the given list.
func hvSpec(customTraits ...string) map[string]any {
	return map[string]any{"lifecycleEnabled": true, "customTraits": anyList(customTraits)}
}

// hvStatus is a Hypervisor status listing traits, with a TraitsUpdated
// condition of the given status unless it is empty.
func hvStatus(updated metav1.ConditionStatus, traits ...string) map[string]any {
	status := map[string]any{"traits": anyList(traits)}
	if updated != "" {
		status["conditions"] = []any{map[string]any{
			"type": hypervisorConditionTraitsUpdated, "status": string(updated), "reason": "Succeeded",
			"message": "Traits successfully updated",
		}}
	}
	return status
}

// anyList is traits as the []any an unstructured object holds.
func anyList(traits []string) []any {
	out := make([]any, 0, len(traits))
	for _, trait := range traits {
		out = append(out, trait)
	}
	return out
}

// tracedNode is an Active entry in testZone recording traits.
func tracedNode(name string, traits ...string) novav1alpha1.NovaComputeNodeStatus {
	node := activeIn(name, testZone)
	node.Traits = traits
	return node
}

// traitsPass is a pass whose NovaRef and Nodes steps ran against api.
func traitsPass(api *computeapitest.Fake, siblings ...novav1alpha1.NovaCompute) *novaComputePass {
	pass := apiPass(api, siblings...)
	pass.nova = readyNovaForCompute()
	pass.customTraitsAnnotated = map[string]bool{}
	return pass
}

// traitsRun is one run of the Traits step against a fake client.
type traitsRun struct {
	result   ctrl.Result
	err      error
	pass     *novaComputePass
	client   client.Client
	recorder *record.FakeRecorder
	// gets are the Hypervisors read, patches the merge patch bodies sent, by
	// Hypervisor name.
	gets    []string
	patches map[string]string
}

// runTraits runs the Traits step for cr with pass, on a fake client holding
// objs whose Hypervisor calls go through funcs first.
func runTraits(t *testing.T, cr *novav1alpha1.NovaCompute, pass *novaComputePass, funcs interceptor.Funcs,
	objs ...client.Object,
) *traitsRun {
	t.Helper()
	run := &traitsRun{pass: pass, patches: map[string]string{}}
	c := novaFakeClientBuilder(append(objs, cr)...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			if _, ok := obj.(*unstructured.Unstructured); ok {
				run.gets = append(run.gets, key.Name)
				if funcs.Get != nil {
					return funcs.Get(ctx, cl, key, obj, opts...)
				}
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption,
		) error {
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			run.patches[obj.GetName()] = string(data)
			if funcs.Patch != nil {
				return funcs.Patch(ctx, cl, obj, patch, opts...)
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
		List: funcs.List,
	}).Build()
	run.recorder = record.NewFakeRecorder(100)
	r := &NovaComputeReconciler{Client: c, Scheme: c.Scheme(), Recorder: run.recorder, HTTPClient: pass.doer}
	run.client = c
	run.result, run.err = r.reconcileNovaComputeTraits(context.Background(), c, cr, pass)
	return run
}

// customTraitsOf reads back the spec.customTraits of node's Hypervisor.
func (run *traitsRun) customTraitsOf(t *testing.T, node string) []string {
	t.Helper()
	g := NewGomegaWithT(t)
	hv := &unstructured.Unstructured{}
	hv.SetGroupVersionKind(hypervisorGVK)
	g.Expect(run.client.Get(context.Background(), types.NamespacedName{Name: node}, hv)).To(Succeed())
	traits, _, err := unstructured.NestedStringSlice(hv.Object, "spec", "customTraits")
	g.Expect(err).NotTo(HaveOccurred())
	return traits
}

// traitsReady is cr's TraitsReady condition.
func traitsReady(t *testing.T, cr *novav1alpha1.NovaCompute) *metav1.Condition {
	t.Helper()
	cond := novaComputeCondition(cr, conditionTypeTraitsReady)
	NewGomegaWithT(t).Expect(cond).NotTo(BeNil())
	return cond
}

// recordOf is the traits status.nodes records for node.
func recordOf(cr *novav1alpha1.NovaCompute, node string) []string {
	for _, entry := range cr.Status.Nodes {
		if entry.Name == node {
			return entry.Traits
		}
	}
	return nil
}

func TestReconcileNovaComputeTraits_NoTraitsDeclared(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := activePool(activeIn(testNodeName, testZone))

	run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return &meta.NoKindMatchError{GroupKind: hypervisorGVK.GroupKind()}
		},
	})

	g.Expect(run.err).NotTo(HaveOccurred())
	g.Expect(run.result.IsZero()).To(BeTrue())
	g.Expect(run.pass.traitsSettled).To(BeTrue())
	g.Expect(run.gets).To(BeEmpty(), "a cluster without the Hypervisor kind is never asked")
	g.Expect(api.Calls()).To(BeEmpty())
	cond := traitsReady(t, cr)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNoTraitsDeclared))
}

func TestReconcileNovaComputeTraits_CreatesTheTraitsAndWritesTheList(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := activePool(activeIn(testNodeName, testZone))
	cr.Spec.Traits = []string{"CUSTOM_B", "CUSTOM_A"}

	run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{}, hypervisor(testNodeName, hvSpec(), map[string]any{}))

	g.Expect(run.err).NotTo(HaveOccurred())
	g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
	puts := api.CallsTo(http.MethodPut, "/traits/")
	g.Expect(puts).To(HaveLen(2))
	g.Expect(puts[0].Path).To(Equal("/traits/CUSTOM_A"))
	g.Expect(puts[1].Path).To(Equal("/traits/CUSTOM_B"))
	g.Expect(cr.Status.CreatedTraits).To(Equal([]string{"CUSTOM_A", "CUSTOM_B"}))
	g.Expect(collectEvents(run.recorder)).To(ConsistOf(
		"Normal TraitCreated Created Placement trait CUSTOM_A",
		"Normal TraitCreated Created Placement trait CUSTOM_B",
	))
	g.Expect(run.customTraitsOf(t, testNodeName)).To(Equal([]string{"CUSTOM_A", "CUSTOM_B"}), "sorted in byte order")
	g.Expect(recordOf(cr, testNodeName)).To(Equal([]string{"CUSTOM_A", "CUSTOM_B"}),
		"a list written but not applied yet is recorded")
	g.Expect(run.pass.traitsSettled).To(BeFalse())
	cond := traitsReady(t, cr)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForHypervisorOperator))
	g.Expect(cond.Message).To(HaveSuffix(" on " + testNodeName))
}

func TestReconcileNovaComputeTraits_AppliedListMakesNoCall(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := activePool(activeIn(testNodeName, testZone))
	cr.Spec.Traits = []string{"CUSTOM_B", "CUSTOM_A"}
	hv := hypervisor(testNodeName, hvSpec("CUSTOM_A", "CUSTOM_B"),
		hvStatus(metav1.ConditionTrue, "CUSTOM_A", "CUSTOM_B", "HW_CPU_X86_VMX"))

	run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{}, hv)

	g.Expect(run.err).NotTo(HaveOccurred())
	g.Expect(run.result.IsZero()).To(BeTrue())
	g.Expect(api.Calls()).To(BeEmpty(), "no token and no PUT")
	g.Expect(run.patches).To(BeEmpty())
	g.Expect(recordOf(cr, testNodeName)).To(Equal([]string{"CUSTOM_A", "CUSTOM_B"}), "the standard trait is left out")
	cond := traitsReady(t, cr)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonTraitsApplied))
	g.Expect(cond.Message).To(Equal("Every managed node carries its custom traits (1 nodes)"))
}

func TestReconcileNovaComputeTraits_HostTraits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry novav1alpha1.NovaComputeHostTraits
		want  map[string][]string
	}{
		{
			name:  "Append adds to the pool's list",
			entry: novav1alpha1.NovaComputeHostTraits{NodeName: "n1", Traits: []string{"CUSTOM_C"}, Mode: novav1alpha1.NovaComputeHostTraitsAppend},
			want:  map[string][]string{"n1": {"CUSTOM_A", "CUSTOM_B", "CUSTOM_C"}, "n2": {"CUSTOM_A", "CUSTOM_B"}},
		},
		{
			name:  "an entry without a mode appends",
			entry: novav1alpha1.NovaComputeHostTraits{NodeName: "n1", Traits: []string{"CUSTOM_A"}},
			want:  map[string][]string{"n1": {"CUSTOM_A", "CUSTOM_B"}, "n2": {"CUSTOM_A", "CUSTOM_B"}},
		},
		{
			name:  "Replace puts its list in place of the pool's",
			entry: novav1alpha1.NovaComputeHostTraits{NodeName: "n1", Traits: []string{"CUSTOM_C"}, Mode: novav1alpha1.NovaComputeHostTraitsReplace},
			want:  map[string][]string{"n1": {"CUSTOM_C"}, "n2": {"CUSTOM_A", "CUSTOM_B"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := activePool(activeIn("n1", testZone), activeIn("n2", testZone))
			cr.Spec.Traits = []string{"CUSTOM_A", "CUSTOM_B"}
			cr.Spec.HostTraits = []novav1alpha1.NovaComputeHostTraits{tc.entry}

			run := runTraits(t, cr, traitsPass(computeapitest.New()), interceptor.Funcs{},
				hypervisor("n1", hvSpec(), nil), hypervisor("n2", hvSpec(), nil))

			g.Expect(run.err).NotTo(HaveOccurred())
			for node, want := range tc.want {
				g.Expect(run.customTraitsOf(t, node)).To(Equal(want), node)
			}
		})
	}

	t.Run("Replace with an empty list clears the node", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := activePool(tracedNode("n1", "CUSTOM_A"))
		cr.Spec.HostTraits = []novav1alpha1.NovaComputeHostTraits{
			{NodeName: "n1", Mode: novav1alpha1.NovaComputeHostTraitsReplace},
		}

		run := runTraits(t, cr, traitsPass(computeapitest.New()), interceptor.Funcs{},
			hypervisor("n1", hvSpec("CUSTOM_A"), hvStatus(metav1.ConditionTrue, "CUSTOM_A")))

		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(run.patches["n1"]).To(ContainSubstring(`"customTraits":[]`))
		g.Expect(run.customTraitsOf(t, "n1")).To(BeEmpty())
		g.Expect(traitsReady(t, cr).Reason).To(Equal(conditionReasonWaitingForHypervisorOperator),
			"the provider still carries CUSTOM_A")
	})
}

func TestReconcileNovaComputeTraits_AnExistingTraitIsNotCreated(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddTrait("CUSTOM_A")
	cr := activePool(activeIn(testNodeName, testZone))
	cr.Spec.Traits = []string{"CUSTOM_A"}

	run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{}, hypervisor(testNodeName, hvSpec(), nil))

	g.Expect(run.err).NotTo(HaveOccurred())
	g.Expect(api.CallsTo(http.MethodPut, "/traits/CUSTOM_A")).To(HaveLen(1))
	g.Expect(cr.Status.CreatedTraits).To(BeEmpty())
	g.Expect(collectEvents(run.recorder)).To(BeEmpty())
	g.Expect(run.customTraitsOf(t, testNodeName)).To(Equal([]string{"CUSTOM_A"}))
}

func TestReconcileNovaComputeTraits_HypervisorOperatorNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := activePool(activeIn(testNodeName, testZone))
	cr.Spec.Traits = []string{"CUSTOM_A"}

	run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return &meta.NoKindMatchError{GroupKind: hypervisorGVK.GroupKind()}
		},
	})

	g.Expect(run.err).NotTo(HaveOccurred())
	g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
	g.Expect(api.Calls()).To(BeEmpty())
	cond := traitsReady(t, cr)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHypervisorOperatorNotInstalled))
	g.Expect(cond.Message).To(ContainSubstring("hypervisors.kvm.cloud.sap"))
}

func TestReconcileNovaComputeTraits_HypervisorAccessFailures(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "kvm.cloud.sap", Resource: "hypervisors"}, "n1",
		errors.New("no grant"))
	injected := errors.New("injected failure")
	failGet := func(err error) interceptor.Funcs {
		return interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object,
			...client.GetOption,
		) error {
			return err
		}}
	}
	failPatch := func(err error) interceptor.Funcs {
		return interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch,
			...client.PatchOption,
		) error {
			return err
		}}
	}

	for _, tc := range []struct {
		name       string
		funcs      interceptor.Funcs
		wantReason string
		wantErr    bool
	}{
		{name: "a forbidden read waits", funcs: failGet(forbidden), wantReason: conditionReasonHypervisorsForbidden},
		{name: "a forbidden patch waits", funcs: failPatch(forbidden), wantReason: conditionReasonHypervisorsForbidden},
		{name: "another read error fails", funcs: failGet(injected), wantReason: conditionReasonHypervisorError, wantErr: true},
		{name: "another patch error fails", funcs: failPatch(injected), wantReason: conditionReasonHypervisorError, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := activePool(activeIn("n1", testZone))
			cr.Spec.Traits = []string{"CUSTOM_A"}

			run := runTraits(t, cr, traitsPass(computeapitest.New()), tc.funcs, hypervisor("n1", hvSpec(), nil))

			cond := traitsReady(t, cr)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
			if tc.wantErr {
				g.Expect(run.err).To(MatchError(injected))
				return
			}
			g.Expect(run.err).NotTo(HaveOccurred())
			g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
			g.Expect(cond.Message).To(ContainSubstring("hypervisorTraits"))
		})
	}

	t.Run("a node without a Hypervisor does not stop the others", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := activePool(activeIn("n1", testZone), activeIn("n2", testZone))
		cr.Spec.Traits = []string{"CUSTOM_A"}

		run := runTraits(t, cr, traitsPass(computeapitest.New()), interceptor.Funcs{}, hypervisor("n2", hvSpec(), nil))

		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		g.Expect(run.customTraitsOf(t, "n2")).To(Equal([]string{"CUSTOM_A"}))
		cond := traitsReady(t, cr)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonNodesWithoutHypervisor))
		g.Expect(cond.Message).To(Equal("Nodes without a Hypervisor: n1"))
	})

	t.Run("a Hypervisor gone before its patch is a node without one", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := activePool(activeIn("n1", testZone))
		cr.Spec.Traits = []string{"CUSTOM_A"}
		gone := apierrors.NewNotFound(schema.GroupResource{Group: "kvm.cloud.sap", Resource: "hypervisors"}, "n1")

		runTraits(t, cr, traitsPass(computeapitest.New()), failPatch(gone), hypervisor("n1", hvSpec(), nil))

		g.Expect(traitsReady(t, cr).Reason).To(Equal(conditionReasonNodesWithoutHypervisor))
		g.Expect(recordOf(cr, "n1")).To(BeEmpty())
	})
}

// TestReconcileNovaComputeTraits_PlacedPool pins that a placed pool reads its
// Hypervisors on its target cluster, and waits while that cluster cannot be
// resolved.
func TestReconcileNovaComputeTraits_PlacedPool(t *testing.T) {
	ctx := context.Background()
	placed := func() *novav1alpha1.NovaCompute {
		cr := activePool(activeIn("n1", testZone))
		cr.Spec.Traits = []string{"CUSTOM_A"}
		cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "compute-a"}
		return cr
	}

	t.Run("an unresolvable target cluster waits", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		cr := placed()
		r := newNovaComputeTestReconciler(api, cr)
		r.Resolver = unresolvableResolver{}

		result, err := r.reconcileNovaComputeTraits(ctx, r.Client, cr, traitsPass(api))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		cond := traitsReady(t, cr)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
		g.Expect(api.Calls()).To(BeEmpty())
	})

	t.Run("a placed pool reads its Hypervisors on the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		cr := placed()
		r := newNovaComputeTestReconciler(api, cr)
		r.APIReader = r.Client
		target := novaFakeClientBuilder(hypervisor("n1", hvSpec(), nil)).Build()
		r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{Client: target})

		_, err := r.reconcileNovaComputeTraits(ctx, target, cr, traitsPass(api))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(traitsReady(t, cr).Reason).To(Equal(conditionReasonWaitingForHypervisorOperator),
			"the management cluster holds no Hypervisor, the target does")
		hv := &unstructured.Unstructured{}
		hv.SetGroupVersionKind(hypervisorGVK)
		g.Expect(target.Get(ctx, types.NamespacedName{Name: "n1"}, hv)).To(Succeed())
		traits, _, err := unstructured.NestedStringSlice(hv.Object, "spec", "customTraits")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(traits).To(Equal([]string{"CUSTOM_A"}))
	})
}

func TestReconcileNovaComputeTraits_AnnotatedNodesAreLeftAlone(t *testing.T) {
	t.Run("an annotated node is neither read nor written", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := activePool(activeIn("n1", testZone), activeIn("n2", testZone))
		cr.Spec.Traits = []string{"CUSTOM_A"}
		pass := traitsPass(computeapitest.New())
		pass.customTraitsAnnotated["n1"] = true

		run := runTraits(t, cr, pass, interceptor.Funcs{},
			hypervisor("n1", hvSpec(), nil), hypervisor("n2", hvSpec(), nil))

		g.Expect(run.gets).To(Equal([]string{"n2"}))
		g.Expect(run.patches).To(HaveKey("n2"))
		g.Expect(run.patches).NotTo(HaveKey("n1"))
		g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		cond := traitsReady(t, cr)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonNodesWithCustomTraitsAnnotation))
		g.Expect(cond.Message).To(HaveSuffix(": n1"))
	})

	t.Run("the message names twenty nodes", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := validNovaCompute()
		cr.Spec.Traits = []string{"CUSTOM_A"}
		pass := traitsPass(computeapitest.New())
		for i := range 25 {
			name := fmt.Sprintf("n%02d", i)
			cr.Status.Nodes = append(cr.Status.Nodes, activeIn(name, testZone))
			pass.customTraitsAnnotated[name] = true
		}

		run := runTraits(t, cr, pass, interceptor.Funcs{})

		g.Expect(run.gets).To(BeEmpty())
		g.Expect(traitsReady(t, cr).Message).To(HaveSuffix("n19 and 5 more"))
	})
}

func TestReconcileNovaComputeTraits_HypervisorOperatorState(t *testing.T) {
	t.Run("a disabled lifecycle applies no trait", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := activePool(activeIn("n1", testZone))
		cr.Spec.Traits = []string{"CUSTOM_A"}
		spec := hvSpec("CUSTOM_A")
		spec["lifecycleEnabled"] = false

		run := runTraits(t, cr, traitsPass(computeapitest.New()), interceptor.Funcs{}, hypervisor("n1", spec, nil))

		g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		cond := traitsReady(t, cr)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonHypervisorLifecycleDisabled))
		g.Expect(cond.Message).To(HaveSuffix(": n1"))
	})

	t.Run("an absent lifecycleEnabled reads as enabled", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := activePool(activeIn("n1", testZone))
		cr.Spec.Traits = []string{"CUSTOM_A"}

		runTraits(t, cr, traitsPass(computeapitest.New()), interceptor.Funcs{},
			hypervisor("n1", map[string]any{"customTraits": []any{}}, nil))

		g.Expect(traitsReady(t, cr).Reason).To(Equal(conditionReasonWaitingForHypervisorOperator))
	})

	t.Run("a failed update carries the hypervisor operator's message", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := activePool(activeIn("n1", testZone))
		cr.Spec.Traits = []string{"CUSTOM_A"}
		status := map[string]any{"conditions": []any{map[string]any{
			"type": hypervisorConditionTraitsUpdated, "status": "False", "reason": "Failed",
			"message": "Failed to update traits in placement: Bad Request",
		}}}

		run := runTraits(t, cr, traitsPass(computeapitest.New()), interceptor.Funcs{},
			hypervisor("n1", hvSpec("CUSTOM_A"), status))

		g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		cond := traitsReady(t, cr)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonHypervisorTraitsFailed))
		g.Expect(cond.Message).To(Equal(
			"openstack-hypervisor-operator could not apply the traits on n1: Failed to update traits in placement: Bad Request"))
	})
}

func TestReconcileNovaComputeTraits_PlacementFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*computeapitest.Fake)
		wantReason string
		wantIn     string
	}{
		{
			name:       "a rejected token",
			setup:      func(api *computeapitest.Fake) { api.RejectAuth = true },
			wantReason: conditionReasonComputeAPIError,
			wantIn:     "authenticating",
		},
		{
			name:       "a catalog without placement",
			setup:      func(api *computeapitest.Fake) { api.OmitPlacementFromCatalog = true },
			wantReason: conditionReasonPlacementEndpointNotFound,
			wantIn:     "RegionOne",
		},
		{
			name: "a failed create",
			setup: func(api *computeapitest.Fake) {
				api.FailNext(http.MethodPut, "/traits/CUSTOM_A", http.StatusInternalServerError)
			},
			wantReason: conditionReasonPlacementAPIError,
			wantIn:     "creating trait CUSTOM_A",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			api := computeapitest.New()
			tc.setup(api)
			cr := activePool(activeIn("n1", testZone))
			cr.Spec.Traits = []string{"CUSTOM_A", "CUSTOM_B"}

			run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{}, hypervisor("n1", hvSpec(), nil))

			g.Expect(run.err).NotTo(HaveOccurred())
			g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
			g.Expect(run.patches).To(BeEmpty(), "no Hypervisor is written in a pass whose create failed")
			cond := traitsReady(t, cr)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantIn))
		})
	}

	t.Run("an override needs no catalog entry", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.OmitPlacementFromCatalog = true
		cr := activePool(activeIn("n1", testZone))
		cr.Spec.Traits = []string{"CUSTOM_A"}
		pass := traitsPass(api)
		pass.nova.Spec.Endpoints.Placement.Override = "http://placement-override.openstack.svc:8778"

		runTraits(t, cr, pass, interceptor.Funcs{}, hypervisor("n1", hvSpec(), nil))

		g.Expect(api.CallsTo(http.MethodPut, "/traits/CUSTOM_A")).To(HaveLen(1))
		g.Expect(traitsReady(t, cr).Reason).To(Equal(conditionReasonWaitingForHypervisorOperator))
	})
}

func TestReconcileNovaComputeTraits_DeletesWhatItCreated(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	api.AddTrait("CUSTOM_A")
	cr := activePool(tracedNode("n1", "CUSTOM_A"))
	cr.Status.CreatedTraits = []string{"CUSTOM_A"}

	run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{},
		hypervisor("n1", hvSpec("CUSTOM_A"), hvStatus(metav1.ConditionTrue, "CUSTOM_A")))

	g.Expect(run.err).NotTo(HaveOccurred())
	g.Expect(api.CallsTo(http.MethodDelete, "/traits/")).To(BeEmpty(), "the provider still carries it")
	g.Expect(cr.Status.CreatedTraits).To(Equal([]string{"CUSTOM_A"}))
	g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
	g.Expect(run.customTraitsOf(t, "n1")).To(BeEmpty())
	g.Expect(recordOf(cr, "n1")).To(Equal([]string{"CUSTOM_A"}))
	g.Expect(run.pass.traitsSettled).To(BeFalse())

	// The hypervisor operator removed it from the provider.
	run = runTraits(t, cr, traitsPass(api), interceptor.Funcs{},
		hypervisor("n1", hvSpec(), hvStatus(metav1.ConditionTrue)))

	g.Expect(run.err).NotTo(HaveOccurred())
	g.Expect(api.CallsTo(http.MethodDelete, "/traits/CUSTOM_A")).To(HaveLen(1))
	g.Expect(api.Traits()).To(BeEmpty())
	g.Expect(cr.Status.CreatedTraits).To(BeEmpty())
	g.Expect(recordOf(cr, "n1")).To(BeEmpty())
	g.Expect(run.result.IsZero()).To(BeTrue())
	g.Expect(run.pass.traitsSettled).To(BeTrue())
	g.Expect(collectEvents(run.recorder)).To(ConsistOf(
		"Normal TraitDeleted Deleted Placement trait CUSTOM_A: no NovaCompute of Nova " + testNovaName +
			" declares or carries it"))
	g.Expect(traitsReady(t, cr).Reason).To(Equal(conditionReasonNoTraitsDeclared))
}

func TestReconcileNovaComputeTraits_Deletion(t *testing.T) {
	// A pool that dropped CUSTOM_A, which it created, and records it nowhere.
	dropped := func() *novav1alpha1.NovaCompute {
		cr := activePool(activeIn("n1", testZone))
		cr.Status.CreatedTraits = []string{"CUSTOM_A"}
		return cr
	}

	t.Run("a trait in use is kept and dropped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.SetTraitInUse("CUSTOM_A", true)
		cr := dropped()

		run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{})

		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(cr.Status.CreatedTraits).To(BeEmpty())
		g.Expect(api.Traits()).To(Equal([]string{"CUSTOM_A"}))
		g.Expect(collectEvents(run.recorder)).To(ConsistOf(
			"Normal TraitKept Kept Placement trait CUSTOM_A: a resource provider no pool of Nova " + testNovaName +
				" manages carries it"))
		g.Expect(run.pass.traitsSettled).To(BeTrue())
	})

	t.Run("a failed delete keeps the trait", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddTrait("CUSTOM_A")
		api.FailNext(http.MethodDelete, "/traits/CUSTOM_A", http.StatusInternalServerError)
		cr := dropped()

		run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{})

		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		g.Expect(cr.Status.CreatedTraits).To(Equal([]string{"CUSTOM_A"}))
		cond := traitsReady(t, cr)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonPlacementAPIError))
		g.Expect(cond.Message).To(ContainSubstring("deleting trait CUSTOM_A"))
		g.Expect(run.pass.traitsSettled).To(BeFalse())
	})

	t.Run("a failed delete keeps it and the rest, drops what went", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		for _, trait := range []string{"CUSTOM_A", "CUSTOM_B", "CUSTOM_C"} {
			api.AddTrait(trait)
		}
		api.FailNext(http.MethodDelete, "/traits/CUSTOM_B", http.StatusInternalServerError)
		cr := activePool(activeIn("n1", testZone))
		cr.Status.CreatedTraits = []string{"CUSTOM_A", "CUSTOM_B", "CUSTOM_C"}

		run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{})

		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(api.Traits()).To(Equal([]string{"CUSTOM_B", "CUSTOM_C"}))
		g.Expect(cr.Status.CreatedTraits).To(Equal([]string{"CUSTOM_B", "CUSTOM_C"}))
		g.Expect(traitsReady(t, cr).Reason).To(Equal(conditionReasonPlacementAPIError))
	})

	t.Run("a deleting pool keeps a sibling's trait dropped past a failed delete", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		for _, trait := range []string{"CUSTOM_A", "CUSTOM_B", "CUSTOM_C"} {
			api.AddTrait(trait)
		}
		api.FailNext(http.MethodDelete, "/traits/CUSTOM_B", http.StatusInternalServerError)
		sibling := rivalPool("pool-b", time.Hour, testPoolLabel, "b")
		sibling.Spec.Traits = []string{"CUSTOM_A"}
		cr := deletingPool(novaComputeDrainFinalizer)
		cr.Status.CreatedTraits = []string{"CUSTOM_A", "CUSTOM_B", "CUSTOM_C"}

		run := runTraits(t, cr, traitsPass(api, *sibling), interceptor.Funcs{})

		g.Expect(run.err).NotTo(HaveOccurred())
		g.Expect(api.Traits()).To(Equal([]string{"CUSTOM_A", "CUSTOM_B", "CUSTOM_C"}))
		g.Expect(cr.Status.CreatedTraits).To(Equal([]string{"CUSTOM_B", "CUSTOM_C"}))
		g.Expect(run.pass.traitsSettled).To(BeFalse())
	})

	for _, tc := range []struct {
		name       string
		setup      func(*computeapitest.Fake)
		wantReason string
	}{
		{
			name:       "a rejected token keeps the trait of a deleting pool",
			setup:      func(api *computeapitest.Fake) { api.RejectAuth = true },
			wantReason: conditionReasonComputeAPIError,
		},
		{
			name:       "a catalog without placement keeps the trait of a deleting pool",
			setup:      func(api *computeapitest.Fake) { api.OmitPlacementFromCatalog = true },
			wantReason: conditionReasonPlacementEndpointNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			api := computeapitest.New()
			api.AddTrait("CUSTOM_A")
			tc.setup(api)
			cr := deletingPool(novaComputeDrainFinalizer)
			cr.Status.CreatedTraits = []string{"CUSTOM_A"}

			run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{})

			g.Expect(run.err).NotTo(HaveOccurred())
			g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
			g.Expect(api.Traits()).To(Equal([]string{"CUSTOM_A"}))
			g.Expect(cr.Status.CreatedTraits).To(Equal([]string{"CUSTOM_A"}))
			cond := traitsReady(t, cr)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
			g.Expect(run.pass.traitsSettled).To(BeFalse())
		})
	}

	t.Run("a non-deleting sibling declaring it keeps it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddTrait("CUSTOM_A")
		sibling := rivalPool("pool-b", time.Hour, testPoolLabel, "b")
		sibling.Spec.HostTraits = []novav1alpha1.NovaComputeHostTraits{{NodeName: "n9", Traits: []string{"CUSTOM_A"}}}
		cr := dropped()

		run := runTraits(t, cr, traitsPass(api, *sibling), interceptor.Funcs{})

		g.Expect(api.CallsTo(http.MethodDelete, "/traits/")).To(BeEmpty())
		g.Expect(cr.Status.CreatedTraits).To(Equal([]string{"CUSTOM_A"}))
		g.Expect(run.result.IsZero()).To(BeTrue(), "nothing waits on the hypervisor operator")
		g.Expect(run.pass.traitsSettled).To(BeFalse())
	})

	t.Run("a deleting sibling declaring it does not keep it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddTrait("CUSTOM_A")
		sibling := rivalPool("pool-b", time.Hour, testPoolLabel, "b")
		sibling.Spec.Traits = []string{"CUSTOM_A"}
		sibling.DeletionTimestamp = ptr.To(metav1.Now())
		cr := dropped()

		runTraits(t, cr, traitsPass(api, *sibling), interceptor.Funcs{})

		g.Expect(api.Traits()).To(BeEmpty())
	})

	t.Run("a sibling recording it on a node keeps it waiting", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddTrait("CUSTOM_A")
		sibling := rivalPool("pool-b", time.Hour, testPoolLabel, "b")
		sibling.DeletionTimestamp = ptr.To(metav1.Now())
		sibling.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{
			{Name: "n9", Phase: novav1alpha1.NovaComputeNodeDraining, Traits: []string{"CUSTOM_A"}},
		}
		cr := dropped()

		run := runTraits(t, cr, traitsPass(api, *sibling), interceptor.Funcs{})

		g.Expect(api.CallsTo(http.MethodDelete, "/traits/")).To(BeEmpty())
		g.Expect(cr.Status.CreatedTraits).To(Equal([]string{"CUSTOM_A"}))
		g.Expect(run.result.RequeueAfter).To(Equal(RequeueComputeDrainPolling))
		cond := traitsReady(t, cr)
		g.Expect(cond.Reason).To(Equal(conditionReasonTraitsApplied))
		g.Expect(cond.Message).To(ContainSubstring("waiting for their last carrier before deletion: CUSTOM_A"))
	})

	t.Run("this pool declaring it keeps it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := computeapitest.New()
		api.AddTrait("CUSTOM_A")
		cr := dropped()
		cr.Spec.Traits = []string{"CUSTOM_A"}

		runTraits(t, cr, traitsPass(api), interceptor.Funcs{}, hypervisor("n1", hvSpec(), nil))

		g.Expect(api.CallsTo(http.MethodDelete, "/traits/")).To(BeEmpty())
		g.Expect(cr.Status.CreatedTraits).To(Equal([]string{"CUSTOM_A"}))
	})

	t.Run("a failed sibling list fails the step", func(t *testing.T) {
		g := NewGomegaWithT(t)
		injected := errors.New("injected list failure")
		cr := dropped()
		pass := traitsPass(computeapitest.New())
		pass.siblings = nil

		run := runTraits(t, cr, pass, interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return injected
			},
		})

		g.Expect(run.err).To(MatchError(injected))
		g.Expect(traitsReady(t, cr).Reason).To(Equal(conditionReasonNovaComputeListError))
		g.Expect(cr.Status.CreatedTraits).To(Equal([]string{"CUSTOM_A"}))
	})
}

func TestReconcileNovaComputeTraits_DrainingNodesAreNotWritten(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := activePool(activeIn("n2", testZone))
	cr.Status.Nodes = append(cr.Status.Nodes, novav1alpha1.NovaComputeNodeStatus{
		Name: "n1", Phase: novav1alpha1.NovaComputeNodeDraining, Zone: testZone, Traits: []string{"CUSTOM_A"},
	})
	cr.Spec.Traits = []string{"CUSTOM_B"}

	run := runTraits(t, cr, traitsPass(computeapitest.New()), interceptor.Funcs{},
		hypervisor("n1", hvSpec("CUSTOM_A"), hvStatus(metav1.ConditionTrue, "CUSTOM_A")), hypervisor("n2", hvSpec(), nil))

	g.Expect(run.err).NotTo(HaveOccurred())
	g.Expect(run.gets).To(Equal([]string{"n2"}))
	g.Expect(run.patches).NotTo(HaveKey("n1"))
	g.Expect(run.customTraitsOf(t, "n1")).To(Equal([]string{"CUSTOM_A"}))
	g.Expect(recordOf(cr, "n1")).To(Equal([]string{"CUSTOM_A"}), "the record of a leaving node stays")
}

func TestReconcileNovaComputeTraits_UnknownHostTraitsEntryIsNamed(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := activePool(activeIn("n1", testZone))
	cr.Spec.Traits = []string{"CUSTOM_A"}
	cr.Spec.HostTraits = []novav1alpha1.NovaComputeHostTraits{{NodeName: "ghost", Traits: []string{"CUSTOM_B"}}}

	run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{},
		hypervisor("n1", hvSpec("CUSTOM_A"), hvStatus(metav1.ConditionTrue, "CUSTOM_A")))

	g.Expect(run.err).NotTo(HaveOccurred())
	g.Expect(run.gets).To(Equal([]string{"n1"}), "no Hypervisor is read for the unknown entry")
	g.Expect(run.patches).To(BeEmpty())
	g.Expect(api.Calls()).To(BeEmpty())
	cond := traitsReady(t, cr)
	g.Expect(cond.Reason).To(Equal(conditionReasonTraitsApplied))
	g.Expect(cond.Message).To(Equal(
		"Every managed node carries its custom traits (1 nodes); hostTraits entries naming no node of the pool: ghost"))
}

// TestReconcileNovaComputeTraits_DroppingTheListBeforeItIsApplied pins that a
// list the hypervisor operator has not applied yet is still cleared: the
// written list is recorded, which keeps the node managed once the pool's
// traits are gone, and keeps the trait from being deleted under it.
func TestReconcileNovaComputeTraits_DroppingTheListBeforeItIsApplied(t *testing.T) {
	g := NewGomegaWithT(t)
	api := computeapitest.New()
	cr := activePool(activeIn("n1", testZone))
	cr.Spec.Traits = []string{"CUSTOM_A"}
	hv := hypervisor("n1", hvSpec(), map[string]any{})

	run := runTraits(t, cr, traitsPass(api), interceptor.Funcs{}, hv)
	g.Expect(run.err).NotTo(HaveOccurred())
	g.Expect(recordOf(cr, "n1")).To(Equal([]string{"CUSTOM_A"}))

	cr.Spec.Traits = nil
	written := hypervisor("n1", hvSpec("CUSTOM_A"), map[string]any{})
	run = runTraits(t, cr, traitsPass(api), interceptor.Funcs{}, written)

	g.Expect(run.err).NotTo(HaveOccurred())
	g.Expect(run.customTraitsOf(t, "n1")).To(BeEmpty())
	g.Expect(api.Traits()).To(BeEmpty(), "nothing carries it once the list is cleared")
	g.Expect(cr.Status.CreatedTraits).To(BeEmpty())
	g.Expect(traitsReady(t, cr).Reason).To(Equal(conditionReasonNoTraitsDeclared))
}

// TestTraitsCondition_FirstMatchWins pins the order of the TraitsReady reasons:
// each one wins over every reason below it.
func TestTraitsCondition_FirstMatchWins(t *testing.T) {
	cr := activePool(activeIn("n1", testZone))
	failedUpdate := &metav1.Condition{Type: hypervisorConditionTraitsUpdated, Status: metav1.ConditionFalse}
	// Each level adds its signal to the outcome; a level is checked with its own
	// signal and those of every level below it.
	levels := []struct {
		reason string
		add    func(o *traitsOutcome)
	}{
		{conditionReasonNodesWithCustomTraitsAnnotation, func(o *traitsOutcome) { o.annotated = []string{"n2"} }},
		{conditionReasonNodesWithoutHypervisor, func(o *traitsOutcome) { o.withoutHypervisor = []string{"n3"} }},
		{conditionReasonHypervisorLifecycleDisabled, func(o *traitsOutcome) {
			view := o.views["n1"]
			view.lifecycleEnabled = false
			o.views["n1"] = view
		}},
		{conditionReasonHypervisorTraitsFailed, func(o *traitsOutcome) {
			view := o.views["n1"]
			view.traitsUpdated = failedUpdate
			o.views["n1"] = view
		}},
		{conditionReasonWaitingForHypervisorOperator, func(o *traitsOutcome) {
			view := o.views["n1"]
			view.observed = nil
			o.views["n1"] = view
		}},
		{conditionReasonTraitsApplied, func(*traitsOutcome) {}},
	}
	for i, level := range levels {
		t.Run(level.reason, func(t *testing.T) {
			o := traitsOutcome{
				managed: []string{"n1"}, written: []string{"n1"},
				desired: map[string][]string{"n1": {"CUSTOM_A"}},
				views:   map[string]hypervisorView{"n1": {lifecycleEnabled: true, observed: []string{"CUSTOM_A"}}},
			}
			for _, lower := range levels[i:] {
				lower.add(&o)
			}

			NewGomegaWithT(t).Expect(traitsCondition(cr, o).Reason).To(Equal(level.reason))
		})
	}
}

func TestDesiredNodeTraits(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	cr.Spec.Traits = []string{"CUSTOM_B", "CUSTOM_A", "CUSTOM_B"}
	cr.Spec.HostTraits = []novav1alpha1.NovaComputeHostTraits{
		{NodeName: "pending", Traits: []string{"CUSTOM_A"}, Mode: novav1alpha1.NovaComputeHostTraitsAppend},
		{NodeName: "cleared", Mode: novav1alpha1.NovaComputeHostTraitsReplace},
		{NodeName: "conflict", Traits: []string{"CUSTOM_C"}},
		{NodeName: "draining", Traits: []string{"CUSTOM_C"}},
		{NodeName: "absent", Traits: []string{"CUSTOM_C"}},
	}
	cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{
		{Name: "active", Phase: novav1alpha1.NovaComputeNodeActive},
		{Name: "cleared", Phase: novav1alpha1.NovaComputeNodeActive},
		{Name: "conflict", Phase: novav1alpha1.NovaComputeNodeConflict},
		{Name: "draining", Phase: novav1alpha1.NovaComputeNodeDraining},
		{Name: "pending", Phase: novav1alpha1.NovaComputeNodePending},
	}

	desired, unknown := desiredNodeTraits(cr)

	g.Expect(desired).To(Equal(map[string][]string{
		"active":  {"CUSTOM_A", "CUSTOM_B"},
		"cleared": {},
		"pending": {"CUSTOM_A", "CUSTOM_B"},
	}))
	g.Expect(unknown).To(Equal([]string{"absent", "conflict"}), "a node in Conflict is not held")
	g.Expect(managedTraitNodes(cr, desired)).To(Equal([]string{"active", "pending"}))

	cr.DeletionTimestamp = ptr.To(metav1.Now())
	g.Expect(managedTraitNodes(cr, desired)).To(BeEmpty(), "a deleting pool manages no node")
}
