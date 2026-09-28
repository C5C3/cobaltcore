// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/multicluster"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

const vpaCondition = "VPAReady"

func vpaScheme() *runtime.Scheme {
	s := newScheme()
	_ = vpav1.AddToScheme(s)
	return s
}

func vpaLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": "svc", "app.kubernetes.io/instance": "test-owner"}
}

func vpaParams(conds *[]metav1.Condition, targets ...VPATarget) VPAFlowParams {
	return VPAFlowParams{
		Targets:        targets,
		Namespace:      "default",
		Labels:         vpaLabels(),
		LocalAvailable: true,
		Conditions:     conds,
		Generation:     5,
		ConditionType:  vpaCondition,
	}
}

func offTarget(name string) VPATarget {
	return VPATarget{Kind: "Deployment", Name: name, Spec: &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}}
}

// seededVPA is a VPA the flow created earlier: it carries the CR's labels and
// the controller reference to testOwner.
func seededVPA(name string) *vpav1.VerticalPodAutoscaler {
	return &vpav1.VerticalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "default", Labels: vpaLabels(),
		OwnerReferences: []metav1.OwnerReference{
			*metav1.NewControllerRef(testOwner(), corev1.SchemeGroupVersion.WithKind("ConfigMap")),
		},
	}}
}

// copiedVPA is somebody else's VPA that copied the CR's labels from the
// workload: it carries them but no controller reference and no ownership
// labels.
func copiedVPA(name string) *vpav1.VerticalPodAutoscaler {
	vpa := seededVPA(name)
	vpa.OwnerReferences = nil
	return vpa
}

// failingOps records every List, Apply and Delete and fails the one named in
// fail, so a test can show both what the flow reports and what it touched.
type failingOps struct {
	fail  string
	err   error
	calls []string
}

func (f *failingOps) funcs() interceptor.Funcs {
	record := func(op string) error {
		f.calls = append(f.calls, op)
		if op == f.fail {
			return f.err
		}
		return nil
	}
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := record("list"); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if err := record("apply"); err != nil {
				return err
			}
			return c.Apply(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := record("delete"); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
}

func TestBuildVPA_RendersTheTargetAndOneContainerPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	vpa := BuildVPA("default", vpaLabels(), VPATarget{Kind: "StatefulSet", Name: "db-nb", Spec: &commonv1.VerticalAutoscalingSpec{
		UpdateMode:  "Auto",
		MinReplicas: ptr.To(int32(1)),
		MinAllowed:  corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("70m")},
		MaxAllowed:  corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
	}})

	g.Expect(vpa.Name).To(Equal("db-nb"))
	g.Expect(vpa.Namespace).To(Equal("default"))
	g.Expect(vpa.Labels).To(Equal(vpaLabels()))
	g.Expect(vpa.Spec.TargetRef.APIVersion).To(Equal("apps/v1"))
	g.Expect(vpa.Spec.TargetRef.Kind).To(Equal("StatefulSet"))
	g.Expect(vpa.Spec.TargetRef.Name).To(Equal("db-nb"))
	g.Expect(vpa.Spec.UpdatePolicy.UpdateMode).To(HaveValue(Equal(vpav1.UpdateMode("Auto"))))
	g.Expect(vpa.Spec.UpdatePolicy.MinReplicas).To(HaveValue(Equal(int32(1))))
	g.Expect(vpa.Spec.ResourcePolicy.ContainerPolicies).To(HaveLen(1))
	policy := vpa.Spec.ResourcePolicy.ContainerPolicies[0]
	g.Expect(policy.ContainerName).To(Equal("*"))
	g.Expect(policy.ControlledValues).To(HaveValue(Equal(vpav1.ContainerControlledValuesRequestsOnly)))
	g.Expect(policy.ControlledResources).To(HaveValue(Equal([]corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory})))
	g.Expect(policy.MinAllowed).To(Equal(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("70m")}))
	g.Expect(policy.MaxAllowed).To(HaveLen(2))
}

