// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/forge/internal/common/policy"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/validation"
)

// ControlPlane defaulting constants. These are the single source of
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
	// well-known defaults for the database, cache, and admin-credential
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
	// DefaultCacheBackend is materialized when spec.infrastructure.cache.backend
	// is empty. It aliases commonv1.DefaultCacheBackend so the keystone and c5c3
	// operators share one source of truth for the cache backend default.
	DefaultCacheBackend = commonv1.DefaultCacheBackend
	// DefaultCacheClusterRefName is the managed Memcached CR name materialized when
	// spec.infrastructure.cache is in managed mode (servers unset).
	DefaultCacheClusterRefName = "openstack-memcached"
	// DefaultDatabaseStorageSize is the effective per-replica MariaDB volume size
	// when spec.infrastructure.database.storageSize is empty. It aliases
	// commonv1.DatabaseStorageSizeDefault (also the CRD +kubebuilder:default and
	// the c5c3 fresh-create fallback) so validateImmutable normalizes an empty
	// stored value to the exact size the live MariaDB already uses. StorageSize is
	// defaulted by the CRD marker rather than Default() below, so this constant is
	// only consulted by the immutability check, not materialized onto the object.
	DefaultDatabaseStorageSize = commonv1.DatabaseStorageSizeDefault
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
// defense-in-depth for callers that bypass CRD schema admission.
var controlPlaneReleaseRegexp = regexp.MustCompile(`^\d{4}\.\d$`)

// ControlPlaneWebhook implements defaulting and validation webhooks for the
// ControlPlane CRD. Client is injected at startup and used by
// ValidateCreate to enforce one ControlPlane per namespace.
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

