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

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/healthcheck"
)

// internalEndpoint is the cluster-local URL the health check probes for the
// shared test-placement fixture, independent of spec.gateway.
const internalEndpoint = "http://test-placement.default.svc.cluster.local:8778/"

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
	placement := testPlacement() // status.endpoint still empty
	r := newPlacementTestReconciler(placement)

	res, err := r.reconcileHealthCheck(context.Background(), placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := conditions.GetCondition(placement.Status.Conditions, conditionTypePlacementAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointNotReady))
}

func TestReconcileHealthCheck_Healthy(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	// A gateway endpoint is advertised externally; the probe must still target
	// the cluster-local Service URL.
	placement.Spec.Gateway = placementGatewaySpec()
	placement.Status.Endpoint = "https://placement.127-0-0-1.nip.io/"
	stub := &stubDoer{status: http.StatusOK}
	r := newPlacementTestReconciler(placement)
	r.HTTPClient = stub

	res, err := r.reconcileHealthCheck(context.Background(), placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(stub.lastURL).To(Equal(internalEndpoint))
	cond := conditions.GetCondition(placement.Status.Conditions, conditionTypePlacementAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIHealthy))
}

func TestReconcileHealthCheck_Non2xxUnhealthy(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Status.Endpoint = internalEndpoint
	r := newPlacementTestReconciler(placement)
	r.HTTPClient = &stubDoer{status: http.StatusInternalServerError}

	res, err := r.reconcileHealthCheck(context.Background(), placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := conditions.GetCondition(placement.Status.Conditions, conditionTypePlacementAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIUnhealthy))
	g.Expect(cond.Message).To(ContainSubstring("500"))
}

func TestReconcileHealthCheck_ConnectionErrorClassified(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Status.Endpoint = internalEndpoint
	r := newPlacementTestReconciler(placement)
	r.HTTPClient = &stubDoer{err: errors.New("dial tcp: connection refused")}

	res, err := r.reconcileHealthCheck(context.Background(), placement)

	g.Expect(err).NotTo(HaveOccurred(), "a refused connection requeues rather than failing the pass")
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := conditions.GetCondition(placement.Status.Conditions, conditionTypePlacementAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(healthcheck.ReasonConnectionFailed))
}

func TestReconcileHealthCheck_TimeoutClassified(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Status.Endpoint = internalEndpoint
	r := newPlacementTestReconciler(placement)
	// The probe's own deadline fires while the parent context stays live.
	r.HTTPClient = &stubDoer{err: context.DeadlineExceeded}

	res, err := r.reconcileHealthCheck(context.Background(), placement)

	g.Expect(err).NotTo(HaveOccurred(), "a probe timeout requeues rather than failing the pass")
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := conditions.GetCondition(placement.Status.Conditions, conditionTypePlacementAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(healthcheck.ReasonHealthCheckTimeout))
}

// A cancelled parent context means a peer in the parallel group failed and
// errgroup cancelled the shared context. The cancellation propagates instead of
// flipping the condition, so an unrelated failure cannot look like "API down".
func TestReconcileHealthCheck_CancelledContextPropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Status.Endpoint = internalEndpoint
	r := newPlacementTestReconciler(placement)
	r.HTTPClient = &stubDoer{err: context.Canceled}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.reconcileHealthCheck(ctx, placement)

	g.Expect(err).To(MatchError(context.Canceled))
	g.Expect(conditions.GetCondition(placement.Status.Conditions, conditionTypePlacementAPIReady)).To(BeNil(),
		"a cancelled pass must not report on API health")
}

func TestReconcileHealthCheck_CacheHitSkipsProbe(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Status.Endpoint = internalEndpoint
	stub := &stubDoer{status: http.StatusOK}
	r := newPlacementTestReconciler(placement)
	r.HTTPClient = stub

	// First pass probes and populates the cache.
	_, err := r.reconcileHealthCheck(context.Background(), placement)
	g.Expect(err).NotTo(HaveOccurred())
	stub.lastURL = ""

	// Second pass within the 30s TTL serves from cache, firing no HTTP GET.
	_, err = r.reconcileHealthCheck(context.Background(), placement)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(BeEmpty(), "cache hit must not fire a probe")

	// Eviction forces the next pass to re-probe.
	r.healthProbeCache.Evict(client.ObjectKeyFromObject(placement))
	_, err = r.reconcileHealthCheck(context.Background(), placement)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(Equal(internalEndpoint), "eviction must force a re-probe")
}
