// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/validation"
	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// OffboardingTaintKey is the taint openstack-hypervisor-operator puts on a
// node it offboards. It waits for every agent pod on the node that tolerates
// the taint indefinitely to go before it deletes the node's compute service.
const OffboardingTaintKey = "kvm.cloud.sap/offboarding"

// novaComputeUpdateStrategyOnDelete is the DaemonSet update strategy that
// hands the rollout pace to whoever deletes the pods. maxUnavailable has no
// meaning under it.
const novaComputeUpdateStrategyOnDelete = "OnDelete"

// NovaComputeWebhook implements defaulting and validation webhooks for the
// NovaCompute CRD. Client reads the Nova a NovaCompute names, for the
// extraConfig catalog check. Production wiring injects mgr.GetAPIReader(), a
// direct uncached reader, so admission never decides from a stale informer
// cache and no lazy informer start happens inside the webhook timeout.
// +kubebuilder:object:generate=false
type NovaComputeWebhook struct {
	commonwebhook.NoopDeleteValidator[*NovaCompute]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*NovaCompute] = &NovaComputeWebhook{}
	_ admission.Validator[*NovaCompute] = &NovaComputeWebhook{}
)

// +kubebuilder:webhook:path=/mutate-nova-openstack-c5c3-io-v1alpha1-novacompute,mutating=true,failurePolicy=fail,sideEffects=None,groups=nova.openstack.c5c3.io,resources=novacomputes,verbs=create;update,versions=v1alpha1,name=mnovacompute.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-nova-openstack-c5c3-io-v1alpha1-novacompute,mutating=false,failurePolicy=fail,sideEffects=None,groups=nova.openstack.c5c3.io,resources=novacomputes,verbs=create;update,versions=v1alpha1,name=vnovacompute.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *NovaComputeWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*NovaCompute](mgr, &NovaCompute{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*NovaCompute]. It leaves the object
// untouched: every NovaCompute default is either a +kubebuilder:default the API
// server applies from the CRD schema, or a value the operator resolves at
// reconcile time (the image and the maxUnavailable of 1), so an unset field
// keeps tracking the operator default across upgrades.
//
// The mutating webhook is registered nonetheless, so a default that has to be
// materialized later can be added without changing the deployed webhook
// configuration.
func (w *NovaComputeWebhook) Default(_ context.Context, _ *NovaCompute) error {
	return nil
}

// ValidateCreate implements admission.Validator[*NovaCompute].
//
// The metadata.name bound is enforced here rather than in validate(), which
// update shares: the name is immutable, and the validating webhook also sees
// the finalizer-removal update of the teardown, so rejecting an update over a
// name a pre-upgrade operator admitted would wedge the CR in Terminating.
func (w *NovaComputeWebhook) ValidateCreate(ctx context.Context, obj *NovaCompute) (admission.Warnings, error) {
	warnings, createErrs, err := w.novaComputeCatalogCheck(ctx, obj)
	if err != nil {
		return nil, err
	}
	createErrs = append(createErrs, validateNovaComputeNameLength(obj.Name)...)
	return warnings, w.validate(obj, createErrs)
}

// validateNovaComputeNameLength bounds metadata.name by the label value it
// becomes on every child. It is called from ValidateCreate only, for the
// reason documented there.
func validateNovaComputeNameLength(name string) field.ErrorList {
	if len(name) <= MaxNovaComputeNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"), name,
		fmt.Sprintf("name must be at most %d characters: it is the app.kubernetes.io/instance label value of every child",
			MaxNovaComputeNameLength),
	)}
}

