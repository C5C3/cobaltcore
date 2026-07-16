// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// GlanceBackendWebhook implements defaulting and validation webhooks for the
// GlanceBackend CRD. Client is injected at startup for the single-default
// uniqueness check (a namespace-scoped List of sibling backends). Production
// wiring injects mgr.GetAPIReader() — a direct, uncached reader — so admission
// never misses a just-created sibling from a stale informer cache and no lazy
// informer start happens inside the webhook timeout.
// +kubebuilder:object:generate=false
type GlanceBackendWebhook struct {
	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*GlanceBackend] = &GlanceBackendWebhook{}
	_ admission.Validator[*GlanceBackend] = &GlanceBackendWebhook{}
)

// +kubebuilder:webhook:path=/mutate-glance-openstack-c5c3-io-v1alpha1-glancebackend,mutating=true,failurePolicy=fail,sideEffects=None,groups=glance.openstack.c5c3.io,resources=glancebackends,verbs=create;update,versions=v1alpha1,name=mglancebackend.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-glance-openstack-c5c3-io-v1alpha1-glancebackend,mutating=false,failurePolicy=fail,sideEffects=None,groups=glance.openstack.c5c3.io,resources=glancebackends,verbs=create;update,versions=v1alpha1,name=vglancebackend.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with
// the manager.
func (w *GlanceBackendWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*GlanceBackend](mgr, &GlanceBackend{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*GlanceBackend]. It materializes the
// documented defaults when the fields carry zero values, so the production
// admission path has a single source of truth; the +kubebuilder:default markers
// remain as defense-in-depth for callers that bypass the webhook (e.g. envtest
// without the defaulter wired up).
func (w *GlanceBackendWebhook) Default(_ context.Context, obj *GlanceBackend) error {
	// Mirror the +kubebuilder:default=path marker on S3BackendSpec.BucketURLFormat
	// for webhook-bypassing callers: fill "path" (https://host/bucket) when the
	// s3 block exists and the field is empty.
	if obj.Spec.S3 != nil && obj.Spec.S3.BucketURLFormat == "" {
		obj.Spec.S3.BucketURLFormat = "path"
	}
	return nil
}

// ValidateCreate implements admission.Validator[*GlanceBackend].
func (w *GlanceBackendWebhook) ValidateCreate(ctx context.Context, obj *GlanceBackend) (admission.Warnings, error) {
	return nil, w.validate(ctx, obj)
}

// ValidateUpdate implements admission.Validator[*GlanceBackend].
// Field immutability (glanceRef, type) is enforced by the CRD CEL transition
// rules, so the webhook re-runs only the value-level rules against the new
// object.
func (w *GlanceBackendWebhook) ValidateUpdate(ctx context.Context, _, newObj *GlanceBackend) (admission.Warnings, error) {
	return nil, w.validate(ctx, newObj)
}

// ValidateDelete implements admission.Validator[*GlanceBackend]. The method is
// required by the Validator interface but is never invoked: the validating
// webhook registers only create/update (no delete verb), so with
// failurePolicy=Fail a down operator can never block backend CR — and thereby
// namespace — deletion.
func (w *GlanceBackendWebhook) ValidateDelete(_ context.Context, _ *GlanceBackend) (admission.Warnings, error) {
	return nil, nil
}

// glanceReservedStoreNames enumerates the store identifiers / glance-api.conf
// section names Glance and the operator own. A GlanceBackend's metadata.name
// becomes the store identifier and its [<name>] config section, so a name equal
// to any of these would clobber (or be clobbered by) an operator-owned section.
// Kubernetes object names are lowercase (RFC 1123), so the set is written in
// lowercase; the upstream "DEFAULT" section is matched case-insensitively via
// its lowercase form (see validate). The two os_glance_* stores are also covered
// by the reserved-prefix check, but are listed here so the exact-name message
// names them explicitly.
var glanceReservedStoreNames = map[string]struct{}{
	"default":                 {},
	"database":                {},
	"keystone_authtoken":      {},
	"glance_store":            {},
	"paste_deploy":            {},
	"oslo_policy":             {},
	"os_glance_staging_store": {},
	"os_glance_tasks_store":   {},
}

// s3ExtraOptionsDenylist enumerates the [<store>] option names spec.extraOptions
// must never carry: the options the operator renders from typed S3BackendSpec
// fields (a duplicate would silently shadow — or be shadowed by — the typed
// value depending on render order) plus the operator-owned store_description.
//
//nolint:gosec // G101 false positive: s3 store option names and spec field paths, not credentials.
var s3ExtraOptionsDenylist = map[string]string{
	"s3_store_host":                    "spec.s3.host",
	"s3_store_bucket":                  "spec.s3.bucket",
	"s3_store_access_key":              "spec.s3.credentialsSecretRef",
	"s3_store_secret_key":              "spec.s3.credentialsSecretRef",
	"s3_store_bucket_url_format":       "spec.s3.bucketURLFormat",
	"s3_store_region_name":             "spec.s3.region",
	"s3_store_create_bucket_on_put":    "spec.s3.createBucketOnPut",
	"s3_store_large_object_size":       "spec.s3.largeObjectSize",
	"s3_store_large_object_chunk_size": "spec.s3.largeObjectChunkSize",
	"store_description":                "the operator (store section wiring)",
}

// s3ExtraOptionKeyPattern is the allowlist every spec.extraOptions key must
// match. glance_store option names are snake_case, so letters, digits, and
// underscores are the full legitimate charset. Anything else (an embedded
// newline, a denylist-evading trailing space) could inject an INI line or slip
// past the exact-match denylist, so it is rejected before either exact-match
// check runs.
var s3ExtraOptionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// validate runs all validation rules against the GlanceBackend spec,
// accumulating every violation into a single field.ErrorList so users see all
// problems at once. ctx is required for the sibling-backend List behind the
// single-default rule.
func (w *GlanceBackendWebhook) validate(ctx context.Context, b *GlanceBackend) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Defense-in-depth union check alongside the spec-level CEL rule: exactly
	// one backend block, matching spec.type.
	if (b.Spec.Type == GlanceBackendTypeS3) != (b.Spec.S3 != nil) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("s3"),
			b.Spec.Type,
			"exactly one backend block matching spec.type must be set (type S3 requires spec.s3)",
		))
	}

	allErrs = append(allErrs, validateStoreName(b)...)
	allErrs = append(allErrs, w.validateExtraOptions(specPath.Child("extraOptions"), b)...)
	allErrs = append(allErrs, w.validateSiblingDefault(ctx, specPath, b)...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "GlanceBackend"},
			b.Name,
			allErrs,
		)
	}
	return nil
}

