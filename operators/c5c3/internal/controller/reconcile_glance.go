// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	"github.com/c5c3/forge/internal/common/secrets"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
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

// glanceServiceAccountName mirrors c5c3v1alpha1.GlanceServiceAccountName, the
// single source of truth for the spec.korc.serviceAccounts entry the Glance
// projection consumes for its Keystone service user. The defaulting webhook
// auto-injects it (name glance, project service (create), roles [service])
// whenever services.glance is set.
const glanceServiceAccountName = c5c3v1alpha1.GlanceServiceAccountName

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
// Glance child's spec.keystoneEndpoint. It is ALWAYS the cluster-local
// convention URL of the projected Keystone child (keystoneEndpointURL, the same
// URL K-ORC authenticates against) — never the external publicEndpoint or
// gateway hostname. Glance validates every token server-side against this URL, so
// it must be reachable from the Glance pods: an externally routable URL that
// resolves to a host-only address (a kind port-mapping, an external LB) is
// unreachable from inside the cluster. Derived top-down from the naming
// convention, never read from the child's status.
func glanceKeystoneEndpoint(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneEndpointURL(cp)
}

// glanceEndpointURL renders the in-cluster URL of the projected Glance API
// Service by naming convention (glance-api listens on 9292), the cross-service
// endpoint contract a later catalog package registers against.
func glanceEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return managedServiceURL(glanceName(cp), cp.GlanceNamespace(), 9292, "")
}

// glanceDBCredentialSecretName returns the deterministic name of the
// per-ControlPlane Glance DB-credential Secret/ExternalSecret. It tracks the
// projected Glance CR, mirroring dbCredentialSecretName's derivation from the
// Keystone child.
func glanceDBCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return glanceName(cp) + dbCredentialSecretNameSuffix
}

// glanceDBCredentialRemoteKeyFor returns the per-ControlPlane, namespace-scoped
// OpenBao KV path the static Glance DB credential is read from (keys username,
// password). The eso-tenant.hcl policy already grants this path shape; the seed
// lands in a later package (nothing seeds it yet), so the ExternalSecret only
// syncs once the path has been seeded out-of-band.
func glanceDBCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "openstack/glance/" + cp.GlanceNamespace() + "/" + cp.Name + "/db"
}

// glanceBackendName returns the deterministic name of the GlanceBackend child
// projected for a backends[] entry: the Glance child name, a hyphen, and the
// entry name — so the prune sweep recognises projected backends by the
// glanceName(cp)+"-" prefix and never touches a hand-created one.
func glanceBackendName(cp *c5c3v1alpha1.ControlPlane, entryName string) string {
	return glanceBackendNamePrefix(cp) + entryName
}

// glanceServiceAccount returns the auto-injected "glance" entry from
// spec.korc.serviceAccounts and whether it is declared. The Glance projection
// derives its Keystone service user from this entry.
func glanceServiceAccount(cp *c5c3v1alpha1.ControlPlane) (c5c3v1alpha1.ServiceAccountSpec, bool) {
	for _, sa := range cp.Spec.KORC.ServiceAccounts {
		if sa.Name == glanceServiceAccountName {
			return sa, true
		}
	}
	return c5c3v1alpha1.ServiceAccountSpec{}, false
}

// glanceServiceAccountReady reports whether the per-account status entry for the
// glance service account is Ready — the readiness reconcileServiceAccounts
// computes for it in the same reconcile pass, ahead of this sub-reconciler.
func glanceServiceAccountReady(cp *c5c3v1alpha1.ControlPlane) bool {
	for _, s := range cp.Status.ServiceAccounts {
		if s.Name == glanceServiceAccountName {
			return s.Ready
		}
	}
	return false
}

