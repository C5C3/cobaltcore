// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"math"
	"slices"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

// services.glance.quotas reaches Keystone as registered limits on the Glance
// registration and reaches Glance as spec.keystoneLimits on its child. The two
// halves move in a fixed order, because oslo.limit counts a resource without
// any limit as limit 0 and a glance-api pod that enforces against missing
// limits refuses every image create:
//
//   - enable: the registration carries the four limits and reader on the
//     system first, and the child gets spec.keystoneLimits only once the
//     registration reports CatalogReady and AccountReady for that spec
//     (glanceQuotaRegistrationReady). Once on, the child keeps it, so a
//     transient CatalogReady=False during a later value change does not roll
//     Glance off and on.
//   - disable: the child loses spec.keystoneLimits first, and the registration
//     keeps the limits until every glance-api pod runs a template without it
//     (glanceQuotaHoldRequired).

const (
	// glanceQuotaSystemRole is the role the glance service user holds on the
	// system while quotas are enforced. oslo.limit finds Glance's endpoint
	// through GET /v3/services and GET /v3/endpoints, which Keystone allows an
	// account without admin only as reader on the system.
	glanceQuotaSystemRole = "reader"
	// glanceQuotaUnbounded is the default an unset quota field resolves to: the
	// largest registered limit Keystone and K-ORC accept. -1 is never written,
	// because oslo.limit 2.10.0 (2026.1) reads it as a limit below any usage.
	glanceQuotaUnbounded int32 = math.MaxInt32
	// reasonWaitingForGlanceQuotas holds GlanceReady while quotas are set but the
	// registration has not reported its limits and its system role ready, so the
	// Glance child is not enforcing yet.
	reasonWaitingForGlanceQuotas = "WaitingForGlanceQuotas"
)

// The four resources Glance enforces, as Keystone registers them.
const (
	glanceQuotaImageSizeTotal      = "image_size_total"
	glanceQuotaImageStageTotal     = "image_stage_total"
	glanceQuotaImageCountTotal     = "image_count_total"
	glanceQuotaImageCountUploading = "image_count_uploading"
)

// glanceQuotaResourceNames are the four resources Glance enforces, in the order
// the registration lists them.
var glanceQuotaResourceNames = []string{
	glanceQuotaImageSizeTotal, glanceQuotaImageStageTotal, glanceQuotaImageCountTotal, glanceQuotaImageCountUploading,
}

// glanceQuotaRegisteredLimits resolves services.glance.quotas into the four
// registered limits of the Glance registration, nil for a nil block. A size is
// registered in MiB, a count as given, an unset field as glanceQuotaUnbounded.
// The error surfaces only for a size the webhook would have refused.
func glanceQuotaRegisteredLimits(
	q *c5c3v1alpha1.GlanceQuotasSpec,
) ([]c5c3v1alpha1.KeystoneServiceRegisteredLimitSpec, error) {
	if q == nil {
		return nil, nil
	}
	size := func(field string, v *resource.Quantity) (int32, error) {
		if v == nil {
			return glanceQuotaUnbounded, nil
		}
		mib, err := c5c3v1alpha1.GlanceQuotaMiB(*v)
		if err != nil {
			return 0, fmt.Errorf("%s %s: %w", field, v.String(), err)
		}
		return mib, nil
	}
	count := func(v *int32) int32 {
		if v == nil {
			return glanceQuotaUnbounded
		}
		return *v
	}
	sizeTotal, err := size("imageSizeTotal", q.ImageSizeTotal)
	if err != nil {
		return nil, err
	}
	stageTotal, err := size("imageStageTotal", q.ImageStageTotal)
	if err != nil {
		return nil, err
	}
	return []c5c3v1alpha1.KeystoneServiceRegisteredLimitSpec{
		{ResourceName: glanceQuotaImageSizeTotal, DefaultLimit: sizeTotal},
		{ResourceName: glanceQuotaImageStageTotal, DefaultLimit: stageTotal},
		{ResourceName: glanceQuotaImageCountTotal, DefaultLimit: count(q.ImageCountTotal)},
		{ResourceName: glanceQuotaImageCountUploading, DefaultLimit: count(q.ImageCountUploading)},
	}, nil
}

