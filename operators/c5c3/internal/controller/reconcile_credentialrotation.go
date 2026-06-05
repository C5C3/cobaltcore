// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// conditionTypeRotationReady is the single Ready condition the CredentialRotation
// reconciler reports. Like the ControlPlane condition constants it is the source
// of truth for the status contract so a rename is caught by the compiler
// (CC-0110, REQ-015).
const conditionTypeRotationReady = "Ready"

// credentialRotationRequeueAfter is the short backoff used while a Bootstrap
// rotation waits for the ControlPlane reconciler to mint the admin
// ApplicationCredential CR (CC-0110, REQ-015).
const credentialRotationRequeueAfter = credentialRotationWaitInterval

// CredentialRotationReconciler reconciles a CredentialRotation object. It drives
// one-shot rotations of a control-plane credential by NUDGING the ControlPlane
// reconciler rather than duplicating any mint logic (CC-0110, REQ-015).
type CredentialRotationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=c5c3.io,resources=credentialrotations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=c5c3.io,resources=credentialrotations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=controlplanes,verbs=get;list;watch
// +kubebuilder:rbac:groups=openstack.k-orc.cloud,resources=applicationcredentials,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile is the main reconciliation loop for the CredentialRotation CR.
//
// DECISION (ControlPlane lookup): a CredentialRotation carries no explicit
// ControlPlane reference, so the reconciler looks up ControlPlane CR(s) in the
// CredentialRotation's OWN namespace. The L1 contract is one control plane per
// namespace, so:
//   - exactly one ControlPlane  -> operate on it;
//   - zero ControlPlanes        -> Ready=False (Reason "NoControlPlane") and a
//     short requeue, because the operator cannot rotate a credential for a
//     control plane that does not exist yet;
//   - multiple ControlPlanes    -> Ready=False (Reason "AmbiguousControlPlane")
//     and NO requeue, because picking one arbitrarily could rotate the wrong
//     credential; an operator must split the control planes into separate
//     namespaces or add an explicit reference (a later-level field).
//
// DECISION (re-mint nudge): the reconciler NEVER mints or deletes the credential
// itself. reconcileKORC stamps the SHA-256 of the admin password onto the owned
// AC CR via adminPasswordHashAnnotation and, on a hash mismatch, re-mints by
// deleting + recreating the AC. To force a re-mint this reconciler simply CLEARS
// (zeroes) the annotation on the AC CR — the lightest possible nudge. On its next
// pass reconcileKORC observes the mismatch (computed hash != "") and performs the
// delete+recreate re-mint, re-stamping the fresh hash. Keeping the AC's resource
// lifecycle (including the delete) owned solely by the ControlPlane reconciler
// avoids two controllers racing on the same object.
func (r *CredentialRotationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cr c5c3v1alpha1.CredentialRotation
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("CredentialRotation resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching CredentialRotation: %w", err)
	}

	// Only the admin application credential target is supported at L1.
	if cr.Spec.Target != c5c3v1alpha1.RotationTargetAdminApplicationCredential {
		return r.finish(ctx, &cr, ctrl.Result{}, metav1.ConditionFalse,
			"UnsupportedTarget",
			fmt.Sprintf("rotation target %q is not supported; only %q is supported",
				cr.Spec.Target, c5c3v1alpha1.RotationTargetAdminApplicationCredential))
	}

	// DECISION (scheduled rotation loop deferred to a later level; matches the L1
	// types decision in credentialrotation_types.go): IntervalDays /
	// PreRotationDays / GracePeriodDays are READ-but-IGNORED here. When any are
	// set we surface an informational event so an operator knows the scheduled
	// loop is not yet active, but we MUST NOT error and MUST NOT run a loop.
	if cr.Spec.IntervalDays != nil || cr.Spec.PreRotationDays != nil || cr.Spec.GracePeriodDays != nil {
		if r.Recorder != nil {
			r.Recorder.Event(&cr, "Normal", "ScheduledRotationDeferred",
				"Scheduled-rotation fields are accepted but not yet implemented at this level; performing one-shot rotation semantics only")
		}
		logger.Info("scheduled-rotation fields set but deferred; ignoring",
			"intervalDays", cr.Spec.IntervalDays,
			"preRotationDays", cr.Spec.PreRotationDays,
			"gracePeriodDays", cr.Spec.GracePeriodDays)
	}

	// Locate the target ControlPlane in the CredentialRotation's namespace.
	cp, result, condition := r.resolveControlPlane(ctx, &cr)
	if cp == nil {
		return r.finish(ctx, &cr, result, condition.status, condition.reason, condition.message)
	}

	// Locate the owned admin ApplicationCredential CR via the reconcile_korc.go
	// naming helper so this reconciler and the ControlPlane reconciler agree on
	// the object identity.
	ac := &orcv1alpha1.ApplicationCredential{}
	acKey := client.ObjectKey{Namespace: childNamespace(cp), Name: adminAppCredentialName(cp)}
	acErr := r.Get(ctx, acKey, ac)
	acExists := acErr == nil
	if acErr != nil && !apierrors.IsNotFound(acErr) {
		return ctrl.Result{}, fmt.Errorf("fetching admin ApplicationCredential %s: %w", acKey, acErr)
	}

	// Bootstrap: idempotent initial mint. If the AC already exists this is a
	// no-op success; otherwise the ControlPlane reconciler is responsible for
	// minting, so wait (Ready=False) and requeue until it appears. We never mint
	// here.
	if cr.Spec.Bootstrap {
		if acExists {
			return r.finish(ctx, &cr, ctrl.Result{}, metav1.ConditionTrue,
				"BootstrapComplete",
				"admin application credential already exists; bootstrap is a no-op")
		}
		return r.finish(ctx, &cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
			metav1.ConditionFalse, "WaitingForBootstrap",
			"admin application credential not yet minted by the ControlPlane reconciler; waiting")
	}

	// Non-bootstrap rotation needs an existing AC to nudge.
	if !acExists {
		return r.finish(ctx, &cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
			metav1.ConditionFalse, "WaitingForApplicationCredential",
			"admin application credential does not exist yet; cannot rotate")
	}

	// Decide whether a re-mint nudge is required: explicit ReMint, or the admin
	// password hash differs from the annotation last stamped by reconcileKORC.
	nudge := cr.Spec.ReMint
	if !nudge {
		changed, err := r.passwordHashChanged(ctx, cp, ac)
		if err != nil {
			if secrets.IsMissingSecretOrKey(err) {
				return r.finish(ctx, &cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
					metav1.ConditionFalse, "WaitingForAdminPassword",
					"admin password Secret is not yet available; deferring rotation decision")
			}
			return ctrl.Result{}, fmt.Errorf("computing admin password hash: %w", err)
		}
		nudge = changed
	}

	if !nudge {
		// Hash matches and no forced re-mint: nothing to do.
		return r.finish(ctx, &cr, ctrl.Result{}, metav1.ConditionTrue,
			"NoRotationNeeded",
			"admin password unchanged and reMint not requested; no rotation performed")
	}

	// Perform the nudge: clear the password-hash annotation so reconcileKORC
	// deletes+recreates the AC (the re-mint) on its next pass. Clearing (vs
	// deleting the key) keeps the AC CR schema-valid and the change minimal.
	if err := r.clearPasswordHashAnnotation(ctx, ac); err != nil {
		return ctrl.Result{}, fmt.Errorf("clearing password-hash annotation to nudge re-mint: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Event(&cr, "Normal", "RotationNudged",
			"cleared admin application credential password-hash annotation to trigger a re-mint by the ControlPlane reconciler")
	}

	return r.finish(ctx, &cr, ctrl.Result{}, metav1.ConditionTrue,
		"RotationTriggered",
		"cleared the password-hash annotation; the ControlPlane reconciler will re-mint the admin application credential")
}

