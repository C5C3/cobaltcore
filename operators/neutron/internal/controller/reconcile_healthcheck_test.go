// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
)

// probeEndpoint is the cluster-local URL the health check probes for the shared
// Neutron fixture, independent of spec.gateway. "/" is the version document,
// which neutron-server answers with 200 without a token.
const probeEndpoint = "http://neutron.openstack.svc.cluster.local:9696/"

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
	neutron := validNeutron() // status.endpoint still empty
	r := newNeutronTestReconciler(neutron)

	res, err := r.reconcileHealthCheck(context.Background(), neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := neutronCondition(neutron, conditionTypeNeutronAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointNotReady))
}

func TestReconcileHealthCheck_Healthy(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	// A gateway endpoint is advertised externally; the probe must still target
	// the cluster-local Service URL.
	neutron.Spec.Gateway = neutronGatewaySpec()
	neutron.Status.Endpoint = "https://neutron.127-0-0-1.nip.io"
	stub := &stubDoer{status: http.StatusOK}
	r := newNeutronTestReconciler(neutron)
	r.HTTPClient = stub

	res, err := r.reconcileHealthCheck(context.Background(), neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(stub.lastURL).To(Equal(probeEndpoint))
	cond := neutronCondition(neutron, conditionTypeNeutronAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIHealthy))
}

func TestReconcileHealthCheck_Non2xxUnhealthy(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Status.Endpoint = probeEndpoint
	r := newNeutronTestReconciler(neutron)
	r.HTTPClient = &stubDoer{status: http.StatusServiceUnavailable}

	res, err := r.reconcileHealthCheck(context.Background(), neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := neutronCondition(neutron, conditionTypeNeutronAPIReady)
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
			neutron := validNeutron()
			neutron.Status.Endpoint = probeEndpoint
			r := newNeutronTestReconciler(neutron)
			r.HTTPClient = &stubDoer{err: tc.err}

			res, err := r.reconcileHealthCheck(context.Background(), neutron)

			g.Expect(err).NotTo(HaveOccurred(), "an unreachable API requeues rather than failing the pass")
			g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
			cond := neutronCondition(neutron, conditionTypeNeutronAPIReady)
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
	neutron := validNeutron()
	neutron.Status.Endpoint = probeEndpoint
	r := newNeutronTestReconciler(neutron)
	r.HTTPClient = &stubDoer{err: context.Canceled}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.reconcileHealthCheck(ctx, neutron)

	g.Expect(err).To(MatchError(context.Canceled))
	g.Expect(neutronCondition(neutron, conditionTypeNeutronAPIReady)).To(BeNil(),
		"a cancelled pass must not report on API health")
}

func TestReconcileHealthCheck_CacheHitSkipsProbe(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Status.Endpoint = probeEndpoint
	stub := &stubDoer{status: http.StatusOK}
	r := newNeutronTestReconciler(neutron)
	r.HTTPClient = stub

	// First pass probes and populates the cache.
	_, err := r.reconcileHealthCheck(context.Background(), neutron)
	g.Expect(err).NotTo(HaveOccurred())
	stub.lastURL = ""

	// Second pass within the TTL serves from cache, firing no HTTP GET.
	_, err = r.reconcileHealthCheck(context.Background(), neutron)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(BeEmpty(), "cache hit must not fire a probe")

	// Eviction forces the next pass to re-probe.
	r.healthProbeCache.Evict(client.ObjectKeyFromObject(neutron))
	_, err = r.reconcileHealthCheck(context.Background(), neutron)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(Equal(probeEndpoint), "eviction must force a re-probe")
}

// --- Placed CRs probe through the target API server's service proxy ---

// placedAPIServer stands in for the target cluster's API server. It records the
// path of every request the service proxy rewrote and answers each with the
// canned status and body.
func placedAPIServer(t *testing.T, status int, body string) (*httptest.Server, *[]string) {
	t.Helper()

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv, &paths
}

func TestReconcileHealthCheck_PlacedCR_ProbesThroughTheServiceProxy(t *testing.T) {
	g := NewGomegaWithT(t)
	srv, paths := placedAPIServer(t, http.StatusOK, "")

	neutron := validNeutron()
	neutron.Status.Endpoint = probeEndpoint
	neutron.Spec.TargetClusterRef = placeOn("remote-a")
	r := newNeutronTestReconciler(neutron)
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{Config: &rest.Config{Host: srv.URL}})

	res, err := r.reconcileHealthCheck(context.Background(), neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(*paths).To(ConsistOf("/api/v1/namespaces/openstack/services/http:neutron:9696/proxy/"),
		"a placed Neutron is probed through the target API server, not over Service DNS")

	cond := neutronCondition(neutron, conditionTypeNeutronAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIHealthy))
	g.Expect(cond.Message).To(ContainSubstring(probeEndpoint),
		"the condition keeps naming the Service URL, whoever executed the probe")
}

// TestReconcileHealthCheck_PlacedCR_UnbuildableTransportNamesTheCluster covers
// the one failure the probe returns as an error instead of reporting on the CR:
// the cluster resolves, but carries nothing to build a transport from. The error
// is what an operator gets to read, so it has to name the cluster; the CR's own
// condition still shows the last probe and says nothing about it.
func TestReconcileHealthCheck_PlacedCR_UnbuildableTransportNamesTheCluster(t *testing.T) {
	g := NewGomegaWithT(t)

	neutron := validNeutron()
	neutron.Status.Endpoint = probeEndpoint
	neutron.Spec.TargetClusterRef = placeOn("remote-a")
	r := newNeutronTestReconciler(neutron)
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{})

	_, err := r.reconcileHealthCheck(context.Background(), neutron)

	g.Expect(err).To(MatchError(ContainSubstring(
		`resolving the health-probe transport for target cluster "remote-a"`)))
	g.Expect(err).To(MatchError(ContainSubstring("the target cluster has no REST config")))
}

// TestReconcileHealthCheck_PlacedCR_ProxyFailureReachesTheCondition covers the
// two ways the proxy itself answers instead of the service: the Service has no
// endpoints, and the registered kubeconfig may not use services/proxy. Both
// carry their cause in the body, which the status code alone does not convey.
func TestReconcileHealthCheck_PlacedCR_ProxyFailureReachesTheCondition(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "no endpoints behind the Service",
			status: http.StatusServiceUnavailable,
			body:   `no endpoints available for service "neutron"`,
		},
		{
			name:   "the registered kubeconfig may not proxy",
			status: http.StatusForbidden,
			body: `services "neutron" is forbidden: User "system:serviceaccount:cobaltcore:neutron-operator" ` +
				`cannot get resource "services/proxy" in API group "" in the namespace "openstack"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			srv, paths := placedAPIServer(t, tc.status, tc.body)

			neutron := validNeutron()
			neutron.Status.Endpoint = probeEndpoint
			neutron.Spec.TargetClusterRef = placeOn("remote-a")
			r := newNeutronTestReconciler(neutron)
			r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{Config: &rest.Config{Host: srv.URL}})

			res, err := r.reconcileHealthCheck(context.Background(), neutron)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
			g.Expect(*paths).To(HaveLen(1))

			cond := neutronCondition(neutron, conditionTypeNeutronAPIReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonAPIUnhealthy))
			g.Expect(cond.Message).To(ContainSubstring(tc.body),
				"the proxy's own answer is the only place the cause is stated")
		})
	}
}
