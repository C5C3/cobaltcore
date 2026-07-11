// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/c5c3/forge/internal/common/conditions"
)

// mockDoer returns a fixed response or error and counts invocations.
type mockDoer struct {
	status int
	err    error
	calls  int
}

func (m *mockDoer) Do(*http.Request) (*http.Response, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return &http.Response{StatusCode: m.status, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func baseParams(conds *[]metav1.Condition, doer HTTPDoer, cache *ProbeCache) ProbeFlowParams {
	return ProbeFlowParams{
		Doer:               doer,
		Cache:              cache,
		Key:                types.NamespacedName{Namespace: "ns", Name: "cr"},
		UID:                "uid-1",
		Subject:            "Test API",
		EndpointConfigured: true,
		ProbeEndpoint:      "http://cr.ns.svc:5000/v3",
		Conditions:         conds,
		Generation:         3,
		ConditionType:      "TestAPIReady",
		HealthyReason:      "APIHealthy",
		UnhealthyReason:    "APIUnhealthy",
		Timeout:            2 * time.Second,
		CacheTTL:           30 * time.Second,
		RequeueAfter:       10 * time.Second,
	}
}

func TestReconcileProbe_EndpointNotConfigured(t *testing.T) {
	g := gomega.NewWithT(t)
	var conds []metav1.Condition
	p := baseParams(&conds, &mockDoer{status: 200}, &ProbeCache{})
	p.EndpointConfigured = false

	res, err := ReconcileProbe(context.Background(), p)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.RequeueAfter).To(gomega.Equal(p.RequeueAfter))
	cond := conditions.GetCondition(conds, p.ConditionType)
	g.Expect(cond).NotTo(gomega.BeNil())
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(gomega.Equal(ReasonEndpointNotReady))
	g.Expect(cond.Message).To(gomega.Equal("endpoint not yet configured"))
}

func TestReconcileProbe_Healthy(t *testing.T) {
	g := gomega.NewWithT(t)
	var conds []metav1.Condition
	doer := &mockDoer{status: 204}
	cache := &ProbeCache{}
	p := baseParams(&conds, doer, cache)

	res, err := ReconcileProbe(context.Background(), p)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.IsZero()).To(gomega.BeTrue())
	cond := conditions.GetCondition(conds, p.ConditionType)
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(gomega.Equal("APIHealthy"))
	g.Expect(cond.Message).To(gomega.Equal("Test API is responding at " + p.ProbeEndpoint))
	// A successful probe is memoized.
	g.Expect(cache.Has(p.Key)).To(gomega.BeTrue())
}

func TestReconcileProbe_NonSuccessEvicts(t *testing.T) {
	g := gomega.NewWithT(t)
	var conds []metav1.Condition
	cache := &ProbeCache{}
	cache.Store(types.NamespacedName{Namespace: "ns", Name: "cr"}, "uid-1", "http://cr.ns.svc:5000/v3")
	p := baseParams(&conds, &mockDoer{status: 503}, cache)

	res, err := ReconcileProbe(context.Background(), p)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.RequeueAfter).To(gomega.Equal(p.RequeueAfter))
	cond := conditions.GetCondition(conds, p.ConditionType)
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(gomega.Equal("APIUnhealthy"))
	g.Expect(cond.Message).To(gomega.Equal("Test API returned HTTP 503"))
	// A failed probe evicts so recovery is detected next reconcile.
	g.Expect(cache.Has(p.Key)).To(gomega.BeFalse())
}

func TestReconcileProbe_ErrorClassifiedAndEvicts(t *testing.T) {
	g := gomega.NewWithT(t)
	var conds []metav1.Condition
	cache := &ProbeCache{}
	cache.Store(types.NamespacedName{Namespace: "ns", Name: "cr"}, "uid-1", "http://cr.ns.svc:5000/v3")
	p := baseParams(&conds, &mockDoer{err: fmt.Errorf("dial tcp 10.0.0.1:5000: connection refused")}, cache)

	res, err := ReconcileProbe(context.Background(), p)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.RequeueAfter).To(gomega.Equal(p.RequeueAfter))
	cond := conditions.GetCondition(conds, p.ConditionType)
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(gomega.Equal(ReasonConnectionFailed))
	g.Expect(cache.Has(p.Key)).To(gomega.BeFalse())
}

func TestReconcileProbe_ParentCancelPropagatesWithoutFlip(t *testing.T) {
	g := gomega.NewWithT(t)
	// Seed a fresh, matching cache entry and a True condition — a cache hit
	// would normally serve without probing, so age it past the TTL to force a
	// probe, then cancel the parent context so the probe error is the
	// cancellation.
	clk := time.Unix(1_700_000_000, 0)
	cache := &ProbeCache{Now: func() time.Time { return clk }}
	key := types.NamespacedName{Namespace: "ns", Name: "cr"}
	cache.Store(key, "uid-1", "http://cr.ns.svc:5000/v3")
	clk = clk.Add(31 * time.Second)

	conds := []metav1.Condition{{
		Type:   "TestAPIReady",
		Status: metav1.ConditionTrue,
	}}
	p := baseParams(&conds, &mockDoer{err: context.Canceled}, cache)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ReconcileProbe(ctx, p)
	g.Expect(err).To(gomega.MatchError(context.Canceled))
	// The condition must not flip and the cache entry must not be evicted.
	cond := conditions.GetCondition(conds, p.ConditionType)
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue))
	g.Expect(cache.Has(key)).To(gomega.BeTrue())
}

func TestReconcileProbe_CacheHitSkipsProbe(t *testing.T) {
	g := gomega.NewWithT(t)
	clk := time.Unix(1_700_000_000, 0)
	cache := &ProbeCache{Now: func() time.Time { return clk }}
	key := types.NamespacedName{Namespace: "ns", Name: "cr"}
	cache.Store(key, "uid-1", "http://cr.ns.svc:5000/v3")

	conds := []metav1.Condition{{Type: "TestAPIReady", Status: metav1.ConditionTrue}}
	doer := &mockDoer{status: 200}
	p := baseParams(&conds, doer, cache)

	res, err := ReconcileProbe(context.Background(), p)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.IsZero()).To(gomega.BeTrue())
	g.Expect(doer.calls).To(gomega.Equal(0), "a fresh cache hit must not probe")
	cond := conditions.GetCondition(conds, p.ConditionType)
	g.Expect(cond.Reason).To(gomega.Equal("APIHealthy"))
}

func TestReconcileProbe_CacheMissWhenConditionNotTrue(t *testing.T) {
	g := gomega.NewWithT(t)
	cache := &ProbeCache{}
	cache.Store(types.NamespacedName{Namespace: "ns", Name: "cr"}, "uid-1", "http://cr.ns.svc:5000/v3")
	// Condition is unset, so the "already True" gate fails and a probe fires.
	var conds []metav1.Condition
	doer := &mockDoer{status: 200}
	p := baseParams(&conds, doer, cache)

	_, err := ReconcileProbe(context.Background(), p)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(doer.calls).To(gomega.Equal(1), "a non-True condition must force a probe even with a fresh cache entry")
}
