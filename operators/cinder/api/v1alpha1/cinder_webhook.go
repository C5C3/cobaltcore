// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"

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

	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/validation"
	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// cinderAppName is the app.kubernetes.io/name label value every Cinder-owned
// object carries, and componentScheduler/componentBackup are the
// app.kubernetes.io/component values of two of the four Deployments. They are
// duplicated from the controller package (which builds the objects) because the
// api package cannot import the controller; the topology-spread selector check
// is measured against the labels they compose.
const (
	cinderAppName      = "cinder"
	componentScheduler = "scheduler"
	componentBackup    = "backup"
)

// Name-length bounds enforced on metadata.name, driven by the purge CronJob —
// the child object with the tightest name budget.
const (
	// MaxCronJobNameLength is the API server's own cap on a CronJob name:
	// DNS1035LabelMaxLength (63) minus the 11-character "-<timestamp>" suffix its
	// controller appends to every Job it spawns.
	MaxCronJobNameLength = 52
	// dbPurgeNameSuffix is appended to metadata.name to name the purge CronJob.
	// It is duplicated from the controller package (which builds the object)
	// because the api package cannot import the controller.
	dbPurgeNameSuffix = "-db-purge"
	// MaxCinderNameLength is what those two leave for metadata.name.
	MaxCinderNameLength = MaxCronJobNameLength - len(dbPurgeNameSuffix)
)

// Database-purge defaults for the recurring cinder-manage purge CronJob, which
// hard-deletes the volume, snapshot and backup rows cinder only ever
// soft-deletes.
//
// They are consumed by the reconcile-time resolver and deliberately NOT applied
// by the defaulting webhook: a nil or partial spec.dbPurge block keeps tracking
// these operator defaults across upgrades instead of freezing today's values into
// the stored CR.
const (
	// DefaultDBPurgeRetentionDays is how long a soft-deleted row survives before
	// the purge hard-deletes it, resolved when spec.dbPurge.retentionDays is
	// unset. A month leaves room to notice and reverse an accidental deletion at
	// the storage layer before the row backing it disappears.
	DefaultDBPurgeRetentionDays int32 = 30
	// DefaultDBPurgeSchedule runs the purge daily at 00:01, resolved when
	// spec.dbPurge.schedule is empty. The off-the-hour minute keeps it clear of
	// the top-of-midnight slot the sibling rotation CronJobs occupy.
	DefaultDBPurgeSchedule = "1 0 * * *"
)

// defaultBackupMemoryLimit replaces the shared 512Mi limit on the cinder-backup
// container. A backup reads the volume in chunks of spec.fileSize bytes and
// compresses each chunk in memory before writing it, so the peak footprint
// follows the chunk size rather than the request rate: under the shared limit the
// process is killed mid-backup, and the restarted service begins the volume
// again. CPU and both requests keep the shared defaults.
//
// It is a var rather than a const because resource.Quantity is a struct, and it
// is exposed only through the accessor below, which returns a copy so no caller
// can mutate the shared default — the idiom of the commonv1 resource defaults.
var defaultBackupMemoryLimit = resource.MustParse("2Gi")

// DefaultBackupMemoryLimit returns a copy of the memory limit the defaulting
// webhook stamps on the cinder-backup container when spec.backup.deployment
// .resources carries neither requests nor limits.
func DefaultBackupMemoryLimit() resource.Quantity { return defaultBackupMemoryLimit.DeepCopy() }

// CinderWebhook implements defaulting and validation webhooks for the Cinder
// CRD. Client is injected at startup for cluster-scoped resource lookups (e.g.
// PriorityClass validation). Production wiring injects mgr.GetAPIReader() — a
// direct, uncached reader — so admission never rejects a just-created object from
// a stale informer cache and no lazy informer start happens inside the webhook
// timeout.
// +kubebuilder:object:generate=false
type CinderWebhook struct {
	commonwebhook.NoopDeleteValidator[*Cinder]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*Cinder] = &CinderWebhook{}
	_ admission.Validator[*Cinder] = &CinderWebhook{}
)

