// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

// The projected Placement CR is named "{controlplane.Name}-placement" — the same
// deterministic, collision-free naming convention as the Keystone, Horizon, and
// Glance children (see keystoneNameSuffix) — and lives in
// cp.PlacementNamespace(): the ControlPlane's own namespace by default, or the
// one services.placement.namespace assigns.
const placementNameSuffix = "-placement"

// defaultPlacementRepository is the canonical Placement image repository; the tag
// is derived from spec.openStackRelease unless spec.services.placement.image
// overrides the whole image reference.
const defaultPlacementRepository = "ghcr.io/c5c3/placement"

// placementDeletionAllowedAnnotation, when set to a truthy value on a
// ControlPlane, opts that ControlPlane in to tearing down a previously-projected
// Placement child (and its DB-credential ExternalSecret) when
// spec.services.placement is unset. The preserve-by-default posture mirrors the
// Keystone/Horizon/Glance annotations for a consistent operator UX: an accidental
// block drop never silently removes a running service.
const placementDeletionAllowedAnnotation = "c5c3.io/allow-placement-deletion"

// defaultPlacementDatabaseName is the logical database name the Placement schema
// always lives in, regardless of whether Placement shares the ControlPlane's
// database cluster or takes a dedicated one — its own logical schema keeps it
// isolated from Keystone's on a shared cluster.
const defaultPlacementDatabaseName = "placement"

// placementServiceAccountName mirrors c5c3v1alpha1.PlacementServiceAccountName,
// the single source of truth for the spec.korc.serviceAccounts entry the
// Placement projection consumes for its Keystone service user. The defaulting
// webhook auto-injects it (name placement, project service-placement (create),
// roles [service]) whenever services.placement is set.
const placementServiceAccountName = c5c3v1alpha1.PlacementServiceAccountName

// placementName returns the deterministic name of the Placement CR projected from
// the given ControlPlane (see placementNameSuffix).
func placementName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + placementNameSuffix
}

// placementDeletionAllowed reports whether cp opts in to deleting its projected
// Placement child when spec.services.placement is unset, via a truthy
// placementDeletionAllowedAnnotation. A missing, malformed, or non-truthy value
// means "preserve".
func placementDeletionAllowed(cp *c5c3v1alpha1.ControlPlane) bool {
	allowed, err := strconv.ParseBool(cp.Annotations[placementDeletionAllowedAnnotation])
	return err == nil && allowed
}

// placementKeystoneEndpoint returns the Keystone endpoint URL projected into the
// Placement child's spec.keystoneEndpoint. Placement validates every token
// server-side against this URL, so what the Placement pods can reach decides it —
// which is the cluster Placement is placed on against the one Keystone runs on,
// the rule keystoneEndpointFor holds for every service.
func placementKeystoneEndpoint(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneEndpointFor(cp, cp.PlacementTargetClusterRef())
}

// placementEndpointURL renders the in-cluster URL of the projected Placement API
// Service by naming convention (the Placement API listens on 8778), the
// cross-service endpoint contract the catalog registers against.
func placementEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return managedServiceURL(placementName(cp), cp.PlacementNamespace(), 8778, "")
}

// placementServiceAccount returns the auto-injected "placement" entry from
// spec.korc.serviceAccounts and whether it is declared. The Placement projection
// derives its Keystone service user from this entry.
func placementServiceAccount(cp *c5c3v1alpha1.ControlPlane) (c5c3v1alpha1.ServiceAccountSpec, bool) {
	for _, sa := range cp.Spec.KORC.ServiceAccounts {
		if sa.Name == placementServiceAccountName {
			return sa, true
		}
	}
	return c5c3v1alpha1.ServiceAccountSpec{}, false
}

// placementServiceAccountReady reports whether the per-account status entry for
// the placement service account is Ready — the readiness reconcileServiceAccounts
// computes for it in the same reconcile pass, ahead of this sub-reconciler.
func placementServiceAccountReady(cp *c5c3v1alpha1.ControlPlane) bool {
	for _, s := range cp.Status.ServiceAccounts {
		if s.Name == placementServiceAccountName {
			return s.Ready
		}
	}
	return false
}

