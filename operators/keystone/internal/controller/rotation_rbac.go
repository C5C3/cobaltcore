// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/rotation"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
)

// ensureRotationRBAC creates the ServiceAccount, Role, and RoleBinding shared by
// every split-compute-write rotation CronJob — Fernet keys, credential keys, and
// the Model B admin password — via the shared rotation.EnsureRBAC. The Role is
// split into a read-only get on readSecret and a get+patch scoped to
// stagingSecret so a compromised CronJob can neither forge production keys nor
// write outside its staging Secret.
func (r *KeystoneReconciler) ensureRotationRBAC(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone, saName, readSecret, stagingSecret string) error {
	return rotation.EnsureRBAC(ctx, children, r.Scheme, keystone, saName, readSecret, stagingSecret)
}
