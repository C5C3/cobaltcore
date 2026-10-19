// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	. "github.com/onsi/gomega"
)

// TestMapGophercloudError pins how gophercloud's response-code errors map onto
// the sentinels callers branch on: every mapped code, a wrapped code, an
// unmapped code that stays reachable through errors.As, a non-HTTP error, and
// nil.
func TestMapGophercloudError(t *testing.T) {
	const op = "GET mapping m"

	tests := []struct {
		name string
		in   error
		// sentinel is the sentinel the result matches; nil means none does.
		sentinel error
		prefix   string
		check    func(g Gomega, err error)
	}{
		{
			name:     "404 maps to ErrNotFound",
			in:       gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusNotFound},
			sentinel: ErrNotFound,
			prefix:   "not found: " + op,
		},
		{
			name:     "409 maps to ErrConflict",
			in:       gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusConflict},
			sentinel: ErrConflict,
			prefix:   "conflict: " + op,
		},
		{
			name:     "401 maps to ErrUnauthorized",
			in:       gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusUnauthorized},
			sentinel: ErrUnauthorized,
			prefix:   "unauthorized: " + op,
		},
		{
			name:     "403 maps to ErrForbidden",
			in:       gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusForbidden},
			sentinel: ErrForbidden,
			prefix:   "forbidden: " + op,
		},
		{
			name:   "unmapped 503 keeps the response-code error",
			in:     gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusServiceUnavailable},
			prefix: op + ": ",
			check: func(g Gomega, err error) {
				var codeErr gophercloud.ErrUnexpectedResponseCode
				g.Expect(errors.As(err, &codeErr)).To(BeTrue())
				g.Expect(codeErr.Actual).To(Equal(http.StatusServiceUnavailable))
			},
		},
		{
			name:     "wrapped 404 maps to ErrNotFound",
			in:       fmt.Errorf("wrapped: %w", gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusNotFound}),
			sentinel: ErrNotFound,
			prefix:   "not found: " + op,
		},
		{
			name:   "non-HTTP error is wrapped with the operation",
			in:     context.DeadlineExceeded,
			prefix: op + ": ",
			check: func(g Gomega, err error) {
				g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue())
				g.Expect(err.Error()).To(Equal(op + ": context deadline exceeded"))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			err := mapGophercloudError(tc.in, op)
			g.Expect(err).To(HaveOccurred())
			if tc.sentinel != nil {
				g.Expect(err).To(MatchError(tc.sentinel))
			} else {
				for _, s := range Sentinels {
					g.Expect(err).NotTo(MatchError(s))
				}
			}
			g.Expect(err.Error()).To(HavePrefix(tc.prefix))
			if tc.check != nil {
				tc.check(g, err)
			}
		})
	}

	t.Run("nil stays nil", func(t *testing.T) {
		NewGomegaWithT(t).Expect(mapGophercloudError(nil, op)).To(Succeed())
	})
}
