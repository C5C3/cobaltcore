// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package assertions

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Feature: CC-0002

// mockT captures whether t.Errorf or t.Fatalf was called.
type mockT struct {
	testing.TB
	failed  bool
	message string
}

func (m *mockT) Helper() {}

func (m *mockT) Errorf(format string, args ...interface{}) {
	m.failed = true
	m.message = fmt.Sprintf(format, args...)
}

func (m *mockT) Fatalf(format string, args ...interface{}) {
	m.failed = true
	m.message = fmt.Sprintf(format, args...)
}

func sampleConditions() []metav1.Condition {
	return []metav1.Condition{
		{
			Type:   "Ready",
			Status: metav1.ConditionTrue,
			Reason: "AllGood",
		},
		{
			Type:   "Degraded",
			Status: metav1.ConditionFalse,
			Reason: "NoProblem",
		},
	}
}

func TestAssertCondition_succeeds(t *testing.T) {
	g := NewGomegaWithT(t)
	mt := &mockT{}
	AssertCondition(mt, sampleConditions(), "Ready", metav1.ConditionTrue)

	g.Expect(mt.failed).To(BeFalse(), "expected no failure, but got: %s", mt.message)
}

func TestAssertCondition_failsWhenTypeMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	mt := &mockT{}
	AssertCondition(mt, sampleConditions(), "Unknown", metav1.ConditionTrue)

	g.Expect(mt.failed).To(BeTrue(), "expected failure for missing condition type")
}

func TestAssertCondition_failsWhenWrongStatus(t *testing.T) {
	g := NewGomegaWithT(t)
	mt := &mockT{}
	AssertCondition(mt, sampleConditions(), "Ready", metav1.ConditionFalse)

	g.Expect(mt.failed).To(BeTrue(), "expected failure for wrong condition status")
}

func TestAssertConditionWithReason_succeeds(t *testing.T) {
	g := NewGomegaWithT(t)
	mt := &mockT{}
	AssertConditionWithReason(mt, sampleConditions(), "Ready", metav1.ConditionTrue, "AllGood")

	g.Expect(mt.failed).To(BeFalse(), "expected no failure, but got: %s", mt.message)
}

func TestAssertConditionWithReason_failsWhenReasonMismatch(t *testing.T) {
	g := NewGomegaWithT(t)
	mt := &mockT{}
	AssertConditionWithReason(mt, sampleConditions(), "Ready", metav1.ConditionTrue, "WrongReason")

	g.Expect(mt.failed).To(BeTrue(), "expected failure for mismatched reason")
}

func TestAssertConditionMissing_succeeds(t *testing.T) {
	g := NewGomegaWithT(t)
	mt := &mockT{}
	AssertConditionMissing(mt, sampleConditions(), "Unknown")

	g.Expect(mt.failed).To(BeFalse(), "expected no failure, but got: %s", mt.message)
}

func TestAssertConditionMissing_failsWhenPresent(t *testing.T) {
	g := NewGomegaWithT(t)
	mt := &mockT{}
	AssertConditionMissing(mt, sampleConditions(), "Ready")

	g.Expect(mt.failed).To(BeTrue(), "expected failure when condition type exists")
}

func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

func TestAssertResourceExists_succeeds(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
	}
	c := newFakeClient(secret)
	mt := &mockT{}

	AssertResourceExists(mt, context.Background(), c, client.ObjectKeyFromObject(secret), &corev1.Secret{})

	g.Expect(mt.failed).To(BeFalse(), "expected no failure, but got: %s", mt.message)
}

func TestAssertResourceExists_failsWhenMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	c := newFakeClient()
	mt := &mockT{}

	AssertResourceExists(mt, context.Background(), c, client.ObjectKey{Name: "missing", Namespace: "default"}, &corev1.Secret{})

	g.Expect(mt.failed).To(BeTrue(), "expected failure for missing resource")
}

func TestAssertResourceNotExists_succeeds(t *testing.T) {
	g := NewGomegaWithT(t)
	c := newFakeClient()
	mt := &mockT{}

	AssertResourceNotExists(mt, context.Background(), c, client.ObjectKey{Name: "missing", Namespace: "default"}, &corev1.Secret{})

	g.Expect(mt.failed).To(BeFalse(), "expected no failure, but got: %s", mt.message)
}

func TestAssertResourceNotExists_failsWhenExists(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "exists",
			Namespace: "default",
		},
	}
	c := newFakeClient(secret)
	mt := &mockT{}

	AssertResourceNotExists(mt, context.Background(), c, client.ObjectKeyFromObject(secret), &corev1.Secret{})

	g.Expect(mt.failed).To(BeTrue(), "expected failure when resource exists")
}

// threadSafeConditions provides a thread-safe container for conditions used in
// EventuallyCondition tests where a goroutine updates conditions concurrently.
type threadSafeConditions struct {
	mu         sync.Mutex
	conditions []metav1.Condition
}

func (c *threadSafeConditions) get() []metav1.Condition {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]metav1.Condition, len(c.conditions))
	copy(result, c.conditions)
	return result
}

func (c *threadSafeConditions) set(conditions []metav1.Condition) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conditions = conditions
}

