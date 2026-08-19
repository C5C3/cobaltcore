// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	"github.com/c5c3/forge/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// The per-block condition types a KeystoneService carries, plus the aggregate
// Ready derived from them. They share their strings with the ControlPlane's own
// CatalogReady, which is harmless and deliberate: the two live on different
// objects, and a reader comparing a registration to the plane it registers
// against should not have to learn a second vocabulary for the same idea.
const (
	conditionTypeKeystoneServiceCatalogReady = "CatalogReady"
	conditionTypeKeystoneServiceAccountReady = "AccountReady"
)

// keystoneServiceSubConditionTypes are the sub-conditions the aggregate Ready is
// derived from. BOTH are always present — an undeclared block reports True with a
// NotDeclared reason rather than being omitted — so SetAggregateReady, which
// requires every listed type present-and-True, can range over a fixed set.
var keystoneServiceSubConditionTypes = []string{
	conditionTypeKeystoneServiceCatalogReady,
	conditionTypeKeystoneServiceAccountReady,
}

// keystoneServiceFinalizerName gates teardown of the projected children. It
// follows controlPlaneORCFinalizer's shape: the group, then what it tears down.
const keystoneServiceFinalizerName = "c5c3.io/keystoneservice-teardown"

// Condition reasons this controller introduces. Everything with an equivalent in
// the ControlPlane's inline machinery reuses that constant instead
// (reasonWaitingForServiceAccountAdmin, reasonServiceAccountStoreNotReady,
// reasonProbingForCollision, reasonServiceAccountCollision,
// reasonWaitingForServiceAccounts, reasonServiceAccountsFailed,
// reasonServiceAccountError, conditionReasonCatalogFailed,
// conditionReasonWaitingForCatalog), so the two mechanisms report one vocabulary
// while they coexist.
const (
	// reasonKeystoneServiceCatalogNotDeclared / reasonKeystoneServiceAccountNotDeclared
	// are the True reasons for a block the spec does not declare. The condition is
	// still written (not omitted) so the status schema is identical whether or not
	// a block is declared.
	reasonKeystoneServiceCatalogNotDeclared = "CatalogNotDeclared"
	reasonKeystoneServiceAccountNotDeclared = "AccountNotDeclared"
	// reasonKeystoneServiceControlPlaneNotFound reports a dangling
	// spec.controlPlaneRef. It is a status condition rather than an admission
	// error by design: GitOps may apply the registration before the ControlPlane.
	reasonKeystoneServiceControlPlaneNotFound = "ControlPlaneNotFound"
	// reasonKeystoneServiceNamespaceNotAllowed reports that the ControlPlane does
	// not consent to registrations from this CR's namespace.
	reasonKeystoneServiceNamespaceNotAllowed = "NamespaceNotAllowed"
	// reasonKeystoneServiceAccountProvisioned is the True reason once the account
	// is provisioned in Keystone and its credentials are materialized.
	reasonKeystoneServiceAccountProvisioned = "AccountProvisioned"
	// reasonKeystoneServiceCatalogRegistered is the True reason once the catalog
	// entry is registered and Available.
	reasonKeystoneServiceCatalogRegistered = "CatalogRegistered"
	// reasonKeystoneServiceCatalogCollision reports the fail-loudly default: a
	// catalog service row of the declared type and name already exists and adopt
	// was not set.
	reasonKeystoneServiceCatalogCollision = "ServiceCollision"
	// reasonKeystoneServiceCatalogError reports a Kubernetes-level failure
	// reconciling a catalog child (not a K-ORC/OpenStack failure).
	reasonKeystoneServiceCatalogError = "CatalogError"
)

// KeystoneServiceReconciler owns the KeystoneService CR lifecycle and is the
// SINGLE writer of its status: the finalizer, the per-block projection of K-ORC
// children, the OpenBao round-trip behind the account's consumer Secret, and the
// CatalogReady / AccountReady / Ready conditions.
//
// It deliberately carries no cluster Resolver. A registration delivers into its
// own namespace on the management cluster; only the ControlPlane-projected
// built-ins keep the placed-service delivery legs.
type KeystoneServiceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Reconcile drives one KeystoneService CR: finalizer installation, the gated
// projection of the declared blocks, and the deletion teardown.
func (r *KeystoneServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ks c5c3v1alpha1.KeystoneService
	if err := r.Get(ctx, req.NamespacedName, &ks); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("KeystoneService not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching KeystoneService: %w", err)
	}

	if ks.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, &ks)
	}

	// Install the finalizer before any projection, so a deletion issued between
	// now and the next pass funnels through reconcileDelete and finds every child.
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &ks, keystoneServiceFinalizerName); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{Requeue: true}, nil
	}

	statusBefore := ks.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, &ks)
	return r.updateStatus(ctx, &ks, statusBefore, result, err)
}

