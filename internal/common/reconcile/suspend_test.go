// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const suspendTestGeneration = 3

// suspendCalls counts the API calls GateSuspend makes: statusWrites counts
// status subresource updates, all counts every call of any kind.
type suspendCalls struct {
	statusWrites int
	all          int
}

// suspendTestClient builds a fake client around a Pod at generation 3 whose
// API calls are counted. A non-nil statusErr fails every status update.
func suspendTestClient(t *testing.T, statusErr error) (client.Client, *corev1.Pod, *suspendCalls) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "p", Namespace: "default", Generation: suspendTestGeneration,
	}}
	calls := &suspendCalls{}
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithObjects(pod).
		WithStatusSubresource(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				calls.all++
				return c.Get(ctx, key, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				calls.all++
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				calls.all++
				return c.Patch(ctx, obj, patch, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				calls.all++
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				calls.all++
				calls.statusWrites++
				if statusErr != nil {
					return statusErr
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	return c, pod, calls
}

func suspendedCondition(status metav1.ConditionStatus, generation int64, since time.Time) metav1.Condition {
	return metav1.Condition{
		Type:               ConditionTypeSuspended,
		Status:             status,
		ObservedGeneration: generation,
		Reason:             ReasonReconciliationSuspended,
		Message:            suspendedConditionMessage,
		LastTransitionTime: metav1.NewTime(since),
	}
}

func drainEvents(rec *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-rec.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

func TestGateSuspend_SuspendSetsConditionOnce(t *testing.T) {
	g := gomega.NewWithT(t)
	c, pod, calls := suspendTestClient(t, nil)
	rec := record.NewFakeRecorder(10)
	var conds []metav1.Condition

	stop, err := GateSuspend(context.Background(), c, rec, pod, &conds, true)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(stop).To(gomega.BeTrue())
	g.Expect(calls.statusWrites).To(gomega.Equal(1))
	cond := meta.FindStatusCondition(conds, ConditionTypeSuspended)
	g.Expect(cond).NotTo(gomega.BeNil())
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(gomega.Equal(ReasonReconciliationSuspended))
	g.Expect(cond.Message).To(gomega.Equal("Reconciliation is suspended by spec.suspend; the operator does not " +
		"create, update or delete anything for this resource until spec.suspend is cleared"))
	g.Expect(cond.ObservedGeneration).To(gomega.Equal(int64(suspendTestGeneration)))
	g.Expect(drainEvents(rec)).To(gomega.ConsistOf(
		"Normal ReconciliationSuspended Reconciliation suspended by spec.suspend"))
}

// A False Suspended condition (never written by the gate, but possible from
// another writer) is a transition too: it flips to True with one event.
func TestGateSuspend_SuspendFlipsFalseCondition(t *testing.T) {
	g := gomega.NewWithT(t)
	c, pod, calls := suspendTestClient(t, nil)
	rec := record.NewFakeRecorder(10)
	conds := []metav1.Condition{suspendedCondition(metav1.ConditionFalse, suspendTestGeneration, time.Now())}

	stop, err := GateSuspend(context.Background(), c, rec, pod, &conds, true)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(stop).To(gomega.BeTrue())
	g.Expect(calls.statusWrites).To(gomega.Equal(1))
	g.Expect(meta.IsStatusConditionTrue(conds, ConditionTypeSuspended)).To(gomega.BeTrue())
	g.Expect(drainEvents(rec)).To(gomega.HaveLen(1))
}

func TestGateSuspend_SteadySuspendWritesNothing(t *testing.T) {
	g := gomega.NewWithT(t)
	c, pod, calls := suspendTestClient(t, nil)
	rec := record.NewFakeRecorder(10)
	conds := []metav1.Condition{suspendedCondition(metav1.ConditionTrue, suspendTestGeneration, time.Now())}

	stop, err := GateSuspend(context.Background(), c, rec, pod, &conds, true)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(stop).To(gomega.BeTrue())
	g.Expect(calls.all).To(gomega.BeZero())
	g.Expect(drainEvents(rec)).To(gomega.BeEmpty())
}

// A spec change made while suspended bumps the generation; the condition
// follows it without a new transition or event.
func TestGateSuspend_SuspendFollowsNewGeneration(t *testing.T) {
	g := gomega.NewWithT(t)
	c, pod, calls := suspendTestClient(t, nil)
	rec := record.NewFakeRecorder(10)
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	conds := []metav1.Condition{suspendedCondition(metav1.ConditionTrue, suspendTestGeneration-1, since)}

	stop, err := GateSuspend(context.Background(), c, rec, pod, &conds, true)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(stop).To(gomega.BeTrue())
	g.Expect(calls.statusWrites).To(gomega.Equal(1))
	cond := meta.FindStatusCondition(conds, ConditionTypeSuspended)
	g.Expect(cond.ObservedGeneration).To(gomega.Equal(int64(suspendTestGeneration)))
	g.Expect(cond.LastTransitionTime.Time).To(gomega.BeTemporally("==", since))
	g.Expect(drainEvents(rec)).To(gomega.BeEmpty())
}

func TestGateSuspend_UnsuspendedWithoutConditionMakesNoCall(t *testing.T) {
	g := gomega.NewWithT(t)
	c, pod, calls := suspendTestClient(t, nil)
	rec := record.NewFakeRecorder(10)
	var conds []metav1.Condition

	stop, err := GateSuspend(context.Background(), c, rec, pod, &conds, false)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(stop).To(gomega.BeFalse())
	g.Expect(calls.all).To(gomega.BeZero())
	g.Expect(conds).To(gomega.BeNil())
	g.Expect(drainEvents(rec)).To(gomega.BeEmpty())
}

func TestGateSuspend_ResumeRemovesCondition(t *testing.T) {
	for _, status := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse} {
		t.Run(string(status), func(t *testing.T) {
			g := gomega.NewWithT(t)
			c, pod, calls := suspendTestClient(t, nil)
			rec := record.NewFakeRecorder(10)
			conds := []metav1.Condition{suspendedCondition(status, suspendTestGeneration, time.Now())}

			stop, err := GateSuspend(context.Background(), c, rec, pod, &conds, false)

			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(stop).To(gomega.BeFalse())
			g.Expect(calls.statusWrites).To(gomega.Equal(1))
			g.Expect(meta.FindStatusCondition(conds, ConditionTypeSuspended)).To(gomega.BeNil())
			g.Expect(drainEvents(rec)).To(gomega.ConsistOf(
				"Normal ReconciliationResumed Reconciliation resumed: spec.suspend is cleared"))
		})
	}
}

// A failed status write stops the pass with a wrapped error that keeps the
// API error class, and emits no event for the transition it did not persist.
func TestGateSuspend_StatusWriteFailureStopsWithoutEvent(t *testing.T) {
	cases := map[string]struct {
		suspended bool
		conds     []metav1.Condition
	}{
		"suspend": {suspended: true},
		"resume": {
			suspended: false,
			conds:     []metav1.Condition{suspendedCondition(metav1.ConditionTrue, suspendTestGeneration, time.Now())},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			conflict := apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "p", errors.New("stale"))
			c, pod, calls := suspendTestClient(t, conflict)
			rec := record.NewFakeRecorder(10)
			conds := tc.conds

			stop, err := GateSuspend(context.Background(), c, rec, pod, &conds, tc.suspended)

			g.Expect(stop).To(gomega.BeTrue())
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.HavePrefix("updating status for the Suspended condition:"))
			var statusErr *apierrors.StatusError
			g.Expect(errors.As(err, &statusErr)).To(gomega.BeTrue())
			g.Expect(apierrors.IsConflict(statusErr)).To(gomega.BeTrue())
			g.Expect(calls.statusWrites).To(gomega.Equal(1))
			g.Expect(drainEvents(rec)).To(gomega.BeEmpty())
		})
	}
}

