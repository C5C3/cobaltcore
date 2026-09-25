// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// The projected Nova CR is named "{controlplane.Name}-nova", the same
// deterministic, collision-free naming convention as the Keystone, Horizon,
// Glance, Placement, Barbican, Neutron, and Cinder children (see
// keystoneNameSuffix), and lives in cp.NovaNamespace(): the ControlPlane's own
// namespace by default, or the one services.nova.namespace assigns.

// novaNameSuffix is appended to the ControlPlane name to derive the name of the
// projected Nova CR (and, through it, its credential and registration objects).
const novaNameSuffix = "-nova"

// novaAPIPort is the port the nova operator's API Service listens on.
const novaAPIPort int32 = 8774

// defaultNovaRepository is the canonical Nova image repository; the tag is
// derived from spec.openStackRelease unless spec.services.nova.image overrides
// the whole image reference.
const defaultNovaRepository = "ghcr.io/c5c3/nova"

// novaDeletionAllowedAnnotation, when set to a truthy value on a ControlPlane,
// opts that ControlPlane in to tearing down a previously-projected Nova child
// (with its two DB-credential chains, its metadata shared secret, and its
// messaging Secrets) when spec.services.nova is unset. The preserve-by-default
// posture mirrors the annotations of every sibling service for a consistent
// operator UX: an accidental block drop never silently removes a running
// service, and removing the compute control plane of a cloud with running
// instances is not something to reach by editing one line.
const novaDeletionAllowedAnnotation = "c5c3.io/allow-nova-deletion"

// defaultNovaAPIDatabaseName / defaultNovaDatabaseName are the logical database
// names the two halves of Nova's state always live in, regardless of whether
// Nova shares the ControlPlane's database cluster or takes a dedicated one. The
// API schema holds the cell map, the flavors and the instance mappings; the cell
// schema holds the instances themselves, and its user is granted on the derived
// "nova_cell0" as well. They are the schemas the pre-wired OpenBao engine roles
// grant on, so neither is a per-ControlPlane knob.
const (
	defaultNovaAPIDatabaseName = "nova_api"
	defaultNovaDatabaseName    = "nova"
)

// reasonWaitingForComputeConfig is the bounded wait while the compute contract
// the nova operator publishes has not been written yet. It is the Secret every
// mirror target is fed from, so no target can be served before it exists.
const reasonWaitingForComputeConfig = "WaitingForComputeConfig"

// novaName returns the name of the Nova CR the reconciler projects for the given
// ControlPlane (see novaNameSuffix).
func novaName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + novaNameSuffix
}

// novaDeletionAllowed reports whether cp opts in to deleting its projected Nova
// child when spec.services.nova is unset, via a truthy
// novaDeletionAllowedAnnotation. A missing, malformed, or non-truthy value means
// "preserve".
func novaDeletionAllowed(cp *c5c3v1alpha1.ControlPlane) bool {
	allowed, err := strconv.ParseBool(cp.Annotations[novaDeletionAllowedAnnotation])
	return err == nil && allowed
}

// novaMessagingTarget names the compute service as a consumer of the shared bus:
// the Secrets are named after the Nova child, written in its namespace, and every
// condition lands on NovaReady.
func novaMessagingTarget(cp *c5c3v1alpha1.ControlPlane) serviceMessagingTarget {
	return serviceMessagingTarget{
		Service:       "Nova",
		ChildName:     novaName(cp),
		Namespace:     cp.NovaNamespace(),
		ConditionType: conditionTypeNovaReady,
	}
}

// novaMessagingSecretName returns the name of the brownfield transport-URL Secret
// the ControlPlane writes beside the Nova child ("<cp>-nova-messaging").
//
// The name is the ControlPlane's own on purpose. In that same namespace the nova
// operator claims messaging.TransportURLSecretName(novaName(cp))
// ("<cp>-nova-transport-url") for the Secret it derives from
// spec.messaging.secretRef, so writing the bus under that name would leave two
// controllers rewriting one object on every pass.
func novaMessagingSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return serviceMessagingSecretName(novaMessagingTarget(cp))
}

// novaMessagingCASecretName returns the name of the CA mirror that carries the
// broker's CA bundle into the Nova namespace ("<cp>-nova-messaging-ca"), under
// the same name-of-our-own rule as novaMessagingSecretName.
func novaMessagingCASecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return serviceMessagingCASecretName(novaMessagingTarget(cp))
}

// novaKeystoneEndpoint returns the Keystone endpoint URL projected into the Nova
// child's spec.keystoneEndpoint. Nova validates every token server-side against
// this URL and calls Placement, Neutron, Glance, Cinder and Barbican through it,
// so what the Nova pods can reach decides it: the cluster Nova is placed on
// against the one Keystone runs on, the rule keystoneEndpointFor holds for every
// service.
func novaKeystoneEndpoint(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneEndpointFor(cp, cp.NovaTargetClusterRef())
}

// novaEndpointURL renders the in-cluster URL of the projected Nova API Service
// by naming convention, the cross-service endpoint contract the catalog
// registers against. It carries no path: the catalog rows append the "/v2.1" the
// compute API is served under (see novaCatalogURL).
func novaEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return managedServiceURL(novaName(cp), cp.NovaNamespace(), novaAPIPort, "")
}

