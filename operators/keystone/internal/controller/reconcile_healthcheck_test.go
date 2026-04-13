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

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0067

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

// trackingReadCloser wraps an io.ReadCloser and records whether Close was called.
type trackingReadCloser struct {
	io.ReadCloser
	closed bool
}

func (t *trackingReadCloser) Close() error {
	t.closed = true
	return t.ReadCloser.Close()
}

// --- httpClient() tests (REQ-006) ---

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

// --- Happy path: HTTP 2xx → True (REQ-001) ---

func TestReconcileHealthCheck_Healthy200_SetsConditionTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newHealthcheckTestReconciler()
	r.HTTPClient = srv.Client()
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

// --- Unhappy path: HTTP 500 → False (REQ-001) ---

func TestReconcileHealthCheck_Unhealthy500_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := newHealthcheckTestReconciler()
	r.HTTPClient = srv.Client()
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

// --- Unhappy path: HTTP 503 → False (REQ-001) ---

func TestReconcileHealthCheck_Unhealthy503_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	r := newHealthcheckTestReconciler()
	r.HTTPClient = srv.Client()
	ks := newTestKeystoneForHealthCheck(srv.URL+"/v3", 1)

	result, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))

	cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIUnhealthy))
}

// --- ObservedGeneration tracking (REQ-001) ---

func TestReconcileHealthCheck_ObservedGenerationSet(t *testing.T) {
	g := NewGomegaWithT(t)

	t.Run("healthy response", func(_ *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		r := newHealthcheckTestReconciler()
		r.HTTPClient = srv.Client()
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
		r.HTTPClient = srv.Client()
		ks := newTestKeystoneForHealthCheck(srv.URL+"/v3", 42)

		_, err := r.reconcileHealthCheck(context.Background(), ks)
		g.Expect(err).NotTo(HaveOccurred())

		cond := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneAPIReady)
		g.Expect(cond.ObservedGeneration).To(Equal(int64(42)))
	})
}

// --- Uses Status.Endpoint as target URL (REQ-004) ---

func TestReconcileHealthCheck_UsesStatusEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	var requestedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedURL = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newHealthcheckTestReconciler()
	r.HTTPClient = srv.Client()
	ks := newTestKeystoneForHealthCheck(srv.URL+"/v3", 1)

	_, err := r.reconcileHealthCheck(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(requestedURL).To(Equal("/v3"))
}

// --- Empty endpoint → EndpointNotReady (REQ-004) ---

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

// --- Timeout error → HealthCheckTimeout (REQ-002) ---

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

// --- Socket-level deadline → HealthCheckTimeout (REQ-002) ---

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

// --- Connection refused → ConnectionFailed (REQ-003) ---

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

// --- DNS error → EndpointNotReady (REQ-003) ---

func TestReconcileHealthCheck_DNSError_SetsConditionFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newHealthcheckTestReconciler()
	r.HTTPClient = &mockHTTPDoer{
		err: &url.Error{
			Op:  "Get",
			URL: "http://test/v3",
			Err: &net.DNSError{
				Err:  "no such host",
				Name: "test-keystone-api.default.svc.cluster.local",
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

// --- Generic network error → HealthCheckFailed (REQ-003) ---

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

// --- Response body close verification (REQ-008) ---

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
