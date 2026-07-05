// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"time"

	"golang.org/x/time/rate"
	"k8s.io/client-go/util/workqueue"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// DefaultMaxConcurrentReconciles is the default for the
// --max-concurrent-reconciles flag and the fallback ControllerOptions applies
// when the given value is unset (<= 0). The controller-runtime default of 1
// serialises reconciles across CRs; 2 lets a slow or flapping CR no longer
// block every other CR while keeping the extra worker footprint modest for a
// control-plane component.
const DefaultMaxConcurrentReconciles = 2

const (
	// rateLimiterBaseDelay is the initial per-item requeue delay of the
	// controllers' exponential failure rate limiter. It matches the
	// controller-runtime default so a first failure retries after 5ms.
	rateLimiterBaseDelay = 5 * time.Millisecond

	// rateLimiterMaxDelay caps the per-item exponential backoff. The
	// controller-runtime default of 1000s is far too conservative for an
	// I/O-bound operator; capping at 30s keeps a persistently failing CR
	// retrying on a bounded cadence. Genuinely slow external waits (DB,
	// bootstrap) do NOT ride this limiter — they use explicit RequeueAfter,
	// which enqueues via AddAfter and bypasses the failure backoff.
	rateLimiterMaxDelay = 30 * time.Second
)

// ControllerOptions builds the shared controller.Options every operator's
// SetupWithManager applies: MaxConcurrentReconciles lets independent CRs
// reconcile in parallel instead of serialising at the controller-runtime
// default of 1 (a value <= 0 falls back to DefaultMaxConcurrentReconciles so
// the zero value is safe for programmatically constructed reconcilers), and
// the tuned RateLimiter caps per-item failure backoff at 30s rather than the
// default 1000s.
func ControllerOptions(maxConcurrentReconciles int) crcontroller.Options {
	if maxConcurrentReconciles <= 0 {
		maxConcurrentReconciles = DefaultMaxConcurrentReconciles
	}
	return crcontroller.Options{
		MaxConcurrentReconciles: maxConcurrentReconciles,
		RateLimiter:             rateLimiter(),
	}
}

// rateLimiter builds the controllers' workqueue rate limiter. It is the same
// composition as workqueue.DefaultTypedControllerRateLimiter — a per-item
// exponential failure limiter maxed against a 10 qps / 100 burst token bucket
// — but with the per-item cap lowered from the controller-runtime default of
// 1000s to rateLimiterMaxDelay (30s). The 1000s default is far too
// conservative for an I/O-bound operator: a transiently failing CR would back
// off toward a ~16-minute retry. Lowering only the per-item cap keeps the
// overall 10 qps / 100-burst ceiling intact while bounding failure backoff to
// 30s.
func rateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](rateLimiterBaseDelay, rateLimiterMaxDelay),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}