// novaComputeConfigSecretName returns the name of the Secret the nova operator
// publishes the compute contract under ("<cp>-nova-compute-config"): the
// nova.conf fragment a nova-compute needs to join this control plane, and the
// bus credentials it connects with.
func novaComputeConfigSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return novaName(cp) + "-compute-config"
}

// computeConfigMirrorTarget names one place the compute contract has to be
// readable from: a namespace, and the cluster it lives on (nil for the local
// one).
type computeConfigMirrorTarget struct {
	ClusterRef *commonv1.TargetClusterRefSpec
	Namespace  string
}

// novaComputeConfigMirrorTargets returns the places this ControlPlane has to
// deliver the compute contract to: one per cluster a NovaCompute of its Nova
// runs on, in the Nova namespace, where the pool's pods mount it.
//
// The pools are user-authored, so the plane reads them and never projects
// them. A pool being deleted is left out, and the mirror it leaves behind is
// its own teardown's to reap (it carries novav1alpha1.ComputeConfigMirrorLabel
// for that): a ControlPlane-status record of what it mirrored would not
// survive, because reconcileNova runs in the parallel group, which keeps only
// conditions and metadata. The cluster the Nova itself is placed on is dropped
// too, since the published Secret already lives there, and pools sharing a
// cluster share one target. A cluster that does not serve the NovaCompute kind
// has no pools, so a no-match yields no target rather than an error.
func (r *ControlPlaneReconciler) novaComputeConfigMirrorTargets(ctx context.Context,
	cp *c5c3v1alpha1.ControlPlane,
) ([]computeConfigMirrorTarget, error) {
	var pools novav1alpha1.NovaComputeList
	if err := r.List(ctx, &pools, client.InNamespace(cp.NovaNamespace())); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing the NovaComputes in namespace %q: %w", cp.NovaNamespace(), err)
	}

	own := clusterNameOf(targetClusterRefForNamespace(cp, cp.NovaNamespace()))
	clusters := map[string]*commonv1.TargetClusterRefSpec{}
	for i := range pools.Items {
		pool := &pools.Items[i]
		if pool.Spec.NovaRef.Name != novaName(cp) || !pool.DeletionTimestamp.IsZero() {
			continue
		}
		name := clusterNameOf(pool.Spec.TargetClusterRef)
		if name == own {
			continue
		}
		clusters[name] = pool.Spec.TargetClusterRef.DeepCopy()
	}

	targets := make([]computeConfigMirrorTarget, 0, len(clusters))
	for _, name := range slices.Sorted(maps.Keys(clusters)) {
		targets = append(targets, computeConfigMirrorTarget{ClusterRef: clusters[name], Namespace: cp.NovaNamespace()})
	}
	return targets, nil
}

// clusterNameOf is the cluster a ref names, "" for the local one.
func clusterNameOf(ref *commonv1.TargetClusterRefSpec) string {
	if ref == nil {
		return ""
	}
	return ref.Name
}

// novaComputeToControlPlaneMapper maps a NovaCompute event onto the
// ControlPlanes whose Nova the pool joins, so a pool that appears on a new
// cluster, or the last one that leaves it, changes the mirror targets without
// waiting for a periodic resync.
//
// A plain Owns() would never fire: pools are user-authored and carry no
// ControlPlane owner reference. The match is on the Nova namespace and the Nova
// name together, so a same-named Nova of an unrelated ControlPlane never wakes
// this one.
func (r *ControlPlaneReconciler) novaComputeToControlPlaneMapper(ctx context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*novav1alpha1.NovaCompute)
	if !ok {
		return nil
	}

	var list c5c3v1alpha1.ControlPlaneList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "listing ControlPlanes for NovaCompute event",
			"novaCompute", client.ObjectKeyFromObject(pool))
		return nil
	}

	var requests []reconcile.Request
	for i := range list.Items {
		cp := &list.Items[i]
		if cp.Spec.Services.Nova != nil && cp.NovaNamespace() == pool.Namespace &&
			novaName(cp) == pool.Spec.NovaRef.Name {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cp)})
		}
	}
	return requests
}

