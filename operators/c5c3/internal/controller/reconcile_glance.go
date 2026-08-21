// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

// The projected Glance CR is named "{controlplane.Name}-glance" — the same
// deterministic, collision-free naming convention as the Keystone and Horizon
// children (see keystoneNameSuffix) — and lives in cp.GlanceNamespace(): the
// ControlPlane's own namespace by default, or the one services.glance.namespace
// assigns.
const glanceNameSuffix = "-glance"

// defaultGlanceRepository is the canonical Glance image repository; the tag is
// derived from spec.openStackRelease unless spec.services.glance.image overrides
// the whole image reference.
const defaultGlanceRepository = "ghcr.io/c5c3/glance"

// glanceDeletionAllowedAnnotation, when set to a truthy value on a ControlPlane,
// opts that ControlPlane in to tearing down a previously-projected Glance child
// (and its GlanceBackend children and DB-credential ExternalSecret) when
// spec.services.glance is unset. The preserve-by-default posture mirrors the
// Keystone/Horizon annotations for a consistent operator UX: an accidental block
// drop never silently removes a running service.
const glanceDeletionAllowedAnnotation = "c5c3.io/allow-glance-deletion"

// defaultGlanceDatabaseName is the logical database name the Glance schema always
// lives in, regardless of whether Glance shares the ControlPlane's database
// cluster or takes a dedicated one — its own logical schema keeps it isolated
// from Keystone's on a shared cluster.
const defaultGlanceDatabaseName = "glance"

// glanceName returns the deterministic name of the Glance CR projected from the
// given ControlPlane (see glanceNameSuffix).
func glanceName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + glanceNameSuffix
}

// glanceBackendNamePrefix is the name prefix every projected GlanceBackend
// carries; the prune/orphan/teardown sweeps match on it, so it must stay in
// lockstep with glanceBackendName.
func glanceBackendNamePrefix(cp *c5c3v1alpha1.ControlPlane) string {
	return glanceName(cp) + "-"
}

// glanceDeletionAllowed reports whether cp opts in to deleting its projected
// Glance child when spec.services.glance is unset, via a truthy
// glanceDeletionAllowedAnnotation. A missing, malformed, or non-truthy value
// means "preserve".
func glanceDeletionAllowed(cp *c5c3v1alpha1.ControlPlane) bool {
	allowed, err := strconv.ParseBool(cp.Annotations[glanceDeletionAllowedAnnotation])
	return err == nil && allowed
}

// glanceKeystoneEndpoint returns the Keystone endpoint URL projected into the
// Glance child's spec.keystoneEndpoint. Glance validates every token
// server-side against this URL, so what the Glance pods can reach decides it —
// which is the cluster Glance is placed on against the one Keystone runs on,
// the rule keystoneEndpointFor holds for every service.
func glanceKeystoneEndpoint(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneEndpointFor(cp, cp.GlanceTargetClusterRef())
}

// glanceEndpointURL renders the in-cluster URL of the projected Glance API
// Service by naming convention (glance-api listens on 9292), the cross-service
// endpoint contract a later catalog package registers against.
func glanceEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return managedServiceURL(glanceName(cp), cp.GlanceNamespace(), 9292, "")
}

// glanceBackendName returns the deterministic name of the GlanceBackend child
// projected for a backends[] entry: the Glance child name, a hyphen, and the
// entry name — so the prune sweep recognises projected backends by the
// glanceName(cp)+"-" prefix and never touches a hand-created one.
func glanceBackendName(cp *c5c3v1alpha1.ControlPlane, entryName string) string {
	return glanceBackendNamePrefix(cp) + entryName
}

