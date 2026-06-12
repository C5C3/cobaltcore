// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// ensureRotationRBAC creates the ServiceAccount, Role, and RoleBinding shared by
// every split-compute-write rotation CronJob — Fernet keys, credential keys, and
// the Model B admin password. The three flavours are
// structurally identical: a dedicated ServiceAccount, a two-rule Role, and a
// RoleBinding with an immutable RoleRef, all named saName and owned by keystone.
//
// The Role is split into two PolicyRules to enforce least-privilege on the
// CronJob ServiceAccount:
//
//  1. Read-only `get` on readSecret — the operator-owned source Secret the
//     CronJob reads (the production keys Secret, or the admin push-source
//     Secret). The CronJob can never write it, so a compromised CronJob cannot
//     forge production keys or a privileged admin credential.
//  2. `get`+`patch` scoped by resourceNames to stagingSecret — the CronJob
//     writes its rotation output there. `create` and `delete` are deliberately
//     withheld because the operator owns the staging Secret's lifecycle.
//
// RoleRef is immutable after creation, so it is only set on new RoleBindings.
func (r *KeystoneReconciler) ensureRotationRBAC(ctx context.Context, keystone *keystonev1alpha1.Keystone, saName, readSecret, stagingSecret string) error {
	// ServiceAccount
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: keystone.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetControllerReference(keystone, sa, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring ServiceAccount %s: %w", saName, err)
	}

	// Role split into two PolicyRules:
	//   1. `get` on the read-only source Secret (operator owns writes).
	//   2. `get`+`patch` on the staging Secret only; no `create`/`delete`
	//      because the operator manages the staging Secret's lifecycle.
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: keystone.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				Verbs:         []string{"get"},
				ResourceNames: []string{readSecret},
			},
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				Verbs:         []string{"get", "patch"},
				ResourceNames: []string{stagingSecret},
			},
		}
		return controllerutil.SetControllerReference(keystone, role, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring Role %s: %w", saName, err)
	}

	// RoleBinding
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: keystone.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: keystone.Namespace,
		}}
		// RoleRef is immutable after creation; only set on new objects.
		if rb.RoleRef.Name == "" {
			rb.RoleRef = rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     saName,
			}
		}
		return controllerutil.SetControllerReference(keystone, rb, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring RoleBinding %s: %w", saName, err)
	}

	return nil
}
