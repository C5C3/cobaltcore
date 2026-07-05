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
	ctrl "sigs.k8s.io/controller-runtime"
)

// passthroughInstrument records the names it saw and calls fn directly, so
// tests can assert which steps were routed through the instrument hook.
func passthroughInstrument(seen *[]string) InstrumentFunc {
	return func(ctx context.Context, name string, fn func(context.Context) (ctrl.Result, error)) (ctrl.Result, error) {
		*seen = append(*seen, name)
		return fn(ctx)
	}
}

func TestRunPipeline_RunsAllStepsOnSuccess(t *testing.T) {
	g := gomega.NewWithT(t)

	var ran []string
	var instrumented []string
	step := func(name string) Step {
		return Step{Name: name, Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, name)
			return ctrl.Result{}, nil
		}}
	}

	result, err := RunPipeline(context.Background(), passthroughInstrument(&instrumented),
		[]Step{step("a"), step("b"), step("c")})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result.IsZero()).To(gomega.BeTrue())
	g.Expect(ran).To(gomega.Equal([]string{"a", "b", "c"}))
	g.Expect(instrumented).To(gomega.Equal([]string{"a", "b", "c"}))
}

func TestRunPipeline_StopsOnError(t *testing.T) {
	g := gomega.NewWithT(t)

	var ran []string
	var instrumented []string
	boom := fmt.Errorf("step b failed")
	steps := []Step{
		{Name: "a", Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, "a")
			return ctrl.Result{}, nil
		}},
		{Name: "b", Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, "b")
			return ctrl.Result{}, boom
		}},
		{Name: "c", Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, "c")
			return ctrl.Result{}, nil
		}},
	}

	_, err := RunPipeline(context.Background(), passthroughInstrument(&instrumented), steps)

	g.Expect(err).To(gomega.MatchError(boom))
	g.Expect(ran).To(gomega.Equal([]string{"a", "b"}), "steps after the failing one must not run")
}

// The stop guard is exactly !result.IsZero() || err != nil, so a non-zero
// RequeueAfter with a nil error must short-circuit the chain too.
func TestRunPipeline_StopsOnNonZeroResult(t *testing.T) {
	g := gomega.NewWithT(t)

	var ran []string
	var instrumented []string
	requeue := ctrl.Result{RequeueAfter: 15 * time.Second}
	steps := []Step{
		{Name: "a", Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, "a")
			return requeue, nil
		}},
		{Name: "b", Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, "b")
			return ctrl.Result{}, nil
		}},
	}

	result, err := RunPipeline(context.Background(), passthroughInstrument(&instrumented), steps)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.Equal(requeue))
	g.Expect(ran).To(gomega.Equal([]string{"a"}))
}

// Empty-Name steps run bare: a parallel group self-instruments its members
// and a deliberately uninstrumented step must not emit a metric sample.
func TestRunPipeline_EmptyNameBypassesInstrument(t *testing.T) {
	g := gomega.NewWithT(t)

	var ran []string
	var instrumented []string
	steps := []Step{
		{Name: "named", Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, "named")
			return ctrl.Result{}, nil
		}},
		{Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, "anonymous")
			return ctrl.Result{}, nil
		}},
	}

	_, err := RunPipeline(context.Background(), passthroughInstrument(&instrumented), steps)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ran).To(gomega.Equal([]string{"named", "anonymous"}))
	g.Expect(instrumented).To(gomega.Equal([]string{"named"}),
		"the empty-Name step must bypass the instrument hook")
}

func TestShortestRequeue_AllZero(t *testing.T) {
	g := gomega.NewWithT(t)

	result := ShortestRequeue(ctrl.Result{}, ctrl.Result{}, ctrl.Result{})

	g.Expect(result).To(gomega.Equal(ctrl.Result{}),
		"all-zero inputs must produce a zero Result")
}

func TestShortestRequeue_SingleNonZero(t *testing.T) {
	g := gomega.NewWithT(t)

	result := ShortestRequeue(
		ctrl.Result{},
		ctrl.Result{RequeueAfter: 15 * time.Second},
		ctrl.Result{},
	)

	g.Expect(result).To(gomega.Equal(ctrl.Result{RequeueAfter: 15 * time.Second}),
		"single non-zero RequeueAfter must be returned")
}

func TestShortestRequeue_PicksMinimum(t *testing.T) {
	g := gomega.NewWithT(t)

	result := ShortestRequeue(
		ctrl.Result{RequeueAfter: 30 * time.Second},
		ctrl.Result{RequeueAfter: 15 * time.Second},
	)

	g.Expect(result).To(gomega.Equal(ctrl.Result{RequeueAfter: 15 * time.Second}),
		"must pick the shortest non-zero RequeueAfter")
}

func TestShortestRequeue_NoArgs(t *testing.T) {
	g := gomega.NewWithT(t)

	result := ShortestRequeue()

	g.Expect(result).To(gomega.Equal(ctrl.Result{}),
		"zero arguments must produce a zero Result")
}