func TestEventuallyCondition_succeeds(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
	}
	c := newFakeClient(secret)
	mt := &mockT{}

	extractor := func(_ client.Object) []metav1.Condition {
		return []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue},
		}
	}

	EventuallyCondition(mt, context.Background(), ConditionPollOpts{
		Client:         c,
		Key:            client.ObjectKeyFromObject(secret),
		Obj:            &corev1.Secret{},
		Extractor:      extractor,
		ConditionType:  "Ready",
		ExpectedStatus: metav1.ConditionTrue,
		Timeout:        time.Second,
		Interval:       20 * time.Millisecond,
	})

	g.Expect(mt.failed).To(BeFalse(), "expected no failure, but got: %s", mt.message)
}

func TestEventuallyCondition_timesOut(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
	}
	c := newFakeClient(secret)
	mt := &mockT{}

	extractor := func(_ client.Object) []metav1.Condition {
		return []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionFalse},
		}
	}

	EventuallyCondition(mt, context.Background(), ConditionPollOpts{
		Client:         c,
		Key:            client.ObjectKeyFromObject(secret),
		Obj:            &corev1.Secret{},
		Extractor:      extractor,
		ConditionType:  "Ready",
		ExpectedStatus: metav1.ConditionTrue,
		Timeout:        100 * time.Millisecond,
		Interval:       20 * time.Millisecond,
	})

	g.Expect(mt.failed).To(BeTrue(), "expected failure on timeout")
}

func TestEventuallyCondition_eventuallySucceeds(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
	}
	c := newFakeClient(secret)
	mt := &mockT{}

	conds := &threadSafeConditions{}

	extractor := func(_ client.Object) []metav1.Condition {
		return conds.get()
	}

	// Update conditions after a short delay.
	go func() {
		time.Sleep(60 * time.Millisecond)
		conds.set([]metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue},
		})
	}()

	EventuallyCondition(mt, context.Background(), ConditionPollOpts{
		Client:         c,
		Key:            client.ObjectKeyFromObject(secret),
		Obj:            &corev1.Secret{},
		Extractor:      extractor,
		ConditionType:  "Ready",
		ExpectedStatus: metav1.ConditionTrue,
		Timeout:        time.Second,
		Interval:       20 * time.Millisecond,
	})

	g.Expect(mt.failed).To(BeFalse(), "expected no failure, but got: %s", mt.message)
}

func TestEventuallyCondition_contextCancelled(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
	}
	c := newFakeClient(secret)
	mt := &mockT{}

	extractor := func(_ client.Object) []metav1.Condition {
		return nil // condition never appears
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the context after a short delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	EventuallyCondition(mt, ctx, ConditionPollOpts{
		Client:         c,
		Key:            client.ObjectKeyFromObject(secret),
		Obj:            &corev1.Secret{},
		Extractor:      extractor,
		ConditionType:  "Ready",
		ExpectedStatus: metav1.ConditionTrue,
		Timeout:        5 * time.Second,
		Interval:       20 * time.Millisecond,
	})

	g.Expect(mt.failed).To(BeTrue(), "expected failure when context is cancelled")
	g.Expect(mt.message).To(ContainSubstring("context cancelled"))
}

func TestEventuallyCondition_failsOnZeroInterval(t *testing.T) {
	g := NewGomegaWithT(t)
	mt := &mockT{}

	EventuallyCondition(mt, context.Background(), ConditionPollOpts{
		Client:         newFakeClient(),
		ConditionType:  "Ready",
		ExpectedStatus: metav1.ConditionTrue,
		Timeout:        time.Second,
		Interval:       0,
	})

	g.Expect(mt.failed).To(BeTrue(), "expected failure for zero Interval")
	g.Expect(mt.message).To(ContainSubstring("Interval must be > 0"))
}

func TestEventuallyCondition_failsOnZeroTimeout(t *testing.T) {
	g := NewGomegaWithT(t)
	mt := &mockT{}

	EventuallyCondition(mt, context.Background(), ConditionPollOpts{
		Client:         newFakeClient(),
		ConditionType:  "Ready",
		ExpectedStatus: metav1.ConditionTrue,
		Timeout:        0,
		Interval:       20 * time.Millisecond,
	})

	g.Expect(mt.failed).To(BeTrue(), "expected failure for zero Timeout")
	g.Expect(mt.message).To(ContainSubstring("Timeout must be > 0"))
}

func TestEventuallyCondition_failsOnNonNotFoundGetError(t *testing.T) {
	g := NewGomegaWithT(t)
	// A fake client with an empty scheme will return a "no kind is registered"
	// error for typed objects, which is a non-NotFound error.
	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mt := &mockT{}

	extractor := func(_ client.Object) []metav1.Condition {
		return nil
	}

	EventuallyCondition(mt, context.Background(), ConditionPollOpts{
		Client:         c,
		Key:            client.ObjectKey{Name: "test", Namespace: "default"},
		Obj:            &corev1.Secret{},
		Extractor:      extractor,
		ConditionType:  "Ready",
		ExpectedStatus: metav1.ConditionTrue,
		Timeout:        time.Second,
		Interval:       20 * time.Millisecond,
	})

	g.Expect(mt.failed).To(BeTrue(), "expected failure for non-NotFound Get error")
	g.Expect(mt.message).To(ContainSubstring("failed to get"))
}

func TestConditionTypes_empty(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(conditionTypes(nil)).To(Equal("(none)"))
	g.Expect(conditionTypes([]metav1.Condition{})).To(Equal("(none)"))
}

func TestConditionTypes_multiple(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(conditionTypes(sampleConditions())).To(Equal(`"Ready", "Degraded"`))
}
