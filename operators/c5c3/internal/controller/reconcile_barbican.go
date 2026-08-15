// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// The projected Barbican CR is named "{controlplane.Name}-barbican", the same
// deterministic, collision-free naming convention as the Keystone, Horizon,
// Glance, and Placement children (see keystoneNameSuffix), and lives in
// cp.BarbicanNamespace(): the ControlPlane's own namespace by default, or the one
// services.barbican.namespace assigns. Beside it sits exactly one
// BarbicanSecretStore, "{barbican}-store", which attaches the key manager to the
// OpenBao server holding its secret material.
const barbicanNameSuffix = "-barbican"

// defaultBarbicanRepository is the canonical Barbican image repository; the tag is
// derived from spec.openStackRelease unless spec.services.barbican.image overrides
// the whole image reference.
const defaultBarbicanRepository = "ghcr.io/c5c3/barbican"

// barbicanDeletionAllowedAnnotation, when set to a truthy value on a ControlPlane,
// opts that ControlPlane in to tearing down a previously-projected Barbican child
// (its secret store, its catalog rows, and its DB-credential ExternalSecret with
// it) when spec.services.barbican is unset. The preserve-by-default posture
// mirrors the Keystone/Horizon/Glance/Placement annotations for a consistent
// operator UX: an accidental block drop never silently removes a running service.
//
// It does NOT reach the dedicated OpenBao instance holding the stored secrets —
// see barbicanStoreDataDeletionAllowedAnnotation.
const barbicanDeletionAllowedAnnotation = "c5c3.io/allow-barbican-deletion"

// barbicanStoreDataDeletionAllowedAnnotation is the SECOND opt-in, required on
// top of barbicanDeletionAllowedAnnotation before the teardown touches the
// dedicated OpenBao ensemble.
//
// It is separate because the two annotations authorise different things. For
// Keystone, Horizon, Glance and Placement the worst case behind the shared
// annotation is a redeployable stateless workload; for the Barbican child it also
// drops the schema indexing the stored secrets, which deleteOrphanedBarbican
// spells out. Here the instance carries DeletionPolicy DeletePVCs, so deleting it
// wipes the raft volume; the teardown also removes the static-seal Secret, so even a
// recovered volume is unreadable. There is no snapshot, no soft delete and no
// grace period in between: every secret Barbican ever stored — the private keys
// and certificates the service exists to hold for its tenants — is gone the moment
// the delete lands. A single boolean shared with four stateless services must not
// be what authorises that.
//
// Without it the instance, its PVC, its seal key and the rest of the ensemble are
// preserved, and BarbicanReady says so. They are still torn down with the
// ControlPlane itself, which is an explicit destructive act on the whole control
// plane rather than the side effect of dropping one service block.
const barbicanStoreDataDeletionAllowedAnnotation = "c5c3.io/allow-barbican-secret-store-data-deletion" //nolint:gosec // G101 false positive: annotation key, not a credential.

// defaultBarbicanDatabaseName is the logical database name the Barbican schema
// always lives in, regardless of whether Barbican shares the ControlPlane's
// database cluster or takes a dedicated one. It is fixed rather than derived: the
// pre-wired OpenBao bootstrap grants the dynamic Barbican credential access to the
// schema "barbican" alone, so any other name would be issued a credential it
// cannot use.
const defaultBarbicanDatabaseName = "barbican"

// defaultBarbicanKVMountpoint mirrors c5c3v1alpha1.DefaultBarbicanKVMountpoint,
// the KV v2 mount barbican's secret material lives under. A dedicated store is
// pinned to it by the BarbicanSecretStore CRD (the self-init contract provisions
// only the barbican/ mount), and an external store that leaves
// services.barbican.secretStore.external.kvMountpoint empty falls back to it.
// Projecting the value rather than leaving it absent keeps the desired store
// comparable with the live one, whose empty mount the CRD default has already
// filled.
const defaultBarbicanKVMountpoint = c5c3v1alpha1.DefaultBarbicanKVMountpoint

// barbicanSecretStoreNameSuffix is appended to barbicanName(cp) to name the one
// BarbicanSecretStore the ControlPlane projects. The prune sweep matches projected
// stores by the barbicanName(cp)+"-" prefix, so the suffix must stay under it.
const barbicanSecretStoreNameSuffix = "-store"

// barbicanServiceAccountName mirrors c5c3v1alpha1.BarbicanServiceAccountName, the
// single source of truth for the spec.korc.serviceAccounts entry the Barbican
// projection consumes for its Keystone service user. The defaulting webhook
// auto-injects it (name barbican, project service-barbican (create), roles
// [service]) whenever services.barbican is set.
const barbicanServiceAccountName = c5c3v1alpha1.BarbicanServiceAccountName

// barbicanName returns the deterministic name of the Barbican CR projected from
// the given ControlPlane (see barbicanNameSuffix).
func barbicanName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + barbicanNameSuffix
}

// barbicanSecretStoreName returns the deterministic name of the
// BarbicanSecretStore projected for this ControlPlane's Barbican service.
func barbicanSecretStoreName(cp *c5c3v1alpha1.ControlPlane) string {
	return barbicanName(cp) + barbicanSecretStoreNameSuffix
}

