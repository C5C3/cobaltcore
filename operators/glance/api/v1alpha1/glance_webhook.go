// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/naming"
	"github.com/c5c3/forge/internal/common/release"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/validation"
)

// uWSGI defaults applied by the defaulting webhook (Processes/Threads/
// HTTPKeepAlive) and the reconciler (when spec.apiServer.uwsgi is nil). They are
// the single source of truth so the webhook and the reconciler cannot drift; the
// +kubebuilder:default markers on UWSGISpec keep the same literals in sync
// separately (markers cannot reference Go constants).
const (
	// DefaultUWSGIProcesses is the uWSGI worker-process count materialized when
	// spec.apiServer.uwsgi.processes is zero.
	DefaultUWSGIProcesses int32 = 2
	// DefaultUWSGIThreads is the per-worker thread count materialized when
	// spec.apiServer.uwsgi.threads is zero.
	DefaultUWSGIThreads int32 = 1
	// DefaultUWSGIHTTPKeepAlive is the --http-keepalive default restored when
	// spec.apiServer.uwsgi.httpKeepAlive is nil (unset).
	DefaultUWSGIHTTPKeepAlive = true
)

// GlanceWebhook implements defaulting and validation webhooks for the Glance
// CRD. Client is injected at startup for cluster-scoped resource lookups (e.g.
// PriorityClass validation). Production wiring injects mgr.GetAPIReader() — a
// direct, uncached reader — so admission never rejects a just-created object
// from a stale informer cache and no lazy informer start happens inside the
// webhook timeout.
// +kubebuilder:object:generate=false
type GlanceWebhook struct {
	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*Glance] = &GlanceWebhook{}
	_ admission.Validator[*Glance] = &GlanceWebhook{}
)

// +kubebuilder:webhook:path=/mutate-glance-openstack-c5c3-io-v1alpha1-glance,mutating=true,failurePolicy=fail,sideEffects=None,groups=glance.openstack.c5c3.io,resources=glances,verbs=create;update,versions=v1alpha1,name=mglance.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-glance-openstack-c5c3-io-v1alpha1-glance,mutating=false,failurePolicy=fail,sideEffects=None,groups=glance.openstack.c5c3.io,resources=glances,verbs=create;update,versions=v1alpha1,name=vglance.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *GlanceWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*Glance](mgr, &Glance{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*Glance]. It sets spec fields to their
// documented defaults when they carry zero values, following the keystone/
// horizon non-mutating discipline: optional pointer blocks are only partially
// filled when explicitly present, except spec.logging which is materialized so
// downstream reconciler code never sees a nil pointer.
func (w *GlanceWebhook) Default(_ context.Context, obj *Glance) error {
	// Shared-type defaults (replicas, container resources) are applied by the
	// commonv1.DeploymentSpec Default method so they cannot drift across
	// operators.
	obj.Spec.Deployment.Default()
	if obj.Spec.Cache.Backend == "" {
		obj.Spec.Cache.Backend = commonv1.DefaultCacheBackend
	}
	// Materialize spec.logging with the production baseline (Format=text,
	// Level=INFO, Debug=false) via the shared LoggingSpec Default method so
	// downstream reconciler code dereferences spec.logging unconditionally.
	if obj.Spec.Logging == nil {
		obj.Spec.Logging = &LoggingSpec{}
	}
	obj.Spec.Logging.Default()

	// ServiceUser identity defaults: fill each only when empty so an explicit
	// value is never clobbered. A minimal CR need only supply the password
	// Secret reference.
	su := &obj.Spec.ServiceUser
	if su.Username == "" {
		su.Username = "glance"
	}
	if su.ProjectName == "" {
		su.ProjectName = "service"
	}
	if su.UserDomainName == "" {
		su.UserDomainName = "Default"
	}
	if su.ProjectDomainName == "" {
		su.ProjectDomainName = "Default"
	}
	if su.SecretRef.Key == "" {
		su.SecretRef.Key = "password"
	}

	// Default zero-valued sub-fields of spec.apiServer.uwsgi when the block is
	// non-nil. When the pointer is nil, do nothing — the reconciler uses
	// hardcoded defaults for the active launch mode. HTTPKeepAlive is a
	// nil-preserving *bool, so "unset" is distinguishable from an explicit
	// false: the webhook restores the documented default (true) only when the
	// pointer is nil, and leaves an explicit true/false untouched.
	if obj.Spec.APIServer != nil && obj.Spec.APIServer.UWSGI != nil {
		u := obj.Spec.APIServer.UWSGI
		if u.Processes == 0 {
			u.Processes = DefaultUWSGIProcesses
		}
		if u.Threads == 0 {
			u.Threads = DefaultUWSGIThreads
		}
		if u.HTTPKeepAlive == nil {
			u.HTTPKeepAlive = ptr.To(DefaultUWSGIHTTPKeepAlive)
		}
	}
	return nil
}

