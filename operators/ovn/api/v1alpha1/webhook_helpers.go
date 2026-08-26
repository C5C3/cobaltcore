// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// validateImage checks an optional image reference. Both kinds in this group
// carry one, and the OVNCentral backup block carries a second, so the rule lives
// here rather than in either webhook file.
//
// A nil reference is valid: the operator resolves its own image, which is how an
// unset field keeps tracking the operator's tested version across upgrades. When
// a reference is present it repeats, in the webhook layer, what the
// +kubebuilder:validation markers and the XValidation rule on commonv1.ImageSpec
// already enforce, so the invariant still holds where a schema-layer check is
// bypassed.
func validateImage(fldPath *field.Path, image *commonv1.ImageSpec) field.ErrorList {
	if image == nil {
		return nil
	}
	var errs field.ErrorList
	if image.Repository == "" {
		errs = append(errs, field.Required(fldPath.Child("repository"), "repository must be set"))
	}
	if (image.Tag != "") == (image.Digest != "") {
		errs = append(errs, field.Invalid(
			fldPath, image, "exactly one of image.tag or image.digest must be set",
		))
	}
	return errs
}
