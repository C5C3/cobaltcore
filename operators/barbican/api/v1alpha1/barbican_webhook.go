// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/naming"
	"github.com/c5c3/forge/internal/common/policy"
	"github.com/c5c3/forge/internal/common/release"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/validation"
	commonwebhook "github.com/c5c3/forge/internal/common/webhook"
)

// DefaultDBCleanRetentionDays is how long a soft-deleted row survives before the
// clean-up hard-deletes it, resolved when spec.dbClean.retentionDays is unset. It
// is barbican's own --min-days default.
//
// It is consumed by the reconcile-time resolver and deliberately NOT applied by
// the defaulting webhook: a nil or partial spec.dbClean block keeps tracking this
// operator default across upgrades instead of freezing today's value into the
// stored CR. The validating webhook reads it to tell a retention reduction from
// an edit that leaves the window alone.
const DefaultDBCleanRetentionDays int32 = 90

// Name-length bounds enforced on metadata.name, driven by the db-clean CronJob —
// the child object with the tightest name budget.
const (
	// MaxCronJobNameLength is the API server's own cap on a CronJob name:
	// DNS1035LabelMaxLength (63) minus the 11-character "-<timestamp>" suffix its
	// controller appends to every Job it spawns.
	MaxCronJobNameLength = 52
	// dbCleanNameSuffix is appended to metadata.name to name the clean-up
	// CronJob. It is duplicated from the controller package (which builds the
	// object) because the api package cannot import the controller.
	dbCleanNameSuffix = "-db-clean"
	// MaxBarbicanNameLength is what those two leave for metadata.name.
	MaxBarbicanNameLength = MaxCronJobNameLength - len(dbCleanNameSuffix)
)

// BarbicanWebhook implements defaulting and validation webhooks for the Barbican
// CRD. Client is injected at startup for cluster-scoped resource lookups (e.g.
// PriorityClass validation). Production wiring injects mgr.GetAPIReader() — a
// direct, uncached reader — so admission never rejects a just-created object from
// a stale informer cache and no lazy informer start happens inside the webhook
// timeout.
// +kubebuilder:object:generate=false
type BarbicanWebhook struct {
	commonwebhook.NoopDeleteValidator[*Barbican]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*Barbican] = &BarbicanWebhook{}
	_ admission.Validator[*Barbican] = &BarbicanWebhook{}
)

// +kubebuilder:webhook:path=/mutate-barbican-openstack-c5c3-io-v1alpha1-barbican,mutating=true,failurePolicy=fail,sideEffects=None,groups=barbican.openstack.c5c3.io,resources=barbicans,verbs=create;update,versions=v1alpha1,name=mbarbican.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-barbican-openstack-c5c3-io-v1alpha1-barbican,mutating=false,failurePolicy=fail,sideEffects=None,groups=barbican.openstack.c5c3.io,resources=barbicans,verbs=create;update,versions=v1alpha1,name=vbarbican.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *BarbicanWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*Barbican](mgr, &Barbican{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*Barbican]. It sets spec fields to their
// documented defaults when they carry zero values, following the keystone/horizon
// non-mutating discipline: optional pointer blocks are only partially filled when
// explicitly present, except spec.logging which is materialized so downstream
// reconciler code never sees a nil pointer.
func (w *BarbicanWebhook) Default(_ context.Context, obj *Barbican) error {
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
	// value is never clobbered. A minimal CR need only supply the password Secret
	// reference.
	su := &obj.Spec.ServiceUser
	if su.Username == "" {
		su.Username = "barbican"
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

// ValidateCreate implements admission.Validator[*Barbican].
//
// The metadata.name bound is enforced here rather than in validate(), which
// update shares: the name is immutable, so on update the rule could only ever
// fire against an object a pre-upgrade operator already admitted — and the
// validating webhook also sees the finalizer-removal update reconcileDelete
// issues, so rejecting it would wedge that CR in Terminating with no field left
// to edit to repair it.
func (w *BarbicanWebhook) ValidateCreate(ctx context.Context, obj *Barbican) (admission.Warnings, error) {
	warnings, createErrs := validateExtraConfigOptions(field.NewPath("spec"), obj)
	createErrs = append(createErrs, validateNameLength(obj.Name)...)
	warnings = append(warnings, warnDBCleanFirstRun(obj)...)
	return warnings, w.validate(ctx, obj, createErrs)
}

// validateNameLength bounds metadata.name by the child object with the tightest
// name budget, the "{name}-db-clean" CronJob: the API server rejects a CronJob
// name longer than MaxCronJobNameLength.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateNameLength(name string) field.ErrorList {
	if len(name) <= MaxBarbicanNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"), name,
		fmt.Sprintf("name must be at most %d characters: the db-clean CronJob appends %q and Kubernetes caps CronJob names at %d characters",
			MaxBarbicanNameLength, dbCleanNameSuffix, MaxCronJobNameLength),
	)}
}