// ValidateUpdate implements admission.Validator[*NovaCompute].
//
// spec.targetClusterRef and spec.novaRef are compared across both revisions
// here, the webhook-layer twin of the three transition CEL rules on
// NovaComputeSpec. The extraConfig catalog check re-runs only when the overlay
// changed, so a CR whose overlay went stale against a newer catalog is not
// rejected by an unrelated update such as a selector change.
func (w *NovaComputeWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *NovaCompute) (admission.Warnings, error) {
	updateErrs := validation.TargetClusterRefImmutable(
		field.NewPath("spec", "targetClusterRef"),
		oldObj.Spec.TargetClusterRef,
		newObj.Spec.TargetClusterRef,
	)

	// A pool re-pointed at another Nova leaves its compute services registered
	// with the first one, where they keep claiming the node names.
	if oldObj.Spec.NovaRef.Name != newObj.Spec.NovaRef.Name {
		updateErrs = append(updateErrs, field.Invalid(
			field.NewPath("spec", "novaRef", "name"),
			newObj.Spec.NovaRef.Name,
			"novaRef is immutable",
		))
	}

	var warnings admission.Warnings
	// The release both revisions are checked against is the Nova's, so only the
	// overlay itself can change the outcome: the same value goes in twice.
	if extraConfigCatalogInputsChanged("", "", oldObj.Spec.ExtraConfig, newObj.Spec.ExtraConfig) {
		var catalogErrs field.ErrorList
		var err error
		warnings, catalogErrs, err = w.novaComputeCatalogCheck(ctx, newObj)
		if err != nil {
			return nil, err
		}
		updateErrs = append(updateErrs, catalogErrs...)
	}

	return warnings, w.validate(newObj, updateErrs)
}

// novaComputeCatalogCheck validates the extraConfig option names against the
// catalog of the release the referenced Nova runs, spec.openStackRelease.
//
// An empty overlay has nothing to check and reads nothing. A Nova that does
// not exist yet (pools are commonly applied beside it) skips the check with one
// warning rather than rejecting: its release is not known, and the
// reconciler's ConfigReady and the pods' own startup still catch an option
// nova-compute does not accept. Any other read error fails admission, so a
// flaky API server does not silently skip the check.
func (w *NovaComputeWebhook) novaComputeCatalogCheck(ctx context.Context, obj *NovaCompute) (admission.Warnings, field.ErrorList, error) {
	if len(obj.Spec.ExtraConfig) == 0 {
		return nil, nil, nil
	}

	key := types.NamespacedName{Namespace: obj.Namespace, Name: obj.Spec.NovaRef.Name}
	nova := &Nova{}
	if err := w.Client.Get(ctx, key, nova); err != nil {
		if apierrors.IsNotFound(err) {
			return admission.Warnings{
				fmt.Sprintf("extraConfig catalog check skipped: Nova %s/%s not found", key.Namespace, key.Name),
			}, nil, nil
		}
		return nil, nil, apierrors.NewInternalError(fmt.Errorf("reading Nova %s: %w", key, err))
	}

	warnings, errs := validateExtraConfigOptions(field.NewPath("spec"),
		nova.Spec.OpenStackRelease, obj.Spec.ExtraConfig, NovaComputeOwnedConfigKeys)
	return warnings, errs, nil
}

// validate runs all validation rules against the NovaCompute spec,
// accumulating every violation so users see the full list in one admission
// response. extra carries the errors accumulated by the caller (the catalog
// check, on create the metadata.name bound, on update the immutability checks)
// so they aggregate into the single Invalid error alongside the rest.
func (w *NovaComputeWebhook) validate(nc *NovaCompute, extra field.ErrorList) error {
	specPath := field.NewPath("spec")

	allErrs := validation.TargetClusterRef(specPath.Child("targetClusterRef"), nc.Spec.TargetClusterRef)
	if nc.Spec.Image != nil {
		allErrs = append(allErrs, validateImage(specPath.Child("image"), *nc.Spec.Image)...)
	}

	// The webhook twin of the MinLength marker on NovaRef.Name.
	if nc.Spec.NovaRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("novaRef", "name"),
			"novaRef.name must be set (the Nova this node pool joins)",
		))
	}

	allErrs = append(allErrs, validateNovaComputeNodeSelector(specPath.Child("nodeSelector"), nc.Spec.NodeSelector)...)
	allErrs = append(allErrs, validateNovaComputeTolerations(specPath.Child("tolerations"), nc.Spec.Tolerations)...)
	allErrs = append(allErrs, validateNovaComputeUpdateStrategy(specPath.Child("updateStrategy"), nc.Spec.UpdateStrategy)...)

	// The webhook twin of the CEL rule on NovaComputeLibvirtSpec.
	libvirt := nc.Spec.Libvirt
	if (libvirt.CPUMode == "custom") != (len(libvirt.CPUModels) > 0) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("libvirt", "cpuModels"), libvirt.CPUModels,
			"cpuModels is required when cpuMode is custom and must be empty otherwise",
		))
	}

	allErrs = append(allErrs, validateExtraConfigShape(specPath, nc.Spec.ExtraConfig, NovaComputeOwnedConfigKeys)...)

	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "NovaCompute"},
			nc.Name,
			allErrs,
		)
	}
	return nil
}