// reconcileGlance projects spec.services.glance into an owned Glance CR (and its
// GlanceBackend children) and drives the GlanceReady condition.
//
// The sub-reconciler is GATED on KeystoneReady (Glance validates every token
// against the ControlPlane's Keystone child) and on the KeystoneService child it
// projects for Glance (Glance authenticates as the Keystone user that
// registration provisions). Once gated through, it ensures the DB-credential
// ExternalSecret, projects the Glance CR — database/cache DeepCopied from the
// resolved backing services, the Keystone endpoint derived top-down
// (glanceKeystoneEndpoint) — and folds both children's readiness into
// GlanceReady.
func (r *ControlPlaneReconciler) reconcileGlance(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// spec.services.glance is optional. When unset, this ControlPlane manages no
	// image service and reports GlanceReady as not-managed so the aggregate Ready
	// condition is not blocked (staged adoption). A previously-projected child is
	// preserved unless the ControlPlane opts in to deletion.
	if cp.Spec.Services.Glance == nil {
		message := "spec.services.glance is unset; no Glance image service is managed by this ControlPlane"
		if glanceDeletionAllowed(cp) {
			if err := r.deleteOrphanedGlance(ctx, cp); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			// Preserve the child — but NEVER the credential minter. A live
			// VaultDynamicSecret keeps issuing a fresh MySQL user with ALL PRIVILEGES
			// on the glance schema at every refresh interval, indefinitely, for a
			// service this ControlPlane has been told it no longer manages: no
			// consumer, no revocation, and a GlanceReady=True/GlanceNotManaged
			// condition that surfaces none of it. Preserving a running service does
			// not imply preserving the generator behind its credentials, so the
			// dynamic objects come down either way.
			//
			// GlanceNamespace() still resolves correctly here: removing a
			// services.glance.namespace assignment is rejected by
			// validateServiceNamespacesImmutable, so the only admissible way to reach
			// this branch with a live generator is the co-located one, where the
			// generator sits in the ControlPlane's own namespace.
			r.deleteDynamicDBCredentialObjects(ctx, cp, glanceDBCredentialTarget(cp))
			message += fmt.Sprintf("; any previously-projected Glance child is preserved "+
				"(set annotation %s=true to allow deletion), but its dynamic DB-credential generator is torn down",
				glanceDeletionAllowedAnnotation)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeGlanceReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "GlanceNotManaged",
			Message:            message,
		})
		return ctrl.Result{}, nil
	}

	// Resolve the backing services Glance actually talks to: its own dedicated
	// instances when it opted into them, the ControlPlane-wide shared ones
	// otherwise.
	//
	// Nil-safety fail-safe. The projection DeepCopies these, so an unresolvable
	// instance has nothing to project and the deref below would panic. The
	// validating webhook requires spec.infrastructure outside External mode (and
	// forbids services.glance in External mode), so this only fires for a
	// webhook-bypassed CR.
	database := effectiveGlanceDatabase(cp)
	cache := effectiveGlanceCache(cp)
	if database == nil || cache == nil {
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// Gate on KeystoneReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeKeystoneReady) {
		logger.Info("Keystone not ready, deferring Glance projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeGlanceReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForKeystone",
			Message:            "KeystoneReady is not True; Glance projection deferred",
		})
		return ctrl.Result{RequeueAfter: keystoneInfraGateRequeueAfter}, nil
	}

	// Register Glance against the identity plane: one KeystoneService child
	// carrying the image catalog entry and the service account Glance
	// authenticates as, mirrored onto a placed Glance's cluster and gated on the
	// account it provisions.
	child, regRes, halt, err := r.reconcileBuiltinRegistration(ctx, cp, desiredGlanceRegistration(cp),
		"Glance", conditionTypeGlanceReady)
	if halt {
		return regRes, err
	}

	// The EFFECTIVE credentials mode of the database Glance connects to, resolved
	// once so the credential projection below, its readiness gate, and the mode
	// stamped onto the child further down can never disagree.
	dynamic := database.ClusterRef != nil && glanceDBCredentialsDynamicEnabled(cp)

	// Ensure the DB-credential objects BEFORE the child so the Secret it references
	// exists when the glance-operator resolves it. Managed only: a brownfield
	// database (ClusterRef nil) carries a user-supplied credential out-of-band, so
	// there is nothing for the operator to project. In Dynamic mode the shared
	// helper also holds the projection until an engine-issued credential has landed
	// (see ensureServiceDBCredential).
	if database.ClusterRef != nil {
		res, halt, err := r.ensureServiceDBCredential(ctx, cp, glanceDBCredentialTarget(cp),
			dynamic, "Glance", conditionTypeGlanceReady)
		if halt {
			return res, err
		}
	}

	// Resolve the Glance image. spec.services.glance.image overrides the
	// release-derived default when set.
	image := commonv1.ImageSpec{
		Repository: defaultGlanceRepository,
		Tag:        cp.Spec.OpenStackRelease,
	}
	if override := cp.Spec.Services.Glance.Image; override != nil {
		image = *override
	}

	// Place the child in the namespace assigned to the Glance service (the
	// ControlPlane's own unless services.glance.namespace says otherwise). A child
	// outside the ControlPlane's namespace can carry no owner reference, so it is
	// stamped with the ownership labels and applied unowned.
	glanceNS := cp.GlanceNamespace()
	crossNamespace := glanceNS != cp.Namespace
	glance := &glancev1alpha1.Glance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      glanceName(cp),
			Namespace: glanceNS,
		},
	}
	if crossNamespace {
		stampControlPlaneChildLabels(glance, cp)
	}

	glance.Spec.OpenStackRelease = cp.Spec.OpenStackRelease
	glance.Spec.Image = image

	// Thread the service's target cluster onto the child verbatim; a nil source
	// yields nil and leaves the child unplaced (see the Keystone projection).
	glance.Spec.TargetClusterRef = cp.Spec.Services.Glance.TargetClusterRef.DeepCopy()

	// Project the merged extraConfig (globalExtraConfig unioned with the
	// per-service block, per-service winning key by key). Assigned
	// unconditionally, following the revert-on-clear convention: a nil merge keeps
	// the SSA-applied intent free of spec.extraConfig, so a direct edit on the
	// child stays unowned until a ControlPlane block is set — and clearing the
	// ControlPlane block reverts the child rather than pinning the last value.
	glance.Spec.ExtraConfig = c5c3v1alpha1.MergedExtraConfig(cp.Spec.GlobalExtraConfig, cp.Spec.Services.Glance.ExtraConfig)

	// Point Glance at the SAME backing services the ControlPlane provisioned. The
	// logical database is always "glance" — its own schema keeps it isolated from
	// Keystone's on a shared cluster. DeepCopy (over a plain struct copy) is
	// required because DatabaseSpec/CacheSpec carry pointer fields — a shallow copy
	// would alias cp.Spec.
	glance.Spec.Database = *database.DeepCopy()
	glance.Spec.Database.Database = defaultGlanceDatabaseName
	// In managed mode the operator OWNS the glance DB credential —
	// reconcileGlance materialises it (above) into a per-ControlPlane Secret named
	// glanceDBCredentialSecretName(cp). Override the projected Glance CR's
	// database.secretRef to that operator-owned Secret (key "password"), and
	// project the EFFECTIVE credentials mode: Dynamic (engine-issued) is the
	// default on the managed shared database, a per-service override or the shared
	// Static opt-out flips it to Static, and a dedicated glance database stays
	// Static (no engine role can mint its credentials). Brownfield (ClusterRef nil)
	// leaves the user-supplied secretRef and credentialsMode in place.
	if database.ClusterRef != nil {
		glance.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: glanceDBCredentialSecretName(cp), Key: "password"}
		if dynamic {
			glance.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
		} else {
			glance.Spec.Database.CredentialsMode = commonv1.CredentialsModeStatic
		}
	}

	glance.Spec.Cache = *cache.DeepCopy()

	// The Keystone endpoint is derived top-down from the ControlPlane rather than
	// read from the Keystone child's status — no machine consumer reads status
	// endpoints per the settled convention. keystonePublicEndpoint is the
	// browser/client-facing URL Glance advertises on a 401 (empty when Keystone is
	// not externally exposed, in which case the child falls back to the internal
	// endpoint).
	glance.Spec.KeystoneEndpoint = glanceKeystoneEndpoint(cp)
	glance.Spec.KeystonePublicEndpoint = keystonePublicEndpoint(cp.Spec.Services.Keystone)

	glance.Spec.Region = cp.Spec.Region

	// The Keystone service user Glance authenticates as, the account the
	// registration child provisions: user and project as declared on that child,
	// both domains the ControlPlane's effective admin domain (which the
	// registration resolves the same way, its own domainName being unset), and the
	// password read from the consumer Secret the registration delivers.
	glance.Spec.ServiceUser = glancev1alpha1.ServiceUserSpec{
		Username:          c5c3v1alpha1.GlanceServiceAccountName,
		ProjectName:       c5c3v1alpha1.GlanceServiceProjectName,
		UserDomainName:    adminDomainName(cp),
		ProjectDomainName: adminDomainName(cp),
		SecretRef: commonv1.SecretRefSpec{
			Name: keystoneServiceCredentialsSecretName(child),
			Key:  "password",
		},
	}

	// Project the ControlPlane's RESOLVED store selection onto the Glance child so
	// it never falls back to its own shared-cluster-store default.
	glance.Spec.SecretStoreRef = effectiveControlPlaneStoreRefPtr(cp)

	// DeepCopy for the same aliasing reason as Database above; a nil source yields
	// nil, clearing any previously-projected gateway so removal tears the HTTPRoute
	// down.
	glance.Spec.Gateway = cp.Spec.Services.Glance.Gateway.DeepCopy()

	// The web-download URI filter is platform security policy, so the ControlPlane
	// projects it (unlike spec.apiServer below, whose child-side defaults stay
	// authoritative). DeepCopied for the same aliasing reason as Gateway above and
	// assigned unconditionally, following the replicas convention: clearing
	// services.glance.importFiltering removes the field from the child, so the
	// Glance operator's restrictive defaults apply again instead of the last
	// projected value staying pinned on the fetched child.
	glance.Spec.ImportFiltering = cp.Spec.Services.Glance.ImportFiltering.DeepCopy()

	// The node-local scratch bound is projected the same way and for the same
	// reasons: clearing services.glance.staging removes the field from the child,
	// so the Glance operator's default size limit applies again.
	glance.Spec.Staging = cp.Spec.Services.Glance.Staging.DeepCopy()

	// The per-replica image cache is projected the same way: clearing
	// services.glance.imageCache removes the field from the child, so the cache is
	// switched off again on the next rollout instead of staying enabled with the
	// last projected budget.
	glance.Spec.ImageCache = cp.Spec.Services.Glance.ImageCache.DeepCopy()

	// The import-plugin selection is projected the same way: clearing
	// services.glance.importPlugins removes the field from the child, so the Glance
	// operator's defaults apply again and the next rollout runs no plugin at all
	// instead of keeping the last projected selection pinned.
	glance.Spec.ImportPlugins = cp.Spec.Services.Glance.ImportPlugins.DeepCopy()

	// Resolve replicas to the shared operator default, then let an override win.
	// Assigning unconditionally means clearing services.glance.replicas reverts the
	// child to the default instead of leaving the previously-projected value pinned
	// on the fetched child.
	glance.Spec.Deployment.Replicas = commonv1.DefaultReplicas
	if cp.Spec.Services.Glance.Replicas != nil {
		glance.Spec.Deployment.Replicas = *cp.Spec.Services.Glance.Replicas
	}

	// spec.apiServer is deliberately NOT set — the child-side release-conditional
	// defaults (workers vs uwsgi) stay authoritative.

	// Project the declared image stores as GlanceBackend children and prune any
	// previously-projected backend whose entry was removed. A GlanceBackend
	// references its Glance by name (inverted attachment), so the ordering relative
	// to the child ensure below is immaterial — GitOps applies them in either order.
	if err := r.reconcileGlanceBackends(ctx, cp, glanceNS); err != nil {
		reason := "GlanceBackendError"
		message := fmt.Sprintf("projecting GlanceBackend children: %v", err)
		if apierrors.IsInvalid(err) {
			reason = "GlanceBackendProjectionRejected"
			message = fmt.Sprintf("Glance API server rejected a projected GlanceBackend; reconcile the "+
				"services.glance.backends entries to a valid projection to recover: %v", err)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeGlanceReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             reason,
			Message:            message,
		})
		return ctrl.Result{}, err
	}

	res, err := commonreconcile.ProjectChild(ctx, r.Client, r.Scheme, cp, commonreconcile.ChildProjectionParams[*glancev1alpha1.Glance]{
		Child:          glance,
		ConditionType:  conditionTypeGlanceReady,
		ReadyReason:    "GlanceReady",
		ReadyMessage:   "Projected Glance CR is ready",
		WaitingReason:  "WaitingForGlance",
		WaitingMessage: fmt.Sprintf("Glance %q is not ready", glance.Name),
		// An Invalid (HTTP 422) rejection from the Glance API server means the
		// projected spec violates a CRD/webhook rule — surface a distinct,
		// actionable reason so the wedge is diagnosable from the condition.
		RejectedReason: "GlanceProjectionRejected",
		RejectedMessage: func(err error) string {
			return fmt.Sprintf("Glance API server rejected the projected spec; reconcile the ControlPlane spec to a "+
				"valid projection to recover: %v", err)
		},
		ErrorReason:     "GlanceError",
		ErrorMessage:    func(err error) string { return fmt.Sprintf("create-or-update Glance: %v", err) },
		WaitRequeue:     infraRequeueAfter,
		Conditions:      &cp.Status.Conditions,
		Generation:      cp.Generation,
		ChildConditions: func(g *glancev1alpha1.Glance) []metav1.Condition { return g.Status.Conditions },
		Unowned:         crossNamespace,
	})
	if err != nil || !res.IsZero() {
		return res, err
	}

	// The Glance child is ready. GlanceReady still folds in the registration: a
	// running Glance whose catalog entry never landed is reachable by nothing that
	// discovers it through the catalog, and the ControlPlane must not report the
	// image service as ready for it.
	if readyRes, pending := foldBuiltinRegistrationReady(cp, child, conditionTypeGlanceReady); pending {
		return readyRes, nil
	}
	return res, nil
}

