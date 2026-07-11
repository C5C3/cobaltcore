// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// scriptedDoer drives the health-probe cache tests. fn maps the 1-based call
// index to a response/error so a test can make the probe succeed, then fail,
// then succeed again across reconcile passes while counting invocations.
type scriptedDoer struct {
	calls int
	fn    func(call int) (*http.Response, error)
}

func (d *scriptedDoer) Do(_ *http.Request) (*http.Response, error) {
	d.calls++
	return d.fn(d.calls)
}

// ok200 returns a fresh HTTP 200 with an empty body on every call so repeated
// probes never reuse (and re-close) the same body.
func ok200(_ int) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// healthyReadyCondition sets KeystoneAPIReady=True on the CR so the cache-hit
// tests can isolate a single hit criterion (UID, endpoint, TTL) without first
// running a real probe.
func healthyReadyCondition(ks *keystonev1alpha1.Keystone) {
	conditions.SetCondition(&ks.Status.Conditions, metav1.Condition{
		Type:   conditionTypeKeystoneAPIReady,
		Status: metav1.ConditionTrue,
		Reason: conditionReasonAPIHealthy,
	})
}

func healthcheckTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return s
}

func newHealthcheckTestReconciler() *KeystoneReconciler {
	s := healthcheckTestScheme()
	cb := fake.NewClientBuilder().WithScheme(s)
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

func newTestKeystoneForHealthCheck(endpoint string, generation int64) *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			Generation: generation,
		},
		Status: keystonev1alpha1.KeystoneStatus{
			Endpoint: endpoint,
		},
	}
}

// mockHTTPDoer is a test double that returns a fixed response or error.
type mockHTTPDoer struct {
	resp *http.Response
	err  error
}

func (m *mockHTTPDoer) Do(_ *http.Request) (*http.Response, error) {
	return m.resp, m.err
}

// rewritingDoer forwards requests to a test server while preserving the
// caller-constructed URL path. Production code targets the cluster-local
// Service URL (internalAPIURL), which is unresolvable from unit tests; this
// wrapper transparently routes the request to an httptest server without
// changing what the caller built.
type rewritingDoer struct {
	inner  HTTPDoer
	target string
}

func (r *rewritingDoer) Do(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(r.target)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	req.Host = u.Host
	return r.inner.Do(req)
}

// capturingDoer records the URL of the last request it processed and delegates
// to an inner doer. Used to assert that the health check targets
// internalAPIURL regardless of Status.Endpoint.
type capturingDoer struct {
	inner HTTPDoer
	url   string
}

func (c *capturingDoer) Do(req *http.Request) (*http.Response, error) {
	c.url = req.URL.String()
	return c.inner.Do(req)
}

// trackingReadCloser wraps an io.ReadCloser and records whether Close was called.
type trackingReadCloser struct {
	io.ReadCloser
	closed bool
}

func (t *trackingReadCloser) Close() error {
	t.closed = true
	return t.ReadCloser.Close()
}

// --- httpClient() tests ---

func TestHttpClientReturnsInjectedClient(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newHealthcheckTestReconciler()

	custom := &http.Client{Timeout: 42}
	r.HTTPClient = custom

	g.Expect(r.httpClient()).To(BeIdenticalTo(custom))
}

func TestHttpClientReturnsDefaultClientWhenNil(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newHealthcheckTestReconciler()

	g.Expect(r.httpClient()).To(BeIdenticalTo(http.DefaultClient))
}

// --- Happy path: HTTP 2xx → True ---

func TestReconcileHealthCheck_Healthy200_SetsConditionTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newHealthcheckTestReconciler()
	r.HTTPClient = &rewritingDoer{inner: srv.Client(), target: srv.URL}
	ks := newTestKeystoneForHealthCheck(srv.URL+"/v3", 1)

	result, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIHealthy))
	g.Expect(cond.Message).To(ContainSubstring("Keystone API is responding at"))
}

// --- Unhappy path: HTTP 500 → False ---

