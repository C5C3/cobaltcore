// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// mariaDBResourceKey returns the client.ObjectKey used for the MariaDB
// Database, User, and Grant CRs owned by keystone. Keeping the naming
// convention (keystone.Name, keystone.Namespace) in one place ensures that the
// provisioning builders, the finalizer cleanup, and the live-resource sentinel
// never disagree on the key — a future name suffix or namespace override
// changes here and propagates to every call site.
func mariaDBResourceKey(keystone *keystonev1alpha1.Keystone) client.ObjectKey {
	return client.ObjectKey{Name: keystone.Name, Namespace: keystone.Namespace}
}

// Condition and event reason constants for the expand-migrate-contract upgrade
// path, aliased to the shared vocabulary in internal/common/database so the
// keystone code and unit tests keep asserting the local names while the values
// stay in lockstep with the shared upgrade flow. Only the reasons keystone still
// emits locally (the target-changed guard in reconcileDatabase) or the tests
// reference by name are aliased here; every other upgrade reason is emitted by
// the shared flow directly.
const (
	conditionReasonUpgradeTargetChanged  = database.ReasonUpgradeTargetChanged
	conditionReasonVersionParseError     = database.ReasonVersionParseError
	conditionReasonDowngradeNotSupported = database.ReasonDowngradeNotSupported
	conditionReasonUpgradePathInvalid    = database.ReasonUpgradePathInvalid
	conditionReasonExpandInProgress      = database.ReasonExpandInProgress
	conditionReasonMigrateInProgress     = database.ReasonMigrateInProgress
	conditionReasonUpgradeRollingUpdate  = database.ReasonUpgradeRollingUpdate
)

// reconcileDatabase ensures the Keystone database schema is provisioned and
// migrated. In managed mode (ClusterRef set) it creates MariaDB Database, User,
// and Grant CRs and waits for them to become Ready before running the db_sync
// Job. In brownfield mode (Host set) it skips the MariaDB CRs and runs db_sync
// directly. The provisioning, steady-state sync, and expand-migrate-contract
// upgrade flows are all shared (internal/common/database); only the
// keystone-specific inputs (assembled by upgradeFlowParams) and the
// target-changed guard below stay local.
func (r *KeystoneReconciler) reconcileDatabase(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone, configMapName, domainsSecretName string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Managed/brownfield provisioning: MariaDB cluster gate, Database/User/Grant
	// ensure, Dynamic-credentials skip of the User/Grant. A non-zero result means
	// the flow set a not-ready condition and we must return it unchanged.
	res, err := database.ReconcileProvision(ctx, database.ProvisionFlowParams{
		Client:        children,
		Scheme:        r.Scheme,
		Owner:         keystone,
		InstanceName:  keystone.Name,
		Namespace:     keystone.Namespace,
		Database:      &keystone.Spec.Database,
		Conditions:    &keystone.Status.Conditions,
		Generation:    keystone.Generation,
		ConditionType: "DatabaseReady",
		RequeueAfter:  RequeueDatabaseWait,
	})
	if err != nil || !res.IsZero() {
		return res, err
	}

	// Active upgrade: the shared flow handles the abort (#468) and the phase
	// dispatch. The target-changed guard stays keystone-side so the error keeps
	// the "image tag" prose the unit test pins (the shared flow phrases it
	// generically as "spec release"). The abort case — spec.image.tag reverted
	// to the installed release — bypasses the guard so the shared AbortUpgrade
	// runs; it is checked first there because a revert also satisfies
	// tag != targetRelease and would otherwise trip this guard.
	if keystone.Status.UpgradePhase != "" {
		if keystone.Spec.Image.Tag != keystone.Status.InstalledRelease &&
			keystone.Spec.Image.Tag != keystone.Status.TargetRelease {
			logger.Info("Image tag changed during active upgrade",
				"targetRelease", keystone.Status.TargetRelease,
				"newTag", keystone.Spec.Image.Tag,
				"phase", keystone.Status.UpgradePhase)
			conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
				Type:               "DatabaseReady",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: keystone.Generation,
				Reason:             conditionReasonUpgradeTargetChanged,
				Message: fmt.Sprintf("Image tag changed to %s during active upgrade %s → %s; complete or roll back the current upgrade first",
					keystone.Spec.Image.Tag, keystone.Status.InstalledRelease, keystone.Status.TargetRelease),
			})
			r.Recorder.Eventf(keystone, corev1.EventTypeWarning, conditionReasonUpgradeTargetChanged, "Image tag changed to %s during active upgrade %s → %s", keystone.Spec.Image.Tag, keystone.Status.InstalledRelease, keystone.Status.TargetRelease)
			return ctrl.Result{}, fmt.Errorf("image tag changed during active upgrade: current upgrade targets %s but spec.image.tag is %s",
				keystone.Status.TargetRelease, keystone.Spec.Image.Tag)
		}
		return database.ReconcileUpgrade(ctx, r.upgradeFlowParams(ctx, children, keystone, configMapName, domainsSecretName))
	}

	// Detect upgrade.
	if isUpgrade(keystone) {
		return database.InitiateUpgrade(ctx, r.upgradeFlowParams(ctx, children, keystone, configMapName, domainsSecretName))
	}

	// Non-upgrade path: db_sync then schema-check, with terminal metrics emitted
	// per phase and the installed release tracked on success.
	return database.ReconcileSyncJobs(ctx, database.SyncFlowParams{
		Client:   children,
		Scheme:   r.Scheme,
		Recorder: r.Recorder,
		Owner:    keystone,
		Jobs:     keystoneJobSetParams(keystone, configMapName, domainsSecretName),
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, keystone, jobSuffix, observed)
		},
		Conditions:       &keystone.Status.Conditions,
		Generation:       keystone.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueDatabaseWait,
		InstalledRelease: &keystone.Status.InstalledRelease,
		ImageTag:         keystone.Spec.Image.Tag,
	})
}