// reconcileNormal drives the non-deleting path: the gates every block shares,
// then each declared block's projection, then the prune of what the spec stopped
// declaring.
//
// The two blocks are INDEPENDENT: a catalog failure never defers the account and
// vice versa, so each writes its own condition and the pass requeues if either
// asked for it. Only a Kubernetes-level error short-circuits, and the condition
// explaining it is already written by the time it propagates.
func (r *KeystoneServiceReconciler) reconcileNormal(ctx context.Context, ks *c5c3v1alpha1.KeystoneService) (ctrl.Result, error) {
	// Stamp the undeclared blocks FIRST, so both sub-condition types exist however
	// the gates below resolve. Without this a gate failure on a single-block CR
	// would leave the other type absent and the aggregate Ready permanently False
	// with nothing on the object explaining which sub-condition is missing.
	r.setUndeclaredBlockConditions(ks)

	cp, result, err := r.resolveControlPlane(ctx, ks)
	if err != nil || cp == nil {
		return result, err
	}

	if !keystoneServiceNamespaceAllowed(cp, ks) {
		r.failDeclaredBlocks(ks, reasonKeystoneServiceNamespaceNotAllowed, fmt.Sprintf(
			"ControlPlane %s/%s does not admit service registrations from namespace %q; nothing is projected",
			cp.Namespace, cp.Name, ks.Namespace,
		))
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// K-ORC cannot talk to Keystone before the admin credential is minted.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeAdminCredentialReady) {
		r.failDeclaredBlocks(ks, reasonWaitingForServiceAccountAdmin, fmt.Sprintf(
			"ControlPlane %s/%s reports AdminCredentialReady is not True; the registration is deferred",
			cp.Namespace, cp.Name,
		))
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)

	requeue := false
	if ks.Spec.Catalog != nil {
		res, err := r.ensureCatalog(ctx, ks, cp, credRef, managedCredRef)
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue = requeue || !res.IsZero()
	}
	if ks.Spec.Account != nil {
		res, err := r.ensureAccount(ctx, ks, cp, credRef, managedCredRef)
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue = requeue || !res.IsZero()
	}

	// Prune whatever the spec stopped declaring — a removed role, a removed
	// endpoint interface, a removed block — and hold the owning block's condition
	// until the removal completes.
	catalogSwept, accountSwept, err := r.sweepChildren(ctx, ks, keystoneServiceDeclaredChildNames(ks))
	if err != nil {
		r.failDeclaredBlocks(ks, reasonServiceAccountError, fmt.Sprintf("pruning undeclared registration children: %v", err))
		return ctrl.Result{}, err
	}
	if len(catalogSwept) > 0 {
		keystoneServiceFail(ks, conditionTypeKeystoneServiceCatalogReady)(conditionReasonWaitingForCatalog, fmt.Sprintf(
			"%d undeclared catalog child CR(s) are still being removed", len(catalogSwept),
		))
		requeue = true
	}
	if len(accountSwept) > 0 {
		keystoneServiceFail(ks, conditionTypeKeystoneServiceAccountReady)(reasonWaitingForServiceAccounts, fmt.Sprintf(
			"%d undeclared service-account child CR(s) are still being removed", len(accountSwept),
		))
		requeue = true
	}

	if requeue {
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

// setUndeclaredBlockConditions reports an undeclared block as True/NotDeclared
// and clears the status it can no longer describe.
func (r *KeystoneServiceReconciler) setUndeclaredBlockConditions(ks *c5c3v1alpha1.KeystoneService) {
	if ks.Spec.Catalog == nil {
		ks.Status.Catalog = nil
		conditions.SetCondition(&ks.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKeystoneServiceCatalogReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: ks.Generation,
			Reason:             reasonKeystoneServiceCatalogNotDeclared,
			Message:            "no catalog entry is declared",
		})
	}
	if ks.Spec.Account == nil {
		ks.Status.Account = nil
		conditions.SetCondition(&ks.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKeystoneServiceAccountReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: ks.Generation,
			Reason:             reasonKeystoneServiceAccountNotDeclared,
			Message:            "no service account is declared",
		})
	}
}

