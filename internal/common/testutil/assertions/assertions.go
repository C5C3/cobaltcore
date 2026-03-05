// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package assertions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Feature: CC-0002

// AssertCondition checks that a condition with the given type exists in the
// conditions slice and has the expected status. It calls t.Helper() and
// t.Errorf on failure.
func AssertCondition(t testing.TB, conditions []metav1.Condition, conditionType string, expectedStatus metav1.ConditionStatus) {
	t.Helper()

	c := meta.FindStatusCondition(conditions, conditionType)
	if c == nil {
		t.Errorf("condition %q not found; available types: %s", conditionType, conditionTypes(conditions))
		return
	}
	if c.Status != expectedStatus {
		t.Errorf("condition %q: expected status %q, got %q", conditionType, expectedStatus, c.Status)
	}
}

// AssertConditionWithReason checks that a condition exists with the expected
// status and reason.
func AssertConditionWithReason(t testing.TB, conditions []metav1.Condition, conditionType string, expectedStatus metav1.ConditionStatus, expectedReason string) {
	t.Helper()

	c := meta.FindStatusCondition(conditions, conditionType)
	if c == nil {
		t.Errorf("condition %q not found; available types: %s", conditionType, conditionTypes(conditions))
		return
	}
	if c.Status != expectedStatus {
		t.Errorf("condition %q: expected status %q, got %q", conditionType, expectedStatus, c.Status)
	}
	if c.Reason != expectedReason {
		t.Errorf("condition %q: expected reason %q, got %q", conditionType, expectedReason, c.Reason)
	}
}

// AssertConditionMissing checks that no condition with the given type exists.
func AssertConditionMissing(t testing.TB, conditions []metav1.Condition, conditionType string) {
	t.Helper()

	if c := meta.FindStatusCondition(conditions, conditionType); c != nil {
		t.Errorf("expected condition %q to be absent, but it exists with status %q", conditionType, c.Status)
	}
}

// AssertResourceExists checks that a Kubernetes resource exists by getting it
// with the provided client. It calls t.Helper() and t.Fatalf on failure.
func AssertResourceExists(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey, obj client.Object) {
	t.Helper()

	if err := c.Get(ctx, key, obj); err != nil {
		t.Fatalf("expected resource %s/%s to exist, but Get returned: %v", key.Namespace, key.Name, err)
	}
}

// AssertResourceNotExists checks that a Kubernetes resource does NOT exist.
// It expects a NotFound error from the client.
func AssertResourceNotExists(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey, obj client.Object) {
	t.Helper()

	err := c.Get(ctx, key, obj)
	if err == nil {
		t.Fatalf("expected resource %s/%s to not exist, but Get succeeded", key.Namespace, key.Name)
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound error for resource %s/%s, but got: %v", key.Namespace, key.Name, err)
	}
}

// ConditionExtractor retrieves conditions from a Kubernetes resource.
type ConditionExtractor func(obj client.Object) []metav1.Condition

// ConditionPollOpts configures how EventuallyCondition polls for a condition.
type ConditionPollOpts struct {
	Client         client.Client
	Key            client.ObjectKey
	Obj            client.Object
	Extractor      ConditionExtractor
	ConditionType  string
	ExpectedStatus metav1.ConditionStatus
	Timeout        time.Duration
	Interval       time.Duration
}

// EventuallyCondition polls a Kubernetes resource until the specified condition
// reaches the expected status or the timeout expires.
func EventuallyCondition(t testing.TB, ctx context.Context, opts ConditionPollOpts) {
	t.Helper()

	if opts.Interval <= 0 {
		t.Fatalf("invalid ConditionPollOpts: Interval must be > 0, got %s", opts.Interval)
		return
	}
	if opts.Timeout <= 0 {
		t.Fatalf("invalid ConditionPollOpts: Timeout must be > 0, got %s", opts.Timeout)
		return
	}

	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	deadline := time.After(opts.Timeout)

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled while waiting for condition %q", opts.ConditionType)
			return
		case <-deadline:
			t.Fatalf("timed out waiting for condition %q to be %q on %s/%s",
				opts.ConditionType, opts.ExpectedStatus, opts.Key.Namespace, opts.Key.Name)
			return
		case <-ticker.C:
			if err := opts.Client.Get(ctx, opts.Key, opts.Obj); err != nil {
				if apierrors.IsNotFound(err) {
					continue // resource might not exist yet
				}
				t.Fatalf("failed to get %s/%s while waiting for condition %q: %v",
					opts.Key.Namespace, opts.Key.Name, opts.ConditionType, err)
				return
			}
			if c := meta.FindStatusCondition(opts.Extractor(opts.Obj), opts.ConditionType); c != nil && c.Status == opts.ExpectedStatus {
				return // success
			}
		}
	}
}

// conditionTypes returns a comma-separated list of condition types for error
// messages.
func conditionTypes(conditions []metav1.Condition) string {
	if len(conditions) == 0 {
		return "(none)"
	}
	types := make([]string, len(conditions))
	for i, c := range conditions {
		types[i] = fmt.Sprintf("%q", c.Type)
	}
	return strings.Join(types, ", ")
}
