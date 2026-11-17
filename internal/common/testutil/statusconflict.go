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

// ConflictFirstStatusUpdate returns fake-client interceptor funcs that answer
// the first status subresource update with a 409 Conflict, the answer of an
// API server whose object changed after the caller read it, and pass every
// later one through to the fake.
func ConflictFirstStatusUpdate() interceptor.Funcs {
	var conflicted atomic.Bool
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if subResource == "status" && conflicted.CompareAndSwap(false, true) {
				return apierrors.NewConflict(schema.GroupResource{}, obj.GetName(), errors.New("the object has been modified"))
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	}
}