// A block that sets only updateMode renders neither minReplicas nor either
// bound, so the VPA defaults apply and a server-side apply owns none of them.
func TestBuildVPA_UnsetFieldsRenderNoKey(t *testing.T) {
	g := NewGomegaWithT(t)
	vpa := BuildVPA("default", vpaLabels(), offTarget("svc"))

	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(vpa)
	g.Expect(err).NotTo(HaveOccurred())
	policies, _, _ := unstructuredSlice(raw, "spec", "resourcePolicy", "containerPolicies")
	g.Expect(policies).To(HaveLen(1))
	g.Expect(policies[0]).To(Equal(map[string]any{
		"containerName":       "*",
		"controlledValues":    "RequestsOnly",
		"controlledResources": []any{"cpu", "memory"},
	}))
	updatePolicy, _, _ := unstructuredMap(raw, "spec", "updatePolicy")
	g.Expect(updatePolicy).To(Equal(map[string]any{"updateMode": "Off"}))
}

// The rendered VPA shares no map with the CR's block, so the flow cannot write
// into the spec it was handed.
func TestBuildVPA_CopiesTheBounds(t *testing.T) {
	g := NewGomegaWithT(t)
	spec := &commonv1.VerticalAutoscalingSpec{
		UpdateMode: "Off", MinReplicas: ptr.To(int32(2)),
		MinAllowed: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
	vpa := BuildVPA("default", vpaLabels(), VPATarget{Kind: "Deployment", Name: "svc", Spec: spec})

	vpa.Spec.ResourcePolicy.ContainerPolicies[0].MinAllowed[corev1.ResourceMemory] = resource.MustParse("8Gi")
	*vpa.Spec.UpdatePolicy.MinReplicas = 9
	g.Expect(spec.MinAllowed[corev1.ResourceMemory]).To(Equal(resource.MustParse("128Mi")))
	g.Expect(*spec.MinReplicas).To(Equal(int32(2)))
}

func TestReconcileVPAs_NoTargets_NotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaScheme()
	ops := &failingOps{}
	c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(ops.funcs()).Build()
	var conds []metav1.Condition

	res, err := ReconcileVPAs(context.Background(), c, s, testOwner(), vpaParams(&conds))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(ops.calls).To(Equal([]string{"list"}), "nothing is wanted, so nothing is applied")
	var list vpav1.VerticalPodAutoscalerList
	g.Expect(c.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
	cond := conditions.GetCondition(conds, vpaCondition)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonVPANotRequired))
	g.Expect(cond.Message).To(Equal("no workload opts into vertical autoscaling"))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(5)))
}

// Targets that do not opt in leave no VPA behind: one the CR controls is an
// opt-out the prune step removes, while one without the CR's labels, or with
// labels copied from the workload but no ownership, is not the CR's and stays.
func TestReconcileVPAs_AllNilSpecs_DeletesStale(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaScheme()
	foreign := &vpav1.VerticalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "hand-written", Namespace: "default"}}
	copied := copiedVPA("svc-worker")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seededVPA("svc"), foreign, copied).Build()
	var conds []metav1.Condition

	_, err := ReconcileVPAs(context.Background(), c, s, testOwner(),
		vpaParams(&conds, VPATarget{Kind: "Deployment", Name: "svc"}, VPATarget{Kind: "Deployment", Name: "svc-worker"}))
	g.Expect(err).NotTo(HaveOccurred())
	err = c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "svc"}, &vpav1.VerticalPodAutoscaler{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &vpav1.VerticalPodAutoscaler{})).To(Succeed())
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(copied), &vpav1.VerticalPodAutoscaler{})).To(Succeed(),
		"the labels select, but a VPA the CR does not control is never deleted")
	cond := conditions.GetCondition(conds, vpaCondition)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonVPANotRequired))
}