// Default implements admission.Defaulter[*ControlPlane].
// It fills only zero-valued fields with their documented defaults, leaving any
// explicit value untouched. It is idempotent: applying it twice produces the
// same result.
func (w *ControlPlaneWebhook) Default(_ context.Context, obj *ControlPlane) error {
	// Plan decision #4: region defaults to RegionOne.
	if obj.Spec.Region == "" {
		obj.Spec.Region = DefaultRegion
	}

	// well-known infrastructure defaults so a minimal managed-mode CR can
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
	// cloudCredentialsRef.cloudName defaults to the conventional admin
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

// ValidateCreate implements admission.Validator[*ControlPlane].
// DECISION boundary 6 = option (a): in addition to the spec
// checks in validate(), a CREATE is rejected when another ControlPlane already
// exists in the same namespace. The per-CR OpenBao credential paths (admin AC,
// bootstrap admin password, fernet/credential keys) are scoped by namespace+name
// and the CredentialRotation reconciler resolves its target by listing
// ControlPlanes in the namespace and expecting exactly one; enforcing
// one-ControlPlane-per-namespace at admission keeps that resolution unambiguous.
// The check runs only on CREATE (not UPDATE) so an existing CR stays mutable.
// Reviewer: please verify boundary 6 = option (a).
func (w *ControlPlaneWebhook) ValidateCreate(ctx context.Context, obj *ControlPlane) (admission.Warnings, error) {
	if err := newInvalidIfErrs(obj, w.validate(obj)); err != nil {
		return nil, err
	}
	return nil, w.validateUniqueInNamespace(ctx, obj)
}

// ValidateUpdate implements admission.Validator[*ControlPlane].
// In addition to the spec checks in validate(), it enforces the create-only
// immutable fields between oldObj and newObj (validateImmutable): flipping the
// database/cache mode or renaming a managed clusterRef would orphan the
// previously-projected MariaDB/Memcached child (and its credentials), renaming
// cloudCredentialsRef.secretName would leak the previously-projected K-ORC
// clouds.yaml ExternalSecret (#476), and renaming the database or changing the
// region would re-point the projection at the now-immutable Keystone child
// fields and wedge the reconcile loop (#466). It additionally rejects an
// openStackRelease downgrade (validateReleaseNotDowngraded), since Keystone DB
// migrations are forward-only. Spec errors, immutability errors, and the
// downgrade error are accumulated into a single Invalid response so a reviewer
// sees all problems at once.
func (w *ControlPlaneWebhook) ValidateUpdate(_ context.Context, oldObj, newObj *ControlPlane) (admission.Warnings, error) {
	allErrs := w.validate(newObj)
	allErrs = append(allErrs, validateImmutable(oldObj, newObj)...)
	allErrs = append(allErrs, validateReleaseNotDowngraded(oldObj, newObj)...)
	return nil, newInvalidIfErrs(newObj, allErrs)
}

// ValidateDelete implements admission.Validator[*ControlPlane]. The method is
// required by the Validator interface but is never invoked: the validating
// webhook registers only create/update (no delete verb), so with
// failurePolicy=Fail a down operator can never block ControlPlane CR — and
// thereby namespace — deletion.
func (w *ControlPlaneWebhook) ValidateDelete(_ context.Context, _ *ControlPlane) (admission.Warnings, error) {
	return nil, nil
}

// validate accumulates all spec validation errors for cp.
// The kubebuilder markers / CEL rules on the CRD are the primary enforcement
// point at admission time; the checks below are defense-in-depth (mirroring the
// KeystoneWebhook discipline) so callers that bypass CRD schema admission still
// get field-specific errors. It returns the accumulated field errors; callers
// wrap them via newInvalidIfErrs so ValidateUpdate can fold in the immutability
// errors before constructing a single Invalid response.
func (w *ControlPlaneWebhook) validate(cp *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// openStackRelease must match the date-based release pattern.
	// Mirrors the +kubebuilder:validation:Pattern marker on
	// ControlPlaneSpec.OpenStackRelease.
	if !controlPlaneReleaseRegexp.MatchString(cp.Spec.OpenStackRelease) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("openStackRelease"),
			cp.Spec.OpenStackRelease,
			"must match the OpenStack release pattern ^\\d{4}\\.\\d$ (e.g. 2025.2)",
		))
	}

	// database must use exactly one of clusterRef or host, and CredentialsMode
	// Dynamic (engine-issued credentials) requires managed mode (ClusterRef
	// set) — the shared validators mirroring the CEL rules on the shared
	// commonv1.DatabaseSpec.
	db := cp.Spec.Infrastructure.Database
	allErrs = append(allErrs, validation.DatabaseXOR(specPath.Child("infrastructure", "database"), &db)...)
	allErrs = append(allErrs, validation.DynamicCredentialsRequireClusterRef(specPath.Child("infrastructure", "database"), &db)...)

	// database.replicas must be 1 (standalone) or >=3 (a quorum-safe Galera
	// cluster). Exactly 2 is rejected because the managed-mode MariaDB projection
	// (ensureMariaDB) turns any replicas>1 into a Galera cluster, and a two-node
	// Galera cluster cannot hold a majority — a single pod disruption (restart,
	// OOM-kill, rolling update, network partition) then loses quorum and takes the
	// whole database offline. The CRD marker only enforces Minimum=1, so this
	// webhook is the enforcement point; the shared commonv1.DatabaseSpec must not
	// carry a c5c3-specific CEL rule the keystone operator (which ignores replicas)
	// would also inherit. A zero value (CRD/webhook default bypassed) is left to
	// the reconciler's floor, so only an explicit 2 is rejected here.
	if db.Replicas == 2 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("infrastructure", "database", "replicas"),
			db.Replicas,
			"database replicas must be 1 (standalone) or >=3 (Galera needs quorum); 2 cannot hold a majority",
		))
	}

	// cache must use exactly one of clusterRef or servers — the shared
	// validator mirroring the CEL rule on the shared commonv1.CacheSpec.
	cache := cp.Spec.Infrastructure.Cache
	allErrs = append(allErrs, validation.CacheXOR(specPath.Child("infrastructure", "cache"), &cache)...)

	// the K-ORC admin-credential password Secret reference is required —
	// without it the reconciler cannot (re-)mint the admin application credential.
	if cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("korc", "adminCredential", "passwordSecretRef", "name"),
			"passwordSecretRef.name must be set",
		))
	}

	// reject a Keystone rotationInterval the reconciler's intervalToCron
	// cannot represent (only a positive whole number of days — 168h weekly or any
	// positive multiple of 24h daily) so a bad interval is a clean admission error
	// rather than a steady-state KeystoneReady=False with no requeue. Mirrors
	// intervalToCron in internal/controller/helpers.go and is kept in sync as
	// defense-in-depth, exactly like the openStackRelease pattern check above.
	// services.keystone is optional; all per-service checks below only apply when
	// the block is present.
	if ks := cp.Spec.Services.Keystone; ks != nil {
		if ri := ks.RotationInterval; ri != nil {
			if d := ri.Duration; d <= 0 || d%(24*time.Hour) != 0 {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("services", "keystone", "rotationInterval"),
					d.String(),
					"must be a positive whole number of days (e.g. 24h, 168h); only daily and weekly Fernet rotation schedules are supported",
				))
			}
		}

		// When a gateway is configured, its hostname must be set. Mirrors the
		// +kubebuilder:validation:MinLength=1 marker on commonv1.GatewaySpec.Hostname;
		// without it the reconciler derives an empty "https:///v3" public endpoint.
		if g := ks.Gateway; g != nil && g.Hostname == "" {
			allErrs = append(allErrs, field.Required(
				specPath.Child("services", "keystone", "gateway", "hostname"),
				"must be set when a gateway is configured",
			))
		}

		// When the Keystone image is overridden, mirror the ImageSpec tag/digest
		// XOR (the +kubebuilder:validation:XValidation rule on commonv1.ImageSpec)
		// with a defense-in-depth check: exactly one of tag or digest must be set.
		if img := ks.Image; img != nil && (img.Tag != "") == (img.Digest != "") {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("services", "keystone", "image"),
				img,
				"exactly one of image.tag or image.digest must be set",
			))
		}
	}

	// Reject empty policy rule names and values on both the global policy and the
	// per-service Keystone override. The c5c3 webhook previously validated policy
	// rules not at all; this mirrors the keystone webhook and the CEL rule on
	// commonv1.PolicySpec, closing the empty-value gap the audit reported
	// (issue #479).
	if g := cp.Spec.GlobalPolicyOverrides; g != nil {
		allErrs = append(allErrs, policy.ValidatePolicyRules(
			g.Rules, specPath.Child("globalPolicyOverrides", "rules"),
		)...)
	}
	if ks := cp.Spec.Services.Keystone; ks != nil && ks.PolicyOverrides != nil {
		allErrs = append(allErrs, policy.ValidatePolicyRules(
			ks.PolicyOverrides.Rules, specPath.Child("services", "keystone", "policyOverrides", "rules"),
		)...)
	}

	return allErrs
}

