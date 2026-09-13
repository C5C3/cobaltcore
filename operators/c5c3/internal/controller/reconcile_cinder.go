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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// The projected Cinder CR is named "{controlplane.Name}-cinder", the same
// deterministic, collision-free naming convention as the Keystone, Horizon,
// Glance, Placement, Barbican, and Neutron children (see keystoneNameSuffix), and
// lives in cp.CinderNamespace(): the ControlPlane's own namespace by default, or
// the one services.cinder.namespace assigns.

// cinderNameSuffix is appended to the ControlPlane name to derive the name of the
// projected Cinder CR (and, through it, its credential and registration objects).
const cinderNameSuffix = "-cinder"

// cinderAPIPort is the port the cinder operator's API Service listens on.
const cinderAPIPort int32 = 8776

// defaultCinderRepository is the canonical Cinder image repository; the tag is
// derived from spec.openStackRelease unless spec.services.cinder.image overrides
// the whole image reference.
const defaultCinderRepository = "ghcr.io/c5c3/cinder"

// cinderDeletionAllowedAnnotation, when set to a truthy value on a ControlPlane,
// opts that ControlPlane in to tearing down a previously-projected Cinder child
// (with its satellites, its DB-credential ExternalSecret, and its messaging
// Secrets) when spec.services.cinder is unset. The preserve-by-default posture
// mirrors the Keystone/Horizon/Glance/Placement/Barbican/Neutron annotations for a
// consistent operator UX: an accidental block drop never silently removes a
// running service.
const cinderDeletionAllowedAnnotation = "c5c3.io/allow-cinder-deletion"

// defaultCinderDatabaseName is the logical database name the Cinder schema always
// lives in, regardless of whether Cinder shares the ControlPlane's database
// cluster or takes a dedicated one. It is also the one schema the pre-wired
// OpenBao engine role grants on, so it is not a per-ControlPlane knob.
const defaultCinderDatabaseName = "cinder"

// cinderName returns the name of the Cinder CR the reconciler projects for the
// given ControlPlane (see cinderNameSuffix).
func cinderName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + cinderNameSuffix
}

// cinderDeletionAllowed reports whether cp opts in to deleting its projected
// Cinder child when spec.services.cinder is unset, via a truthy
// cinderDeletionAllowedAnnotation. A missing, malformed, or non-truthy value means
// "preserve".
func cinderDeletionAllowed(cp *c5c3v1alpha1.ControlPlane) bool {
	allowed, err := strconv.ParseBool(cp.Annotations[cinderDeletionAllowedAnnotation])
	return err == nil && allowed
}

// cinderMessagingTarget names the block-storage service as a consumer of the
// shared bus: the Secrets are named after the Cinder child, written in its
// namespace, and every condition lands on CinderReady.
func cinderMessagingTarget(cp *c5c3v1alpha1.ControlPlane) serviceMessagingTarget {
	return serviceMessagingTarget{
		Service:       "Cinder",
		ChildName:     cinderName(cp),
		Namespace:     cp.CinderNamespace(),
		ConditionType: conditionTypeCinderReady,
	}
}

// cinderMessagingSecretName returns the name of the brownfield transport-URL
// Secret the ControlPlane writes beside the Cinder child ("<cp>-cinder-messaging").
//
// The name is the ControlPlane's own on purpose. In that same namespace the cinder
// operator claims messaging.TransportURLSecretName(cinderName(cp))
// ("<cp>-cinder-transport-url") for the Secret it derives from
// spec.messaging.secretRef, so writing the bus under that name would leave two
// controllers rewriting one object on every pass.
func cinderMessagingSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return serviceMessagingSecretName(cinderMessagingTarget(cp))
}

// cinderMessagingCASecretName returns the name of the CA mirror that carries the
// broker's CA bundle into the Cinder namespace ("<cp>-cinder-messaging-ca"), under
// the same name-of-our-own rule as cinderMessagingSecretName.
func cinderMessagingCASecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return serviceMessagingCASecretName(cinderMessagingTarget(cp))
}

// cinderKeystoneEndpoint returns the Keystone endpoint URL projected into the
// Cinder child's spec.keystoneEndpoint. Cinder validates every token server-side
// against this URL, so what the Cinder pods can reach decides it: the cluster
// Cinder is placed on against the one Keystone runs on, the rule
// keystoneEndpointFor holds for every service.
func cinderKeystoneEndpoint(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneEndpointFor(cp, cp.CinderTargetClusterRef())
}

