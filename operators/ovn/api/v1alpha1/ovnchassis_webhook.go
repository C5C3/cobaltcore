// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/validation"
	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// updateStrategyOnDelete is the DaemonSet update strategy that hands the rollout
// pace to whoever deletes the pods. maxUnavailable has no meaning under it.
const updateStrategyOnDelete = "OnDelete"

// OVNChassisWebhook implements defaulting and validation webhooks for the
// OVNChassis CRD. Client is injected at startup for cluster-scoped resource
// lookups. Production wiring injects mgr.GetAPIReader(), a direct uncached
// reader, so admission never rejects a just-created object from a stale informer
// cache and no lazy informer start happens inside the webhook timeout.
// +kubebuilder:object:generate=false
type OVNChassisWebhook struct {
	commonwebhook.NoopDeleteValidator[*OVNChassis]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*OVNChassis] = &OVNChassisWebhook{}
	_ admission.Validator[*OVNChassis] = &OVNChassisWebhook{}
)

// +kubebuilder:webhook:path=/mutate-ovn-openstack-c5c3-io-v1alpha1-ovnchassis,mutating=true,failurePolicy=fail,sideEffects=None,groups=ovn.openstack.c5c3.io,resources=ovnchassis,verbs=create;update,versions=v1alpha1,name=movnchassis.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-ovn-openstack-c5c3-io-v1alpha1-ovnchassis,mutating=false,failurePolicy=fail,sideEffects=None,groups=ovn.openstack.c5c3.io,resources=ovnchassis,verbs=create;update,versions=v1alpha1,name=vovnchassis.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *OVNChassisWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*OVNChassis](mgr, &OVNChassis{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*OVNChassis]. It leaves the object
// untouched: every OVNChassis default is either a +kubebuilder:default the API
// server applies from the CRD schema, or a value the operator resolves at
// reconcile time (the image, and the maxUnavailable of 1) so an unset field
// keeps tracking the operator default across upgrades instead of freezing
// today's value into the stored CR.
//
// The mutating webhook is registered nonetheless, so a default that has to be
// materialized later can be added without changing the deployed webhook
// configuration.
func (w *OVNChassisWebhook) Default(_ context.Context, _ *OVNChassis) error {
	return nil
}

// ValidateCreate implements admission.Validator[*OVNChassis].
//
// The metadata.name bound is enforced here rather than in validate(), which
// update shares: the name is immutable, so on update the rule could only ever
// fire against an object a pre-upgrade operator already admitted, and the
// validating webhook also sees the finalizer-removal update reconcileDelete
// issues, so rejecting it would wedge that CR in Terminating with no field left
// to edit to repair it.
func (w *OVNChassisWebhook) ValidateCreate(_ context.Context, obj *OVNChassis) (admission.Warnings, error) {
	return nil, w.validate(obj, validateOVNChassisNameLength(obj.Name))
}

// validateOVNChassisNameLength bounds metadata.name by the child object with the
// tightest name budget, the per-node "{name}-chassis-del-{8 hex}" Job.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateOVNChassisNameLength(name string) field.ErrorList {
	if len(name) <= MaxOVNChassisNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"), name,
		fmt.Sprintf("name must be at most %d characters: the per-node Jobs append up to 21 characters and Kubernetes caps object names at 63",
			MaxOVNChassisNameLength),
	)}
}

// ValidateUpdate implements admission.Validator[*OVNChassis].
//
// spec.targetClusterRef and spec.centralRef are compared across both revisions
// here, the webhook-layer twin of the three transition CEL rules on
// OVNChassisSpec.
func (w *OVNChassisWebhook) ValidateUpdate(_ context.Context, oldObj, newObj *OVNChassis) (admission.Warnings, error) {
	updateErrs := validation.TargetClusterRefImmutable(
		field.NewPath("spec", "targetClusterRef"),
		oldObj.Spec.TargetClusterRef,
		newObj.Spec.TargetClusterRef,
	)

	// Repointing a live chassis leaves its registration behind in the old
	// Southbound database, where it keeps claiming the ports of workloads that
	// have moved on.
	if oldObj.Spec.CentralRef.Name != newObj.Spec.CentralRef.Name {
		updateErrs = append(updateErrs, field.Invalid(
			field.NewPath("spec", "centralRef", "name"),
			newObj.Spec.CentralRef.Name,
			"centralRef is immutable",
		))
	}

	return nil, w.validate(newObj, updateErrs)
}