func TestReconcileHealthCheck_Unhealthy500_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := newHealthcheckTestReconciler()
	r.HTTPClient = &rewritingDoer{inner: srv.Client(), target: srv.URL}
	ks := newTestKeystoneForHealthCheck(srv.URL+"/v3", 1)

	result, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))

	cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIUnhealthy))
	g.Expect(cond.Message).To(ContainSubstring("500"))
}

// --- Unhappy path: HTTP 503 → False ---

func TestReconcileHealthCheck_Unhealthy503_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	r := newHealthcheckTestReconciler()
	r.HTTPClient = &rewritingDoer{inner: srv.Client(), target: srv.URL}
	ks := newTestKeystoneForHealthCheck(srv.URL+"/v3", 1)

	result, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))

	cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIUnhealthy))
}

// TestReconcileHealthCheck_ConditionObservedGeneration verifies that
// ObservedGeneration is set on the KeystoneAPIReady condition for both
// the True (healthy) and False (unhealthy) paths with distinct
// generation values.
func TestReconcileHealthCheck_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)

	t.Run("healthy response", func(_ *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		r := newHealthcheckTestReconciler()
		r.HTTPClient = &rewritingDoer{inner: srv.Client(), target: srv.URL}
		ks := newTestKeystoneForHealthCheck(srv.URL+"/v3", 7)

		_, err := r.reconcileHealthCheck(context.Background(), ks)
		g.Expect(err).NotTo(HaveOccurred())

		cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
		g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))
	})

	t.Run("unhealthy response", func(_ *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		r := newHealthcheckTestReconciler()
		r.HTTPClient = &rewritingDoer{inner: srv.Client(), target: srv.URL}
		ks := newTestKeystoneForHealthCheck(srv.URL+"/v3", 42)

		_, err := r.reconcileHealthCheck(context.Background(), ks)
		g.Expect(err).NotTo(HaveOccurred())

		cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
		g.Expect(cond.ObservedGeneration).To(Equal(int64(42)))
	})
}

// --- Health check always targets the cluster-local internal URL,
// independent of Status.Endpoint and spec.gateway. ---

func TestReconcileHealthCheck_AlwaysTargetsInternalAPIURL_NoGateway(t *testing.T) {
	g := NewGomegaWithT(t)
	capture := &capturingDoer{inner: &mockHTTPDoer{resp: &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
	}}}

	r := newHealthcheckTestReconciler()
	r.HTTPClient = capture
	ks := newTestKeystoneForHealthCheck("https://public.example.com/v3", 1)

	_, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(capture.url).To(Equal("http://test-keystone.default.svc.cluster.local:5000/v3"),
		"health check must probe the cluster-local Service URL regardless of Status.Endpoint")
}

func TestReconcileHealthCheck_AlwaysTargetsInternalAPIURL_GatewaySet(t *testing.T) {
	g := NewGomegaWithT(t)
	capture := &capturingDoer{inner: &mockHTTPDoer{resp: &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
	}}}

	r := newHealthcheckTestReconciler()
	r.HTTPClient = capture
	ks := newTestKeystoneForHealthCheck("https://keystone.example.com/v3", 1)
	ks.Spec.Gateway = &keystonev1alpha1.GatewaySpec{
		ParentRef: keystonev1alpha1.GatewayParentRefSpec{Name: "public-gateway"},
		Hostname:  "keystone.example.com",
	}

	_, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(capture.url).To(Equal("http://test-keystone.default.svc.cluster.local:5000/v3"),
		"health check must not probe the public Gateway URL; conflating ingress/DNS/cert health with API readiness is a regression")
}

// --- Empty endpoint → EndpointNotReady ---

func TestReconcileHealthCheck_EmptyEndpoint_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newHealthcheckTestReconciler()
	ks := newTestKeystoneForHealthCheck("", 3)

	result, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))

	cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointNotReady))
	g.Expect(cond.Message).To(ContainSubstring("endpoint not yet configured"))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(3)))
}

// --- Timeout error → HealthCheckTimeout ---

