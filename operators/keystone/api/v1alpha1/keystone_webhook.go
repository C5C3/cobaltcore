// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"maps"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Default resource requests and limits for the Keystone API container (CC-0042).
// These constants are the single source of truth for the defaulting webhook.
// They ensure Burstable QoS class and enable HPA utilization-based scaling.
// Exported because the controller package tests (reconcile_deployment_test.go)
// reference them for assertion. Mutation is safe: all call sites use DeepCopy().
var (
	DefaultMemoryRequest = resource.MustParse("256Mi")
	DefaultCPURequest    = resource.MustParse("100m")
	DefaultMemoryLimit   = resource.MustParse("512Mi")
	DefaultCPULimit      = resource.MustParse("500m")
)

// KeystoneWebhook implements defaulting and validation webhooks for the Keystone CRD (CC-0011).
// Client is injected at startup for cluster-scoped resource lookups (e.g. PriorityClass validation, CC-0075 REQ-006).
// +kubebuilder:object:generate=false
type KeystoneWebhook struct {
	Client client.Reader
}

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
	// REQ-002 (CC-0040): Default zero-valued sub-fields of spec.uwsgi when non-nil.
	// When the pointer is nil, do nothing — the reconciler uses hardcoded defaults.
	// HTTPKeepAlive is NOT defaulted here: its bool zero value (false) is
	// indistinguishable from an explicit false, so we cannot safely override it
	// without risking overriding explicit user intent (e.g. `httpKeepAlive: false`
	// sent via kubectl patch or weaker schema enforcement paths). The CRD schema
	// default (+kubebuilder:default=true) handles HTTPKeepAlive in the normal
	// admission path; uwsgiCommand uses the CRD default at runtime.
	if obj.Spec.UWSGI != nil {
		if obj.Spec.UWSGI.Processes == 0 {
			obj.Spec.UWSGI.Processes = 2
		}
		if obj.Spec.UWSGI.Threads == 0 {
			obj.Spec.UWSGI.Threads = 1
		}
	}
	// REQ-004 (CC-0042): Default resource requests and limits for Burstable QoS
	// and HPA utilization calculations. Also defaults when Resources is non-nil
	// but empty (e.g. `resources: {}`), which would otherwise produce BestEffort
	// QoS and break HPA utilization calculations.
	if obj.Spec.Resources == nil || (len(obj.Spec.Resources.Requests) == 0 && len(obj.Spec.Resources.Limits) == 0) {
		obj.Spec.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: DefaultMemoryRequest.DeepCopy(),
				corev1.ResourceCPU:    DefaultCPURequest.DeepCopy(),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: DefaultMemoryLimit.DeepCopy(),
				corev1.ResourceCPU:    DefaultCPULimit.DeepCopy(),
			},
		}
	}
	return nil
}

// ValidateCreate implements admission.Validator[*Keystone] (CC-0011, REQ-005).
func (w *KeystoneWebhook) ValidateCreate(ctx context.Context, obj *Keystone) (admission.Warnings, error) {
	return nil, w.validate(ctx, obj)
}

// ValidateUpdate implements admission.Validator[*Keystone] (CC-0011, REQ-006).
func (w *KeystoneWebhook) ValidateUpdate(ctx context.Context, _, newObj *Keystone) (admission.Warnings, error) {
	return nil, w.validate(ctx, newObj)
}

// ValidateDelete implements admission.Validator[*Keystone].
// Deletion is always allowed.
func (w *KeystoneWebhook) ValidateDelete(_ context.Context, _ *Keystone) (admission.Warnings, error) {
	return nil, nil
}

