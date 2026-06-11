// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ControlPlane defaulting constants (CC-0110). These are the single source of
// truth shared by the defaulting webhook and (where relevant) the validation
// error messages, so the defaults cannot drift across call sites. The matching
// +kubebuilder:default markers on the spec fields remain as defense-in-depth
// for callers that bypass this webhook (e.g. envtest without the defaulter
// wired up) — kubebuilder markers require literals and cannot reference these
// Go constants.
const (
	// DefaultRegion is materialized when spec.region is empty (plan decision #4).
	DefaultRegion = "RegionOne"
	// DefaultCloudCredentialsSecretName is materialized when
	// spec.korc.adminCredential.cloudCredentialsRef.secretName is empty.
	DefaultCloudCredentialsSecretName = "k-orc-clouds-yaml" //nolint:gosec // G101 false positive: Secret name, not a credential
	// CC-0115: well-known defaults for the database, cache, and admin-credential
	// fields so a minimal managed-mode ControlPlane can omit spec.infrastructure
	// and the spec.korc.adminCredential body. The shared commonv1 leaves
	// (DatabaseSpec, CacheSpec, SecretRefSpec) are defaulted webhook-only — never
	// via a +kubebuilder:default marker — because the keystone operator reuses
	// those types and a c5c3-specific default would leak.
	//
	// DefaultDatabaseName is materialized when spec.infrastructure.database.database is empty.
	DefaultDatabaseName = "keystone"
	// DefaultDatabaseSecretName is materialized when spec.infrastructure.database.secretRef.name is empty.
	DefaultDatabaseSecretName = "keystone-db" //nolint:gosec // G101 false positive: Secret name, not a credential
	// DefaultDatabaseClusterRefName is the managed MariaDB CR name materialized when
	// spec.infrastructure.database is in managed mode (host unset).
	DefaultDatabaseClusterRefName = "openstack-db"
	// DefaultCacheBackend is materialized when spec.infrastructure.cache.backend is empty.
	DefaultCacheBackend = "dogpile.cache.pymemcache"
	// DefaultCacheClusterRefName is the managed Memcached CR name materialized when
	// spec.infrastructure.cache is in managed mode (servers unset).
	DefaultCacheClusterRefName = "openstack-memcached"
	// DefaultAdminPasswordSecretName is materialized when
	// spec.korc.adminCredential.passwordSecretRef.name is empty.
	DefaultAdminPasswordSecretName = "keystone-admin" //nolint:gosec // G101 false positive: Secret name, not a credential
	// DefaultAdminPasswordSecretKey is materialized when
	// spec.korc.adminCredential.passwordSecretRef.key is empty. Unlike the Secret
	// *name* constants above (which carry a //nolint:gosec G101 false-positive
	// annotation), "password" is the Secret data KEY — the field name within the
	// Secret (SecretRefSpec.Key), not credential material — so it correctly needs
	// no G101 nolint.
	DefaultAdminPasswordSecretKey = "password"
	// DefaultCloudName is materialized when
	// spec.korc.adminCredential.cloudCredentialsRef.cloudName is empty.
	DefaultCloudName = "admin"
)

// controlPlaneReleaseRegexp mirrors the +kubebuilder:validation:Pattern marker
// on ControlPlaneSpec.OpenStackRelease. The validating webhook re-checks it as
// defense-in-depth for callers that bypass CRD schema admission (CC-0110,
// REQ-006).
var controlPlaneReleaseRegexp = regexp.MustCompile(`^\d{4}\.\d$`)

// ControlPlaneWebhook implements defaulting and validation webhooks for the
// ControlPlane CRD (CC-0110). Client is injected at startup and used by
// ValidateCreate to enforce one ControlPlane per namespace (CC-0112, REQ-010).
// Production wiring injects mgr.GetAPIReader() — a direct, uncached reader —
// so concurrent or cache-sync-window CREATEs cannot both pass the check
// against an empty informer cache.
// +kubebuilder:object:generate=false
type ControlPlaneWebhook struct {
	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*ControlPlane] = &ControlPlaneWebhook{}
	_ admission.Validator[*ControlPlane] = &ControlPlaneWebhook{}
)

