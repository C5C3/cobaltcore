// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/naming"
	"github.com/c5c3/forge/internal/common/release"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/validation"
	commonwebhook "github.com/c5c3/forge/internal/common/webhook"
)

// Placement-specific container memory defaults, replacing the shared 256Mi/512Mi
// baseline. The API container runs commonv1.DefaultUWSGIProcesses preforked
// workers, each carrying its own interpreter, SQLAlchemy session pool and oslo
// stack, so the container footprint is a multiple of a single worker's rather
// than one interpreter's. 512Mi request / 1Gi limit keeps that multiple inside
// the limit and matches the budget the sibling glance-api container gets. CPU
// keeps the shared defaults.
var (
	defaultPlacementMemoryRequest = resource.MustParse("512Mi")
	defaultPlacementMemoryLimit   = resource.MustParse("1Gi")
)

// PlacementWebhook implements defaulting and validation webhooks for the
// Placement CRD. Client is injected at startup for cluster-scoped resource
// lookups (e.g. PriorityClass validation). Production wiring injects
// mgr.GetAPIReader() — a direct, uncached reader — so admission never rejects a
// just-created object from a stale informer cache and no lazy informer start
// happens inside the webhook timeout.
// +kubebuilder:object:generate=false
type PlacementWebhook struct {
	commonwebhook.NoopDeleteValidator[*Placement]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*Placement] = &PlacementWebhook{}
	_ admission.Validator[*Placement] = &PlacementWebhook{}
)

// +kubebuilder:webhook:path=/mutate-placement-openstack-c5c3-io-v1alpha1-placement,mutating=true,failurePolicy=fail,sideEffects=None,groups=placement.openstack.c5c3.io,resources=placements,verbs=create;update,versions=v1alpha1,name=mplacement.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-placement-openstack-c5c3-io-v1alpha1-placement,mutating=false,failurePolicy=fail,sideEffects=None,groups=placement.openstack.c5c3.io,resources=placements,verbs=create;update,versions=v1alpha1,name=vplacement.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *PlacementWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*Placement](mgr, &Placement{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*Placement]. It sets spec fields to
// their documented defaults when they carry zero values, following the keystone/
// horizon non-mutating discipline: optional pointer blocks are only partially
// filled when explicitly present, except spec.logging which is materialized so
// downstream reconciler code never sees a nil pointer.
func (w *PlacementWebhook) Default(_ context.Context, obj *Placement) error {
	// Fill spec.deployment.resources with the placement-specific memory defaults
	// before the shared DeploymentSpec defaults run — Deployment.Default() would
	// otherwise inject the shared 256Mi/512Mi baseline. Same nil-or-empty
	// condition as the shared method so an explicit user value is never
	// clobbered.
	if obj.Spec.Deployment.Resources == nil ||
		(len(obj.Spec.Deployment.Resources.Requests) == 0 && len(obj.Spec.Deployment.Resources.Limits) == 0) {
		obj.Spec.Deployment.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: defaultPlacementMemoryRequest.DeepCopy(),
				corev1.ResourceCPU:    commonv1.DefaultCPURequest(),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: defaultPlacementMemoryLimit.DeepCopy(),
				corev1.ResourceCPU:    commonv1.DefaultCPULimit(),
			},
		}
	}
	// Shared-type defaults (replicas, remaining container resources) are applied
	// by the commonv1.DeploymentSpec Default method so they cannot drift across
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
		su.Username = "placement"
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
	// non-nil. The leaf defaults are applied by the commonv1.UWSGISpec Default
	// method so they cannot drift across operators; a nil pointer is a no-op
	// there — the reconciler uses the same constants as its hardcoded defaults.
	if obj.Spec.APIServer != nil {
		obj.Spec.APIServer.UWSGI.Default()
	}
	return nil
}