// ValidateCreate implements admission.Validator[*Glance].
func (w *GlanceWebhook) ValidateCreate(ctx context.Context, obj *Glance) (admission.Warnings, error) {
	catalogWarnings, catalogErrs := validateExtraConfigOptions(field.NewPath("spec"), obj)
	return append(warnInertLaunchModeKnobs(obj), catalogWarnings...), w.validate(ctx, obj, catalogErrs)
}

// ValidateUpdate implements admission.Validator[*Glance].
//
// The extraConfig option-catalog check is re-run only when one of its inputs
// changed (extraConfig, the plugin config-section set, or spec.openStackRelease).
// This keeps an unrelated update — for example scaling replicas — from
// retroactively rejecting a CR whose extraConfig was accepted at create time but
// has since been invalidated by a regenerated catalog.
func (w *GlanceWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *Glance) (admission.Warnings, error) {
	var catalogWarnings admission.Warnings
	var catalogErrs field.ErrorList
	if extraConfigCatalogInputsChanged(oldObj, newObj) {
		catalogWarnings, catalogErrs = validateExtraConfigOptions(field.NewPath("spec"), newObj)
	}
	return append(warnInertLaunchModeKnobs(newObj), catalogWarnings...), w.validate(ctx, newObj, catalogErrs)
}

// ValidateDelete implements admission.Validator[*Glance]. The method is required
// by the Validator interface but is never invoked: the validating webhook
// registers only create/update (no delete verb), so with failurePolicy=Fail a
// down operator can never block Glance CR — and thereby namespace — deletion.
func (w *GlanceWebhook) ValidateDelete(_ context.Context, _ *Glance) (admission.Warnings, error) {
	return nil, nil
}