// +kubebuilder:webhook:path=/mutate-c5c3-io-v1alpha1-controlplane,mutating=true,failurePolicy=fail,sideEffects=None,groups=c5c3.io,resources=controlplanes,verbs=create;update,versions=v1alpha1,name=mcontrolplane.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-c5c3-io-v1alpha1-controlplane,mutating=false,failurePolicy=fail,sideEffects=None,groups=c5c3.io,resources=controlplanes,verbs=create;update,versions=v1alpha1,name=vcontrolplane.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *ControlPlaneWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*ControlPlane](mgr, &ControlPlane{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*ControlPlane] (CC-0110, REQ-005).
// It fills only zero-valued fields with their documented defaults, leaving any
// explicit value untouched. It is idempotent: applying it twice produces the
// same result.
func (w *ControlPlaneWebhook) Default(_ context.Context, obj *ControlPlane) error {
	// Plan decision #4: region defaults to RegionOne.
	if obj.Spec.Region == "" {
		obj.Spec.Region = DefaultRegion
	}

	// CC-0115: well-known infrastructure defaults so a minimal managed-mode CR can
	// omit spec.infrastructure entirely. The mode-neutral leaves (database name,
	// secretRef.name, cache backend) are defaulted in BOTH managed and brownfield
	// mode; the managed clusterRef is only invented when the brownfield
	// discriminator (database.host / cache.servers) is unset, so the validating
	// webhook's database/cache XOR check still passes for a brownfield CR — the
	// webhook never coerces an explicit brownfield endpoint into managed mode.
	db := &obj.Spec.Infrastructure.Database
	if db.Database == "" {
		db.Database = DefaultDatabaseName
	}
	if db.SecretRef.Name == "" {
		db.SecretRef.Name = DefaultDatabaseSecretName
	}
	if db.Host == "" {
		if db.ClusterRef == nil {
			db.ClusterRef = &corev1.LocalObjectReference{Name: DefaultDatabaseClusterRefName}
		} else if db.ClusterRef.Name == "" {
			db.ClusterRef.Name = DefaultDatabaseClusterRefName
		}
	}

	cache := &obj.Spec.Infrastructure.Cache
	if cache.Backend == "" {
		cache.Backend = DefaultCacheBackend
	}
	if len(cache.Servers) == 0 {
		if cache.ClusterRef == nil {
			cache.ClusterRef = &corev1.LocalObjectReference{Name: DefaultCacheClusterRefName}
		} else if cache.ClusterRef.Name == "" {
			cache.ClusterRef.Name = DefaultCacheClusterRefName
		}
	}

	// K-ORC admin-credential defaults. cloudCredentialsRef.secretName defaults to
	// the documented shared Secret name.
	korc := &obj.Spec.KORC.AdminCredential
	if korc.CloudCredentialsRef.SecretName == "" {
		korc.CloudCredentialsRef.SecretName = DefaultCloudCredentialsSecretName
	}
	// CC-0115: cloudCredentialsRef.cloudName defaults to the conventional admin
	// cloud entry; passwordSecretRef.name/.key default to the conventional admin
	// Secret and its data key. Defaulting .key makes the stored spec explicit and
	// consistent with the reconciler's existing readAdminPassword "password"
	// fallback. These are webhook-only (no marker on the shared commonv1 types).
	if korc.CloudCredentialsRef.CloudName == "" {
		korc.CloudCredentialsRef.CloudName = DefaultCloudName
	}
	if korc.PasswordSecretRef.Name == "" {
		korc.PasswordSecretRef.Name = DefaultAdminPasswordSecretName
	}
	if korc.PasswordSecretRef.Key == "" {
		korc.PasswordSecretRef.Key = DefaultAdminPasswordSecretKey
	}

	// applicationCredential.restricted defaults to true (least-privilege). The
	// pointer lets us distinguish "unset" (nil → default true) from an explicit
	// false, which we must preserve.
	appCred := &korc.ApplicationCredential
	if appCred.Restricted == nil {
		restricted := true
		appCred.Restricted = &restricted
	}

	// applicationCredential.rotation.mode defaults to PasswordDriven.
	if appCred.Rotation.Mode == "" {
		appCred.Rotation.Mode = RotationModePasswordDriven
	}

	return nil
}

// ValidateCreate implements admission.Validator[*ControlPlane] (CC-0110, REQ-006).
// CC-0112, REQ-010 — DECISION boundary 6 = option (a): in addition to the spec
// checks in validate(), a CREATE is rejected when another ControlPlane already
// exists in the same namespace. The per-CR OpenBao credential paths (admin AC,
// bootstrap admin password, fernet/credential keys) are scoped by namespace+name
// and the CredentialRotation reconciler resolves its target by listing
// ControlPlanes in the namespace and expecting exactly one; enforcing
// one-ControlPlane-per-namespace at admission keeps that resolution unambiguous.
// The check runs only on CREATE (not UPDATE) so an existing CR stays mutable.
// Reviewer: please verify boundary 6 = option (a).
func (w *ControlPlaneWebhook) ValidateCreate(ctx context.Context, obj *ControlPlane) (admission.Warnings, error) {
	if err := w.validate(obj); err != nil {
		return nil, err
	}
	return nil, w.validateUniqueInNamespace(ctx, obj)
}

// ValidateUpdate implements admission.Validator[*ControlPlane] (CC-0110, REQ-006).
func (w *ControlPlaneWebhook) ValidateUpdate(_ context.Context, _, newObj *ControlPlane) (admission.Warnings, error) {
	return nil, w.validate(newObj)
}