// ValidateCreate implements admission.Validator[*Placement].
//
// Unlike the sibling glance webhook there is no metadata.name length bound here.
// Glance needs one because its db-purge CronJob leaves only 52 characters for the
// CR name; placement renders no CronJob, so every child object name fits within
// the 63 characters Kubernetes already allows for the CR name itself and any
// admissible name stays admissible.
func (w *PlacementWebhook) ValidateCreate(ctx context.Context, obj *Placement) (admission.Warnings, error) {
	warnings, createErrs := validateExtraConfigOptions(field.NewPath("spec"), obj)
	return warnings, w.validate(ctx, obj, createErrs)
}

// ValidateUpdate implements admission.Validator[*Placement].
//
// The extraConfig option-catalog check is re-run only when one of its inputs
// changed (extraConfig or spec.openStackRelease). This keeps an unrelated update
// (scaling replicas, say) from retroactively rejecting a CR whose extraConfig was
// accepted at create time but has since been invalidated by a regenerated
// catalog.
//
// spec.targetClusterRef is compared across both revisions here, the webhook-layer
// twin of the two transition CEL rules on PlacementSpec.
func (w *PlacementWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *Placement) (admission.Warnings, error) {
	var warnings admission.Warnings
	var updateErrs field.ErrorList
	if extraConfigCatalogInputsChanged(oldObj, newObj) {
		warnings, updateErrs = validateExtraConfigOptions(field.NewPath("spec"), newObj)
	}
	updateErrs = append(updateErrs, validation.TargetClusterRefImmutable(
		field.NewPath("spec", "targetClusterRef"),
		oldObj.Spec.TargetClusterRef,
		newObj.Spec.TargetClusterRef,
	)...)
	return warnings, w.validate(ctx, newObj, updateErrs)
}

