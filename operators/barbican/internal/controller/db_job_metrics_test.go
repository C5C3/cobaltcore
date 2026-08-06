// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	barbicanmetrics "github.com/c5c3/forge/operators/barbican/internal/metrics"
)

// counterValueForLabels returns the value of the counter series in famName whose
// label set equals want, or -1 when no such series has been observed yet. A
// negative sentinel keeps "never recorded" distinguishable from a recorded zero.
func counterValueForLabels(t *testing.T, g prometheus.Gatherer, famName string, want map[string]string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != famName {
			continue
		}
		for _, m := range fam.GetMetric() {
			labels := m.GetLabel()
			if len(labels) != len(want) {
				continue
			}
			match := true
			for _, lp := range labels {
				if want[lp.GetName()] != lp.GetValue() {
					match = false
					break
				}
			}
			if match {
				return m.GetCounter().GetValue()
			}
		}
	}
	return -1
}

// TestRecordDBJobTerminalState_CountsAFailedRunOncePerUID pins the dedupe the
// bridge exists for: a failed db-sync Job sits there until someone fixes the
// migration, so every reconcile in between observes the same terminal state. The
// counter must not track the number of passes.
func TestRecordDBJobTerminalState_CountsAFailedRunOncePerUID(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(barbicanmetrics.Register()).To(Succeed())

	barbican := testBarbican()
	barbican.Name = "db-sync-failed-metric"
	t.Cleanup(func() { barbicanmetrics.DeleteForBarbican(barbican.Name, barbican.Namespace) })

	r := newBarbicanTestReconciler(barbican, terminatedSyncJob(barbican, batchv1.JobFailed, "sync-failed-uid"))
	labels := map[string]string{
		"barbican":  barbican.Name,
		"namespace": barbican.Namespace,
		"result":    "failed",
	}

	_, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)
	g.Expect(err).To(HaveOccurred())
	g.Expect(counterValueForLabels(t, ctrlmetrics.Registry, "barbican_operator_db_sync_total", labels)).
		To(Equal(1.0), "a failed Job must be counted as result=failed")

	// Same Job, same UID: a second pass must not count it again.
	_, err = r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)
	g.Expect(err).To(HaveOccurred())
	g.Expect(counterValueForLabels(t, ctrlmetrics.Registry, "barbican_operator_db_sync_total", labels)).
		To(Equal(1.0), "the terminal state MUST be recorded at most once per Job UID")
}
