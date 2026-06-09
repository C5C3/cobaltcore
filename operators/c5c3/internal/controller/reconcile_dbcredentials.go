// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// dbCredentialSecretNameSuffix is appended to the projected Keystone CR name to
// derive the operator-owned Secret (and same-named ExternalSecret) holding the
// per-ControlPlane Keystone DB credential. Mirrors the keystoneNameSuffix /
// adminAppCredentialSecretSuffix discipline so a single namespace can host the DB
// credential of multiple ControlPlanes without clashing (CC-0116, REQ-003).
const dbCredentialSecretNameSuffix = "-db-credentials" //nolint:gosec // G101 false positive: Secret name suffix, not a credential.

// dbCredentialRemoteKeyFor returns the per-ControlPlane OpenBao path the Keystone
// database credential is read from (CC-0116, REQ-003). The path is scoped by both
// the ControlPlane's Namespace and Name so two ControlPlanes never resolve to the
// same DB credential on the cluster-global OpenBao backend, replacing the legacy
// shared flat "openstack/keystone/db" path. It mirrors adminAppCredentialRemoteKeyFor's
// per-CR scoping (CC-0112).
func dbCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("openstack/keystone/%s/%s/db", cp.Namespace, cp.Name)
}

// dbCredentialSecretName returns the name of the operator-owned Secret (and the
// ExternalSecret that materialises it) holding the Keystone DB credential for the
// given ControlPlane. It is derived from the projected Keystone CR name so it is
// deterministic and collision-free within a namespace, and is the value
// reconcileKeystone substitutes into keystone.Spec.Database.SecretRef in managed
// mode (CC-0116, REQ-002, REQ-003).
func dbCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneName(cp) + dbCredentialSecretNameSuffix
}

// dbCredentialExternalSecret builds the per-ControlPlane ExternalSecret that reads
// the Keystone DB credential (username/password) from OpenBao and materialises it
// as a Secret of the same name in the ControlPlane's namespace (CC-0116, REQ-001).
// It mirrors the field shape of the static deploy/eso/externalsecrets/keystone-db.yaml
// — the OpenBao ClusterSecretStore, a 1h refresh, and an Owner-created target
// Secret — but scoped to the per-CP remote key. The returned object carries no
// owner reference; reconcileDBCredentials sets it inside the CreateOrUpdate closure.
func dbCredentialExternalSecret(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	name := dbCredentialSecretName(cp)
	remoteKey := dbCredentialRemoteKeyFor(cp)
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: childNamespace(cp),
		},
		Spec: esov1.ExternalSecretSpec{
			RefreshInterval: &metav1.Duration{Duration: time.Hour},
			SecretStoreRef: esov1.SecretStoreRef{
				Name: openBaoClusterStoreName,
				Kind: "ClusterSecretStore",
			},
			Target: esov1.ExternalSecretTarget{
				Name:           name,
				CreationPolicy: esov1.CreatePolicyOwner,
			},
			Data: []esov1.ExternalSecretData{
				{
					SecretKey: "username",
					RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: "username"},
				},
				{
					SecretKey: "password",
					RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: "password"},
				},
			},
		},
	}
}

// reconcileDBCredentials ensures the operator-owned, per-ControlPlane Keystone DB
// credential and drives the DBCredentialsReady condition (CC-0116, REQ-001,
// REQ-004, REQ-005). It runs BEFORE reconcileKeystone so the Keystone CR is never
// projected against a DB-credential Secret that does not yet exist.
//
// Managed mode (database ClusterRef set): ensure the per-CP ExternalSecret (which
// ESO materialises from OpenBao) and gate DBCredentialsReady on it reporting
// Ready. While the ExternalSecret has not synced, the condition is False
// (WaitingForDBCredentialSecret) and the reconcile requeues.
//
// Brownfield mode (ClusterRef nil): the operator does not provision the database,
// so it must not manufacture a credential for it. No ExternalSecret is created and
// DBCredentialsReady is True immediately, leaving the user-supplied secretRef in
// place.
func (r *ControlPlaneReconciler) reconcileDBCredentials(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if cp.Spec.Infrastructure.Database.ClusterRef == nil {
		logger.V(1).Info("Database is brownfield; no operator-owned DB credential is provisioned")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "BrownfieldDatabase",
			Message:            "Database is brownfield (user-supplied); no operator-owned DB credential is provisioned",
		})
		return ctrl.Result{}, nil
	}

	// Ensure the per-CP ExternalSecret. The builder yields the desired object; a
	// fresh object carrying only its identity drives CreateOrUpdate so the spec and
	// owner reference are re-asserted idempotently against any existing one.
	desired := dbCredentialExternalSecret(cp)
	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
		es.Spec = desired.Spec
		return controllerutil.SetControllerReference(cp, es, r.Scheme)
	}); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ExternalSecretError",
			Message:            fmt.Sprintf("ensuring DB-credential ExternalSecret: %v", err),
		})
		return ctrl.Result{}, fmt.Errorf("ensuring DB-credential ExternalSecret %q: %w", desired.Name, err)
	}

	// Gate on the ExternalSecret reporting Ready so the Keystone CR is not projected
	// against an unmaterialised Secret.
	ready, err := secrets.WaitForExternalSecret(ctx, r.Client,
		types.NamespacedName{Namespace: childNamespace(cp), Name: dbCredentialSecretName(cp)})
	if err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ExternalSecretError",
			Message:            fmt.Sprintf("checking DB-credential ExternalSecret: %v", err),
		})
		return ctrl.Result{}, err
	}
	if !ready {
		logger.Info("DB-credential ExternalSecret not ready, requeuing", "externalSecret", dbCredentialSecretName(cp))
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForDBCredentialSecret",
			Message:            "DB-credential ExternalSecret is not yet Ready",
		})
		return ctrl.Result{RequeueAfter: dbCredentialsRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeDBCredentialsReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "DBCredentialsReady",
		Message:            "Per-ControlPlane DB-credential ExternalSecret is Ready",
	})
	return ctrl.Result{}, nil
}