// deleteOrphanedGlance removes a previously-projected Glance child — and the
// GlanceBackend children, DB-credential ExternalSecret, and KeystoneService
// registration that follow it — when spec.services.glance is unset AND the
// ControlPlane has opted in to deletion via glanceDeletionAllowedAnnotation (the
// caller gates this). Each object is only
// deleted when this ControlPlane still owns it (by owner reference in its own
// namespace, by the ownership labels in a service namespace); a hand-created
// GlanceBackend that merely shares the namespace, or a foreign object colliding
// on a name, is left alone.
//
// Deleting the registration is what removes Glance from the Keystone catalog and
// from the identity plane: the KeystoneService controller's finalizer tears down
// the catalog rows, the service user and its project behind it.
func (r *ControlPlaneReconciler) deleteOrphanedGlance(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	glanceNS := cp.GlanceNamespace()

	// The Glance child.
	child := &glancev1alpha1.Glance{
		ObjectMeta: metav1.ObjectMeta{Name: glanceName(cp), Namespace: glanceNS},
	}
	if err := commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, child, func(live client.Object) bool {
		return isControlPlaneChild(live, cp)
	}); err != nil {
		return err
	}

	// Every projected GlanceBackend: owned by this ControlPlane AND carrying the
	// glance child's name prefix, so a hand-created GlanceBackend attached to the
	// child is never touched.
	var backends glancev1alpha1.GlanceBackendList
	if err := r.List(ctx, &backends, client.InNamespace(glanceNS)); err != nil {
		return fmt.Errorf("listing GlanceBackends for orphan cleanup: %w", err)
	}
	prefix := glanceBackendNamePrefix(cp)
	for i := range backends.Items {
		b := &backends.Items[i]
		if !isControlPlaneChild(b, cp) || !strings.HasPrefix(b.Name, prefix) {
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, b, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
			return fmt.Errorf("deleting orphaned GlanceBackend %s/%s: %w", b.Namespace, b.Name, err)
		}
	}

	// The DB-credential ExternalSecret.
	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: glanceDBCredentialSecretName(cp), Namespace: glanceNS},
	}
	if err := commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, es, func(live client.Object) bool {
		return isControlPlaneChild(live, cp)
	}); err != nil {
		return err
	}

	// The Dynamic-mode DB-credential objects: the VaultDynamicSecret generator, its
	// mTLS client Certificate, and the ServiceAccount whose token it authenticates
	// with. Each is ownership-checked, so a foreign object colliding on a name is
	// left alone.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(glanceDBCredentialClientCertName(cp))
	cert.SetNamespace(glanceNS)
	dynamicChildren := []client.Object{
		&esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{Name: glanceDBCredentialSecretName(cp), Namespace: glanceNS}},
		cert,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: glanceDBCredentialServiceAccountName, Namespace: glanceNS}},
	}
	for _, child := range dynamicChildren {
		if err := commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, child, func(live client.Object) bool {
			return isControlPlaneChild(live, cp)
		}); err != nil {
			return err
		}
	}

	// The KeystoneService registration. It lives beside the service, on the
	// management cluster whatever cluster Glance runs on. The credential mirror a
	// PLACED service carries is not swept here: like every object this function
	// names it is resolved through GlanceNamespace(), which without a
	// services.glance block is the ControlPlane's own namespace — so this sweep
	// reaches co-located objects only. The mirror is reaped by the
	// ControlPlane teardown, which sweeps a placed namespace's label-owned
	// ExternalSecrets on the target cluster.
	registration := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: glanceName(cp), Namespace: glanceNS},
	}
	return commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, registration, func(live client.Object) bool {
		return isControlPlaneChild(live, cp)
	})
}