// reconcilePlacement projects spec.services.placement into an owned Placement CR
// and drives the PlacementReady condition.
//
// The sub-reconciler is GATED on KeystoneReady (Placement validates every token
// against the ControlPlane's Keystone child) and on the placement service account
// (Placement authenticates as that Keystone user). Once gated through, it ensures
// the DB-credential ExternalSecret, projects the Placement CR — database/cache
// DeepCopied from the resolved backing services, the Keystone endpoint derived
// top-down (placementKeystoneEndpoint) — and mirrors the child's Ready condition
// back into PlacementReady.
func (r *ControlPlaneReconciler) reconcilePlacement(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// spec.services.placement is optional. When unset, this ControlPlane manages no
	// placement service and reports PlacementReady as not-managed so the aggregate
	// Ready condition is not blocked (staged adoption). A previously-projected child
	// is preserved unless the ControlPlane opts in to deletion.
	if cp.Spec.Services.Placement == nil {
		message := "spec.services.placement is unset; no Placement service is managed by this ControlPlane"
		if placementDeletionAllowed(cp) {
			if err := r.deleteOrphanedPlacement(ctx, cp); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			// Preserve the child — but NEVER the credential minter. A live
			// VaultDynamicSecret keeps issuing a fresh MySQL user with ALL PRIVILEGES
			// on the placement schema at every refresh interval, indefinitely, for a
			// service this ControlPlane has been told it no longer manages: no
			// consumer, no revocation, and a PlacementReady=True/PlacementNotManaged
			// condition that surfaces none of it. Preserving a running service does
			// not imply preserving the generator behind its credentials, so the
			// dynamic objects come down either way.
			//
			// PlacementNamespace() still resolves correctly here: removing a
			// services.placement.namespace assignment is rejected by
			// validateServiceNamespacesImmutable, so the only admissible way to reach
			// this branch with a live generator is the co-located one, where the
			// generator sits in the ControlPlane's own namespace.
			r.deleteDynamicDBCredentialObjects(ctx, cp, placementDBCredentialTarget(cp))
			message += fmt.Sprintf("; any previously-projected Placement child is preserved "+
				"(set annotation %s=true to allow deletion), but its dynamic DB-credential generator is torn down",
				placementDeletionAllowedAnnotation)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypePlacementReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "PlacementNotManaged",
			Message:            message,
		})
		return ctrl.Result{}, nil
	}

	// Resolve the backing services Placement actually talks to: its own dedicated
	// instances when it opted into them, the ControlPlane-wide shared ones
	// otherwise.
	//
	// Nil-safety fail-safe. The projection DeepCopies these, so an unresolvable
	// instance has nothing to project and the deref below would panic. The
	// validating webhook requires spec.infrastructure outside External mode (and
	// forbids services.placement in External mode), so this only fires for a
	// webhook-bypassed CR.
	database := effectivePlacementDatabase(cp)
	cache := effectivePlacementCache(cp)
	if database == nil || cache == nil {
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// Gate on KeystoneReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeKeystoneReady) {
		logger.Info("Keystone not ready, deferring Placement projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypePlacementReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForKeystone",
			Message:            "KeystoneReady is not True; Placement projection deferred",
		})
		return ctrl.Result{RequeueAfter: keystoneInfraGateRequeueAfter}, nil
	}

	// Gate on the placement service account. Its declaration is auto-injected by the
	// defaulting webhook whenever services.placement is set; a missing entry means
	// admission was bypassed, so fail loud with a named field rather than projecting
	// a Placement with no Keystone user. A declared-but-not-yet-Ready account defers
	// projection until its user and password are provisioned.
	sa, declared := placementServiceAccount(cp)
	if !declared {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypePlacementReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ServiceAccountNotDeclared",
			Message: fmt.Sprintf("no spec.korc.serviceAccounts entry named %q is declared; the defaulting webhook "+
				"injects it whenever services.placement is set, so a ControlPlane reaching here bypassed admission",
				placementServiceAccountName),
		})
		return ctrl.Result{}, nil
	}
	if !placementServiceAccountReady(cp) {
		logger.Info("placement service account not ready, deferring Placement projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypePlacementReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForServiceAccount",
			Message: fmt.Sprintf("service account %q is not yet Ready; Placement projection deferred until its "+
				"Keystone user and password are provisioned", placementServiceAccountName),
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// The EFFECTIVE credentials mode of the database Placement connects to, resolved
	// once so the credential projection below, its readiness gate, and the mode
	// stamped onto the child further down can never disagree.
	dynamic := database.ClusterRef != nil && placementDBCredentialsDynamicEnabled(cp)

	// Ensure the DB-credential objects BEFORE the child so the Secret it references
	// exists when the placement-operator resolves it. Managed only: a brownfield
	// database (ClusterRef nil) carries a user-supplied credential out-of-band, so
	// there is nothing for the operator to project. In Dynamic mode the shared
	// helper also holds the projection until an engine-issued credential has landed
	// (see ensureServiceDBCredential).
	if database.ClusterRef != nil {
		res, halt, err := r.ensureServiceDBCredential(ctx, cp, placementDBCredentialTarget(cp),
			dynamic, "Placement", conditionTypePlacementReady)
		if halt {
			return res, err
		}
	}

	// Resolve the Placement image. spec.services.placement.image overrides the
	// release-derived default when set.
	image := commonv1.ImageSpec{
		Repository: defaultPlacementRepository,
		Tag:        cp.Spec.OpenStackRelease,
	}
	if override := cp.Spec.Services.Placement.Image; override != nil {
		image = *override
	}

	// Place the child in the namespace assigned to the Placement service (the
	// ControlPlane's own unless services.placement.namespace says otherwise). A
	// child outside the ControlPlane's namespace can carry no owner reference, so it
	// is stamped with the ownership labels and applied unowned.
	placementNS := cp.PlacementNamespace()
	crossNamespace := placementNS != cp.Namespace
	pl := &placementv1alpha1.Placement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      placementName(cp),
			Namespace: placementNS,
		},
	}
	if crossNamespace {
		stampControlPlaneChildLabels(pl, cp)
	}

	pl.Spec.OpenStackRelease = cp.Spec.OpenStackRelease
	pl.Spec.Image = image

	// Thread the service's target cluster onto the child verbatim; a nil source
	// yields nil and leaves the child unplaced (see the Keystone projection).
	pl.Spec.TargetClusterRef = cp.Spec.Services.Placement.TargetClusterRef.DeepCopy()

	// Project the merged extraConfig (globalExtraConfig unioned with the
	// per-service block, per-service winning key by key). Assigned
	// unconditionally, following the revert-on-clear convention: a nil merge keeps
	// the SSA-applied intent free of spec.extraConfig, so a direct edit on the
	// child stays unowned until a ControlPlane block is set — and clearing the
	// ControlPlane block reverts the child rather than pinning the last value.
	pl.Spec.ExtraConfig = c5c3v1alpha1.MergedExtraConfig(
		cp.Spec.GlobalExtraConfig, cp.Spec.Services.Placement.ExtraConfig)

	// Point Placement at the SAME backing services the ControlPlane provisioned. The
	// logical database is always "placement" — its own schema keeps it isolated from
	// Keystone's on a shared cluster. DeepCopy (over a plain struct copy) is
	// required because DatabaseSpec/CacheSpec carry pointer fields — a shallow copy
	// would alias cp.Spec.
	pl.Spec.Database = *database.DeepCopy()
	pl.Spec.Database.Database = defaultPlacementDatabaseName
	// In managed mode the operator OWNS the placement DB credential —
	// reconcilePlacement materialises it (above) into a per-ControlPlane Secret
	// named placementDBCredentialSecretName(cp). Override the projected Placement
	// CR's database.secretRef to that operator-owned Secret (key "password"), and
	// project the EFFECTIVE credentials mode: Dynamic (engine-issued) is the default
	// on the managed shared database, a per-service override or the shared Static
	// opt-out flips it to Static, and a dedicated placement database stays Static
	// (no engine role can mint its credentials). Brownfield (ClusterRef nil) leaves
	// the user-supplied secretRef and credentialsMode in place.
	if database.ClusterRef != nil {
		pl.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: placementDBCredentialSecretName(cp), Key: "password"}
		if dynamic {
			pl.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
		} else {
			pl.Spec.Database.CredentialsMode = commonv1.CredentialsModeStatic
		}
	}

	pl.Spec.Cache = *cache.DeepCopy()

	// The Keystone endpoint is derived top-down from the ControlPlane rather than
	// read from the Keystone child's status — no machine consumer reads status
	// endpoints per the settled convention. keystonePublicEndpoint is the
	// browser/client-facing URL Placement advertises on a 401 (empty when Keystone
	// is not externally exposed, in which case the child falls back to the internal
	// endpoint).
	pl.Spec.KeystoneEndpoint = placementKeystoneEndpoint(cp)
	pl.Spec.KeystonePublicEndpoint = keystonePublicEndpoint(cp.Spec.Services.Keystone)

	pl.Spec.Region = cp.Spec.Region

	// The Keystone service user Placement authenticates as, derived from the
	// auto-injected placement service account: the user and project domains resolve
	// to the same effective domain the account is provisioned in, and the password
	// is read from the account's materialized consumer Secret.
	pl.Spec.ServiceUser = placementv1alpha1.ServiceUserSpec{
		Username:          serviceAccountUserName(sa),
		ProjectName:       sa.Project.Name,
		UserDomainName:    serviceAccountDomainName(cp, sa),
		ProjectDomainName: serviceAccountDomainName(cp, sa),
		SecretRef:         commonv1.SecretRefSpec{Name: serviceAccountCredentialsSecretName(cp, sa), Key: "password"},
	}

	// Project the ControlPlane's RESOLVED store selection onto the Placement child
	// so it never falls back to its own shared-cluster-store default.
	pl.Spec.SecretStoreRef = effectiveControlPlaneStoreRefPtr(cp)

	// DeepCopy for the same aliasing reason as Database above; a nil source yields
	// nil, clearing any previously-projected gateway so removal tears the HTTPRoute
	// down.
	pl.Spec.Gateway = cp.Spec.Services.Placement.Gateway.DeepCopy()

	// Resolve replicas to the shared operator default, then let an override win.
	// Assigning unconditionally means clearing services.placement.replicas reverts
	// the child to the default instead of leaving the previously-projected value
	// pinned on the fetched child.
	pl.Spec.Deployment.Replicas = commonv1.DefaultReplicas
	if cp.Spec.Services.Placement.Replicas != nil {
		pl.Spec.Deployment.Replicas = *cp.Spec.Services.Placement.Replicas
	}

	// spec.apiServer is deliberately NOT set — the child-side uWSGI defaults stay
	// authoritative, and tuning them stays a standalone-CR concern.

	return commonreconcile.ProjectChild(ctx, r.Client, r.Scheme, cp,
		commonreconcile.ChildProjectionParams[*placementv1alpha1.Placement]{
			Child:          pl,
			ConditionType:  conditionTypePlacementReady,
			ReadyReason:    "PlacementReady",
			ReadyMessage:   "Projected Placement CR is ready",
			WaitingReason:  "WaitingForPlacement",
			WaitingMessage: fmt.Sprintf("Placement %q is not ready", pl.Name),
			// An Invalid (HTTP 422) rejection from the Placement API server means the
			// projected spec violates a CRD/webhook rule — surface a distinct,
			// actionable reason so the wedge is diagnosable from the condition.
			RejectedReason: "PlacementProjectionRejected",
			RejectedMessage: func(err error) string {
				return fmt.Sprintf("Placement API server rejected the projected spec; reconcile the ControlPlane spec "+
					"to a valid projection to recover: %v", err)
			},
			ErrorReason:     "PlacementError",
			ErrorMessage:    func(err error) string { return fmt.Sprintf("create-or-update Placement: %v", err) },
			WaitRequeue:     infraRequeueAfter,
			Conditions:      &cp.Status.Conditions,
			Generation:      cp.Generation,
			ChildConditions: func(p *placementv1alpha1.Placement) []metav1.Condition { return p.Status.Conditions },
			Unowned:         crossNamespace,
		})
}

