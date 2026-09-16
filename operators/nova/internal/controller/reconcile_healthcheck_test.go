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
// Nova fixture, independent of the gateway blocks. The compute API ships no
// /healthcheck route, so the probe reads the version document off the root.
const probeEndpoint = "http://nova.openstack.svc.cluster.local:8774/"

// stubDoer implements healthcheck.HTTPDoer, returning a canned response or error
// and recording the requested URL.
type stubDoer struct {
	status  int
	body    string
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
		Body:       io.NopCloser(strings.NewReader(s.body)),
	}, nil
}

func TestNovaHealthCheckURL(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	g.Expect(novaHealthCheckURL(nova)).To(Equal(probeEndpoint))
	g.Expect(novaHealthCheckURL(nova)).To(HavePrefix(internalNovaURL(nova)),
		"the probe dials the Service, not the hostname status.endpoint advertises")
}

func TestReconcileHealthCheck_EndpointNotConfigured(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova() // status.endpoint still empty
	r := newNovaTestReconciler(nova)
	stub := &stubDoer{status: http.StatusOK}
	r.HTTPClient = stub

	res, err := r.reconcileHealthCheck(context.Background(), nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	g.Expect(stub.lastURL).To(BeEmpty(), "an API with no advertised endpoint is not probed")
	cond := novaCondition(nova, conditionTypeNovaAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointNotReady))
}

func TestReconcileHealthCheck_Healthy(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	// The gateway hostname is what status.endpoint advertises; the probe must
	// still target the cluster-local Service URL.
	nova.Status.Endpoint = "https://nova.example.com/"
	stub := &stubDoer{status: http.StatusOK}
	r := newNovaTestReconciler(nova)
	r.HTTPClient = stub

	res, err := r.reconcileHealthCheck(context.Background(), nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(stub.lastURL).To(Equal(probeEndpoint))
	cond := novaCondition(nova, conditionTypeNovaAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIHealthy))
}

// A failing API answers with a status code and, through a target cluster's
// service proxy, with a body naming the cause. Both belong in the condition.
func TestReconcileHealthCheck_Non2xxUnhealthy(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Status.Endpoint = probeEndpoint
	r := newNovaTestReconciler(nova)
	r.HTTPClient = &stubDoer{
		status: http.StatusInternalServerError,
		body:   "no endpoints available for service \"nova\"",
	}

	res, err := r.reconcileHealthCheck(context.Background(), nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := novaCondition(nova, conditionTypeNovaAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIUnhealthy))
	g.Expect(cond.Message).To(ContainSubstring("500"))
	g.Expect(cond.Message).To(ContainSubstring("no endpoints available for service"))
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
			nova := validNova()
			nova.Status.Endpoint = probeEndpoint
			r := newNovaTestReconciler(nova)
			r.HTTPClient = &stubDoer{err: tc.err}

			res, err := r.reconcileHealthCheck(context.Background(), nova)

			g.Expect(err).NotTo(HaveOccurred(), "an unreachable API requeues rather than failing the pass")
			g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
			cond := novaCondition(nova, conditionTypeNovaAPIReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.reason))
		})
	}
}

func TestReconcileHealthCheck_CacheHitSkipsProbe(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Status.Endpoint = probeEndpoint
	stub := &stubDoer{status: http.StatusOK}
	r := newNovaTestReconciler(nova)
	r.HTTPClient = stub

	// First pass probes and populates the cache.
	_, err := r.reconcileHealthCheck(context.Background(), nova)
	g.Expect(err).NotTo(HaveOccurred())
	stub.lastURL = ""

	// Second pass within the TTL serves from cache, firing no HTTP GET.
	_, err = r.reconcileHealthCheck(context.Background(), nova)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(BeEmpty(), "cache hit must not fire a probe")

	// Eviction forces the next pass to re-probe.
	r.healthProbeCache.Evict(client.ObjectKeyFromObject(nova))
	_, err = r.reconcileHealthCheck(context.Background(), nova)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(Equal(probeEndpoint), "eviction must force a re-probe")
}

// The one failure the probe returns as an error instead of reporting on the CR:
// the target cluster resolves, but carries nothing to build a transport from.
// The error is what an operator gets to read, so it has to name the cluster.
func TestReconcileHealthCheck_PlacedCR_UnbuildableTransportNamesTheCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Status.Endpoint = probeEndpoint
	nova.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "remote-a"}
	r := newNovaTestReconciler(nova)
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{})

	_, err := r.reconcileHealthCheck(context.Background(), nova)

	g.Expect(err).To(MatchError(ContainSubstring(
		`resolving the health-probe transport for target cluster "remote-a"`)))
	g.Expect(novaCondition(nova, conditionTypeNovaAPIReady)).To(BeNil(),
		"a pass that never probed reports nothing about API health")
}