// newInvalidIfErrs wraps a non-empty field.ErrorList in an apierrors.NewInvalid
// for the ControlPlane GroupKind, or returns nil when there are no errors. It is
// the single point where the validating webhook turns accumulated field errors
// into the admission response, so ValidateCreate and ValidateUpdate share an
// identical error shape.
func newInvalidIfErrs(cp *ControlPlane, allErrs field.ErrorList) error {
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "ControlPlane"},
		cp.Name,
		allErrs,
	)
}

// validateImmutable accumulates errors for every create-only-immutable field
// that changed between oldObj and newObj (#476). The validating webhook is the
// load-bearing mechanism here because the affected leaves live in the shared
// commonv1.DatabaseSpec/CacheSpec types, which the keystone operator reuses and
// which therefore must not carry c5c3-specific CEL immutability markers.
//
//   - Database/cache MODE (managed clusterRef vs brownfield host/servers):
//     flipping it leaves the previously-projected MariaDB/Memcached child (and,
//     in managed mode, its per-ControlPlane credential ExternalSecret) running
//     and owned until the ControlPlane is deleted.
//   - A managed clusterRef.NAME change re-points the projection at a new child
//     and orphans the old one the same way.
//   - A cloudCredentialsRef.secretName change re-points the K-ORC clouds.yaml
//     projection and leaks the previously-named ExternalSecret.
//   - The database NAME (spec.infrastructure.database.database) and the region
//     (spec.region) are projected verbatim into the Keystone child's now-immutable
//     spec.database.database / spec.bootstrap.region (#466). Renaming either here
//     would make the next reconcile attempt an update the Keystone CEL rule
//     rejects, wedging the loop; rejecting the change at the ControlPlane layer
//     surfaces a clean error instead.
//
// validate() already enforces the database/cache XOR (exactly one of clusterRef
// or host/servers), so clusterRef nil-ness is an unambiguous mode discriminator
// here.
func validateImmutable(oldObj, newObj *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList

	dbPath := field.NewPath("spec", "infrastructure", "database")
	oldDB := oldObj.Spec.Infrastructure.Database
	newDB := newObj.Spec.Infrastructure.Database
	switch {
	case (oldDB.ClusterRef != nil) != (newDB.ClusterRef != nil):
		allErrs = append(allErrs, field.Invalid(dbPath, newDB,
			"database mode (managed clusterRef vs brownfield host) is immutable"))
	case oldDB.ClusterRef != nil && newDB.ClusterRef != nil && oldDB.ClusterRef.Name != newDB.ClusterRef.Name:
		allErrs = append(allErrs, field.Invalid(dbPath.Child("clusterRef", "name"),
			newDB.ClusterRef.Name, "managed database clusterRef.name is immutable"))
	}
	if oldDB.Database != newDB.Database {
		allErrs = append(allErrs, field.Invalid(dbPath.Child("database"),
			newDB.Database, "database name is immutable"))
	}
	// database.replicas is create-only. It is projected into the managed MariaDB
	// child's replica count and the derived Galera topology (ensureMariaDB), so
	// editing it on a live ControlPlane would drive a destructive Update on the
	// owned cluster — toggling Galera off (3->1) or scaling a running Galera
	// cluster down. Freezing it here keeps the CR the single source of truth for a
	// topology that can only be changed safely by recreating the control plane,
	// mirroring the database name / region / clusterRef.name immutability above.
	if oldDB.Replicas != newDB.Replicas {
		allErrs = append(allErrs, field.Invalid(dbPath.Child("replicas"),
			newDB.Replicas, "database replicas is immutable after creation "+
				"(toggling Galera or scaling down a live cluster is destructive)"))
	}
	// database.storageSize is create-only for the same reason: it is projected
	// into the owned MariaDB's spec.storage.size, which the mariadb-operator
	// rejects changing on a live CR (its webhook forbids resizing/shrinking the
	// PVC). Freezing it at the ControlPlane layer surfaces the constraint at
	// admission with a clear message instead of letting the edit reach — and be
	// rejected by — the child MariaDB. Mirrors the replicas immutability above.
	//
	// The comparison is against the DEFAULTED value on both sides: a ControlPlane
	// created before storageSize existed has "" persisted (and any read/apply
	// path that bypasses CRD defaulting, e.g. a fake-client test, can surface ""),
	// yet its live MariaDB was provisioned at the default size. Normalizing ""
	// to DefaultDatabaseStorageSize lets such a CR migrate once — an update that
	// pins the field to the default it already runs at is admitted — while any
	// OTHER value is still rejected as a resize the mariadb-operator would refuse.
	if effectiveStorageSize(oldDB.StorageSize) != effectiveStorageSize(newDB.StorageSize) {
		allErrs = append(allErrs, field.Invalid(dbPath.Child("storageSize"),
			newDB.StorageSize, "database storageSize is immutable after creation "+
				"(the mariadb-operator rejects resizing a live volume)"))
	}

	cachePath := field.NewPath("spec", "infrastructure", "cache")
	oldCache := oldObj.Spec.Infrastructure.Cache
	newCache := newObj.Spec.Infrastructure.Cache
	switch {
	case (oldCache.ClusterRef != nil) != (newCache.ClusterRef != nil):
		allErrs = append(allErrs, field.Invalid(cachePath, newCache,
			"cache mode (managed clusterRef vs brownfield servers) is immutable"))
	case oldCache.ClusterRef != nil && newCache.ClusterRef != nil && oldCache.ClusterRef.Name != newCache.ClusterRef.Name:
		allErrs = append(allErrs, field.Invalid(cachePath.Child("clusterRef", "name"),
			newCache.ClusterRef.Name, "managed cache clusterRef.name is immutable"))
	}

	oldSecretName := oldObj.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	newSecretName := newObj.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	if oldSecretName != newSecretName {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "korc", "adminCredential", "cloudCredentialsRef", "secretName"),
			newSecretName, "cloudCredentialsRef.secretName is immutable",
		))
	}

	if oldObj.Spec.Region != newObj.Spec.Region {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "region"),
			newObj.Spec.Region, "region is immutable",
		))
	}

	return allErrs
}