// validate runs all validation rules against the Glance spec, accumulating
// every violation so users see the full list in one admission response.
// ctx is required for cluster-scoped lookups (PriorityClass validation).
// extra carries errors accumulated by the caller (the extraConfig option-catalog
// check) so they aggregate into the single Invalid error alongside the rest.
func (w *GlanceWebhook) validate(ctx context.Context, g *Glance, extra field.ErrorList) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Defense-in-depth replicas check alongside the
	// +kubebuilder:validation:Minimum=1 marker.
	if g.Spec.Deployment.Replicas < 1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "replicas"),
			g.Spec.Deployment.Replicas,
			"replicas must be at least 1",
		))
	}

	// Defense-in-depth image tag/digest XOR check alongside the
	// +kubebuilder:validation:XValidation rule on commonv1.ImageSpec: exactly
	// one of tag or digest must be set.
	if (g.Spec.Image.Tag != "") == (g.Spec.Image.Digest != "") {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("image"),
			g.Spec.Image,
			"exactly one of image.tag or image.digest must be set",
		))
	}

	// Defense-in-depth database/cache mutual-exclusivity and
	// Dynamic-requires-clusterRef checks alongside the
	// +kubebuilder:validation:XValidation CEL rules on the shared commonv1
	// types, via the shared validators.
	allErrs = append(allErrs, validation.DatabaseXOR(specPath.Child("database"), &g.Spec.Database)...)
	allErrs = append(allErrs, validation.DynamicCredentialsRequireClusterRef(specPath.Child("database"), &g.Spec.Database)...)
	allErrs = append(allErrs, validation.CacheXOR(specPath.Child("cache"), &g.Spec.Cache)...)
	allErrs = append(allErrs, validation.SecretStoreRef(specPath.Child("secretStoreRef"), g.Spec.SecretStoreRef)...)

	// keystoneEndpoint is required (rendered as [keystone_authtoken] auth_url):
	// empty is Required, otherwise it must parse as an absolute http(s) URL.
	// Glance hands the value verbatim to keystonemiddleware, so an unparseable
	// URL or a missing host would only surface as a token-validation failure at
	// runtime.
	if g.Spec.KeystoneEndpoint == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("keystoneEndpoint"),
			"keystoneEndpoint must be set (the Keystone auth_url the Glance pods reach)",
		))
	} else {
		allErrs = append(allErrs, validateEndpointURL(specPath.Child("keystoneEndpoint"), g.Spec.KeystoneEndpoint)...)
	}
	// keystonePublicEndpoint is optional (falls back to keystoneEndpoint at
	// render time); validate it only when set.
	if g.Spec.KeystonePublicEndpoint != "" {
		allErrs = append(allErrs, validateEndpointURL(specPath.Child("keystonePublicEndpoint"), g.Spec.KeystonePublicEndpoint)...)
	}

	// Defense-in-depth logging validation alongside the CRD enum markers on
	// LoggingSpec.Format / .Level. Map values cannot be expressed as a CRD enum
	// on additionalProperties, so the per-logger level check has no schema-layer
	// counterpart — the webhook is the only enforcement point for that case.
	if g.Spec.Logging != nil {
		loggingPath := specPath.Child("logging")
		validLevels := map[string]struct{}{
			"DEBUG":    {},
			"INFO":     {},
			"WARNING":  {},
			"ERROR":    {},
			"CRITICAL": {},
		}
		if g.Spec.Logging.Format != "" && g.Spec.Logging.Format != "text" && g.Spec.Logging.Format != "json" {
			allErrs = append(allErrs, field.NotSupported(
				loggingPath.Child("format"),
				g.Spec.Logging.Format,
				[]string{"text", "json"},
			))
		}
		if g.Spec.Logging.Level != "" {
			if _, ok := validLevels[g.Spec.Logging.Level]; !ok {
				allErrs = append(allErrs, field.NotSupported(
					loggingPath.Child("level"),
					g.Spec.Logging.Level,
					[]string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"},
				))
			}
		}
		perLoggerPath := loggingPath.Child("perLoggerLevels")
		for name, lvl := range g.Spec.Logging.PerLoggerLevels {
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
	if g.Spec.Deployment.TerminationGracePeriodSeconds != nil && *g.Spec.Deployment.TerminationGracePeriodSeconds < 10 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "terminationGracePeriodSeconds"),
			*g.Spec.Deployment.TerminationGracePeriodSeconds,
			"terminationGracePeriodSeconds must be at least 10",
		))
	}
	// Defense-in-depth range check on spec.deployment.preStopSleepSeconds
	// alongside the +kubebuilder:validation:Minimum=0 marker.
	if g.Spec.Deployment.PreStopSleepSeconds != nil && *g.Spec.Deployment.PreStopSleepSeconds < 0 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "preStopSleepSeconds"),
			*g.Spec.Deployment.PreStopSleepSeconds,
			"preStopSleepSeconds must be non-negative",
		))
	}

	// preStopSleepSeconds must be strictly less than
	// terminationGracePeriodSeconds so there is a non-zero drain window between
	// the end of the preStop sleep and the forced kubelet kill. Resolve nil
	// pointers to the reconciler's effective defaults so the cross-field rule
	// holds even when one or both pointers are omitted.
	resolvedGrace := commonv1.DefaultTerminationGracePeriodSeconds
	if g.Spec.Deployment.TerminationGracePeriodSeconds != nil {
		resolvedGrace = *g.Spec.Deployment.TerminationGracePeriodSeconds
	}
	resolvedPreStop := commonv1.DefaultPreStopSleepSeconds
	if g.Spec.Deployment.PreStopSleepSeconds != nil {
		resolvedPreStop = *g.Spec.Deployment.PreStopSleepSeconds
	}
	if resolvedPreStop >= resolvedGrace {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "preStopSleepSeconds"),
			resolvedPreStop,
			fmt.Sprintf("preStopSleepSeconds (%d) must be strictly less than terminationGracePeriodSeconds (%d)", resolvedPreStop, resolvedGrace),
		))
	}

	// harakiri must be strictly less than the drain window
	// (terminationGracePeriodSeconds - preStopSleepSeconds) so the worst-case
	// uWSGI per-request kill fits inside the envelope between preStop sleep
	// completion and SIGKILL. Only applied when spec.apiServer.uwsgi.harakiri is
	// set, reusing the grace/preStop values already resolved above.
	if g.Spec.APIServer != nil && g.Spec.APIServer.UWSGI != nil && g.Spec.APIServer.UWSGI.Harakiri != nil {
		drain := resolvedGrace - resolvedPreStop
		harakiri := int64(*g.Spec.APIServer.UWSGI.Harakiri)
		if harakiri >= drain {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("apiServer", "uwsgi", "harakiri"),
				*g.Spec.APIServer.UWSGI.Harakiri,
				fmt.Sprintf("harakiri (%d) must be strictly less than terminationGracePeriodSeconds - preStopSleepSeconds (%d)", harakiri, drain),
			))
		}
	}

	// spec.deployment.strategy sanity check — a Recreate strategy must not carry
	// a RollingUpdate block because the Deployment controller would reject the
	// object at apply time.
	if g.Spec.Deployment.Strategy != nil {
		if g.Spec.Deployment.Strategy.Type == appsv1.RecreateDeploymentStrategyType && g.Spec.Deployment.Strategy.RollingUpdate != nil {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("deployment", "strategy", "rollingUpdate"),
				g.Spec.Deployment.Strategy.RollingUpdate,
				"rollingUpdate must not be set when strategy.type is Recreate",
			))
		}
	}

	// Defense-in-depth autoscaling validation alongside kubebuilder markers and
	// CEL rules.
	if g.Spec.Autoscaling != nil {
		autoscalingPath := specPath.Child("autoscaling")
		if g.Spec.Autoscaling.MaxReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				g.Spec.Autoscaling.MaxReplicas,
				"maxReplicas must be at least 1",
			))
		}
		if g.Spec.Autoscaling.MinReplicas != nil && *g.Spec.Autoscaling.MinReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*g.Spec.Autoscaling.MinReplicas,
				"minReplicas must be at least 1",
			))
		}
		if g.Spec.Autoscaling.MinReplicas != nil && *g.Spec.Autoscaling.MinReplicas > g.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*g.Spec.Autoscaling.MinReplicas,
				"minReplicas must not exceed maxReplicas",
			))
		}
		// When minReplicas is unset, the reconciler defaults it to
		// spec.deployment.replicas. Reject configurations where the implicit
		// default would exceed maxReplicas, which would produce an HPA rejected
		// by the API server.
		if g.Spec.Autoscaling.MinReplicas == nil && g.Spec.Deployment.Replicas > g.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				g.Spec.Autoscaling.MaxReplicas,
				fmt.Sprintf("maxReplicas must be >= spec.deployment.replicas (%d) when minReplicas is not set, because minReplicas defaults to spec.deployment.replicas", g.Spec.Deployment.Replicas),
			))
		}
		// Defense-in-depth bounds checks for utilization targets alongside
		// +kubebuilder:validation:Minimum=1 / Maximum=100 markers.
		if g.Spec.Autoscaling.TargetCPUUtilization != nil && (*g.Spec.Autoscaling.TargetCPUUtilization < 1 || *g.Spec.Autoscaling.TargetCPUUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetCPUUtilization"),
				*g.Spec.Autoscaling.TargetCPUUtilization,
				"targetCPUUtilization must be between 1 and 100",
			))
		}
		if g.Spec.Autoscaling.TargetMemoryUtilization != nil && (*g.Spec.Autoscaling.TargetMemoryUtilization < 1 || *g.Spec.Autoscaling.TargetMemoryUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetMemoryUtilization"),
				*g.Spec.Autoscaling.TargetMemoryUtilization,
				"targetMemoryUtilization must be between 1 and 100",
			))
		}
		if g.Spec.Autoscaling.TargetCPUUtilization == nil && g.Spec.Autoscaling.TargetMemoryUtilization == nil {
			allErrs = append(allErrs, field.Required(
				autoscalingPath,
				"at least one of targetCPUUtilization or targetMemoryUtilization must be set",
			))
		}
	}

	// Defense-in-depth networkPolicy ingress check alongside the
	// +kubebuilder:validation:XValidation CEL rule on NetworkPolicySpec.
	if g.Spec.NetworkPolicy != nil && len(g.Spec.NetworkPolicy.Ingress) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("networkPolicy", "ingress"),
			"at least one ingress source must be specified",
		))
	}

	// Defense-in-depth gateway validation alongside the
	// +kubebuilder:validation:MinLength=1 markers on GatewaySpec.Hostname and
	// GatewayParentRefSpec.Name.
	if g.Spec.Gateway != nil {
		gatewayPath := specPath.Child("gateway")
		if g.Spec.Gateway.Hostname == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("hostname"),
				"hostname must be set when spec.gateway is configured",
			))
		}
		if g.Spec.Gateway.ParentRef.Name == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("parentRef", "name"),
				"parentRef.name must be set when spec.gateway is configured",
			))
		}
	}

	// extraConfig sanity: reject an empty section name or an empty option key so
	// the rendered glance-api.conf never carries a nameless [<section>] or a
	// bare "= value" line. extraConfig is a preserve-unknown-fields map, so CEL
	// cannot constrain its keys — the webhook is the sole admission-time gate.
	for section, opts := range g.Spec.ExtraConfig {
		if section == "" {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("extraConfig"),
				section,
				"extraConfig section name must not be empty",
			))
			continue
		}
		for key := range opts {
			if key == "" {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("extraConfig").Key(section),
					key,
					"extraConfig key must not be empty",
				))
			}
		}
	}

	// Reject an extraConfig override of any Rejected owned key. The rejection
	// list is driven by the owned-config-key registry's Rejected flag; for glance
	// it is [keystone_authtoken] password, whose file value is inert at runtime
	// (env-injected via OS_KEYSTONE_AUTHTOKEN__PASSWORD) but would leak the
	// service password into the namespace-readable ConfigMap if rendered.
	// Admission is the only gate before the credential reaches the ConfigMap, so
	// this is a rejection, not a reported-but-honored override.
	for _, e := range OwnedConfigKeys {
		if !e.Rejected {
			continue
		}
		if _, ok := g.Spec.ExtraConfig[e.Section][e.Key]; ok {
			msg := fmt.Sprintf("%s is managed via %s and must not be set in extraConfig", e.Key, e.OwnedBy)
			if e.Impact != "" {
				msg += fmt.Sprintf(" (%s)", e.Impact)
			}
			allErrs = append(allErrs, field.Forbidden(
				specPath.Child("extraConfig").Key(e.Section).Key(e.Key),
				msg,
			))
		}
	}

	// Validate that resource requests do not exceed limits.
	if g.Spec.Deployment.Resources != nil && g.Spec.Deployment.Resources.Limits != nil {
		for resourceName, request := range g.Spec.Deployment.Resources.Requests {
			if limit, hasLimit := g.Spec.Deployment.Resources.Limits[resourceName]; hasLimit && request.Cmp(limit) > 0 {
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
	if g.Spec.Deployment.PriorityClassName != nil {
		allErrs = append(allErrs, validation.PriorityClassExists(ctx, w.Client,
			specPath.Child("deployment", "priorityClassName"), *g.Spec.Deployment.PriorityClassName)...)
	}

	// Validate that custom TopologySpreadConstraints use the correct
	// LabelSelector matching the Deployment's selector labels.
	if g.Spec.Deployment.TopologySpreadConstraints != nil {
		allErrs = append(allErrs, validation.TopologySpreadSelector(
			specPath.Child("deployment", "topologySpreadConstraints"),
			g.Spec.Deployment.TopologySpreadConstraints,
			map[string]string{
				naming.LabelKeyName:     "glance",
				naming.LabelKeyInstance: g.Name,
			},
		)...)
	}

	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Glance"},
			g.Name,
			allErrs,
		)
	}
	return nil
}