// keystoneServiceFail returns a closure bound to ks and condType that stamps a
// ConditionFalse status, mirroring conditionFailer for the ControlPlane —
// including the truncation, since these messages relay K-ORC's own verbatim and
// one over-long message would make the whole status write fail.
func keystoneServiceFail(ks *c5c3v1alpha1.KeystoneService, condType string) func(reason, message string) {
	return func(reason, message string) {
		conditions.SetCondition(&ks.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: ks.Generation,
			Reason:             reason,
			Message:            truncateConditionMessage(message),
		})
	}
}

// keystoneServiceSetTrue upserts a per-block condition as True. The message is
// truncated for the same reason the failure path truncates it: it interpolates
// spec strings that carry no length budget of their own, and one over-long
// message makes the whole status write fail.
func keystoneServiceSetTrue(ks *c5c3v1alpha1.KeystoneService, condType, reason, message string) {
	conditions.SetCondition(&ks.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ks.Generation,
		Reason:             reason,
		Message:            truncateConditionMessage(message),
	})
}

// failDeclaredBlocks writes one shared gate failure onto every DECLARED block's
// condition. An undeclared block keeps its NotDeclared True: a gate the
// registration never reaches for that block is not a failure of it.
func (r *KeystoneServiceReconciler) failDeclaredBlocks(ks *c5c3v1alpha1.KeystoneService, reason, message string) {
	if ks.Spec.Catalog != nil {
		keystoneServiceFail(ks, conditionTypeKeystoneServiceCatalogReady)(reason, message)
	}
	if ks.Spec.Account != nil {
		keystoneServiceFail(ks, conditionTypeKeystoneServiceAccountReady)(reason, message)
	}
}

// resolveControlPlane fetches the referenced ControlPlane. It returns a nil
// ControlPlane with a nil error and the gate condition already written when the
// reference dangles, and a wrapped error for any other read failure so the
// workqueue backs off instead of reporting a dangling reference it did not see.
func (r *KeystoneServiceReconciler) resolveControlPlane(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService,
) (*c5c3v1alpha1.ControlPlane, ctrl.Result, error) {
	key := client.ObjectKey{
		Namespace: cmp.Or(ks.Spec.ControlPlaneRef.Namespace, ks.Namespace),
		Name:      ks.Spec.ControlPlaneRef.Name,
	}
	var cp c5c3v1alpha1.ControlPlane
	if err := r.Get(ctx, key, &cp); err != nil {
		if apierrors.IsNotFound(err) {
			r.failDeclaredBlocks(ks, reasonKeystoneServiceControlPlaneNotFound, fmt.Sprintf(
				"ControlPlane %s not found; the registration is deferred until it exists", key,
			))
			return nil, ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		return nil, ctrl.Result{}, fmt.Errorf("fetching ControlPlane %s: %w", key, err)
	}
	return &cp, ctrl.Result{}, nil
}

// keystoneServiceNamespaceAllowed reports whether cp consents to a registration
// from ks's namespace.
//
// Today consent is own-namespace-only, which is what makes this controller safe
// to run before an authorization surface exists: a KeystoneService can mint a
// service user with arbitrary roles, so admitting a foreign namespace by default
// would turn namespace access into cloud admin. It is the ONE seam the
// ControlPlane's namespace allowlist widens; every other part of the reconciler
// is already namespace-agnostic.
func keystoneServiceNamespaceAllowed(cp *c5c3v1alpha1.ControlPlane, ks *c5c3v1alpha1.KeystoneService) bool {
	return ks.Namespace == cp.Namespace
}

// keystoneServiceCredentialRefs returns the two clouds.yaml references the
// projected children authenticate with, mirroring reconcileServiceAccounts.
//
// credRef is the ControlPlane's admin credential, used by the READ-ONLY probes
// and unmanaged imports. managedCredRef is the operator-owned admin PASSWORD
// cloud, used by every MANAGED child: the ApplicationCredential's K-ORC finalizer
// revokes the credential at teardown, so a managed child still authenticating
// through it would get a 404 on its own DELETE and orphan the Keystone resource.
func keystoneServiceCredentialRefs(cp *c5c3v1alpha1.ControlPlane) (credRef, managedCredRef orcv1alpha1.CloudCredentialsReference) {
	credRef = orcv1alpha1.CloudCredentialsReference{
		// Fall back to the conventional name for a webhook-bypassed CR, exactly as
		// reconcileCatalog and reconcileServiceAccounts do, so every child resolves
		// the same clouds.yaml Secret.
		SecretName: cmp.Or(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName, korcCloudsYamlSecretName),
		CloudName:  cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName,
	}
	return credRef, entryCredentialsRef(cp, credRef)
}

// updateStatus persists the status via the shared helper: the write is skipped
// when the pass changed nothing, the aggregate Ready is re-derived from both
// sub-conditions on every persist, and ObservedGeneration is stamped.
func (r *KeystoneServiceReconciler) updateStatus(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService,
	statusBefore *c5c3v1alpha1.KeystoneServiceStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, r.Client, ks, statusBefore, &ks.Status, func() {
		commonreconcile.SetAggregateReady(&ks.Status.Conditions, ks.Generation, keystoneServiceSubConditionTypes)
		ks.Status.ObservedGeneration = ks.Generation
	}, result, reconcileErr)
}

