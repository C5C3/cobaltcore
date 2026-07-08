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
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// adminPasswordSecretNameSuffix is appended to keystoneName(cp) to derive the
// deterministic, collision-free name of the per-ControlPlane admin-password
// Secret/ExternalSecret, mirroring the *Suffix discipline used
// by the DB-credential and K-ORC admin-credential names so a single namespace can
// host the admin password of multiple ControlPlanes.
const adminPasswordSecretNameSuffix = "-admin-credentials" //nolint:gosec // G101 false positive: Secret name suffix, not a credential.

// adminPasswordRemoteKeyFor returns the per-ControlPlane OpenBao path the admin
// password is read from. Unlike the DB-credential path, this
// path is keystone-NAME-scoped — bootstrap/{ns}/{keystoneName}/admin, NOT
// cp-name-scoped — because it must match the seeder and the keystone-operator
// Model B rotation PushSecret, which both write/read the admin password at
// bootstrap/{keystone.Namespace}/{keystone.Name}/admin
// (operators/keystone/internal/controller/reconcile_passwordrotation.go). The
// {ns}/{keystoneName} scoping still keeps two ControlPlanes from resolving to the
// same key on the cluster-global OpenBao backend.
func adminPasswordRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("bootstrap/%s/%s/admin", cp.Namespace, keystoneName(cp))
}

// adminPasswordSecretName returns the deterministic name of the per-ControlPlane
// admin-password Secret/ExternalSecret (see adminPasswordSecretNameSuffix). It is
// derived from keystoneName(cp) so it tracks the projected Keystone CR.
func adminPasswordSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneName(cp) + adminPasswordSecretNameSuffix
}

// adminPasswordExternalSecret builds the per-ControlPlane, OpenBao-backed
// ExternalSecret that materialises the admin password into childNamespace(cp)
// It is a PURE builder: no owner reference is set here — the
// reconciler adds the ControlPlane controller reference in the CreateOrUpdate mutate
// closure (so GC is wired) while keeping this builder usable for shape assertions.
// The ExternalSecret type is esov1 (the v1 API), matching dbCredentialGeneratorExternalSecret.
func adminPasswordExternalSecret(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	name := adminPasswordSecretName(cp)
	remoteKey := adminPasswordRemoteKeyFor(cp)
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: childNamespace(cp)},
		Spec: esov1.ExternalSecretSpec{
			RefreshInterval: &metav1.Duration{Duration: time.Hour},
			SecretStoreRef:  esov1.SecretStoreRef{Kind: "ClusterSecretStore", Name: openBaoClusterStoreName},
			Target:          esov1.ExternalSecretTarget{Name: name, CreationPolicy: esov1.CreatePolicyOwner},
			Data: []esov1.ExternalSecretData{
				{SecretKey: "password", RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: "password"}},
			},
		},
	}
}

// effectiveAdminPasswordSecretRef returns the SecretRef every cp-side admin-password
// consumer will read. In managed mode (Database.ClusterRef != nil)
// the operator projects the admin password from OpenBao into the per-ControlPlane
// Secret named adminPasswordSecretName(cp), so the effective ref points at that
// Secret's "password" key. In brownfield mode the user supplies their own admin
// password Secret out-of-band, so the ref is the user-declared
// cp.Spec.KORC.AdminCredential.PasswordSecretRef verbatim. This NEVER mutates
// cp.Spec — it returns the ref by value so callers cannot alias the user's spec.
//
// NOTE: callers of effectiveAdminPasswordSecretRef are added in Level 2 — defining
// it now is intentional; it is not wired into any consumer yet.
//
//nolint:unused // declared by task 1.2; consumed by the Keystone projection, readAdminPassword, and the Secret-name extractor wired in Level 2 (tasks 2.1-2.3).
func effectiveAdminPasswordSecretRef(cp *c5c3v1alpha1.ControlPlane) commonv1.SecretRefSpec {
	// spec.infrastructure is optional (External keystone mode omits it). A nil
	// block reads as "no managed database", so the effective ref is the
	// user-supplied passwordSecretRef — the same branch brownfield mode takes.
	if infra := cp.Spec.Infrastructure; infra != nil && infra.Database.ClusterRef != nil {
		return commonv1.SecretRefSpec{Name: adminPasswordSecretName(cp), Key: "password"}
	}
	return cp.Spec.KORC.AdminCredential.PasswordSecretRef
}