// controlPlaneCondition bundles the Ready condition fields the resolveControlPlane
// helper returns when it cannot operate on a single ControlPlane.
type controlPlaneCondition struct {
	status  metav1.ConditionStatus
	reason  string
	message string
}

// resolveControlPlane finds the single ControlPlane in the CredentialRotation's
// namespace. On success it returns the ControlPlane and a zero condition; on a
// zero/multiple-match it returns a nil ControlPlane plus the result+condition the
// caller should persist (see the DECISION on Reconcile).
func (r *CredentialRotationReconciler) resolveControlPlane(
	ctx context.Context, cr *c5c3v1alpha1.CredentialRotation,
) (*c5c3v1alpha1.ControlPlane, ctrl.Result, controlPlaneCondition) {
	var cps c5c3v1alpha1.ControlPlaneList
	if err := r.List(ctx, &cps, client.InNamespace(cr.Namespace)); err != nil {
		return nil, ctrl.Result{}, controlPlaneCondition{
			status:  metav1.ConditionFalse,
			reason:  "ControlPlaneListError",
			message: fmt.Sprintf("listing ControlPlanes in namespace %q: %v", cr.Namespace, err),
		}
	}

	switch len(cps.Items) {
	case 1:
		return &cps.Items[0], ctrl.Result{}, controlPlaneCondition{}
	case 0:
		return nil, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter}, controlPlaneCondition{
			status:  metav1.ConditionFalse,
			reason:  "NoControlPlane",
			message: fmt.Sprintf("no ControlPlane found in namespace %q", cr.Namespace),
		}
	default:
		return nil, ctrl.Result{}, controlPlaneCondition{
			status:  metav1.ConditionFalse,
			reason:  "AmbiguousControlPlane",
			message: fmt.Sprintf("%d ControlPlanes found in namespace %q; cannot determine the rotation target", len(cps.Items), cr.Namespace),
		}
	}
}

