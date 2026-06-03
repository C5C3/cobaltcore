// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"regexp"
	"time"

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
)

// controlPlaneReleaseRegexp mirrors the +kubebuilder:validation:Pattern marker
// on ControlPlaneSpec.OpenStackRelease. The validating webhook re-checks it as
// defense-in-depth for callers that bypass CRD schema admission (CC-0110,
// REQ-006).
var controlPlaneReleaseRegexp = regexp.MustCompile(`^\d{4}\.\d$`)

// ControlPlaneWebhook implements defaulting and validation webhooks for the
// ControlPlane CRD (CC-0110). Client is injected at startup for any future
// cluster-scoped lookups; it is currently unused by validate() but kept to
// mirror the KeystoneWebhook shape and to avoid a signature change later.
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
// +kubebuilder:webhook:path=/validate-c5c3-io-v1alpha1-controlplane,mutating=false,failurePolicy=fail,sideEffects=None,groups=c5c3.io,resources=controlplanes,verbs=create;update;delete,versions=v1alpha1,name=vcontrolplane.kb.io,admissionReviewVersions=v1

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

	// K-ORC admin-credential defaults. cloudCredentialsRef.secretName defaults to
	// the documented shared Secret name.
	korc := &obj.Spec.KORC.AdminCredential
	if korc.CloudCredentialsRef.SecretName == "" {
		korc.CloudCredentialsRef.SecretName = DefaultCloudCredentialsSecretName
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
func (w *ControlPlaneWebhook) ValidateCreate(_ context.Context, obj *ControlPlane) (admission.Warnings, error) {
	return nil, w.validate(obj)
}

// ValidateUpdate implements admission.Validator[*ControlPlane] (CC-0110, REQ-006).
func (w *ControlPlaneWebhook) ValidateUpdate(_ context.Context, _, newObj *ControlPlane) (admission.Warnings, error) {
	return nil, w.validate(newObj)
}

// ValidateDelete implements admission.Validator[*ControlPlane].
// Deletion is always allowed.
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