// ValidateDelete implements admission.Validator[*ControlPlane]. The method is
// required by the Validator interface but is never invoked: the validating
// webhook registers only create/update (no delete verb), so with
// failurePolicy=Fail a down operator can never block ControlPlane CR — and
// thereby namespace — deletion.
func (w *ControlPlaneWebhook) ValidateDelete(_ context.Context, _ *ControlPlane) (admission.Warnings, error) {
	return nil, nil
}

// validate runs all validation rules against the ControlPlane spec (CC-0110,
// REQ-006). The kubebuilder markers / CEL rules on the CRD are the primary
// enforcement point at admission time; the checks below are defense-in-depth
// (mirroring the KeystoneWebhook discipline) so callers that bypass CRD schema
// admission still get field-specific errors.
func (w *ControlPlaneWebhook) validate(cp *ControlPlane) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// REQ-006: openStackRelease must match the date-based release pattern.
	// Mirrors the +kubebuilder:validation:Pattern marker on
	// ControlPlaneSpec.OpenStackRelease.
	if !controlPlaneReleaseRegexp.MatchString(cp.Spec.OpenStackRelease) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("openStackRelease"),
			cp.Spec.OpenStackRelease,
			"must match the OpenStack release pattern ^\\d{4}\\.\\d$ (e.g. 2025.2)",
		))
	}

	// REQ-006: database must use exactly one of clusterRef or host (mirrors the
	// keystone database XOR check / CEL rule).
	db := cp.Spec.Infrastructure.Database
	if (db.ClusterRef != nil) == (db.Host != "") {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("infrastructure", "database"),
			db,
			"exactly one of clusterRef or host must be set",
		))
	}

	// REQ-006: cache must use exactly one of clusterRef or servers (mirrors the
	// keystone cache XOR check / CEL rule).
	cache := cp.Spec.Infrastructure.Cache
	if (cache.ClusterRef != nil) == (len(cache.Servers) > 0) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("infrastructure", "cache"),
			cache,
			"exactly one of clusterRef or servers must be set",
		))
	}

	// REQ-006: the K-ORC admin-credential password Secret reference is required —
	// without it the reconciler cannot (re-)mint the admin application credential.
	if cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("korc", "adminCredential", "passwordSecretRef", "name"),
			"passwordSecretRef.name must be set",
		))
	}

	// REQ-006: reject a Keystone rotationInterval the reconciler's intervalToCron
	// cannot represent (only a positive whole number of days — 168h weekly or any
	// positive multiple of 24h daily) so a bad interval is a clean admission error
	// rather than a steady-state KeystoneReady=False with no requeue. Mirrors
	// intervalToCron in internal/controller/helpers.go and is kept in sync as
	// defense-in-depth, exactly like the openStackRelease pattern check above.
	if ri := cp.Spec.Services.Keystone.RotationInterval; ri != nil {
		if d := ri.Duration; d <= 0 || d%(24*time.Hour) != 0 {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("services", "keystone", "rotationInterval"),
				d.String(),
				"must be a positive whole number of days (e.g. 24h, 168h); only daily and weekly Fernet rotation schedules are supported",
			))
		}
	}

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "ControlPlane"},
			cp.Name,
			allErrs,
		)
	}
	return nil
}

// validateUniqueInNamespace enforces the one-ControlPlane-per-namespace contract
// (CC-0112, REQ-010). It lists existing ControlPlanes in the new object's
// namespace; any pre-existing CR (len >= 1, since the object under admission is
// not yet persisted) makes this CREATE a Forbidden error naming the incumbent.
// The List goes through the injected uncached API reader (mgr.GetAPIReader() in
// production), so the check cannot admit a second CR off a stale or still-empty
// informer cache. The reconciler's duplicate-ControlPlane guard
// (duplicateControlPlaneIncumbent in operators/c5c3/internal/controller) is the
// defense-in-depth for CREATEs that race within the API server itself or bypass
// the webhook entirely.
//
// DECISION: when w.Client is nil (spec-level unit tests that construct a bare
// &ControlPlaneWebhook{}, or any caller that did not inject a client) the check
// is skipped rather than panicking. Production and envtest wiring always inject
// mgr.GetAPIReader() (operators/c5c3/main.go, integration_test.go), so the
// guard never trips at runtime; it only keeps the spec-validation unit tests
// client-free.
func (w *ControlPlaneWebhook) validateUniqueInNamespace(ctx context.Context, obj *ControlPlane) error {
	if w.Client == nil {
		return nil
	}
	var existing ControlPlaneList
	if err := w.Client.List(ctx, &existing, client.InNamespace(obj.Namespace)); err != nil {
		return apierrors.NewInternalError(
			fmt.Errorf("listing ControlPlanes in namespace %q to enforce one-per-namespace: %w", obj.Namespace, err),
		)
	}
	if len(existing.Items) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "ControlPlane"},
		obj.Name,
		field.ErrorList{field.Forbidden(
			field.NewPath("metadata", "namespace"),
			fmt.Sprintf("only one ControlPlane is permitted per namespace; %q already exists in namespace %q (CC-0112)",
				existing.Items[0].Name, obj.Namespace),
		)},
	)
}
