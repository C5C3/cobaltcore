// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"context"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/cache"
)

// MemcachedWithServerList returns an unstructured Memcached namespace/name
// whose status.serverList holds entries in the given order. The fake client
// accepts it without a scheme registration.
func MemcachedWithServerList(namespace, name string, entries ...string) *unstructured.Unstructured {
	u := cache.NewMemcached()
	u.SetNamespace(namespace)
	u.SetName(name)
	if err := unstructured.SetNestedStringSlice(u.Object, entries, "status", "serverList"); err != nil {
		panic(err)
	}
	return u
}

// MemcachedGetInterceptor returns fake-client interceptor funcs that count
// every Get of a Memcached in gets and answer it with err, or pass it through
// to the fake when err is nil. Every other Get passes through unchanged.
func MemcachedGetInterceptor(gets *atomic.Int32, err error) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == cache.MemcachedGVK {
				gets.Add(1)
				if err != nil {
					return err
				}
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}
}