// validateNovaComputeNodeSelector is the defense-in-depth mirror of the
// MinProperties=1 marker, plus the label grammar the schema cannot express on a
// map: a key or a value the API server would refuse in a label selector turns
// into a Node List that fails on every pass.
func validateNovaComputeNodeSelector(fldPath *field.Path, selector map[string]string) field.ErrorList {
	if len(selector) == 0 {
		return field.ErrorList{field.Required(fldPath, "nodeSelector must carry at least one label")}
	}
	var errs field.ErrorList
	for key, value := range selector {
		for _, msg := range k8svalidation.IsQualifiedName(key) {
			errs = append(errs, field.Invalid(fldPath, key, msg))
		}
		for _, msg := range k8svalidation.IsValidLabelValue(value) {
			errs = append(errs, field.Invalid(fldPath.Key(key), value, msg))
		}
	}
	return errs
}

// validateNovaComputeTolerations rejects a toleration that keeps the pod on a
// node openstack-hypervisor-operator offboards. It waits for every agent pod
// that tolerates kvm.cloud.sap/offboarding:NoExecute indefinitely to leave
// before it deletes the compute service, and a nova-compute that stays would
// register the service again. A toleration carrying tolerationSeconds is
// admitted: the operator counts it as evictable, and so does the taint manager.
// The match is the one the operator itself runs, so "operator: Exists" with an
// empty key, which tolerates every taint, is rejected too.
func validateNovaComputeTolerations(fldPath *field.Path, tolerations []corev1.Toleration) field.ErrorList {
	offboarding := &corev1.Taint{Key: OffboardingTaintKey, Effect: corev1.TaintEffectNoExecute}
	var errs field.ErrorList
	for i := range tolerations {
		t := tolerations[i]
		if t.TolerationSeconds != nil || !t.ToleratesTaint(logr.Discard(), offboarding, false) {
			continue
		}
		errs = append(errs, field.Invalid(fldPath.Index(i), t,
			"tolerates "+OffboardingTaintKey+":NoExecute; openstack-hypervisor-operator deletes the compute "+
				"service only after every agent pod on the node is gone, and a nova-compute that stays re-registers it"))
	}
	return errs
}

// validateNovaComputeUpdateStrategy rejects the maxUnavailable shapes the
// DaemonSet would accept but not act on: one paired with OnDelete, where
// nothing reads it, and any value that does not resolve to at least one pod.
// Kubernetes' own DaemonSet validation rejects the malformed and zero values
// too, but only at apply time, where the operator would fail every pass.
//
// The percentage is scaled against 100 rather than against the node count,
// which is not known at admission time, and rounded up the way the DaemonSet
// controller rounds maxUnavailable, so "1%" is admitted and behaves as one
// node.
func validateNovaComputeUpdateStrategy(fldPath *field.Path, strategy NovaComputeUpdateStrategy) field.ErrorList {
	if strategy.MaxUnavailable == nil {
		return nil
	}
	maxUnavailablePath := fldPath.Child("maxUnavailable")

	if strategy.Type == novaComputeUpdateStrategyOnDelete {
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