// validateStoreName rejects a metadata.name that collides with a reserved Glance
// store identifier / glance-api.conf section. The comparison is done on the
// lowercased name (Kubernetes names are already lowercase), which also makes the
// "DEFAULT" match case-insensitive. The os_glance_ / os-glance- prefix carves
// out the whole reserved-store namespace Glance uses for its staging and tasks
// stores.
func validateStoreName(b *GlanceBackend) field.ErrorList {
	var errs field.ErrorList
	namePath := field.NewPath("metadata", "name")
	lower := strings.ToLower(b.Name)
	switch {
	case reservedStoreName(lower):
		errs = append(errs, field.Invalid(
			namePath,
			b.Name,
			fmt.Sprintf("name %q collides with the reserved or operator-managed Glance store section of the same name", b.Name),
		))
	case strings.HasPrefix(lower, "os_glance_") || strings.HasPrefix(lower, "os-glance-"):
		errs = append(errs, field.Invalid(
			namePath,
			b.Name,
			`name must not use the reserved "os_glance_" / "os-glance-" store-section prefix (Glance owns that namespace for its staging and tasks stores)`,
		))
	}
	return errs
}

// reservedStoreName reports whether the lowercased name is an exact reserved
// store / section identifier.
func reservedStoreName(lower string) bool {
	_, reserved := glanceReservedStoreNames[lower]
	return reserved
}