// withGlanceQuotaRegistration adds the registered limits and the system role
// quota enforcement needs to the desired Glance registration. Without limits it
// leaves both fields unset, so a registration without quotas keeps the spec,
// and the generation, it had before quotas existed.
func withGlanceQuotaRegistration(
	ks *c5c3v1alpha1.KeystoneService, limits []c5c3v1alpha1.KeystoneServiceRegisteredLimitSpec,
) *c5c3v1alpha1.KeystoneService {
	if len(limits) > 0 {
		ks.Spec.Catalog.RegisteredLimits = limits
		ks.Spec.Account.SystemRoles = []string{glanceQuotaSystemRole}
	}
	return ks
}

// glanceQuotaHeldLimits returns the registered limits of the four Glance
// resources the live registration carries, the entries the disable hold keeps
// asserting.
func glanceQuotaHeldLimits(live *c5c3v1alpha1.KeystoneService) []c5c3v1alpha1.KeystoneServiceRegisteredLimitSpec {
	if live == nil || live.Spec.Catalog == nil {
		return nil
	}
	var held []c5c3v1alpha1.KeystoneServiceRegisteredLimitSpec
	for _, rl := range live.Spec.Catalog.RegisteredLimits {
		if slices.Contains(glanceQuotaResourceNames, rl.ResourceName) {
			held = append(held, rl)
		}
	}
	return held
}

// glanceQuotaHoldRequired reports whether a registration whose quotas were
// removed must keep its registered limits: the live registration still carries
// one of the four, and a glance-api pod may still enforce, because the live
// Glance child either still sets spec.keystoneLimits or has not rolled every
// replica onto its current template. A pod that enforces against a deleted
// limit counts it as 0 and refuses every upload. The caller runs it only while
// services.glance.quotas is nil.
func glanceQuotaHoldRequired(liveRegistration *c5c3v1alpha1.KeystoneService, liveGlance *glancev1alpha1.Glance) bool {
	if len(glanceQuotaHeldLimits(liveRegistration)) == 0 || liveGlance == nil {
		return false
	}
	// The surge-tolerant Ready condition turns True while old-template pods still
	// serve, so the hold reads status.rolledOutGeneration instead.
	return liveGlance.Spec.KeystoneLimits != nil || liveGlance.Status.RolledOutGeneration != liveGlance.Generation
}

// glanceQuotaRegistrationReady reports whether the registration has registered
// all four limits and granted reader on the system for its current spec: both
// lists name them, and CatalogReady and AccountReady are True for the
// registration's generation.
func glanceQuotaRegistrationReady(child *c5c3v1alpha1.KeystoneService) bool {
	return glanceQuotaBlockingCondition(child) == nil && glanceQuotaRegistrationDeclared(child)
}

// glanceQuotaRegistrationDeclared reports whether the registration's spec carries
// the four limits and the system role.
func glanceQuotaRegistrationDeclared(child *c5c3v1alpha1.KeystoneService) bool {
	if child.Spec.Catalog == nil || child.Spec.Account == nil ||
		!slices.Contains(child.Spec.Account.SystemRoles, glanceQuotaSystemRole) {
		return false
	}
	for _, name := range glanceQuotaResourceNames {
		if !slices.ContainsFunc(child.Spec.Catalog.RegisteredLimits, func(rl c5c3v1alpha1.KeystoneServiceRegisteredLimitSpec) bool {
			return rl.ResourceName == name
		}) {
			return false
		}
	}
	return true
}

// glanceQuotaBlockingCondition returns the first of CatalogReady and AccountReady
// that is not True for the registration's current generation, a placeholder for
// one not reported yet, or nil when both are.
func glanceQuotaBlockingCondition(child *c5c3v1alpha1.KeystoneService) *metav1.Condition {
	for _, condType := range []string{conditionTypeKeystoneServiceCatalogReady, conditionTypeKeystoneServiceAccountReady} {
		cond := conditions.GetCondition(child.Status.Conditions, condType)
		if cond == nil {
			return &metav1.Condition{
				Type: condType, Status: metav1.ConditionUnknown, Reason: "NotReported",
				Message: "the condition has not been reported yet",
			}
		}
		if cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != child.Generation {
			return cond
		}
	}
	return nil
}