// --- naming ---

// keystoneServiceChildPrefix scopes every child this CR owns, and every sweep
// that removes one.
//
// The hash is taken over "namespace/name", not over the name alone: the children
// live in the ControlPlane's namespace, so once registrations from other
// namespaces are admitted two same-named CRs would otherwise project onto the
// same child names — one silently reshaping and then deleting the other's
// Keystone identity. Qualifying the prefix now means that extension is a gate
// change and not a rename, and a rename of a live child would delete and re-add
// the Keystone user and catalog row it stands for.
//
// The readable base is kept in front so `kubectl get user` still reads as the
// registration it belongs to (the same shape serviceAccountRoleSlug uses).
func keystoneServiceChildPrefix(ks *c5c3v1alpha1.KeystoneService) string {
	sum := sha256.Sum256([]byte(ks.Namespace + "/" + ks.Name))
	return ks.Name + "-" + hex.EncodeToString(sum[:])[:8] + "-registration-"
}

// The child names embed a per-kind discriminator in a fixed position after the
// prefix, so no CR's child can ever alias another's.
func keystoneServiceUserRef(ks *c5c3v1alpha1.KeystoneService) string {
	return keystoneServiceChildPrefix(ks) + "user"
}

func keystoneServiceUserProbeRef(ks *c5c3v1alpha1.KeystoneService) string {
	return keystoneServiceChildPrefix(ks) + "user-probe"
}

func keystoneServiceProjectRef(ks *c5c3v1alpha1.KeystoneService) string {
	return keystoneServiceChildPrefix(ks) + "project"
}

func keystoneServiceProjectProbeRef(ks *c5c3v1alpha1.KeystoneService) string {
	return keystoneServiceChildPrefix(ks) + "project-probe"
}

func keystoneServiceRoleImportRef(ks *c5c3v1alpha1.KeystoneService, role string) string {
	return keystoneServiceChildPrefix(ks) + "role-" + serviceAccountRoleSlug(role)
}

func keystoneServiceRoleAssignmentRef(ks *c5c3v1alpha1.KeystoneService, role string) string {
	return keystoneServiceChildPrefix(ks) + "assign-" + serviceAccountRoleSlug(role)
}

func keystoneServiceCatalogServiceRef(ks *c5c3v1alpha1.KeystoneService) string {
	return keystoneServiceChildPrefix(ks) + "service"
}

func keystoneServiceCatalogServiceProbeRef(ks *c5c3v1alpha1.KeystoneService) string {
	return keystoneServiceChildPrefix(ks) + "service-probe"
}

func keystoneServiceCatalogEndpointRef(ks *c5c3v1alpha1.KeystoneService, iface c5c3v1alpha1.ExternalEndpointType) string {
	return keystoneServiceChildPrefix(ks) + "endpoint-" + string(iface)
}

func keystoneServicePasswordSecretName(ks *c5c3v1alpha1.KeystoneService, gen int64) string {
	return fmt.Sprintf("%spassword-v%d", keystoneServiceChildPrefix(ks), gen)
}

func keystoneServiceSourceSecretName(ks *c5c3v1alpha1.KeystoneService) string {
	return keystoneServiceChildPrefix(ks) + "source"
}

func keystoneServicePushSecretName(ks *c5c3v1alpha1.KeystoneService) string {
	return keystoneServiceChildPrefix(ks) + "backup"
}

