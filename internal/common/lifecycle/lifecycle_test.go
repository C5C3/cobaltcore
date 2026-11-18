// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package lifecycle_test

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/cobaltcore/internal/common/lifecycle"
	"github.com/c5c3/cobaltcore/internal/common/reconcile"
)

func TestReadyReportsCurrentEvidence(t *testing.T) {
	g := NewWithT(t)
	conds := []metav1.Condition{{Type: "Config", Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "Rendered"}}
	required := []string{"Config", "Workload"}
	check := func(status metav1.ConditionStatus, reason, message string, err error) {
		t.Helper()
		lifecycle.SetReady(&conds, 2, required, err)
		c := meta.FindStatusCondition(conds, "Ready")
		g.Expect(c.Status).To(Equal(status))
		g.Expect(c.Reason).To(Equal(reason))
		g.Expect(c.Message).To(ContainSubstring(message))
		g.Expect(c.ObservedGeneration).To(Equal(int64(2)))
	}
	check(metav1.ConditionFalse, "NotAllReady", "Config last observed generation 1", nil)
	conds[0].ObservedGeneration = 2
	check(metav1.ConditionFalse, "NotAllReady", "Workload has not been reported", nil)
	meta.SetStatusCondition(&conds, metav1.Condition{Type: "Workload", Status: metav1.ConditionTrue, ObservedGeneration: 2, Reason: "Available"})
	check(metav1.ConditionTrue, "AllReady", "ready", nil)
	err := errors.New("apply failed")
	check(metav1.ConditionFalse, "ReconcileError", "failed", err)
	lifecycle.StepFailed(&conds, 2, "Deploy", "Workload", err)
	check(metav1.ConditionFalse, "NotAllReady", "Workload: ReconcileError", err)
	meta.SetStatusCondition(&conds, metav1.Condition{Type: "Workload", Status: metav1.ConditionFalse, ObservedGeneration: 2, Reason: "ImageUnavailable"})
	lifecycle.StepFailed(&conds, 2, "Deploy", "Workload", err)
	check(metav1.ConditionFalse, "NotAllReady", "Workload: ImageUnavailable", err)
}

// A built-in status subresource exercises the driver without depending on any
// operator module. A second writer races the first pass after its snapshot.
func TestDriverRecoversFromStatusConflictAndSkipsUnchangedWrite(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	obj := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "default", Generation: 2, Finalizers: []string{"example.org/service"}}}
	c := fake.NewClientBuilder().WithStatusSubresource(obj).WithObjects(obj).Build()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
	race := true
	driver := lifecycle.Driver[*policyv1.PodDisruptionBudget, policyv1.PodDisruptionBudgetStatus]{
		Client: c, Finalizer: "example.org/service", ReadyConditions: []string{"Dependency"},
		Fields: func(p *policyv1.PodDisruptionBudget) lifecycle.Fields[policyv1.PodDisruptionBudgetStatus] {
			return lifecycle.Fields[policyv1.PodDisruptionBudgetStatus]{Status: &p.Status, Conditions: &p.Status.Conditions, ObservedGeneration: &p.Status.ObservedGeneration}
		},
		Sync: func(ctx context.Context, children client.Client, p *policyv1.PodDisruptionBudget) (ctrl.Result, error) {
			if race {
				concurrent := p.DeepCopy()
				concurrent.Labels = map[string]string{"writer": "other"}
				g.Expect(children.Update(ctx, concurrent)).To(Succeed())
				race = false
			}
			meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "Dependency", Status: metav1.ConditionTrue, ObservedGeneration: p.Generation, Reason: "Available"})
			return ctrl.Result{}, nil
		},
	}
	run := func() ctrl.Result {
		t.Helper()
		result, err := driver.Reconcile(ctx, req, &policyv1.PodDisruptionBudget{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(c.Get(ctx, req.NamespacedName, obj)).To(Succeed())
		return result
	}
	g.Expect(run().RequeueAfter).To(Equal(reconcile.RequeueNextPass))
	g.Expect(obj.Status.Conditions).To(BeEmpty())
	g.Expect(run()).To(Equal(ctrl.Result{}))
	g.Expect(lifecycle.CurrentTrue(obj.Status.Conditions, "Ready", 2)).To(BeTrue())
	before := obj.DeepCopy()
	g.Expect(run()).To(Equal(ctrl.Result{}))
	g.Expect(obj.Status).To(Equal(before.Status))
	g.Expect(obj.ResourceVersion).To(Equal(before.ResourceVersion))
}
