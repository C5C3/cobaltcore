// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"regexp"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/validation"
)

// pythonSettingName matches a valid Python identifier. extraConfig keys are
// rendered verbatim as the left-hand side of a `NAME = <literal>` assignment
// in local_settings.py, so a key that is not an identifier could inject
// arbitrary statements (an embedded newline) or evade the exact-match
// SECRET_KEY guard (a trailing space). Anything outside this set is rejected.
var pythonSettingName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// HorizonWebhook implements defaulting and validation webhooks for the
// Horizon CRD. Client is injected at startup for cluster-scoped resource
// lookups (e.g. PriorityClass validation). Production wiring injects
// mgr.GetAPIReader() — a direct, uncached reader — so admission never rejects
// a just-created object from a stale informer cache and no lazy informer
// start happens inside the webhook timeout.
// +kubebuilder:object:generate=false
type HorizonWebhook struct {
	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*Horizon] = &HorizonWebhook{}
	_ admission.Validator[*Horizon] = &HorizonWebhook{}
)

// +kubebuilder:webhook:path=/mutate-horizon-openstack-c5c3-io-v1alpha1-horizon,mutating=true,failurePolicy=fail,sideEffects=None,groups=horizon.openstack.c5c3.io,resources=horizons,verbs=create;update,versions=v1alpha1,name=mhorizon.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-horizon-openstack-c5c3-io-v1alpha1-horizon,mutating=false,failurePolicy=fail,sideEffects=None,groups=horizon.openstack.c5c3.io,resources=horizons,verbs=create;update,versions=v1alpha1,name=vhorizon.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *HorizonWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*Horizon](mgr, &Horizon{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*Horizon]. It sets spec fields to
// their documented defaults when they carry zero values, following the
// keystone webhook's non-mutating discipline: optional pointer blocks are
// only partially filled when explicitly present, except spec.logging which is
// materialized so downstream reconciler code never sees a nil pointer.
func (w *HorizonWebhook) Default(_ context.Context, obj *Horizon) error {
	// Shared-type defaults (replicas, container resources) are applied by the
	// commonv1.DeploymentSpec Default method so they cannot drift across
	// operators.
	obj.Spec.Deployment.Default()
	if obj.Spec.Cache.Backend == "" {
		obj.Spec.Cache.Backend = DefaultCacheBackend
	}
	if obj.Spec.SecretKeyRef.Key == "" {
		obj.Spec.SecretKeyRef.Key = DefaultSecretKeyKey
	}
	// Materialize spec.logging with the production baseline (Format=text,
	// Level=INFO, Debug=false) via the shared LoggingSpec Default method.
	if obj.Spec.Logging == nil {
		obj.Spec.Logging = &LoggingSpec{}
	}
	obj.Spec.Logging.Default()
	return nil
}

// ValidateCreate implements admission.Validator[*Horizon].
func (w *HorizonWebhook) ValidateCreate(ctx context.Context, obj *Horizon) (admission.Warnings, error) {
	return nil, w.validate(ctx, obj)
}

// ValidateUpdate implements admission.Validator[*Horizon].
func (w *HorizonWebhook) ValidateUpdate(ctx context.Context, _, newObj *Horizon) (admission.Warnings, error) {
	return nil, w.validate(ctx, newObj)
}

// ValidateDelete implements admission.Validator[*Horizon]. The method is
// required by the Validator interface but is never invoked: the validating
// webhook registers only create/update (no delete verb), so with
// failurePolicy=Fail a down operator can never block Horizon CR — and
// thereby namespace — deletion.
func (w *HorizonWebhook) ValidateDelete(_ context.Context, _ *Horizon) (admission.Warnings, error) {
	return nil, nil
}

