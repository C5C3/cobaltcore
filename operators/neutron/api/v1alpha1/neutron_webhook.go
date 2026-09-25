// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/validation"
	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// defaultMessagingCABundleKey is the Secret key spec.messaging.tls
// .caBundleSecretRef is defaulted to, the name cert-manager gives the CA bundle
// in every Secret it issues.
const defaultMessagingCABundleKey = "ca.crt"

// neutronAppName is the app.kubernetes.io/name label value every Neutron-owned
// object carries. It is duplicated from the controller package (which builds the
// objects) because the api package cannot import the controller; the
// topology-spread selector check composes the API Deployment's selector from it.
const neutronAppName = "neutron"

// NeutronWebhook implements defaulting and validation webhooks for the Neutron
// CRD. Client is injected at startup for cluster-scoped resource lookups (e.g.
// PriorityClass validation). Production wiring injects mgr.GetAPIReader() — a
// direct, uncached reader — so admission never rejects a just-created object from
// a stale informer cache and no lazy informer start happens inside the webhook
// timeout.
// +kubebuilder:object:generate=false
type NeutronWebhook struct {
	commonwebhook.NoopDeleteValidator[*Neutron]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*Neutron] = &NeutronWebhook{}
	_ admission.Validator[*Neutron] = &NeutronWebhook{}
)

// +kubebuilder:webhook:path=/mutate-neutron-openstack-c5c3-io-v1alpha1-neutron,mutating=true,failurePolicy=fail,sideEffects=None,groups=neutron.openstack.c5c3.io,resources=neutrons,verbs=create;update,versions=v1alpha1,name=mneutron.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-neutron-openstack-c5c3-io-v1alpha1-neutron,mutating=false,failurePolicy=fail,sideEffects=None,groups=neutron.openstack.c5c3.io,resources=neutrons,verbs=create;update,versions=v1alpha1,name=vneutron.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *NeutronWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*Neutron](mgr, &Neutron{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*Neutron]. It sets spec fields to their
// documented defaults when they carry zero values, following the keystone/horizon
// non-mutating discipline: optional pointer blocks are only partially filled when
// explicitly present, except spec.logging which is materialized so downstream
// reconciler code never sees a nil pointer.
func (w *NeutronWebhook) Default(_ context.Context, obj *Neutron) error {
	// Shared-type defaults (replicas) are applied by the
	// commonv1.DeploymentSpec Default method so they cannot drift across
	// operators. Both Deployments get them: the API pods and the RPC workers are
	// sized independently.
	obj.Spec.Deployment.Default()
	obj.Spec.Workers.Deployment.Default()
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
		su.Username = "neutron"
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

	// The Nova notifier identity is defaulted the same way, but inside a present
	// block alone: an absent spec.nova is what keeps the port notifications off,
	// so materializing the block here would switch them on for every CR.
	if nova := obj.Spec.Nova; nova != nil {
		nu := &nova.ServiceUser
		if nu.Username == "" {
			nu.Username = "neutron-nova"
		}
		if nu.ProjectName == "" {
			nu.ProjectName = "service"
		}
		if nu.UserDomainName == "" {
			nu.UserDomainName = "Default"
		}
		if nu.ProjectDomainName == "" {
			nu.ProjectDomainName = "Default"
		}
		if nu.SecretRef.Key == "" {
			nu.SecretRef.Key = "password"
		}
	}

	// The OVN control plane is resolved by name and namespace. An empty namespace
	// is materialized rather than resolved at reconcile time so the CR records
	// which namespace was meant when it was created, and a later move of the
	// Neutron itself cannot silently re-point it.
	if obj.Spec.OVN.CentralRef.Namespace == "" {
		obj.Spec.OVN.CentralRef.Namespace = obj.Namespace
	}

	// Messaging Secret-key defaults, filled only for the halves the CR carries: a
	// brownfield transport-URL Secret and the CA bundle a TLS block names.
	if obj.Spec.Messaging.SecretRef != nil && obj.Spec.Messaging.SecretRef.Key == "" {
		obj.Spec.Messaging.SecretRef.Key = commonv1.DefaultTransportURLSecretKey
	}
	if obj.Spec.Messaging.TLS != nil && obj.Spec.Messaging.TLS.CABundleSecretRef.Key == "" {
		obj.Spec.Messaging.TLS.CABundleSecretRef.Key = defaultMessagingCABundleKey
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

// ValidateCreate implements admission.Validator[*Neutron].
//
// The metadata.name bound is enforced here rather than in validate(), which
// update shares: the name is immutable, so on update the rule could only ever
// fire against an object a pre-upgrade operator already admitted, and it would
// refuse every update to that CR with no field left to edit to repair it. (The
// finalizer removal on delete skips validation altogether; see ValidateUpdate.)
func (w *NeutronWebhook) ValidateCreate(ctx context.Context, obj *Neutron) (admission.Warnings, error) {
	warnings, createErrs := validateExtraConfigOptions(
		field.NewPath("spec"), obj.Spec.OpenStackRelease, obj.Spec.ExtraConfig, OwnedConfigKeys)
	createErrs = append(createErrs, validateNeutronNameLength(obj.Name)...)
	return warnings, w.validate(ctx, obj, nil, createErrs)
}

// validateNeutronNameLength bounds metadata.name by the child object with the
// tightest name budget, the "{name}-ovn-db-sync" CronJob: the API server rejects
// a CronJob name longer than MaxCronJobNameLength.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateNeutronNameLength(name string) field.ErrorList {
	if len(name) <= MaxNeutronNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"), name,
		fmt.Sprintf("name must be at most %d characters: the ovn-db-sync CronJob appends %q and Kubernetes caps CronJob names at %d characters",
			MaxNeutronNameLength, ovnDBSyncNameSuffix, MaxCronJobNameLength),
	)}
}