// novaComputeMembershipPredicate admits the NovaCompute events that change a
// Nova's mirror targets: a pool created, deleted, or starting to be deleted.
// novaRef and targetClusterRef are immutable, so no other update moves a pool
// between targets, and a pool writes its status at least once a minute, which
// must not reconcile the whole plane each time.
func novaComputeMembershipPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// mirrorNovaComputeConfig copies the compute contract the nova operator
// published into one target namespace, and reports whether that target is served.
//
// The contract is a Secret in the Nova namespace, updated in place by the nova
// operator. A compute node does not read it there: it runs outside this cluster,
// its agents are configured from the namespace its own attachment names, so the
// Secret has to exist a second time wherever those agents look.
//
// While services.nova.remoteCompute is set, the source is the remote contract
// (<cp>-nova-remote-compute-config), whose addresses a compute cluster reaches.
// The mirror keeps the in-cluster contract's name either way, the name a
// NovaCompute on the compute cluster reads from status.computeConfigSecretRef.
//
// ok=false is a WAIT rather than a failure, and reason/message carry what an
// operator reading the ControlPlane condition needs: the contract has not been
// published yet, or the target's cluster does not resolve. The resolver's own
// text is relayed verbatim for the latter ("cluster not found" for an
// unregistered name). A non-nil err is a genuine read or write failure and the
// caller returns it.
//
// The mirror carries this ControlPlane's ownership labels, so the teardown that
// sweeps a placed namespace's label-owned children reaps it with the rest, and
// novav1alpha1.ComputeConfigMirrorLabel, which is how the last NovaCompute on
// its cluster recognizes it as a mirror to reap.
func (r *ControlPlaneReconciler) mirrorNovaComputeConfig(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, target computeConfigMirrorTarget,
) (ok bool, reason, message string, err error) {
	name := novaComputeConfigSecretName(cp)
	sourceName := name
	if cp.Spec.Services.Nova != nil && cp.Spec.Services.Nova.RemoteCompute != nil {
		sourceName = novaRemoteComputeConfigSecretName(cp)
	}
	novaNS := cp.NovaNamespace()

	source, err := r.childrenClientFor(ctx, cp, novaNS)
	if err != nil {
		return false, commonmulticluster.TargetClusterUnavailable, fmt.Sprintf("%v", err), nil
	}

	published := &corev1.Secret{}
	switch gerr := source.Get(ctx, client.ObjectKey{Namespace: novaNS, Name: sourceName}, published); {
	case apierrors.IsNotFound(gerr):
		return false, reasonWaitingForComputeConfig, fmt.Sprintf(
			"the compute config Secret %s/%s has not been published yet; the nova operator writes it once the "+
				"control plane has converged, and the compute nodes in namespace %q are configured from it",
			novaNS, sourceName, target.Namespace), nil
	case gerr != nil:
		return false, "", "", fmt.Errorf("reading the compute config Secret %s/%s: %w", novaNS, sourceName, gerr)
	}

	delivery, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, target.ClusterRef)
	if err != nil {
		return false, commonmulticluster.TargetClusterUnavailable, fmt.Sprintf("%v", err), nil
	}

	mirror := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: target.Namespace},
		Data:       published.Data,
	}
	// Stamped before the apply, so the mirror is recognizable the moment it
	// exists whichever ownership mechanism its namespace permits.
	stampControlPlaneChildLabels(mirror, cp)
	mirror.Labels[novav1alpha1.ComputeConfigMirrorLabel] = "true"
	if err := r.ensureUnownedOrOwned(ctx, delivery, cp, mirror); err != nil {
		return false, "", "", fmt.Errorf("mirroring the compute config Secret into namespace %q: %w",
			target.Namespace, err)
	}
	return true, "", "", nil
}