// isUpgrade reports whether spec.image.tag requires the shared
// expand-migrate-contract flow for this Keystone CR. It is a thin package-private
// wrapper over database.IsUpgrade (kept because the unit test calls it directly)
// that passes the two values database.IsUpgrade reads: the spec release and the
// installed release.
func isUpgrade(keystone *keystonev1alpha1.Keystone) bool {
	return database.IsUpgrade(keystone.Spec.Image.Tag, keystone.Status.InstalledRelease)
}

// upgradeFlowParams assembles the shared expand-migrate-contract upgrade-flow
// inputs for this Keystone CR. The service-specific parts — the owner CR, the
// "DatabaseReady" condition vocabulary, spec.image.tag as the requested release,
// the per-phase Job builder, and the terminal-metric callback — are bound here;
// the phase choreography itself lives in internal/common/database. The three
// status pointers (UpgradePhase, InstalledRelease, TargetRelease) are mutated in
// place by the flow and persisted by the caller after it returns.
func (r *KeystoneReconciler) upgradeFlowParams(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone, configMapName, domainsSecretName string) database.UpgradeFlowParams {
	return database.UpgradeFlowParams{
		Client:           children,
		Scheme:           r.Scheme,
		Recorder:         r.Recorder,
		Owner:            keystone,
		Conditions:       &keystone.Status.Conditions,
		Generation:       keystone.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueUpgradeWait,
		Phase:            &keystone.Status.UpgradePhase,
		InstalledRelease: &keystone.Status.InstalledRelease,
		TargetRelease:    &keystone.Status.TargetRelease,
		SpecRelease:      keystone.Spec.Image.Tag,
		BuildPhaseJob: func(phase keystonev1alpha1.UpgradePhase) *batchv1.Job {
			switch phase {
			case keystonev1alpha1.UpgradePhaseExpanding:
				return buildExpandJob(keystone, configMapName, domainsSecretName, keystone.Spec.Image.Tag)
			case keystonev1alpha1.UpgradePhaseMigrating:
				return buildMigrateJob(keystone, configMapName, domainsSecretName, keystone.Spec.Image.Tag)
			case keystonev1alpha1.UpgradePhaseContracting:
				return buildContractJob(keystone, configMapName, domainsSecretName, keystone.Spec.Image.Tag)
			case keystonev1alpha1.UpgradePhaseRollingUpdate:
				// RollingUpdate builds no Job; the Deployment rollout drives it.
				return nil
			default:
				// The empty steady-state phase never reaches BuildPhaseJob.
				return nil
			}
		},
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, keystone, jobSuffix, observed)
		},
	}
}

// finalizeDatabaseResources issues Delete for the MariaDB Database, User, and
// Grant CRs named after the Keystone CR, delegating to the shared
// database.FinalizeResources. It returns as soon as the Delete requests have
// been accepted (or tolerated as NotFound) so the Keystone CR finalizer can be
// released in the same reconcile pass; see database.FinalizeResources for the
// deadlock-avoidance rationale.
func (r *KeystoneReconciler) finalizeDatabaseResources(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone) error {
	return database.FinalizeResources(ctx, children, mariaDBResourceKey(keystone))
}
