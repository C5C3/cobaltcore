// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// terminalSyncJob returns a db-sync Job with the given terminal condition and a
// stable UID, which is what the emission is deduped on.
func terminalSyncJob(uid string, conditionType batchv1.JobConditionType) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testCinderName + "-db-sync",
			Namespace: testNamespace,
			UID:       types.UID(uid),
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{Type: conditionType, Status: corev1.ConditionTrue}},
		},
	}
}

// TestRecordDBJobTerminalState covers the emission gate: a terminal Job is
// recorded once per UID, and a Job that has not terminated yet is not recorded
// at all — the counter represents terminal transitions, so an in-progress Job
// counted early could never be corrected.
func TestRecordDBJobTerminalState(t *testing.T) {
	cases := []struct {
		name     string
		observed *batchv1.Job
		stamped  bool
	}{
		{name: "a succeeded Job", observed: terminalSyncJob("succeeded-uid", batchv1.JobComplete), stamped: true},
		{name: "a failed Job", observed: terminalSyncJob("failed-uid", batchv1.JobFailed), stamped: true},
		{
			name:     "a Job still running",
			observed: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "cinder-db-sync", UID: "running-uid"}},
		},
		{name: "no Job at all", observed: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cinder := validCinder()
			r := newCinderTestReconciler(cinder)

			r.recordDBJobTerminalState(context.Background(), cinder, "db-sync", tc.observed)

			if !tc.stamped {
				g.Expect(cinder.Annotations).NotTo(HaveKey(dbJobUIDAnnotationKey("db-sync")))
				return
			}
			g.Expect(cinder.Annotations).To(HaveKeyWithValue(
				dbJobUIDAnnotationKey("db-sync"), string(tc.observed.UID)))
		})
	}

	// Each phase keeps its own annotation, so recording one never suppresses
	// another's.
	t.Run("the phases dedupe independently", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := validCinder()
		r := newCinderTestReconciler(cinder)

		expand := terminalSyncJob("expand-uid", batchv1.JobComplete)
		r.recordDBJobTerminalState(context.Background(), cinder, upgradeExpandJobSuffix, expand)
		r.recordDBJobTerminalState(context.Background(), cinder, upgradeContractJobSuffix,
			terminalSyncJob("contract-uid", batchv1.JobComplete))

		g.Expect(cinder.Annotations).To(HaveKeyWithValue(
			dbJobUIDAnnotationKey(upgradeExpandJobSuffix), "expand-uid"))
		g.Expect(cinder.Annotations).To(HaveKeyWithValue(
			dbJobUIDAnnotationKey(upgradeContractJobSuffix), "contract-uid"))
	})
}

// TestRecordDBJobTerminalState_PatchFailureDefers covers the transient apiserver
// failure: the dedupe annotation is what makes the emission at-most-once, so a
// pass that cannot persist it must not record either. The degradation is
// surfaced as an event rather than as a reconcile error, because the metric is
// observability and the Job it describes has already finished.
func TestRecordDBJobTerminalState_PatchFailureDefers(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	c := cinderFakeClientBuilder(cinder).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object,
				patch client.Patch, opts ...client.PatchOption,
			) error {
				return apierrors.NewInternalError(errors.New("etcd is unavailable"))
			},
		}).Build()
	r := &CinderReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	r.recordDBJobTerminalState(context.Background(), cinder, "db-sync",
		terminalSyncJob("deferred-uid", batchv1.JobComplete))

	g.Expect(cinder.Annotations).NotTo(HaveKey(dbJobUIDAnnotationKey("db-sync")),
		"an unpersisted UID must leave the emission for the next pass")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ConsistOf(
		ContainSubstring("Warning DBSyncMetricEmissionDeferred")))
}