// keystoneServiceCredentialsSecretName is the consumer-facing handle, and the one
// child name deliberately NOT carrying the internal prefix: it is the documented
// contract a service reads its credentials from, so it stays predictable from the
// CR's name alone.
func keystoneServiceCredentialsSecretName(ks *c5c3v1alpha1.KeystoneService) string {
	return ks.Name + "-credentials"
}

// keystoneServiceDomainRef returns the Domain import the account's user and
// project resolve against. When the effective domain is the ControlPlane's admin
// domain it REUSES the admin Domain import — created by reconcileKORC, owned by
// the ControlPlane, and therefore never swept by this CR's prune.
func keystoneServiceDomainRef(cp *c5c3v1alpha1.ControlPlane, ks *c5c3v1alpha1.KeystoneService) string {
	if keystoneServiceDomainName(cp, ks) == adminDomainName(cp) {
		return adminDomainRef(cp)
	}
	return keystoneServiceChildPrefix(ks) + "domain"
}

// keystoneServiceUserName resolves the OpenStack user name, defaulting to the
// CR's own name. It is resolved here rather than assumed from the defaulting
// webhook, for the reason DedicatedServiceNamespaces() gives: admission can be
// absent, not merely unavailable (an unregistered webhook during install, a
// GitOps or etcd restore replaying stored objects), and a nil-defaulted user name
// would otherwise project a K-ORC User with an empty name.
func keystoneServiceUserName(ks *c5c3v1alpha1.KeystoneService) string {
	if ks.Spec.Account == nil {
		return ""
	}
	return cmp.Or(ks.Spec.Account.UserName, ks.Name)
}

// keystoneServiceDomainName resolves the OpenStack domain, defaulting to the
// ControlPlane's effective admin domain.
func keystoneServiceDomainName(cp *c5c3v1alpha1.ControlPlane, ks *c5c3v1alpha1.KeystoneService) string {
	if ks.Spec.Account == nil {
		return adminDomainName(cp)
	}
	return cmp.Or(ks.Spec.Account.DomainName, adminDomainName(cp))
}

// keystoneServiceCatalogName resolves the catalog service name, defaulting to the
// CR's own name rather than to K-ORC's default (the child CR's name), which
// carries this CR's uniqueness suffix and would be advertised in the catalog.
func keystoneServiceCatalogName(ks *c5c3v1alpha1.KeystoneService) string {
	if ks.Spec.Catalog == nil {
		return ""
	}
	return cmp.Or(ks.Spec.Catalog.ServiceName, ks.Name)
}

// keystoneServiceRemoteKeyFor returns the per-CR, namespace-scoped OpenBao path
// the account's credentials are mirrored to.
//
// The namespace segment is the CR's own, which is what the templated eso-tenant
// policy admits (openstack/keystone/{caller-namespace}/+/service-accounts/+): the
// CR's name takes the per-CR segment the ControlPlane's inline accounts give to
// the ControlPlane name, and the fixed "credentials" leaf keeps the two
// mechanisms' paths disjoint while both exist.
func keystoneServiceRemoteKeyFor(ks *c5c3v1alpha1.KeystoneService) string {
	return "openstack/keystone/" + ks.Namespace + "/" + ks.Name + "/service-accounts/credentials"
}

// ownsKeystoneServiceChild reports whether obj is a child this CR created. BOTH
// the controller reference and the name test must pass, so a foreign object
// sharing the namespace can never be reshaped or swept. The name test admits the
// unprefixed consumer-credentials name alongside the prefix, since that one child
// is deliberately named for its consumers.
func ownsKeystoneServiceChild(ks *c5c3v1alpha1.KeystoneService, obj client.Object) bool {
	if !metav1.IsControlledBy(obj, ks) {
		return false
	}
	name := obj.GetName()
	return strings.HasPrefix(name, keystoneServiceChildPrefix(ks)) || name == keystoneServiceCredentialsSecretName(ks)
}

// --- prune and teardown ---