// +kubebuilder:webhook:path=/mutate-cinder-openstack-c5c3-io-v1alpha1-cinder,mutating=true,failurePolicy=fail,sideEffects=None,groups=cinder.openstack.c5c3.io,resources=cinders,verbs=create;update,versions=v1alpha1,name=mcinder.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-cinder-openstack-c5c3-io-v1alpha1-cinder,mutating=false,failurePolicy=fail,sideEffects=None,groups=cinder.openstack.c5c3.io,resources=cinders,verbs=create;update,versions=v1alpha1,name=vcinder.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *CinderWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*Cinder](mgr, &Cinder{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*Cinder]. It sets spec fields to their
// documented defaults when they carry zero values, following the keystone/glance
// non-mutating discipline: optional pointer blocks are only partially filled when
// explicitly present, except spec.logging and spec.api.uwsgi, which are
// materialized so downstream reconciler code never sees a nil pointer.
//
// The one-replica defaults for the scheduler, volume and backup Deployments reach
// the absent-block case only. commonv1.DeploymentSpec.Replicas carries
// +kubebuilder:default=3, which the API server materializes as soon as the CR
// carries a deployment object at all — before any mutating webhook runs — so a
// block that sets anything else arrives here with replicas already at 3, and
// rewriting that to 1 would silently overwrite a value the submitter can see in
// their own manifest. A CR that spells out spec.volume.deployment or
// spec.backup.deployment therefore spells out replicas: 1 alongside it; the CEL
// rules on CinderSpec reject anything else.
func (w *CinderWebhook) Default(_ context.Context, obj *Cinder) error {
	// The three non-API Deployments resolve to one replica rather than the shared
	// default of three. The scheduler is a peer that holds no state and may be
	// raised; the volume and backup services own their storage through a host
	// identity and are pinned at one by the CEL rules. All three are set before
	// the shared Default() runs, which would otherwise fill the absent block with
	// three.
	if obj.Spec.Scheduler.Deployment.Replicas == 0 {
		obj.Spec.Scheduler.Deployment.Replicas = 1
	}
	if obj.Spec.Volume.Deployment.Replicas == 0 {
		obj.Spec.Volume.Deployment.Replicas = 1
	}
	if obj.Spec.Backup.Deployment.Replicas == 0 {
		obj.Spec.Backup.Deployment.Replicas = 1
	}

	// Fill the backup container's resources with the raised memory limit before
	// the shared DeploymentSpec defaults run — Default() would otherwise inject
	// the shared 512Mi limit, which a chunked, compressing backup overruns. Same
	// nil-or-empty condition as the shared method so an explicit user value is
	// never clobbered.
	backup := &obj.Spec.Backup.Deployment
	if backup.Resources == nil ||
		(len(backup.Resources.Requests) == 0 && len(backup.Resources.Limits) == 0) {
		backup.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: commonv1.DefaultMemoryRequest(),
				corev1.ResourceCPU:    commonv1.DefaultCPURequest(),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: DefaultBackupMemoryLimit(),
				corev1.ResourceCPU:    commonv1.DefaultCPULimit(),
			},
		}
	}

	// Shared-type defaults (replicas, remaining container resources) are applied
	// by the commonv1.DeploymentSpec Default method so they cannot drift across
	// operators. All four processes get them: they are sized independently.
	obj.Spec.API.Deployment.Default()
	obj.Spec.Scheduler.Deployment.Default()
	obj.Spec.Volume.Deployment.Default()
	obj.Spec.Backup.Deployment.Default()

	// A rolling update would run the outgoing and the incoming pod at once, and
	// both would hold the same volume state open under the same host identity, so
	// the two single-writer Deployments default to Recreate. An explicit strategy
	// is left alone; the CEL rules keep it at Recreate.
	if obj.Spec.Volume.Deployment.Strategy == nil {
		obj.Spec.Volume.Deployment.Strategy = &appsv1.DeploymentStrategy{
			Type: appsv1.RecreateDeploymentStrategyType,
		}
	}
	if obj.Spec.Backup.Deployment.Strategy == nil {
		obj.Spec.Backup.Deployment.Strategy = &appsv1.DeploymentStrategy{
			Type: appsv1.RecreateDeploymentStrategyType,
		}
	}

	// The API always runs under uWSGI, so the block is materialized and its leaf
	// defaults applied by the commonv1.UWSGISpec Default method, which keeps them
	// from drifting across operators.
	if obj.Spec.API.UWSGI == nil {
		obj.Spec.API.UWSGI = &UWSGISpec{}
	}
	obj.Spec.API.UWSGI.Default()

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

	// ServiceUser identity defaults: the block itself stays optional (a
	// Keystone-free deployment omits it together with spec.keystoneEndpoint), and
	// each field is filled only when empty so an explicit value is never
	// clobbered. A minimal block need only supply the password Secret reference.
	if su := obj.Spec.ServiceUser; su != nil {
		if su.Username == "" {
			su.Username = "cinder"
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
	}

	// Barbican is the only castellan key manager this operator supports, so a
	// present block that names none is filled rather than rejected.
	if obj.Spec.KeyManager != nil && obj.Spec.KeyManager.Type == "" {
		obj.Spec.KeyManager.Type = KeyManagerTypeBarbican
	}
	return nil
}