// glanceBackendForEntry builds the GlanceBackend child projected from one
// services.glance.backends entry. The CP-side S3 endpoint maps to the child's
// spec.s3.host, and an unset bucketURLFormat serializes away (omitempty) so the
// GlanceBackend CRD's own default ("path") applies at exactly one layer.
func glanceBackendForEntry(cp *c5c3v1alpha1.ControlPlane, entry c5c3v1alpha1.GlanceBackendEntry, glanceNS string) *glancev1alpha1.GlanceBackend {
	s3 := &glancev1alpha1.S3BackendSpec{
		Host:                 entry.S3.Endpoint,
		Bucket:               entry.S3.Bucket,
		Region:               entry.S3.Region,
		BucketURLFormat:      entry.S3.BucketURLFormat,
		CredentialsSecretRef: glancev1alpha1.SecretNameRefSpec{Name: entry.S3.CredentialsSecretRef.Name},
	}
	return &glancev1alpha1.GlanceBackend{
		ObjectMeta: metav1.ObjectMeta{
			Name:      glanceBackendName(cp, entry.Name),
			Namespace: glanceNS,
		},
		Spec: glancev1alpha1.GlanceBackendSpec{
			GlanceRef: glancev1alpha1.GlanceRefSpec{Name: glanceName(cp)},
			Type:      glancev1alpha1.GlanceBackendTypeS3,
			S3:        s3,
			IsDefault: entry.IsDefault,
		},
	}
}