// keystoneServiceDeclaredChildNames returns the non-password child names the
// declared blocks legitimately own. Password Secrets are excluded and guarded by
// prefix in sweepChildren instead: their generation lifecycle belongs to
// ensureKeystoneServiceUser (create v(N), delete superseded v(<N) once K-ORC
// applies it), and a sweep must not race it into deleting the generation the
// managed User currently references.
func keystoneServiceDeclaredChildNames(ks *c5c3v1alpha1.KeystoneService) map[string]bool {
	keep := map[string]bool{}
	if ks.Spec.Catalog != nil {
		keep[keystoneServiceCatalogServiceRef(ks)] = true
		keep[keystoneServiceCatalogServiceProbeRef(ks)] = true
		for _, ep := range ks.Spec.Catalog.Endpoints {
			keep[keystoneServiceCatalogEndpointRef(ks, ep.Interface)] = true
		}
	}
	if ks.Spec.Account != nil {
		keep[keystoneServiceUserRef(ks)] = true
		keep[keystoneServiceUserProbeRef(ks)] = true
		keep[keystoneServiceProjectRef(ks)] = true
		keep[keystoneServiceProjectProbeRef(ks)] = true
		keep[keystoneServiceChildPrefix(ks)+"domain"] = true
		keep[keystoneServiceSourceSecretName(ks)] = true
		keep[keystoneServicePushSecretName(ks)] = true
		keep[keystoneServiceCredentialsSecretName(ks)] = true
		for _, role := range ks.Spec.Account.Roles {
			keep[keystoneServiceRoleImportRef(ks, role)] = true
			keep[keystoneServiceRoleAssignmentRef(ks, role)] = true
		}
	}
	return keep
}