// ValidateUpdate implements admission.Validator[*Neutron].
//
// The extraConfig option-catalog check is re-run only when one of its inputs
// changed (extraConfig or spec.openStackRelease). This keeps an unrelated update
// (scaling replicas, say) from retroactively rejecting a CR whose extraConfig was
// accepted at create time but has since been invalidated by a regenerated
// catalog.
//
// spec.targetClusterRef is compared across both revisions here, the webhook-layer
// twin of the two transition CEL rules on NeutronSpec.
//
// A Rejected extraConfig key the update carries over unchanged is reported as a
// warning rather than refused (see validateExtraConfigShape), so a CR admitted
// before the operator started owning that key stays updatable.
//
// An update to a CR that is being deleted and leaves its spec alone is admitted
// without validation. That is the finalizer removal reconcileDelete issues, and
// the rules below can reject an unchanged spec that was admitted earlier: a
// PriorityClass deleted since, or a topology-spread selector that predates the
// requirement of the API component. Rejecting the removal would hold the CR in
// Terminating. A deleting CR whose spec changes is still validated. The
// defaulting webhook has already run on newObj, so a copy of the stored object is
// defaulted the same way before the two specs are compared: a default an operator
// release added after the CR was last written is no spec change.
func (w *NeutronWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *Neutron) (admission.Warnings, error) {
	if newObj.DeletionTimestamp != nil {
		stored := oldObj.DeepCopy()
		if err := w.Default(ctx, stored); err != nil {
			return nil, fmt.Errorf("defaulting the stored Neutron: %w", err)
		}
		if equality.Semantic.DeepEqual(stored.Spec, newObj.Spec) {
			return nil, nil
		}
	}

	var warnings admission.Warnings
	var updateErrs field.ErrorList
	if extraConfigCatalogInputsChanged(
		oldObj.Spec.OpenStackRelease, newObj.Spec.OpenStackRelease,
		oldObj.Spec.ExtraConfig, newObj.Spec.ExtraConfig,
	) {
		warnings, updateErrs = validateExtraConfigOptions(
			field.NewPath("spec"), newObj.Spec.OpenStackRelease, newObj.Spec.ExtraConfig, OwnedConfigKeys)
	}
	updateErrs = append(updateErrs, validation.TargetClusterRefImmutable(
		field.NewPath("spec", "targetClusterRef"),
		oldObj.Spec.TargetClusterRef,
		newObj.Spec.TargetClusterRef,
	)...)
	warnings = append(warnings, carriedRejectedKeyWarnings(
		field.NewPath("spec"), newObj.Spec.ExtraConfig, oldObj.Spec.ExtraConfig, OwnedConfigKeys)...)
	return warnings, w.validate(ctx, newObj, oldObj.Spec.ExtraConfig, updateErrs)
}