// A VPA whose ownership cannot be decided is neither deleted nor passed over:
// the pass fails with VPAError.
func TestReconcileVPAs_OwnershipUndecidable_VPAError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := runtime.NewScheme()
	_ = vpav1.AddToScheme(s) // no ConfigMap: the owner's kind cannot be resolved
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seededVPA("stale")).Build()
	var conds []metav1.Condition

	_, err := ReconcileVPAs(context.Background(), c, s, testOwner(), vpaParams(&conds, VPATarget{Kind: "Deployment", Name: "svc"}))
	g.Expect(err).To(MatchError(ContainSubstring("checking ownership of VerticalPodAutoscaler default/stale")))
	g.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "stale"}, &vpav1.VerticalPodAutoscaler{})).To(Succeed())
	cond := conditions.GetCondition(conds, vpaCondition)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(ReasonVPAError))
}

func TestReconcileVPAs_LocalCreatesWithOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(testOwner()).Build()
	var conds []metav1.Condition
	p := vpaParams(&conds, offTarget("svc"), VPATarget{Kind: "Deployment", Name: "svc-worker"})

	res, err := ReconcileVPAs(context.Background(), c, s, testOwner(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	created := &vpav1.VerticalPodAutoscaler{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "svc"}, created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].Controller).To(HaveValue(BeTrue()))
	g.Expect(created.Labels).To(Equal(vpaLabels()))
	g.Expect(created.Spec.TargetRef.Name).To(Equal("svc"))
	cond := conditions.GetCondition(conds, vpaCondition)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonVPAReady))
	g.Expect(cond.Message).To(Equal("VerticalPodAutoscalers are configured: svc"))

	// A second pass with the same input changes nothing.
	before := created.DeepCopy()
	_, err = ReconcileVPAs(context.Background(), c, s, testOwner(), p)
	g.Expect(err).NotTo(HaveOccurred())
	after := &vpav1.VerticalPodAutoscaler{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(created), after)).To(Succeed())
	g.Expect(after.Spec).To(Equal(before.Spec))
	g.Expect(after.Labels).To(Equal(before.Labels))
	g.Expect(after.OwnerReferences).To(Equal(before.OwnerReferences))
	var list vpav1.VerticalPodAutoscalerList
	g.Expect(c.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}