// passwordHashChanged reports whether the current admin password hash differs
// from the hash annotation last stamped on the AC CR by reconcileKORC. A missing
// annotation is treated as "changed" so a never-stamped AC is nudged.
func (r *CredentialRotationReconciler) passwordHashChanged(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, ac *orcv1alpha1.ApplicationCredential,
) (bool, error) {
	current, err := computeAdminPasswordHash(ctx, r.Client, cp)
	if err != nil {
		return false, err
	}
	stamped := ac.Annotations[adminPasswordHashAnnotation]
	return current != stamped, nil
}

// clearPasswordHashAnnotation zeroes the password-hash annotation on the AC CR so
// reconcileKORC re-mints on its next pass. It is a no-op (no Update) when the
// annotation is already empty/absent so a repeated reconcile does not churn the
// object.
func (r *CredentialRotationReconciler) clearPasswordHashAnnotation(
	ctx context.Context, ac *orcv1alpha1.ApplicationCredential,
) error {
	if ac.Annotations == nil || ac.Annotations[adminPasswordHashAnnotation] == "" {
		return nil
	}
	ac.Annotations[adminPasswordHashAnnotation] = ""
	return r.Update(ctx, ac)
}

// finish sets the Ready condition + ObservedGeneration and persists status,
// returning the given result. It mirrors the ControlPlane reconciler's
// updateStatus discipline so a stale status is distinguishable from a current
// one (CC-0110, REQ-015).
func (r *CredentialRotationReconciler) finish(
	ctx context.Context, cr *c5c3v1alpha1.CredentialRotation, result ctrl.Result,
	status metav1.ConditionStatus, reason, message string,
) (ctrl.Result, error) {
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeRotationReady,
		Status:             status,
		ObservedGeneration: cr.Generation,
		Reason:             reason,
		Message:            message,
	})
	cr.Status.ObservedGeneration = cr.Generation
	if err := r.Status().Update(ctx, cr); err != nil {
		log.FromContext(ctx).Error(err, "unable to update CredentialRotation status")
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}
	return result, nil
}

// SetupWithManager registers the CredentialRotationReconciler with the manager.
func (r *CredentialRotationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&c5c3v1alpha1.CredentialRotation{}).
		Complete(r)
}
