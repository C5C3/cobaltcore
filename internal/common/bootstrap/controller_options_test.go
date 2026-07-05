// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"testing"
	"time"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestControllerOptions_ConcurrencyFallback verifies the worker count falls
// back to the shared default when unset (<= 0) and passes an explicit
// positive value through unchanged.
func TestControllerOptions_ConcurrencyFallback(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(ControllerOptions(0).MaxConcurrentReconciles).
		To(gomega.Equal(DefaultMaxConcurrentReconciles), "zero value must fall back to the default")
	g.Expect(ControllerOptions(-1).MaxConcurrentReconciles).
		To(gomega.Equal(DefaultMaxConcurrentReconciles), "negative value must fall back to the default")
	g.Expect(ControllerOptions(8).MaxConcurrentReconciles).
		To(gomega.Equal(8), "explicit positive value must pass through")
}

// TestControllerOptions_RateLimiter verifies the tuned failure limiter starts
// at the base delay, grows exponentially, caps at rateLimiterMaxDelay rather
// than the controller-runtime default of 1000s, and resets on Forget.
func TestControllerOptions_RateLimiter(t *testing.T) {
	g := gomega.NewWithT(t)

	rl := ControllerOptions(0).RateLimiter
	g.Expect(rl).NotTo(gomega.BeNil())
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "cr"}}

	// The first failure retries after the base delay (the token bucket is
	// within burst, so the exponential limiter dominates the MaxOf).
	g.Expect(rl.When(req)).To(gomega.Equal(rateLimiterBaseDelay), "first requeue must use the base delay")

	// Second failure doubles the base delay.
	g.Expect(rl.When(req)).To(gomega.Equal(2*rateLimiterBaseDelay), "second requeue must double the base delay")

	// Drive the exponential limiter well past its cap and confirm it never
	// exceeds rateLimiterMaxDelay (the whole point of the tuning: the default
	// would keep climbing toward ~1000s).
	var last time.Duration
	for i := 0; i < 40; i++ {
		last = rl.When(req)
		g.Expect(last).To(gomega.BeNumerically("<=", rateLimiterMaxDelay),
			"per-item backoff must never exceed the tuned cap")
	}
	g.Expect(last).To(gomega.Equal(rateLimiterMaxDelay), "backoff must saturate at the tuned cap")

	// Forget resets the exponential counter so a recovered CR retries fast again.
	rl.Forget(req)
	g.Expect(rl.When(req)).To(gomega.Equal(rateLimiterBaseDelay), "Forget must reset the backoff")
}
