// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/healthcheck"
)

// probeEndpoint is the cluster-local URL the health check probes for the shared
// test-barbican fixture, independent of spec.gateway. It is the healthcheck app
// the paste composite routes outside the authtoken pipeline.
const probeEndpoint = "http://test-barbican.openstack.svc.cluster.local:9311/healthcheck"

// stubDoer implements healthcheck.HTTPDoer, returning a canned response or error
// and recording the requested URL.
type stubDoer struct {
	status  int
	err     error
	lastURL string
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	s.lastURL = req.URL.String()
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

func TestReconcileHealthCheck_EndpointNotConfigured(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican() // status.endpoint still empty
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileHealthCheck(context.Background(), barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := barbicanCondition(barbican, conditionTypeBarbicanAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointNotReady))
}

func TestReconcileHealthCheck_Healthy(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	// A gateway endpoint is advertised externally; the probe must still target
	// the cluster-local Service URL.
	barbican.Spec.Gateway = barbicanGatewaySpec()
	barbican.Status.Endpoint = "https://barbican.127-0-0-1.nip.io"
	stub := &stubDoer{status: http.StatusOK}
	r := newBarbicanTestReconciler(barbican)
	r.HTTPClient = stub

	res, err := r.reconcileHealthCheck(context.Background(), barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(stub.lastURL).To(Equal(probeEndpoint))
	cond := barbicanCondition(barbican, conditionTypeBarbicanAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIHealthy))
}

func TestReconcileHealthCheck_Non2xxUnhealthy(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Status.Endpoint = probeEndpoint
	r := newBarbicanTestReconciler(barbican)
	r.HTTPClient = &stubDoer{status: http.StatusInternalServerError}

	res, err := r.reconcileHealthCheck(context.Background(), barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := barbicanCondition(barbican, conditionTypeBarbicanAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIUnhealthy))
	g.Expect(cond.Message).To(ContainSubstring("500"))
}

func TestReconcileHealthCheck_TransportErrorsClassified(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason string
	}{
		{
			name:   "connection refused",
			err:    errors.New("dial tcp: connection refused"),
			reason: healthcheck.ReasonConnectionFailed,
		},
		{
			// The probe's own deadline fires while the parent context stays live.
			name:   "probe timeout",
			err:    context.DeadlineExceeded,
			reason: healthcheck.ReasonHealthCheckTimeout,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			barbican := testBarbican()
			barbican.Status.Endpoint = probeEndpoint
			r := newBarbicanTestReconciler(barbican)
			r.HTTPClient = &stubDoer{err: tc.err}

			res, err := r.reconcileHealthCheck(context.Background(), barbican)

			g.Expect(err).NotTo(HaveOccurred(), "an unreachable API requeues rather than failing the pass")
			g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
			cond := barbicanCondition(barbican, conditionTypeBarbicanAPIReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.reason))
		})
	}
}

// A cancelled parent context means a peer in the parallel group failed and
// errgroup cancelled the shared context. The cancellation propagates instead of
// flipping the condition, so an unrelated failure cannot look like "API down".
func TestReconcileHealthCheck_CancelledContextPropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Status.Endpoint = probeEndpoint
	r := newBarbicanTestReconciler(barbican)
	r.HTTPClient = &stubDoer{err: context.Canceled}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.reconcileHealthCheck(ctx, barbican)

	g.Expect(err).To(MatchError(context.Canceled))
	g.Expect(barbicanCondition(barbican, conditionTypeBarbicanAPIReady)).To(BeNil(),
		"a cancelled pass must not report on API health")
}

func TestReconcileHealthCheck_CacheHitSkipsProbe(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Status.Endpoint = probeEndpoint
	stub := &stubDoer{status: http.StatusOK}
	r := newBarbicanTestReconciler(barbican)
	r.HTTPClient = stub

	// First pass probes and populates the cache.
	_, err := r.reconcileHealthCheck(context.Background(), barbican)
	g.Expect(err).NotTo(HaveOccurred())
	stub.lastURL = ""

	// Second pass within the TTL serves from cache, firing no HTTP GET.
	_, err = r.reconcileHealthCheck(context.Background(), barbican)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(BeEmpty(), "cache hit must not fire a probe")

	// Eviction forces the next pass to re-probe.
	r.healthProbeCache.Evict(client.ObjectKeyFromObject(barbican))
	_, err = r.reconcileHealthCheck(context.Background(), barbican)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(Equal(probeEndpoint), "eviction must force a re-probe")
}
