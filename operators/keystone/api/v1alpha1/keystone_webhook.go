// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"

	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// KeystoneWebhook implements defaulting and validation webhooks for the Keystone CRD (CC-0011).
// +kubebuilder:object:generate=false
type KeystoneWebhook struct{}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*Keystone] = &KeystoneWebhook{}
	_ admission.Validator[*Keystone] = &KeystoneWebhook{}
)

// +kubebuilder:webhook:path=/mutate-keystone-openstack-c5c3-io-v1alpha1-keystone,mutating=true,failurePolicy=fail,sideEffects=None,groups=keystone.openstack.c5c3.io,resources=keystones,verbs=create;update,versions=v1alpha1,name=mkeystone.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-keystone-openstack-c5c3-io-v1alpha1-keystone,mutating=false,failurePolicy=fail,sideEffects=None,groups=keystone.openstack.c5c3.io,resources=keystones,verbs=create;update;delete,versions=v1alpha1,name=vkeystone.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *KeystoneWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*Keystone](mgr, &Keystone{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*Keystone] (CC-0011, REQ-001).
// It sets spec fields to their documented defaults when they carry zero values.
// Fernet.RotationSchedule is NOT defaulted here — it relies on the Kubebuilder
// +kubebuilder:default marker only (plan decision #3, CC-0011).
func (w *KeystoneWebhook) Default(_ context.Context, obj *Keystone) error {
	if obj.Spec.Replicas == 0 {
		obj.Spec.Replicas = 3
	}
	if obj.Spec.Fernet.MaxActiveKeys == 0 {
		obj.Spec.Fernet.MaxActiveKeys = 3
	}
	if obj.Spec.CredentialKeys.MaxActiveKeys == 0 {
		obj.Spec.CredentialKeys.MaxActiveKeys = 3
	}
	if obj.Spec.Cache.Backend == "" {
		obj.Spec.Cache.Backend = "dogpile.cache.pymemcache"
	}
	if obj.Spec.Bootstrap.AdminUser == "" {
		obj.Spec.Bootstrap.AdminUser = "admin"
	}
	if obj.Spec.Bootstrap.Region == "" {
		obj.Spec.Bootstrap.Region = "RegionOne"
	}
	return nil
}

// ValidateCreate implements admission.Validator[*Keystone] (CC-0011, REQ-005).
func (w *KeystoneWebhook) ValidateCreate(_ context.Context, obj *Keystone) (admission.Warnings, error) {
	return nil, w.validate(obj)
}

// ValidateUpdate implements admission.Validator[*Keystone] (CC-0011, REQ-006).
func (w *KeystoneWebhook) ValidateUpdate(_ context.Context, _, newObj *Keystone) (admission.Warnings, error) {
	return nil, w.validate(newObj)
}

// ValidateDelete implements admission.Validator[*Keystone].
// Deletion is always allowed.
func (w *KeystoneWebhook) ValidateDelete(_ context.Context, _ *Keystone) (admission.Warnings, error) {
	return nil, nil
}