// validate runs all validation rules against the Neutron spec, accumulating
// every violation so users see the full list in one admission response.
// ctx is required for cluster-scoped lookups (PriorityClass validation).
// oldExtraConfig is the stored object's spec.extraConfig on update and nil on
// create (see validateExtraConfigShape). extra carries the errors accumulated by
// the caller (the extraConfig option-catalog check, on create the metadata.name
// bound, and on update the targetClusterRef immutability check) so they
// aggregate into the single Invalid error alongside the rest.
func (w *NeutronWebhook) validate(
	ctx context.Context, n *Neutron, oldExtraConfig map[string]map[string]string, extra field.ErrorList,
) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// spec.deployment configures the API Deployment and spec.workers.deployment
	// the periodic-workers and ovn-maintenance-worker Deployments. Both carry
	// the same pod-level knobs and are validated by the same rules. The worker
	// block has no single selector a topology-spread constraint could name,
	// because it configures two Deployments with different selectors.
	allErrs = append(allErrs, w.validateDeploymentBlock(ctx, specPath.Child("deployment"),
		&n.Spec.Deployment, APIPodSelector(n.Name))...)
	allErrs = append(allErrs, w.validateDeploymentBlock(ctx, specPath.Child("workers", "deployment"),
		&n.Spec.Workers.Deployment, nil)...)

	// Defense-in-depth image checks alongside the +kubebuilder:validation markers
	// and the XValidation rule on commonv1.ImageSpec.
	allErrs = append(allErrs, validateImage(specPath.Child("image"), n.Spec.Image)...)

	// Defense-in-depth database/cache/messaging mutual-exclusivity and
	// Dynamic-requires-clusterRef checks alongside the
	// +kubebuilder:validation:XValidation CEL rules on the shared commonv1 types,
	// via the shared validators.
	allErrs = append(allErrs, validation.DatabaseXOR(specPath.Child("database"), &n.Spec.Database)...)
	allErrs = append(allErrs, validation.DynamicCredentialsRequireClusterRef(specPath.Child("database"), &n.Spec.Database)...)
	allErrs = append(allErrs, validation.CacheXOR(specPath.Child("cache"), &n.Spec.Cache)...)
	// spec.cache reaches the verbatim INI renderer the same way the typed fields
	// below do: cache.ResolveServers derives [keystone_authtoken].memcached_servers
	// from spec.cache.servers or spec.cache.clusterRef.name. This is the
	// defense-in-depth twin of the items pattern and XValidation rule the shared
	// commonv1.CacheSpec carries, for objects that bypass schema validation.
	allErrs = append(allErrs, validation.CacheNoControlChars(specPath.Child("cache"), &n.Spec.Cache)...)
	allErrs = append(allErrs, validation.MessagingXOR(specPath.Child("messaging"), &n.Spec.Messaging)...)
	allErrs = append(allErrs, validation.SecretStoreRef(specPath.Child("secretStoreRef"), n.Spec.SecretStoreRef)...)
	allErrs = append(allErrs, validation.TargetClusterRef(specPath.Child("targetClusterRef"), n.Spec.TargetClusterRef)...)

	// A TLS block with no CA bundle name has nothing to verify the broker
	// against. The MinLength marker on the shared SecretRefSpec covers the same
	// case at the schema layer; this is its webhook twin.
	if n.Spec.Messaging.TLS != nil && n.Spec.Messaging.TLS.CABundleSecretRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("messaging", "tls", "caBundleSecretRef", "name"),
			"caBundleSecretRef.name must be set when spec.messaging.tls is configured",
		))
	}

	// The OVN control plane is what the ML2/OVN mechanism driver writes the
	// logical network model into, so a Neutron without one has nothing to
	// program. This is the webhook twin of the MinLength marker on
	// OVNCentralRef.Name.
	if n.Spec.OVN.CentralRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("ovn", "centralRef", "name"),
			"centralRef.name must be set (the OVNCentral this Neutron programs)",
		))
	}

	// A spec.nova block with an unnamed Secret has no password to notify with,
	// and the notifier would fail every os-server-external-events call at
	// runtime. The MinLength marker on the shared SecretRefSpec covers the same
	// case at the schema layer; this is its webhook twin.
	if n.Spec.Nova != nil && n.Spec.Nova.ServiceUser.SecretRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("nova", "serviceUser", "secretRef", "name"),
			"serviceUser.secretRef.name must be set when spec.nova is configured: "+
				"it carries the password Neutron notifies Nova with",
		))
	}

	// keystoneEndpoint is required (rendered as [keystone_authtoken] auth_url):
	// empty is Required, otherwise it must parse as an absolute http(s) URL.
	// Neutron hands the value verbatim to keystonemiddleware, so an unparseable
	// URL or a missing host would only surface as a token-validation failure at
	// runtime.
	if n.Spec.KeystoneEndpoint == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("keystoneEndpoint"),
			"keystoneEndpoint must be set (the Keystone auth_url the Neutron pods reach)",
		))
	} else {
		allErrs = append(allErrs, validateEndpointURL(specPath.Child("keystoneEndpoint"), n.Spec.KeystoneEndpoint)...)
	}
	// keystonePublicEndpoint is optional (falls back to keystoneEndpoint at render
	// time); validate it only when set.
	if n.Spec.KeystonePublicEndpoint != "" {
		allErrs = append(allErrs, validateEndpointURL(specPath.Child("keystonePublicEndpoint"), n.Spec.KeystonePublicEndpoint)...)
	}

	// Typed spec fields reach the same verbatim INI renderer as extraConfig: each
	// value below is rendered as "%s = %s" into the section that carries it. A
	// newline or carriage return therefore injects an additional config line —
	// smuggling a whole key past the (section, key)-keyed ownership
	// (FindOwnedOverrides) and catalog (FindUnknownOptions) gates, exactly the
	// bypass validateExtraConfigShape blocks. The two Keystone endpoint fields need
	// no entry here: they go through validateEndpointURL, whose url.Parse rejects
	// control bytes; spec.cache feeds the same section and is covered by
	// CacheNoControlChars above.
	//
	// The two centralRef fields are on the list because the operator resolves them
	// into the [ovn] connection strings, and the five spec.nova values because they
	// reach [nova] the way spec.serviceUser reaches [keystone_authtoken].
	// gateway.hostname is on it because it reaches the renderer as a [DEFAULT]
	// option, and [DEFAULT] is rendered first — an injected line therefore lands in
	// a section the operator never writes, so nothing overrides it. The HTTPRoute
	// step would reject the hostname later, but it runs after the config Secret has
	// been written and mounted.

	// spec.nova is optional, so its five values are read through a zero-valued
	// copy: an absent block carries empty strings, which pass.
	var nova NovaSpec
	if n.Spec.Nova != nil {
		nova = *n.Spec.Nova
	}
	for _, f := range []struct {
		path  *field.Path
		value string
	}{
		{specPath.Child("region"), n.Spec.Region},
		{specPath.Child("serviceUser", "username"), n.Spec.ServiceUser.Username},
		{specPath.Child("serviceUser", "projectName"), n.Spec.ServiceUser.ProjectName},
		{specPath.Child("serviceUser", "userDomainName"), n.Spec.ServiceUser.UserDomainName},
		{specPath.Child("serviceUser", "projectDomainName"), n.Spec.ServiceUser.ProjectDomainName},
		{specPath.Child("nova", "region"), nova.Region},
		{specPath.Child("nova", "serviceUser", "username"), nova.ServiceUser.Username},
		{specPath.Child("nova", "serviceUser", "projectName"), nova.ServiceUser.ProjectName},
		{specPath.Child("nova", "serviceUser", "userDomainName"), nova.ServiceUser.UserDomainName},
		{specPath.Child("nova", "serviceUser", "projectDomainName"), nova.ServiceUser.ProjectDomainName},
		{specPath.Child("ovn", "centralRef", "name"), n.Spec.OVN.CentralRef.Name},
		{specPath.Child("ovn", "centralRef", "namespace"), n.Spec.OVN.CentralRef.Namespace},
		{specPath.Child("gateway", "hostname"), gatewayHostname(n)},
	} {
		if validation.HasControlChars(f.value) {
			allErrs = append(allErrs, field.Invalid(f.path, f.value,
				"value must not contain a newline or carriage return: it is rendered verbatim into "+
					"neutron.conf, so a newline injects arbitrary config lines"))
		}
	}

	allErrs = append(allErrs, validateLogging(specPath.Child("logging"), n.Spec.Logging, "neutron.conf")...)

	if n.Spec.APIServer != nil && n.Spec.APIServer.UWSGI != nil {
		uwsgiPath := specPath.Child("apiServer", "uwsgi")
		u := n.Spec.APIServer.UWSGI
		// harakiri must be strictly less than the drain window
		// (terminationGracePeriodSeconds - preStopSleepSeconds) so the worst-case
		// uWSGI per-request kill fits inside the envelope between preStop sleep
		// completion and SIGKILL. Only applied when harakiri is set. The window is
		// the API block's: uWSGI runs only in the API pods.
		if u.Harakiri != nil {
			grace, preStop := resolveDrainWindow(&n.Spec.Deployment)
			drain := grace - preStop
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

	// Defense-in-depth autoscaling validation alongside kubebuilder markers and CEL
	// rules.
	if n.Spec.Autoscaling != nil {
		autoscalingPath := specPath.Child("autoscaling")
		if n.Spec.Autoscaling.MaxReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				n.Spec.Autoscaling.MaxReplicas,
				"maxReplicas must be at least 1",
			))
		}
		if n.Spec.Autoscaling.MinReplicas != nil && *n.Spec.Autoscaling.MinReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*n.Spec.Autoscaling.MinReplicas,
				"minReplicas must be at least 1",
			))
		}
		if n.Spec.Autoscaling.MinReplicas != nil && *n.Spec.Autoscaling.MinReplicas > n.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*n.Spec.Autoscaling.MinReplicas,
				"minReplicas must not exceed maxReplicas",
			))
		}
		// When minReplicas is unset, the reconciler defaults it to
		// spec.deployment.replicas. Reject configurations where the implicit default
		// would exceed maxReplicas, which would produce an HPA rejected by the API
		// server.
		if n.Spec.Autoscaling.MinReplicas == nil && n.Spec.Deployment.Replicas > n.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				n.Spec.Autoscaling.MaxReplicas,
				fmt.Sprintf("maxReplicas must be >= spec.deployment.replicas (%d) when minReplicas is not set, because minReplicas defaults to spec.deployment.replicas", n.Spec.Deployment.Replicas),
			))
		}
		// Defense-in-depth bounds checks for utilization targets alongside
		// +kubebuilder:validation:Minimum=1 / Maximum=100 markers.
		if n.Spec.Autoscaling.TargetCPUUtilization != nil && (*n.Spec.Autoscaling.TargetCPUUtilization < 1 || *n.Spec.Autoscaling.TargetCPUUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetCPUUtilization"),
				*n.Spec.Autoscaling.TargetCPUUtilization,
				"targetCPUUtilization must be between 1 and 100",
			))
		}
		if n.Spec.Autoscaling.TargetMemoryUtilization != nil && (*n.Spec.Autoscaling.TargetMemoryUtilization < 1 || *n.Spec.Autoscaling.TargetMemoryUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetMemoryUtilization"),
				*n.Spec.Autoscaling.TargetMemoryUtilization,
				"targetMemoryUtilization must be between 1 and 100",
			))
		}
		if n.Spec.Autoscaling.TargetCPUUtilization == nil && n.Spec.Autoscaling.TargetMemoryUtilization == nil {
			allErrs = append(allErrs, field.Required(
				autoscalingPath,
				"at least one of targetCPUUtilization or targetMemoryUtilization must be set",
			))
		}
	}

	// An HPA utilization target is measured against the summed requests of
	// every container in the API pod, so a zero request under a target either
	// fails the metric or inflates it. The render-time default fills a positive
	// request when the block names none.
	allErrs = append(allErrs, validation.AutoscalingTargetRequests(specPath.Child("deployment", "resources"), n.Spec.Deployment.Resources, n.Spec.Autoscaling)...)

	// Defense-in-depth networkPolicy ingress check alongside the
	// +kubebuilder:validation:XValidation CEL rule on NetworkPolicySpec.
	if n.Spec.NetworkPolicy != nil && len(n.Spec.NetworkPolicy.Ingress) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("networkPolicy", "ingress"),
			"at least one ingress source must be specified",
		))
	}

	// Defense-in-depth gateway validation alongside the
	// +kubebuilder:validation:MinLength=1 markers on GatewaySpec.Hostname and
	// GatewayParentRefSpec.Name.
	if n.Spec.Gateway != nil {
		gatewayPath := specPath.Child("gateway")
		if n.Spec.Gateway.Hostname == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("hostname"),
				"hostname must be set when spec.gateway is configured",
			))
		}
		if n.Spec.Gateway.ParentRef.Name == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("parentRef", "name"),
				"parentRef.name must be set when spec.gateway is configured",
			))
		}
	}

	allErrs = append(allErrs, validateOVNDBSync(specPath.Child("ovnDBSync"), n.Spec.OVNDBSync)...)
	allErrs = append(allErrs, validateExtraConfigShape(specPath, n.Spec.ExtraConfig, oldExtraConfig, OwnedConfigKeys)...)

	// The spec.jobs block: requests within limits, an existing priority class,
	// and its own placement.
	allErrs = append(allErrs, validation.Job(ctx, w.Client, specPath.Child("jobs"), n.Spec.Jobs)...)

	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Neutron"},
			n.Name,
			allErrs,
		)
	}
	return nil
}