// barbicanDeletionAllowed reports whether cp opts in to deleting its projected
// Barbican child when spec.services.barbican is unset, via a truthy
// barbicanDeletionAllowedAnnotation. A missing, malformed, or non-truthy value
// means "preserve".
func barbicanDeletionAllowed(cp *c5c3v1alpha1.ControlPlane) bool {
	allowed, err := strconv.ParseBool(cp.Annotations[barbicanDeletionAllowedAnnotation])
	return err == nil && allowed
}

// barbicanStoreDataDeletionAllowed reports whether cp additionally opts in to
// destroying the dedicated OpenBao instance and everything stored in it, via a
// truthy barbicanStoreDataDeletionAllowedAnnotation. A missing, malformed, or
// non-truthy value means "preserve".
func barbicanStoreDataDeletionAllowed(cp *c5c3v1alpha1.ControlPlane) bool {
	allowed, err := strconv.ParseBool(cp.Annotations[barbicanStoreDataDeletionAllowedAnnotation])
	return err == nil && allowed
}

// barbicanKeystoneEndpoint returns the Keystone endpoint URL projected into the
// Barbican child's spec.keystoneEndpoint. Barbican validates every token
// server-side against this URL, so what the Barbican pods can reach decides it —
// which is the cluster Barbican is placed on against the one Keystone runs on,
// the rule keystoneEndpointFor holds for every service.
func barbicanKeystoneEndpoint(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneEndpointFor(cp, cp.BarbicanTargetClusterRef())
}

// barbicanEndpointURL renders the in-cluster URL of the projected Barbican API
// Service by naming convention (the Barbican API listens on 9311), the
// cross-service endpoint contract the catalog registers against.
func barbicanEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return managedServiceURL(barbicanName(cp), cp.BarbicanNamespace(), 9311, "")
}

// barbicanServiceAccount returns the auto-injected "barbican" entry from
// spec.korc.serviceAccounts and whether it is declared. The Barbican projection
// derives its Keystone service user from this entry.
func barbicanServiceAccount(cp *c5c3v1alpha1.ControlPlane) (c5c3v1alpha1.ServiceAccountSpec, bool) {
	for _, sa := range cp.Spec.KORC.ServiceAccounts {
		if sa.Name == barbicanServiceAccountName {
			return sa, true
		}
	}
	return c5c3v1alpha1.ServiceAccountSpec{}, false
}

// barbicanServiceAccountReady reports whether the per-account status entry for the
// barbican service account is Ready, the readiness reconcileServiceAccounts
// computes for it in the same reconcile pass, ahead of this sub-reconciler.
func barbicanServiceAccountReady(cp *c5c3v1alpha1.ControlPlane) bool {
	for _, s := range cp.Status.ServiceAccounts {
		if s.Name == barbicanServiceAccountName {
			return s.Ready
		}
	}
	return false
}

// barbicanSecretStoreDedicated reports whether the Barbican service stores its
// secret material in the OpenBao instance the ControlPlane provisions for it,
// rather than in a server run outside this control plane. The CRD's union rule
// admits exactly one of the two blocks, so reading the external one is enough; a
// block naming neither resolves to dedicated, where the ensemble the reconciler
// projects and the store it points at describe the same server.
func barbicanSecretStoreDedicated(cp *c5c3v1alpha1.ControlPlane) bool {
	return cp.Spec.Services.Barbican.SecretStore.External == nil
}