// ValidateCreate implements admission.Validator[*Cinder].
//
// The metadata.name bound is enforced here rather than in validate(), which
// update shares: the name is immutable, so on update the rule could only ever
// fire against an object a pre-upgrade operator already admitted — and the
// validating webhook also sees the finalizer-removal update reconcileDelete
// issues, so rejecting it would wedge that CR in Terminating with no field left
// to edit to repair it.
func (w *CinderWebhook) ValidateCreate(ctx context.Context, obj *Cinder) (admission.Warnings, error) {
	warnings, createErrs := validateExtraConfigOptions(
		field.NewPath("spec"), obj.Spec.OpenStackRelease, obj.Spec.ExtraConfig, OwnedConfigKeys)
	createErrs = append(createErrs, validateNameLength(obj.Name)...)
	return warnings, w.validate(ctx, obj, createErrs)
}

// validateNameLength bounds metadata.name by the child object with the tightest
// name budget, the "{name}-db-purge" CronJob: the API server rejects a CronJob
// name longer than MaxCronJobNameLength.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateNameLength(name string) field.ErrorList {
	if len(name) <= MaxCinderNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"), name,
		fmt.Sprintf("name must be at most %d characters: the db-purge CronJob appends %q and Kubernetes caps CronJob names at %d characters",
			MaxCinderNameLength, dbPurgeNameSuffix, MaxCronJobNameLength),
	)}
}

// ValidateUpdate implements admission.Validator[*Cinder].
//
// The extraConfig option-catalog check is re-run only when one of its inputs
// changed (extraConfig or spec.openStackRelease). This keeps an unrelated update
// (scaling replicas, say) from retroactively rejecting a CR whose extraConfig was
// accepted at create time but has since been invalidated by a regenerated
// catalog.
//
// spec.targetClusterRef is compared across both revisions here, the webhook-layer
// twin of the two transition CEL rules on CinderSpec.
func (w *CinderWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *Cinder) (admission.Warnings, error) {
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
	warnings = append(warnings, warnDBPurgeRetention(oldObj.Spec.DBPurge, newObj.Spec.DBPurge)...)
	return warnings, w.validate(ctx, newObj, updateErrs)
}

