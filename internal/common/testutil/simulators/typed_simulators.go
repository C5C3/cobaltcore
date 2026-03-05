// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package simulators

import (
	"context"
	"fmt"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Feature: CC-0005

// SimulateDatabaseReady updates a typed MariaDB Database resource's status to
// indicate readiness by setting the Ready condition to True.
func SimulateDatabaseReady(ctx context.Context, c client.Client, key client.ObjectKey) error {
	db := &mariadbv1alpha1.Database{}
	if err := c.Get(ctx, key, db); err != nil {
		return fmt.Errorf("getting Database %s: %w", key, err)
	}

	now := metav1.Now()
	meta.SetStatusCondition(&db.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             "DatabaseReady",
		Message:            "Database is ready",
		LastTransitionTime: now,
	})

	return c.Status().Update(ctx, db)
}

// SimulateUserReady updates a typed MariaDB User resource's status to
// indicate readiness by setting the Ready condition to True.
func SimulateUserReady(ctx context.Context, c client.Client, key client.ObjectKey) error {
	user := &mariadbv1alpha1.User{}
	if err := c.Get(ctx, key, user); err != nil {
		return fmt.Errorf("getting User %s: %w", key, err)
	}

	now := metav1.Now()
	meta.SetStatusCondition(&user.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             "UserReady",
		Message:            "User is ready",
		LastTransitionTime: now,
	})

	return c.Status().Update(ctx, user)
}

// SimulateGrantReady updates a typed MariaDB Grant resource's status to
// indicate readiness by setting the Ready condition to True.
func SimulateGrantReady(ctx context.Context, c client.Client, key client.ObjectKey) error {
	grant := &mariadbv1alpha1.Grant{}
	if err := c.Get(ctx, key, grant); err != nil {
		return fmt.Errorf("getting Grant %s: %w", key, err)
	}

	now := metav1.Now()
	meta.SetStatusCondition(&grant.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             "GrantReady",
		Message:            "Grant is ready",
		LastTransitionTime: now,
	})

	return c.Status().Update(ctx, grant)
}

// SimulatePushSecretSynced updates a typed ESO PushSecret resource's status to
// indicate successful synchronization by setting the Ready condition to True.
func SimulatePushSecretSynced(ctx context.Context, c client.Client, key client.ObjectKey) error {
	ps := &esov1alpha1.PushSecret{}
	if err := c.Get(ctx, key, ps); err != nil {
		return fmt.Errorf("getting PushSecret %s: %w", key, err)
	}

	now := metav1.Now()

	// Replace or add the Ready condition (PushSecret uses a custom condition type).
	found := false
	for i, cond := range ps.Status.Conditions {
		if cond.Type == esov1alpha1.PushSecretReady {
			ps.Status.Conditions[i] = esov1alpha1.PushSecretStatusCondition{
				Type:               esov1alpha1.PushSecretReady,
				Status:             corev1.ConditionTrue,
				Reason:             esov1alpha1.ReasonSynced,
				Message:            "PushSecret synced successfully",
				LastTransitionTime: now,
			}
			found = true
			break
		}
	}
	if !found {
		ps.Status.Conditions = append(ps.Status.Conditions, esov1alpha1.PushSecretStatusCondition{
			Type:               esov1alpha1.PushSecretReady,
			Status:             corev1.ConditionTrue,
			Reason:             esov1alpha1.ReasonSynced,
			Message:            "PushSecret synced successfully",
			LastTransitionTime: now,
		})
	}
	ps.Status.RefreshTime = now

	return c.Status().Update(ctx, ps)
}

// SimulateCertificateReady updates a typed cert-manager Certificate resource's
// status to indicate readiness by setting the Ready condition to True.
func SimulateCertificateReady(ctx context.Context, c client.Client, key client.ObjectKey) error {
	cert := &certmanagerv1.Certificate{}
	if err := c.Get(ctx, key, cert); err != nil {
		return fmt.Errorf("getting Certificate %s: %w", key, err)
	}

	now := metav1.Now()

	// Replace or add the Ready condition (Certificate uses a custom condition type).
	found := false
	for i, cond := range cert.Status.Conditions {
		if cond.Type == certmanagerv1.CertificateConditionReady {
			cert.Status.Conditions[i] = certmanagerv1.CertificateCondition{
				Type:               certmanagerv1.CertificateConditionReady,
				Status:             cmmeta.ConditionTrue,
				Reason:             "Ready",
				Message:            "Certificate is ready",
				LastTransitionTime: &now,
			}
			found = true
			break
		}
	}
	if !found {
		cert.Status.Conditions = append(cert.Status.Conditions, certmanagerv1.CertificateCondition{
			Type:               certmanagerv1.CertificateConditionReady,
			Status:             cmmeta.ConditionTrue,
			Reason:             "Ready",
			Message:            "Certificate is ready",
			LastTransitionTime: &now,
		})
	}

	return c.Status().Update(ctx, cert)
}
