// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"errors"
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

func TestRunSequentialGroup_RunsAllStepsAndAggregatesShortestRequeue(t *testing.T) {
	g := gomega.NewWithT(t)

	var ran []string
	var instrumented []string
	step := func(name string, requeue time.Duration) Step {
		return Step{Name: name, Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, name)
			return ctrl.Result{RequeueAfter: requeue}, nil
		}}
	}

	result, err := RunSequentialGroup(context.Background(), passthroughInstrument(&instrumented),
		[]Step{
			step("a", 15*time.Second),
			step("b", 0),
			step("c", 10*time.Second),
		})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ran).To(gomega.Equal([]string{"a", "b", "c"}),
		"every member must run once, in slice order")
	g.Expect(result).To(gomega.Equal(ctrl.Result{RequeueAfter: 10 * time.Second}),
		"the shortest non-zero member RequeueAfter must win")
}

func TestRunSequentialGroup_JoinsErrorsAndDiscardsRequeues(t *testing.T) {
	g := gomega.NewWithT(t)

	var instrumented []string
	errA := errors.New("member a failed")
	errB := errors.New("member b failed")
	steps := []Step{
		{Name: "a", Fn: func(context.Context) (ctrl.Result, error) {
			return ctrl.Result{}, errA
		}},
		{Name: "b", Fn: func(context.Context) (ctrl.Result, error) {
			return ctrl.Result{}, errB
		}},
		{Name: "c", Fn: func(context.Context) (ctrl.Result, error) {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}},
	}

	result, err := RunSequentialGroup(context.Background(), passthroughInstrument(&instrumented), steps)

	g.Expect(errors.Is(err, errA)).To(gomega.BeTrue(), "the joined error must retain errA")
	g.Expect(errors.Is(err, errB)).To(gomega.BeTrue(), "the joined error must retain errB")
	g.Expect(result).To(gomega.Equal(ctrl.Result{}),
		"member requeues must be discarded on the error path")
}

func TestRunSequentialGroup_ContinuesAfterError(t *testing.T) {
	g := gomega.NewWithT(t)

	var instrumented []string
	var reached bool
	steps := []Step{
		{Name: "boom", Fn: func(context.Context) (ctrl.Result, error) {
			return ctrl.Result{}, fmt.Errorf("boom")
		}},
		{Name: "after", Fn: func(context.Context) (ctrl.Result, error) {
			reached = true
			return ctrl.Result{}, nil
		}},
	}

	_, err := RunSequentialGroup(context.Background(), passthroughInstrument(&instrumented), steps)

	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(reached).To(gomega.BeTrue(),
		"a later step must still run after an earlier step errored")
}

func TestRunSequentialGroup_InstrumentsNamedMembersOnly(t *testing.T) {
	g := gomega.NewWithT(t)

	var ran []string
	var instrumented []string
	steps := []Step{
		{Name: "first", Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, "first")
			return ctrl.Result{}, nil
		}},
		{Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, "anonymous")
			return ctrl.Result{}, nil
		}},
		{Name: "second", Fn: func(context.Context) (ctrl.Result, error) {
			ran = append(ran, "second")
			return ctrl.Result{}, nil
		}},
	}

	_, err := RunSequentialGroup(context.Background(), passthroughInstrument(&instrumented), steps)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ran).To(gomega.Equal([]string{"first", "anonymous", "second"}),
		"every member must run")
	g.Expect(instrumented).To(gomega.Equal([]string{"first", "second"}),
		"only named members are routed through the instrument hook, once each in order")
}

func TestRunSequentialGroup_NilSteps(t *testing.T) {
	g := gomega.NewWithT(t)

	var instrumented []string
	result, err := RunSequentialGroup(context.Background(), passthroughInstrument(&instrumented), nil)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.Equal(ctrl.Result{}), "a nil steps slice must produce a zero Result")
}

func TestRunSequentialGroup_EmptySteps(t *testing.T) {
	g := gomega.NewWithT(t)

	var instrumented []string
	result, err := RunSequentialGroup(context.Background(), passthroughInstrument(&instrumented), []Step{})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.Equal(ctrl.Result{}), "an empty steps slice must produce a zero Result")
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