// validate runs all validation rules against the Cinder spec, accumulating every
// violation so users see the full list in one admission response.
// ctx is required for cluster-scoped lookups (PriorityClass validation).
// extra carries the errors accumulated by the caller (the extraConfig
// option-catalog check, on create the metadata.name bound, and on update the
// targetClusterRef immutability check) so they aggregate into the single Invalid
// error alongside the rest.
func (w *CinderWebhook) validate(ctx context.Context, c *Cinder, extra field.ErrorList) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Defense-in-depth image checks alongside the +kubebuilder:validation markers
	// and the XValidation rule on commonv1.ImageSpec.
	allErrs = append(allErrs, validateImage(specPath.Child("image"), c.Spec.Image)...)

	// Defense-in-depth database/cache/messaging mutual-exclusivity and
	// Dynamic-requires-clusterRef checks alongside the
	// +kubebuilder:validation:XValidation CEL rules on the shared commonv1 types,
	// via the shared validators. Messaging is checked unconditionally because the
	// block is required: every volume request travels the bus.
	allErrs = append(allErrs, validation.DatabaseXOR(specPath.Child("database"), &c.Spec.Database)...)
	allErrs = append(allErrs, validation.DynamicCredentialsRequireClusterRef(specPath.Child("database"), &c.Spec.Database)...)
	allErrs = append(allErrs, validation.CacheXOR(specPath.Child("cache"), &c.Spec.Cache)...)
	// spec.cache reaches the verbatim INI renderer the same way the typed fields
	// below do: cache.ResolveServers derives the [coordination] backend_url and
	// [keystone_authtoken] memcached_servers from spec.cache.servers or
	// spec.cache.clusterRef.name. This is the defense-in-depth twin of the items
	// pattern and XValidation rule the shared commonv1.CacheSpec carries, for
	// objects that bypass schema validation.
	allErrs = append(allErrs, validation.CacheNoControlChars(specPath.Child("cache"), &c.Spec.Cache)...)
	allErrs = append(allErrs, validation.MessagingXOR(specPath.Child("messaging"), &c.Spec.Messaging)...)
	allErrs = append(allErrs, validation.SecretStoreRef(specPath.Child("secretStoreRef"), c.Spec.SecretStoreRef)...)
	allErrs = append(allErrs, validation.TargetClusterRef(specPath.Child("targetClusterRef"), c.Spec.TargetClusterRef)...)

	// The four process Deployments carry the same pod-level knobs and are
	// validated by the same rules; only the API additionally runs under uWSGI.
	// Each block also carries the pod selector of the Deployment it configures,
	// which is what a topology-spread constraint on that block has to name.
	//
	// The volume block carries no selector: the operator projects one Deployment
	// per attached backend, each narrowed by its own "volume-<backend>"
	// component, so no matchLabels map is the selector of all of them, and a
	// nil one rejects the constraint outright rather than measuring it against a
	// wider selector than the block controls.
	for _, block := range []struct {
		path       *field.Path
		deployment *commonv1.DeploymentSpec
		selector   map[string]string
	}{
		{
			specPath.Child("api", "deployment"), &c.Spec.API.Deployment,
			naming.APISelectorLabels(cinderAppName, c.Name),
		},
		{
			specPath.Child("scheduler", "deployment"), &c.Spec.Scheduler.Deployment,
			componentSelectorLabels(c.Name, componentScheduler),
		},
		{specPath.Child("volume", "deployment"), &c.Spec.Volume.Deployment, nil},
		{
			specPath.Child("backup", "deployment"), &c.Spec.Backup.Deployment,
			componentSelectorLabels(c.Name, componentBackup),
		},
	} {
		allErrs = append(allErrs, w.validateDeploymentBlock(ctx, block.path, block.deployment, block.selector)...)
	}
	allErrs = append(allErrs, validateUWSGIHarakiri(
		specPath.Child("api", "uwsgi"), c.Spec.API.UWSGI, &c.Spec.API.Deployment)...)

	// Defense-in-depth twins of the four single-writer CEL rules on CinderSpec.
	allErrs = append(allErrs, validateSingletonDeployment(
		specPath.Child("volume", "deployment"), &c.Spec.Volume.Deployment)...)
	allErrs = append(allErrs, validateSingletonDeployment(
		specPath.Child("backup", "deployment"), &c.Spec.Backup.Deployment)...)

	// Every endpoint is handed to a client library verbatim, so an unparseable
	// URL or a missing host would surface as a connection failure at runtime
	// rather than at admission. Each is optional here (a Keystone-free deployment
	// omits the first two, an image-less one the third), so only a non-empty value
	// is measured. The schema-layer twin is the ^https?:// pattern each field
	// carries.
	for _, endpoint := range []struct {
		path  *field.Path
		value string
	}{
		{specPath.Child("keystoneEndpoint"), c.Spec.KeystoneEndpoint},
		{specPath.Child("keystonePublicEndpoint"), c.Spec.KeystonePublicEndpoint},
		{specPath.Child("glanceEndpoint"), c.Spec.GlanceEndpoint},
	} {
		if endpoint.value != "" {
			allErrs = append(allErrs, validateEndpointURL(endpoint.path, endpoint.value)...)
		}
	}

	// The endpoint names who to authenticate against and the service user carries
	// the credentials, so either alone renders a [keystone_authtoken] section the
	// service cannot use. The message repeats the CEL rule's text so both gates
	// say the same thing.
	if (c.Spec.KeystoneEndpoint != "") != (c.Spec.ServiceUser != nil) {
		const pairMsg = "keystoneEndpoint and serviceUser must be set together"
		if c.Spec.ServiceUser == nil {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("serviceUser"), nil, pairMsg))
		} else {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("keystoneEndpoint"), c.Spec.KeystoneEndpoint, pairMsg))
		}
	}

	// External exposure requires the Keystone integration. Without it the
	// rendered pipeline is auth_strategy = noauth, which reads the project from
	// the request URL and validates no token, so an HTTPRoute in front of it
	// serves every volume operation unauthenticated. The message repeats the CEL
	// rule's text so both gates say the same thing.
	if c.Spec.Gateway != nil && c.Spec.KeystoneEndpoint == "" {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("gateway"), c.Spec.Gateway.Hostname,
			"gateway requires keystoneEndpoint: without it the API renders auth_strategy = noauth, "+
				"so a Gateway would serve every volume operation unauthenticated",
		))
	}

	if c.Spec.KeyManager != nil {
		keyManagerPath := specPath.Child("keyManager")
		if c.Spec.KeystoneEndpoint == "" {
			allErrs = append(allErrs, field.Invalid(
				keyManagerPath, c.Spec.KeyManager.Type,
				"keyManager requires keystoneEndpoint (castellan authenticates against Keystone)",
			))
		}
		// Defense-in-depth union check alongside the CEL rule on KeyManagerSpec:
		// exactly one key-manager block, matching spec.keyManager.type.
		if (c.Spec.KeyManager.Type == KeyManagerTypeBarbican) != (c.Spec.KeyManager.Barbican != nil) {
			allErrs = append(allErrs, field.Invalid(
				keyManagerPath.Child("barbican"), c.Spec.KeyManager.Type,
				"exactly one key-manager block matching spec.keyManager.type must be set (type Barbican requires spec.keyManager.barbican)",
			))
		}
		if c.Spec.KeyManager.Barbican != nil {
			allErrs = append(allErrs, validateEndpointURL(
				keyManagerPath.Child("barbican", "endpoint"), c.Spec.KeyManager.Barbican.Endpoint)...)
		}
	}

	// Both halves address the image-volume cache's own volumes, and cinder passes
	// them to the volume API without resolving them through Keystone, so an empty
	// one creates those volumes under nothing. Defense in depth behind the
	// MinLength markers on InternalTenantSpec.
	if c.Spec.InternalTenant != nil {
		internalTenantPath := specPath.Child("internalTenant")
		if c.Spec.InternalTenant.ProjectID == "" {
			allErrs = append(allErrs, field.Required(
				internalTenantPath.Child("projectID"),
				"projectID must be set when spec.internalTenant is configured",
			))
		}
		if c.Spec.InternalTenant.UserID == "" {
			allErrs = append(allErrs, field.Required(
				internalTenantPath.Child("userID"),
				"userID must be set when spec.internalTenant is configured",
			))
		}
	}

	allErrs = append(allErrs, validateLogging(specPath.Child("logging"), c.Spec.Logging, "cinder.conf")...)

	// Defense-in-depth autoscaling validation alongside kubebuilder markers and
	// CEL rules. The HPA targets the API Deployment alone, so every cross-field
	// rule below reads spec.api.deployment.replicas.
	if c.Spec.Autoscaling != nil {
		autoscalingPath := specPath.Child("autoscaling")
		if c.Spec.Autoscaling.MaxReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				c.Spec.Autoscaling.MaxReplicas,
				"maxReplicas must be at least 1",
			))
		}
		if c.Spec.Autoscaling.MinReplicas != nil && *c.Spec.Autoscaling.MinReplicas < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*c.Spec.Autoscaling.MinReplicas,
				"minReplicas must be at least 1",
			))
		}
		if c.Spec.Autoscaling.MinReplicas != nil && *c.Spec.Autoscaling.MinReplicas > c.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("minReplicas"),
				*c.Spec.Autoscaling.MinReplicas,
				"minReplicas must not exceed maxReplicas",
			))
		}
		// When minReplicas is unset, the reconciler defaults it to the API
		// Deployment's replica count. Reject configurations where the implicit
		// default would exceed maxReplicas, which would produce an HPA rejected by
		// the API server.
		if c.Spec.Autoscaling.MinReplicas == nil && c.Spec.API.Deployment.Replicas > c.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				c.Spec.Autoscaling.MaxReplicas,
				fmt.Sprintf("maxReplicas must be >= spec.api.deployment.replicas (%d) when minReplicas is not set, because minReplicas defaults to spec.api.deployment.replicas", c.Spec.API.Deployment.Replicas),
			))
		}
		// Defense-in-depth bounds checks for utilization targets alongside
		// +kubebuilder:validation:Minimum=1 / Maximum=100 markers.
		if c.Spec.Autoscaling.TargetCPUUtilization != nil && (*c.Spec.Autoscaling.TargetCPUUtilization < 1 || *c.Spec.Autoscaling.TargetCPUUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetCPUUtilization"),
				*c.Spec.Autoscaling.TargetCPUUtilization,
				"targetCPUUtilization must be between 1 and 100",
			))
		}
		if c.Spec.Autoscaling.TargetMemoryUtilization != nil && (*c.Spec.Autoscaling.TargetMemoryUtilization < 1 || *c.Spec.Autoscaling.TargetMemoryUtilization > 100) {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetMemoryUtilization"),
				*c.Spec.Autoscaling.TargetMemoryUtilization,
				"targetMemoryUtilization must be between 1 and 100",
			))
		}
		if c.Spec.Autoscaling.TargetCPUUtilization == nil && c.Spec.Autoscaling.TargetMemoryUtilization == nil {
			allErrs = append(allErrs, field.Required(
				autoscalingPath,
				"at least one of targetCPUUtilization or targetMemoryUtilization must be set",
			))
		}
	}

	// Defense-in-depth networkPolicy ingress check alongside the
	// +kubebuilder:validation:XValidation CEL rule on NetworkPolicySpec.
	if c.Spec.NetworkPolicy != nil && len(c.Spec.NetworkPolicy.Ingress) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("networkPolicy", "ingress"),
			"at least one ingress source must be specified",
		))
	}

	// Defense-in-depth gateway validation alongside the
	// +kubebuilder:validation:MinLength=1 markers on GatewaySpec.Hostname and
	// GatewayParentRefSpec.Name.
	if c.Spec.Gateway != nil {
		gatewayPath := specPath.Child("gateway")
		if c.Spec.Gateway.Hostname == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("hostname"),
				"hostname must be set when spec.gateway is configured",
			))
		}
		if c.Spec.Gateway.ParentRef.Name == "" {
			allErrs = append(allErrs, field.Required(
				gatewayPath.Child("parentRef", "name"),
				"parentRef.name must be set when spec.gateway is configured",
			))
		}
	}

	allErrs = append(allErrs, validateDBPurge(specPath.Child("dbPurge"), c.Spec.DBPurge)...)
	allErrs = append(allErrs, validateExtraConfigShape(specPath, c.Spec.ExtraConfig, OwnedConfigKeys)...)

	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Cinder"},
			c.Name,
			allErrs,
		)
	}
	return nil
}

