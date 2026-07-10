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
const conditionTypeRotationReady = "Ready"

// credentialRotationRequeueAfter is the short backoff used while a Bootstrap
// rotation waits for the ControlPlane reconciler to mint the admin
// ApplicationCredential CR.
const credentialRotationRequeueAfter = credentialRotationWaitInterval

// CredentialRotationReconciler reconciles a CredentialRotation object. It drives
// one-shot rotations of a control-plane credential by NUDGING the ControlPlane
// reconciler rather than duplicating any mint logic.
type CredentialRotationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=c5c3.io,resources=credentialrotations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=c5c3.io,resources=credentialrotations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=controlplanes,verbs=get;list;watch
// +kubebuilder:rbac:groups=openstack.k-orc.cloud,resources=applicationcredentials;users,verbs=get;list;watch;update;patch
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
//
// DECISION (reMint is one-shot per spec generation): an explicit spec.reMint is
// LATCHED on status.lastTriggeredGeneration. Without a latch a `reMint: true` left
// in the spec would re-fire on every cache resync (~10 min via SyncPeriod) and on
// every operator restart, revoking + re-minting the admin credential indefinitely
// and re-opening the stale-credential auth window each cycle. The reconciler
// therefore nudges for an explicit reMint only while
// status.lastTriggeredGeneration != metadata.generation, and records the
// generation once it has nudged; a subsequent pass over the same generation
// reports NoRotationNeeded. The auto-detect path (password-hash change) is NOT
// latched: it is already self-limiting (it stops nudging once the hash matches
// again) and relies on resync to observe an out-of-band password rotation, so a
// generation latch must not gate it.
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

	// Dispatch on the rotation target. Both supported targets share the
	// scheduled-deferral handling and the ControlPlane resolution below; the
	// per-target nudge differs (a different owned CR, a different annotation).
	switch cr.Spec.Target {
	case c5c3v1alpha1.RotationTargetAdminApplicationCredential, c5c3v1alpha1.RotationTargetServiceAccountPassword:
	default:
		return r.finish(ctx, &cr, ctrl.Result{}, metav1.ConditionFalse,
			"UnsupportedTarget",
			fmt.Sprintf("rotation target %q is not supported; supported targets are %q and %q",
				cr.Spec.Target, c5c3v1alpha1.RotationTargetAdminApplicationCredential,
				c5c3v1alpha1.RotationTargetServiceAccountPassword))
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

	if cr.Spec.Target == c5c3v1alpha1.RotationTargetServiceAccountPassword {
		return r.rotateServiceAccountPassword(ctx, &cr, cp)
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

	// Decide whether a re-mint nudge is required: explicit ReMint (latched to the
	// current spec generation so it fires once per edit, not on every resync), or
	// the admin password hash differs from the annotation last stamped by
	// reconcileKORC.
	remintRequested := cr.Spec.ReMint && cr.Status.LastTriggeredGeneration != cr.Generation
	nudge := remintRequested
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
		// Hash matches and no (un-latched) forced re-mint: nothing to do. A
		// `reMint: true` left in the spec lands here once it has already fired for
		// this generation, so it does NOT loop on every resync/restart.
		return r.finish(ctx, &cr, ctrl.Result{}, metav1.ConditionTrue,
			"NoRotationNeeded",
			"admin password unchanged and no pending reMint; no rotation performed")
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

	// Latch an explicit reMint to this spec generation so it is a one-shot: the
	// next reconcile of the same generation observes the recorded generation and
	// reports NoRotationNeeded instead of nudging again. The auto-detect path is
	// intentionally not latched (it self-limits once the hash matches), so only
	// stamp when this pass was driven by an explicit reMint.
	if remintRequested {
		cr.Status.LastTriggeredGeneration = cr.Generation
	}

	return r.finish(ctx, &cr, ctrl.Result{}, metav1.ConditionTrue,
		"RotationTriggered",
		"cleared the password-hash annotation; the ControlPlane reconciler will re-mint the admin application credential")
}

// rotateServiceAccountPassword nudges reconcileServiceAccounts to rotate one
// declared service account's password, mirroring the admin re-mint nudge: it
// never touches the User itself beyond CLEARING the generation annotation, so the
// account's resource lifecycle stays owned solely by the ControlPlane reconciler.
//
// There is no auto-detect path (unlike the admin credential there is no external
// password source to observe), so a rotation fires only on an explicit reMint,
// latched to the spec generation exactly like the admin flow so a `reMint: true`
// left in the spec does not re-fire on every resync.
func (r *CredentialRotationReconciler) rotateServiceAccountPassword(
	ctx context.Context, cr *c5c3v1alpha1.CredentialRotation, cp *c5c3v1alpha1.ControlPlane,
) (ctrl.Result, error) {
	// Require the named account to be declared on the ControlPlane.
	var sa *c5c3v1alpha1.ServiceAccountSpec
	for i := range cp.Spec.KORC.ServiceAccounts {
		if cp.Spec.KORC.ServiceAccounts[i].Name == cr.Spec.ServiceAccount {
			sa = &cp.Spec.KORC.ServiceAccounts[i]
			break
		}
	}
	if sa == nil {
		return r.finish(ctx, cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
			metav1.ConditionFalse, "UnknownServiceAccount",
			fmt.Sprintf("no service account %q is declared on ControlPlane %q", cr.Spec.ServiceAccount, cp.Name))
	}

	// Locate the owned managed User via the reconcile_serviceaccounts.go naming
	// helper so both reconcilers agree on the object identity.
	user := &orcv1alpha1.User{}
	userKey := client.ObjectKey{Namespace: childNamespace(cp), Name: serviceAccountUserRef(cp, *sa)}
	userErr := r.Get(ctx, userKey, user)
	userExists := userErr == nil
	if userErr != nil && !apierrors.IsNotFound(userErr) {
		return ctrl.Result{}, fmt.Errorf("fetching service-account User %s: %w", userKey, userErr)
	}

	// Bootstrap: idempotent initial provision. If the User exists this is a no-op
	// success; otherwise the ControlPlane reconciler is responsible for creating
	// it, so wait and requeue. We never create it here.
	if cr.Spec.Bootstrap {
		if userExists {
			return r.finish(ctx, cr, ctrl.Result{}, metav1.ConditionTrue,
				"BootstrapComplete",
				fmt.Sprintf("service account %q already exists; bootstrap is a no-op", cr.Spec.ServiceAccount))
		}
		return r.finish(ctx, cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
			metav1.ConditionFalse, "WaitingForBootstrap",
			fmt.Sprintf("service account %q not yet provisioned by the ControlPlane reconciler; waiting", cr.Spec.ServiceAccount))
	}

	if !userExists {
		return r.finish(ctx, cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
			metav1.ConditionFalse, "WaitingForServiceAccount",
			fmt.Sprintf("service account %q does not exist yet; cannot rotate", cr.Spec.ServiceAccount))
	}

	// A service-account rotation fires only on an explicit reMint (latched).
	if !cr.Spec.ReMint || cr.Status.LastTriggeredGeneration == cr.Generation {
		return r.finish(ctx, cr, ctrl.Result{}, metav1.ConditionTrue,
			"NoRotationNeeded",
			"no pending reMint; no rotation performed")
	}

	if err := r.clearServiceAccountGenerationAnnotation(ctx, user); err != nil {
		return ctrl.Result{}, fmt.Errorf("clearing generation annotation to nudge service-account rotation: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Event(cr, "Normal", "RotationNudged",
			fmt.Sprintf("cleared service account %q generation annotation to trigger a password rotation by the ControlPlane reconciler",
				cr.Spec.ServiceAccount))
	}
	cr.Status.LastTriggeredGeneration = cr.Generation
	return r.finish(ctx, cr, ctrl.Result{}, metav1.ConditionTrue,
		"RotationTriggered",
		fmt.Sprintf("cleared the generation annotation; the ControlPlane reconciler will rotate service account %q's password",
			cr.Spec.ServiceAccount))
}

// clearServiceAccountGenerationAnnotation zeroes the password-generation
// annotation on the managed User so reconcileServiceAccounts rotates on its next
// pass. It is a no-op (no Update) when the annotation is already empty/absent so a
// repeated reconcile does not churn the object.
func (r *CredentialRotationReconciler) clearServiceAccountGenerationAnnotation(
	ctx context.Context, user *orcv1alpha1.User,
) error {
	if user.Annotations == nil || user.Annotations[serviceAccountPasswordGenerationAnnotation] == "" {
		return nil
	}
	user.Annotations[serviceAccountPasswordGenerationAnnotation] = ""
	return r.Update(ctx, user)
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
// caller should persist (see the DECISION on Reconcile). The multiple-match case
// is defense-in-depth: the ControlPlane validating webhook now enforces one
// ControlPlane per namespace on CREATE, so it should be
// unreachable in practice and only fires for CRs that predate the guard or
// callers that bypass the webhook.
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
		// AmbiguousControlPlane is defense-in-depth the
		// ControlPlane validating webhook enforces one ControlPlane per namespace
		// on CREATE (operators/c5c3/api/v1alpha1/controlplane_webhook.go), so a
		// namespace should never hold two. This branch remains as an explicit,
		// safe failure for CRs created before that guard shipped or callers that
		// bypass the webhook — it fails the rotation rather than silently picking
		// cps.Items[0].
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
// one.
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