// reconcileNova projects spec.services.nova into an owned Nova CR and drives the
// NovaReady condition.
//
// The sub-reconciler is GATED on KeystoneReady (Nova validates every token
// against the ControlPlane's Keystone child), on PlacementReady (every instance
// boot claims its resources against Placement first, so a compute service ahead
// of its placement service accepts requests it cannot serve), and on the
// KeystoneService child it projects for Nova (Nova authenticates as the Keystone
// user that registration provisions). Once gated through, it delivers the shared
// message bus into the compute service's namespace (and the external bus URL
// the remote compute contract carries, while services.nova.remoteCompute is
// set), ensures the two DB-credential chains the nova_api and cell schemas take,
// generates the metadata shared secret, projects the Nova CR (databases and
// cache DeepCopied from the resolved backing services, the Keystone endpoint
// derived top-down through novaKeystoneEndpoint), delivers the compute contract
// to every mirror target, provisions the hypervisor operator's account and
// delivers its auth Secret to the same targets while
// spec.services.nova.hypervisorOperator is set (pruning both while it is not,
// see reconcileNovaHypervisorOperator), copies the metadata shared secret to
// the NeutronMetadataAgents on target clusters that name
// "<cp>-nova-metadata-agent-secret" (see reconcileNovaMetadataAgentSecrets), and
// folds both children's readiness into NovaReady.
func (r *ControlPlaneReconciler) reconcileNova(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// spec.services.nova is optional. When unset, this ControlPlane manages no
	// compute service and reports NovaReady as not-managed so the aggregate Ready
	// condition is not blocked (staged adoption). A previously-projected child is
	// preserved unless the ControlPlane opts in to deletion.
	if cp.Spec.Services.Nova == nil {
		message := "spec.services.nova is unset; no Nova service is managed by this ControlPlane"
		if novaDeletionAllowed(cp) {
			if err := r.deleteOrphanedNova(ctx, cp); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			// Preserve the child, but NEVER the credential minters. A live
			// VaultDynamicSecret keeps issuing a fresh MySQL user with ALL PRIVILEGES
			// on a nova schema at every refresh interval, indefinitely, for a service
			// this ControlPlane has been told it no longer manages: no consumer, no
			// revocation, and a NovaReady=True/NovaNotManaged condition that surfaces
			// none of it. Preserving a running service does not imply preserving the
			// generators behind its credentials, so both chains come down either way.
			//
			// NovaNamespace() still resolves correctly here: removing a
			// services.nova.namespace assignment is rejected by
			// validateServiceNamespacesImmutable, so the only admissible way to reach
			// this branch with live generators is the co-located one, where they sit
			// in the ControlPlane's own namespace.
			r.deleteDynamicDBCredentialObjects(ctx, cp, novaAPIDBCredentialTarget(cp))
			r.deleteDynamicDBCredentialObjects(ctx, cp, novaCellDBCredentialTarget(cp))
			message += fmt.Sprintf("; any previously-projected Nova child is preserved "+
				"(set annotation %s=true to allow deletion), but its dynamic DB-credential generators are torn down",
				novaDeletionAllowedAnnotation)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNovaReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "NovaNotManaged",
			Message:            message,
		})
		return ctrl.Result{}, nil
	}

	// Resolve the backing services Nova actually talks to: its own dedicated
	// instances when it opted into them, the ControlPlane-wide shared ones
	// otherwise. One database instance carries both schemas.
	//
	// Nil-safety fail-safe. The projection DeepCopies these, so an unresolvable
	// instance has nothing to project and the deref below would panic; the shared
	// bus is dereferenced for its TLS block on the same path. The validating
	// webhook requires spec.infrastructure with a messaging block outside External
	// mode (and forbids services.nova in External mode), so this only fires for a
	// webhook-bypassed CR.
	database := effectiveNovaDatabase(cp)
	cache := effectiveNovaCache(cp)
	if database == nil || cache == nil ||
		cp.Spec.Infrastructure == nil || cp.Spec.Infrastructure.Messaging == nil {
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// Gate on KeystoneReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeKeystoneReady) {
		logger.Info("Keystone not ready, deferring Nova projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNovaReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForKeystone",
			Message:            "KeystoneReady is not True; Nova projection deferred",
		})
		return ctrl.Result{RequeueAfter: keystoneInfraGateRequeueAfter}, nil
	}

	// And on PlacementReady. Placement is not a sibling Nova can run without: the
	// conductor claims every instance's resources there before it boots, so a
	// compute service brought up ahead of its placement service accepts requests
	// it cannot serve and leaves them in ERROR. A ControlPlane that manages no
	// placement service reports the condition True under its own not-managed
	// reason, so this gate reads it rather than the block.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypePlacementReady) {
		logger.Info("Placement not ready, deferring Nova projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNovaReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForPlacement",
			Message:            "PlacementReady is not True; Nova projection deferred",
		})
		return ctrl.Result{RequeueAfter: keystoneInfraGateRequeueAfter}, nil
	}

	// Deliver the ControlPlane-wide bus into the namespace (and onto the cluster)
	// the compute service runs in. The transport URL's digest is not projected:
	// the nova operator rolls its pods off the Secret it derives itself, so a
	// second digest on the child would only add a redundant rollout trigger.
	if msgRes, halt, err := r.reconcileServiceMessaging(ctx, cp, novaMessagingTarget(cp)); halt {
		return msgRes, err
	}

	// And the external bus URL the remote compute contract carries, when the
	// ControlPlane was handed one. It halts the same way: a child projected with
	// spec.remoteCompute before the Secret it names exists would only wait on it.
	if msgRes, halt, err := r.reconcileNovaRemoteMessaging(ctx, cp); halt {
		return msgRes, err
	}

	// Register Nova against the identity plane: one KeystoneService child carrying
	// the compute catalog entry and the service account Nova authenticates as,
	// mirrored onto a placed Nova's cluster and gated on the account it provisions.
	child, regRes, halt, err := r.reconcileBuiltinRegistration(ctx, cp, desiredNovaRegistration(cp),
		"Nova", conditionTypeNovaReady)
	if halt {
		return regRes, err
	}

	// The EFFECTIVE credentials mode of the database Nova connects to, resolved
	// once so the two credential projections below, their readiness gates, and the
	// mode stamped onto both of the child's database blocks can never disagree.
	// The Nova CRD rejects a child whose two blocks carry different modes, so one
	// verdict has to drive both.
	dynamic := database.ClusterRef != nil && novaDBCredentialsDynamicEnabled(cp)

	// Ensure the DB-credential objects BEFORE the child so the Secrets it
	// references exist when the nova-operator resolves them. Managed only: a
	// brownfield database (ClusterRef nil) carries user-supplied credentials
	// out-of-band, so there is nothing for the operator to project. In Dynamic mode
	// the shared helper also holds the projection until an engine-issued credential
	// has landed (see ensureServiceDBCredential). The API chain goes first because
	// its schema is the one the cell schema's mappings are registered in, so an
	// operator watching a stalled onboarding reads the two waits in the order the
	// db-sync will need them.
	if database.ClusterRef != nil {
		for _, credential := range []struct {
			target  dbCredentialTarget
			service string
		}{
			{novaAPIDBCredentialTarget(cp), "NovaAPI"},
			{novaCellDBCredentialTarget(cp), "NovaCell"},
		} {
			res, halt, err := r.ensureServiceDBCredential(ctx, cp, credential.target,
				dynamic, credential.service, conditionTypeNovaReady)
			if halt {
				return res, err
			}
		}
	}

	// Generate the metadata shared secret, or take a generated one down once the
	// ControlPlane supplies its own. It does not wait for the value to
	// materialise: the child gates on that itself.
	if metaRes, halt, err := r.reconcileNovaMetadataSecret(ctx, cp); halt {
		return metaRes, err
	}

	// Resolve the Nova image. spec.services.nova.image overrides the
	// release-derived default when set.
	image := commonv1.ImageSpec{
		Repository: defaultNovaRepository,
		Tag:        cp.Spec.OpenStackRelease,
	}
	if override := cp.Spec.Services.Nova.Image; override != nil {
		image = *override
	}

	// Place the child in the namespace assigned to the compute service (the
	// ControlPlane's own unless services.nova.namespace says otherwise). A child
	// outside the ControlPlane's namespace can carry no owner reference, so it is
	// stamped with the ownership labels and applied unowned.
	novaNS := cp.NovaNamespace()
	crossNamespace := novaNS != cp.Namespace
	nv := &novav1alpha1.Nova{
		ObjectMeta: metav1.ObjectMeta{
			Name:      novaName(cp),
			Namespace: novaNS,
		},
	}
	if crossNamespace {
		stampControlPlaneChildLabels(nv, cp)
	}

	nv.Spec.OpenStackRelease = cp.Spec.OpenStackRelease
	nv.Spec.Image = image

	// Thread the service's target cluster onto the child verbatim; a nil source
	// yields nil and leaves the child unplaced (see the Keystone projection).
	nv.Spec.TargetClusterRef = cp.Spec.Services.Nova.TargetClusterRef.DeepCopy()

	// Project the merged extraConfig (globalExtraConfig unioned with the
	// per-service block, per-service winning key by key). Assigned
	// unconditionally, following the revert-on-clear convention: a nil merge keeps
	// the SSA-applied intent free of spec.extraConfig, so a direct edit on the
	// child stays unowned until a ControlPlane block is set, and clearing the
	// ControlPlane block reverts the child rather than pinning the last value.
	nv.Spec.ExtraConfig = c5c3v1alpha1.MergedExtraConfig(
		cp.Spec.GlobalExtraConfig, cp.Spec.Services.Nova.ExtraConfig)

	// Point Nova at the SAME backing services the ControlPlane provisioned. The
	// two schemas are two logical databases on ONE instance: "nova_api" for the
	// global half and "nova" for the cell half, which the nova operator also
	// derives "nova_cell0" from. They are the schemas the pre-wired OpenBao engine
	// roles grant on. DeepCopy (over a plain struct copy) is required because
	// DatabaseSpec carries pointer fields, so a shallow copy would alias cp.Spec,
	// and each block takes a copy of its own so the two never share one.
	nv.Spec.APIDatabase = *database.DeepCopy()
	nv.Spec.APIDatabase.Database = defaultNovaAPIDatabaseName
	nv.Spec.Database = *database.DeepCopy()
	nv.Spec.Database.Database = defaultNovaDatabaseName

	// In managed mode the operator OWNS both nova DB credentials: reconcileNova
	// materialises them (above) into two per-ControlPlane Secrets. Override each
	// projected database block's secretRef to its own operator-owned Secret (key
	// "password"), and project the EFFECTIVE credentials mode onto both: Dynamic
	// (engine-issued) is the default on the managed shared database, a per-service
	// override or the shared Static opt-out flips it to Static, and a dedicated
	// nova database stays Static (no engine role can mint its credentials).
	// Brownfield (ClusterRef nil) leaves the user-supplied secretRef and
	// credentialsMode in place on both.
	if database.ClusterRef != nil {
		nv.Spec.APIDatabase.SecretRef = commonv1.SecretRefSpec{
			Name: novaAPIDBCredentialSecretName(cp), Key: "password",
		}
		nv.Spec.Database.SecretRef = commonv1.SecretRefSpec{
			Name: novaCellDBCredentialSecretName(cp), Key: "password",
		}
		mode := commonv1.CredentialsModeStatic
		if dynamic {
			mode = commonv1.CredentialsModeDynamic
		}
		nv.Spec.APIDatabase.CredentialsMode = mode
		nv.Spec.Database.CredentialsMode = mode
	}

	nv.Spec.Cache = *cache.DeepCopy()

	// The shared bus reaches the child as a BROWNFIELD secretRef naming the Secret
	// reconcileServiceMessaging wrote beside it: the nova operator resolves
	// spec.messaging in the Nova's own namespace on the Nova's own cluster, which
	// is where that delivery lands. The CA mirror follows the same rule, and only
	// when the bus declares TLS at all.
	nv.Spec.Messaging = serviceMessagingSpec(cp, novaMessagingTarget(cp))

	// The Keystone endpoint is derived top-down from the ControlPlane rather than
	// read from the Keystone child's status: no machine consumer reads status
	// endpoints per the settled convention. keystonePublicEndpoint is the
	// browser/client-facing URL Nova advertises on a 401 (empty when Keystone is
	// not externally exposed, in which case the child falls back to the internal
	// endpoint).
	nv.Spec.KeystoneEndpoint = novaKeystoneEndpoint(cp)
	nv.Spec.KeystonePublicEndpoint = keystonePublicEndpoint(cp.Spec.Services.Keystone)

	// The remote compute contract is addressed at the public Keystone URL and the
	// external bus URL delivered above. Assigned unconditionally, so clearing the
	// ControlPlane block reverts the child and the nova operator takes the remote
	// contract down.
	nv.Spec.RemoteCompute = novaRemoteComputeSpec(cp)

	nv.Spec.Region = cp.Spec.Region

	// The Keystone service user Nova authenticates as, the account the
	// registration child provisions: user and project as declared on that child,
	// both domains the ControlPlane's effective admin domain (which the
	// registration resolves the same way, its own domainName being unset), and the
	// password read from the consumer Secret the registration delivers.
	nv.Spec.ServiceUser = novav1alpha1.ServiceUserSpec{
		Username:          c5c3v1alpha1.NovaServiceAccountName,
		ProjectName:       c5c3v1alpha1.NovaServiceProjectName,
		UserDomainName:    adminDomainName(cp),
		ProjectDomainName: adminDomainName(cp),
		SecretRef: commonv1.SecretRefSpec{
			Name: keystoneServiceCredentialsSecretName(child),
			Key:  "password",
		},
	}

	// Project the ControlPlane's RESOLVED store selection onto the Nova child so
	// it never falls back to its own shared-cluster-store default.
	nv.Spec.SecretStoreRef = effectiveControlPlaneStoreRefPtr(cp)

	// DeepCopy for the same aliasing reason as the databases above; a nil source
	// yields nil, clearing any previously-projected gateway so removal tears the
	// HTTPRoute down. The Nova CRD's GatewaySpec is an alias of the shared commonv1
	// type, so the ControlPlane's block is projected as it stands. The compute
	// service carries three of them, one per listener, and each is independent:
	// the API's here, the metadata API's below, and the console proxy's inside its
	// own block.
	nv.Spec.Gateway = cp.Spec.Services.Nova.Gateway.DeepCopy()

	// The metadata API, with the shared secret its caller signs requests with. The
	// reference is resolved rather than materialised, so a ControlPlane that stops
	// supplying its own Secret reverts to the generated one.
	nv.Spec.Metadata = novav1alpha1.NovaMetadataSpec{
		Deployment:      novav1alpha1.DeploymentSpec{Replicas: novav1alpha1.DefaultComponentReplicas},
		SharedSecretRef: effectiveNovaMetadataSharedSecretRef(cp),
		Gateway:         cp.Spec.Services.Nova.MetadataGateway.DeepCopy(),
	}
	if override := cp.Spec.Services.Nova.MetadataReplicas; override != nil {
		nv.Spec.Metadata.Deployment.Replicas = *override
	}

	// Resolve the four replica counts, then let the overrides win. Assigning
	// unconditionally means clearing an override reverts the child to the default
	// instead of leaving the previously-projected value pinned on the fetched
	// child.
	//
	// The metadata, scheduler and conductor counts are written EXPLICITLY, and
	// they are written to one rather than to the shared default of three. Their
	// deployment blocks are struct values, so the apply carries them whatever this
	// projection assigns, and the API server materializes the shared
	// DeploymentSpec default of three into every one of them on the wire before
	// the nova defaulting webhook runs. That webhook only reaches an ABSENT block,
	// so leaving these alone silently runs three metadata APIs, three schedulers
	// and three conductors where the nova operator's own standalone default is one
	// of each.
	nv.Spec.API.Deployment.Replicas = commonv1.DefaultReplicas
	if override := cp.Spec.Services.Nova.Replicas; override != nil {
		nv.Spec.API.Deployment.Replicas = *override
	}
	nv.Spec.Scheduler.Deployment.Replicas = novav1alpha1.DefaultComponentReplicas
	if override := cp.Spec.Services.Nova.SchedulerReplicas; override != nil {
		nv.Spec.Scheduler.Deployment.Replicas = *override
	}
	nv.Spec.Conductor.Deployment.Replicas = novav1alpha1.DefaultComponentReplicas
	if override := cp.Spec.Services.Nova.ConductorReplicas; override != nil {
		nv.Spec.Conductor.Deployment.Replicas = *override
	}

	// The console proxy takes three shapes, and the empty one is deliberate. An
	// absent services.nova.consoleProxy projects the ZERO block, which leaves both
	// the switch and the deployment absent on the wire and lets the nova defaulting
	// webhook enable the proxy at one replica, the standalone default. A disabled
	// proxy projects the switch and nothing else, because the Nova CRD rejects a
	// spec.consoleProxy.deployment written on a disabled proxy. An enabled one
	// carries the sizing and the listener it was given.
	nv.Spec.ConsoleProxy = novav1alpha1.NovaConsoleProxySpec{}
	if proxy := cp.Spec.Services.Nova.ConsoleProxy; proxy != nil {
		if proxy.Enabled != nil && !*proxy.Enabled {
			nv.Spec.ConsoleProxy = novav1alpha1.NovaConsoleProxySpec{Enabled: ptr.To(false)}
		} else {
			replicas := novav1alpha1.DefaultComponentReplicas
			if proxy.Replicas != nil {
				replicas = *proxy.Replicas
			}
			nv.Spec.ConsoleProxy = novav1alpha1.NovaConsoleProxySpec{
				Enabled:    ptr.To(true),
				Deployment: &novav1alpha1.DeploymentSpec{Replicas: replicas},
				Gateway:    proxy.Gateway.DeepCopy(),
			}
		}
	}

	// The two optional client sections follow their sibling blocks: a ControlPlane
	// that runs block storage lets Nova attach volumes, one that runs a key
	// manager lets it read an encrypted volume's key, and one that runs neither
	// serves every other request without them. Every endpoint override stays empty
	// on all five sections, Placement, Neutron and Glance included (decision D10 of
	// #1019): the catalog's internal rows already carry the managed in-cluster
	// URLs, so resolving through the catalog reaches exactly the addresses an
	// override would have pinned, and a placed service's rows follow it without a
	// second projection having to agree.
	nv.Spec.Endpoints = novav1alpha1.NovaEndpointsSpec{
		Cinder:   novav1alpha1.NovaOptionalEndpointSpec{Enabled: cp.Spec.Services.Cinder != nil},
		Barbican: novav1alpha1.NovaOptionalEndpointSpec{Enabled: cp.Spec.Services.Barbican != nil},
	}

	nv.Spec.DBArchive = (*novav1alpha1.DBArchiveSpec)(cp.Spec.Services.Nova.DBArchive.DeepCopy())

	// spec.networkPolicy, spec.autoscaling, spec.logging, spec.api.uwsgi and
	// spec.metadata.uwsgi are deliberately NOT set, the Placement posture: the
	// child-side defaults stay authoritative, and tuning them stays a
	// standalone-CR concern.

	res, err := commonreconcile.ProjectChild(ctx, r.Client, r.Scheme, cp,
		commonreconcile.ChildProjectionParams[*novav1alpha1.Nova]{
			Child:          nv,
			ConditionType:  conditionTypeNovaReady,
			ReadyReason:    "NovaReady",
			ReadyMessage:   "Projected Nova CR is ready",
			WaitingReason:  "WaitingForNova",
			WaitingMessage: fmt.Sprintf("Nova %q is not ready", nv.Name),
			// An Invalid (HTTP 422) rejection from the Nova API server means the
			// projected spec violates a CRD/webhook rule: surface a distinct,
			// actionable reason so the wedge is diagnosable from the condition.
			RejectedReason: "NovaProjectionRejected",
			RejectedMessage: func(err error) string {
				return fmt.Sprintf("Nova API server rejected the projected spec; reconcile the ControlPlane spec "+
					"to a valid projection to recover: %v", err)
			},
			ErrorReason:     "NovaError",
			ErrorMessage:    func(err error) string { return fmt.Sprintf("create-or-update Nova: %v", err) },
			WaitRequeue:     infraRequeueAfter,
			Conditions:      &cp.Status.Conditions,
			Generation:      cp.Generation,
			ChildConditions: func(n *novav1alpha1.Nova) []metav1.Condition { return n.Status.Conditions },
			Unowned:         crossNamespace,
		})
	if err != nil {
		return res, err
	}

	if !res.IsZero() {
		return res, nil
	}

	// The apply above is what removed spec.messaging.tls from the child when the
	// shared bus dropped its tls block, but only from the CR. The live Deployments
	// keep mounting the mirror as a REQUIRED Secret volume source until the
	// nova-operator has re-rendered them on a pass of its own, so the reap waits
	// for the child's own verdict on the pointer-free spec. See
	// pruneServiceMessagingCA, and the Cinder leg for the window this closes.
	if messagingCAMirrorReleasable(cp, nv.Spec.Messaging, nv.Status.ObservedGeneration, nv.Generation) {
		if pruneRes, halt, perr := r.pruneServiceMessagingCA(ctx, cp, novaMessagingTarget(cp)); halt {
			return pruneRes, perr
		}
	}

	// The delivered remote bus URL waits for the same verdict once
	// services.nova.remoteCompute is gone: the nova operator reads the Secret on
	// every pass until it has reconciled the child without spec.remoteCompute.
	if reapRes, halt, rerr := r.reapNovaRemoteMessaging(ctx, cp, nv); halt {
		return reapRes, rerr
	}

	// The generated metadata shared secret waits for the same verdict once the
	// ControlPlane names a Secret of its own. The apply above re-pointed the CR,
	// but the live metadata Deployment sources its env from the generated Secret
	// through a non-optional secretKeyRef until the nova operator has re-rendered
	// it, and that operator parks on the new Secret (WaitingForMetadataSharedSecret)
	// before its Deployment step for as long as the Secret is absent. Reaping
	// ahead of the child's own verdict would take the old Secret away in exactly
	// that window, and every metadata pod restarted in it would fail to start.
	if nv.Status.ObservedGeneration >= nv.Generation {
		if reapRes, halt, rerr := r.reapGeneratedNovaMetadataSecret(ctx, cp); halt {
			return reapRes, rerr
		}
	}

	// The child is ready, so the compute contract it publishes exists and can be
	// carried to the namespaces the compute nodes read it from. A target that
	// cannot be served parks NovaReady on the reason the mirror returned: the
	// control plane is up, but a compute cluster that never receives the contract
	// registers no hypervisor, and the plane must not report the compute service
	// as ready for it.
	targets, err := r.novaComputeConfigMirrorTargets(ctx, cp)
	if err != nil {
		conditionFailer(cp, conditionTypeNovaReady)("NovaComputeConfigError", err.Error())
		return ctrl.Result{}, err
	}
	for _, target := range targets {
		ok, reason, message, err := r.mirrorNovaComputeConfig(ctx, cp, target)
		if err != nil {
			conditionFailer(cp, conditionTypeNovaReady)("NovaComputeConfigError", err.Error())
			return ctrl.Result{}, err
		}
		if !ok {
			logger.Info("compute config not deliverable, deferring Nova readiness",
				"namespace", target.Namespace, "reason", reason)
			conditionFailer(cp, conditionTypeNovaReady)(reason, message)
			return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
		}
	}

	// The hypervisor operator's account follows the compute service to the same
	// targets as the contract, or is pruned while its block is unset. It runs
	// after the Nova child is ready, so a stuck account holds NovaReady only.
	hvoLeg := r.pruneNovaHypervisorOperator
	if cp.Spec.Services.Nova.HypervisorOperator != nil {
		hvoLeg = r.reconcileNovaHypervisorOperator
	}
	if hvoRes, halt, err := hvoLeg(ctx, cp, targets); halt {
		return hvoRes, err
	}

	// The metadata agents on target clusters sign with the shared secret the
	// contract carries, so the copy follows the contract. A copy that cannot be
	// delivered holds NovaReady: instances on that cluster boot without metadata.
	if agentRes, halt, err := r.reconcileNovaMetadataAgentSecrets(ctx, cp); halt {
		return agentRes, err
	}

	// The Nova child is ready. NovaReady still folds in the registration: a running
	// Nova whose catalog entry never landed is reachable by nothing that discovers
	// it through the catalog, and the ControlPlane must not report the compute
	// service as ready for it.
	if readyRes, pending := foldBuiltinRegistrationReady(cp, child, conditionTypeNovaReady); pending {
		return readyRes, nil
	}
	return res, nil
}