// reconcileBarbican projects spec.services.barbican into an owned Barbican CR plus
// its BarbicanSecretStore, and drives the BarbicanReady condition.
//
// The sub-reconciler is GATED on KeystoneReady (Barbican validates every token
// against the ControlPlane's Keystone child) and on the barbican service account
// (Barbican authenticates as that Keystone user). Once gated through, it ensures
// the DB-credential ExternalSecret, provisions the dedicated OpenBao instance when
// the service takes one, attaches the secret store, projects the Barbican CR
// (database/cache DeepCopied from the resolved backing services, the Keystone
// endpoint derived top-down via barbicanKeystoneEndpoint), and mirrors the child's
// Ready condition back into BarbicanReady.
func (r *ControlPlaneReconciler) reconcileBarbican(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// spec.services.barbican is optional. When unset, this ControlPlane manages no
	// key manager and reports BarbicanReady as not-managed so the aggregate Ready
	// condition is not blocked (staged adoption). A previously-projected child is
	// preserved unless the ControlPlane opts in to deletion.
	if cp.Spec.Services.Barbican == nil {
		message := "spec.services.barbican is unset; no Barbican key manager is managed by this ControlPlane"
		result := ctrl.Result{}
		if barbicanDeletionAllowed(cp) {
			res, err := r.deleteOrphanedBarbican(ctx, cp)
			if err != nil {
				return ctrl.Result{}, err
			}
			result = res
			if !barbicanStoreDataDeletionAllowed(cp) {
				message += fmt.Sprintf("; the dedicated OpenBao instance holding its stored secrets is preserved "+
					"(set annotation %s=true to destroy it and every secret in it along with the service)",
					barbicanStoreDataDeletionAllowedAnnotation)
			}
		} else {
			// Preserve the child, but NEVER the credential minter. A live
			// VaultDynamicSecret keeps issuing a fresh MySQL user with ALL PRIVILEGES
			// on the barbican schema at every refresh interval, indefinitely, for a
			// service this ControlPlane has been told it no longer manages: no
			// consumer, no revocation, and a BarbicanReady=True/BarbicanNotManaged
			// condition that surfaces none of it. Preserving a running service does
			// not imply preserving the generator behind its credentials, so the
			// dynamic objects come down either way.
			//
			// BarbicanNamespace() still resolves correctly here: removing a
			// services.barbican.namespace assignment is rejected by
			// validateServiceNamespacesImmutable, so the only admissible way to reach
			// this branch with a live generator is the co-located one, where the
			// generator sits in the ControlPlane's own namespace.
			r.deleteDynamicDBCredentialObjects(ctx, cp, barbicanDBCredentialTarget(cp))
			message += fmt.Sprintf("; any previously-projected Barbican child is preserved "+
				"(set annotation %s=true to allow deletion), but its dynamic DB-credential generator is torn down",
				barbicanDeletionAllowedAnnotation)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBarbicanReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "BarbicanNotManaged",
			Message:            message,
		})
		return result, nil
	}

	// Resolve the backing services Barbican actually talks to: its own dedicated
	// instances when it opted into them, the ControlPlane-wide shared ones
	// otherwise.
	//
	// Nil-safety fail-safe. The projection DeepCopies these, so an unresolvable
	// instance has nothing to project and the deref below would panic. The
	// validating webhook requires spec.infrastructure outside External mode (and
	// forbids services.barbican in External mode), so this only fires for a
	// webhook-bypassed CR.
	database := effectiveBarbicanDatabase(cp)
	cache := effectiveBarbicanCache(cp)
	if database == nil || cache == nil {
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// Gate on KeystoneReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeKeystoneReady) {
		logger.Info("Keystone not ready, deferring Barbican projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBarbicanReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForKeystone",
			Message:            "KeystoneReady is not True; Barbican projection deferred",
		})
		return ctrl.Result{RequeueAfter: keystoneInfraGateRequeueAfter}, nil
	}

	// Gate on the barbican service account. Its declaration is auto-injected by the
	// defaulting webhook whenever services.barbican is set; a missing entry means
	// admission was bypassed, so fail loud with a named field rather than projecting
	// a Barbican with no Keystone user. A declared-but-not-yet-Ready account defers
	// projection until its user and password are provisioned.
	sa, declared := barbicanServiceAccount(cp)
	if !declared {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBarbicanReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ServiceAccountNotDeclared",
			Message: fmt.Sprintf("no spec.korc.serviceAccounts entry named %q is declared; the defaulting webhook "+
				"injects it whenever services.barbican is set, so a ControlPlane reaching here bypassed admission",
				barbicanServiceAccountName),
		})
		return ctrl.Result{}, nil
	}
	if !barbicanServiceAccountReady(cp) {
		logger.Info("barbican service account not ready, deferring Barbican projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBarbicanReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForServiceAccount",
			Message: fmt.Sprintf("service account %q is not yet Ready; Barbican projection deferred until its "+
				"Keystone user and password are provisioned", barbicanServiceAccountName),
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// The EFFECTIVE credentials mode of the database Barbican connects to, resolved
	// once so the credential projection below, its readiness gate, and the mode
	// stamped onto the child further down can never disagree.
	dynamic := database.ClusterRef != nil && barbicanDBCredentialsDynamicEnabled(cp)

	// Ensure the DB-credential objects BEFORE the child so the Secret it references
	// exists when the barbican-operator resolves it. Managed only: a brownfield
	// database (ClusterRef nil) carries a user-supplied credential out-of-band, so
	// there is nothing for the operator to project. In Dynamic mode the shared
	// helper also holds the projection until an engine-issued credential has landed
	// (see ensureServiceDBCredential).
	if database.ClusterRef != nil {
		res, halt, err := r.ensureServiceDBCredential(ctx, cp, barbicanDBCredentialTarget(cp),
			dynamic, "Barbican", conditionTypeBarbicanReady)
		if halt {
			return res, err
		}
	}

	barbicanNS := cp.BarbicanNamespace()

	// Provision the dedicated OpenBao instance the store will point at. It is the
	// server Barbican's own secret material lives in, so the store (and with it the
	// child) waits until the instance serves requests: a store attached to an
	// instance that is still initialising reports ProvisioningDenied and would have
	// to be re-driven from a failure state. An external store addresses a server run
	// outside this control plane, so nothing is provisioned for it.
	if barbicanSecretStoreDedicated(cp) {
		// The ensemble rides the Barbican service's own cluster, so its cluster is
		// resolved before the first object is written. An external store resolves
		// nothing: it names a server this ControlPlane does not provision, and an
		// unresolvable target cluster must not park a Barbican that has no ensemble
		// on it.
		children, err := r.childrenClientFor(ctx, cp, barbicanNS)
		if err != nil {
			conditionFailer(cp, conditionTypeBarbicanReady)(commonmulticluster.TargetClusterUnavailable, err.Error())
			return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
		}

		available, err := r.ensureBarbicanOpenBao(ctx, children, cp)
		if err != nil {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeBarbicanReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "BarbicanOpenBaoError",
				Message:            fmt.Sprintf("provisioning the dedicated OpenBao instance: %v", err),
			})
			return ctrl.Result{}, err
		}
		if !available {
			logger.Info("dedicated OpenBao instance not yet available, deferring Barbican projection")
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeBarbicanReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "WaitingForOpenBaoInstance",
				Message: fmt.Sprintf("the dedicated OpenBao instance %q is not yet Available; the Barbican secret "+
					"store and child are projected once it serves requests", barbicanOpenBaoName(cp)),
			})
			return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
		}
	}

	// Attach the secret store. It references its Barbican by name (inverted
	// attachment), so it can be applied before the child exists.
	recreating, err := r.reconcileBarbicanSecretStore(ctx, cp, barbicanNS)
	if err != nil {
		reason := "BarbicanSecretStoreError"
		message := fmt.Sprintf("projecting the BarbicanSecretStore child: %v", err)
		if apierrors.IsInvalid(err) {
			reason = "BarbicanSecretStoreProjectionRejected"
			message = fmt.Sprintf("the Barbican API server rejected the projected BarbicanSecretStore; reconcile "+
				"services.barbican.secretStore to a valid projection to recover: %v", err)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBarbicanReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             reason,
			Message:            message,
		})
		return ctrl.Result{}, err
	}
	if recreating {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBarbicanReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "RecreatingBarbicanSecretStore",
			Message: fmt.Sprintf("the live BarbicanSecretStore %q differs from the projection in a field its CRD "+
				"freezes, so it was deleted and is recreated on the next pass", barbicanSecretStoreName(cp)),
		})
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// Resolve the Barbican image. spec.services.barbican.image overrides the
	// release-derived default when set.
	image := commonv1.ImageSpec{
		Repository: defaultBarbicanRepository,
		Tag:        cp.Spec.OpenStackRelease,
	}
	if override := cp.Spec.Services.Barbican.Image; override != nil {
		image = *override
	}

	// Place the child in the namespace assigned to the Barbican service (the
	// ControlPlane's own unless services.barbican.namespace says otherwise). A child
	// outside the ControlPlane's namespace can carry no owner reference, so it is
	// stamped with the ownership labels and applied unowned.
	crossNamespace := barbicanNS != cp.Namespace
	barbican := &barbicanv1alpha1.Barbican{
		ObjectMeta: metav1.ObjectMeta{
			Name:      barbicanName(cp),
			Namespace: barbicanNS,
		},
	}
	if crossNamespace {
		stampControlPlaneChildLabels(barbican, cp)
	}

	barbican.Spec.OpenStackRelease = cp.Spec.OpenStackRelease
	barbican.Spec.Image = image

	// Thread the service's target cluster onto the child verbatim; a nil source
	// yields nil and leaves the child unplaced (see the Keystone projection).
	barbican.Spec.TargetClusterRef = cp.Spec.Services.Barbican.TargetClusterRef.DeepCopy()

	// Project the merged extraConfig (globalExtraConfig unioned with the per-service
	// block, per-service winning key by key). Assigned unconditionally, following
	// the revert-on-clear convention: a nil merge keeps the SSA-applied intent free
	// of spec.extraConfig, so a direct edit on the child stays unowned until a
	// ControlPlane block is set, and clearing the ControlPlane block reverts the
	// child rather than pinning the last value.
	barbican.Spec.ExtraConfig = c5c3v1alpha1.MergedExtraConfig(
		cp.Spec.GlobalExtraConfig, cp.Spec.Services.Barbican.ExtraConfig)

	// Point Barbican at the SAME backing services the ControlPlane provisioned. The
	// logical database is always "barbican": its own schema keeps it isolated from
	// Keystone's on a shared cluster, and it is the one schema the pre-wired OpenBao
	// role can issue credentials for. DeepCopy (over a plain struct copy) is
	// required because DatabaseSpec/CacheSpec carry pointer fields, which a shallow
	// copy would alias into cp.Spec.
	barbican.Spec.Database = *database.DeepCopy()
	barbican.Spec.Database.Database = defaultBarbicanDatabaseName
	// In managed mode the operator OWNS the barbican DB credential:
	// reconcileBarbican materialises it (above) into a per-ControlPlane Secret named
	// barbicanDBCredentialSecretName(cp). Override the projected Barbican CR's
	// database.secretRef to that operator-owned Secret (key "password"), and project
	// the EFFECTIVE credentials mode: Dynamic (engine-issued) is the default on the
	// managed shared database, a per-service override or the shared Static opt-out
	// flips it to Static, and a dedicated barbican database stays Static (no engine
	// role can mint its credentials). Brownfield (ClusterRef nil) leaves the
	// user-supplied secretRef and credentialsMode in place.
	if database.ClusterRef != nil {
		barbican.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: barbicanDBCredentialSecretName(cp), Key: "password"}
		if dynamic {
			barbican.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
		} else {
			barbican.Spec.Database.CredentialsMode = commonv1.CredentialsModeStatic
		}
	}

	barbican.Spec.Cache = *cache.DeepCopy()

	// The Keystone endpoint is derived top-down from the ControlPlane rather than
	// read from the Keystone child's status; no machine consumer reads status
	// endpoints per the settled convention. keystonePublicEndpoint is the
	// browser/client-facing URL Barbican advertises on a 401 (empty when Keystone is
	// not externally exposed, in which case the child falls back to the internal
	// endpoint).
	barbican.Spec.KeystoneEndpoint = barbicanKeystoneEndpoint(cp)
	barbican.Spec.KeystonePublicEndpoint = keystonePublicEndpoint(cp.Spec.Services.Keystone)

	barbican.Spec.Region = cp.Spec.Region

	// The Keystone service user Barbican authenticates as, derived from the
	// auto-injected barbican service account: the user and project domains resolve
	// to the same effective domain the account is provisioned in, and the password
	// is read from the account's materialized consumer Secret.
	barbican.Spec.ServiceUser = barbicanv1alpha1.ServiceUserSpec{
		Username:          serviceAccountUserName(sa),
		ProjectName:       sa.Project.Name,
		UserDomainName:    serviceAccountDomainName(cp, sa),
		ProjectDomainName: serviceAccountDomainName(cp, sa),
		SecretRef:         commonv1.SecretRefSpec{Name: serviceAccountCredentialsSecretName(cp, sa), Key: "password"},
	}

	// Project the ControlPlane's RESOLVED store selection onto the Barbican child so
	// it never falls back to its own shared-cluster-store default. This is the ESO
	// store the operator routes the child's ExternalSecrets through, a separate
	// concern from the BarbicanSecretStore holding the tenant key material.
	barbican.Spec.SecretStoreRef = effectiveControlPlaneStoreRefPtr(cp)

	// DeepCopy for the same aliasing reason as Database above; a nil source yields
	// nil, clearing any previously-projected gateway so removal tears the HTTPRoute
	// down.
	barbican.Spec.Gateway = cp.Spec.Services.Barbican.Gateway.DeepCopy()

	// Resolve replicas to the shared operator default, then let an override win.
	// Assigning unconditionally means clearing services.barbican.replicas reverts
	// the child to the default instead of leaving the previously-projected value
	// pinned on the fetched child.
	barbican.Spec.Deployment.Replicas = commonv1.DefaultReplicas
	if cp.Spec.Services.Barbican.Replicas != nil {
		barbican.Spec.Deployment.Replicas = *cp.Spec.Services.Barbican.Replicas
	}

	// spec.apiServer and spec.dbClean are deliberately NOT set: the child-side uWSGI
	// parameters and the clean-up retention/schedule the barbican-operator resolves
	// at reconcile time stay authoritative, so both keep tracking the operator
	// defaults instead of being frozen into the projection.

	return commonreconcile.ProjectChild(ctx, r.Client, r.Scheme, cp,
		commonreconcile.ChildProjectionParams[*barbicanv1alpha1.Barbican]{
			Child:          barbican,
			ConditionType:  conditionTypeBarbicanReady,
			ReadyReason:    "BarbicanReady",
			ReadyMessage:   "Projected Barbican CR is ready",
			WaitingReason:  "WaitingForBarbican",
			WaitingMessage: fmt.Sprintf("Barbican %q is not ready", barbican.Name),
			// An Invalid (HTTP 422) rejection from the Barbican API server means the
			// projected spec violates a CRD/webhook rule. Surface a distinct,
			// actionable reason so the wedge is diagnosable from the condition.
			RejectedReason: "BarbicanProjectionRejected",
			RejectedMessage: func(err error) string {
				return fmt.Sprintf("Barbican API server rejected the projected spec; reconcile the ControlPlane spec "+
					"to a valid projection to recover: %v", err)
			},
			ErrorReason:     "BarbicanError",
			ErrorMessage:    func(err error) string { return fmt.Sprintf("create-or-update Barbican: %v", err) },
			WaitRequeue:     infraRequeueAfter,
			Conditions:      &cp.Status.Conditions,
			Generation:      cp.Generation,
			ChildConditions: func(b *barbicanv1alpha1.Barbican) []metav1.Condition { return b.Status.Conditions },
			Unowned:         crossNamespace,
		})
}