// validate runs all validation rules against the OVNChassis spec, accumulating
// every violation so users see the full list in one admission response. extra
// carries the errors accumulated by the caller (on create the metadata.name
// bound, on update the targetClusterRef and centralRef immutability checks) so
// they aggregate into the single Invalid error alongside the rest.
func (w *OVNChassisWebhook) validate(c *OVNChassis, extra field.ErrorList) error {
	specPath := field.NewPath("spec")

	allErrs := validation.TargetClusterRef(specPath.Child("targetClusterRef"), c.Spec.TargetClusterRef)
	allErrs = append(allErrs, validateImage(specPath.Child("image"), c.Spec.Image)...)
	allErrs = append(allErrs, validateBridgeMappings(specPath.Child("bridgeMappings"), c.Spec.BridgeMappings)...)
	allErrs = append(allErrs, validateUpdateStrategy(specPath.Child("updateStrategy"), c.Spec.UpdateStrategy)...)

	if c.Spec.CentralRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("centralRef", "name"), "centralRef.name must be set",
		))
	}

	// Defense-in-depth mirrors of the two +kubebuilder:validation:MinProperties=1
	// markers. An empty selector matches every node rather than none, so the
	// permissive reading of "unset" is the dangerous one in both places.
	if len(c.Spec.NodeSelector) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("nodeSelector"), "nodeSelector must carry at least one label",
		))
	}
	if c.Spec.Gateway != nil && len(c.Spec.Gateway.NodeSelector) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("gateway", "nodeSelector"), "gateway.nodeSelector must carry at least one label",
		))
	}

	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "OVNChassis"},
			c.Name,
			allErrs,
		)
	}
	return nil
}

// validateBridgeMappings rejects a bridge or a physical network that appears
// twice. The +listMapKey already makes physicalNetwork unique per list, but the
// bridge side has no schema-level counterpart, and a repeated bridge renders an
// ovn-bridge-mappings string whose second entry silently shadows the first.
func validateBridgeMappings(fldPath *field.Path, mappings []OVNBridgeMapping) field.ErrorList {
	var errs field.ErrorList
	seenNetworks := make(map[string]struct{}, len(mappings))
	seenBridges := make(map[string]struct{}, len(mappings))

	for i, m := range mappings {
		if _, dup := seenNetworks[m.PhysicalNetwork]; dup {
			errs = append(errs, field.Duplicate(fldPath.Index(i).Child("physicalNetwork"), m.PhysicalNetwork))
		}
		seenNetworks[m.PhysicalNetwork] = struct{}{}

		if _, dup := seenBridges[m.Bridge]; dup {
			errs = append(errs, field.Duplicate(fldPath.Index(i).Child("bridge"), m.Bridge))
		}
		seenBridges[m.Bridge] = struct{}{}
	}
	return errs
}

// validateUpdateStrategy rejects the maxUnavailable shapes the DaemonSet would
// accept but not act on: one paired with OnDelete, where nothing reads it, and
// any value that does not resolve to at least one pod.
//
// The field is an IntOrString, so the schema carries x-kubernetes-int-or-string
// and no bound of its own, and chassisUpdateStrategy copies the value through
// verbatim with no maxSurge beside it. Kubernetes' own DaemonSet validation
// then rejects a negative value, a malformed percentage and a percentage that
// scales to zero, and the operator has nowhere to report that: it would fail
// the apply on every pass while OVSReady sat at DaemonSetError. Resolving the
// value here is what turns all of those into one admission message.
//
// The percentage is scaled against 100 rather than against the node count: the
// nodes the DaemonSet lands on are not known at admission time, and a
// percentage that rounds to zero on a smaller cluster is the same wedge. Round
// up, the way the DaemonSet controller does for maxUnavailable, so "1%" is
// admitted and behaves as one node.
func validateUpdateStrategy(fldPath *field.Path, strategy OVNChassisUpdateStrategy) field.ErrorList {
	if strategy.MaxUnavailable == nil {
		return nil
	}
	maxUnavailablePath := fldPath.Child("maxUnavailable")

	if strategy.Type == updateStrategyOnDelete {
		return field.ErrorList{field.Invalid(
			maxUnavailablePath, strategy.MaxUnavailable.String(),
			"maxUnavailable applies to RollingUpdate only",
		)}
	}

	resolved, err := intstr.GetScaledValueFromIntOrPercent(strategy.MaxUnavailable, 100, true)
	if err != nil {
		return field.ErrorList{field.Invalid(
			maxUnavailablePath, strategy.MaxUnavailable.String(),
			"maxUnavailable must be an integer or a percentage such as \"25%\"",
		)}
	}
	if resolved < 1 {
		return field.ErrorList{field.Invalid(
			maxUnavailablePath, strategy.MaxUnavailable.String(),
			"maxUnavailable must resolve to at least 1 for RollingUpdate",
		)}
	}
	return nil
}