// validateDeploymentBlock runs the pod-level rules both Neutron Deployment
// blocks share: the replica floor, the graceful-termination arithmetic, the
// strategy sanity check, the request/limit ordering, the PriorityClass lookup
// and the node placement. selector is the pod selector of the Deployment the
// block configures, which a topology-spread constraint on it has to name
// exactly. It is nil for spec.workers.deployment, which configures two
// Deployments with different selectors, so a constraint there has no selector
// to name and is rejected instead.
//
// ctx is required for the PriorityClass lookup, which is skipped when no reader
// is injected (a programmatically constructed webhook), mirroring the shared
// validator's own behavior.
func (w *NeutronWebhook) validateDeploymentBlock(
	ctx context.Context,
	fldPath *field.Path,
	d *commonv1.DeploymentSpec,
	selector map[string]string,
) field.ErrorList {
	var errs field.ErrorList

	// Defense-in-depth replicas check alongside the
	// +kubebuilder:validation:Minimum=1 marker.
	if d.Replicas < 1 {
		errs = append(errs, field.Invalid(
			fldPath.Child("replicas"), d.Replicas, "replicas must be at least 1",
		))
	}

	// Defense-in-depth range checks alongside the
	// +kubebuilder:validation:Minimum=10 / Minimum=0 markers.
	if d.TerminationGracePeriodSeconds != nil && *d.TerminationGracePeriodSeconds < 10 {
		errs = append(errs, field.Invalid(
			fldPath.Child("terminationGracePeriodSeconds"),
			*d.TerminationGracePeriodSeconds,
			"terminationGracePeriodSeconds must be at least 10",
		))
	}
	if d.PreStopSleepSeconds != nil && *d.PreStopSleepSeconds < 0 {
		errs = append(errs, field.Invalid(
			fldPath.Child("preStopSleepSeconds"),
			*d.PreStopSleepSeconds,
			"preStopSleepSeconds must be non-negative",
		))
	}

	// preStopSleepSeconds must be strictly less than
	// terminationGracePeriodSeconds so there is a non-zero drain window between
	// the end of the preStop sleep and the forced kubelet kill.
	grace, preStop := resolveDrainWindow(d)
	if preStop >= grace {
		errs = append(errs, field.Invalid(
			fldPath.Child("preStopSleepSeconds"), preStop,
			fmt.Sprintf("preStopSleepSeconds (%d) must be strictly less than terminationGracePeriodSeconds (%d)", preStop, grace),
		))
	}

	// A Recreate strategy must not carry a RollingUpdate block because the
	// Deployment controller would reject the object at apply time.
	if d.Strategy != nil &&
		d.Strategy.Type == appsv1.RecreateDeploymentStrategyType && d.Strategy.RollingUpdate != nil {
		errs = append(errs, field.Invalid(
			fldPath.Child("strategy", "rollingUpdate"), d.Strategy.RollingUpdate,
			"rollingUpdate must not be set when strategy.type is Recreate",
		))
	}

	errs = append(errs, validation.RequestsWithinLimits(fldPath.Child("resources"), d.Resources)...)

	// Validate that priorityClassName references an existing
	// scheduling.k8s.io/v1 PriorityClass (shared validator; catches typos at
	// admission time, skipped when no lookup client is injected).
	if d.PriorityClassName != nil {
		errs = append(errs, validation.PriorityClassExists(ctx, w.Client,
			fldPath.Child("priorityClassName"), *d.PriorityClassName)...)
	}

	// Node selector grammar and tolerations of the Deployment.
	errs = append(errs, validation.NodePlacement(fldPath, &d.NodePlacementSpec)...)

	// A constraint on the API block must name the API Deployment's pod selector:
	// the shared selector labels narrowed by app.kubernetes.io/component=api. The
	// pods of the two worker Deployments and of the ovn-db-sync CronJob share the
	// name and instance labels, so a selector without the component key would
	// count them too.
	//
	// The worker block has no such selector. The empty slice is the exception
	// there: it names no selector and only switches the operator's injected
	// defaults off.
	if selector == nil {
		if len(d.TopologySpreadConstraints) > 0 {
			errs = append(errs, field.Forbidden(
				fldPath.Child("topologySpreadConstraints"),
				"topologySpreadConstraints is not supported here: the operator projects the "+
					"periodic-workers and ovn-maintenance-worker Deployments from this block, each "+
					"with its own selector, so a constraint set here would spread each worker's "+
					"pods against the pods of the other components. An empty list, which only "+
					"switches the injected defaults off, is accepted",
			))
		}
	} else if d.TopologySpreadConstraints != nil {
		errs = append(errs, validation.TopologySpreadSelector(
			fldPath.Child("topologySpreadConstraints"),
			d.TopologySpreadConstraints,
			selector,
		)...)
	}
	return errs
}