// barbicanSecretStoreFor builds the single BarbicanSecretStore projected from
// services.barbican.secretStore. A dedicated store names the OpenBao instance the
// ControlPlane provisions and derives everything else from it; an external store
// carries the server URL and the Secrets the barbican-operator reads its AppRole
// credentials and CA bundle from. The store is always the Barbican's default: it
// is the only one attached, and a Barbican with no default store never reaches
// Ready.
func barbicanSecretStoreFor(cp *c5c3v1alpha1.ControlPlane, namespace string) *barbicanv1alpha1.BarbicanSecretStore {
	store := &barbicanv1alpha1.BarbicanSecretStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      barbicanSecretStoreName(cp),
			Namespace: namespace,
		},
		Spec: barbicanv1alpha1.BarbicanSecretStoreSpec{
			BarbicanRef: barbicanv1alpha1.BarbicanRefSpec{Name: barbicanName(cp)},
			Type:        barbicanv1alpha1.BarbicanSecretStoreTypeOpenBao,
			IsDefault:   true,
		},
	}

	if barbicanSecretStoreDedicated(cp) {
		store.Spec.OpenBao = &barbicanv1alpha1.OpenBaoStoreSpec{
			InstanceRef:  &barbicanv1alpha1.InstanceRefSpec{Name: barbicanOpenBaoName(cp)},
			KVMountpoint: defaultBarbicanKVMountpoint,
		}
		return store
	}

	external := cp.Spec.Services.Barbican.SecretStore.External
	mountpoint := external.KVMountpoint
	if mountpoint == "" {
		mountpoint = defaultBarbicanKVMountpoint
	}
	store.Spec.OpenBao = &barbicanv1alpha1.OpenBaoStoreSpec{
		Server: &barbicanv1alpha1.OpenBaoServerSpec{
			URL: external.URL,
			// DeepCopy: the CP-side reference is a pointer into cp.Spec, which a
			// shallow copy would alias into the projected child.
			CABundleSecretRef:    external.CABundleSecretRef.DeepCopy(),
			CredentialsSecretRef: external.CredentialsSecretRef,
		},
		KVMountpoint: mountpoint,
		Namespace:    external.Namespace,
	}
	return store
}

