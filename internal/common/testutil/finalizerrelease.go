// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"context"
	"errors"
	"sync/atomic"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// ConflictFirstUpdate returns fake-client interceptor funcs that answer the
// first Update with a 409 Conflict, the answer of an API server whose object
// changed after the caller read it, and pass every later one through to the
// fake.
func ConflictFirstUpdate() interceptor.Funcs {
	var conflicted atomic.Bool
	return interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if conflicted.CompareAndSwap(false, true) {
				return apierrors.NewConflict(schema.GroupResource{}, obj.GetName(), errors.New("the object has been modified"))
			}
			return c.Update(ctx, obj, opts...)
		},
	}
}

// NotFoundOnUpdate returns fake-client interceptor funcs that answer every
// Update with a 404 Not Found, the answer of an API server whose object left
// etcd after the caller read it from its cache.
func NotFoundOnUpdate() interceptor.Funcs {
	return interceptor.Funcs{
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			return apierrors.NewNotFound(schema.GroupResource{}, obj.GetName())
		},
	}
}
