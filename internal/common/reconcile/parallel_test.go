// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

// fakeCR is a minimal CR stand-in carrying only status conditions, satisfying
// the DeepCopy constraint of RunParallelGroup.
type fakeCR struct {
	conditions []metav1.Condition
}

func (f *fakeCR) DeepCopy() *fakeCR {
	out := &fakeCR{conditions: make([]metav1.Condition, len(f.conditions))}
	copy(out.conditions, f.conditions)
	return out
}

func fakeConditions(f *fakeCR) *[]metav1.Condition { return &f.conditions }

// noopInstrument satisfies InstrumentFunc without recording anything.
func noopInstrument(ctx context.Context, _ string, fn func(context.Context) (ctrl.Result, error)) (ctrl.Result, error) {
	return fn(ctx)
}

func setCond(cr *fakeCR, condType string) {
	meta.SetStatusCondition(&cr.conditions, metav1.Condition{
		Type:   condType,
		Status: metav1.ConditionTrue,
		Reason: "Ready",
	})
}

func TestRunParallelGroup_MergesConditionsAndPicksShortestRequeue(t *testing.T) {
	g := gomega.NewWithT(t)
	cr := &fakeCR{}

	steps := []ParallelStep[*fakeCR]{
		{Name: "a", ConditionType: "AReady", Fn: func(_ context.Context, c *fakeCR) (ctrl.Result, error) {
			setCond(c, "AReady")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}},
		{Name: "b", ConditionType: "BReady", Fn: func(_ context.Context, c *fakeCR) (ctrl.Result, error) {
			setCond(c, "BReady")
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}},
	}

	result, err := RunParallelGroup(context.Background(), cr, fakeConditions, noopInstrument, steps)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.Equal(ctrl.Result{RequeueAfter: 15 * time.Second}))
	g.Expect(meta.FindStatusCondition(cr.conditions, "AReady")).NotTo(gomega.BeNil())
	g.Expect(meta.FindStatusCondition(cr.conditions, "BReady")).NotTo(gomega.BeNil())
}

// A failing member must propagate its error while the succeeding peer's
// condition is still merged onto the primary CR — partial progress stays
// visible in status.
func TestRunParallelGroup_PartialFailureMergesSucceededConditions(t *testing.T) {
	g := gomega.NewWithT(t)
	cr := &fakeCR{}

	steps := []ParallelStep[*fakeCR]{
		{Name: "ok", ConditionType: "OKReady", Fn: func(_ context.Context, c *fakeCR) (ctrl.Result, error) {
			setCond(c, "OKReady")
			return ctrl.Result{}, nil
		}},
		{Name: "boom", ConditionType: "BoomReady", Fn: func(_ context.Context, _ *fakeCR) (ctrl.Result, error) {
			return ctrl.Result{}, fmt.Errorf("member failed")
		}},
	}

	result, err := RunParallelGroup(context.Background(), cr, fakeConditions, noopInstrument, steps)

	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("member failed")))
	g.Expect(result).To(gomega.Equal(ctrl.Result{}), "a failed group returns a zero result")
	g.Expect(meta.FindStatusCondition(cr.conditions, "OKReady")).NotTo(gomega.BeNil(),
		"the succeeded member's condition must be merged despite the peer failure")
	g.Expect(meta.FindStatusCondition(cr.conditions, "BoomReady")).To(gomega.BeNil(),
		"the failed member set no condition, so none may appear")
}

// Each member operates on its own DeepCopy: a condition a member sets outside
// its declared ConditionType must NOT leak onto the primary CR.
func TestRunParallelGroup_DeepCopyIsolation(t *testing.T) {
	g := gomega.NewWithT(t)
	cr := &fakeCR{}

	steps := []ParallelStep[*fakeCR]{
		{Name: "sneaky", ConditionType: "DeclaredReady", Fn: func(_ context.Context, c *fakeCR) (ctrl.Result, error) {
			setCond(c, "DeclaredReady")
			setCond(c, "UndeclaredReady")
			return ctrl.Result{}, nil
		}},
	}

	_, err := RunParallelGroup(context.Background(), cr, fakeConditions, noopInstrument, steps)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(meta.FindStatusCondition(cr.conditions, "DeclaredReady")).NotTo(gomega.BeNil())
	g.Expect(meta.FindStatusCondition(cr.conditions, "UndeclaredReady")).To(gomega.BeNil(),
		"only the declared condition type is merged back from the member's copy")
}

// Errgroup cancels the derived context when a member fails; a peer blocking
// on ctx.Done() proves the cancellation propagates (the test would hang
// otherwise).
func TestRunParallelGroup_ErrorCancelsPeers(t *testing.T) {
	g := gomega.NewWithT(t)
	cr := &fakeCR{}

	steps := []ParallelStep[*fakeCR]{
		{Name: "boom", ConditionType: "BoomReady", Fn: func(_ context.Context, _ *fakeCR) (ctrl.Result, error) {
			return ctrl.Result{}, fmt.Errorf("boom")
		}},
		{Name: "waiter", ConditionType: "WaiterReady", Fn: func(ctx context.Context, _ *fakeCR) (ctrl.Result, error) {
			<-ctx.Done()
			return ctrl.Result{}, nil
		}},
	}

	_, err := RunParallelGroup(context.Background(), cr, fakeConditions, noopInstrument, steps)

	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("boom")))
}

func TestMergeCondition_CopiesRequestedType(t *testing.T) {
	g := gomega.NewWithT(t)

	var dst []metav1.Condition
	src := []metav1.Condition{
		{Type: "AReady", Status: metav1.ConditionTrue, Reason: "Ready"},
		{Type: "BReady", Status: metav1.ConditionTrue, Reason: "Ready"},
	}

	MergeCondition(&dst, src, "AReady")

	g.Expect(meta.FindStatusCondition(dst, "AReady")).NotTo(gomega.BeNil())
	g.Expect(meta.FindStatusCondition(dst, "BReady")).To(gomega.BeNil())
}

// A missing condition type leaves dst untouched — including pre-existing
// conditions.
func TestMergeCondition_MissingTypeLeavesDstUnchanged(t *testing.T) {
	g := gomega.NewWithT(t)

	dst := []metav1.Condition{{Type: "Existing", Status: metav1.ConditionTrue, Reason: "Ready"}}

	MergeCondition(&dst, nil, "AReady")

	g.Expect(dst).To(gomega.HaveLen(1))
	g.Expect(meta.FindStatusCondition(dst, "Existing")).NotTo(gomega.BeNil())
}