// sweepChildren deletes every child this CR owns whose name `keep` does not
// carry, and reports what it swept, split by the block that owns the kind.
//
// One sweep serves both callers: the per-pass prune passes the declared names,
// and the teardown passes nothing so everything goes. The split is by KIND rather
// than by name, which is exact here — Service and Endpoint are only ever catalog
// children, and User / Project / Domain / Role / RoleAssignment and the delivery
// objects are only ever account children — so a pending removal always holds the
// condition of the block that actually owns it.
//
// Deletion runs dependents-first (assignments before roles, endpoints before the
// service they reference, both before the user and project they bind). K-ORC
// enforces its own ordering through finalizers, but issuing the deletes in
// dependency order keeps the intent legible and avoids a guaranteed retry.
//
// A swept name is reported on the pass that issued its Delete, not once the
// object is gone: a K-ORC child stays Terminating behind its finalizer while the
// Keystone row is removed, so the caller holds readiness until a later pass no
// longer lists it.
func (r *KeystoneServiceReconciler) sweepChildren(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, keep map[string]bool,
) (catalogSwept, accountSwept []string, err error) {
	logger := log.FromContext(ctx)
	ns := ks.Namespace

	// A declared account's password Secrets are kept whatever their generation.
	passwordPrefix := keystoneServiceChildPrefix(ks) + "password-v"
	keepPasswords := ks.Spec.Account != nil

	sweep := func(obj client.Object, swept *[]string) error {
		name := obj.GetName()
		if !ownsKeystoneServiceChild(ks, obj) {
			return nil
		}
		if strings.HasPrefix(name, passwordPrefix) {
			if keepPasswords {
				return nil
			}
		} else if keep[name] {
			return nil
		}
		logger.Info("removing an undeclared KeystoneService child", "name", name, "namespace", ns)
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			return fmt.Errorf("deleting KeystoneService child %q: %w", name, err)
		}
		*swept = append(*swept, name)
		return nil
	}

	var assignments orcv1alpha1.RoleAssignmentList
	if err := r.List(ctx, &assignments, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing registration RoleAssignments: %w", err)
	}
	for i := range assignments.Items {
		if err := sweep(&assignments.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}
	var roles orcv1alpha1.RoleList
	if err := r.List(ctx, &roles, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Roles: %w", err)
	}
	for i := range roles.Items {
		if err := sweep(&roles.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}
	var endpoints orcv1alpha1.EndpointList
	if err := r.List(ctx, &endpoints, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Endpoints: %w", err)
	}
	for i := range endpoints.Items {
		if err := sweep(&endpoints.Items[i], &catalogSwept); err != nil {
			return nil, nil, err
		}
	}
	var services orcv1alpha1.ServiceList
	if err := r.List(ctx, &services, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Services: %w", err)
	}
	for i := range services.Items {
		if err := sweep(&services.Items[i], &catalogSwept); err != nil {
			return nil, nil, err
		}
	}
	var users orcv1alpha1.UserList
	if err := r.List(ctx, &users, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Users: %w", err)
	}
	for i := range users.Items {
		if err := sweep(&users.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}
	var projects orcv1alpha1.ProjectList
	if err := r.List(ctx, &projects, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Projects: %w", err)
	}
	for i := range projects.Items {
		if err := sweep(&projects.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}
	var domains orcv1alpha1.DomainList
	if err := r.List(ctx, &domains, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Domains: %w", err)
	}
	for i := range domains.Items {
		if err := sweep(&domains.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}

	// The delivery objects. The materialized credentials Secret is ESO-owned
	// (CreationPolicy Owner) and garbage-collected with its ExternalSecret, so it
	// carries no controller reference of ours and the ownership test correctly
	// leaves it to the ExternalSecret's deletion.
	var pushSecrets esov1alpha1.PushSecretList
	if err := r.List(ctx, &pushSecrets, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing registration PushSecrets: %w", err)
	}
	for i := range pushSecrets.Items {
		if err := sweep(&pushSecrets.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}
	var externalSecrets esov1.ExternalSecretList
	if err := r.List(ctx, &externalSecrets, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing registration ExternalSecrets: %w", err)
	}
	for i := range externalSecrets.Items {
		if err := sweep(&externalSecrets.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}
	var kubeSecrets corev1.SecretList
	if err := r.List(ctx, &kubeSecrets, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Secrets: %w", err)
	}
	for i := range kubeSecrets.Items {
		if err := sweep(&kubeSecrets.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}

	return catalogSwept, accountSwept, nil
}

// reconcileDelete tears the projected children down and releases the finalizer.
//
// With the ControlPlane present the teardown is patient: K-ORC's finalizers take
// the catalog rows and the Keystone user out of the identity plane, so the
// finalizer is held until no owned child is listed any more. There is
// deliberately NO stall escape — a child K-ORC cannot delete (a 403 on DELETE, an
// unreachable Keystone) leaves the CR visibly Terminating rather than silently
// orphaning a Keystone identity nobody is tracking any more.
//
// With the ControlPlane GONE the teardown fails open instead: K-ORC has no
// credentials left to reach Keystone with, so waiting could only hold the CR
// hostage to a plane that no longer exists. The Kubernetes children are still
// deleted — they would otherwise leak — but their outcome no longer gates the
// finalizer. This mirrors the identity-backend teardown's posture for a Keystone
// that is already gone.
func (r *KeystoneServiceReconciler) reconcileDelete(ctx context.Context, ks *c5c3v1alpha1.KeystoneService) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ks, keystoneServiceFinalizerName) {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)

	key := client.ObjectKey{
		Namespace: cmp.Or(ks.Spec.ControlPlaneRef.Namespace, ks.Namespace),
		Name:      ks.Spec.ControlPlaneRef.Name,
	}
	controlPlaneGone := false
	var cp c5c3v1alpha1.ControlPlane
	if err := r.Get(ctx, key, &cp); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("fetching ControlPlane %s during teardown: %w", key, err)
		}
		controlPlaneGone = true
	}

	catalogSwept, accountSwept, err := r.sweepChildren(ctx, ks, nil)
	if err != nil {
		if !controlPlaneGone {
			return ctrl.Result{}, err
		}
		logger.Error(err, "best-effort teardown of KeystoneService children failed; releasing the finalizer anyway",
			"controlPlane", key)
		return r.removeFinalizer(ctx, ks)
	}
	if !controlPlaneGone && len(catalogSwept)+len(accountSwept) > 0 {
		logger.V(1).Info("waiting for KeystoneService children to be removed before releasing the finalizer",
			"catalog", len(catalogSwept), "account", len(accountSwept))
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}
	if controlPlaneGone {
		logger.Info("referenced ControlPlane is gone; releasing the KeystoneService finalizer", "controlPlane", key)
	}
	return r.removeFinalizer(ctx, ks)
}

// removeFinalizer releases the teardown finalizer and persists the update.
func (r *KeystoneServiceReconciler) removeFinalizer(ctx context.Context, ks *c5c3v1alpha1.KeystoneService) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(ks, keystoneServiceFinalizerName)
	if err := r.Update(ctx, ks); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing KeystoneService finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// --- owned-object helpers ---

// ensureKeystoneServiceSecret create-or-updates an operator-owned Secret in the
// CR's namespace. It is the KeystoneService twin of ensureOwnedSecret, without
// that helper's cross-namespace claim logic: a registration only ever writes into
// its own namespace, where a controller reference is legal and the garbage
// collector reaps the Secret with the CR.
//
// The Secret's Data map is non-nil before mutate runs, so callers set only the
// keys they own, and mutate may fail the write (a value generator that errors).
// It stays read-modify-write rather than Server-Side Apply precisely because
// mutate reads the LIVE Data to preserve generated-once values across passes,
// which is not a pure projection.
func (r *KeystoneServiceReconciler) ensureKeystoneServiceSecret(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, name string, mutate func(*corev1.Secret) error,
) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ks.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if err := mutate(secret); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(ks, secret, r.Scheme)
	})
	return err
}