func TestReconcileHealthCheck_Timeout_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newHealthcheckTestReconciler()
	r.HTTPClient = &mockHTTPDoer{
		err: &url.Error{
			Op:  "Get",
			URL: "http://test/v3",
			Err: context.DeadlineExceeded,
		},
	}
	ks := newTestKeystoneForHealthCheck("http://test/v3", 5)

	result, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))

	cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHealthCheckTimeout))
	g.Expect(cond.Message).To(ContainSubstring("health check timed out"))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(5)))
}

// --- Socket-level deadline → HealthCheckTimeout ---

func TestReconcileHealthCheck_OSDeadlineExceeded_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newHealthcheckTestReconciler()
	r.HTTPClient = &mockHTTPDoer{
		err: &url.Error{
			Op:  "Get",
			URL: "http://test/v3",
			Err: os.ErrDeadlineExceeded,
		},
	}
	ks := newTestKeystoneForHealthCheck("http://test/v3", 5)

	result, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))

	cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHealthCheckTimeout))
	g.Expect(cond.Message).To(ContainSubstring("health check timed out"))
}

// --- Connection refused → ConnectionFailed ---

func TestReconcileHealthCheck_ConnectionRefused_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newHealthcheckTestReconciler()
	r.HTTPClient = &mockHTTPDoer{
		err: &url.Error{
			Op:  "Get",
			URL: "http://test/v3",
			Err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: errors.New("connection refused"),
			},
		},
	}
	ks := newTestKeystoneForHealthCheck("http://test/v3", 4)

	result, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))

	cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonConnectionFailed))
	g.Expect(cond.Message).To(ContainSubstring("connection refused"))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(4)))
}

// --- DNS error → EndpointNotReady ---

func TestReconcileHealthCheck_DNSError_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newHealthcheckTestReconciler()
	r.HTTPClient = &mockHTTPDoer{
		err: &url.Error{
			Op:  "Get",
			URL: "http://test/v3",
			Err: &net.DNSError{
				Err:  "no such host",
				Name: "test-keystone.default.svc.cluster.local",
			},
		},
	}
	ks := newTestKeystoneForHealthCheck("http://test/v3", 6)

	result, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))

	cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointNotReady))
	g.Expect(cond.Message).To(ContainSubstring("endpoint not resolvable"))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(6)))
}

// --- Generic network error → HealthCheckFailed ---

func TestReconcileHealthCheck_GenericNetworkError_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newHealthcheckTestReconciler()
	r.HTTPClient = &mockHTTPDoer{
		err: fmt.Errorf("unexpected network failure"),
	}
	ks := newTestKeystoneForHealthCheck("http://test/v3", 2)

	result, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))

	cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHealthCheckFailed))
	g.Expect(cond.Message).To(ContainSubstring("unexpected network failure"))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(2)))
}

// --- Peer cancellation in the parallel group must not flip KeystoneAPIReady ---

// TestReconcileHealthCheck_ParentContextCancelled_PropagatesWithoutFlipping
// reproduces issue #361: the health probe runs inside the parallel
// post-deployment group under an errgroup context. When a peer (e.g. Bootstrap
// returning ErrJobFailed) fails, errgroup cancels the group context and the
// in-flight probe's Do returns context.Canceled. That is not an API-health
// signal — the reconciler must propagate the cancellation and leave both the
// KeystoneAPIReady condition and the probe cache untouched.
func TestReconcileHealthCheck_ParentContextCancelled_PropagatesWithoutFlipping(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newHealthcheckTestReconciler()
	r.HTTPClient = &mockHTTPDoer{
		err: &url.Error{Op: "Get", URL: "http://test/v3", Err: context.Canceled},
	}
	clk := time.Unix(1_700_000_000, 0)
	r.healthProbeCache.Now = func() time.Time { return clk }
	ks := newTestKeystoneForHealthCheck("http://ignored/v3", 9)
	ks.UID = "uid-peer-cancel"
	healthyReadyCondition(ks)

	// Seed a cache entry, then age it past the TTL so this pass actually
	// re-probes (a fresh entry would serve from cache and never hit the network).
	r.healthProbeCache.Store(client.ObjectKeyFromObject(ks), ks.UID, internalAPIURL(ks))
	key := client.ObjectKeyFromObject(ks)
	clk = clk.Add(HealthCheckCacheTTL + time.Second)

	// A cancelled parent context stands in for the errgroup cancelling gctx after
	// a peer sub-reconciler failed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := r.reconcileHealthCheck(ctx, ks)
	g.Expect(err).To(MatchError(context.Canceled),
		"a cancelled parent context must propagate, not be reclassified as an API-health failure")
	g.Expect(result).To(Equal(ctrl.Result{}))

	cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"peer cancellation must not flip KeystoneAPIReady to False")
	g.Expect(r.healthProbeCache.Has(key)).To(BeTrue(),
		"peer cancellation must not evict the probe cache")
}

