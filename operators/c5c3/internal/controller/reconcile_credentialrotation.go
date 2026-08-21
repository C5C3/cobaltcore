// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
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

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
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
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneservices,verbs=get;list;watch
// +kubebuilder:rbac:groups=openstack.k-orc.cloud,resources=applicationcredentials;users,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile is the main reconciliation loop for the CredentialRotation CR.
//
// DECISION (ControlPlane lookup): the adminApplicationCredential target carries
// no explicit ControlPlane reference, so for it the reconciler looks up
// ControlPlane CR(s) in the CredentialRotation's OWN namespace. The L1 contract
// is one control plane per namespace, so:
//   - exactly one ControlPlane  -> operate on it;
//   - zero ControlPlanes        -> Ready=False (Reason "NoControlPlane") and a
//     short requeue, because the operator cannot rotate a credential for a
//     control plane that does not exist yet;
//   - multiple ControlPlanes    -> Ready=False (Reason "AmbiguousControlPlane")
//     and NO requeue, because picking one arbitrarily could rotate the wrong
//     credential; an operator must split the control planes into separate
//     namespaces or add an explicit reference (a later-level field).
//
// The serviceAccountPassword target does not use that lookup: it names a
// KeystoneService in its own namespace and resolves the ControlPlane through
// that CR's controlPlaneRef, so a registration in a namespace that holds no
// ControlPlane at all still rotates.
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
	// scheduled-deferral handling below, but they resolve their ControlPlane
	// differently and nudge a different owned CR through a different annotation.
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

	if cr.Spec.Target == c5c3v1alpha1.RotationTargetServiceAccountPassword {
		return r.rotateServiceAccountPassword(ctx, &cr)
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

// rotateServiceAccountPassword nudges the KeystoneService reconciler to rotate
// the password of the account of the KeystoneService named by
// spec.keystoneService, mirroring the admin re-mint nudge: it never touches the
// User itself beyond CLEARING the generation annotation, so the account's
// resource lifecycle stays owned solely by the KeystoneService reconciler
// (ensureManagedAccountUser in registration_projection.go consumes the cleared
// annotation).
//
// The KeystoneService lives in the CredentialRotation's own namespace and names
// the ControlPlane itself, so the ControlPlane comes from its controlPlaneRef
// rather than from the same-namespace lookup the admin path uses. Because that
// reference crosses namespaces and is fully caller-controlled, the path repeats
// the KeystoneService reconciler's own pre-write gates before it nudges: the
// plane's registration consent for the CR's namespace, the plane's
// AdminCredentialReady condition, and the ownership labels on the User it is
// about to nudge.
//
// There is no auto-detect path (unlike the admin credential there is no external
// password source to observe), so a rotation fires only on an explicit reMint,
// latched to the spec generation exactly like the admin flow so a `reMint: true`
// left in the spec does not re-fire on every resync.
func (r *CredentialRotationReconciler) rotateServiceAccountPassword(
	ctx context.Context, cr *c5c3v1alpha1.CredentialRotation,
) (ctrl.Result, error) {
	// MinLength=1 plus the CEL rule make an empty name unreachable on CREATE, so
	// the only way in is a stored CR that predates this field and decoded with the
	// name dropped. Say that, rather than looking the name up and reporting a
	// dangling reference to a KeystoneService nobody ever wrote; and do not
	// requeue, because only a re-created CR can resolve it.
	if cr.Spec.KeystoneService == "" {
		return r.finish(ctx, cr, ctrl.Result{}, metav1.ConditionFalse,
			"MissingKeystoneService",
			"spec.keystoneService is empty; it replaced the removed spec.serviceAccount field and names a "+
				"KeystoneService registration, so a CredentialRotation written against the old field must be re-created")
	}

	ks := &c5c3v1alpha1.KeystoneService{}
	ksKey := client.ObjectKey{Namespace: cr.Namespace, Name: cr.Spec.KeystoneService}
	if err := r.Get(ctx, ksKey, ks); err != nil {
		if apierrors.IsNotFound(err) {
			return r.finish(ctx, cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
				metav1.ConditionFalse, "KeystoneServiceNotFound",
				fmt.Sprintf("KeystoneService %s/%s not found; cannot rotate its account password",
					cr.Namespace, cr.Spec.KeystoneService))
		}
		return ctrl.Result{}, fmt.Errorf("fetching KeystoneService %s/%s: %w",
			cr.Namespace, cr.Spec.KeystoneService, err)
	}

	// A catalog-only registration declares no account to rotate. The account
	// block can still be added by a later spec edit, so this is a wait rather
	// than a terminal failure.
	if ks.Spec.Account == nil {
		return r.finish(ctx, cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
			metav1.ConditionFalse, "NoAccountDeclared",
			fmt.Sprintf("KeystoneService %q declares no account; there is no password to rotate", ks.Name))
	}

	// Resolve the ControlPlane exactly as the KeystoneService reconciler does, so
	// both agree on which plane the registration's children belong to.
	cp := &c5c3v1alpha1.ControlPlane{}
	cpKey := client.ObjectKey{
		Namespace: cmp.Or(ks.Spec.ControlPlaneRef.Namespace, ks.Namespace),
		Name:      ks.Spec.ControlPlaneRef.Name,
	}
	if err := r.Get(ctx, cpKey, cp); err != nil {
		if apierrors.IsNotFound(err) {
			return r.finish(ctx, cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
				metav1.ConditionFalse, "ControlPlaneNotFound",
				fmt.Sprintf("ControlPlane %s referenced by KeystoneService %q not found; cannot rotate", cpKey, ks.Name))
		}
		return ctrl.Result{}, fmt.Errorf("fetching ControlPlane %s: %w", cpKey, err)
	}

	// Resolution parity is not enough: the KeystoneService reconciler gates on the
	// plane's consent immediately after it (keystoneservice_controller.go), and
	// this path writes into the very namespace that gate protects. Skipping it
	// would let a namespace the plane does not admit reach the plane's namespace
	// through a pointer it fully controls — and because de-listing a namespace
	// FREEZES its registrations rather than deleting their children, the User is
	// still there to be nudged while the reconciler that would act on the nudge no
	// longer runs, so the rotation would report success and rotate nothing.
	if !keystoneServiceNamespaceAllowed(cp, ks) {
		return r.finish(ctx, cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
			metav1.ConditionFalse, reasonKeystoneServiceNamespaceNotAllowed,
			fmt.Sprintf("ControlPlane %s does not admit service registrations from namespace %q; "+
				"its KeystoneService reconciler is frozen there and cannot rotate. "+
				"Add the namespace to its spec.korc.serviceRegistrations.allowedNamespaces to admit it",
				cpKey, ks.Namespace))
	}

	// Locate the owned managed User via the keystoneservice_controller.go naming
	// helpers so both reconcilers agree on the object identity.
	user := &orcv1alpha1.User{}
	userKey := client.ObjectKey{Namespace: keystoneServiceChildNamespace(cp), Name: keystoneServiceUserRef(ks)}
	userErr := r.Get(ctx, userKey, user)
	userExists := userErr == nil
	if userErr != nil && !apierrors.IsNotFound(userErr) {
		return ctrl.Result{}, fmt.Errorf("fetching service-account User %s: %w", userKey, userErr)
	}

	// A child outside the registration's namespace carries no owner reference, so
	// the derived name is the only thing tying it to ks and the API server
	// enforces nothing. Test the same ownership labels the KeystoneService
	// reconciler stamps on every projected child, so a User the operator does not
	// consider this registration's account — one whose prefix digest collides with
	// another registration's, or one nothing has claimed — is never nudged on this
	// CR's behalf.
	//
	// This is a consistency gate against the operator's own ownership contract,
	// NOT a boundary that outranks the projection. The managed User is the one
	// child that never goes through ensureKeystoneServiceChild (it is created by
	// ensureManagedAccountUser behind managedChildProbeGate, which tests no
	// ownership), so once the KeystoneService reconciler adopts and labels an
	// object left at those coordinates, the next pass here sees its own child and
	// rotates it. And a leftover from a same-named predecessor is indistinguishable
	// by construction: the same name and namespace derive the same digest and the
	// same labels.
	if userExists && !isKeystoneServiceChild(user, ks) {
		return r.finish(ctx, cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
			metav1.ConditionFalse, "ForeignServiceAccount",
			fmt.Sprintf("User %s was not created by KeystoneService %q; refusing to rotate it", userKey, ks.Name))
	}

	// Bootstrap: idempotent initial provision. If the User exists this is a no-op
	// success; otherwise the KeystoneService reconciler is responsible for
	// creating it, so wait and requeue. We never create it here.
	if cr.Spec.Bootstrap {
		if userExists {
			return r.finish(ctx, cr, ctrl.Result{}, metav1.ConditionTrue,
				"BootstrapComplete",
				fmt.Sprintf("service account of KeystoneService %q already exists; bootstrap is a no-op", cr.Spec.KeystoneService))
		}
		return r.finish(ctx, cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
			metav1.ConditionFalse, "WaitingForBootstrap",
			fmt.Sprintf("service account of KeystoneService %q not yet provisioned by the KeystoneService reconciler; waiting", cr.Spec.KeystoneService))
	}

	if !userExists {
		return r.finish(ctx, cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
			metav1.ConditionFalse, "WaitingForServiceAccount",
			fmt.Sprintf("service account of KeystoneService %q does not exist yet; cannot rotate", cr.Spec.KeystoneService))
	}

	// A service-account rotation fires only on an explicit reMint (latched).
	if !cr.Spec.ReMint || cr.Status.LastTriggeredGeneration == cr.Generation {
		return r.finish(ctx, cr, ctrl.Result{}, metav1.ConditionTrue,
			"NoRotationNeeded",
			"no pending reMint; no rotation performed")
	}

	// The third gate the KeystoneService reconciler's reconcileNormal runs before
	// it touches the account: K-ORC cannot reach Keystone before the plane's admin
	// credential is minted, so an unready plane defers the registration exactly as
	// a withdrawn consent freezes it — the User is still there to nudge while the
	// reconciler that would consume the nudge does not act. Refusing here rather
	// than nudging keeps the generation UNLATCHED, so the rotation fires once the
	// plane recovers instead of reporting a green one-shot that never changed a
	// password. It therefore gates the NUDGE ONLY, below the read-only branches
	// above: AdminCredentialReady also dips on an ESO/OpenBao blip, a not-yet-Ready
	// clouds.yaml or an admin re-mint, and neither a settled one-shot (already
	// rotated, generation latched) nor a bootstrap no-op (nothing written at all)
	// has anything pending that the dip could hold up.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeAdminCredentialReady) {
		return r.finish(ctx, cr, ctrl.Result{RequeueAfter: credentialRotationRequeueAfter},
			metav1.ConditionFalse, reasonWaitingForServiceAccountAdmin,
			fmt.Sprintf("ControlPlane %s reports AdminCredentialReady is not True; its KeystoneService "+
				"reconciler cannot reach Keystone, so the nudge would not be consumed", cpKey))
	}

	if err := r.clearServiceAccountGenerationAnnotation(ctx, user); err != nil {
		return ctrl.Result{}, fmt.Errorf("clearing generation annotation to nudge service-account rotation: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Event(cr, "Normal", "RotationNudged",
			fmt.Sprintf("cleared the generation annotation of KeystoneService %q's service account to trigger a password rotation by the KeystoneService reconciler",
				cr.Spec.KeystoneService))
	}
	cr.Status.LastTriggeredGeneration = cr.Generation
	return r.finish(ctx, cr, ctrl.Result{}, metav1.ConditionTrue,
		"RotationTriggered",
		fmt.Sprintf("cleared the generation annotation; the KeystoneService reconciler will rotate the password of KeystoneService %q's service account",
			cr.Spec.KeystoneService))
}

// clearServiceAccountGenerationAnnotation zeroes the password-generation
// annotation on the managed User so the KeystoneService reconciler rotates on its
// next pass. It is a no-op (no Update) when the annotation is already empty/absent
// so a repeated reconcile does not churn the object.
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
	statusBefore := cr.Status.DeepCopy()
	return commonreconcile.UpdateStatus(ctx, r.Client, cr, statusBefore, &cr.Status, func() {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeRotationReady,
			Status:             status,
			ObservedGeneration: cr.Generation,
			Reason:             reason,
			Message:            message,
		})
		cr.Status.ObservedGeneration = cr.Generation
	}, result, nil)
}

// SetupWithManager registers the CredentialRotationReconciler with the manager.
func (r *CredentialRotationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&c5c3v1alpha1.CredentialRotation{}).
		Complete(r)
}
