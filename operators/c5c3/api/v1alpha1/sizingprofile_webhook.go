// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// SizingProfileWebhook implements defaulting and validation webhooks for the
// SizingProfile CRD. Client reads the PriorityClasses a profile names and the
// ControlPlanes that reference it; production wiring injects
// mgr.GetAPIReader(), so a create or an update is checked against the
// ControlPlanes that exist now rather than a cache that may lag behind. A nil
// Client skips both reads.
//
// Delete is not validated: the shared webhook template registers no DELETE
// rule, so a referenced profile can be deleted, and the ControlPlanes that
// reference it report SizingReady=False and keep their children as last
// projected.
// +kubebuilder:object:generate=false
type SizingProfileWebhook struct {
	commonwebhook.NoopDeleteValidator[*SizingProfile]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*SizingProfile] = &SizingProfileWebhook{}
	_ admission.Validator[*SizingProfile] = &SizingProfileWebhook{}
)

// +kubebuilder:webhook:path=/mutate-c5c3-io-v1alpha1-sizingprofile,mutating=true,failurePolicy=fail,sideEffects=None,groups=c5c3.io,resources=sizingprofiles,verbs=create;update,versions=v1alpha1,name=msizingprofile.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-c5c3-io-v1alpha1-sizingprofile,mutating=false,failurePolicy=fail,sideEffects=None,groups=c5c3.io,resources=sizingprofiles,verbs=create;update,versions=v1alpha1,name=vsizingprofile.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with
// the manager.
func (w *SizingProfileWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*SizingProfile](mgr, &SizingProfile{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*SizingProfile]. An empty spec.base
// becomes Standard, the twin of the CRD default for callers that bypass it.
func (w *SizingProfileWebhook) Default(_ context.Context, obj *SizingProfile) error {
	if obj.Spec.Base == "" {
		obj.Spec.Base = SizingProfileStandard
	}
	return nil
}

// ValidateCreate implements admission.Validator[*SizingProfile]. It checks the
// profile's values (validateSizingSpec), the base, and every PriorityClass the
// profile names, and then the merged sizing of every ControlPlane that already
// references it: a profile restored after its deletion meets ControlPlanes
// whose spec.sizing edits were admitted without a merge while it was missing.
func (w *SizingProfileWebhook) ValidateCreate(ctx context.Context, obj *SizingProfile) (admission.Warnings, error) {
	allErrs := validateSizingProfile(obj)
	allErrs = append(allErrs, validateNewPriorityClasses(ctx, w.Client, field.NewPath("spec"), nil, &obj.Spec.SizingSpec)...)
	allErrs = append(allErrs, w.validateReferencingControlPlanes(ctx, obj)...)
	return nil, newInvalidSizingProfileIfErrs(obj, allErrs)
}

// ValidateUpdate implements admission.Validator[*SizingProfile]. It runs the
// create checks, looking up only the PriorityClasses the old object did not
// name, and then, when the spec changed, checks the merged sizing of every
// ControlPlane that references the profile, since such an edit changes what
// each of them projects. Each such error names the ControlPlane it belongs to.
// A metadata-only edit changes no merged sizing, so it lists nothing.
func (w *SizingProfileWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *SizingProfile) (admission.Warnings, error) {
	allErrs := validateSizingProfile(newObj)
	allErrs = append(allErrs, validateNewPriorityClasses(ctx, w.Client, field.NewPath("spec"),
		&oldObj.Spec.SizingSpec, &newObj.Spec.SizingSpec)...)
	if !equality.Semantic.DeepEqual(oldObj.Spec, newObj.Spec) {
		allErrs = append(allErrs, w.validateReferencingControlPlanes(ctx, newObj)...)
	}
	return nil, newInvalidSizingProfileIfErrs(newObj, allErrs)
}

// validateSizingProfile checks the values of a profile as written. The base
// check is the twin of the Enum marker on SizingProfileName.
func validateSizingProfile(p *SizingProfile) field.ErrorList {
	specPath := field.NewPath("spec")
	allErrs := validateSizingSpec(specPath, &p.Spec.SizingSpec)
	switch p.Spec.Base {
	case "", SizingProfileMinimal, SizingProfileStandard:
	default:
		allErrs = append(allErrs, field.NotSupported(specPath.Child("base"), p.Spec.Base,
			[]SizingProfileName{SizingProfileMinimal, SizingProfileStandard}))
	}
	return allErrs
}

// validateReferencingControlPlanes resolves the sizing of every ControlPlane
// whose spec.sizing.profileRef names p against p's new values and reports
// every merged value validateResolvedSizing rejects, at spec, naming the
// ControlPlane. A failed List rejects the request: admitting a profile that
// could not be checked would let an invalid sizing reach the children.
func (w *SizingProfileWebhook) validateReferencingControlPlanes(ctx context.Context, p *SizingProfile) field.ErrorList {
	if w.Client == nil {
		return nil
	}
	specPath := field.NewPath("spec")
	var cps ControlPlaneList
	if err := w.Client.List(ctx, &cps); err != nil {
		return field.ErrorList{field.InternalError(specPath, fmt.Errorf("listing ControlPlanes: %w", err))}
	}
	var allErrs field.ErrorList
	for i := range cps.Items {
		cp := &cps.Items[i]
		if s := cp.Spec.Sizing; s == nil || s.ProfileRef == nil || s.ProfileRef.Name != p.Name {
			continue
		}
		resolved := ResolveSizing(cp, p)
		for _, err := range validateResolvedSizing(field.NewPath("spec", "sizing"), &resolved) {
			allErrs = append(allErrs, field.Invalid(specPath, p.Name,
				fmt.Sprintf("ControlPlane %s/%s: %s", cp.Namespace, cp.Name, err.Error())))
		}
	}
	return allErrs
}

// newInvalidSizingProfileIfErrs wraps accumulated field errors into a single
// Invalid response, or returns nil when there are none.
func newInvalidSizingProfileIfErrs(p *SizingProfile, allErrs field.ErrorList) error {
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "SizingProfile"},
		p.Name,
		allErrs,
	)
}
