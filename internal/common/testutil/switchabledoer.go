// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
)

// SwitchableDoer stands in for a service API whose health a test switches
// while the reconciler runs. Do answers every request with an empty body and
// the status last set, and counts its calls. The status and the count are
// atomics because the reconciler goroutine reads them while the test writes.
type SwitchableDoer struct {
	status atomic.Int32
	calls  atomic.Int64
}

// NewSwitchableDoer returns a SwitchableDoer that answers with status.
func NewSwitchableDoer(status int) *SwitchableDoer {
	d := &SwitchableDoer{}
	d.SetStatus(status)
	return d
}

// SetStatus sets the status code every later call answers with.
func (d *SwitchableDoer) SetStatus(status int) {
	d.status.Store(int32(status)) //nolint:gosec // G115: an HTTP status code fits in an int32.
}

// Calls reports how many requests Do has answered.
func (d *SwitchableDoer) Calls() int64 {
	return d.calls.Load()
}

// Do answers req with the status last set and an empty body.
func (d *SwitchableDoer) Do(*http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return &http.Response{StatusCode: int(d.status.Load()), Body: http.NoBody}, nil
}

// reprobeSlack covers envtest scheduling and the status write on top of the
// re-probe and recovery bounds ExpectReprobeTracksAPI waits for.
const reprobeSlack = 15 * time.Second

// ExpectReprobeTracksAPI proves on a Ready CR that the requeue at the
// probe-cache expiry carries the API's health into the conditions. It switches
// doer to 503 and writes no object, so only that requeue can turn apiCondType
// False/APIUnhealthy and Ready False/NotAllReady; it waits
// HealthCheckCacheTTL + HealthCheckTimeout for that. It then switches doer back
// to 200 and expects both True again within RequeueHealthCheck. conditions
// reads the CR and returns its status conditions.
func ExpectReprobeTracksAPI(t testing.TB, doer *SwitchableDoer,
	conditions func(g gomega.Gomega) []metav1.Condition, apiCondType string, poll time.Duration,
) {
	t.Helper()
	g := gomega.NewWithT(t)

	doer.SetStatus(http.StatusServiceUnavailable)
	g.Eventually(func(g gomega.Gomega) {
		conds := conditions(g)
		api := meta.FindStatusCondition(conds, apiCondType)
		g.Expect(api).NotTo(gomega.BeNil())
		g.Expect(api.Status).To(gomega.Equal(metav1.ConditionFalse))
		g.Expect(api.Reason).To(gomega.Equal("APIUnhealthy"))
		ready := meta.FindStatusCondition(conds, "Ready")
		g.Expect(ready).NotTo(gomega.BeNil())
		g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionFalse))
		g.Expect(ready.Reason).To(gomega.Equal("NotAllReady"))
	}, healthcheck.HealthCheckCacheTTL+healthcheck.HealthCheckTimeout+reprobeSlack, poll).Should(gomega.Succeed(),
		"a wedged API should turn %s and Ready False without an event", apiCondType)

	doer.SetStatus(http.StatusOK)
	g.Eventually(func(g gomega.Gomega) {
		conds := conditions(g)
		g.Expect(meta.IsStatusConditionTrue(conds, apiCondType)).To(gomega.BeTrue())
		g.Expect(meta.IsStatusConditionTrue(conds, "Ready")).To(gomega.BeTrue())
	}, healthcheck.RequeueHealthCheck+reprobeSlack, poll).Should(gomega.Succeed(),
		"a recovered API should turn %s and Ready True again", apiCondType)
}