// cinderEndpointURL renders the in-cluster URL of the projected Cinder API
// Service by naming convention, the cross-service endpoint contract the catalog
// registers against. It carries no path: the catalog rows append the "/v3" the
// block-storage API is served under (see cinderCatalogURL).
func cinderEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return managedServiceURL(cinderName(cp), cp.CinderNamespace(), cinderAPIPort, "")
}

// cinderBackendChildren names the projected CinderBackend satellites in
// namespace for the shared prune and sweep; keep lists the names never touched.
//
// The prefix is empty, which selects every CinderBackend in the namespace this
// ControlPlane owns and nothing else: a satellite carries the entry's bare name
// (a rename would strand the volumes already on the backend), so there is no
// prefix to recognise it by, and the ownership check the sweep applies on top is
// what keeps a hand-created backend out of it. The two satellite kinds get one
// selector each, so their sweeps stay apart.
func cinderBackendChildren(namespace string, keep map[string]struct{}) projectedChildren {
	return projectedChildren{
		List:      &cinderv1alpha1.CinderBackendList{},
		Kind:      "CinderBackend",
		Namespace: namespace,
		Prefix:    "",
		Keep:      keep,
	}
}

// cinderBackupBackendChildren names the projected CinderBackupBackend satellite
// in namespace for the shared prune and sweep, under the empty-prefix rule
// cinderBackendChildren documents; keep lists the names never touched.
func cinderBackupBackendChildren(namespace string, keep map[string]struct{}) projectedChildren {
	return projectedChildren{
		List:      &cinderv1alpha1.CinderBackupBackendList{},
		Kind:      "CinderBackupBackend",
		Namespace: namespace,
		Prefix:    "",
		Keep:      keep,
	}
}

// cinderBackendForEntry builds the CinderBackend satellite projected from one
// services.cinder.backends entry, named after the entry itself. An unset
// mountOptions serializes away (omitempty) so the CinderBackend CRD's own default
// applies at exactly one layer.
//
// The nfs block is copied only when the entry carries one. The CRD's union rule
// requires it for type NFS, so a nil block reaches here only on a
// webhook-and-schema-bypassed CR, where leaving the satellite's field unset has
// the satellite's own admission reject it rather than this projection panicking.
func cinderBackendForEntry(
	cp *c5c3v1alpha1.ControlPlane, entry c5c3v1alpha1.CinderBackendEntry, cinderNS string,
) *cinderv1alpha1.CinderBackend {
	backend := &cinderv1alpha1.CinderBackend{
		ObjectMeta: metav1.ObjectMeta{Name: entry.Name, Namespace: cinderNS},
		Spec: cinderv1alpha1.CinderBackendSpec{
			CinderRef: cinderv1alpha1.CinderRefSpec{Name: cinderName(cp)},
			Type:      cinderv1alpha1.CinderBackendTypeNFS,
		},
	}
	if entry.NFS != nil {
		backend.Spec.NFS = &cinderv1alpha1.NFSBackendSpec{
			Server:       entry.NFS.Server,
			Path:         entry.NFS.Path,
			MountOptions: entry.NFS.MountOptions,
		}
	}
	// DeepCopied for its pointer bounds, so the projected satellite never aliases
	// cp.Spec.
	if cache := entry.ImageVolumeCache.DeepCopy(); cache != nil {
		backend.Spec.ImageVolumeCache = &cinderv1alpha1.ImageVolumeCacheSpec{
			Enabled:   cache.Enabled,
			MaxSizeGB: cache.MaxSizeGB,
			MaxCount:  cache.MaxCount,
		}
	}
	return backend
}