// validateDeploymentBlock runs the pod-level rules every Cinder Deployment block
// shares: the replica floor, the graceful-termination arithmetic, the strategy
// sanity check, the request/limit ordering, and the two cluster-scoped lookups.
// selector is the pod selector of the Deployment this block configures, which a
// topology-spread constraint on it has to name exactly. It is nil for a block
// that configures more than one Deployment, where such a constraint has no
// selector to name and is rejected instead.
//
// ctx is required for the PriorityClass lookup, which is skipped when no reader
// is injected (a programmatically constructed webhook), mirroring the shared
// validator's own behavior.
func (w *CinderWebhook) validateDeploymentBlock(
	ctx context.Context,
	fldPath *field.Path,
	d *commonv1.DeploymentSpec,
	selector map[string]string,
) field.ErrorList {
	var errs field.ErrorList

	// Defense-in-depth replicas check alongside the
	// +kubebuilder:validation:Minimum=1 marker. Every block carries a count by the
	// time it reaches validation: the schema default fills a present block and the
	// defaulting webhook an absent one.
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

	// Validate that resource requests do not exceed limits.
	if d.Resources != nil && d.Resources.Limits != nil {
		for resourceName, request := range d.Resources.Requests {
			if limit, hasLimit := d.Resources.Limits[resourceName]; hasLimit && request.Cmp(limit) > 0 {
				errs = append(errs, field.Invalid(
					fldPath.Child("resources", "requests", string(resourceName)),
					request.String(),
					fmt.Sprintf("%s request must not exceed limit (%s)", resourceName, limit.String()),
				))
			}
		}
	}

	// Validate that priorityClassName references an existing
	// scheduling.k8s.io/v1 PriorityClass (shared validator; catches typos at
	// admission time, skipped when no lookup client is injected).
	if d.PriorityClassName != nil {
		errs = append(errs, validation.PriorityClassExists(ctx, w.Client,
			fldPath.Child("priorityClassName"), *d.PriorityClassName)...)
	}

	// Validate that custom TopologySpreadConstraints use the correct
	// LabelSelector matching the Deployment's selector labels.
	//
	// A block without a selector configures one Deployment per attached backend,
	// each pinned to a single replica and selected by its own component: every
	// matchLabels map that selects them all selects the API, scheduler and backup
	// pods too, so the scheduler would count pods this block does not control and
	// could leave a volume pod Pending over a skew none of its own pods produced.
	// The empty slice is the exception there: it names no selector and only
	// switches the operator's injected defaults off.
	if selector == nil {
		if len(d.TopologySpreadConstraints) > 0 {
			errs = append(errs, field.Forbidden(
				fldPath.Child("topologySpreadConstraints"),
				"topologySpreadConstraints is not supported here: the operator projects one "+
					"cinder-volume Deployment per attached backend from this block, each with a "+
					"single replica and its own selector, so a constraint set here would spread "+
					"each volume pod against the pods of the other components. An empty list, "+
					"which only switches the injected defaults off, is accepted",
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

// componentSelectorLabels returns the pod selector of the Deployment of one
// non-API component: the shared selector labels narrowed by that component. It
// is the api-package twin of the controller helper of the same name.
func componentSelectorLabels(cinderName, component string) map[string]string {
	labels := naming.SelectorLabels(cinderAppName, cinderName)
	labels[naming.LabelKeyComponent] = component
	return labels
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

// validateUWSGIHarakiri requires the worst-case uWSGI per-request kill to fit
// inside the envelope between preStop sleep completion and SIGKILL, so a request
// harakiri aborts still has time to be answered before the kubelet kills the pod.
// It reaches the API Deployment alone: the scheduler, volume and backup processes
// run no application server. It is nil-safe on both arguments' optional halves.
func validateUWSGIHarakiri(fldPath *field.Path, u *commonv1.UWSGISpec, d *commonv1.DeploymentSpec) field.ErrorList {
	if u == nil || u.Harakiri == nil {
		return nil
	}
	grace, preStop := resolveDrainWindow(d)
	drain := grace - preStop
	harakiri := int64(*u.Harakiri)
	if harakiri < drain {
		return nil
	}
	return field.ErrorList{field.Invalid(
		fldPath.Child("harakiri"), *u.Harakiri,
		fmt.Sprintf("harakiri (%d) must be strictly less than terminationGracePeriodSeconds - preStopSleepSeconds (%d)", harakiri, drain),
	)}
}

// validateSingletonDeployment is the webhook twin of the four single-writer CEL
// rules on CinderSpec, which pin the volume and backup Deployments at one replica
// on the Recreate strategy. Both services own their storage through a host
// identity rather than through a lock, so a second process under the same
// identity — a second replica, or the surge pod of a rolling update overlapping
// the outgoing one — has the same volume state open twice. A zero replica count
// is the absent block, which the defaulting webhook resolves to one, and an
// unset strategy resolves to Recreate the same way; neither is a violation. The
// messages repeat the CEL rules' text so both gates say the same thing.
func validateSingletonDeployment(fldPath *field.Path, d *commonv1.DeploymentSpec) field.ErrorList {
	var errs field.ErrorList
	if d.Replicas != 0 && d.Replicas != 1 {
		errs = append(errs, field.Invalid(
			fldPath.Child("replicas"), d.Replicas,
			"volume/backup deployments run exactly one replica (the NFS drivers refuse active/active)",
		))
	}
	if d.Strategy != nil && d.Strategy.Type != "" && d.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		errs = append(errs, field.Invalid(
			fldPath.Child("strategy", "type"), d.Strategy.Type,
			"volume/backup deployments must use the Recreate strategy",
		))
	}
	return errs
}

// validateEndpointURL checks that a non-empty endpoint parses cleanly, uses an
// http(s) scheme, and carries a host — the same shape the sibling operators
// enforce on their endpoints. It is applied to every URL field on CinderSpec.
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

// validateDBPurge mirrors the spec.dbPurge schema as defense in depth behind the
// CRD layer — the Minimum marker on retentionDays — and carries the one check
// with no schema counterpart: the cron grammar, which DBPurgeSpec.Schedule
// deliberately leaves to the webhook. It is nil-safe, and so is each half: an
// unset field carries nothing to validate and is resolved to the operator default
// at reconcile time.
func validateDBPurge(fldPath *field.Path, p *DBPurgeSpec) field.ErrorList {
	if p == nil {
		return nil
	}
	var errs field.ErrorList

	if p.RetentionDays != nil && *p.RetentionDays < 1 {
		errs = append(errs, field.Invalid(
			fldPath.Child("retentionDays"), *p.RetentionDays, "retentionDays must be at least 1",
		))
	}
	if p.Schedule != "" {
		errs = append(errs, validation.CronSchedule(fldPath.Child("schedule"), p.Schedule)...)
	}
	return errs
}

// warnDBPurgeRetention surfaces a retention window that an update shortens.
// Unlike every other knob on the CR, the effect is immediate and irreversible: at
// the next firing the purge hard-deletes every row that fell out of the window,
// and nothing brings those rows back. A typo (3 for 30) is indistinguishable from
// an intended change at admission time, so the reduction is echoed back to
// whoever made it. It stays a warning rather than a rejection because shortening
// the window is a legitimate operational choice.
func warnDBPurgeRetention(oldPurge, newPurge *DBPurgeSpec) admission.Warnings {
	oldDays, newDays := effectiveRetentionDays(oldPurge), effectiveRetentionDays(newPurge)
	if newDays >= oldDays {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.dbPurge.retentionDays reduced %d → %d: the next purge run hard-deletes the rows soft-deleted between %d and %d days ago, which cannot be undone",
		oldDays, newDays, newDays, oldDays,
	)}
}

// effectiveRetentionDays resolves the retention a spec.dbPurge block runs with,
// mirroring the reconcile-time resolver so a block that leaves the field unset
// compares as the operator default rather than as zero — otherwise dropping
// spec.dbPurge entirely would read as a reduction to nothing.
func effectiveRetentionDays(p *DBPurgeSpec) int32 {
	if p == nil || p.RetentionDays == nil {
		return DefaultDBPurgeRetentionDays
	}
	return *p.RetentionDays
}