// reconcileGlanceBackends projects one GlanceBackend child per
// services.glance.backends entry and prunes previously-projected children whose
// entry was removed.
//
// Every write routes through ensureUnownedOrOwned, which owner-references the
// child in the ControlPlane's own namespace and label-stamps it (unowned) in a
// service namespace — and, in a namespace the ControlPlane does not own, REFUSES
// to adopt a same-named object it did not create. The prune sweep deletes only
// c5c3-owned children carrying the glance child's name prefix, so a hand-created
// GlanceBackend attached to the same Glance — or any foreign object colliding on
// a name — is never pruned or overwritten.
func (r *ControlPlaneReconciler) reconcileGlanceBackends(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, glanceNS string) error {
	declared := make(map[string]struct{}, len(cp.Spec.Services.Glance.Backends))
	for i := range cp.Spec.Services.Glance.Backends {
		backend := glanceBackendForEntry(cp, cp.Spec.Services.Glance.Backends[i], glanceNS)
		if err := r.ensureUnownedOrOwned(ctx, r.Client, cp, backend); err != nil {
			return fmt.Errorf("projecting GlanceBackend %q: %w", backend.Name, err)
		}
		declared[backend.Name] = struct{}{}
	}

	var list glancev1alpha1.GlanceBackendList
	if err := r.List(ctx, &list, client.InNamespace(glanceNS)); err != nil {
		return fmt.Errorf("listing GlanceBackends for prune: %w", err)
	}
	prefix := glanceBackendNamePrefix(cp)
	for i := range list.Items {
		b := &list.Items[i]
		if _, kept := declared[b.Name]; kept {
			continue
		}
		if !isControlPlaneChild(b, cp) || !strings.HasPrefix(b.Name, prefix) {
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, b, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
			return fmt.Errorf("pruning undeclared GlanceBackend %q: %w", b.Name, err)
		}
	}
	return nil
}
