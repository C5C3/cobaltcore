// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

// FuzzGlanceQuotaMiB checks that GlanceQuotaMiB never panics on a parsed
// Quantity and that every size it admits is a whole number of MiB between 1 and
// glanceQuotaMaxMiB that equals the Quantity exactly.
func FuzzGlanceQuotaMiB(f *testing.F) {
	for _, raw := range []string{
		// The inputs of TestValidateGlanceQuotas.
		"1Mi", "2147483647Mi", "50Gi", "1048576", "1536Ki", "0", "512Ki", "-1Mi",
		"2147483648Mi", "1e2000000000", "1.5",
		// A negative exponent past the scale bound, a decimal suffix that is a
		// whole number of MiB, and a fraction that cancels to one.
		"1e-30", "1048576000m", "1.048576M",
	} {
		f.Add(raw)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		q, err := resource.ParseQuantity(raw)
		if err != nil {
			return
		}
		mib, err := GlanceQuotaMiB(q)
		if err != nil {
			return
		}
		if mib < 1 || int64(mib) > glanceQuotaMaxMiB {
			t.Fatalf("GlanceQuotaMiB(%s) = %d, want 1 to %d", raw, mib, glanceQuotaMaxMiB)
		}
		if want := resource.NewQuantity(int64(mib)*glanceQuotaMiBBytes, resource.BinarySI); want.Cmp(q) != 0 {
			t.Fatalf("GlanceQuotaMiB(%s) = %d, which is %s, not %s", raw, mib, want, q.String())
		}
	})
}
