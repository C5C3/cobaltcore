// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"testing"
	"time"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

// assertConcurrencyFallback asserts the worker count an options constructor
// yields: the shared default when the value is unset (<= 0), and an explicit
// positive value unchanged. It takes the constructor rather than calling one,
// so each entry point is exercised through its own signature.
func assertConcurrencyFallback[request comparable](
	t *testing.T,
	options func(int) crcontroller.TypedOptions[request],
) {
	t.Helper()
	g := gomega.NewWithT(t)

	g.Expect(options(0).MaxConcurrentReconciles).
		To(gomega.Equal(DefaultMaxConcurrentReconciles), "zero value must fall back to the default")
	g.Expect(options(-1).MaxConcurrentReconciles).
		To(gomega.Equal(DefaultMaxConcurrentReconciles), "negative value must fall back to the default")
	g.Expect(options(8).MaxConcurrentReconciles).
		To(gomega.Equal(8), "explicit positive value must pass through")
}

// assertTunedRateLimiter asserts the tuned failure limiter starts at the base
// delay, grows exponentially, caps at rateLimiterMaxDelay rather than the
// controller-runtime default of 1000s, and resets on Forget. req is the
// workqueue item the limiter is driven with, which is the only thing that
// differs between the two instantiations.
func assertTunedRateLimiter[request comparable](
	t *testing.T,
	rl workqueue.TypedRateLimiter[request],
	req request,
) {
	t.Helper()
	g := gomega.NewWithT(t)

	g.Expect(rl).NotTo(gomega.BeNil())

	// The first failure retries after the base delay (the token bucket is
	// within burst, so the exponential limiter dominates the MaxOf).
	g.Expect(rl.When(req)).To(gomega.Equal(rateLimiterBaseDelay), "first requeue must use the base delay")

	// Second failure doubles the base delay.
	g.Expect(rl.When(req)).To(gomega.Equal(2*rateLimiterBaseDelay), "second requeue must double the base delay")

	// Drive the exponential limiter well past its cap and confirm it never
	// exceeds rateLimiterMaxDelay (the whole point of the tuning: the default
	// would keep climbing toward ~1000s).
	var last time.Duration
	for range 40 {
		last = rl.When(req)
		g.Expect(last).To(gomega.BeNumerically("<=", rateLimiterMaxDelay),
			"per-item backoff must never exceed the tuned cap")
	}
	g.Expect(last).To(gomega.Equal(rateLimiterMaxDelay), "backoff must saturate at the tuned cap")

	// Forget resets the exponential counter so a recovered CR retries fast again.
	rl.Forget(req)
	g.Expect(rl.When(req)).To(gomega.Equal(rateLimiterBaseDelay), "Forget must reset the backoff")
}

// singleClusterRequest is the workqueue item of a classic controller.
var singleClusterRequest = reconcile.Request{
	NamespacedName: types.NamespacedName{Namespace: "ns", Name: "cr"},
}

// multiClusterRequest is the same item as a multicluster controller sees it,
// carrying the cluster name that makes it a distinct key.
var multiClusterRequest = mcreconcile.Request{Request: singleClusterRequest, ClusterName: "target"}

// TestControllerOptions_ConcurrencyFallback verifies the worker count falls
// back to the shared default when unset (<= 0) and passes an explicit
// positive value through unchanged.
func TestControllerOptions_ConcurrencyFallback(t *testing.T) {
	assertConcurrencyFallback(t, ControllerOptions)
}

// TestControllerOptions_RateLimiter verifies the reconcile.Request
// instantiation carries the tuned failure limiter.
func TestControllerOptions_RateLimiter(t *testing.T) {
	assertTunedRateLimiter(t, ControllerOptions(0).RateLimiter, singleClusterRequest)
}

// TestTypedControllerOptions_ConcurrencyFallback verifies the generic variant
// applies the same worker-count fallback as ControllerOptions when
// instantiated over the multicluster request type.
func TestTypedControllerOptions_ConcurrencyFallback(t *testing.T) {
	assertConcurrencyFallback(t, TypedControllerOptions[mcreconcile.Request])
}

// TestTypedControllerOptions_RateLimiter verifies the generic variant carries
// the same tuned failure limiter when its items are cluster-scoped multicluster
// requests, whose extra field makes them a different comparable key.
func TestTypedControllerOptions_RateLimiter(t *testing.T) {
	assertTunedRateLimiter(t, TypedControllerOptions[mcreconcile.Request](0).RateLimiter, multiClusterRequest)
}