// validate runs all validation rules against the Horizon spec, accumulating
// every violation so users see the full list in one admission response.
// ctx is required for cluster-scoped lookups (PriorityClass validation).
func (w *HorizonWebhook) validate(ctx context.Context, h *Horizon) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Defense-in-depth replicas check alongside the
	// +kubebuilder:validation:Minimum=1 marker.
	if h.Spec.Deployment.Replicas < 1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "replicas"),
			h.Spec.Deployment.Replicas,
			"replicas must be at least 1",
		))
	}

	// Defense-in-depth image tag/digest XOR check alongside the
	// +kubebuilder:validation:XValidation rule on commonv1.ImageSpec: exactly
	// one of tag or digest must be set.
	if (h.Spec.Image.Tag != "") == (h.Spec.Image.Digest != "") {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("image"),
			h.Spec.Image,
			"exactly one of image.tag or image.digest must be set",
		))
	}

	// Defense-in-depth cache mutual-exclusivity check alongside the
	// +kubebuilder:validation:XValidation CEL rule on the shared commonv1
	// type, via the shared validator.
	allErrs = append(allErrs, validation.CacheXOR(specPath.Child("cache"), &h.Spec.Cache)...)

	// Defense-in-depth keystoneEndpoint URL check alongside the
	// +kubebuilder:validation:Pattern=^https?:// marker. The dashboard hands
	// the value verbatim to django-openstack-auth, so an unparseable URL or a
	// missing host would only surface as a login failure at runtime.
	allErrs = append(allErrs, validateKeystoneEndpoint(specPath.Child("keystoneEndpoint"), h.Spec.KeystoneEndpoint)...)

	// Defense-in-depth secretKeyRef check alongside the
	// +kubebuilder:validation:MinLength=1 marker on SecretRefSpec.Name.
	if h.Spec.SecretKeyRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("secretKeyRef", "name"),
			"secretKeyRef.name must be set (the Django SECRET_KEY Secret)",
		))
	}

	// SECRET_KEY must never enter the rendered local_settings.py ConfigMap:
	// the reconciler injects it via the HORIZON_SECRET_KEY env var sourced
	// from spec.secretKeyRef. An extraConfig assignment would render after
	// the env lookup and override it with plaintext key material.
	if _, ok := h.Spec.ExtraConfig["SECRET_KEY"]; ok {
		allErrs = append(allErrs, field.Forbidden(
			specPath.Child("extraConfig").Key("SECRET_KEY"),
			"SECRET_KEY is managed via spec.secretKeyRef and must not be set in extraConfig",
		))
	}
	// extraConfig keys are emitted verbatim as assignment targets in the
	// rendered local_settings.py, so any key that is not a valid Python
	// identifier is a code-injection vector. CEL cannot constrain
	// preserve-unknown-fields map keys, so the webhook is the sole
	// admission-time enforcement point; the pysettings renderer re-validates
	// as the last line of defense.
	for name := range h.Spec.ExtraConfig {
		if name == "" {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("extraConfig"),
				name,
				"extraConfig setting name must not be empty",
			))
			continue
		}
		if !pythonSettingName.MatchString(name) {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("extraConfig").Key(name),
				name,
				"extraConfig setting name must be a valid Python identifier ([A-Za-z_][A-Za-z0-9_]*)",
			))
		}
	}

	// Defense-in-depth logging validation alongside the CRD enum markers on
	// LoggingSpec.Format / .Level. Map values cannot be expressed as a CRD
	// enum on additionalProperties, so the per-logger level check has no
	// schema-layer counterpart — the webhook is the only enforcement point
	// for that case.
	if h.Spec.Logging != nil {
		loggingPath := specPath.Child("logging")
		validLevels := map[string]struct{}{
			"DEBUG":    {},
			"INFO":     {},
			"WARNING":  {},
			"ERROR":    {},
			"CRITICAL": {},
		}
		if h.Spec.Logging.Format != "" && h.Spec.Logging.Format != "text" && h.Spec.Logging.Format != "json" {
			allErrs = append(allErrs, field.NotSupported(
				loggingPath.Child("format"),
				h.Spec.Logging.Format,
				[]string{"text", "json"},
			))
		}
		if h.Spec.Logging.Level != "" {
			if _, ok := validLevels[h.Spec.Logging.Level]; !ok {
				allErrs = append(allErrs, field.NotSupported(
					loggingPath.Child("level"),
					h.Spec.Logging.Level,
					[]string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"},
				))
			}
		}
		perLoggerPath := loggingPath.Child("perLoggerLevels")
		for name, lvl := range h.Spec.Logging.PerLoggerLevels {
			if name == "" {
				allErrs = append(allErrs, field.Invalid(
					perLoggerPath,
					name,
					"logger name must not be empty",
				))
				continue
			}
			if _, ok := validLevels[lvl]; !ok {
				allErrs = append(allErrs, field.Invalid(
					perLoggerPath.Key(name),
					lvl,
					"level must be one of DEBUG, INFO, WARNING, ERROR, CRITICAL",
				))
			}
		}
	}

	// Defense-in-depth range check on spec.deployment.terminationGracePeriodSeconds
	// alongside the +kubebuilder:validation:Minimum=10 marker.
	if h.Spec.Deployment.TerminationGracePeriodSeconds != nil && *h.Spec.Deployment.TerminationGracePeriodSeconds < 10 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "terminationGracePeriodSeconds"),
			*h.Spec.Deployment.TerminationGracePeriodSeconds,
			"terminationGracePeriodSeconds must be at least 10",
		))
	}
	// Defense-in-depth range check on spec.deployment.preStopSleepSeconds
	// alongside the +kubebuilder:validation:Minimum=0 marker.
	if h.Spec.Deployment.PreStopSleepSeconds != nil && *h.Spec.Deployment.PreStopSleepSeconds < 0 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "preStopSleepSeconds"),
			*h.Spec.Deployment.PreStopSleepSeconds,
			"preStopSleepSeconds must be non-negative",
		))
	}

	// preStopSleepSeconds must be strictly less than
	// terminationGracePeriodSeconds so there is a non-zero drain window
	// between the end of the preStop sleep and the forced kubelet kill.
	// Resolve nil pointers to the reconciler's effective defaults so the
	// cross-field rule holds even when one or both pointers are omitted.
	resolvedGrace := commonv1.DefaultTerminationGracePeriodSeconds
	if h.Spec.Deployment.TerminationGracePeriodSeconds != nil {
		resolvedGrace = *h.Spec.Deployment.TerminationGracePeriodSeconds
	}
	resolvedPreStop := commonv1.DefaultPreStopSleepSeconds
	if h.Spec.Deployment.PreStopSleepSeconds != nil {
		resolvedPreStop = *h.Spec.Deployment.PreStopSleepSeconds
	}
	if resolvedPreStop >= resolvedGrace {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "preStopSleepSeconds"),
			resolvedPreStop,
			fmt.Sprintf("preStopSleepSeconds (%d) must be strictly less than terminationGracePeriodSeconds (%d)", resolvedPreStop, resolvedGrace),
		))
	}

	// spec.deployment.strategy sanity check — a Recreate strategy must not
	// carry a RollingUpdate block because the Deployment controller would
	// reject the object at apply time.
	if h.Spec.Deployment.Strategy != nil {
		if h.Spec.Deployment.Strategy.Type == appsv1.RecreateDeploymentStrategyType && h.Spec.Deployment.Strategy.RollingUpdate != nil {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("deployment", "strategy", "rollingUpdate"),
				h.Spec.Deployment.Strategy.RollingUpdate,
				"rollingUpdate must not be set when strategy.type is Recreate",
			))
		}
	}

	// Defense-in-depth autoscaling validation alongside kubebuilder markers
	// and CEL rules.
	if h.Spec.Autoscaling != nil {
		autoscalingPath := specPath.Child("autoscaling")
		if h.Spec.Autoscaling.MaxReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				h.Spec.Autoscaling.MaxReplicas,
				"maxReplicas must be at least 1",
			))
		}
		if h.Spec.Autoscaling.MinReplicas != nil && *h.Spec.Autoscaling.MinReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*h.Spec.Autoscaling.MinReplicas,
				"minReplicas must be at least 1",
			))
		}
		if h.Spec.Autoscaling.MinReplicas != nil && *h.Spec.Autoscaling.MinReplicas > h.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*h.Spec.Autoscaling.MinReplicas,
				"minReplicas must not exceed maxReplicas",
			))
		}
		// When minReplicas is unset, the reconciler defaults it to
		// spec.deployment.replicas. Reject configurations where the implicit
		// default would exceed maxReplicas, which would produce an HPA
		// rejected by the API server.
		if h.Spec.Autoscaling.MinReplicas == nil && h.Spec.Deployment.Replicas > h.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				h.Spec.Autoscaling.MaxReplicas,
				fmt.Sprintf("maxReplicas must be >= spec.deployment.replicas (%d) when minReplicas is not set, because minReplicas defaults to spec.deployment.replicas", h.Spec.Deployment.Replicas),
			))
		}
		// Defense-in-depth bounds checks for utilization targets alongside
		// +kubebuilder:validation:Minimum=1 / Maximum=100 markers.
		if h.Spec.Autoscaling.TargetCPUUtilization != nil && (*h.Spec.Autoscaling.TargetCPUUtilization < 1 || *h.Spec.Autoscaling.TargetCPUUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetCPUUtilization"),
				*h.Spec.Autoscaling.TargetCPUUtilization,
				"targetCPUUtilization must be between 1 and 100",
			))
		}
		if h.Spec.Autoscaling.TargetMemoryUtilization != nil && (*h.Spec.Autoscaling.TargetMemoryUtilization < 1 || *h.Spec.Autoscaling.TargetMemoryUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetMemoryUtilization"),
				*h.Spec.Autoscaling.TargetMemoryUtilization,
				"targetMemoryUtilization must be between 1 and 100",
			))
		}
		if h.Spec.Autoscaling.TargetCPUUtilization == nil && h.Spec.Autoscaling.TargetMemoryUtilization == nil {
			allErrs = append(allErrs, field.Required(
				autoscalingPath,
				"at least one of targetCPUUtilization or targetMemoryUtilization must be set",
			))
		}
	}

	// Defense-in-depth networkPolicy ingress check alongside the
	// +kubebuilder:validation:XValidation CEL rule on NetworkPolicySpec.
	if h.Spec.NetworkPolicy != nil && len(h.Spec.NetworkPolicy.Ingress) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("networkPolicy", "ingress"),
			"at least one ingress source must be specified",
		))
	}

	// Defense-in-depth gateway validation alongside the
	// +kubebuilder:validation:MinLength=1 markers on GatewaySpec.Hostname and
	// GatewayParentRefSpec.Name.
	if h.Spec.Gateway != nil {
		gatewayPath := specPath.Child("gateway")
		if h.Spec.Gateway.Hostname == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("hostname"),
				"hostname must be set when spec.gateway is configured",
			))
		}
		if h.Spec.Gateway.ParentRef.Name == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("parentRef", "name"),
				"parentRef.name must be set when spec.gateway is configured",
			))
		}
	}

	// Validate that resource requests do not exceed limits.
	if h.Spec.Deployment.Resources != nil && h.Spec.Deployment.Resources.Limits != nil {
		for resourceName, request := range h.Spec.Deployment.Resources.Requests {
			if limit, hasLimit := h.Spec.Deployment.Resources.Limits[resourceName]; hasLimit && request.Cmp(limit) > 0 {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("deployment", "resources", "requests", string(resourceName)),
					request.String(),
					fmt.Sprintf("%s request must not exceed limit (%s)", resourceName, limit.String()),
				))
			}
		}
	}

	// Validate that spec.deployment.priorityClassName references an existing
	// scheduling.k8s.io/v1 PriorityClass (shared validator; catches typos at
	// admission time, skipped when no lookup client is injected).
	if h.Spec.Deployment.PriorityClassName != nil {
		allErrs = append(allErrs, validation.PriorityClassExists(ctx, w.Client,
			specPath.Child("deployment", "priorityClassName"), *h.Spec.Deployment.PriorityClassName)...)
	}

	// Validate that custom TopologySpreadConstraints use the correct
	// LabelSelector matching the Deployment's selector labels.
	if h.Spec.Deployment.TopologySpreadConstraints != nil {
		allErrs = append(allErrs, validation.TopologySpreadSelector(
			specPath.Child("deployment", "topologySpreadConstraints"),
			h.Spec.Deployment.TopologySpreadConstraints,
			map[string]string{
				LabelKeyName:     AppName,
				LabelKeyInstance: h.Name,
			},
		)...)
	}

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Horizon"},
			h.Name,
			allErrs,
		)
	}
	return nil
}

// validateKeystoneEndpoint checks that the keystoneEndpoint URL is non-empty,
// parses cleanly, uses an http(s) scheme, and carries a host.
func validateKeystoneEndpoint(fldPath *field.Path, endpoint string) field.ErrorList {
	var errs field.ErrorList
	if endpoint == "" {
		errs = append(errs, field.Required(fldPath, "keystoneEndpoint must be set (the Keystone public endpoint URL)"))
		return errs
	}
	u, err := url.Parse(endpoint)
	switch {
	case err != nil:
		errs = append(errs, field.Invalid(fldPath, endpoint, fmt.Sprintf("must be a valid URL: %v", err)))
	case u.Scheme != "http" && u.Scheme != "https":
		errs = append(errs, field.Invalid(fldPath, endpoint, "scheme must be http or https"))
	case u.Host == "":
		errs = append(errs, field.Invalid(fldPath, endpoint, "URL must include a host"))
	}
	return errs
}