// cinderBackupBackendForEntry builds the CinderBackupBackend satellite projected
// from services.cinder.backupBackend, named after the entry itself. An unset
// fileSize or compression serializes away (omitempty) so the CinderBackupBackend
// CRD's own defaults apply at exactly one layer; the nfs block follows the rule
// cinderBackendForEntry documents.
func cinderBackupBackendForEntry(
	cp *c5c3v1alpha1.ControlPlane, entry c5c3v1alpha1.CinderBackupBackendEntry, cinderNS string,
) *cinderv1alpha1.CinderBackupBackend {
	backupBackend := &cinderv1alpha1.CinderBackupBackend{
		ObjectMeta: metav1.ObjectMeta{Name: entry.Name, Namespace: cinderNS},
		Spec: cinderv1alpha1.CinderBackupBackendSpec{
			CinderRef:   cinderv1alpha1.CinderRefSpec{Name: cinderName(cp)},
			Type:        cinderv1alpha1.CinderBackupBackendTypeNFS,
			Compression: entry.Compression,
		},
	}
	if entry.NFS != nil {
		backupBackend.Spec.NFS = &cinderv1alpha1.NFSBackupBackendSpec{
			Server:       entry.NFS.Server,
			Path:         entry.NFS.Path,
			MountOptions: entry.NFS.MountOptions,
		}
	}
	// The value, not the pointer: the projected satellite must not alias cp.Spec.
	if entry.FileSize != nil {
		backupBackend.Spec.FileSize = ptr.To(*entry.FileSize)
	}
	return backupBackend
}

// reconcileCinderBackends projects one CinderBackend satellite per
// services.cinder.backends entry and prunes previously-projected satellites whose
// entry was removed.
//
// Every write routes through ensureProjectedSatellite, which refuses to adopt a
// same-named object this ControlPlane did not create in ANY namespace, its own
// included: a satellite carries the entry's bare name, which is also what a person
// naming a hand-made CinderBackend picks. The prune sweep deletes only c5c3-owned
// satellites, so a hand-created CinderBackend attached to the same Cinder is
// neither pruned nor overwritten.
//
// A pruned CinderBackend keeps the cinder operator's service-remove finalizer
// until its detach Job has run, so the object outlives this call while cinder
// takes the backend out of service.
func (r *ControlPlaneReconciler) reconcileCinderBackends(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, cinderNS string,
) error {
	declared := make(map[string]struct{}, len(cp.Spec.Services.Cinder.Backends))
	for i := range cp.Spec.Services.Cinder.Backends {
		backend := cinderBackendForEntry(cp, cp.Spec.Services.Cinder.Backends[i], cinderNS)
		if err := r.ensureProjectedSatellite(ctx, r.Client, cp, backend); err != nil {
			return fmt.Errorf("projecting CinderBackend %q: %w", backend.Name, err)
		}
		declared[backend.Name] = struct{}{}
	}

	return r.pruneProjectedChildren(ctx, cp, cinderBackendChildren(cinderNS, declared))
}

// reconcileCinderBackupBackend projects the CinderBackupBackend satellite
// services.cinder.backupBackend declares and prunes a previously-projected one
// once the block is removed, which is what drops the backup service from the
// plane. It carries the ownership rules reconcileCinderBackends documents.
func (r *ControlPlaneReconciler) reconcileCinderBackupBackend(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, cinderNS string,
) error {
	var keep map[string]struct{}
	if entry := cp.Spec.Services.Cinder.BackupBackend; entry != nil {
		backupBackend := cinderBackupBackendForEntry(cp, *entry, cinderNS)
		if err := r.ensureProjectedSatellite(ctx, r.Client, cp, backupBackend); err != nil {
			return fmt.Errorf("projecting CinderBackupBackend %q: %w", backupBackend.Name, err)
		}
		keep = map[string]struct{}{backupBackend.Name: {}}
	}

	return r.pruneProjectedChildren(ctx, cp, cinderBackupBackendChildren(cinderNS, keep))
}