// ValidateUpdate implements admission.Validator[*Barbican].
//
// The extraConfig option-catalog check is re-run only when one of its inputs
// changed (extraConfig or spec.openStackRelease). This keeps an unrelated update
// (scaling replicas, say) from retroactively rejecting a CR whose extraConfig was
// accepted at create time but has since been invalidated by a regenerated
// catalog.
//
// spec.targetClusterRef is compared across both revisions here, the webhook-layer
// twin of the two transition CEL rules on BarbicanSpec.
func (w *BarbicanWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *Barbican) (admission.Warnings, error) {
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
	warnings = append(warnings, warnDBCleanRetention(oldObj.Spec.DBClean, newObj.Spec.DBClean)...)
	return warnings, w.validate(ctx, newObj, updateErrs)
}

// validate runs all validation rules against the Barbican spec, accumulating
// every violation so users see the full list in one admission response.
// ctx is required for cluster-scoped lookups (PriorityClass validation).
// extra carries the errors accumulated by the caller (the extraConfig
// option-catalog check, on create the metadata.name bound, and on update the
// targetClusterRef immutability check) so they aggregate into the single Invalid
// error alongside the rest.
func (w *BarbicanWebhook) validate(ctx context.Context, b *Barbican, extra field.ErrorList) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Defense-in-depth replicas check alongside the
	// +kubebuilder:validation:Minimum=1 marker.
	if b.Spec.Deployment.Replicas < 1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "replicas"),
			b.Spec.Deployment.Replicas,
			"replicas must be at least 1",
		))
	}

	// Defense-in-depth image tag/digest XOR check alongside the
	// +kubebuilder:validation:XValidation rule on commonv1.ImageSpec: exactly one
	// of tag or digest must be set.
	if (b.Spec.Image.Tag != "") == (b.Spec.Image.Digest != "") {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("image"),
			b.Spec.Image,
			"exactly one of image.tag or image.digest must be set",
		))
	}

	// Defense-in-depth database/cache mutual-exclusivity and
	// Dynamic-requires-clusterRef checks alongside the
	// +kubebuilder:validation:XValidation CEL rules on the shared commonv1 types,
	// via the shared validators.
	allErrs = append(allErrs, validation.DatabaseXOR(specPath.Child("database"), &b.Spec.Database)...)
	allErrs = append(allErrs, validation.DynamicCredentialsRequireClusterRef(specPath.Child("database"), &b.Spec.Database)...)
	allErrs = append(allErrs, validation.CacheXOR(specPath.Child("cache"), &b.Spec.Cache)...)
	// spec.cache reaches the verbatim INI renderer the same way the typed fields
	// below do: cache.ResolveServers derives [keystone_authtoken].memcached_servers
	// from spec.cache.servers or spec.cache.clusterRef.name. This is the
	// defense-in-depth twin of the items pattern and XValidation rule the shared
	// commonv1.CacheSpec carries, for objects that bypass schema validation.
	allErrs = append(allErrs, validation.CacheNoControlChars(specPath.Child("cache"), &b.Spec.Cache)...)
	allErrs = append(allErrs, validation.SecretStoreRef(specPath.Child("secretStoreRef"), b.Spec.SecretStoreRef)...)
	allErrs = append(allErrs, validation.TargetClusterRef(specPath.Child("targetClusterRef"), b.Spec.TargetClusterRef)...)

	// keystoneEndpoint is required (rendered as [keystone_authtoken] auth_url):
	// empty is Required, otherwise it must parse as an absolute http(s) URL.
	// Barbican hands the value verbatim to keystonemiddleware, so an unparseable
	// URL or a missing host would only surface as a token-validation failure at
	// runtime.
	if b.Spec.KeystoneEndpoint == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("keystoneEndpoint"),
			"keystoneEndpoint must be set (the Keystone auth_url the Barbican pods reach)",
		))
	} else {
		allErrs = append(allErrs, validateEndpointURL(specPath.Child("keystoneEndpoint"), b.Spec.KeystoneEndpoint)...)
	}
	// keystonePublicEndpoint is optional (falls back to keystoneEndpoint at render
	// time); validate it only when set.
	if b.Spec.KeystonePublicEndpoint != "" {
		allErrs = append(allErrs, validateEndpointURL(specPath.Child("keystonePublicEndpoint"), b.Spec.KeystonePublicEndpoint)...)
	}

	// Typed spec fields reach the same verbatim INI renderer as extraConfig: each
	// value below is rendered into [keystone_authtoken] as "%s = %s". A newline or
	// carriage return therefore injects an additional config line — smuggling a
	// whole key past the (section, key)-keyed ownership (FindOwnedOverrides) and
	// catalog (FindUnknownOptions) gates, exactly the bypass the extraConfig check
	// below blocks. The two Keystone endpoint fields need no entry here: they go
	// through validateEndpointURL, whose url.Parse rejects control bytes; spec.cache
	// feeds the same section and is covered by CacheNoControlChars above.
	//
	// gateway.hostname is on the list because it reaches the renderer as [DEFAULT]
	// host_href, and [DEFAULT] is rendered first — an injected line therefore
	// lands in a section the operator never writes, so nothing overrides it. The
	// HTTPRoute step would reject the hostname later, but it runs after the config
	// Secret has been written and mounted.
	for _, f := range []struct {
		path  *field.Path
		value string
	}{
		{specPath.Child("region"), b.Spec.Region},
		{specPath.Child("serviceUser", "username"), b.Spec.ServiceUser.Username},
		{specPath.Child("serviceUser", "projectName"), b.Spec.ServiceUser.ProjectName},
		{specPath.Child("serviceUser", "userDomainName"), b.Spec.ServiceUser.UserDomainName},
		{specPath.Child("serviceUser", "projectDomainName"), b.Spec.ServiceUser.ProjectDomainName},
		{specPath.Child("gateway", "hostname"), gatewayHostname(b)},
	} {
		if validation.HasControlChars(f.value) {
			allErrs = append(allErrs, field.Invalid(f.path, f.value,
				"value must not contain a newline or carriage return: it is rendered verbatim into "+
					"barbican.conf, so a newline injects arbitrary config lines"))
		}
	}

	// Defense-in-depth logging validation alongside the CRD enum markers on
	// LoggingSpec.Format / .Level. Map values cannot be expressed as a CRD enum on
	// additionalProperties, so the per-logger level check has no schema-layer
	// counterpart — the webhook is the only enforcement point for that case.
	if b.Spec.Logging != nil {
		loggingPath := specPath.Child("logging")
		validLevels := map[string]struct{}{
			"DEBUG":    {},
			"INFO":     {},
			"WARNING":  {},
			"ERROR":    {},
			"CRITICAL": {},
		}
		if b.Spec.Logging.Format != "" && b.Spec.Logging.Format != "text" && b.Spec.Logging.Format != "json" {
			allErrs = append(allErrs, field.NotSupported(
				loggingPath.Child("format"),
				b.Spec.Logging.Format,
				[]string{"text", "json"},
			))
		}
		if b.Spec.Logging.Level != "" {
			if _, ok := validLevels[b.Spec.Logging.Level]; !ok {
				allErrs = append(allErrs, field.NotSupported(
					loggingPath.Child("level"),
					b.Spec.Logging.Level,
					[]string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"},
				))
			}
		}
		perLoggerPath := loggingPath.Child("perLoggerLevels")
		for name, lvl := range b.Spec.Logging.PerLoggerLevels {
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
						"verbatim into barbican.conf, so a newline injects arbitrary config lines",
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
	if b.Spec.Deployment.TerminationGracePeriodSeconds != nil && *b.Spec.Deployment.TerminationGracePeriodSeconds < 10 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "terminationGracePeriodSeconds"),
			*b.Spec.Deployment.TerminationGracePeriodSeconds,
			"terminationGracePeriodSeconds must be at least 10",
		))
	}
	// Defense-in-depth range check on spec.deployment.preStopSleepSeconds
	// alongside the +kubebuilder:validation:Minimum=0 marker.
	if b.Spec.Deployment.PreStopSleepSeconds != nil && *b.Spec.Deployment.PreStopSleepSeconds < 0 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "preStopSleepSeconds"),
			*b.Spec.Deployment.PreStopSleepSeconds,
			"preStopSleepSeconds must be non-negative",
		))
	}

	// preStopSleepSeconds must be strictly less than
	// terminationGracePeriodSeconds so there is a non-zero drain window between the
	// end of the preStop sleep and the forced kubelet kill. Resolve nil pointers to
	// the reconciler's effective defaults so the cross-field rule holds even when
	// one or both pointers are omitted.
	resolvedGrace := commonv1.DefaultTerminationGracePeriodSeconds
	if b.Spec.Deployment.TerminationGracePeriodSeconds != nil {
		resolvedGrace = *b.Spec.Deployment.TerminationGracePeriodSeconds
	}
	resolvedPreStop := commonv1.DefaultPreStopSleepSeconds
	if b.Spec.Deployment.PreStopSleepSeconds != nil {
		resolvedPreStop = *b.Spec.Deployment.PreStopSleepSeconds
	}
	if resolvedPreStop >= resolvedGrace {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("deployment", "preStopSleepSeconds"),
			resolvedPreStop,
			fmt.Sprintf("preStopSleepSeconds (%d) must be strictly less than terminationGracePeriodSeconds (%d)", resolvedPreStop, resolvedGrace),
		))
	}

	if b.Spec.APIServer != nil && b.Spec.APIServer.UWSGI != nil {
		uwsgiPath := specPath.Child("apiServer", "uwsgi")
		u := b.Spec.APIServer.UWSGI
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
		// HTTPKeepAlive pointer means "unset", which resolves to the default true, so
		// only an EXPLICIT false conflicts with a set timeout. This mirrors the
		// XValidation rule on UWSGISpec.
		if u.HTTPKeepAliveTimeout != nil && u.HTTPKeepAlive != nil && !*u.HTTPKeepAlive {
			allErrs = append(allErrs, field.Invalid(
				uwsgiPath.Child("httpKeepAliveTimeout"),
				*u.HTTPKeepAliveTimeout,
				"httpKeepAliveTimeout may only be set when httpKeepAlive is true",
			))
		}
	}

	// spec.deployment.strategy sanity check — a Recreate strategy must not carry a
	// RollingUpdate block because the Deployment controller would reject the object
	// at apply time.
	if b.Spec.Deployment.Strategy != nil {
		if b.Spec.Deployment.Strategy.Type == appsv1.RecreateDeploymentStrategyType && b.Spec.Deployment.Strategy.RollingUpdate != nil {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("deployment", "strategy", "rollingUpdate"),
				b.Spec.Deployment.Strategy.RollingUpdate,
				"rollingUpdate must not be set when strategy.type is Recreate",
			))
		}
	}

	// Defense-in-depth autoscaling validation alongside kubebuilder markers and CEL
	// rules.
	if b.Spec.Autoscaling != nil {
		autoscalingPath := specPath.Child("autoscaling")
		if b.Spec.Autoscaling.MaxReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				b.Spec.Autoscaling.MaxReplicas,
				"maxReplicas must be at least 1",
			))
		}
		if b.Spec.Autoscaling.MinReplicas != nil && *b.Spec.Autoscaling.MinReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*b.Spec.Autoscaling.MinReplicas,
				"minReplicas must be at least 1",
			))
		}
		if b.Spec.Autoscaling.MinReplicas != nil && *b.Spec.Autoscaling.MinReplicas > b.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*b.Spec.Autoscaling.MinReplicas,
				"minReplicas must not exceed maxReplicas",
			))
		}
		// When minReplicas is unset, the reconciler defaults it to
		// spec.deployment.replicas. Reject configurations where the implicit default
		// would exceed maxReplicas, which would produce an HPA rejected by the API
		// server.
		if b.Spec.Autoscaling.MinReplicas == nil && b.Spec.Deployment.Replicas > b.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				b.Spec.Autoscaling.MaxReplicas,
				fmt.Sprintf("maxReplicas must be >= spec.deployment.replicas (%d) when minReplicas is not set, because minReplicas defaults to spec.deployment.replicas", b.Spec.Deployment.Replicas),
			))
		}
		// Defense-in-depth bounds checks for utilization targets alongside
		// +kubebuilder:validation:Minimum=1 / Maximum=100 markers.
		if b.Spec.Autoscaling.TargetCPUUtilization != nil && (*b.Spec.Autoscaling.TargetCPUUtilization < 1 || *b.Spec.Autoscaling.TargetCPUUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetCPUUtilization"),
				*b.Spec.Autoscaling.TargetCPUUtilization,
				"targetCPUUtilization must be between 1 and 100",
			))
		}
		if b.Spec.Autoscaling.TargetMemoryUtilization != nil && (*b.Spec.Autoscaling.TargetMemoryUtilization < 1 || *b.Spec.Autoscaling.TargetMemoryUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetMemoryUtilization"),
				*b.Spec.Autoscaling.TargetMemoryUtilization,
				"targetMemoryUtilization must be between 1 and 100",
			))
		}
		if b.Spec.Autoscaling.TargetCPUUtilization == nil && b.Spec.Autoscaling.TargetMemoryUtilization == nil {
			allErrs = append(allErrs, field.Required(
				autoscalingPath,
				"at least one of targetCPUUtilization or targetMemoryUtilization must be set",
			))
		}
	}

	// Defense-in-depth networkPolicy ingress check alongside the
	// +kubebuilder:validation:XValidation CEL rule on NetworkPolicySpec.
	if b.Spec.NetworkPolicy != nil && len(b.Spec.NetworkPolicy.Ingress) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("networkPolicy", "ingress"),
			"at least one ingress source must be specified",
		))
	}

	// Defense-in-depth gateway validation alongside the
	// +kubebuilder:validation:MinLength=1 markers on GatewaySpec.Hostname and
	// GatewayParentRefSpec.Name.
	if b.Spec.Gateway != nil {
		gatewayPath := specPath.Child("gateway")
		if b.Spec.Gateway.Hostname == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("hostname"),
				"hostname must be set when spec.gateway is configured",
			))
		}
		if b.Spec.Gateway.ParentRef.Name == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("parentRef", "name"),
				"parentRef.name must be set when spec.gateway is configured",
			))
		}
	}

	// Reject an empty policy rule name or an empty rule value via the shared
	// validator. Neither is reachable from a CRD marker on a map, so this is the
	// only gate: oslo.policy reads an empty rule value as "allow everyone", which
	// is the opposite of what an override that spells the rule out means.
	if b.Spec.PolicyOverrides != nil {
		allErrs = append(allErrs, policy.ValidatePolicyRules(
			b.Spec.PolicyOverrides.Rules,
			specPath.Child("policyOverrides", "rules"),
		)...)
	}

	// Defense-in-depth dbClean validation alongside the Minimum marker on
	// DBCleanSpec.RetentionDays, plus the cron-grammar check that has no schema
	// counterpart.
	allErrs = append(allErrs, validateDBClean(specPath.Child("dbClean"), b.Spec.DBClean)...)

	// extraConfig sanity: reject an empty section name or an empty option key so
	// the rendered barbican.conf never carries a nameless [<section>] or a bare
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
	for section, opts := range b.Spec.ExtraConfig {
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
	// is driven by the owned-config-key registry's Rejected flag; for barbican it is
	// [keystone_authtoken] password and the two [vault_plugin] credential keys, each
	// inert at runtime (env-injected) but copied into the rendered config Secret
	// every API pod mounts if rendered. Admission is the only gate before the
	// credential reaches that Secret, so this is a rejection, not a
	// reported-but-honored override.
	for _, e := range OwnedConfigKeys {
		if !e.Rejected {
			continue
		}
		if _, ok := b.Spec.ExtraConfig[e.Section][e.Key]; ok {
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
	if b.Spec.Deployment.Resources != nil && b.Spec.Deployment.Resources.Limits != nil {
		for resourceName, request := range b.Spec.Deployment.Resources.Requests {
			if limit, hasLimit := b.Spec.Deployment.Resources.Limits[resourceName]; hasLimit && request.Cmp(limit) > 0 {
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
	if b.Spec.Deployment.PriorityClassName != nil {
		allErrs = append(allErrs, validation.PriorityClassExists(ctx, w.Client,
			specPath.Child("deployment", "priorityClassName"), *b.Spec.Deployment.PriorityClassName)...)
	}

	// Validate that custom TopologySpreadConstraints use the correct LabelSelector
	// matching the Deployment's selector labels.
	if b.Spec.Deployment.TopologySpreadConstraints != nil {
		allErrs = append(allErrs, validation.TopologySpreadSelector(
			specPath.Child("deployment", "topologySpreadConstraints"),
			b.Spec.Deployment.TopologySpreadConstraints,
			map[string]string{
				naming.LabelKeyName:     "barbican",
				naming.LabelKeyInstance: b.Name,
			},
		)...)
	}

	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Barbican"},
			b.Name,
			allErrs,
		)
	}
	return nil
}

// validateEndpointURL checks that a non-empty endpoint parses cleanly, uses an
// http(s) scheme, and carries a host — the same shape the sibling operators
// enforce on their Keystone endpoints. It is applied to both Barbican Keystone
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

// gatewayHostname returns spec.gateway.hostname, or "" when spec.gateway is
// unset. It exists so the control-character guard can carry the hostname as a
// plain table entry: HasControlChars("") is false, so a CR without a gateway
// contributes no error.
func gatewayHostname(b *Barbican) string {
	if b.Spec.Gateway == nil {
		return ""
	}
	return b.Spec.Gateway.Hostname
}

// validateDBClean mirrors the spec.dbClean schema as defense in depth behind the
// CRD layer — the Minimum marker on retentionDays — and carries the one check
// with no schema counterpart: the cron grammar, which DBCleanSpec.Schedule
// deliberately leaves to the webhook. It is nil-safe, and so is each half: an
// unset field carries nothing to validate and is resolved to the operator default
// at reconcile time.
func validateDBClean(fldPath *field.Path, c *DBCleanSpec) field.ErrorList {
	if c == nil {
		return nil
	}
	var errs field.ErrorList

	if c.RetentionDays != nil && *c.RetentionDays < 1 {
		errs = append(errs, field.Invalid(
			fldPath.Child("retentionDays"), *c.RetentionDays, "retentionDays must be at least 1",
		))
	}
	if c.Schedule != "" {
		errs = append(errs, validation.CronSchedule(fldPath.Child("schedule"), c.Schedule)...)
	}
	return errs
}

// warnDBCleanRetention surfaces a retention window that an update shortens.
// Unlike every other knob on the CR, the effect is immediate and irreversible: at
// the next firing the clean-up hard-deletes every row that fell out of the
// window, and nothing brings those rows back. A typo (9 for 90) is
// indistinguishable from an intended change at admission time, so the reduction
// is echoed back to whoever made it. It stays a warning rather than a rejection
// because shortening the window is a legitimate operational choice.
func warnDBCleanRetention(oldClean, newClean *DBCleanSpec) admission.Warnings {
	oldDays, newDays := effectiveRetentionDays(oldClean), effectiveRetentionDays(newClean)
	if newDays >= oldDays {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.dbClean.retentionDays reduced %d → %d: the next clean-up run hard-deletes the rows soft-deleted between %d and %d days ago, which cannot be undone",
		oldDays, newDays, newDays, oldDays,
	)}
}

// warnDBCleanFirstRun surfaces what the first clean-up run does when a
// brownfield database is adopted without suspending it.
//
// The clean-up defaults to enabled, and its first firing applies the retention
// window retroactively. On a database this operator provisions that is harmless
// — there is no history to purge — but a brownfield database brought under
// management carries a backlog that has never been cleaned, so the first run
// hard-deletes every row soft-deleted outside the window and soft-deletes every
// currently-expired secret, in one pass, irreversibly. DBCleanSpec documents the
// same hazard and the escape hatch, but the default is the destructive one, so
// the consequence is echoed back to whoever adopts the database rather than
// relying on them having read the field doc. warnDBCleanRetention covers the
// later reduction; this covers the larger, first deletion.
func warnDBCleanFirstRun(b *Barbican) admission.Warnings {
	if b.Spec.Database.ClusterRef != nil {
		return nil
	}
	if b.Spec.DBClean != nil && b.Spec.DBClean.Suspend {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.database names an existing server (brownfield) and the database clean-up runs by default: its first "+
			"firing hard-deletes every row soft-deleted more than %d days ago and soft-deletes every expired secret, "+
			"which on a database that has never been cleaned cannot be undone — set spec.dbClean.suspend to stage it",
		effectiveRetentionDays(b.Spec.DBClean),
	)}
}

// effectiveRetentionDays resolves the retention a spec.dbClean block runs with,
// mirroring the reconcile-time resolver so a block that leaves the field unset
// compares as the operator default rather than as zero — otherwise dropping
// spec.dbClean entirely would read as a reduction to nothing.
func effectiveRetentionDays(c *DBCleanSpec) int32 {
	if c == nil || c.RetentionDays == nil {
		return DefaultDBCleanRetentionDays
	}
	return *c.RetentionDays
}

// validateExtraConfigOptions validates the option names in spec.extraConfig
// against the option catalog embedded for the release named by
// spec.openStackRelease.
//
// It fails open: when spec.openStackRelease cannot be resolved to an embedded
// catalog it returns exactly one warning and no errors, so a value that does not
// name a release or a build that ships no catalog for the release never blocks
// admission. The exemptions come from catalogExemptions below. Every remaining
// unknown option or unknown section becomes a field error; every
// deprecated-but-accepted option becomes a warning naming its replacement.
func validateExtraConfigOptions(specPath *field.Path, b *Barbican) (admission.Warnings, field.ErrorList) {
	if len(b.Spec.ExtraConfig) == 0 {
		return nil, nil
	}

	catalog, ok := OptionCatalogForRelease(b.Spec.OpenStackRelease)
	if !ok {
		// Fail open with exactly one warning. Distinguish a value that does not name
		// a release at all from a parseable release the operator ships no catalog
		// for.
		rel, err := release.ParseRelease(b.Spec.OpenStackRelease)
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

	base := catalog.Release
	unknown, deprecated := catalog.FindUnknownOptions(b.Spec.ExtraConfig, catalogExemptions(b.Spec.ExtraConfig))

	extraConfigPath := specPath.Child("extraConfig")
	var errs field.ErrorList
	for _, u := range unknown {
		fldPath := extraConfigPath.Key(u.Section).Key(u.Key)
		if u.SectionUnknown {
			errs = append(errs, field.Invalid(fldPath, u.Key,
				fmt.Sprintf("no such section in the barbican %s option catalog", base)))
			continue
		}
		errs = append(errs, field.Invalid(fldPath, u.Key,
			fmt.Sprintf("no such option in the barbican %s option catalog", base)))
	}

	var warnings admission.Warnings
	for _, d := range deprecated {
		if d.Replacement == "" {
			warnings = append(warnings, fmt.Sprintf(
				"spec.extraConfig [%s] %s: deprecated option in barbican %s with no replacement",
				d.Section, d.Key, base,
			))
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"spec.extraConfig [%s] %s: deprecated option in barbican %s, replaced by %s",
			d.Section, d.Key, base, d.Replacement,
		))
	}
	return warnings, errs
}

// catalogExemptions builds the exemption set the unknown-option scan runs with:
// the operator-owned keys from OwnedConfigKeys, and every extraConfig section
// whose name starts with a CatalogExemptSectionPrefixes entry.
//
// The prefix expansion happens here because config.CatalogExemptions.Sections
// matches exact section names only. The per-store [secretstore:<name>] sections
// take their names from the attached BarbicanSecretStore CRs, so the exact names
// are only known from the configuration under validation.
func catalogExemptions(extraConfig map[string]map[string]string) config.CatalogExemptions {
	sections := make(map[string]struct{})
	for section := range extraConfig {
		for _, prefix := range CatalogExemptSectionPrefixes {
			if strings.HasPrefix(section, prefix) {
				sections[section] = struct{}{}
			}
		}
	}
	return config.CatalogExemptions{
		Sections: sections,
		Keys:     config.KeyExemptionsFromRegistry(OwnedConfigKeys),
	}
}

// extraConfigCatalogInputsChanged reports whether anything the extraConfig
// option-catalog check depends on differs between oldObj and newObj: the
// extraConfig map itself, or spec.openStackRelease that selects the release
// catalog. ValidateUpdate gates the catalog re-validation on this so a CR whose
// extraConfig went stale-invalid is not rejected by an otherwise-unrelated
// update.
func extraConfigCatalogInputsChanged(oldObj, newObj *Barbican) bool {
	if oldObj.Spec.OpenStackRelease != newObj.Spec.OpenStackRelease {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec.ExtraConfig, newObj.Spec.ExtraConfig)
}
