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

// dbCredentialSecretNameSuffix is appended to keystoneName(cp) to derive the
// deterministic, collision-free name of the per-ControlPlane service DB-credential
// Secret/ExternalSecret, mirroring the *Suffix discipline used
// by the K-ORC admin-credential names so a single namespace can host the DB
// credential of multiple ControlPlanes.
const dbCredentialSecretNameSuffix = "-db-credentials" //nolint:gosec // G101 false positive: Secret name suffix, not a credential.

// dbCredentialRemoteKeyFor returns the per-ControlPlane OpenBao path the service
// DB credential is read from. The path is scoped by both the
// ControlPlane's Namespace and Name so two ControlPlanes never resolve to the
// same key on the cluster-global OpenBao backend.
func dbCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("openstack/keystone/%s/%s/db", cp.Namespace, cp.Name)
}

// dbCredentialSecretName returns the deterministic name of the per-ControlPlane
// DB-credential Secret/ExternalSecret (see dbCredentialSecretNameSuffix). It is
// derived from keystoneName(cp) so it tracks the projected Keystone CR.
func dbCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneName(cp) + dbCredentialSecretNameSuffix
}

// dbCredentialExternalSecret builds the per-ControlPlane, OpenBao-backed
// ExternalSecret that materialises the service DB credential into childNamespace(cp)
// It is a PURE builder: no owner reference is set here — the
// reconciler adds the ControlPlane controller reference in the CreateOrUpdate mutate
// closure (so GC is wired) while keeping this builder usable for shape assertions.
// The ExternalSecret type is esov1 (the v1 API), matching ensureKORCCloudsYAMLExternalSecret.
func dbCredentialExternalSecret(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	name := dbCredentialSecretName(cp)
	remoteKey := dbCredentialRemoteKeyFor(cp)
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: childNamespace(cp)},
		Spec: esov1.ExternalSecretSpec{
			RefreshInterval: &metav1.Duration{Duration: time.Hour},
			SecretStoreRef:  esov1.SecretStoreRef{Kind: "ClusterSecretStore", Name: openBaoClusterStoreName},
			Target:          esov1.ExternalSecretTarget{Name: name, CreationPolicy: esov1.CreatePolicyOwner},
			Data: []esov1.ExternalSecretData{
				{SecretKey: "username", RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: "username"}},
				{SecretKey: "password", RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: "password"}},
			},
		},
	}
}

// reconcileDBCredentials projects (in managed mode) the per-ControlPlane service
// DB-credential ExternalSecret and drives the DBCredentialsReady condition
//
// Brownfield CONTROL: when the ControlPlane supplies its own database connection
// (Database.ClusterRef == nil, i.e. Host-based brownfield mode), the user owns the
// DB credential Secret out-of-band, so the operator projects NO ExternalSecret and
// never references OpenBao / the ClusterSecretStore. DBCredentialsReady is reported
// True immediately so the chain proceeds to Keystone.
//
// Managed CONTROL: the per-CP ExternalSecret is create-or-updated (owner-referenced
// to the ControlPlane for GC, ESO owning the materialised Secret via CreationPolicy
// Owner). The condition mirrors the ExternalSecret's Ready status, requeuing while
// ESO has not yet synced — the wait/condition handling mirrors reconcileAdminCredential.
func (r *ControlPlaneReconciler) reconcileDBCredentials(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Brownfield early-exit: the user supplies their own DB credential Secret, so
	// there is nothing for the operator to project or reference in OpenBao.
	if cp.Spec.Infrastructure.Database.ClusterRef == nil {
		logger.Info("brownfield database (user-supplied credential), skipping DB credential ExternalSecret projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "BrownfieldUserSuppliedCredential",
			Message:            "brownfield database: the user supplies the DB credential Secret out-of-band; no ExternalSecret is projected",
		})
		return ctrl.Result{}, nil
	}

	// Check the OpenBao-backed ClusterSecretStore first so an ESO/OpenBao outage
	// surfaces as DBCredentialsReady=False even while the per-ExternalSecret cache
	// still reports Ready=True from its last successful sync. ESO only re-syncs
	// ExternalSecrets at their refreshInterval (default 1h), so relying on the
	// ExternalSecret Ready alone would miss short-lived outages; the
	// ClusterSecretStore watch wakes the ControlPlane the moment ESO flips the
	// store condition (#476). Mirrors reconcile_secrets.go in the keystone operator.
	storeReady, err := secrets.IsClusterSecretStoreReady(ctx, r.Client, openBaoClusterStoreName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !storeReady {
		logger.Info("ClusterSecretStore not ready, requeuing DB credential projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "SecretStoreNotReady",
			Message: fmt.Sprintf("ClusterSecretStore %q is not ready; upstream secret backend unreachable",
				openBaoClusterStoreName),
		})
		return ctrl.Result{RequeueAfter: dbCredentialsRequeueAfter}, nil
	}

	// Managed mode: create-or-update the per-CP DB-credential ExternalSecret,
	// owner-referencing it to the ControlPlane so it is garbage-collected with the CR.
	desired := dbCredentialExternalSecret(cp)
	es := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
		es.Spec = desired.Spec
		return controllerutil.SetControllerReference(cp, es, r.Scheme)
	}); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ExternalSecretError",
			Message:            fmt.Sprintf("ensuring DB credential ExternalSecret: %v", err),
		})
		return ctrl.Result{}, err
	}

	exists, ready, err := secrets.WaitForExternalSecret(ctx, r.Client,
		types.NamespacedName{Namespace: childNamespace(cp), Name: dbCredentialSecretName(cp)})
	if err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ExternalSecretError",
			Message:            fmt.Sprintf("checking DB credential ExternalSecret: %v", err),
		})
		return ctrl.Result{}, err
	}
	if !ready {
		// The reconciler ensures this ExternalSecret just above, so exists is
		// almost always true here; a false value indicates a transient cache
		// lag and is surfaced in the log rather than the user-facing status.
		logger.Info("DB credential ExternalSecret not ready, requeuing", "exists", exists)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForDBCredentialSecret",
			Message:            "DB credential ExternalSecret is not yet Ready",
		})
		return ctrl.Result{RequeueAfter: dbCredentialsRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeDBCredentialsReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "DBCredentialsReady",
		Message:            "DB credential ExternalSecret is Ready",
	})
	return ctrl.Result{}, nil
}