// --- Response body close verification ---

func TestReconcileHealthCheck_ResponseBodyClosed_Success(t *testing.T) {
	g := NewGomegaWithT(t)
	tracker := &trackingReadCloser{ReadCloser: io.NopCloser(strings.NewReader(""))}
	r := newHealthcheckTestReconciler()
	r.HTTPClient = &mockHTTPDoer{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       tracker,
		},
	}
	ks := newTestKeystoneForHealthCheck("http://test/v3", 1)

	_, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(tracker.closed).To(BeTrue(), "response body should be closed after successful health check")
}

func TestReconcileHealthCheck_ResponseBodyClosed_Error(t *testing.T) {
	g := NewGomegaWithT(t)
	tracker := &trackingReadCloser{ReadCloser: io.NopCloser(strings.NewReader(""))}
	r := newHealthcheckTestReconciler()
	r.HTTPClient = &mockHTTPDoer{
		resp: &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       tracker,
		},
	}
	ks := newTestKeystoneForHealthCheck("http://test/v3", 1)

	_, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(tracker.closed).To(BeTrue(), "response body should be closed after failed health check")
}

// --- Probe cache (A2) ---

// TestReconcileHealthCheck_CacheHit_SkipsProbe verifies that a second reconcile
// within the TTL, with KeystoneAPIReady already True, does not fire another
// HTTP GET.
func TestReconcileHealthCheck_CacheHit_SkipsProbe(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := &scriptedDoer{fn: ok200}
	r := newHealthcheckTestReconciler()
	r.HTTPClient = doer
	base := time.Unix(1_700_000_000, 0)
	r.healthProbeCache.Now = func() time.Time { return base }
	ks := newTestKeystoneForHealthCheck("http://ignored/v3", 1)
	ks.UID = "uid-cache-hit"

	_, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(doer.calls).To(Equal(1), "first pass must probe once")
	g.Expect(conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady).Status).
		To(Equal(metav1.ConditionTrue))

	_, err = r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(doer.calls).To(Equal(1), "cache hit within the TTL must not re-probe")
	g.Expect(conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady).Status).
		To(Equal(metav1.ConditionTrue))
}

// TestReconcileHealthCheck_CacheExpiry_ReProbes verifies the probe fires again
// once the cached entry ages past HealthCheckCacheTTL.
func TestReconcileHealthCheck_CacheExpiry_ReProbes(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := &scriptedDoer{fn: ok200}
	r := newHealthcheckTestReconciler()
	r.HTTPClient = doer
	clk := time.Unix(1_700_000_000, 0)
	r.healthProbeCache.Now = func() time.Time { return clk }
	ks := newTestKeystoneForHealthCheck("http://ignored/v3", 1)
	ks.UID = "uid-expiry"

	_, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(doer.calls).To(Equal(1))

	clk = clk.Add(HealthCheckCacheTTL + time.Second)

	_, err = r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(doer.calls).To(Equal(2), "an expired cache entry must trigger a fresh probe")
}