func TestGateSuspend_NilRecorderStillWrites(t *testing.T) {
	cases := map[string]struct {
		suspended bool
		conds     []metav1.Condition
	}{
		"suspend": {suspended: true},
		"resume": {
			suspended: false,
			conds:     []metav1.Condition{suspendedCondition(metav1.ConditionTrue, suspendTestGeneration, time.Now())},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			c, pod, calls := suspendTestClient(t, nil)
			conds := tc.conds

			g.Expect(func() {
				_, err := GateSuspend(context.Background(), c, nil, pod, &conds, tc.suspended)
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}).NotTo(gomega.Panic())
			g.Expect(calls.statusWrites).To(gomega.Equal(1))
		})
	}
}

// The gate owns only the Suspended condition: Ready and every sub-condition
// keep their exact value across either transition.
func TestGateSuspend_LeavesOtherConditionsAlone(t *testing.T) {
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	others := []metav1.Condition{
		{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllReady", Message: "ok",
			ObservedGeneration: 1, LastTransitionTime: metav1.NewTime(since),
		},
		{
			Type: "SecretsReady", Status: metav1.ConditionFalse, Reason: "Waiting", Message: "no secret",
			ObservedGeneration: 1, LastTransitionTime: metav1.NewTime(since),
		},
	}
	cases := map[string]struct {
		suspended bool
		conds     []metav1.Condition
	}{
		"suspend": {suspended: true, conds: others},
		"resume": {
			suspended: false,
			conds:     append([]metav1.Condition{suspendedCondition(metav1.ConditionTrue, 1, since)}, others...),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			c, pod, _ := suspendTestClient(t, nil)
			conds := append([]metav1.Condition(nil), tc.conds...)

			_, err := GateSuspend(context.Background(), c, record.NewFakeRecorder(10), pod, &conds, tc.suspended)

			g.Expect(err).NotTo(gomega.HaveOccurred())
			for _, want := range others {
				g.Expect(meta.FindStatusCondition(conds, want.Type)).To(gomega.HaveValue(gomega.Equal(want)))
			}
		})
	}
}