// resolveDrainWindow resolves the graceful-termination pair a Deployment block
// runs with, substituting the reconciler's effective defaults for the
// nil-preserving pointers so the cross-field rules hold even when one or both are
// omitted.
func resolveDrainWindow(d *commonv1.DeploymentSpec) (grace, preStop int64) {
	grace = commonv1.DefaultTerminationGracePeriodSeconds
	if d.TerminationGracePeriodSeconds != nil {
		grace = *d.TerminationGracePeriodSeconds
	}
	preStop = commonv1.DefaultPreStopSleepSeconds
	if d.PreStopSleepSeconds != nil {
		preStop = *d.PreStopSleepSeconds
	}
	return grace, preStop
}

// validateEndpointURL checks that a non-empty endpoint parses cleanly, uses an
// http(s) scheme, and carries a host — the same shape the sibling operators
// enforce on their Keystone endpoints. It is applied to both Neutron Keystone
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
func gatewayHostname(n *Neutron) string {
	if n.Spec.Gateway == nil {
		return ""
	}
	return n.Spec.Gateway.Hostname
}

// validateOVNDBSync mirrors the spec.ovnDBSync schema as defense in depth behind
// the CRD layer — the Enum marker on syncMode — and carries the one check with no
// schema counterpart: the cron grammar, which OVNDBSyncSpec.Schedule deliberately
// leaves to the webhook. It is nil-safe, and so is each half: an unset field
// carries nothing to validate and is resolved to the operator default at
// reconcile time.
func validateOVNDBSync(fldPath *field.Path, s *OVNDBSyncSpec) field.ErrorList {
	if s == nil {
		return nil
	}
	var errs field.ErrorList

	if s.Schedule != "" {
		errs = append(errs, validation.CronSchedule(fldPath.Child("schedule"), s.Schedule)...)
	}
	if s.SyncMode != "" && s.SyncMode != "log" && s.SyncMode != "repair" {
		errs = append(errs, field.NotSupported(
			fldPath.Child("syncMode"), s.SyncMode, []string{"log", "repair"},
		))
	}
	return errs
}

// APIPodSelector returns the pod selector of the API Deployment of the Neutron
// named name, which the labelSelector of every custom topology spread
// constraint on spec.deployment must equal. The ControlPlane reads it to
// complete the spread constraints it projects.
func APIPodSelector(name string) map[string]string {
	return naming.APISelectorLabels(neutronAppName, name)
}
