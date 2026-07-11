// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package healthcheck provides the HTTP health-check core shared by the
// service operators: the HTTPDoer client seam, the probe-error classifier
// mapping transport failures to condition reasons, and the TTL probe cache
// that spares a steady-state reconcile the synchronous HTTP GET.
package healthcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// HTTPDoer abstracts the Do method of *http.Client so that tests can inject a
// stub transport for the API health check.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Condition reason constants ClassifyError maps probe errors to. Shared so
// every operator's API-ready condition uses the same failure vocabulary.
const (
	ReasonEndpointNotReady   = "EndpointNotReady"
	ReasonHealthCheckTimeout = "HealthCheckTimeout"
	ReasonConnectionFailed   = "ConnectionFailed"
	ReasonHealthCheckFailed  = "HealthCheckFailed"
)

// Timing constants shared by every operator's HTTP health-check flow.
const (
	// RequeueHealthCheck is the interval for requeuing when the service API
	// health check fails. The API may take a few seconds to start responding
	// after the Deployment reports ready, so a moderate interval is
	// appropriate.
	RequeueHealthCheck = 10 * time.Second

	// HealthCheckTimeout is the bounded timeout for the HTTP health check
	// request. Prevents a hanging API from blocking the reconcile loop
	// indefinitely.
	HealthCheckTimeout = 10 * time.Second

	// HealthCheckCacheTTL bounds how long a successful API probe is reused
	// before the operator re-probes. Every reconcile pass otherwise fires a
	// synchronous HTTP GET (bounded by HealthCheckTimeout, up to 10s on a
	// flapping API), which dominates hot-path latency once a CR is Ready.
	// Caching for 30s suppresses re-probes during event/resync bursts.
	//
	// Trade-off: a wedged-but-Ready API — one whose pods still pass their
	// readiness probe (so DeploymentReady stays True) while requests hang — is
	// masked for up to HealthCheckCacheTTL after the last good probe. Within
	// that window reconciles serve the API-ready condition True from cache
	// without probing, so failure-detection latency for this case is increased
	// by up to HealthCheckCacheTTL. The probe-error/non-2xx eviction does NOT
	// bound this: eviction fires only once a probe runs, and inside the TTL the
	// cache is exactly what suppresses that probe. Keep this TTL at or below the
	// API-ready outage-detection SLO.
	HealthCheckCacheTTL = 30 * time.Second
)

// ClassifyError returns the condition Reason and Message for the given HTTP
// client error.
func ClassifyError(err error) (reason, message string) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return ReasonHealthCheckTimeout, "health check timed out"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ReasonEndpointNotReady, "endpoint not resolvable"
	}

	if strings.Contains(err.Error(), "connection refused") {
		return ReasonConnectionFailed, fmt.Sprintf("connection failed: %s", err)
	}

	return ReasonHealthCheckFailed, fmt.Sprintf("health check failed: %s", err)
}

// probeEntry records the last successful API probe for one CR. uid guards
// against a CR recreated under the same name/namespace serving a stale probe;
// endpoint invalidates the entry when the target Service URL changes;
// probedAt drives the TTL comparison.
type probeEntry struct {
	uid      types.UID
	endpoint string
	probedAt time.Time
}

// ProbeCache memoizes the last successful health probe per CR so a
// steady-state reconcile does not fire a synchronous HTTP GET on every pass.
// The zero value is ready to use; the internal mutex guards concurrent access
// under MaxConcurrentReconciles > 1, so a ProbeCache must not be copied after
// first use.
type ProbeCache struct {
	mu      sync.Mutex
	entries map[types.NamespacedName]probeEntry

	// Now is the clock used for the TTL comparison. When nil it defaults to
	// time.Now; tests inject a controllable clock so the TTL boundary is
	// deterministic.
	Now func() time.Time
}

func (c *ProbeCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Hit reports whether the cached probe for the CR at key can be reused in
// place of a fresh HTTP GET: a stored entry matches the CR's UID and endpoint
// and is still within ttl. Callers layer their own condition-state gate on
// top (only reuse a probe while the API-ready condition is already True).
func (c *ProbeCache) Hit(key types.NamespacedName, uid types.UID, endpoint string, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return false
	}
	return entry.uid == uid &&
		entry.endpoint == endpoint &&
		c.now().Sub(entry.probedAt) < ttl
}

// Store records a successful probe so reconciles within the TTL can skip the
// synchronous HTTP GET.
func (c *ProbeCache) Store(key types.NamespacedName, uid types.UID, endpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[types.NamespacedName]probeEntry)
	}
	c.entries[key] = probeEntry{uid: uid, endpoint: endpoint, probedAt: c.now()}
}

// Evict drops the cached probe for a CR so the next reconcile re-probes.
// Called on any probe failure and on CR deletion.
func (c *ProbeCache) Evict(key types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Has reports whether an entry is cached for key, regardless of TTL. Intended
// for test assertions.
func (c *ProbeCache) Has(key types.NamespacedName) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[key]
	return ok
}