func TestReconcileVPAs_RemoteCreatesWithOwnershipLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaScheme()
	local := fake.NewClientBuilder().WithScheme(s).Build()
	target := mctestutil.TargetFake(fake.NewClientBuilder().WithScheme(s), VPAGVK)
	children := mctestutil.RemoteChildren(t, local, target)
	var conds []metav1.Condition
	p := vpaParams(&conds, offTarget("svc"))
	p.LocalAvailable = false // the target answers for itself

	_, err := ReconcileVPAs(context.Background(), children, s, testOwner(), p)
	g.Expect(err).NotTo(HaveOccurred())
	created := &vpav1.VerticalPodAutoscaler{}
	g.Expect(target.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "svc"}, created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(BeEmpty())
	g.Expect(created.Labels).To(HaveKeyWithValue(multicluster.OwnerKindLabel, "ConfigMap"))
	g.Expect(created.Labels).To(HaveKeyWithValue(multicluster.OwnerNameLabel, "test-owner"))
	g.Expect(created.Labels).To(HaveKeyWithValue(multicluster.OwnerNamespaceLabel, "default"))
	g.Expect(conditions.GetCondition(conds, vpaCondition).Reason).To(Equal(ReasonVPAReady))
	g.Expect(local.Get(context.Background(), client.ObjectKeyFromObject(created), &vpav1.VerticalPodAutoscaler{})).
		NotTo(Succeed(), "a remote child lands on the target only")

	// Dropping the block removes the VPA from the target, where the prune list
	// runs, and leaves one that only copied the CR's labels. The params are
	// built afresh, as every reconcile pass builds them.
	copied := copiedVPA("svc-copy")
	g.Expect(target.Create(context.Background(), copied)).To(Succeed())
	p = vpaParams(&conds, VPATarget{Kind: "Deployment", Name: "svc"})
	p.LocalAvailable = false
	_, err = ReconcileVPAs(context.Background(), children, s, testOwner(), p)
	g.Expect(err).NotTo(HaveOccurred())
	err = target.Get(context.Background(), client.ObjectKeyFromObject(created), &vpav1.VerticalPodAutoscaler{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(target.Get(context.Background(), client.ObjectKeyFromObject(copied), &vpav1.VerticalPodAutoscaler{})).To(Succeed())
	g.Expect(conditions.GetCondition(conds, vpaCondition).Reason).To(Equal(ReasonVPANotRequired))
}

func TestReconcileVPAs_NotServedLocal_SetsNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaScheme()
	ops := &failingOps{}
	c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(ops.funcs()).Build()
	var conds []metav1.Condition
	p := vpaParams(&conds, offTarget("svc"), offTarget("svc-worker"), VPATarget{Kind: "Deployment", Name: "svc-other"})
	p.LocalAvailable = false

	res, err := ReconcileVPAs(context.Background(), c, s, testOwner(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(ops.calls).To(BeEmpty(), "no VPA can exist on the cluster, so nothing is listed, applied or deleted")
	cond := conditions.GetCondition(conds, vpaCondition)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(ReasonVPANotInstalled))
	g.Expect(cond.Message).To(ContainSubstring("does not serve autoscaling.k8s.io/v1 VerticalPodAutoscaler"))
	g.Expect(cond.Message).To(ContainSubstring("opted-in workloads: svc, svc-worker ("))
	g.Expect(cond.Message).To(ContainSubstring("restart the operator"))
}

func TestReconcileVPAs_NotServedRemote_SetsNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaScheme()
	ops := &failingOps{}
	local := fake.NewClientBuilder().WithScheme(s).Build()
	target := mctestutil.TargetFake(fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(ops.funcs()))
	children := mctestutil.RemoteChildren(t, local, target)
	var conds []metav1.Condition
	p := vpaParams(&conds, offTarget("svc"))
	// The management cluster serves the kind; the target does not, and the
	// target's answer is the one that counts.
	p.LocalAvailable = true

	_, err := ReconcileVPAs(context.Background(), children, s, testOwner(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ops.calls).To(BeEmpty())
	cond := conditions.GetCondition(conds, vpaCondition)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(ReasonVPANotInstalled))
	g.Expect(cond.Message).To(ContainSubstring("target cluster does not serve autoscaling.k8s.io/v1 VerticalPodAutoscaler"))
	g.Expect(cond.Message).To(ContainSubstring("opted-in workloads: svc"))
	g.Expect(cond.Message).NotTo(ContainSubstring("restart"))
}

func TestReconcileVPAs_NotServed_NoOptIn_NotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()
	var conds []metav1.Condition
	p := vpaParams(&conds, VPATarget{Kind: "Deployment", Name: "svc"})
	p.LocalAvailable = false

	_, err := ReconcileVPAs(context.Background(), c, s, testOwner(), p)
	g.Expect(err).NotTo(HaveOccurred())
	cond := conditions.GetCondition(conds, vpaCondition)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonVPANotRequired))
}

func TestReconcileVPAs_Unprobeable_CapabilityProbeFailed(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaScheme()
	c := mctestutil.UnprobeableChildren(fake.NewClientBuilder().WithScheme(s).Build())
	var conds []metav1.Condition

	_, err := ReconcileVPAs(context.Background(), c, s, testOwner(), vpaParams(&conds, offTarget("svc")))
	g.Expect(err).To(MatchError(ContainSubstring("probing the target cluster for the VerticalPodAutoscaler kind")))
	g.Expect(err).To(MatchError(ContainSubstring("discovery is unavailable")))
	cond := conditions.GetCondition(conds, vpaCondition)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(multicluster.CapabilityProbeFailed))
}