// validateEndpointURL checks that a non-empty endpoint parses cleanly, uses an
// http(s) scheme, and carries a host — the same shape horizon enforces on its
// keystoneEndpoint. It is applied to both Glance Keystone endpoint fields.
func validateEndpointURL(fldPath *field.Path, endpoint string) field.ErrorList {
	var errs field.ErrorList
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

// warnInertLaunchModeKnobs flags an apiServer knob that has no effect under the
// active launch mode. Glance runs the eventlet glance-api server below release
// 2026.1 — where spec.apiServer.uwsgi is inert — and switches to uWSGI from
// 2026.1, where spec.apiServer.workers is inert. Both knobs are legal in either
// mode (the operator simply ignores the inert one), so this is a warning, not a
// rejection. An unparseable release is left to the CRD pattern / release
// tracking, so the warning is skipped rather than guessing a mode.
func warnInertLaunchModeKnobs(g *Glance) admission.Warnings {
	if g.Spec.APIServer == nil {
		return nil
	}
	rel, err := release.ParseRelease(g.Spec.OpenStackRelease)
	if err != nil {
		return nil
	}
	uwsgiMode := rel.Year > 2026 || (rel.Year == 2026 && rel.Minor >= 1)
	var warnings admission.Warnings
	if g.Spec.APIServer.UWSGI != nil && !uwsgiMode {
		warnings = append(warnings, fmt.Sprintf(
			"spec.apiServer.uwsgi is set but release %q runs the eventlet glance-api server (below 2026.1), "+
				"so the uWSGI knobs are inert; configure spec.apiServer.workers instead.",
			g.Spec.OpenStackRelease,
		))
	}
	if g.Spec.APIServer.Workers != nil && uwsgiMode {
		warnings = append(warnings, fmt.Sprintf(
			"spec.apiServer.workers is set but release %q runs under uWSGI (2026.1 or later), "+
				"so the eventlet worker count is inert; configure spec.apiServer.uwsgi instead.",
			g.Spec.OpenStackRelease,
		))
	}
	return warnings
}

// validateExtraConfigOptions validates the option names in spec.extraConfig
// against the option catalog embedded for the release named by
// spec.openStackRelease.
//
// It fails open: when spec.openStackRelease cannot be resolved to an embedded
// catalog it returns exactly one warning and no errors, so a value that does not
// name a release or a build that ships no catalog for the release never blocks
// admission. Sections a loaded plugin declares (spec.plugins[].configSection)
// and the reserved glance_store sections in CatalogExemptSections are skipped
// whole, and the operator-owned keys in OwnedConfigKeys are exempt individually,
// so neither a plugin's dynamically named options, a file-store option under a
// reserved store, nor an operator-owned override is mistaken for an unknown
// option. Every remaining unknown option or unknown section becomes a field
// error; every deprecated-but-accepted option becomes a warning naming its
// replacement.
func validateExtraConfigOptions(specPath *field.Path, g *Glance) (admission.Warnings, field.ErrorList) {
	if len(g.Spec.ExtraConfig) == 0 {
		return nil, nil
	}

	catalog, ok := OptionCatalogForRelease(g.Spec.OpenStackRelease)
	if !ok {
		// Fail open with exactly one warning. Distinguish a value that does not
		// name a release at all from a parseable release the operator ships no
		// catalog for.
		rel, err := release.ParseRelease(g.Spec.OpenStackRelease)
		if err != nil {
			return admission.Warnings{
				"spec.extraConfig was not validated against an option catalog: " +
					"spec.openStackRelease does not name an OpenStack release",
			}, nil
		}
		return admission.Warnings{
			fmt.Sprintf("spec.extraConfig was not validated against an option catalog: "+
				"no catalog for release %q is embedded in this operator build",
				fmt.Sprintf("%d.%d", rel.Year, rel.Minor)),
		}, nil
	}

	// Exempt the sections a loaded plugin owns and the reserved glance_store
	// sections the catalog cannot enumerate (their option set is not derivable
	// from the release catalog), plus the operator-owned keys, so none is flagged
	// as unknown.
	exemptSections := make(map[string]struct{}, len(g.Spec.Plugins)+len(CatalogExemptSections))
	for _, p := range g.Spec.Plugins {
		exemptSections[p.ConfigSection] = struct{}{}
	}
	for _, section := range CatalogExemptSections {
		exemptSections[section] = struct{}{}
	}
	ex := config.CatalogExemptions{
		Sections: exemptSections,
		Keys:     config.KeyExemptionsFromRegistry(OwnedConfigKeys),
	}

	base := catalog.Release
	unknown, deprecated := catalog.FindUnknownOptions(g.Spec.ExtraConfig, ex)

	extraConfigPath := specPath.Child("extraConfig")
	var errs field.ErrorList
	for _, u := range unknown {
		fldPath := extraConfigPath.Key(u.Section).Key(u.Key)
		if u.SectionUnknown {
			errs = append(errs, field.Invalid(fldPath, u.Key,
				fmt.Sprintf("no such section in the glance %s option catalog "+
					"(sections registered by a loaded plugin must be declared via spec.plugins)", base)))
			continue
		}
		errs = append(errs, field.Invalid(fldPath, u.Key,
			fmt.Sprintf("no such option in the glance %s option catalog", base)))
	}

	var warnings admission.Warnings
	for _, d := range deprecated {
		if d.Replacement == "" {
			warnings = append(warnings, fmt.Sprintf(
				"spec.extraConfig [%s] %s: deprecated option in glance %s with no replacement",
				d.Section, d.Key, base,
			))
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"spec.extraConfig [%s] %s: deprecated option in glance %s, replaced by %s",
			d.Section, d.Key, base, d.Replacement,
		))
	}
	return warnings, errs
}