// validate runs all validation rules against the Placement spec, accumulating
// every violation so users see the full list in one admission response.
// ctx is required for cluster-scoped lookups (PriorityClass validation).
// extra carries the errors accumulated by the caller (the extraConfig
// option-catalog check, and on update the targetClusterRef immutability check)
// so they aggregate into the single Invalid error alongside the rest.
func (w *PlacementWebhook) validate(ctx context.Context, p *Placement, extra field.ErrorList) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Defense-in-depth replicas check alongside the
	// +kubebuilder:validation:Minimum=1 marker.
	if p.Spec.Deployment.Replicas < 1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "replicas"),
			p.Spec.Deployment.Replicas,
			"replicas must be at least 1",
		))
	}

	// Defense-in-depth image tag/digest XOR check alongside the
	// +kubebuilder:validation:XValidation rule on commonv1.ImageSpec: exactly one
	// of tag or digest must be set.
	if (p.Spec.Image.Tag != "") == (p.Spec.Image.Digest != "") {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("image"),
			p.Spec.Image,
			"exactly one of image.tag or image.digest must be set",
		))
	}

	// Defense-in-depth database/cache mutual-exclusivity and
	// Dynamic-requires-clusterRef checks alongside the
	// +kubebuilder:validation:XValidation CEL rules on the shared commonv1 types,
	// via the shared validators.
	allErrs = append(allErrs, validation.DatabaseXOR(specPath.Child("database"), &p.Spec.Database)...)
	allErrs = append(allErrs, validation.DynamicCredentialsRequireClusterRef(specPath.Child("database"), &p.Spec.Database)...)
	allErrs = append(allErrs, validation.CacheXOR(specPath.Child("cache"), &p.Spec.Cache)...)
	// spec.cache reaches the verbatim INI renderer the same way the typed fields
	// below do: cache.ResolveServers derives [keystone_authtoken].memcached_servers
	// from spec.cache.servers or spec.cache.clusterRef.name. This is the
	// defense-in-depth twin of the items pattern and XValidation rule the shared
	// commonv1.CacheSpec carries, for objects that bypass schema validation.
	allErrs = append(allErrs, validation.CacheNoControlChars(specPath.Child("cache"), &p.Spec.Cache)...)
	allErrs = append(allErrs, validation.SecretStoreRef(specPath.Child("secretStoreRef"), p.Spec.SecretStoreRef)...)
	allErrs = append(allErrs, validation.TargetClusterRef(specPath.Child("targetClusterRef"), p.Spec.TargetClusterRef)...)

	// keystoneEndpoint is required (rendered as [keystone_authtoken] auth_url):
	// empty is Required, otherwise it must parse as an absolute http(s) URL.
	// Placement hands the value verbatim to keystonemiddleware, so an unparseable
	// URL or a missing host would only surface as a token-validation failure at
	// runtime.
	if p.Spec.KeystoneEndpoint == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("keystoneEndpoint"),
			"keystoneEndpoint must be set (the Keystone auth_url the Placement pods reach)",
		))
	} else {
		allErrs = append(allErrs, validateEndpointURL(specPath.Child("keystoneEndpoint"), p.Spec.KeystoneEndpoint)...)
	}
	// keystonePublicEndpoint is optional (falls back to keystoneEndpoint at
	// render time); validate it only when set.
	if p.Spec.KeystonePublicEndpoint != "" {
		allErrs = append(allErrs, validateEndpointURL(specPath.Child("keystonePublicEndpoint"), p.Spec.KeystonePublicEndpoint)...)
	}

	// Typed spec fields reach the same verbatim INI renderer as extraConfig: each
	// value below is rendered into [keystone_authtoken] as "%s = %s". A newline or
	// carriage return therefore injects an additional config line — smuggling a
	// whole key past the (section, key)-keyed ownership (FindOwnedOverrides) and
	// catalog (FindUnknownOptions) gates, exactly the bypass the extraConfig check
	// below blocks. The two Keystone endpoint fields need no entry here: they go
	// through validateEndpointURL, whose url.Parse rejects control bytes; spec.cache
	// feeds the same section and is covered by CacheNoControlChars above.
	for _, f := range []struct {
		path  *field.Path
		value string
	}{
		{specPath.Child("region"), p.Spec.Region},
		{specPath.Child("serviceUser", "username"), p.Spec.ServiceUser.Username},
		{specPath.Child("serviceUser", "projectName"), p.Spec.ServiceUser.ProjectName},
		{specPath.Child("serviceUser", "userDomainName"), p.Spec.ServiceUser.UserDomainName},
		{specPath.Child("serviceUser", "projectDomainName"), p.Spec.ServiceUser.ProjectDomainName},
	} {
		if validation.HasControlChars(f.value) {
			allErrs = append(allErrs, field.Invalid(f.path, f.value,
				"value must not contain a newline or carriage return: it is rendered verbatim into "+
					"placement.conf, so a newline injects arbitrary config lines"))
		}
	}

	// Defense-in-depth logging validation alongside the CRD enum markers on
	// LoggingSpec.Format / .Level. Map values cannot be expressed as a CRD enum on
	// additionalProperties, so the per-logger level check has no schema-layer
	// counterpart — the webhook is the only enforcement point for that case.
	if p.Spec.Logging != nil {
		loggingPath := specPath.Child("logging")
		validLevels := map[string]struct{}{
			"DEBUG":    {},
			"INFO":     {},
			"WARNING":  {},
			"ERROR":    {},
			"CRITICAL": {},
		}
		if p.Spec.Logging.Format != "" && p.Spec.Logging.Format != "text" && p.Spec.Logging.Format != "json" {
			allErrs = append(allErrs, field.NotSupported(
				loggingPath.Child("format"),
				p.Spec.Logging.Format,
				[]string{"text", "json"},
			))
		}
		if p.Spec.Logging.Level != "" {
			if _, ok := validLevels[p.Spec.Logging.Level]; !ok {
				allErrs = append(allErrs, field.NotSupported(
					loggingPath.Child("level"),
					p.Spec.Logging.Level,
					[]string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"},
				))
			}
		}
		perLoggerPath := loggingPath.Child("perLoggerLevels")
		for name, lvl := range p.Spec.Logging.PerLoggerLevels {
			if name == "" {
				allErrs = append(allErrs, field.Invalid(
					perLoggerPath,
					name,
					"logger name must not be empty",
				))
				continue
			}
			// The name renders into the [DEFAULT] default_log_levels CSV
			// (RenderSortedPairs), which the INI renderer writes verbatim, so a
			// newline in it injects arbitrary config lines the same way an
			// extraConfig key would.
			if validation.HasControlChars(name) {
				allErrs = append(allErrs, field.Invalid(
					perLoggerPath,
					name,
					"logger name must not contain a newline or carriage return: it is rendered "+
						"verbatim into placement.conf, so a newline injects arbitrary config lines",
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
	if p.Spec.Deployment.TerminationGracePeriodSeconds != nil && *p.Spec.Deployment.TerminationGracePeriodSeconds < 10 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "terminationGracePeriodSeconds"),
			*p.Spec.Deployment.TerminationGracePeriodSeconds,
			"terminationGracePeriodSeconds must be at least 10",
		))
	}
	// Defense-in-depth range check on spec.deployment.preStopSleepSeconds
	// alongside the +kubebuilder:validation:Minimum=0 marker.
	if p.Spec.Deployment.PreStopSleepSeconds != nil && *p.Spec.Deployment.PreStopSleepSeconds < 0 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "preStopSleepSeconds"),
			*p.Spec.Deployment.PreStopSleepSeconds,
			"preStopSleepSeconds must be non-negative",
		))
	}

	// preStopSleepSeconds must be strictly less than
	// terminationGracePeriodSeconds so there is a non-zero drain window between
	// the end of the preStop sleep and the forced kubelet kill. Resolve nil
	// pointers to the reconciler's effective defaults so the cross-field rule
	// holds even when one or both pointers are omitted.
	resolvedGrace := commonv1.DefaultTerminationGracePeriodSeconds
	if p.Spec.Deployment.TerminationGracePeriodSeconds != nil {
		resolvedGrace = *p.Spec.Deployment.TerminationGracePeriodSeconds
	}
	resolvedPreStop := commonv1.DefaultPreStopSleepSeconds
	if p.Spec.Deployment.PreStopSleepSeconds != nil {
		resolvedPreStop = *p.Spec.Deployment.PreStopSleepSeconds
	}
	if resolvedPreStop >= resolvedGrace {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "preStopSleepSeconds"),
			resolvedPreStop,
			fmt.Sprintf("preStopSleepSeconds (%d) must be strictly less than terminationGracePeriodSeconds (%d)", resolvedPreStop, resolvedGrace),
		))
	}

	if p.Spec.APIServer != nil && p.Spec.APIServer.UWSGI != nil {
		uwsgiPath := specPath.Child("apiServer", "uwsgi")
		u := p.Spec.APIServer.UWSGI
		// harakiri must be strictly less than the drain window
		// (terminationGracePeriodSeconds - preStopSleepSeconds) so the worst-case
		// uWSGI per-request kill fits inside the envelope between preStop sleep
		// completion and SIGKILL. Only applied when harakiri is set, reusing the
		// grace/preStop values already resolved above.
		if u.Harakiri != nil {
			drain := resolvedGrace - resolvedPreStop
			harakiri := int64(*u.Harakiri)
			if harakiri >= drain {
				allErrs = append(allErrs, field.Invalid(
					uwsgiPath.Child("harakiri"),
					*u.Harakiri,
					fmt.Sprintf("harakiri (%d) must be strictly less than terminationGracePeriodSeconds - preStopSleepSeconds (%d)", harakiri, drain),
				))
			}
		}
		// httpKeepAliveTimeout is only meaningful when httpKeepAlive is true —
		// otherwise the --http-keepalive-timeout flag is never emitted. A nil
		// HTTPKeepAlive pointer means "unset", which resolves to the default true,
		// so only an EXPLICIT false conflicts with a set timeout. This mirrors the
		// XValidation rule on UWSGISpec.
		if u.HTTPKeepAliveTimeout != nil && u.HTTPKeepAlive != nil && !*u.HTTPKeepAlive {
			allErrs = append(allErrs, field.Invalid(
				uwsgiPath.Child("httpKeepAliveTimeout"),
				*u.HTTPKeepAliveTimeout,
				"httpKeepAliveTimeout may only be set when httpKeepAlive is true",
			))
		}
	}

	// spec.deployment.strategy sanity check — a Recreate strategy must not carry
	// a RollingUpdate block because the Deployment controller would reject the
	// object at apply time.
	if p.Spec.Deployment.Strategy != nil {
		if p.Spec.Deployment.Strategy.Type == appsv1.RecreateDeploymentStrategyType && p.Spec.Deployment.Strategy.RollingUpdate != nil {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("deployment", "strategy", "rollingUpdate"),
				p.Spec.Deployment.Strategy.RollingUpdate,
				"rollingUpdate must not be set when strategy.type is Recreate",
			))
		}
	}

	// Defense-in-depth autoscaling validation alongside kubebuilder markers and
	// CEL rules.
	if p.Spec.Autoscaling != nil {
		autoscalingPath := specPath.Child("autoscaling")
		if p.Spec.Autoscaling.MaxReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				p.Spec.Autoscaling.MaxReplicas,
				"maxReplicas must be at least 1",
			))
		}
		if p.Spec.Autoscaling.MinReplicas != nil && *p.Spec.Autoscaling.MinReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*p.Spec.Autoscaling.MinReplicas,
				"minReplicas must be at least 1",
			))
		}
		if p.Spec.Autoscaling.MinReplicas != nil && *p.Spec.Autoscaling.MinReplicas > p.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*p.Spec.Autoscaling.MinReplicas,
				"minReplicas must not exceed maxReplicas",
			))
		}
		// When minReplicas is unset, the reconciler defaults it to
		// spec.deployment.replicas. Reject configurations where the implicit
		// default would exceed maxReplicas, which would produce an HPA rejected by
		// the API server.
		if p.Spec.Autoscaling.MinReplicas == nil && p.Spec.Deployment.Replicas > p.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				p.Spec.Autoscaling.MaxReplicas,
				fmt.Sprintf("maxReplicas must be >= spec.deployment.replicas (%d) when minReplicas is not set, because minReplicas defaults to spec.deployment.replicas", p.Spec.Deployment.Replicas),
			))
		}
		// Defense-in-depth bounds checks for utilization targets alongside
		// +kubebuilder:validation:Minimum=1 / Maximum=100 markers.
		if p.Spec.Autoscaling.TargetCPUUtilization != nil && (*p.Spec.Autoscaling.TargetCPUUtilization < 1 || *p.Spec.Autoscaling.TargetCPUUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetCPUUtilization"),
				*p.Spec.Autoscaling.TargetCPUUtilization,
				"targetCPUUtilization must be between 1 and 100",
			))
		}
		if p.Spec.Autoscaling.TargetMemoryUtilization != nil && (*p.Spec.Autoscaling.TargetMemoryUtilization < 1 || *p.Spec.Autoscaling.TargetMemoryUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetMemoryUtilization"),
				*p.Spec.Autoscaling.TargetMemoryUtilization,
				"targetMemoryUtilization must be between 1 and 100",
			))
		}
		if p.Spec.Autoscaling.TargetCPUUtilization == nil && p.Spec.Autoscaling.TargetMemoryUtilization == nil {
			allErrs = append(allErrs, field.Required(
				autoscalingPath,
				"at least one of targetCPUUtilization or targetMemoryUtilization must be set",
			))
		}
	}

	// Defense-in-depth networkPolicy ingress check alongside the
	// +kubebuilder:validation:XValidation CEL rule on NetworkPolicySpec.
	if p.Spec.NetworkPolicy != nil && len(p.Spec.NetworkPolicy.Ingress) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("networkPolicy", "ingress"),
			"at least one ingress source must be specified",
		))
	}

	// Defense-in-depth gateway validation alongside the
	// +kubebuilder:validation:MinLength=1 markers on GatewaySpec.Hostname and
	// GatewayParentRefSpec.Name.
	if p.Spec.Gateway != nil {
		gatewayPath := specPath.Child("gateway")
		if p.Spec.Gateway.Hostname == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("hostname"),
				"hostname must be set when spec.gateway is configured",
			))
		}
		if p.Spec.Gateway.ParentRef.Name == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("parentRef", "name"),
				"parentRef.name must be set when spec.gateway is configured",
			))
		}
	}

	// extraConfig sanity: reject an empty section name or an empty option key so
	// the rendered placement.conf never carries a nameless [<section>] or a bare
	// "= value" line. extraConfig is a preserve-unknown-fields map, so CEL cannot
	// constrain its keys — the webhook is the sole admission-time gate.
	//
	// It also rejects a newline or carriage return in a section name, a key, OR a
	// value. The INI renderer writes each verbatim ("[%s]" for the section,
	// "%s = %s" per option), so such a character injects arbitrary additional
	// config lines — smuggling a whole [section]/key past the (section,
	// key)-name-keyed ownership (FindOwnedOverrides) and catalog
	// (FindUnknownOptions) gates, which inspect map structure only and never look
	// inside a value.
	for section, opts := range p.Spec.ExtraConfig {
		if section == "" {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("extraConfig"),
				section,
				"extraConfig section name must not be empty",
			))
			continue
		}
		if validation.HasControlChars(section) {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("extraConfig"),
				section,
				"extraConfig section name must not contain a newline or carriage return",
			))
			continue
		}
		for key, value := range opts {
			if key == "" {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("extraConfig").Key(section),
					key,
					"extraConfig key must not be empty",
				))
				continue
			}
			if validation.HasControlChars(key) || validation.HasControlChars(value) {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("extraConfig").Key(section).Key(key),
					key,
					"extraConfig key and value must not contain a newline or carriage return: "+
						"the rendered INI writes them verbatim, so a newline injects arbitrary config lines",
				))
			}
		}
	}

	// Reject an extraConfig override of any Rejected owned key. The rejection list
	// is driven by the owned-config-key registry's Rejected flag; for placement it
	// is [keystone_authtoken] password, whose file value is inert at runtime
	// (env-injected via OS_KEYSTONE_AUTHTOKEN__PASSWORD) but would leak the
	// service password into the namespace-readable ConfigMap if rendered.
	// Admission is the only gate before the credential reaches the ConfigMap, so
	// this is a rejection, not a reported-but-honored override.
	for _, e := range OwnedConfigKeys {
		if !e.Rejected {
			continue
		}
		if _, ok := p.Spec.ExtraConfig[e.Section][e.Key]; ok {
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
	if p.Spec.Deployment.Resources != nil && p.Spec.Deployment.Resources.Limits != nil {
		for resourceName, request := range p.Spec.Deployment.Resources.Requests {
			if limit, hasLimit := p.Spec.Deployment.Resources.Limits[resourceName]; hasLimit && request.Cmp(limit) > 0 {
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
	if p.Spec.Deployment.PriorityClassName != nil {
		allErrs = append(allErrs, validation.PriorityClassExists(ctx, w.Client,
			specPath.Child("deployment", "priorityClassName"), *p.Spec.Deployment.PriorityClassName)...)
	}

	// Validate that custom TopologySpreadConstraints use the correct
	// LabelSelector matching the Deployment's selector labels.
	if p.Spec.Deployment.TopologySpreadConstraints != nil {
		allErrs = append(allErrs, validation.TopologySpreadSelector(
			specPath.Child("deployment", "topologySpreadConstraints"),
			p.Spec.Deployment.TopologySpreadConstraints,
			map[string]string{
				naming.LabelKeyName:     "placement",
				naming.LabelKeyInstance: p.Name,
			},
		)...)
	}

	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Placement"},
			p.Name,
			allErrs,
		)
	}
	return nil
}

// validateEndpointURL checks that a non-empty endpoint parses cleanly, uses an
// http(s) scheme, and carries a host — the same shape the sibling operators
// enforce on their Keystone endpoints. It is applied to both Placement Keystone
// endpoint fields.
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

// validateExtraConfigOptions validates the option names in spec.extraConfig
// against the option catalog embedded for the release named by
// spec.openStackRelease.
//
// It fails open: when spec.openStackRelease cannot be resolved to an embedded
// catalog it returns exactly one warning and no errors, so a value that does not
// name a release or a build that ships no catalog for the release never blocks
// admission. The operator-owned keys in OwnedConfigKeys are exempt individually
// so an operator-owned override is not mistaken for an unknown option; placement
// exempts no section whole (see OptionCatalogForRelease). Every remaining unknown
// option or unknown section becomes a field error; every deprecated-but-accepted
// option becomes a warning naming its replacement.
func validateExtraConfigOptions(specPath *field.Path, p *Placement) (admission.Warnings, field.ErrorList) {
	if len(p.Spec.ExtraConfig) == 0 {
		return nil, nil
	}

	catalog, ok := OptionCatalogForRelease(p.Spec.OpenStackRelease)
	if !ok {
		// Fail open with exactly one warning. Distinguish a value that does not
		// name a release at all from a parseable release the operator ships no
		// catalog for.
		rel, err := release.ParseRelease(p.Spec.OpenStackRelease)
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

	ex := config.CatalogExemptions{Keys: config.KeyExemptionsFromRegistry(OwnedConfigKeys)}

	base := catalog.Release
	unknown, deprecated := catalog.FindUnknownOptions(p.Spec.ExtraConfig, ex)

	extraConfigPath := specPath.Child("extraConfig")
	var errs field.ErrorList
	for _, u := range unknown {
		fldPath := extraConfigPath.Key(u.Section).Key(u.Key)
		if u.SectionUnknown {
			errs = append(errs, field.Invalid(fldPath, u.Key,
				fmt.Sprintf("no such section in the placement %s option catalog", base)))
			continue
		}
		errs = append(errs, field.Invalid(fldPath, u.Key,
			fmt.Sprintf("no such option in the placement %s option catalog", base)))
	}

	var warnings admission.Warnings
	for _, d := range deprecated {
		if d.Replacement == "" {
			warnings = append(warnings, fmt.Sprintf(
				"spec.extraConfig [%s] %s: deprecated option in placement %s with no replacement",
				d.Section, d.Key, base,
			))
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"spec.extraConfig [%s] %s: deprecated option in placement %s, replaced by %s",
			d.Section, d.Key, base, d.Replacement,
		))
	}
	return warnings, errs
}

// extraConfigCatalogInputsChanged reports whether anything the extraConfig
// option-catalog check depends on differs between oldObj and newObj: the
// extraConfig map itself, or spec.openStackRelease that selects the release
// catalog. ValidateUpdate gates the catalog re-validation on this so a CR whose
// extraConfig went stale-invalid is not rejected by an otherwise-unrelated
// update.
func extraConfigCatalogInputsChanged(oldObj, newObj *Placement) bool {
	if oldObj.Spec.OpenStackRelease != newObj.Spec.OpenStackRelease {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec.ExtraConfig, newObj.Spec.ExtraConfig)
}