// effectiveStorageSize resolves an empty database.storageSize to the default the
// c5c3 fresh-create projection actually provisions (DefaultDatabaseStorageSize),
// so validateImmutable compares the sizes the live MariaDB runs at rather than
// the raw spec strings. This is what lets a pre-existing ControlPlane (stored
// with "" before the field existed) migrate once to an explicit default.
func effectiveStorageSize(size string) string {
	if size == "" {
		return DefaultDatabaseStorageSize
	}
	return size
}

// validateReleaseNotDowngraded rejects an openStackRelease downgrade on UPDATE.
// OpenStack/Keystone DB migrations are forward-only (keystone-manage db_sync has
// no down-migration path), so re-pointing a live control plane at an older
// release would project an older image whose schema is behind the already-migrated
// database -- an unrecoverable state. Upgrades and same-release updates are
// allowed. Both releases are guarded against controlPlaneReleaseRegexp first; a
// release that does not match the YYYY.N pattern is left to validate()'s pattern
// check rather than mis-parsed here, so a malformed value yields the pattern
// error alone instead of a confusing downgrade message.
func validateReleaseNotDowngraded(oldObj, newObj *ControlPlane) field.ErrorList {
	oldRel := oldObj.Spec.OpenStackRelease
	newRel := newObj.Spec.OpenStackRelease
	if !controlPlaneReleaseRegexp.MatchString(oldRel) || !controlPlaneReleaseRegexp.MatchString(newRel) {
		return nil
	}
	// Compare the (year, minor) integer tuples rather than the raw strings. The
	// regex re-check above guarantees both values are well-formed (a 4-digit year,
	// the dot, then an all-digit minor), so the byte offsets are safe and Atoi
	// cannot fail. Integer comparison keeps this correct even if
	// controlPlaneReleaseRegexp is ever loosened to admit a multi-digit minor
	// (e.g. 2025.10), where lexicographic string ordering would silently invert
	// ("2025.10" < "2025.2" as strings) and reject a real upgrade while admitting
	// a real downgrade.
	parse := func(r string) (year, minor int) {
		year, _ = strconv.Atoi(r[:4])
		minor, _ = strconv.Atoi(r[5:])
		return year, minor
	}
	oldYear, oldMinor := parse(oldRel)
	newYear, newMinor := parse(newRel)
	if newYear < oldYear || (newYear == oldYear && newMinor < oldMinor) {
		return field.ErrorList{field.Invalid(
			field.NewPath("spec", "openStackRelease"),
			newRel,
			fmt.Sprintf("openStackRelease downgrade from %q to %q is not permitted; Keystone DB migrations are not reversible", oldRel, newRel),
		)}
	}
	return nil
}

// validateUniqueInNamespace enforces the one-ControlPlane-per-namespace contract
// It lists existing ControlPlanes in the new object's
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
			fmt.Sprintf("only one ControlPlane is permitted per namespace; %q already exists in namespace %q",
				existing.Items[0].Name, obj.Namespace),
		)},
	)
}