// deleteOrphanedPlacement removes a previously-projected Placement child — and the
// DB-credential ExternalSecret and placement catalog K-ORC CRs that follow it —
// when spec.services.placement is unset AND the ControlPlane has opted in to
// deletion via placementDeletionAllowedAnnotation (the caller gates this). Each
// object is only deleted when this ControlPlane still owns it (by owner reference
// in its own namespace, by the ownership labels in a service namespace); a foreign
// object colliding on a name is left alone.
func (r *ControlPlaneReconciler) deleteOrphanedPlacement(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	placementNS := cp.PlacementNamespace()
	// The catalog CRs are ControlPlane-scoped and live in the ControlPlane's own
	// namespace regardless of Placement placement (see managedCatalogService), so
	// they are swept from childNamespace(cp), not placementNS.
	catalogNS := childNamespace(cp)

	// The Dynamic-mode client Certificate has no Go type, so it is addressed
	// unstructured.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(placementDBCredentialClientCertName(cp))
	cert.SetNamespace(placementNS)

	children := []client.Object{
		// The Placement child.
		&placementv1alpha1.Placement{
			ObjectMeta: metav1.ObjectMeta{Name: placementName(cp), Namespace: placementNS},
		},
		// The DB-credential ExternalSecret.
		&esov1.ExternalSecret{
			ObjectMeta: metav1.ObjectMeta{Name: placementDBCredentialSecretName(cp), Namespace: placementNS},
		},
		// The Dynamic-mode DB-credential objects: the VaultDynamicSecret generator,
		// its mTLS client Certificate, and the ServiceAccount whose token it
		// authenticates with.
		&esgenv1alpha1.VaultDynamicSecret{
			ObjectMeta: metav1.ObjectMeta{Name: placementDBCredentialSecretName(cp), Namespace: placementNS},
		},
		cert,
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: placementDBCredentialServiceAccountName, Namespace: placementNS},
		},
		// The Placement catalog K-ORC CRs: the placement Service and its
		// internal/public Endpoints, in catalogNS.
		&orcv1alpha1.Service{ObjectMeta: metav1.ObjectMeta{Name: placementCatalogServiceName(cp), Namespace: catalogNS}},
		&orcv1alpha1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{Name: placementCatalogEndpointName(cp, "internal"), Namespace: catalogNS},
		},
		&orcv1alpha1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{Name: placementCatalogEndpointName(cp, "public"), Namespace: catalogNS},
		},
	}
	for _, child := range children {
		if err := commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, child, func(live client.Object) bool {
			return isControlPlaneChild(live, cp)
		}); err != nil {
			return err
		}
	}
	return nil
}