func TestReconcileVPAs_ClientErrors(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name    string
		fail    string
		err     error
		wantErr string
	}{
		{name: "list", fail: "list", err: boom, wantErr: "listing VerticalPodAutoscalers: boom"},
		{name: "apply", fail: "apply", err: boom, wantErr: "ensuring VerticalPodAutoscaler default/svc: "},
		{name: "delete", fail: "delete", err: boom, wantErr: "deleting VerticalPodAutoscaler default/stale: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := vpaScheme()
			ops := &failingOps{fail: tc.fail, err: tc.err}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(testOwner(), seededVPA("stale")).
				WithInterceptorFuncs(ops.funcs()).Build()
			var conds []metav1.Condition

			res, err := ReconcileVPAs(context.Background(), c, s, testOwner(), vpaParams(&conds, offTarget("svc")))
			g.Expect(err).To(MatchError(ContainSubstring(tc.wantErr)))
			g.Expect(errors.Is(err, boom)).To(BeTrue())
			g.Expect(res.IsZero()).To(BeTrue())
			cond := conditions.GetCondition(conds, vpaCondition)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(ReasonVPAError))
		})
	}
}

// A VPA that vanishes between the list and the delete is the outcome the delete
// wanted, not an error.
func TestReconcileVPAs_DeleteNotFoundIsTolerated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaScheme()
	ops := &failingOps{fail: "delete", err: apierrors.NewNotFound(vpav1.Resource("verticalpodautoscaler"), "stale")}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(testOwner(), seededVPA("stale")).
		WithInterceptorFuncs(ops.funcs()).Build()
	var conds []metav1.Condition

	_, err := ReconcileVPAs(context.Background(), c, s, testOwner(), vpaParams(&conds, offTarget("svc")))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ops.calls).To(Equal([]string{"list", "apply", "delete"}))
	g.Expect(conditions.GetCondition(conds, vpaCondition).Reason).To(Equal(ReasonVPAReady))
}

func TestReconcileVPAs_OptOutOfOneTarget_DeletesOnlyThatVPA(t *testing.T) {
	g := NewGomegaWithT(t)
	s := vpaScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(testOwner()).Build()
	var conds []metav1.Condition

	_, err := ReconcileVPAs(context.Background(), c, s, testOwner(), vpaParams(&conds, offTarget("svc"), offTarget("svc-worker")))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(conditions.GetCondition(conds, vpaCondition).Message).To(Equal("VerticalPodAutoscalers are configured: svc, svc-worker"))

	_, err = ReconcileVPAs(context.Background(), c, s, testOwner(),
		vpaParams(&conds, offTarget("svc"), VPATarget{Kind: "Deployment", Name: "svc-worker"}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "svc"}, &vpav1.VerticalPodAutoscaler{})).To(Succeed())
	err = c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "svc-worker"}, &vpav1.VerticalPodAutoscaler{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	cond := conditions.GetCondition(conds, vpaCondition)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonVPAReady))
	g.Expect(cond.Message).To(Equal("VerticalPodAutoscalers are configured: svc"))
}

func unstructuredMap(obj map[string]any, fields ...string) (map[string]any, bool, error) {
	cur := obj
	for _, f := range fields {
		next, ok := cur[f].(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur = next
	}
	return cur, true, nil
}

func unstructuredSlice(obj map[string]any, fields ...string) ([]map[string]any, bool, error) {
	parent, ok, _ := unstructuredMap(obj, fields[:len(fields)-1]...)
	if !ok {
		return nil, false, nil
	}
	raw, ok := parent[fields[len(fields)-1]].([]any)
	if !ok {
		return nil, false, nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		out = append(out, m)
	}
	return out, true, nil
}
