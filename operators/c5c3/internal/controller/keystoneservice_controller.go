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
	"slices"
	"strings"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/apply"
	"github.com/c5c3/forge/internal/common/bootstrap"
	"github.com/c5c3/forge/internal/common/conditions"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	"github.com/c5c3/forge/internal/common/secrets"
	"github.com/c5c3/forge/internal/common/watch"
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

// The ownership labels a K-ORC child carries when it cannot carry an owner
// reference, mirroring controlPlaneNameLabel / controlPlaneNamespaceLabel for the
// ControlPlane's own cross-namespace children. A registration's children live
// beside the admin credential (keystoneServiceChildNamespace), which for a
// foreign registration is not the CR's namespace, and Kubernetes rejects an owner
// reference across namespaces. The pair identifies the CR uniquely: a name is
// only unique within its namespace, so both halves are required.
const (
	keystoneServiceNameLabel      = "c5c3.io/keystoneservice-name"
	keystoneServiceNamespaceLabel = "c5c3.io/keystoneservice-namespace"
)

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
	Scheme                  *runtime.Scheme
	Recorder                record.EventRecorder
	MaxConcurrentReconciles int
}

// RBAC for the KeystoneService kind itself: the controller reads the CRs and
// updates them only to install and release its teardown finalizer; it never
// creates or deletes them. Every other kind it touches (the K-ORC kinds, the
// ESO kinds, Secrets, events, ControlPlane reads) is already granted by the
// ControlPlane and CredentialRotation marker blocks, and the chart mirrors the
// deduplicated union.
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneservices,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneservices/finalizers,verbs=update

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
			"ControlPlane %s/%s does not admit service registrations from namespace %q; nothing is projected. "+
				"Add the namespace to its spec.korc.serviceRegistrations.allowedNamespaces to admit it",
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

	// Each declared block runs through the shared instrumenter, so its duration
	// and errors land on the c5c3_operator metric vectors under its own
	// sub_reconciler label (see subReconcilerConditionTypes).
	requeue := false
	if ks.Spec.Catalog != nil {
		res, err := instrumenter.Instrument(ctx, "KeystoneServiceCatalog", func(ctx context.Context) (ctrl.Result, error) {
			return r.ensureCatalog(ctx, ks, cp, credRef, managedCredRef)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue = requeue || !res.IsZero()
	}
	if ks.Spec.Account != nil {
		res, err := instrumenter.Instrument(ctx, "KeystoneServiceAccount", func(ctx context.Context) (ctrl.Result, error) {
			return r.ensureAccount(ctx, ks, cp, credRef, managedCredRef)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue = requeue || !res.IsZero()
	}

	// Prune whatever the spec stopped declaring — a removed role, a removed
	// endpoint interface, a removed block — and hold the owning block's condition
	// until the removal completes.
	catalogSwept, accountSwept, err := r.sweepChildren(ctx, ks,
		keystoneServiceChildNamespace(cp), keystoneServiceDeclaredChildNames(ks))
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
// Consent matters because a KeystoneService can mint a service user with
// arbitrary roles: admitting a namespace by default would turn namespace access
// into cloud admin. Three layers grant it, in widening order.
//
// The ControlPlane's OWN namespace and its DEDICATED service namespaces are
// admitted implicitly. Both are already the ControlPlane's: it declares the
// dedicated ones itself through the services' namespace blocks and provisions a
// tenant store in each, which is the same reasoning that lets an inline service
// account deliver its consumer Secret there.
//
// Every OTHER namespace needs an explicit entry in
// spec.korc.serviceRegistrations.allowedNamespaces. A nil block and an empty list
// are therefore identical: both leave the implicit two layers and admit nothing
// beyond them.
func keystoneServiceNamespaceAllowed(cp *c5c3v1alpha1.ControlPlane, ks *c5c3v1alpha1.KeystoneService) bool {
	if ks.Namespace == cp.Namespace {
		return true
	}
	for _, ns := range cp.DedicatedServiceNamespaces() {
		if ns.Name == ks.Namespace {
			return true
		}
	}
	if sr := cp.Spec.KORC.ServiceRegistrations; sr != nil {
		return slices.Contains(sr.AllowedNamespaces, ks.Namespace)
	}
	return false
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

// keystoneServiceChildNamespace is where a registration's K-ORC children and the
// password Secret they resolve live: the ControlPlane's namespace, never the
// registration's.
//
// K-ORC reads the clouds.yaml named by a child's cloudCredentialsRef from the
// CHILD's own namespace, and resolves a domainRef there too. The admin credential
// the operator authenticates every registration with is materialized once, in the
// ControlPlane's namespace, and deliberately stays there: copying it into each
// allowlisted namespace would hand every tenant the cloud-admin credential and
// undo the escalation the registration allowlist exists to prevent. So the
// children go to the credential rather than the credential to the children —
// which is what the inline service accounts have always done (childNamespace in
// reconcile_serviceaccounts.go).
//
// Only the DELIVERY objects stay in the registration's namespace: the assembled
// source Secret, the PushSecret that pushes it through that namespace's tenant
// store, the ExternalSecret, and the consumer Secret the service reads.
func keystoneServiceChildNamespace(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Namespace
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

// keystoneServiceCredentialsSecretSuffix is the fixed tail of the consumer
// Secret's name. Defined once so the name builder and the consumer-Secret watch
// mapper cannot disagree on the contract.
const keystoneServiceCredentialsSecretSuffix = "-credentials"

// keystoneServiceCredentialsSecretName is the consumer-facing handle, and the one
// child name deliberately NOT carrying the internal prefix: it is the documented
// contract a service reads its credentials from, so it stays predictable from the
// CR's name alone.
func keystoneServiceCredentialsSecretName(ks *c5c3v1alpha1.KeystoneService) string {
	return ks.Name + keystoneServiceCredentialsSecretSuffix
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

// keystoneServiceChildLabels returns the ownership labels identifying ks as the
// owner of a child it cannot owner-reference.
func keystoneServiceChildLabels(ks *c5c3v1alpha1.KeystoneService) map[string]string {
	return map[string]string{
		keystoneServiceNameLabel:      ks.Name,
		keystoneServiceNamespaceLabel: ks.Namespace,
	}
}

// stampKeystoneServiceChildLabels merges ks's ownership labels onto obj,
// preserving anything already there, so the child is recognizable the moment it
// exists rather than after a later pass.
func stampKeystoneServiceChildLabels(obj client.Object, ks *c5c3v1alpha1.KeystoneService) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range keystoneServiceChildLabels(ks) {
		labels[k] = v
	}
	obj.SetLabels(labels)
}

// claimKeystoneServiceChild makes obj a child of ks by the only mechanism its
// namespace permits: a controller owner reference when it shares the CR's
// namespace, so the garbage collector reaps it, and the ownership labels when it
// does not, so the finalizer-driven teardown can still find it. It mirrors
// claimChildOwnership, minus the target-cluster arm a registration never has: a
// registration delivers on the management cluster by design.
func claimKeystoneServiceChild(ks *c5c3v1alpha1.KeystoneService, obj client.Object, scheme *runtime.Scheme) error {
	stampKeystoneServiceChildLabels(obj, ks)
	if obj.GetNamespace() != ks.Namespace {
		return nil
	}
	return controllerutil.SetControllerReference(ks, obj, scheme)
}

// ensureKeystoneServiceChild is the Server-Side-Apply twin of
// claimKeystoneServiceChild, and the single write path for every child a
// registration projects.
//
// A child that lands outside the CR's namespace cannot be owner-referenced, so
// before adopting a name that already exists there the apply refuses anything
// this registration did not create: the apply would otherwise overwrite a
// stranger's spec and the teardown would then delete it. The pre-check reads
// through the cached client rather than an uncached reader, unlike the
// ControlPlane's ensureUnownedOrOwned: a registration's child names carry a
// digest of the CR's identity, so a name collision with an object somebody else
// authored is not a race to lose but a name nobody would write.
func (r *KeystoneServiceReconciler) ensureKeystoneServiceChild(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, obj client.Object,
) error {
	if obj.GetNamespace() != ks.Namespace {
		live := obj.DeepCopyObject().(client.Object)
		switch err := r.Get(ctx, client.ObjectKeyFromObject(obj), live); {
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		case err != nil:
			return fmt.Errorf("checking for a pre-existing %T %s before adopting it: %w",
				obj, client.ObjectKeyFromObject(obj), err)
		default:
			if !isKeystoneServiceChild(live, ks) {
				return fmt.Errorf("refusing to adopt pre-existing %T %s: it was not created by this "+
					"registration, so adopting it would overwrite its spec and delete it at teardown",
					obj, client.ObjectKeyFromObject(obj))
			}
		}
	}
	if err := claimKeystoneServiceChild(ks, obj, r.Scheme); err != nil {
		return err
	}
	return apply.EnsureUnownedObject(ctx, r.Client, r.Scheme, obj, apply.FieldManager)
}

// keystoneServiceEnsure returns the ensure seam the shared projection helpers
// take, bound to ks: every child they build is written through
// ensureKeystoneServiceChild, so ownership follows the namespace it lands in.
func (r *KeystoneServiceReconciler) keystoneServiceEnsure(ks *c5c3v1alpha1.KeystoneService) registrationEnsure {
	return func(ctx context.Context, obj client.Object) error {
		return r.ensureKeystoneServiceChild(ctx, ks, obj)
	}
}

// isKeystoneServiceChild reports whether ks owns obj: either it is the controller
// owner reference (the same-namespace case) or obj carries ks's ownership labels
// (the cross-namespace case, where no owner reference is possible).
func isKeystoneServiceChild(obj client.Object, ks *c5c3v1alpha1.KeystoneService) bool {
	if metav1.IsControlledBy(obj, ks) {
		return true
	}
	labels := obj.GetLabels()
	return labels[keystoneServiceNameLabel] == ks.Name &&
		labels[keystoneServiceNamespaceLabel] == ks.Namespace
}

// ownsKeystoneServiceChild reports whether obj is a child this CR created. BOTH
// the ownership test and the name test must pass, so a foreign object sharing the
// namespace can never be reshaped or swept — which matters more since the K-ORC
// children moved to the ControlPlane's namespace, where they sit beside the
// plane's own children and those of every other registration. The name test
// admits the unprefixed consumer-credentials name alongside the prefix, since
// that one child is deliberately named for its consumers.
func ownsKeystoneServiceChild(ks *c5c3v1alpha1.KeystoneService, obj client.Object) bool {
	if !isKeystoneServiceChild(obj, ks) {
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
// childNS is where the K-ORC children and the password Secrets live (the
// ControlPlane's namespace, see keystoneServiceChildNamespace); the delivery
// objects are looked for in the CR's own namespace. The two are the same
// namespace for a co-located registration, and the Secret sweep visits each of
// them once.
func (r *KeystoneServiceReconciler) sweepChildren(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, childNS string, keep map[string]bool,
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
		logger.Info("removing an undeclared KeystoneService child",
			"name", name, "namespace", obj.GetNamespace())
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			return fmt.Errorf("deleting KeystoneService child %q: %w", name, err)
		}
		*swept = append(*swept, name)
		return nil
	}

	var assignments orcv1alpha1.RoleAssignmentList
	if err := r.List(ctx, &assignments, client.InNamespace(childNS)); err != nil {
		return nil, nil, fmt.Errorf("listing registration RoleAssignments: %w", err)
	}
	for i := range assignments.Items {
		if err := sweep(&assignments.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}
	var roles orcv1alpha1.RoleList
	if err := r.List(ctx, &roles, client.InNamespace(childNS)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Roles: %w", err)
	}
	for i := range roles.Items {
		if err := sweep(&roles.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}
	var endpoints orcv1alpha1.EndpointList
	if err := r.List(ctx, &endpoints, client.InNamespace(childNS)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Endpoints: %w", err)
	}
	for i := range endpoints.Items {
		if err := sweep(&endpoints.Items[i], &catalogSwept); err != nil {
			return nil, nil, err
		}
	}
	var services orcv1alpha1.ServiceList
	if err := r.List(ctx, &services, client.InNamespace(childNS)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Services: %w", err)
	}
	for i := range services.Items {
		if err := sweep(&services.Items[i], &catalogSwept); err != nil {
			return nil, nil, err
		}
	}
	var users orcv1alpha1.UserList
	if err := r.List(ctx, &users, client.InNamespace(childNS)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Users: %w", err)
	}
	for i := range users.Items {
		if err := sweep(&users.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}
	var projects orcv1alpha1.ProjectList
	if err := r.List(ctx, &projects, client.InNamespace(childNS)); err != nil {
		return nil, nil, fmt.Errorf("listing registration Projects: %w", err)
	}
	for i := range projects.Items {
		if err := sweep(&projects.Items[i], &accountSwept); err != nil {
			return nil, nil, err
		}
	}
	var domains orcv1alpha1.DomainList
	if err := r.List(ctx, &domains, client.InNamespace(childNS)); err != nil {
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
	// Secrets straddle both namespaces: the generation-scoped password Secret sits
	// with the User that resolves it, the source and consumer Secrets with the
	// service that reads them. A co-located registration has one namespace, so the
	// second visit is skipped rather than listing the same Secrets twice.
	secretNamespaces := []string{ns}
	if childNS != ns {
		secretNamespaces = append(secretNamespaces, childNS)
	}
	for _, secretNS := range secretNamespaces {
		var kubeSecrets corev1.SecretList
		if err := r.List(ctx, &kubeSecrets, client.InNamespace(secretNS)); err != nil {
			return nil, nil, fmt.Errorf("listing registration Secrets in namespace %q: %w", secretNS, err)
		}
		for i := range kubeSecrets.Items {
			if err := sweep(&kubeSecrets.Items[i], &accountSwept); err != nil {
				return nil, nil, err
			}
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

	// key.Namespace is where the children are, whether or not the plane itself is
	// still there to be read: it is the resolved controlPlaneRef namespace, which
	// is what keystoneServiceChildNamespace returns for a live one.
	catalogSwept, accountSwept, err := r.sweepChildren(ctx, ks, key.Namespace, nil)
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
// The namespace is explicit because the two Secrets it writes no longer share
// one: the generation-scoped password Secret belongs beside the User that
// resolves it, the assembled source Secret beside the service that consumes it.
// Ownership follows the namespace through claimKeystoneServiceChild.
func (r *KeystoneServiceReconciler) ensureKeystoneServiceSecret(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, name, namespace string,
	mutate func(*corev1.Secret) error,
) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if err := mutate(secret); err != nil {
			return err
		}
		return claimKeystoneServiceChild(ks, secret, r.Scheme)
	})
	return err
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

// --- manager wiring ---

// KeystoneServiceControlPlaneRefIndexKey is the field-indexer key under which a
// KeystoneService is indexed by its RESOLVED ControlPlane reference, as the
// namespace-qualified "<namespace>/<name>". The qualification matters twice: the
// ControlPlane watch must not wake registrations referencing a same-named
// ControlPlane in another namespace, and it is what lets the mapper's List stay
// cluster-wide now that the allowlist admits foreign namespaces.
const KeystoneServiceControlPlaneRefIndexKey = "spec.controlPlaneRef"

// keystoneServiceControlPlaneRefExtractor returns the resolved
// "<namespace>/<name>" of obj's ControlPlane reference, the namespace defaulting
// to the CR's own exactly as resolveControlPlane resolves it. An empty name (a
// CR that bypassed admission) indexes nothing rather than a dangling
// "<namespace>/" key.
func keystoneServiceControlPlaneRefExtractor(obj client.Object) []string {
	ks, ok := obj.(*c5c3v1alpha1.KeystoneService)
	if !ok || ks.Spec.ControlPlaneRef.Name == "" {
		return nil
	}
	return []string{cmp.Or(ks.Spec.ControlPlaneRef.Namespace, ks.Namespace) + "/" + ks.Spec.ControlPlaneRef.Name}
}

// registerKeystoneServiceControlPlaneRefIndex registers the field indexer
// controlPlaneToKeystoneServicesMapper relies on for its MatchingFields lookup.
// setupWithOptions calls it before the watch legs are wired, mirroring
// registerControlPlaneSecretNameIndex.
func registerKeystoneServiceControlPlaneRefIndex(ctx context.Context, indexer client.FieldIndexer) error {
	return indexer.IndexField(ctx, &c5c3v1alpha1.KeystoneService{},
		KeystoneServiceControlPlaneRefIndexKey, keystoneServiceControlPlaneRefExtractor)
}

// controlPlaneToKeystoneServicesMapper maps a ControlPlane event to reconcile
// requests for every KeystoneService referencing it, resolved through the
// KeystoneServiceControlPlaneRefIndexKey index. The List is cluster-wide on
// purpose: a registration may sit in any namespace the allowlist admits, and the
// namespace-qualified index key already scopes the match. A
// List failure is logged and maps to nothing — a MapFunc cannot return an error,
// and the korcRequeueAfter polls are the fallback.
func controlPlaneToKeystoneServicesMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var registrations c5c3v1alpha1.KeystoneServiceList
		if err := c.List(ctx, &registrations, client.MatchingFields{
			KeystoneServiceControlPlaneRefIndexKey: obj.GetNamespace() + "/" + obj.GetName(),
		}); err != nil {
			log.FromContext(ctx).Error(err, "listing KeystoneServices for ControlPlane watch")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(registrations.Items))
		for i := range registrations.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&registrations.Items[i]),
			})
		}
		return requests
	}
}

// credentialsSecretToKeystoneServiceMapper maps a Secret event to the
// KeystoneService whose consumer Secret it is. The consumer Secret is ESO-owned
// (CreationPolicy: Owner), so no owner reference points at the CR and the Owns
// legs never fire for it; the <cr-name>-credentials name contract is what maps
// it back. A Secret without the suffix maps to nothing without a read, and a
// trimmed name naming no KeystoneService is somebody else's Secret.
func credentialsSecretToKeystoneServiceMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		name, found := strings.CutSuffix(obj.GetName(), keystoneServiceCredentialsSecretSuffix)
		if !found || name == "" {
			return nil
		}
		key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: name}
		var ks c5c3v1alpha1.KeystoneService
		if err := c.Get(ctx, key, &ks); err != nil {
			if !apierrors.IsNotFound(err) {
				log.FromContext(ctx).Error(err, "fetching KeystoneService for consumer-Secret watch", "keystoneService", key)
			}
			return nil
		}
		return []reconcile.Request{{NamespacedName: key}}
	}
}

// keystoneServiceChildToRequest maps a projected child back to the registration
// that owns it, by the ownership labels every child carries.
//
// It replaces the Owns() legs the children used to arrive through. A child in the
// ControlPlane's namespace cannot hold an owner reference to a registration in
// another namespace, so ownership is expressed as labels and the mapping has to
// read them. It needs no client: the labels carry the whole key.
//
// An object without both labels belongs to something else — the ControlPlane's
// own inline children share these namespaces — and maps to nothing.
func keystoneServiceChildToRequest(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	name, namespace := labels[keystoneServiceNameLabel], labels[keystoneServiceNamespaceLabel]
	if name == "" || namespace == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: namespace, Name: name}}}
}

