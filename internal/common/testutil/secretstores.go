// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// ForbidClusterSecretStoreGet returns fake-client interceptor funcs that fail t
// on any Get of a ClusterSecretStore and pass every Get through to the fake.
// A namespace-scoped operator must never read the cluster-scoped kind: its Role
// cannot grant it, and a cached read would block the reconcile worker on an
// informer that never syncs.
func ForbidClusterSecretStoreGet(t testing.TB) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*esov1.ClusterSecretStore); ok {
				t.Errorf("unexpected Get of ClusterSecretStore %q by a namespace-scoped operator", key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}
}