// forceRepushKeystoneServicePushSecret stamps a content hash on the PushSecret so
// ESO re-pushes immediately. ESO's PushSecret controller reacts only to the
// PushSecret's own metadata hash — it does not watch the source Secret — so
// without this a rotated password would not reach OpenBao until the hourly
// refresh. An unchanged hash is a no-op, so a steady-state pass writes nothing.
//
// The read-modify-write runs under RetryOnConflict: ESO mutates this object's
// status on every push, so a 409 between the Get and the Update is expected
// concurrency rather than a fault.
func (r *KeystoneServiceReconciler) forceRepushKeystoneServicePushSecret(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, name, annotation, hash string,
) error {
	key := types.NamespacedName{Namespace: ks.Namespace, Name: name}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ps := &esov1alpha1.PushSecret{}
		if err := r.Get(ctx, key, ps); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if ps.Annotations[annotation] == hash {
			return nil
		}
		if ps.Annotations == nil {
			ps.Annotations = map[string]string{}
		}
		ps.Annotations[annotation] = hash
		return r.Update(ctx, ps)
	}); err != nil {
		return fmt.Errorf("forcing PushSecret %q re-push: %w", name, err)
	}
	return nil
}

// forceSyncKeystoneServiceExternalSecret nudges ESO to re-materialize the consumer
// Secret now rather than at the next hourly refresh. ESO folds the
// ExternalSecret's annotations into its sync-decision hash, so a changed trigger
// forces a re-sync and an unchanged one is a no-op. A missing ExternalSecret is a
// no-op nil: the freshness gate is the caller's byte-compare, not this nudge.
func (r *KeystoneServiceReconciler) forceSyncKeystoneServiceExternalSecret(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, name, trigger string,
) error {
	key := types.NamespacedName{Namespace: ks.Namespace, Name: name}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		es := &esov1.ExternalSecret{}
		if err := r.Get(ctx, key, es); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if es.Annotations[esov1.AnnotationForceSync] == trigger {
			return nil
		}
		if es.Annotations == nil {
			es.Annotations = map[string]string{}
		}
		es.Annotations[esov1.AnnotationForceSync] = trigger
		return r.Update(ctx, es)
	}); err != nil {
		return fmt.Errorf("forcing ExternalSecret %q re-sync: %w", name, err)
	}
	return nil
}

// keystoneServiceStoreReady gates the account's delivery leg on the store the
// ControlPlane selected, resolved in the CR's OWN namespace: a namespaced tenant
// store is per namespace, and the credentials are delivered here. It is scoped to
// the account block on purpose — a catalog-only registration touches no secret
// machinery and must not wait for a store it never reads.
func (r *KeystoneServiceReconciler) keystoneServiceStoreReady(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
) (bool, error) {
	storeRef := effectiveControlPlaneStoreRef(cp)
	ready, err := secrets.IsStoreRefReady(ctx, r.Client, storeRef, ks.Namespace)
	if err != nil {
		return false, fmt.Errorf("checking %s %q in namespace %q: %w", storeRef.Kind, storeRef.Name, ks.Namespace, err)
	}
	return ready, nil
}

// keystoneServiceWaitOrClassify writes a bounded-wait condition, PREFERRING a
// classifiable External-mode K-ORC failure over the generic wait reason.
//
// A K-ORC import that cannot reach the external Keystone reports a non-terminal
// TransientError, so without this arm an authentication failure, a TLS error or a
// catalog mismatch would all read as "registered but not yet Available" forever,
// with nothing in the condition naming the actual cause.
func (r *KeystoneServiceReconciler) keystoneServiceWaitOrClassify(
	ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	condType, waitReason, waitMessage string, pending ...orcv1alpha1.ObjectWithConditions,
) {
	fail := keystoneServiceFail(ks, condType)
	if cp.IsExternalKeystone() {
		if reason, raw := classifyExternalKORCFailure(pending...); reason != "" {
			message := fmt.Sprintf("external Keystone at %s: %s", externalKeystoneAuthURL(cp), raw)
			if reason == conditionReasonCatalogEndpointMismatch {
				message += "; " + catalogEndpointMismatchHint(cp)
			}
			fail(reason, message)
			return
		}
	}
	fail(waitReason, waitMessage)
}