// SetupWithManager registers the KeystoneServiceReconciler with the manager.
func (r *KeystoneServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.ControllerOptions(r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production wiring SetupWithManager applies. The
// controller options are a parameter so the integration suite can register this
// exact chain with SkipNameValidation set, rather than a hand-built copy of it
// that drifts the moment a leg is added.
//
// The watch surface, beyond the CR itself:
//   - Owns: every same-namespace child the projections create with a controller
//     reference — the seven K-ORC kinds, the ESO delivery pair, and the
//     operator-written password/source Secrets.
//   - ControlPlane, WITHOUT a predicate: the AdminCredentialReady flip the
//     shared gate waits on and the allowlist edits that admit or de-list a
//     namespace both arrive as ControlPlane updates, and both must re-enqueue
//     the registrations at watch latency rather than at the next poll.
//   - Secret, through the consumer-Secret mapper: the materialized credentials
//     Secret is ESO-owned, so only its name contract maps it back to the CR.
func (r *KeystoneServiceReconciler) setupWithOptions(mgr ctrl.Manager, opts crcontroller.Options) error {
	// Register the field indexer before the watch legs so
	// controlPlaneToKeystoneServicesMapper can rely on it for its
	// MatchingFields lookup.
	if err := registerKeystoneServiceControlPlaneRefIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(opts).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&c5c3v1alpha1.KeystoneService{}, builder.WithPredicates(watch.CRUpdatePredicate())).
		// The K-ORC children and the password Secret live in the ControlPlane's
		// namespace, so they carry the ownership labels instead of an owner
		// reference and Owns() would never see them. The label mapper reaches both
		// placements: a co-located child is stamped with the same labels on top of
		// its owner reference, so one leg per kind covers every registration.
		Watches(&orcv1alpha1.Service{}, handler.EnqueueRequestsFromMapFunc(keystoneServiceChildToRequest)).
		Watches(&orcv1alpha1.Endpoint{}, handler.EnqueueRequestsFromMapFunc(keystoneServiceChildToRequest)).
		Watches(&orcv1alpha1.User{}, handler.EnqueueRequestsFromMapFunc(keystoneServiceChildToRequest)).
		Watches(&orcv1alpha1.Domain{}, handler.EnqueueRequestsFromMapFunc(keystoneServiceChildToRequest)).
		Watches(&orcv1alpha1.Project{}, handler.EnqueueRequestsFromMapFunc(keystoneServiceChildToRequest)).
		Watches(&orcv1alpha1.Role{}, handler.EnqueueRequestsFromMapFunc(keystoneServiceChildToRequest)).
		Watches(&orcv1alpha1.RoleAssignment{}, handler.EnqueueRequestsFromMapFunc(keystoneServiceChildToRequest)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(keystoneServiceChildToRequest)).
		// The delivery objects stay in the CR's own namespace, where an owner
		// reference is legal and the garbage collector reaps them.
		Owns(&esov1.ExternalSecret{}).
		Owns(&esov1alpha1.PushSecret{}).
		Watches(&c5c3v1alpha1.ControlPlane{}, handler.EnqueueRequestsFromMapFunc(
			controlPlaneToKeystoneServicesMapper(mgr.GetClient()),
		)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			credentialsSecretToKeystoneServiceMapper(mgr.GetClient()),
		)).
		Complete(r)
}
