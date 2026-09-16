// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the sub-reconciler instrumentation wiring.
package controller

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/cobaltcore/internal/common/instrumentation"
	"github.com/c5c3/cobaltcore/operators/nova/internal/metrics"
)

// TestSubReconcilerConditionTypesCoversAllNames is one half of the drift guard:
// every condition_type value in subReconcilerConditionTypes must be a member of
// subConditionTypes, otherwise an addition to one list without the other will
// silently produce metrics with a stale condition_type label. The repo's
// condition-coverage audit keys on this exact test.
func TestSubReconcilerConditionTypesCoversAllNames(t *testing.T) {
	g := NewGomegaWithT(t)

	for name, condType := range subReconcilerConditionTypes {
		g.Expect(subConditionTypes).To(ContainElement(condType),
			"sub_reconciler %q maps to condition_type %q, which subConditionTypes does not "+
				"aggregate. update the list or fix the mapping", name, condType)
	}
}

// TestPipelineStepNamesAreMapped is the other half of the drift guard, walking
// the mapping in the opposite direction: every step name the pipeline actually
// runs must be a key in subReconcilerConditionTypes, otherwise its error series
// carries condition_type=UNKNOWN. The names come from the pipeline itself, so a
// step added to Reconcile without a mapping entry fails here rather than in a
// Prometheus query.
func TestPipelineStepNamesAreMapped(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &NovaReconciler{}
	state := &pipelineState{}

	var names []string
	add := func(name string) {
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	// The parallel-group step carries no name of its own by design: it
	// self-instruments its members (see commonreconcile.Step), which are collected
	// from parallelSteps below. The state both methods read is the outputs of
	// earlier steps, and none of them is read here: only the step names and the
	// members' condition types are.
	for _, step := range r.pipelineSteps(r.Client, validNova(), state) {
		add(step.Name)
	}
	for _, sub := range r.parallelSteps(r.Client, state) {
		add(sub.Name)
		g.Expect(subReconcilerConditionTypes[sub.Name]).To(Equal(sub.ConditionType),
			"parallel member %q reports condition %q but the metrics map says %q",
			sub.Name, sub.ConditionType, subReconcilerConditionTypes[sub.Name])
	}

	for _, name := range names {
		g.Expect(subReconcilerConditionTypes).To(HaveKey(name),
			"pipeline step %q has no subReconcilerConditionTypes entry, so its errors "+
				"would be attributed to condition_type=UNKNOWN", name)
	}

	g.Expect(names).To(HaveLen(len(subReconcilerConditionTypes)),
		"every mapped sub_reconciler must correspond to a pipeline step")
	for name := range subReconcilerConditionTypes {
		g.Expect(names).To(ContainElement(name),
			"subReconcilerConditionTypes maps sub_reconciler %q, which no Nova pipeline step runs", name)
	}
}

// TestPipelineStepOrder pins the dependency order the steps are declared in. The
// credential steps produce the digests and the transport URL the compute
// contract and the workloads read, Config renders the ConfigMap the database and
// every workload mount, and Database runs both migrations before a process that
// queries a schema is started. A reordering that compiles would only surface as a
// pod crash-looping on a schema that does not exist yet.
func TestPipelineStepOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &NovaReconciler{}
	state := &pipelineState{}

	var names []string
	for _, step := range r.pipelineSteps(r.Client, validNova(), state) {
		names = append(names, step.Name)
	}

	g.Expect(names).To(Equal([]string{
		"Secrets", "DBConnectionSecrets", "TransportURLSecret", "Config", "ComputeConfig",
		"Database", "Conductor", "Scheduler", "Metadata", "ConsoleProxy", "Deployment", "DBArchive",
		// The trailing empty name is the parallel group, which self-instruments its
		// members instead of reporting under one of its own.
		"",
	}))

	var members []string
	for _, sub := range r.parallelSteps(r.Client, state) {
		members = append(members, sub.Name)
	}
	g.Expect(members).To(Equal([]string{
		"HTTPRoute", "MetadataHTTPRoute", "ConsoleHTTPRoute", "HealthCheck", "HPA", "NetworkPolicy",
	}))
}

// TestSubReconcilerConditionTypes_UnknownNameMapsToNothing pins the fallback the
// instrumenter relies on. A step name absent from the map resolves to no
// condition type, which is what makes the helper stamp
// instrumentation.ConditionTypeUnknown instead of an empty label, so the drift
// shows up as UNKNOWN in an alert rather than as a series that silently merges
// with a legitimate one.
func TestSubReconcilerConditionTypes_UnknownNameMapsToNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	condType, ok := subReconcilerConditionTypes["NoSuchStep"]

	g.Expect(ok).To(BeFalse())
	g.Expect(condType).To(BeEmpty())
	g.Expect(instrumentation.ConditionTypeUnknown).NotTo(BeEmpty(),
		"the fallback label must be a readable value, not an empty string")
}

// registeredSeries returns the series of the named family on the
// controller-runtime registry whose label set is exactly want, or nil when the
// registry carries no such series.
func registeredSeries(t *testing.T, family string, want map[string]string) *dto.Metric {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering the metrics registry: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != family {
			continue
		}
		for _, metric := range fam.GetMetric() {
			got := map[string]string{}
			for _, pair := range metric.GetLabel() {
				got[pair.GetName()] = pair.GetValue()
			}
			if maps.Equal(got, want) {
				return metric
			}
		}
	}
	return nil
}

// TestRegisterMetrics_ExposesOperatorMetrics proves the startup registration
// main.go performs puts every operator metric family on the controller-runtime
// registry under the nova_operator prefix: the sub-reconciler pair and the per-CR
// job collectors. The dashboards and the alerts query exactly these names, and
// the dashboard test builds its own vectors, so a renamed prefix or a dropped
// registration would otherwise only show up as empty panels. A vector without
// children is not gathered, so one instrumented call and one recorded run of
// each job have to happen first.
func TestRegisterMetrics_ExposesOperatorMetrics(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(RegisterMetrics()).To(Succeed())

	const name = "HealthCheck"
	_, err := instrumenter.Instrument(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, errors.New("boom")
	})
	g.Expect(err).To(HaveOccurred())

	const probe = "register-metrics-probe"
	t.Cleanup(func() { metrics.DeleteForNova(probe, testNamespace) })
	metrics.RecordDBSync(probe, testNamespace, "succeeded", time.Second)
	metrics.RecordDBArchive(probe, testNamespace, "succeeded", time.Second)

	g.Expect(registeredSeries(t, "nova_operator_reconcile_duration_seconds",
		map[string]string{"sub_reconciler": name})).NotTo(BeNil())
	g.Expect(registeredSeries(t, "nova_operator_reconcile_errors_total",
		map[string]string{"sub_reconciler": name, "condition_type": "NovaAPIReady"})).NotTo(BeNil())
	for _, family := range []string{"nova_operator_db_sync_total", "nova_operator_db_archive_total"} {
		g.Expect(registeredSeries(t, family, map[string]string{
			"nova": probe, "namespace": testNamespace, "result": "succeeded",
		})).NotTo(BeNil(), "RegisterMetrics must expose %s", family)
	}
}