// deleteOrphanedNova removes a previously-projected Nova child, the two
// DB-credential chains, the generated metadata shared secret, the three messaging
// Secrets, the KeystoneService registration, and the hypervisor operator's
// registration and auth Secret that follow it, when
// spec.services.nova is unset AND the ControlPlane has opted in to deletion via
// novaDeletionAllowedAnnotation (the caller gates this). Each object is only
// deleted when this ControlPlane still owns it (by owner reference in its own
// namespace, by the ownership labels in a service namespace); a foreign object
// colliding on a name is left alone.
//
// Deleting the registration is what removes Nova from the Keystone catalog and
// from the identity plane: the KeystoneService controller's finalizer tears down
// the catalog rows, the service user and its project behind it.
func (r *ControlPlaneReconciler) deleteOrphanedNova(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	novaNS := cp.NovaNamespace()

	children := []client.Object{
		// The Nova child.
		&novav1alpha1.Nova{
			ObjectMeta: metav1.ObjectMeta{Name: novaName(cp), Namespace: novaNS},
		},
	}

	// Both DB-credential chains: the generator-backed ExternalSecret, the
	// VaultDynamicSecret generator behind it, its mTLS client Certificate, and the
	// ServiceAccount whose token it authenticates with. The Certificates have no Go
	// type, so they are addressed unstructured.
	for _, target := range []dbCredentialTarget{
		novaAPIDBCredentialTarget(cp),
		novaCellDBCredentialTarget(cp),
	} {
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certificateGVK)
		cert.SetName(target.certName)
		cert.SetNamespace(novaNS)
		children = append(children,
			&esov1.ExternalSecret{
				ObjectMeta: metav1.ObjectMeta{Name: target.secretName, Namespace: novaNS},
			},
			&esgenv1alpha1.VaultDynamicSecret{
				ObjectMeta: metav1.ObjectMeta{Name: target.secretName, Namespace: novaNS},
			},
			cert,
			&corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{Name: target.saName, Namespace: novaNS},
			},
		)
	}

	// The bus delivery: the brownfield transport-URL Secret, the CA mirror beside
	// it, and the external bus URL the remote compute contract carries. Nothing
	// else writes them, so an unmanaged service leaves no broker credential behind
	// in the namespace.
	children = append(children, serviceMessagingSecrets(novaMessagingTarget(cp))...)
	children = append(children, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: novaRemoteMessagingSecretName(cp), Namespace: novaNS},
	})

	// The KeystoneService registration. It lives beside the service, on the
	// management cluster whatever cluster Nova runs on. The credential mirror a
	// PLACED service carries is not swept here: like every object this function
	// names it is resolved through NovaNamespace(), which without a services.nova
	// block is the ControlPlane's own namespace, so this sweep reaches co-located
	// objects only. The mirror is reaped by the ControlPlane teardown, which sweeps
	// a placed namespace's label-owned ExternalSecrets on the target cluster.
	children = append(children, &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: novaName(cp), Namespace: novaNS},
	})

	// The hypervisor operator's registration and its auth Secret, reached the
	// same way. The copies on the compute clusters are each NovaCompute
	// teardown's to reap, like the contract mirrors.
	children = append(children,
		&c5c3v1alpha1.KeystoneService{
			ObjectMeta: metav1.ObjectMeta{Name: novaHypervisorOperatorRegistrationName(cp), Namespace: novaNS},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: novaHypervisorOperatorAuthSecretName(cp), Namespace: novaNS},
		},
	)

	for _, child := range children {
		if err := commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, child, func(live client.Object) bool {
			return isControlPlaneChild(live, cp)
		}); err != nil {
			return err
		}
	}

	// The generated metadata shared secret: the ExternalSecret and the Password
	// generator behind it, the latter read uncached (see
	// deleteGeneratedNovaMetadataSecret). The Secret they materialise carries ESO's
	// own owner reference, so it comes down with the ExternalSecret.
	return r.deleteGeneratedNovaMetadataSecret(ctx, r.Client, cp)
}