// validate runs all validation rules against the Keystone spec.
// ctx is required for cluster-scoped lookups (PriorityClass validation, CC-0075 REQ-006).
func (w *KeystoneWebhook) validate(ctx context.Context, k *Keystone) error {
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

	// REQ-008 (CC-0057): Validate cron expression for trust flush schedule.
	// Only validated when spec.trustFlush is set (optional pointer field).
	if k.Spec.TrustFlush != nil {
		if k.Spec.TrustFlush.Schedule == "" {
			allErrs = append(allErrs, field.Required(
				specPath.Child("trustFlush", "schedule"),
				"schedule must be set; default is \"0 * * * *\"",
			))
		} else if _, err := cron.ParseStandard(k.Spec.TrustFlush.Schedule); err != nil {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("trustFlush", "schedule"),
				k.Spec.TrustFlush.Schedule,
				fmt.Sprintf("invalid cron expression: %v", err),
			))
		}
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

	// REQ-003 (CC-0040): Defense-in-depth uWSGI validation alongside
	// +kubebuilder:validation:Minimum=1 markers on UWSGISpec fields.
	if k.Spec.UWSGI != nil {
		uwsgiPath := specPath.Child("uwsgi")
		if k.Spec.UWSGI.Processes < 1 {
			allErrs = append(allErrs, field.Invalid(
				uwsgiPath.Child("processes"),
				k.Spec.UWSGI.Processes,
				"processes must be at least 1",
			))
		}
		if k.Spec.UWSGI.Threads < 1 {
			allErrs = append(allErrs, field.Invalid(
				uwsgiPath.Child("threads"),
				k.Spec.UWSGI.Threads,
				"threads must be at least 1",
			))
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

	// REQ-001 (CC-0039): Defense-in-depth networkPolicy ingress check alongside the
	// +kubebuilder:validation:XValidation CEL rule on NetworkPolicySpec (CC-0039).
	if k.Spec.NetworkPolicy != nil && len(k.Spec.NetworkPolicy.Ingress) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("networkPolicy", "ingress"),
			"at least one ingress source must be specified",
		))
	}

	// REQ-007 (CC-0065): Defense-in-depth gateway validation alongside the
	// +kubebuilder:validation:MinLength=1 markers on GatewaySpec.Hostname and
	// GatewayParentRefSpec.Name. Missing required fields would produce an
	// invalid HTTPRoute, so reject early with field-specific errors.
	if k.Spec.Gateway != nil {
		gatewayPath := specPath.Child("gateway")
		if k.Spec.Gateway.Hostname == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("hostname"),
				"hostname must be set when spec.gateway is configured",
			))
		}
		if k.Spec.Gateway.ParentRef.Name == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("parentRef", "name"),
				"parentRef.name must be set when spec.gateway is configured",
			))
		}
	}

	// REQ-004 (CC-0042): Validate that resource requests do not exceed limits.
	if k.Spec.Resources != nil && k.Spec.Resources.Limits != nil {
		for resourceName, request := range k.Spec.Resources.Requests {
			if limit, hasLimit := k.Spec.Resources.Limits[resourceName]; hasLimit && request.Cmp(limit) > 0 {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("resources", "requests", string(resourceName)),
					request.String(),
					fmt.Sprintf("%s request must not exceed limit (%s)", resourceName, limit.String()),
				))
			}
		}
	}

	// REQ-004 (CC-0075): Validate that spec.priorityClassName references an existing
	// scheduling.k8s.io/v1 PriorityClass. Catches typos at admission time.
	if k.Spec.PriorityClassName != nil && *k.Spec.PriorityClassName != "" && w.Client != nil {
		pc := &schedulingv1.PriorityClass{}
		if err := w.Client.Get(ctx, types.NamespacedName{Name: *k.Spec.PriorityClassName}, pc); err != nil {
			if apierrors.IsNotFound(err) {
				allErrs = append(allErrs, field.NotFound(
					specPath.Child("priorityClassName"),
					*k.Spec.PriorityClassName,
				))
			} else {
				allErrs = append(allErrs, field.InternalError(
					specPath.Child("priorityClassName"),
					fmt.Errorf("failed to look up PriorityClass: %w", err),
				))
			}
		}
	}

	// REQ-005 (CC-0075): Validate that custom TopologySpreadConstraints use the correct
	// LabelSelector matching the Deployment's selector labels.
	if k.Spec.TopologySpreadConstraints != nil {
		expectedLabels := map[string]string{
			LabelKeyName:     AppName,
			LabelKeyInstance: k.Name,
		}
		tscPath := specPath.Child("topologySpreadConstraints")
		for i, tsc := range k.Spec.TopologySpreadConstraints {
			if tsc.LabelSelector == nil {
				allErrs = append(allErrs, field.Required(
					tscPath.Index(i).Child("labelSelector"),
					"labelSelector is required on each TopologySpreadConstraint",
				))
				continue
			}
			if !maps.Equal(tsc.LabelSelector.MatchLabels, expectedLabels) {
				allErrs = append(allErrs, field.Invalid(
					tscPath.Index(i).Child("labelSelector"),
					tsc.LabelSelector.MatchLabels,
					fmt.Sprintf("labelSelector.matchLabels must equal the Deployment selector labels %v", expectedLabels),
				))
			}
			// REQ-005 (CC-0075): Reject MatchExpressions to prevent selectors that widen
			// or narrow beyond the Deployment's intent. Only exact matchLabels are allowed.
			if len(tsc.LabelSelector.MatchExpressions) > 0 {
				allErrs = append(allErrs, field.Invalid(
					tscPath.Index(i).Child("labelSelector", "matchExpressions"),
					tsc.LabelSelector.MatchExpressions,
					"matchExpressions are not allowed; labelSelector must use matchLabels only",
				))
			}
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
