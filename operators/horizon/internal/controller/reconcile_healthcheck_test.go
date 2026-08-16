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

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/healthcheck"
	mctestutil "github.com/c5c3/forge/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// stubDoer implements HTTPDoer, returning a canned response or error and
// recording the requested URL.
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
	h := testHorizon()
	r := newTestReconciler(testScheme(), h)

	res, err := r.reconcileHealthCheck(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeHorizonAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointNotReady))
}

func TestReconcileHealthCheck_LoginPageHealthy(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Status.Endpoint = "http://test-horizon.default.svc.cluster.local:8080/"
	stub := &stubDoer{status: http.StatusOK}
	r := newTestReconciler(testScheme(), h)
	r.HTTPClient = stub

	res, err := r.reconcileHealthCheck(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// The probe targets the cluster-local login page, never the gateway URL.
	g.Expect(stub.lastURL).To(Equal("http://test-horizon.default.svc.cluster.local:8080/auth/login/"))
	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeHorizonAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIHealthy))
}

func TestReconcileHealthCheck_Non2xxUnhealthy(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Status.Endpoint = "http://test-horizon.default.svc.cluster.local:8080/"
	r := newTestReconciler(testScheme(), h)
	r.HTTPClient = &stubDoer{status: http.StatusInternalServerError}

	res, err := r.reconcileHealthCheck(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeHorizonAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIUnhealthy))
	g.Expect(cond.Message).To(ContainSubstring("500"))
}

func TestReconcileHealthCheck_ConnectionErrorClassified(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Status.Endpoint = "http://test-horizon.default.svc.cluster.local:8080/"
	r := newTestReconciler(testScheme(), h)
	r.HTTPClient = &stubDoer{err: errors.New("dial tcp: connection refused")}

	res, err := r.reconcileHealthCheck(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeHorizonAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(healthcheck.ReasonConnectionFailed))
}

func TestReconcileHealthCheck_CacheHitSkipsProbe(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Status.Endpoint = "http://test-horizon.default.svc.cluster.local:8080/"
	stub := &stubDoer{status: http.StatusOK}
	r := newTestReconciler(testScheme(), h)
	r.HTTPClient = stub

	// First pass probes and populates the cache.
	_, err := r.reconcileHealthCheck(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())
	stub.lastURL = ""

	// Second pass within the TTL serves from cache — no HTTP GET fired.
	_, err = r.reconcileHealthCheck(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(BeEmpty(), "cache hit must not fire a probe")
	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeHorizonAPIReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
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

	h := testHorizon()
	h.Status.Endpoint = "http://test-horizon.default.svc.cluster.local:8080/"
	h.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "remote-a"}
	r := newTestReconciler(testScheme(), h)
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{Config: &rest.Config{Host: srv.URL}})

	res, err := r.reconcileHealthCheck(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(*paths).To(ConsistOf("/api/v1/namespaces/default/services/http:test-horizon:8080/proxy/auth/login/"),
		"a placed Horizon is probed through the target API server, not over Service DNS")

	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeHorizonAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIHealthy))
	g.Expect(cond.Message).To(ContainSubstring(dashboardLoginURL(h)),
		"the condition keeps naming the Service URL, whoever executed the probe")
}

// TestReconcileHealthCheck_PlacedCR_UnbuildableTransportNamesTheCluster covers
// the one failure the probe returns as an error instead of reporting on the CR:
// the cluster resolves, but carries nothing to build a transport from. The error
// is what an operator gets to read, so it has to name the cluster — the CR's own
// condition still shows the last probe and says nothing about it.
func TestReconcileHealthCheck_PlacedCR_UnbuildableTransportNamesTheCluster(t *testing.T) {
	g := NewGomegaWithT(t)

	h := testHorizon()
	h.Status.Endpoint = "http://test-horizon.default.svc.cluster.local:8080/"
	h.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "remote-a"}
	r := newTestReconciler(testScheme(), h)
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{})

	_, err := r.reconcileHealthCheck(context.Background(), h)

	g.Expect(err).To(MatchError(ContainSubstring(
		`resolving the health-probe transport for target cluster "remote-a"`)))
	g.Expect(err).To(MatchError(ContainSubstring("the target cluster has no REST config")))
}

// TestReconcileHealthCheck_PlacedCR_ProxyFailureReachesTheCondition covers the
// two ways the proxy itself answers instead of the dashboard: the Service has
// no endpoints, and the registered kubeconfig may not use services/proxy. Both
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
			body:   `no endpoints available for service "test-horizon"`,
		},
		{
			name:   "the registered kubeconfig may not proxy",
			status: http.StatusForbidden,
			body: `services "test-horizon" is forbidden: User "system:serviceaccount:forge:horizon-operator" ` +
				`cannot get resource "services/proxy" in API group "" in the namespace "default"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			srv, paths := placedAPIServer(t, tc.status, tc.body)

			h := testHorizon()
			h.Status.Endpoint = "http://test-horizon.default.svc.cluster.local:8080/"
			h.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "remote-a"}
			r := newTestReconciler(testScheme(), h)
			r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{Config: &rest.Config{Host: srv.URL}})

			res, err := r.reconcileHealthCheck(context.Background(), h)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(healthcheck.RequeueHealthCheck))
			g.Expect(*paths).To(HaveLen(1))

			cond := conditions.GetCondition(h.Status.Conditions, conditionTypeHorizonAPIReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonAPIUnhealthy))
			g.Expect(cond.Message).To(ContainSubstring(tc.body),
				"the proxy's own answer is the only place the cause is stated")
		})
	}
}