// glanceDBCredentialStaticExternalSecret builds the static, KV-backed
// ExternalSecret that materialises the Glance DB user's username+password into
// the Glance service namespace via the store the ControlPlane selected. It
// mirrors dbCredentialStaticExternalSecret: there is no OpenBao database-engine
// role for the glance schema (see the DECISION on the projected Database below),
// so the credential is always read from a KV path, never engine-issued.
func glanceDBCredentialStaticExternalSecret(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	name := glanceDBCredentialSecretName(cp)
	remoteKey := glanceDBCredentialRemoteKeyFor(cp)
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cp.GlanceNamespace()},
		Spec: esov1.ExternalSecretSpec{
			RefreshInterval: &metav1.Duration{Duration: time.Hour},
			SecretStoreRef:  secrets.ESOSecretStoreRef(effectiveControlPlaneStoreRef(cp)),
			Target:          esov1.ExternalSecretTarget{Name: name, CreationPolicy: esov1.CreatePolicyOwner},
			Data: []esov1.ExternalSecretData{
				{SecretKey: "username", RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: "username"}},
				{SecretKey: "password", RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: "password"}},
			},
		},
	}
}

// reconcileGlance projects spec.services.glance into an owned Glance CR (and its
// GlanceBackend children) and drives the GlanceReady condition.
//
// The sub-reconciler is GATED on KeystoneReady (Glance validates every token
// against the ControlPlane's Keystone child) and on the glance service account
// (Glance authenticates as that Keystone user). Once gated through, it ensures
// the DB-credential ExternalSecret, projects the Glance CR — database/cache
// DeepCopied from the resolved backing services, the Keystone endpoint derived
// top-down (glanceKeystoneEndpoint) — and mirrors the child's Ready condition
// back into GlanceReady.
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
			message += fmt.Sprintf("; any previously-projected Glance child is preserved "+
				"(set annotation %s=true to allow deletion)", glanceDeletionAllowedAnnotation)
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

	// Gate on the glance service account. Its declaration is auto-injected by the
	// defaulting webhook whenever services.glance is set; a missing entry means
	// admission was bypassed, so fail loud with a named field rather than
	// projecting a Glance with no Keystone user. A declared-but-not-yet-Ready
	// account defers projection until its user and password are provisioned.
	sa, declared := glanceServiceAccount(cp)
	if !declared {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeGlanceReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ServiceAccountNotDeclared",
			Message: fmt.Sprintf("no spec.korc.serviceAccounts entry named %q is declared; the defaulting webhook "+
				"injects it whenever services.glance is set, so a ControlPlane reaching here bypassed admission",
				glanceServiceAccountName),
		})
		return ctrl.Result{}, nil
	}
	if !glanceServiceAccountReady(cp) {
		logger.Info("glance service account not ready, deferring Glance projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeGlanceReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForServiceAccount",
			Message: fmt.Sprintf("service account %q is not yet Ready; Glance projection deferred until its "+
				"Keystone user and password are provisioned", glanceServiceAccountName),
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Ensure the DB-credential ExternalSecret BEFORE the child so the Secret it
	// references exists when the glance-operator resolves it. Managed only: a
	// brownfield database (ClusterRef nil) carries a user-supplied credential
	// out-of-band, so there is nothing for the operator to project.
	if database.ClusterRef != nil {
		if err := r.ensureUnownedOrOwned(ctx, cp, glanceDBCredentialStaticExternalSecret(cp)); err != nil {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeGlanceReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "GlanceDBCredentialError",
				Message:            fmt.Sprintf("ensuring Glance DB credential ExternalSecret: %v", err),
			})
			return ctrl.Result{}, err
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

	// Point Glance at the SAME backing services the ControlPlane provisioned. The
	// logical database is always "glance" — its own schema keeps it isolated from
	// Keystone's on a shared cluster. DeepCopy (over a plain struct copy) is
	// required because DatabaseSpec/CacheSpec carry pointer fields — a shallow copy
	// would alias cp.Spec.
	glance.Spec.Database = *database.DeepCopy()
	glance.Spec.Database.Database = defaultGlanceDatabaseName
	// DECISION (phase-0 D10): there is NO OpenBao dynamic-engine role for the
	// glance schema — the database engine role is bootstrapped once per namespace
	// and is keystone-scoped, so no role can mint credentials for the glance user.
	// In managed mode the operator therefore OWNS the glance DB credential as a
	// STATIC KV-backed ExternalSecret (ensured above) and forces credentialsMode
	// Static; the KV path (glanceDBCredentialRemoteKeyFor) is seeded at the OpenBao
	// source out-of-band. Brownfield (ClusterRef nil) leaves the user-supplied
	// secretRef in place.
	if database.ClusterRef != nil {
		glance.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: glanceDBCredentialSecretName(cp), Key: "password"}
		glance.Spec.Database.CredentialsMode = commonv1.CredentialsModeStatic
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

	// The Keystone service user Glance authenticates as, derived from the
	// auto-injected glance service account: the user and project domains resolve to
	// the same effective domain the account is provisioned in, and the password is
	// read from the account's materialized consumer Secret.
	glance.Spec.ServiceUser = glancev1alpha1.ServiceUserSpec{
		Username:          serviceAccountUserName(sa),
		ProjectName:       sa.Project.Name,
		UserDomainName:    serviceAccountDomainName(cp, sa),
		ProjectDomainName: serviceAccountDomainName(cp, sa),
		SecretRef:         commonv1.SecretRefSpec{Name: serviceAccountCredentialsSecretName(cp, sa), Key: "password"},
	}

	// Project the ControlPlane's RESOLVED store selection onto the Glance child so
	// it never falls back to its own shared-cluster-store default.
	glance.Spec.SecretStoreRef = effectiveControlPlaneStoreRefPtr(cp)

	// DeepCopy for the same aliasing reason as Database above; a nil source yields
	// nil, clearing any previously-projected gateway so removal tears the HTTPRoute
	// down.
	glance.Spec.Gateway = cp.Spec.Services.Glance.Gateway.DeepCopy()

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

	return commonreconcile.ProjectChild(ctx, r.Client, r.Scheme, cp, commonreconcile.ChildProjectionParams[*glancev1alpha1.Glance]{
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
}

// deleteOrphanedGlance removes a previously-projected Glance child — and the
// GlanceBackend children, DB-credential ExternalSecret, and image catalog K-ORC
// CRs that follow it — when spec.services.glance is unset AND the ControlPlane has
// opted in to deletion via glanceDeletionAllowedAnnotation (the caller gates
// this). Each object is only
// deleted when this ControlPlane still owns it (by owner reference in its own
// namespace, by the ownership labels in a service namespace); a hand-created
// GlanceBackend that merely shares the namespace, or a foreign object colliding
// on a name, is left alone.
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

	// The Glance catalog K-ORC CRs: the image Service and its internal/public
	// Endpoints. These are ControlPlane-scoped and live in the ControlPlane's own
	// namespace regardless of Glance placement (see managedCatalogService), so they
	// are swept from childNamespace(cp), not glanceNS. Each is ownership-checked, so
	// a foreign CR colliding on a name is left alone.
	catalogNS := childNamespace(cp)
	catalogChildren := []client.Object{
		&orcv1alpha1.Service{ObjectMeta: metav1.ObjectMeta{Name: glanceCatalogServiceName(cp), Namespace: catalogNS}},
		&orcv1alpha1.Endpoint{ObjectMeta: metav1.ObjectMeta{Name: glanceCatalogEndpointName(cp, "internal"), Namespace: catalogNS}},
		&orcv1alpha1.Endpoint{ObjectMeta: metav1.ObjectMeta{Name: glanceCatalogEndpointName(cp, "public"), Namespace: catalogNS}},
	}
	for _, child := range catalogChildren {
		if err := commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, child, func(live client.Object) bool {
			return isControlPlaneChild(live, cp)
		}); err != nil {
			return err
		}
	}
	return nil
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
		if err := r.ensureUnownedOrOwned(ctx, cp, backend); err != nil {
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
