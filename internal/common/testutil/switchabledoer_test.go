// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/onsi/gomega"

	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
)

var _ healthcheck.HTTPDoer = (*SwitchableDoer)(nil)

func TestSwitchableDoer_AnswersWithTheLastStatusAndCountsCalls(t *testing.T) {
	g := gomega.NewWithT(t)
	doer := NewSwitchableDoer(http.StatusOK)
	g.Expect(doer.Calls()).To(gomega.BeZero(), "a new doer has answered nothing")

	get := func() (int, string) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://api.ns.svc:5000/", nil)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		resp, err := doer.Do(req)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return resp.StatusCode, string(body)
	}

	status, body := get()
	g.Expect(status).To(gomega.Equal(http.StatusOK))
	g.Expect(body).To(gomega.BeEmpty())
	g.Expect(doer.Calls()).To(gomega.Equal(int64(1)))

	doer.SetStatus(http.StatusServiceUnavailable)
	status, body = get()
	g.Expect(status).To(gomega.Equal(http.StatusServiceUnavailable))
	g.Expect(body).To(gomega.BeEmpty())
	g.Expect(doer.Calls()).To(gomega.Equal(int64(2)))
}