// reconcileAdminPassword projects (in managed mode) the per-ControlPlane
// admin-password ExternalSecret and drives the AdminPasswordReady condition
//
// It runs BEFORE the Keystone sub-reconciler in the chain: the keystone-operator's
// SecretsReady gate needs the admin-password ExternalSecret to exist before the
// projected Keystone child references it.
//
// Brownfield CONTROL: when the ControlPlane supplies its own database connection
// (Database.ClusterRef == nil, i.e. Host-based brownfield mode), the user owns the
// admin-password Secret out-of-band, so the operator projects NO ExternalSecret and
// never references OpenBao / the ClusterSecretStore. AdminPasswordReady is reported
// True immediately so the chain proceeds to Keystone.
//
// Managed CONTROL: the per-CP ExternalSecret is create-or-updated (owner-referenced
// to the ControlPlane for GC, ESO owning the materialised Secret via CreationPolicy
// Owner). The condition mirrors the ExternalSecret's Ready status, requeuing while
// ESO has not yet synced — the wait/condition handling mirrors reconcileDBCredentials.
func (r *ControlPlaneReconciler) reconcileAdminPassword(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Brownfield early-exit: the user supplies their own admin-password Secret, so
	// there is nothing for the operator to project or reference in OpenBao. A nil
	// spec.infrastructure (External keystone mode) is treated the same way — no
	// managed database means no operator-owned admin-password projection.
	if infra := cp.Spec.Infrastructure; infra == nil || infra.Database.ClusterRef == nil {
		logger.Info("brownfield database (user-supplied credential), skipping admin password ExternalSecret projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminPasswordReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "BrownfieldUserSuppliedCredential",
			Message:            "brownfield database: the user supplies the admin password Secret out-of-band; no ExternalSecret is projected",
		})
		return ctrl.Result{}, nil
	}

	// Check the OpenBao-backed ClusterSecretStore first so an ESO/OpenBao outage
	// surfaces as AdminPasswordReady=False even while the per-ExternalSecret cache
	// still reports Ready=True from its last successful sync (#476). Mirrors
	// reconcileDBCredentials and the keystone operator's reconcile_secrets.go.
	storeReady, err := secrets.IsClusterSecretStoreReady(ctx, r.Client, openBaoClusterStoreName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !storeReady {
		logger.Info("ClusterSecretStore not ready, requeuing admin password projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminPasswordReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "SecretStoreNotReady",
			Message: fmt.Sprintf("ClusterSecretStore %q is not ready; upstream secret backend unreachable",
				openBaoClusterStoreName),
		})
		return ctrl.Result{RequeueAfter: adminPasswordRequeueAfter}, nil
	}

	// Managed mode: create-or-update the per-CP admin-password ExternalSecret,
	// owner-referencing it to the ControlPlane so it is garbage-collected with the CR.
	desired := adminPasswordExternalSecret(cp)
	es := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
		es.Spec = desired.Spec
		return controllerutil.SetControllerReference(cp, es, r.Scheme)
	}); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminPasswordReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ExternalSecretError",
			Message:            fmt.Sprintf("ensuring admin password ExternalSecret: %v", err),
		})
		return ctrl.Result{}, err
	}

	exists, ready, err := secrets.WaitForExternalSecret(ctx, r.Client,
		types.NamespacedName{Namespace: childNamespace(cp), Name: adminPasswordSecretName(cp)})
	if err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminPasswordReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ExternalSecretError",
			Message:            fmt.Sprintf("checking admin password ExternalSecret: %v", err),
		})
		return ctrl.Result{}, err
	}
	if !ready {
		// The reconciler ensures this ExternalSecret just above, so exists is
		// almost always true here; a false value indicates a transient cache
		// lag and is surfaced in the log rather than the user-facing status.
		logger.Info("admin password ExternalSecret not ready, requeuing", "exists", exists)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminPasswordReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForAdminPasswordSecret",
			Message:            "admin password ExternalSecret is not yet Ready",
		})
		return ctrl.Result{RequeueAfter: adminPasswordRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeAdminPasswordReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "AdminPasswordReady",
		Message:            "admin password ExternalSecret is Ready",
	})
	return ctrl.Result{}, nil
}
