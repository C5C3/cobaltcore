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

	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// probeEndpoint is the cluster-local URL the health check probes for the shared
// Cinder fixture, independent of spec.gateway. The oslo healthcheck middleware
// answers it without a token and without touching the database.
const probeEndpoint = "http://cinder.openstack.svc.cluster.local:8776/healthcheck"

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

func TestCinderHealthCheckURL(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(cinderHealthCheckURL(validCinder())).To(Equal(probeEndpoint))
}

func TestReconcileHealthCheck_EndpointNotConfigured(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder() // status.endpoint still empty
	r := newCinderTestReconciler(cinder)
	r.HTTPClient = &stubDoer{status: http.StatusOK}

	res, err := r.reconcileHealthCheck(context.Background(), cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := cinderCondition(cinder, conditionTypeCinderAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointNotReady))
}

func TestReconcileHealthCheck_Healthy(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	// A gateway endpoint is advertised externally; the probe must still target
	// the cluster-local Service URL.
	cinder.Spec.Gateway = cinderGatewaySpec()
	cinder.Status.Endpoint = "https://cinder.127-0-0-1.nip.io/"
	stub := &stubDoer{status: http.StatusOK}
	r := newCinderTestReconciler(cinder)
	r.HTTPClient = stub

	res, err := r.reconcileHealthCheck(context.Background(), cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(stub.lastURL).To(Equal(probeEndpoint))
	cond := cinderCondition(cinder, conditionTypeCinderAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIHealthy))
}

func TestReconcileHealthCheck_Non2xxUnhealthy(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Status.Endpoint = probeEndpoint
	r := newCinderTestReconciler(cinder)
	r.HTTPClient = &stubDoer{status: http.StatusServiceUnavailable}

	res, err := r.reconcileHealthCheck(context.Background(), cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := cinderCondition(cinder, conditionTypeCinderAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIUnhealthy))
	g.Expect(cond.Message).To(ContainSubstring("503"))
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
			cinder := validCinder()
			cinder.Status.Endpoint = probeEndpoint
			r := newCinderTestReconciler(cinder)
			r.HTTPClient = &stubDoer{err: tc.err}

			res, err := r.reconcileHealthCheck(context.Background(), cinder)

			g.Expect(err).NotTo(HaveOccurred(), "an unreachable API requeues rather than failing the pass")
			g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
			cond := cinderCondition(cinder, conditionTypeCinderAPIReady)
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
	cinder := validCinder()
	cinder.Status.Endpoint = probeEndpoint
	r := newCinderTestReconciler(cinder)
	r.HTTPClient = &stubDoer{err: context.Canceled}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.reconcileHealthCheck(ctx, cinder)

	g.Expect(err).To(MatchError(context.Canceled))
	g.Expect(cinderCondition(cinder, conditionTypeCinderAPIReady)).To(BeNil(),
		"a cancelled pass must not report on API health")
}

func TestReconcileHealthCheck_CacheHitSkipsProbe(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Status.Endpoint = probeEndpoint
	stub := &stubDoer{status: http.StatusOK}
	r := newCinderTestReconciler(cinder)
	r.HTTPClient = stub

	// First pass probes and populates the cache.
	_, err := r.reconcileHealthCheck(context.Background(), cinder)
	g.Expect(err).NotTo(HaveOccurred())
	stub.lastURL = ""

	// Second pass within the TTL serves from cache, firing no HTTP GET.
	_, err = r.reconcileHealthCheck(context.Background(), cinder)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(BeEmpty(), "cache hit must not fire a probe")

	// Eviction forces the next pass to re-probe.
	r.healthProbeCache.Evict(client.ObjectKeyFromObject(cinder))
	_, err = r.reconcileHealthCheck(context.Background(), cinder)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(Equal(probeEndpoint), "eviction must force a re-probe")
}

// The one failure the probe returns as an error instead of reporting on the CR:
// the target cluster resolves, but carries nothing to build a transport from.
// The error is what an operator gets to read, so it has to name the cluster.
func TestReconcileHealthCheck_PlacedCR_UnbuildableTransportNamesTheCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Status.Endpoint = probeEndpoint
	cinder.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "remote-a"}
	r := newCinderTestReconciler(cinder)
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{})

	_, err := r.reconcileHealthCheck(context.Background(), cinder)

	g.Expect(err).To(MatchError(ContainSubstring(
		`resolving the health-probe transport for target cluster "remote-a"`)))
	g.Expect(cinderCondition(cinder, conditionTypeCinderAPIReady)).To(BeNil(),
		"a pass that never probed reports nothing about API health")
}
