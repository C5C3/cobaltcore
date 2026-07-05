// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantReason string
	}{
		{"context deadline", context.DeadlineExceeded, ReasonHealthCheckTimeout},
		{"io deadline", os.ErrDeadlineExceeded, ReasonHealthCheckTimeout},
		{"wrapped deadline", fmt.Errorf("Get \"http://ks\": %w", context.DeadlineExceeded), ReasonHealthCheckTimeout},
		{"dns error", &net.DNSError{Err: "no such host", Name: "ks.svc"}, ReasonEndpointNotReady},
		{"connection refused", fmt.Errorf("dial tcp 10.0.0.1:5000: connection refused"), ReasonConnectionFailed},
		{"anything else", fmt.Errorf("unexpected EOF"), ReasonHealthCheckFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			reason, message := ClassifyError(tc.err)
			g.Expect(reason).To(gomega.Equal(tc.wantReason))
			g.Expect(message).NotTo(gomega.BeEmpty())
		})
	}
}

func TestProbeCache_TTLBoundary(t *testing.T) {
	g := gomega.NewWithT(t)

	base := time.Now()
	clk := base
	cache := &ProbeCache{Now: func() time.Time { return clk }}
	key := types.NamespacedName{Namespace: "ns", Name: "ks"}
	const ttl = 30 * time.Second

	// Miss on an empty cache.
	g.Expect(cache.Hit(key, "uid-1", "http://ks/v3", ttl)).To(gomega.BeFalse())

	cache.Store(key, "uid-1", "http://ks/v3")
	g.Expect(cache.Hit(key, "uid-1", "http://ks/v3", ttl)).To(gomega.BeTrue())

	// Exactly at the TTL boundary the entry is stale (strict <).
	clk = base.Add(ttl)
	g.Expect(cache.Hit(key, "uid-1", "http://ks/v3", ttl)).To(gomega.BeFalse())

	// Just inside the TTL it still hits.
	clk = base.Add(ttl - time.Nanosecond)
	g.Expect(cache.Hit(key, "uid-1", "http://ks/v3", ttl)).To(gomega.BeTrue())
}

// A CR recreated under the same name/namespace (new UID) or a changed target
// endpoint must never serve a stale probe.
func TestProbeCache_UIDAndEndpointGuards(t *testing.T) {
	g := gomega.NewWithT(t)

	cache := &ProbeCache{}
	key := types.NamespacedName{Namespace: "ns", Name: "ks"}
	cache.Store(key, "uid-1", "http://ks/v3")

	g.Expect(cache.Hit(key, "uid-2", "http://ks/v3", time.Minute)).To(gomega.BeFalse(),
		"a recreated CR (different UID) must not reuse the old probe")
	g.Expect(cache.Hit(key, "uid-1", "http://other/v3", time.Minute)).To(gomega.BeFalse(),
		"a changed endpoint must invalidate the entry")
}

func TestProbeCache_Evict(t *testing.T) {
	g := gomega.NewWithT(t)

	cache := &ProbeCache{}
	key := types.NamespacedName{Namespace: "ns", Name: "ks"}
	cache.Store(key, "uid-1", "http://ks/v3")
	g.Expect(cache.Has(key)).To(gomega.BeTrue())

	cache.Evict(key)
	g.Expect(cache.Has(key)).To(gomega.BeFalse())

	// Evicting an absent key is a no-op, not a panic.
	cache.Evict(types.NamespacedName{Namespace: "ns", Name: "absent"})
}