// validate runs all validation rules against the Keystone spec.
func (w *KeystoneWebhook) validate(k *Keystone) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// REQ-007 (CC-0011): Defense-in-depth replicas check alongside the
	// +kubebuilder:validation:Minimum=1 marker.
	if k.Spec.Replicas < 1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("replicas"),
			k.Spec.Replicas,
			"replicas must be at least 1",
		))
	}

	// REQ-007 (CC-0011): Defense-in-depth maxActiveKeys check alongside the
	// +kubebuilder:validation:Minimum=3 marker.
	if k.Spec.Fernet.MaxActiveKeys < 3 && k.Spec.Fernet.MaxActiveKeys != 0 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("fernet", "maxActiveKeys"),
			k.Spec.Fernet.MaxActiveKeys,
			"maxActiveKeys must be at least 3",
		))
	}

	// REQ-009 (CC-0036): Defense-in-depth credentialKeys maxActiveKeys check alongside the
	// +kubebuilder:validation:Minimum=3 marker.
	if k.Spec.CredentialKeys.MaxActiveKeys < 3 && k.Spec.CredentialKeys.MaxActiveKeys != 0 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("credentialKeys", "maxActiveKeys"),
			k.Spec.CredentialKeys.MaxActiveKeys,
			"maxActiveKeys must be at least 3",
		))
	}

	// REQ-009 (CC-0011): Defense-in-depth cache mutual-exclusivity check alongside the
	// +kubebuilder:validation:XValidation CEL rule on KeystoneSpec.Cache.
	if (k.Spec.Cache.ClusterRef != nil) == (len(k.Spec.Cache.Servers) > 0) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("cache"),
			k.Spec.Cache,
			"exactly one of clusterRef or servers must be set",
		))
	}

	// REQ-010 (CC-0011): Defense-in-depth database mutual-exclusivity check alongside the
	// +kubebuilder:validation:XValidation CEL rule on KeystoneSpec.Database.
	if (k.Spec.Database.ClusterRef != nil) == (k.Spec.Database.Host != "") {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("database"),
			k.Spec.Database,
			"exactly one of clusterRef or host must be set",
		))
	}

	// REQ-002 (CC-0011): Validate cron expression for Fernet key rotation schedule.
	// NOTE: RotationSchedule is populated by the +kubebuilder:default CRD marker
	// (applied between the mutating and validating webhook phases in the Kubernetes
	// admission pipeline). Callers outside that pipeline must set RotationSchedule
	// explicitly before invoking validate().
	if k.Spec.Fernet.RotationSchedule == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("fernet", "rotationSchedule"),
			"rotationSchedule must be set; default is \"0 0 * * 0\"",
		))
	} else if _, err := cron.ParseStandard(k.Spec.Fernet.RotationSchedule); err != nil {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("fernet", "rotationSchedule"),
			k.Spec.Fernet.RotationSchedule,
			fmt.Sprintf("invalid cron expression: %v", err),
		))
	}

	// REQ-005 (CC-0036): Validate cron expression for credential key rotation schedule.
	if k.Spec.CredentialKeys.RotationSchedule == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("credentialKeys", "rotationSchedule"),
			"rotationSchedule must be set; default is \"0 0 * * 0\"",
		))
	} else if _, err := cron.ParseStandard(k.Spec.CredentialKeys.RotationSchedule); err != nil {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("credentialKeys", "rotationSchedule"),
			k.Spec.CredentialKeys.RotationSchedule,
			fmt.Sprintf("invalid cron expression: %v", err),
		))
	}

	// REQ-003 (CC-0011): Detect duplicate plugin config sections.
	seen := make(map[string]bool, len(k.Spec.Plugins))
	for i, p := range k.Spec.Plugins {
		if seen[p.ConfigSection] {
			allErrs = append(allErrs, field.Duplicate(
				specPath.Child("plugins").Index(i).Child("configSection"),
				p.ConfigSection,
			))
		}
		seen[p.ConfigSection] = true
	}

	// REQ-004 (CC-0011): PolicyOverrides must have at least one source when set.
	if k.Spec.PolicyOverrides != nil {
		if len(k.Spec.PolicyOverrides.Rules) == 0 && k.Spec.PolicyOverrides.ConfigMapRef == nil {
			allErrs = append(allErrs, field.Required(
				specPath.Child("policyOverrides"),
				"at least one of rules or configMapRef must be set",
			))
		}

		// REQ-008 (CC-0011): Detect empty policy rule names.
		for name := range k.Spec.PolicyOverrides.Rules {
			if name == "" {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("policyOverrides", "rules"),
					name,
					"policy rule name must not be empty",
				))
			}
		}
	}

	// REQ-001 (CC-0038): Defense-in-depth autoscaling validation alongside
	// kubebuilder markers and CEL rules.
	if k.Spec.Autoscaling != nil {
		autoscalingPath := specPath.Child("autoscaling")
		if k.Spec.Autoscaling.MaxReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				k.Spec.Autoscaling.MaxReplicas,
				"maxReplicas must be at least 1",
			))
		}
		if k.Spec.Autoscaling.MinReplicas != nil && *k.Spec.Autoscaling.MinReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*k.Spec.Autoscaling.MinReplicas,
				"minReplicas must be at least 1",
			))
		}
		if k.Spec.Autoscaling.MinReplicas != nil && *k.Spec.Autoscaling.MinReplicas > k.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*k.Spec.Autoscaling.MinReplicas,
				"minReplicas must not exceed maxReplicas",
			))
		}
		// When minReplicas is unset, the reconciler defaults it to spec.replicas.
		// Reject configurations where the implicit default would exceed maxReplicas,
		// which would produce an HPA rejected by the API server (CC-0038).
		if k.Spec.Autoscaling.MinReplicas == nil && k.Spec.Replicas > k.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				k.Spec.Autoscaling.MaxReplicas,
				fmt.Sprintf("maxReplicas must be >= spec.replicas (%d) when minReplicas is not set, because minReplicas defaults to spec.replicas", k.Spec.Replicas),
			))
		}
		// REQ-001 (CC-0038): Defense-in-depth bounds checks for utilization targets
		// alongside +kubebuilder:validation:Minimum=1 and +kubebuilder:validation:Maximum=100 markers.
		if k.Spec.Autoscaling.TargetCPUUtilization != nil && (*k.Spec.Autoscaling.TargetCPUUtilization < 1 || *k.Spec.Autoscaling.TargetCPUUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetCPUUtilization"),
				*k.Spec.Autoscaling.TargetCPUUtilization,
				"targetCPUUtilization must be between 1 and 100",
			))
		}
		if k.Spec.Autoscaling.TargetMemoryUtilization != nil && (*k.Spec.Autoscaling.TargetMemoryUtilization < 1 || *k.Spec.Autoscaling.TargetMemoryUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetMemoryUtilization"),
				*k.Spec.Autoscaling.TargetMemoryUtilization,
				"targetMemoryUtilization must be between 1 and 100",
			))
		}
		if k.Spec.Autoscaling.TargetCPUUtilization == nil && k.Spec.Autoscaling.TargetMemoryUtilization == nil {
			allErrs = append(allErrs, field.Required(
				autoscalingPath,
				"at least one of targetCPUUtilization or targetMemoryUtilization must be set",
			))
		}
	}

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Keystone"},
			k.Name,
			allErrs,
		)
	}
	return nil
}