// hasControlChars reports whether s contains a newline or carriage-return.
// RenderINI writes every value verbatim as `key = value`, so either character
// lets a value inject an additional store-section line, defeating the
// extraOptions denylist. Every value-side input into the projection is rejected
// for these characters.
func hasControlChars(s string) bool {
	return strings.ContainsAny(s, "\n\r")
}

// validateExtraOptions rejects spec.extraOptions keys the projection owns: the
// options rendered from typed fields, the operator-owned store_description,
// empty keys, keys whose shape could inject INI lines or evade the denylist, and
// values carrying control characters.
func (w *GlanceBackendWebhook) validateExtraOptions(optsPath *field.Path, b *GlanceBackend) field.ErrorList {
	var errs field.ErrorList
	if len(b.Spec.ExtraOptions) == 0 {
		return nil
	}

	// Sort keys so the aggregated error list is deterministic.
	keys := make([]string, 0, len(b.Spec.ExtraOptions))
	for k := range b.Spec.ExtraOptions {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if k == "" {
			errs = append(errs, field.Invalid(optsPath, k, "option name must not be empty"))
			continue
		}
		// Enforce the key allowlist before the denylist, which matches exact
		// strings and so is blind to a newline in the key (an INI-injection
		// vector through RenderINI's verbatim `key = value` write) or a
		// denylist-evading variant such as a trailing space.
		if !s3ExtraOptionKeyPattern.MatchString(k) {
			errs = append(errs, field.Invalid(
				optsPath.Key(k), k,
				"option name must match ^[A-Za-z0-9_]+$ (letters, digits, and underscore)",
			))
			continue
		}
		if owner, denied := s3ExtraOptionsDenylist[k]; denied {
			errs = append(errs, field.Invalid(
				optsPath.Key(k),
				b.Spec.ExtraOptions[k],
				fmt.Sprintf("option %q is owned by %s and must not be set via extraOptions", k, owner),
			))
			continue
		}
		// INI-injection guard: a value with an embedded newline injects arbitrary
		// store-section lines regardless of how innocuous the key is.
		if hasControlChars(b.Spec.ExtraOptions[k]) {
			errs = append(errs, field.Invalid(
				optsPath.Key(k),
				b.Spec.ExtraOptions[k],
				"value must not contain newline or carriage-return characters",
			))
		}
	}
	return errs
}

// validateSiblingDefault enforces the single-default invariant: at most one
// GlanceBackend attached to a given Glance may be marked isDefault. When the CR
// under validation is a default, it lists its namespace siblings (uncached
// reader), filters to the same spec.glanceRef.name, skips self and Terminating
// siblings, and rejects if another default already exists.
func (w *GlanceBackendWebhook) validateSiblingDefault(ctx context.Context, specPath *field.Path, b *GlanceBackend) field.ErrorList {
	// Skip when no lookup client is injected (e.g. direct unit invocation
	// without a reader) — mirrors the PriorityClass validator's behavior.
	if w.Client == nil || !b.Spec.IsDefault {
		return nil
	}

	isDefaultPath := specPath.Child("isDefault")
	var siblings GlanceBackendList
	if err := w.Client.List(ctx, &siblings, client.InNamespace(b.Namespace)); err != nil {
		return field.ErrorList{field.InternalError(isDefaultPath,
			fmt.Errorf("listing GlanceBackends for the single-default check: %w", err))}
	}

	var errs field.ErrorList
	for i := range siblings.Items {
		other := &siblings.Items[i]
		if other.Name == b.Name {
			// Self on UPDATE.
			continue
		}
		if other.DeletionTimestamp != nil {
			// A Terminating sibling is on its way out; blocking a replacement
			// default on it would deadlock recreate-during-teardown.
			continue
		}
		if other.Spec.GlanceRef.Name != b.Spec.GlanceRef.Name {
			continue
		}
		if other.Spec.IsDefault {
			errs = append(errs, field.Invalid(
				isDefaultPath,
				b.Spec.IsDefault,
				fmt.Sprintf("GlanceBackend %q attached to the same Glance %q is already marked isDefault; exactly one default store is allowed",
					other.Name, b.Spec.GlanceRef.Name),
			))
		}
	}
	return errs
}