// extraConfigCatalogInputsChanged reports whether anything the extraConfig
// option-catalog check depends on differs between oldObj and newObj: the
// extraConfig map itself, spec.openStackRelease that selects the release
// catalog, or the set of plugin config sections (compared order-insensitively,
// since it only feeds an exemption set). ValidateUpdate gates the catalog
// re-validation on this so a CR whose extraConfig went stale-invalid is not
// rejected by an otherwise-unrelated update.
func extraConfigCatalogInputsChanged(oldObj, newObj *Glance) bool {
	if !reflect.DeepEqual(oldObj.Spec.ExtraConfig, newObj.Spec.ExtraConfig) {
		return true
	}
	if oldObj.Spec.OpenStackRelease != newObj.Spec.OpenStackRelease {
		return true
	}
	if len(oldObj.Spec.Plugins) != len(newObj.Spec.Plugins) {
		return true
	}
	oldSections := make([]string, len(oldObj.Spec.Plugins))
	for i, p := range oldObj.Spec.Plugins {
		oldSections[i] = p.ConfigSection
	}
	newSections := make([]string, len(newObj.Spec.Plugins))
	for i, p := range newObj.Spec.Plugins {
		newSections[i] = p.ConfigSection
	}
	sort.Strings(oldSections)
	sort.Strings(newSections)
	for i := range oldSections {
		if oldSections[i] != newSections[i] {
			return true
		}
	}
	return false
}