// reconcileCinder projects spec.services.cinder into an owned Cinder CR with its
// CinderBackend and CinderBackupBackend satellites, and drives the CinderReady
// condition.
//
// The sub-reconciler is GATED on KeystoneReady (Cinder validates every token
// against the ControlPlane's Keystone child) and on the KeystoneService child it
// projects for Cinder (Cinder authenticates as the Keystone user that registration
// provisions). Once gated through, it delivers the shared message bus into the
// block-storage service's namespace, ensures the DB-credential ExternalSecret,
// projects the satellites and then the Cinder CR (database/cache DeepCopied from
// the resolved backing services, the Keystone endpoint derived top-down through
// cinderKeystoneEndpoint), and folds both children's readiness into CinderReady.
func (r *ControlPlaneReconciler) reconcileCinder(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// spec.services.cinder is optional. When unset, this ControlPlane manages no
	// block-storage service and reports CinderReady as not-managed so the aggregate
	// Ready condition is not blocked (staged adoption). A previously-projected child
	// is preserved unless the ControlPlane opts in to deletion.
	if cp.Spec.Services.Cinder == nil {
		message := "spec.services.cinder is unset; no Cinder service is managed by this ControlPlane"
		if cinderDeletionAllowed(cp) {
			if err := r.deleteOrphanedCinder(ctx, cp); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			// Preserve the child, but NEVER the credential minter. A live
			// VaultDynamicSecret keeps issuing a fresh MySQL user with ALL PRIVILEGES
			// on the cinder schema at every refresh interval, indefinitely, for a
			// service this ControlPlane has been told it no longer manages: no
			// consumer, no revocation, and a CinderReady=True/CinderNotManaged
			// condition that surfaces none of it. Preserving a running service does
			// not imply preserving the generator behind its credentials, so the
			// dynamic objects come down either way.
			//
			// CinderNamespace() still resolves correctly here: removing a
			// services.cinder.namespace assignment is rejected by
			// validateServiceNamespacesImmutable, so the only admissible way to reach
			// this branch with a live generator is the co-located one, where the
			// generator sits in the ControlPlane's own namespace.
			r.deleteDynamicDBCredentialObjects(ctx, cp, cinderDBCredentialTarget(cp))
			message += fmt.Sprintf("; any previously-projected Cinder child is preserved "+
				"(set annotation %s=true to allow deletion), but its dynamic DB-credential generator is torn down",
				cinderDeletionAllowedAnnotation)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCinderReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "CinderNotManaged",
			Message:            message,
		})
		return ctrl.Result{}, nil
	}

	// Resolve the backing services Cinder actually talks to: its own dedicated
	// instances when it opted into them, the ControlPlane-wide shared ones
	// otherwise.
	//
	// Nil-safety fail-safe. The projection DeepCopies these, so an unresolvable
	// instance has nothing to project and the deref below would panic; the shared
	// bus is dereferenced for its TLS block on the same path. The validating webhook
	// requires spec.infrastructure with a messaging block outside External mode (and
	// forbids services.cinder in External mode), so this only fires for a
	// webhook-bypassed CR.
	database := effectiveCinderDatabase(cp)
	cache := effectiveCinderCache(cp)
	if database == nil || cache == nil ||
		cp.Spec.Infrastructure == nil || cp.Spec.Infrastructure.Messaging == nil {
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// Gate on KeystoneReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeKeystoneReady) {
		logger.Info("Keystone not ready, deferring Cinder projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCinderReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForKeystone",
			Message:            "KeystoneReady is not True; Cinder projection deferred",
		})
		return ctrl.Result{RequeueAfter: keystoneInfraGateRequeueAfter}, nil
	}

	// Deliver the ControlPlane-wide bus into the namespace (and onto the cluster)
	// the block-storage service runs in. The transport URL's digest is not
	// projected: the cinder operator rolls its pods off the Secret it derives
	// itself, so a second digest on the child would only add a redundant rollout
	// trigger.
	if msgRes, halt, err := r.reconcileServiceMessaging(ctx, cp, cinderMessagingTarget(cp)); halt {
		return msgRes, err
	}

	// Register Cinder against the identity plane: one KeystoneService child carrying
	// the block-storage catalog entry and the service account Cinder authenticates
	// as, mirrored onto a placed Cinder's cluster and gated on the account it
	// provisions.
	child, regRes, halt, err := r.reconcileBuiltinRegistration(ctx, cp, desiredCinderRegistration(cp),
		"Cinder", conditionTypeCinderReady)
	if halt {
		return regRes, err
	}

	// The EFFECTIVE credentials mode of the database Cinder connects to, resolved
	// once so the credential projection below, its readiness gate, and the mode
	// stamped onto the child further down can never disagree.
	dynamic := database.ClusterRef != nil && cinderDBCredentialsDynamicEnabled(cp)

	// Ensure the DB-credential objects BEFORE the child so the Secret it references
	// exists when the cinder-operator resolves it. Managed only: a brownfield
	// database (ClusterRef nil) carries a user-supplied credential out-of-band, so
	// there is nothing for the operator to project. In Dynamic mode the shared
	// helper also holds the projection until an engine-issued credential has landed
	// (see ensureServiceDBCredential).
	if database.ClusterRef != nil {
		res, halt, err := r.ensureServiceDBCredential(ctx, cp, cinderDBCredentialTarget(cp),
			dynamic, "Cinder", conditionTypeCinderReady)
		if halt {
			return res, err
		}
	}

	// Resolve the Cinder image. spec.services.cinder.image overrides the
	// release-derived default when set.
	image := commonv1.ImageSpec{
		Repository: defaultCinderRepository,
		Tag:        cp.Spec.OpenStackRelease,
	}
	if override := cp.Spec.Services.Cinder.Image; override != nil {
		image = *override
	}

	// Place the child in the namespace assigned to the block-storage service (the
	// ControlPlane's own unless services.cinder.namespace says otherwise). A child
	// outside the ControlPlane's namespace can carry no owner reference, so it is
	// stamped with the ownership labels and applied unowned.
	cinderNS := cp.CinderNamespace()
	crossNamespace := cinderNS != cp.Namespace
	cn := &cinderv1alpha1.Cinder{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cinderName(cp),
			Namespace: cinderNS,
		},
	}
	if crossNamespace {
		stampControlPlaneChildLabels(cn, cp)
	}

	// Project the volume backends and the backup driver as satellites of their own
	// kinds. A satellite references its Cinder by name (inverted attachment), so the
	// order relative to the child apply below is immaterial to the cinder operator,
	// but a satellite that could not be written has to halt the pass here: a child
	// applied behind it would run with a backend set the ControlPlane never
	// projected.
	err = r.reconcileCinderBackends(ctx, cp, cinderNS)
	if err == nil {
		err = r.reconcileCinderBackupBackend(ctx, cp, cinderNS)
	}
	if err != nil {
		reason := "CinderBackendError"
		message := fmt.Sprintf("projecting the Cinder satellites: %v", err)
		if apierrors.IsInvalid(err) {
			reason = "CinderBackendProjectionRejected"
			message = fmt.Sprintf("Cinder API server rejected a projected satellite; reconcile the "+
				"services.cinder.backends / services.cinder.backupBackend entries to a valid projection "+
				"to recover: %v", err)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCinderReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             reason,
			Message:            message,
		})
		return ctrl.Result{}, err
	}

	cn.Spec.OpenStackRelease = cp.Spec.OpenStackRelease
	cn.Spec.Image = image

	// Thread the service's target cluster onto the child verbatim; a nil source
	// yields nil and leaves the child unplaced (see the Keystone projection).
	cn.Spec.TargetClusterRef = cp.Spec.Services.Cinder.TargetClusterRef.DeepCopy()

	// Project the merged extraConfig (globalExtraConfig unioned with the per-service
	// block, per-service winning key by key). Assigned unconditionally, following
	// the revert-on-clear convention: a nil merge keeps the SSA-applied intent free
	// of spec.extraConfig, so a direct edit on the child stays unowned until a
	// ControlPlane block is set, and clearing the ControlPlane block reverts the
	// child rather than pinning the last value.
	cn.Spec.ExtraConfig = c5c3v1alpha1.MergedExtraConfig(
		cp.Spec.GlobalExtraConfig, cp.Spec.Services.Cinder.ExtraConfig)

	// Point Cinder at the SAME backing services the ControlPlane provisioned. The
	// logical database is always "cinder": its own schema keeps it isolated from
	// Keystone's on a shared cluster, and it is the one schema the pre-wired OpenBao
	// engine role grants on. DeepCopy (over a plain struct copy) is required because
	// DatabaseSpec/CacheSpec carry pointer fields, so a shallow copy would alias
	// cp.Spec.
	cn.Spec.Database = *database.DeepCopy()
	cn.Spec.Database.Database = defaultCinderDatabaseName
	// In managed mode the operator OWNS the cinder DB credential: reconcileCinder
	// materialises it (above) into a per-ControlPlane Secret named
	// cinderDBCredentialSecretName(cp). Override the projected Cinder CR's
	// database.secretRef to that operator-owned Secret (key "password"), and project
	// the EFFECTIVE credentials mode: Dynamic (engine-issued) is the default on the
	// managed shared database, a per-service override or the shared Static opt-out
	// flips it to Static, and a dedicated cinder database stays Static (no engine
	// role can mint its credentials). Brownfield (ClusterRef nil) leaves the
	// user-supplied secretRef and credentialsMode in place.
	if database.ClusterRef != nil {
		cn.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: cinderDBCredentialSecretName(cp), Key: "password"}
		if dynamic {
			cn.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
		} else {
			cn.Spec.Database.CredentialsMode = commonv1.CredentialsModeStatic
		}
	}

	cn.Spec.Cache = *cache.DeepCopy()

	// The Keystone endpoint is derived top-down from the ControlPlane rather than
	// read from the Keystone child's status: no machine consumer reads status
	// endpoints per the settled convention. keystonePublicEndpoint is the
	// browser/client-facing URL Cinder advertises on a 401 (empty when Keystone is
	// not externally exposed, in which case the child falls back to the internal
	// endpoint).
	cn.Spec.KeystoneEndpoint = cinderKeystoneEndpoint(cp)
	cn.Spec.KeystonePublicEndpoint = keystonePublicEndpoint(cp.Spec.Services.Keystone)

	cn.Spec.Region = cp.Spec.Region

	// The Keystone service user Cinder authenticates as, the account the
	// registration child provisions: user and project as declared on that child,
	// both domains the ControlPlane's effective admin domain (which the registration
	// resolves the same way, its own domainName being unset), and the password read
	// from the consumer Secret the registration delivers.
	cn.Spec.ServiceUser = &cinderv1alpha1.ServiceUserSpec{
		Username:          c5c3v1alpha1.CinderServiceAccountName,
		ProjectName:       c5c3v1alpha1.CinderServiceProjectName,
		UserDomainName:    adminDomainName(cp),
		ProjectDomainName: adminDomainName(cp),
		SecretRef: commonv1.SecretRefSpec{
			Name: keystoneServiceCredentialsSecretName(child),
			Key:  "password",
		},
	}

	// Project the ControlPlane's RESOLVED store selection onto the Cinder child so
	// it never falls back to its own shared-cluster-store default.
	cn.Spec.SecretStoreRef = effectiveControlPlaneStoreRefPtr(cp)

	// DeepCopy for the same aliasing reason as Database above; a nil source yields
	// nil, clearing any previously-projected gateway so removal tears the HTTPRoute
	// down. The Cinder CRD's GatewaySpec is an alias of the shared commonv1 type, so
	// the ControlPlane's block is projected as it stands.
	cn.Spec.Gateway = cp.Spec.Services.Cinder.Gateway.DeepCopy()

	// Resolve API replicas to the shared operator default, then let an override win.
	// Assigning unconditionally means clearing services.cinder.replicas reverts the
	// child to the default instead of leaving the previously-projected value pinned
	// on the fetched child.
	cn.Spec.API.Deployment.Replicas = commonv1.DefaultReplicas
	if cp.Spec.Services.Cinder.Replicas != nil {
		cn.Spec.API.Deployment.Replicas = *cp.Spec.Services.Cinder.Replicas
	}

	// The shared bus reaches the child as a BROWNFIELD secretRef naming the Secret
	// reconcileServiceMessaging wrote beside it: the cinder operator resolves
	// spec.messaging in the Cinder's own namespace on the Cinder's own cluster,
	// which is where that delivery lands. The CA mirror follows the same rule, and
	// only when the bus declares TLS at all.
	cn.Spec.Messaging = serviceMessagingSpec(cp, cinderMessagingTarget(cp))

	// Glance and Barbican are SIBLINGS, not gates. Cinder creates a volume from an
	// image through the Glance endpoint and stores volume-encryption keys through
	// the Barbican one, and it serves every other request without either: a Cinder
	// with an empty glanceEndpoint and no keyManager runs, and the pass that follows
	// a sibling block being declared re-renders the child with it.
	cn.Spec.GlanceEndpoint = ""
	if cp.Spec.Services.Glance != nil {
		cn.Spec.GlanceEndpoint = glanceEndpointURL(cp)
	}
	cn.Spec.KeyManager = nil
	if cp.Spec.Services.Barbican != nil {
		cn.Spec.KeyManager = &cinderv1alpha1.KeyManagerSpec{
			Type:     cinderv1alpha1.KeyManagerTypeBarbican,
			Barbican: &cinderv1alpha1.BarbicanKeyManagerSpec{Endpoint: barbicanEndpointURL(cp)},
		}
	}

	// The internal tenant the image-volume cache owns its cached volumes as
	// (decision D10 of #979). Cinder passes both IDs to the volume API without
	// resolving them through Keystone, so only the ones Keystone actually assigned
	// are projectable: they are read off the registration child's account status,
	// which is the one place they exist, and an account that has not published them
	// yet leaves the block unset rather than naming a project that does not exist.
	cn.Spec.InternalTenant = nil
	if account := child.Status.Account; account != nil && account.ProjectID != "" && account.UserID != "" {
		cn.Spec.InternalTenant = &cinderv1alpha1.InternalTenantSpec{
			ProjectID: account.ProjectID,
			UserID:    account.UserID,
		}
	}

	// The volume and backup Deployments run exactly one replica, which the Cinder
	// CRD's CEL rules require: the NFS drivers refuse a second process under the
	// same host identity. The scheduler runs one because that is what the cinder
	// operator's defaulting webhook gives a standalone CR; no CEL rule guards it.
	// All three blocks are struct values rather than pointers, so the apply carries
	// them whatever this projection assigns, and the API server materializes the
	// shared DeploymentSpec default of three into the replicas of every deployment
	// block on the wire before that webhook runs. Writing the one replica here is
	// what keeps the projected child admissible: leaving the volume and backup
	// blocks alone has the Cinder API server reject the child on every pass, and
	// leaving the scheduler block alone has it silently run three schedulers.
	cn.Spec.Scheduler.Deployment.Replicas = 1
	cn.Spec.Volume.Deployment.Replicas = 1
	cn.Spec.Backup.Deployment.Replicas = 1

	// spec.dbPurge, spec.networkPolicy, spec.autoscaling, spec.logging,
	// spec.api.uwsgi and spec.policyOverrides are deliberately NOT set, the
	// Placement posture: the child-side defaults stay authoritative, and tuning
	// them stays a standalone-CR concern.

	res, err := commonreconcile.ProjectChild(ctx, r.Client, r.Scheme, cp,
		commonreconcile.ChildProjectionParams[*cinderv1alpha1.Cinder]{
			Child:          cn,
			ConditionType:  conditionTypeCinderReady,
			ReadyReason:    "CinderReady",
			ReadyMessage:   "Projected Cinder CR is ready",
			WaitingReason:  "WaitingForCinder",
			WaitingMessage: fmt.Sprintf("Cinder %q is not ready", cn.Name),
			// An Invalid (HTTP 422) rejection from the Cinder API server means the
			// projected spec violates a CRD/webhook rule: surface a distinct,
			// actionable reason so the wedge is diagnosable from the condition.
			RejectedReason: "CinderProjectionRejected",
			RejectedMessage: func(err error) string {
				return fmt.Sprintf("Cinder API server rejected the projected spec; reconcile the ControlPlane spec "+
					"to a valid projection to recover: %v", err)
			},
			ErrorReason:     "CinderError",
			ErrorMessage:    func(err error) string { return fmt.Sprintf("create-or-update Cinder: %v", err) },
			WaitRequeue:     infraRequeueAfter,
			Conditions:      &cp.Status.Conditions,
			Generation:      cp.Generation,
			ChildConditions: func(c *cinderv1alpha1.Cinder) []metav1.Condition { return c.Status.Conditions },
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
	// cinder-operator has re-rendered them on a pass of its own, so a reap that fires
	// before then wedges every pod created in that window on FailedMount, and a
	// cinder-operator that is down or backing off turns the window into a permanent
	// one. The reap therefore waits for the child's own verdict on the pointer-free
	// spec: the server's view of the child names no CA bundle any more, and it has
	// reconciled the generation the apply produced. The readiness return above is the
	// other half of that verdict: a child whose pipeline short-circuited before the
	// Deployment step stamps observedGeneration anyway, but reports Ready=False while
	// it does. The status write that carries the verdict wakes this ControlPlane
	// again, so the reap is one watch away.
	//
	// It runs here and NOT on the messaging leg, which runs ahead of every gate that
	// can halt this pass with the pointer still live. See pruneServiceMessagingCA.
	if messagingCAMirrorReleasable(cp, cn.Spec.Messaging, cn.Status.ObservedGeneration, cn.Generation) {
		if pruneRes, halt, perr := r.pruneServiceMessagingCA(ctx, cp, cinderMessagingTarget(cp)); halt {
			return pruneRes, perr
		}
	}

	// The Cinder child is ready. CinderReady still folds in the registration: a
	// running Cinder whose catalog entry never landed is reachable by nothing that
	// discovers it through the catalog, and the ControlPlane must not report the
	// block-storage service as ready for it.
	if readyRes, pending := foldBuiltinRegistrationReady(cp, child, conditionTypeCinderReady); pending {
		return readyRes, nil
	}
	return res, nil
}

// deleteOrphanedCinder removes a previously-projected Cinder child, its
// CinderBackend and CinderBackupBackend satellites, the DB-credential
// ExternalSecret, the two messaging Secrets, and the KeystoneService registration
// that follow it, when spec.services.cinder is unset AND the ControlPlane has
// opted in to deletion via cinderDeletionAllowedAnnotation (the caller gates
// this). Each object is only deleted when this ControlPlane still owns it (by
// owner reference in its own namespace, by the ownership labels in a service
// namespace); a foreign object colliding on a name is left alone.
//
// A pruned CinderBackend keeps the cinder operator's service-remove finalizer
// until its detach Job has run, so the satellites outlive this call while cinder
// takes their backends out of service.
//
// Deleting the registration is what removes Cinder from the Keystone catalog and
// from the identity plane: the KeystoneService controller's finalizer tears down
// the catalog rows, the service user and its project behind it.
func (r *ControlPlaneReconciler) deleteOrphanedCinder(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	cinderNS := cp.CinderNamespace()

	// The Dynamic-mode client Certificate has no Go type, so it is addressed
	// unstructured.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(cinderDBCredentialClientCertName(cp))
	cert.SetNamespace(cinderNS)

	children := []client.Object{
		// The Cinder child.
		&cinderv1alpha1.Cinder{
			ObjectMeta: metav1.ObjectMeta{Name: cinderName(cp), Namespace: cinderNS},
		},
		// The DB-credential ExternalSecret.
		&esov1.ExternalSecret{
			ObjectMeta: metav1.ObjectMeta{Name: cinderDBCredentialSecretName(cp), Namespace: cinderNS},
		},
		// The Dynamic-mode DB-credential objects: the VaultDynamicSecret generator,
		// its mTLS client Certificate, and the ServiceAccount whose token it
		// authenticates with.
		&esgenv1alpha1.VaultDynamicSecret{
			ObjectMeta: metav1.ObjectMeta{Name: cinderDBCredentialSecretName(cp), Namespace: cinderNS},
		},
		cert,
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: cinderDBCredentialServiceAccountName, Namespace: cinderNS},
		},
	}
	// The bus delivery: the brownfield transport-URL Secret and the CA mirror beside
	// it. Nothing else writes them, so an unmanaged service leaves no broker
	// credential behind in the namespace.
	children = append(children, serviceMessagingSecrets(cinderMessagingTarget(cp))...)
	for _, child := range children {
		if err := commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, child, func(live client.Object) bool {
			return isControlPlaneChild(live, cp)
		}); err != nil {
			return err
		}
	}

	// Every projected satellite of both kinds: owned by this ControlPlane, which is
	// what keeps a hand-created CinderBackend attached to the same Cinder out of the
	// sweep. The prunes name only themselves, so the phase is wrapped on: a failure
	// here tears down every backend because services.cinder was unset, which reads
	// very differently from the same wrapper raised by reconcileCinderBackends
	// dropping a single removed entry.
	for _, satellites := range []projectedChildren{
		cinderBackendChildren(cinderNS, nil),
		cinderBackupBackendChildren(cinderNS, nil),
	} {
		if err := r.pruneProjectedChildren(ctx, cp, satellites); err != nil {
			return fmt.Errorf("orphan cleanup: %w", err)
		}
	}

	// The KeystoneService registration. It lives beside the service, on the
	// management cluster whatever cluster Cinder runs on. The credential mirror a
	// PLACED service carries is not swept here: like every object this function
	// names it is resolved through CinderNamespace(), which without a services.cinder
	// block is the ControlPlane's own namespace, so this sweep reaches co-located
	// objects only. The mirror is reaped by the ControlPlane teardown, which sweeps a
	// placed namespace's label-owned ExternalSecrets on the target cluster.
	registration := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: cinderName(cp), Namespace: cinderNS},
	}
	return commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, registration, func(live client.Object) bool {
		return isControlPlaneChild(live, cp)
	})
}