// TestReconcileHealthCheck_ProbeError_EvictsCache verifies that a probe error
// after a cached success drops the cache entry, so a stale success is not
// served on the following pass.
func TestReconcileHealthCheck_ProbeError_EvictsCache(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := &scriptedDoer{fn: func(call int) (*http.Response, error) {
		if call == 1 {
			return ok200(call)
		}
		return nil, &url.Error{Op: "Get", URL: "http://test/v3", Err: errors.New("connection refused")}
	}}
	r := newHealthcheckTestReconciler()
	r.HTTPClient = doer
	clk := time.Unix(1_700_000_000, 0)
	r.healthProbeCache.Now = func() time.Time { return clk }
	ks := newTestKeystoneForHealthCheck("http://ignored/v3", 1)
	ks.UID = "uid-evict"
	key := client.ObjectKeyFromObject(ks)

	_, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.healthProbeCache.Has(key)).To(BeTrue(), "successful probe must be cached")

	// Age past the TTL so the next pass re-probes and hits the error branch.
	clk = clk.Add(HealthCheckCacheTTL + time.Second)
	result, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))
	g.Expect(doer.calls).To(Equal(2))
	g.Expect(r.healthProbeCache.Has(key)).To(BeFalse(), "a probe error must evict the cached entry")
}

// TestReconcileHealthCheck_ConditionNotTrue_ReProbes verifies that a fresh
// cache entry does not short-circuit the probe when KeystoneAPIReady is not
// already True.
func TestReconcileHealthCheck_ConditionNotTrue_ReProbes(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := &scriptedDoer{fn: ok200}
	r := newHealthcheckTestReconciler()
	r.HTTPClient = doer
	base := time.Unix(1_700_000_000, 0)
	r.healthProbeCache.Now = func() time.Time { return base }
	ks := newTestKeystoneForHealthCheck("http://ignored/v3", 1)
	ks.UID = "uid-cond-false"

	// Seed a fresh, matching cache entry but leave the condition unset so the
	// hit criterion "condition already True" fails.
	r.healthProbeCache.Store(client.ObjectKeyFromObject(ks), ks.UID, internalAPIURL(ks))

	_, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(doer.calls).To(Equal(1), "a non-True condition must force a probe even with a fresh cache entry")
}

// TestReconcileHealthCheck_EndpointChange_ReProbes verifies a cached entry for a
// different endpoint does not satisfy the current pass.
func TestReconcileHealthCheck_EndpointChange_ReProbes(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := &scriptedDoer{fn: ok200}
	r := newHealthcheckTestReconciler()
	r.HTTPClient = doer
	base := time.Unix(1_700_000_000, 0)
	r.healthProbeCache.Now = func() time.Time { return base }
	ks := newTestKeystoneForHealthCheck("http://ignored/v3", 1)
	ks.UID = "uid-endpoint"
	healthyReadyCondition(ks)

	// Seed a fresh entry for a stale endpoint; the reconcile targets
	// internalAPIURL, so the endpoints differ and the entry must be a miss.
	r.healthProbeCache.Store(client.ObjectKeyFromObject(ks), ks.UID, "http://stale.endpoint/v3")

	_, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(doer.calls).To(Equal(1), "an endpoint mismatch must force a probe")
}

// TestReconcileHealthCheck_UIDChange_ReProbes verifies a cached entry whose UID
// no longer matches (CR recreated under the same name) is a miss.
func TestReconcileHealthCheck_UIDChange_ReProbes(t *testing.T) {
	g := NewGomegaWithT(t)
	doer := &scriptedDoer{fn: ok200}
	r := newHealthcheckTestReconciler()
	r.HTTPClient = doer
	base := time.Unix(1_700_000_000, 0)
	r.healthProbeCache.Now = func() time.Time { return base }
	ks := newTestKeystoneForHealthCheck("http://ignored/v3", 1)
	ks.UID = "uid-old"
	healthyReadyCondition(ks)
	r.healthProbeCache.Store(client.ObjectKeyFromObject(ks), ks.UID, internalAPIURL(ks))

	// A CR recreated under the same name/namespace carries a new UID.
	ks.UID = "uid-new"
	_, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(doer.calls).To(Equal(1), "a UID mismatch must force a probe")
}