// barbicanSecretStoreImmutableDrift reports whether live and desired differ in a
// field the BarbicanSecretStore CRD freezes with a CEL transition rule: the
// barbicanRef, the type, the store mode (instanceRef vs server), and the KV
// mountpoint. Each is frozen for the same reason: the secret material already
// written through the store is addressed by mode and by mount, so an update would
// re-point a live store at material it cannot reach, and the API server rejects
// it. The reconciler answers such a change by deleting the store instead.
func barbicanSecretStoreImmutableDrift(live, desired *barbicanv1alpha1.BarbicanSecretStore) bool {
	if live.Spec.BarbicanRef.Name != desired.Spec.BarbicanRef.Name || live.Spec.Type != desired.Spec.Type {
		return true
	}
	liveBao, desiredBao := live.Spec.OpenBao, desired.Spec.OpenBao
	if (liveBao == nil) != (desiredBao == nil) {
		return true
	}
	if liveBao == nil {
		return false
	}
	return (liveBao.InstanceRef == nil) != (desiredBao.InstanceRef == nil) ||
		liveBao.KVMountpoint != desiredBao.KVMountpoint
}

// reconcileBarbicanSecretStore projects the one BarbicanSecretStore the Barbican
// service attaches to and prunes previously-projected stores that no longer match
// the desired name. It reports whether a live store was deleted for recreation, so
// the caller can requeue instead of projecting a child against a store that is
// about to reappear.
//
// A store whose frozen fields moved (a dedicated/external mode flip, a changed KV
// mountpoint) cannot be updated: the CRD's transition rules reject the write, and
// the loop would re-attempt it on every requeue with no self-heal. Such a store is
// deleted and recreated by the next pass. The AppRole credentials the
// barbican-operator minted for it carry its ownerReference, so they are collected
// with it rather than left behind pointing at the retired mount.
//
// Every write routes through ensureUnownedOrOwned, which owner-references the store
// in the ControlPlane's own namespace and label-stamps it (unowned) in a service
// namespace, and refuses to adopt a same-named store it did not create in a
// namespace the ControlPlane does not own. The prune sweep deletes only c5c3-owned
// stores carrying the Barbican child's name prefix, so a hand-created store
// attached to the same Barbican is never pruned or overwritten.
func (r *ControlPlaneReconciler) reconcileBarbicanSecretStore(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, namespace string,
) (bool, error) {
	desired := barbicanSecretStoreFor(cp, namespace)

	live := &barbicanv1alpha1.BarbicanSecretStore{}
	switch err := r.Get(ctx, client.ObjectKeyFromObject(desired), live); {
	case apierrors.IsNotFound(err):
		// Nothing live; the apply below creates it.
	case err != nil:
		return false, fmt.Errorf("getting BarbicanSecretStore %q: %w", desired.Name, err)
	case isControlPlaneChild(live, cp) && barbicanSecretStoreImmutableDrift(live, desired):
		log.FromContext(ctx).Info("deleting BarbicanSecretStore whose immutable fields moved, recreating next pass",
			"store", client.ObjectKeyFromObject(live))
		if derr := client.IgnoreNotFound(
			r.Delete(ctx, live, client.PropagationPolicy(metav1.DeletePropagationBackground))); derr != nil {
			return false, fmt.Errorf("deleting BarbicanSecretStore %q for recreation: %w", desired.Name, derr)
		}
		return true, nil
	}

	if err := r.ensureUnownedOrOwned(ctx, r.Client, cp, desired); err != nil {
		return false, fmt.Errorf("projecting BarbicanSecretStore %q: %w", desired.Name, err)
	}

	var list barbicanv1alpha1.BarbicanSecretStoreList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return false, fmt.Errorf("listing BarbicanSecretStores for prune: %w", err)
	}
	prefix := barbicanName(cp) + "-"
	for i := range list.Items {
		store := &list.Items[i]
		if store.Name == desired.Name {
			continue
		}
		if !isControlPlaneChild(store, cp) || !strings.HasPrefix(store.Name, prefix) {
			continue
		}
		if err := client.IgnoreNotFound(
			r.Delete(ctx, store, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
			return false, fmt.Errorf("pruning undeclared BarbicanSecretStore %q: %w", store.Name, err)
		}
	}
	return false, nil
}

