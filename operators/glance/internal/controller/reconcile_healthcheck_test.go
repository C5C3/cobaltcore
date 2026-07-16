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
	glance := testGlance()
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileHealthCheck(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueHealthCheck))
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeGlanceAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointNotReady))
}

func TestReconcileHealthCheck_Healthy(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Status.Endpoint = "http://test-glance.default.svc.cluster.local:9292/"
	stub := &stubDoer{status: http.StatusOK}
	r := newGlanceTestReconciler(glance)
	r.HTTPClient = stub

	res, err := r.reconcileHealthCheck(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// The probe targets the cluster-local /healthcheck, never the gateway URL.
	g.Expect(stub.lastURL).To(Equal("http://test-glance.default.svc.cluster.local:9292/healthcheck"))
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeGlanceAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIHealthy))
}

func TestReconcileHealthCheck_Non2xxUnhealthy(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Status.Endpoint = "http://test-glance.default.svc.cluster.local:9292/"
	r := newGlanceTestReconciler(glance)
	r.HTTPClient = &stubDoer{status: http.StatusInternalServerError}

	res, err := r.reconcileHealthCheck(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueHealthCheck))
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeGlanceAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonAPIUnhealthy))
	g.Expect(cond.Message).To(ContainSubstring("500"))
}

func TestReconcileHealthCheck_ConnectionErrorClassified(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Status.Endpoint = "http://test-glance.default.svc.cluster.local:9292/"
	r := newGlanceTestReconciler(glance)
	r.HTTPClient = &stubDoer{err: errors.New("dial tcp: connection refused")}

	res, err := r.reconcileHealthCheck(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueHealthCheck))
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeGlanceAPIReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(healthcheck.ReasonConnectionFailed))
}

func TestReconcileHealthCheck_CacheHitSkipsProbe(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Status.Endpoint = "http://test-glance.default.svc.cluster.local:9292/"
	stub := &stubDoer{status: http.StatusOK}
	r := newGlanceTestReconciler(glance)
	r.HTTPClient = stub

	// First pass probes and populates the cache.
	_, err := r.reconcileHealthCheck(context.Background(), glance)
	g.Expect(err).NotTo(HaveOccurred())
	stub.lastURL = ""

	// Second pass within the TTL serves from cache — no HTTP GET fired.
	_, err = r.reconcileHealthCheck(context.Background(), glance)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).To(BeEmpty(), "cache hit must not fire a probe")

	// Eviction forces the next pass to re-probe.
	r.evictHealthProbe(client.ObjectKeyFromObject(glance))
	_, err = r.reconcileHealthCheck(context.Background(), glance)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stub.lastURL).NotTo(BeEmpty(), "eviction must force a re-probe")
}