// deleteOrphanedBarbican removes a previously-projected Barbican child and
// everything that followed it (the secret store, the DB-credential objects, and
// the key-manager catalog K-ORC CRs) when spec.services.barbican is unset AND the
// ControlPlane has opted in to deletion via barbicanDeletionAllowedAnnotation
// (the caller gates this). The dedicated OpenBao ensemble behind the store — the
// instance, its raft PVC, and its seal key — takes the SECOND opt-in
// (barbicanStoreDataDeletionAllowedAnnotation) and is preserved without it, so
// dropping the service block can never be what destroys the stored secret
// material.
//
// Each object is only deleted when this ControlPlane still owns it (by owner
// reference in its own namespace, by the ownership labels in a service
// namespace); a foreign object colliding on a name is left alone, and an object
// already gone is tolerated.
//
// It returns a requeue while the OpenBao instance is still finalizing: the tenant
// admitting the namespace to the openbao-operator is what the instance's own
// teardown runs under, so it is removed only once the instance is gone.
func (r *ControlPlaneReconciler) deleteOrphanedBarbican(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	barbicanNS := cp.BarbicanNamespace()
	// The catalog CRs are ControlPlane-scoped and live in the ControlPlane's own
	// namespace regardless of Barbican placement (see managedCatalogService), so
	// they are swept from childNamespace(cp), not barbicanNS.
	catalogNS := childNamespace(cp)
	instanceName := barbicanOpenBaoName(cp)

	// The Dynamic-mode client Certificate has no Go type, so it is addressed
	// unstructured.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(barbicanDBCredentialClientCertName(cp))
	cert.SetNamespace(barbicanNS)

	children := []client.Object{
		// The secret store comes down before the instance it was provisioned
		// against, so its controller can still reach the server while it finalizes.
		&barbicanv1alpha1.BarbicanSecretStore{
			ObjectMeta: metav1.ObjectMeta{Name: barbicanSecretStoreName(cp), Namespace: barbicanNS},
		},
		// The Barbican child.
		&barbicanv1alpha1.Barbican{
			ObjectMeta: metav1.ObjectMeta{Name: barbicanName(cp), Namespace: barbicanNS},
		},
		// The DB-credential ExternalSecret.
		&esov1.ExternalSecret{
			ObjectMeta: metav1.ObjectMeta{Name: barbicanDBCredentialSecretName(cp), Namespace: barbicanNS},
		},
		// The Dynamic-mode DB-credential objects: the VaultDynamicSecret generator,
		// its mTLS client Certificate, and the ServiceAccount whose token it
		// authenticates with.
		&esgenv1alpha1.VaultDynamicSecret{
			ObjectMeta: metav1.ObjectMeta{Name: barbicanDBCredentialSecretName(cp), Namespace: barbicanNS},
		},
		cert,
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: barbicanDBCredentialServiceAccountName, Namespace: barbicanNS},
		},
		// The Barbican catalog K-ORC CRs: the key-manager Service and its
		// internal/public Endpoints, in catalogNS.
		&orcv1alpha1.Service{ObjectMeta: metav1.ObjectMeta{Name: barbicanCatalogServiceName(cp), Namespace: catalogNS}},
		&orcv1alpha1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{Name: barbicanCatalogEndpointName(cp, "internal"), Namespace: catalogNS},
		},
		&orcv1alpha1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{Name: barbicanCatalogEndpointName(cp, "public"), Namespace: catalogNS},
		},
	}
	for _, child := range children {
		if err := commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, child, func(live client.Object) bool {
			return isControlPlaneChild(live, cp)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The dedicated OpenBao instance and the ensemble around it are the STORED
	// SECRETS, and destroying them takes its own opt-in on top of the one that got
	// us here (see barbicanStoreDataDeletionAllowedAnnotation). Without it the
	// service is gone and the store it wrote through stands, so re-declaring
	// services.barbican reattaches to the same INSTANCE — but not to the same
	// secrets: the Barbican child deleted above finalizes its MariaDB Database CR,
	// which database.BuildDatabase leaves without a cleanupPolicy, so the
	// mariadb-operator Delete default drops the schema holding
	// secret_store_metadata and the secret rows. The payloads survive in the raft
	// volume with nothing left referencing them; only a schema dump taken before
	// the teardown makes this branch a round trip.
	if !barbicanStoreDataDeletionAllowed(cp) {
		log.FromContext(ctx).Info("preserving the dedicated OpenBao instance and its stored secrets; "+
			"set the data-deletion annotation to destroy them with the service",
			"instance", instanceName, "annotation", barbicanStoreDataDeletionAllowedAnnotation)
		return ctrl.Result{}, nil
	}

	// The instance itself. DeletionPolicy is DeletePVCs, so this is the delete that
	// wipes the raft volume.
	if err := commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client,
		&openbaov1alpha1.OpenBaoCluster{
			ObjectMeta: metav1.ObjectMeta{Name: instanceName, Namespace: barbicanNS},
		},
		func(live client.Object) bool { return isControlPlaneChild(live, cp) },
	); err != nil {
		return ctrl.Result{}, err
	}

	// The instance carries the openbao-operator's finalizer, so the delete above
	// only starts its teardown. Hold the tenant back until the instance has left
	// etcd: the tenant is what admits the namespace to the openbao-operator, and the
	// instance's own teardown runs under that admission. An instance this
	// ControlPlane does not own was never part of the teardown, so it never blocks
	// it.
	instance := &openbaov1alpha1.OpenBaoCluster{}
	switch err := r.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: barbicanNS}, instance); {
	case apierrors.IsNotFound(err):
		// Gone: the tenant that admitted the namespace can follow.
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("checking whether OpenBaoCluster %q is gone: %w", instanceName, err)
	case isControlPlaneChild(instance, cp):
		log.FromContext(ctx).Info("OpenBaoCluster still terminating, deferring the rest of the Barbican teardown",
			"instance", instanceName)
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// The rest of the ensemble. The unseal Secret is owned by the instance too, so
	// the GC cascade usually reaps it first; deleting it explicitly covers the pass
	// that created it before the instance existed. The auth-delegator binding is
	// cluster-scoped, so no namespace deletion collects it.
	for _, child := range barbicanOpenBaoEnsembleObjects(instanceName, barbicanNS) {
		if err := r.deleteOwnedEnsembleObject(ctx, cp, child); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// deleteOwnedEnsembleObject is DeleteOrphanedChildFunc's shape for the dedicated
// OpenBao ensemble: read the live object, delete it only when this ControlPlane
// owns it, tolerate an absent object and an absent CRD.
//
// It exists rather than reusing the shared helper for ONE reason: the read goes
// through the uncached reader. Three of the ensemble's kinds are RBAC (Role,
// RoleBinding, and the cluster-scoped ClusterRoleBinding), which the operator
// never watches, so a cached read would have controller-runtime start an
// unfiltered cluster-wide informer for each — see ControlPlaneReconciler.APIReader.
func (r *ControlPlaneReconciler) deleteOwnedEnsembleObject(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, obj client.Object,
) error {
	key := client.ObjectKeyFromObject(obj)
	switch err := r.apiReader().Get(ctx, key, obj); {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return nil
	case err != nil:
		return fmt.Errorf("getting %T %s for the Barbican ensemble teardown: %w", obj, key, err)
	}
	if !isControlPlaneChild(obj, cp) {
		return nil
	}
	if err := client.IgnoreNotFound(
		r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
		return fmt.Errorf("deleting %T %s: %w", obj, key, err)
	}
	return nil
}

// barbicanOpenBaoEnsembleObjects addresses the objects the dedicated OpenBao
// instance is projected with, in teardown order: the tenant that admits the
// namespace, the two transport Certificates, the provisioner ServiceAccount, its
// TokenRequest Role and RoleBinding, the static-seal Secret, and the
// cluster-scoped auth-delegator binding. The instance itself is not in the list —
// every caller sequences it separately, and one holds the tenant back with it.
func barbicanOpenBaoEnsembleObjects(name, namespace string) []client.Object {
	return []client.Object{
		&openbaov1alpha1.OpenBaoTenant{
			ObjectMeta: metav1.ObjectMeta{Name: name + barbicanOpenBaoTenantSuffix, Namespace: namespace},
		},
		barbicanOpenBaoCertificateObject(name+barbicanOpenBaoServerCertSuffix, namespace),
		barbicanOpenBaoCertificateObject(name+barbicanOpenBaoCACertSuffix, namespace),
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name + barbicanOpenBaoProvisionerSuffix, Namespace: namespace},
		},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name + barbicanOpenBaoTokenGrantSuffix, Namespace: namespace},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name + barbicanOpenBaoTokenGrantSuffix, Namespace: namespace},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + barbicanOpenBaoUnsealSecretSuffix, Namespace: namespace},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: barbicanOpenBaoAuthDelegatorName(name, namespace)},
		},
	}
}

// barbicanOpenBaoCertificateObject addresses one of the ensemble's cert-manager
// Certificates by name. They have no Go type, so the teardown names them
// unstructured, exactly as the projection creates them.
func barbicanOpenBaoCertificateObject(name, namespace string) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(name)
	cert.SetNamespace(namespace)
	return cert
}
