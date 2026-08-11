// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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

const (
	// DefaultEventletWorkers is the eventlet [DEFAULT] workers count materialized
	// when spec.apiServer.workers is nil below release 2026.1. It is the eventlet
	// analog of commonv1.DefaultUWSGIProcesses: without it the eventlet glance-api
	// server falls back to its own default of one worker per host CPU, which
	// ignores the pod's CPU limit and overruns the memory limit under load.
	// Pinning it keeps the worker count — and thus the memory footprint —
	// deterministic, matching keystone's bounded process count.
	DefaultEventletWorkers int32 = 2
)

// Database-purge defaults for the recurring glance-manage purge CronJob, which
// hard-deletes the soft-deleted image rows and the import-task rows glance
// itself never removes.
//
// They are consumed by the reconcile-time resolver (effectiveDBPurge in the
// controller package) and deliberately NOT applied by the defaulting webhook: a
// nil or partial spec.dbPurge block keeps tracking these operator defaults
// across upgrades instead of freezing today's values into the stored CR, the
// same reasoning as the import-filtering defaults documented right below.
const (
	// DefaultDBPurgeRetentionDays is how long a soft-deleted row survives before
	// the purge hard-deletes it, resolved when spec.dbPurge.retentionDays is
	// unset. A month leaves room to notice and reverse an accidental image
	// deletion at the storage layer before the row backing it disappears.
	DefaultDBPurgeRetentionDays int32 = 30
	// DefaultDBPurgeSchedule runs the purge daily at 00:01, resolved when
	// spec.dbPurge.schedule is empty. The off-the-hour minute keeps it clear of
	// the top-of-midnight slot the sibling rotation CronJobs occupy.
	DefaultDBPurgeSchedule = "1 0 * * *"
)

// defaultStagingSizeLimit bounds each of the two node-local scratch emptyDirs
// the API pod mounts for image imports (staging and tasks-work), so one
// oversized import cannot fill the node's disk. Ten gibibytes leaves ample room
// for the stock cloud images while staying well inside a normal node filesystem,
// even with both emptyDirs at the cap.
//
// It is consumed by the reconcile-time resolver (effectiveStagingSizeLimit in
// the controller package) and deliberately NOT applied by the defaulting
// webhook: a nil spec.staging block, or one leaving sizeLimit unset, keeps
// tracking this operator default across upgrades instead of freezing today's
// value into the stored CR, the same reasoning as the db-purge defaults above.
//
// It is a var rather than a const because resource.Quantity is a struct, and it
// is exposed only through the accessor below, which returns a copy so no caller
// can mutate the shared default — the idiom of the commonv1 resource defaults.
var defaultStagingSizeLimit = resource.MustParse("10Gi")

// DefaultStagingSizeLimit returns a copy of the size limit stamped on the
// staging and tasks-work emptyDirs when spec.staging.sizeLimit is unset.
func DefaultStagingSizeLimit() resource.Quantity { return defaultStagingSizeLimit.DeepCopy() }

// defaultImageCacheSizeLimit bounds the per-replica image-cache emptyDir mounted
// when spec.imageCache is set. Ten gibibytes matches the staging bound above: it
// holds a working set of the stock cloud images without letting one replica's
// cache dominate a normal node filesystem, and reusing the same number keeps a
// glance-api pod's node-local footprint expressible as a small multiple of one
// value.
//
// It is consumed by the render-time resolver and deliberately NOT applied by the
// defaulting webhook: a spec.imageCache block leaving sizeLimit unset keeps
// tracking this operator default across upgrades instead of freezing today's
// value into the stored CR, the same reasoning as the staging and db-purge
// defaults above.
//
// It is a var rather than a const for the same reason as
// defaultStagingSizeLimit — resource.Quantity is a struct — and is exposed only
// through the accessor below, which returns a copy.
var defaultImageCacheSizeLimit = resource.MustParse("10Gi")

// DefaultImageCacheSizeLimit returns a copy of the size limit stamped on the
// image-cache emptyDir when spec.imageCache.sizeLimit is unset. The rendered
// image_cache_max_size is derived from it, not equal to it — see ImageCacheSpec.
func DefaultImageCacheSizeLimit() resource.Quantity { return defaultImageCacheSizeLimit.DeepCopy() }

// DefaultImageCacheMaintenanceInterval is how often the cache-maintenance
// sidecar runs glance-cache-pruner and glance-cache-cleaner when
// spec.imageCache.maintenanceInterval is unset. Five minutes bounds how long the
// cache may sit above image_cache_max_size after a burst of downloads while
// leaving the loop cheap enough to ignore. Like the size limit above it is
// resolved at render time rather than by the defaulting webhook.
const DefaultImageCacheMaintenanceInterval = 5 * time.Minute

// imageCacheFilterName is the api-paste.ini filter the operator injects while
// spec.imageCache is set. It is glance's own name for that filter, so it is not
// the operator's to rename; the webhook instead reserves it against
// spec.middleware for as long as the cache is enabled.
const imageCacheFilterName = "cache"

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
	// MaxGlanceNameLength is what those two leave for metadata.name.
	MaxGlanceNameLength = MaxCronJobNameLength - len(dbPurgeNameSuffix)
)

// Import-filtering defaults for the web-download image-import method, narrowing
// glance's own permissive defaults (schemes [http, https], ports [80, 443], no
// host denylist). They are variables rather than constants only because Go has
// no constant slices — treat them as read-only.
//
// They are consumed by the render-time resolver (effectiveImportFiltering in the
// controller package) and deliberately NOT applied by the defaulting webhook: a
// nil or partial spec.importFiltering block keeps tracking these operator
// defaults across upgrades instead of freezing today's values into the stored
// CR, the same reasoning as EffectiveKeystonePublicEndpoint.
var (
	// DefaultImportAllowedSchemes pins web-download imports to HTTPS, so an
	// import URI cannot downgrade the transport to plaintext.
	DefaultImportAllowedSchemes = []string{"https"}
	// DefaultImportAllowedPorts pins web-download imports to the HTTPS port,
	// so an import URI cannot reach an arbitrary service port.
	DefaultImportAllowedPorts = []int32{443}
	// DefaultImportDisallowedHosts blocks the hosts an import URI must never
	// reach: loopback, the link-local cloud metadata endpoint, and the
	// in-cluster API server. Glance matches hosts as literal strings — no CIDR
	// ranges, no wildcards, no name resolution — so each spelling is a separate
	// entry. A pod's resolv.conf carries the search list
	// "<ns>.svc.cluster.local svc.cluster.local cluster.local" at ndots:5, which
	// is why all four resolvable spellings of the API server Service are listed
	// and not just the fully-qualified one.
	//
	// Literal matching cannot be exhaustive: a trailing-dot FQDN, an IPv6-mapped
	// form, an integer-encoded address, or the Service's raw ClusterIP reaches the
	// same endpoint under a name no denylist can enumerate — enumerating a few
	// more spellings would only imply a completeness this list cannot have. The
	// allow-side pin (DefaultImportAllowedSchemes and DefaultImportAllowedPorts)
	// is the primary control, which is why a CR widening it is not silent:
	// WarnImportFiltering flags the widening and names the two controls that can
	// still hold — an allowedHosts pin to the deployment's mirrors, or an egress
	// restriction via spec.networkPolicy.additionalEgress.
	DefaultImportDisallowedHosts = []string{
		"localhost",
		"127.0.0.1",
		// 0.0.0.0 routes to loopback on Linux, so it is a loopback spelling too.
		"0.0.0.0",
		"::1",
		"169.254.169.254",
		"kubernetes",
		"kubernetes.default",
		"kubernetes.default.svc",
		"kubernetes.default.svc.cluster.local",
	}
)

// DefaultImportConversionOutputFormat is the disk format the image_conversion
// plugin rewrites an imported image into when
// spec.importPlugins.conversion.outputFormat is empty. It is glance's own
// default and the format a Ceph/RBD store needs: RBD clones copy-on-write from
// raw only, so any other format turns every boot from that image into a full
// flatten.
//
// Like the import-filtering defaults above it is consumed by the render-time
// resolver in the controller package and deliberately NOT applied by the
// defaulting webhook, so an unset field keeps tracking it across upgrades
// instead of freezing today's value into the stored CR.
const DefaultImportConversionOutputFormat = "raw"

// DefaultImportInjectIgnoreUserRoles are the Keystone roles the
// inject_image_metadata plugin skips when
// spec.importPlugins.injectMetadata.ignoreUserRoles is nil. It is glance's own
// default: an admin import is taken to know which properties it wants, so the
// injection stays out of its way. It is a variable rather than a constant only
// because Go has no constant slices — treat it as read-only, and note that an
// explicitly empty list in the CR is honored verbatim and drops the exemption.
var DefaultImportInjectIgnoreUserRoles = []string{"admin"}

// Messages and bounds shared between the spec.importFiltering CRD schema and the
// webhook mirror below. The markers on ImportFilteringSpec cannot reference a Go
// constant, so the same literals appear there and must stay byte-identical —
// both the webhook unit tests and the invalid-CR e2e corpus assert the text.
const (
	importFilteringSchemesExclusiveMsg = "allowedSchemes and disallowedSchemes are mutually exclusive: glance ignores the deny-list when the allow-list is non-empty"
	importFilteringHostsExclusiveMsg   = "allowedHosts and disallowedHosts are mutually exclusive: glance ignores the deny-list when the allow-list is non-empty"
	importFilteringPortsExclusiveMsg   = "allowedPorts and disallowedPorts are mutually exclusive: glance ignores the deny-list when the allow-list is non-empty"

	// maxImportFilteringItems mirrors the MaxItems marker carried by every
	// spec.importFiltering list.
	maxImportFilteringItems = 64
	// maxImportFilteringHostLength mirrors the items:MaxLength marker on the
	// host lists (the DNS name length limit).
	maxImportFilteringHostLength = 253
)

// Bounds the spec.importPlugins webhook mirror shares with the CRD schema. As
// with the importFiltering bounds, the markers on ImportInjectMetadataSpec
// cannot reference a Go constant, so the same literals appear there and must
// stay in sync.
const (
	// maxImportInjectEntries mirrors the MaxProperties marker on
	// injectMetadata.properties and the MaxItems marker on
	// injectMetadata.ignoreUserRoles, which share the bound.
	maxImportInjectEntries = 64
	// maxImportInjectStringLength mirrors the items:MaxLength marker on
	// injectMetadata.ignoreUserRoles and bounds both halves of an injected
	// property by the same measure. A map key carries no marker of its own, so
	// the schema counterpart for the pair is the CEL rule on
	// injectMetadata.properties, which repeats this literal.
	//
	// Both schema counterparts count CODE POINTS — CEL size() over a string and
	// the MaxLength keyword alike — so every mirror below measures with
	// utf8.RuneCountInString rather than len(). Counting bytes would make the
	// webhook the stricter half and reject a 90-character CJK value the schema
	// admits, under a message naming a limit that value is nowhere near.
	maxImportInjectStringLength = 255
)

// importConversionOutputFormats are the disk formats
// spec.importPlugins.conversion.outputFormat accepts, mirroring the Enum marker
// on the field. They are glance's own choices for [image_conversion]
// output_format; a value outside them makes qemu-img fail on every import
// rather than at admission.
var importConversionOutputFormats = []string{"qcow2", "raw", "vmdk"}

// Glance-specific container memory defaults, replacing the shared 256Mi/512Mi
// baseline that keystone and horizon fit in. The glance-api container carries
// the S3 store driver (boto3/botocore), which raises both the per-process
// import footprint and the per-request allocation churn: with the bounded two
// workers the container already idles near 360Mi and, under concurrent image
// traffic, overruns a 512Mi limit within a minute — an OOM-kill crash loop the
// gateway surfaces as waves of 503s. CPU keeps the shared defaults.
var (
	defaultGlanceMemoryRequest = resource.MustParse("512Mi")
	defaultGlanceMemoryLimit   = resource.MustParse("1Gi")
)

// GlanceWebhook implements defaulting and validation webhooks for the Glance
// CRD. Client is injected at startup for cluster-scoped resource lookups (e.g.
// PriorityClass validation). Production wiring injects mgr.GetAPIReader() — a
// direct, uncached reader — so admission never rejects a just-created object
// from a stale informer cache and no lazy informer start happens inside the
// webhook timeout.
// +kubebuilder:object:generate=false
type GlanceWebhook struct {
	commonwebhook.NoopDeleteValidator[*Glance]

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
	// Fill spec.deployment.resources with the glance-specific memory defaults
	// before the shared DeploymentSpec defaults run — Deployment.Default()
	// would otherwise inject the shared 256Mi/512Mi baseline, which the
	// S3-backed glance-api overruns under load. Same nil-or-empty condition as
	// the shared method so an explicit user value is never clobbered.
	if obj.Spec.Deployment.Resources == nil ||
		(len(obj.Spec.Deployment.Resources.Requests) == 0 && len(obj.Spec.Deployment.Resources.Limits) == 0) {
		obj.Spec.Deployment.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: defaultGlanceMemoryRequest.DeepCopy(),
				corev1.ResourceCPU:    commonv1.DefaultCPURequest(),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: defaultGlanceMemoryLimit.DeepCopy(),
				corev1.ResourceCPU:    commonv1.DefaultCPULimit(),
			},
		}
	}
	// Shared-type defaults (replicas, remaining container resources) are
	// applied by the commonv1.DeploymentSpec Default method so they cannot
	// drift across operators.
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
	// non-nil. The leaf defaults are applied by the commonv1.UWSGISpec Default
	// method so they cannot drift across operators; a nil pointer is a no-op
	// there — the reconciler uses hardcoded defaults for the active launch mode.
	if obj.Spec.APIServer != nil {
		obj.Spec.APIServer.UWSGI.Default()
	}
	return nil
}

// ValidateCreate implements admission.Validator[*Glance].
//
// The metadata.name bound is enforced here rather than in validate(), which
// update shares: the name is immutable, so on update the rule could only ever
// fire against an object a pre-upgrade operator already admitted — and the
// validating webhook also sees the finalizer-removal update reconcileDelete
// issues, so rejecting it would wedge that CR in Terminating with no field left
// to edit to repair it.
func (w *GlanceWebhook) ValidateCreate(ctx context.Context, obj *Glance) (admission.Warnings, error) {
	catalogWarnings, createErrs := validateExtraConfigOptions(field.NewPath("spec"), obj)
	createErrs = append(createErrs, validateNameLength(obj.Name)...)
	warnings := append(warnInertLaunchModeKnobs(obj), catalogWarnings...)
	warnings = append(warnings, WarnImportFiltering(
		field.NewPath("spec", "importFiltering"), obj.Spec.ImportFiltering,
	)...)
	return warnings, w.validate(ctx, obj, createErrs)
}

// validateNameLength bounds metadata.name by the child object with the tightest
// name budget, the "{name}-db-purge" CronJob: the API server rejects a CronJob
// name longer than MaxCronJobNameLength. The controller stays total for a name
// that predates this bound — it collapses the overflowing tail onto a hash — but
// that form is opaque, so a CR being authored now is held to the plain one.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateNameLength(name string) field.ErrorList {
	if len(name) <= MaxGlanceNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"), name,
		fmt.Sprintf("name must be at most %d characters: the db-purge CronJob appends %q and Kubernetes caps CronJob names at %d characters",
			MaxGlanceNameLength, dbPurgeNameSuffix, MaxCronJobNameLength),
	)}
}

// ValidateUpdate implements admission.Validator[*Glance].
//
// The extraConfig option-catalog check is re-run only when one of its inputs
// changed (extraConfig, the plugin config-section set, or spec.openStackRelease).
// This keeps an unrelated update — for example scaling replicas — from
// retroactively rejecting a CR whose extraConfig was accepted at create time but
// has since been invalidated by a regenerated catalog.
//
// spec.targetClusterRef is compared across both revisions here, the webhook-layer
// twin of the two transition CEL rules on GlanceSpec.
func (w *GlanceWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *Glance) (admission.Warnings, error) {
	var catalogWarnings admission.Warnings
	var updateErrs field.ErrorList
	if extraConfigCatalogInputsChanged(oldObj, newObj) {
		catalogWarnings, updateErrs = validateExtraConfigOptions(field.NewPath("spec"), newObj)
	}
	updateErrs = append(updateErrs, validation.TargetClusterRefImmutable(
		field.NewPath("spec", "targetClusterRef"),
		oldObj.Spec.TargetClusterRef,
		newObj.Spec.TargetClusterRef,
	)...)
	warnings := append(warnInertLaunchModeKnobs(newObj), catalogWarnings...)
	warnings = append(warnings, WarnImportFiltering(
		field.NewPath("spec", "importFiltering"), newObj.Spec.ImportFiltering,
	)...)
	warnings = append(warnings, warnDBPurgeRetention(oldObj.Spec.DBPurge, newObj.Spec.DBPurge)...)
	return warnings, w.validate(ctx, newObj, updateErrs)
}

// validate runs all validation rules against the Glance spec, accumulating
// every violation so users see the full list in one admission response.
// ctx is required for cluster-scoped lookups (PriorityClass validation).
// extra carries errors accumulated by the caller (the extraConfig option-catalog
// check, on create the metadata.name bound, and on update the targetClusterRef
// immutability check) so they aggregate into the single Invalid error alongside
// the rest.
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
	// spec.cache reaches the verbatim INI renderer the same way the typed fields
	// below do: cache.ResolveServers derives [keystone_authtoken].memcached_servers
	// from spec.cache.servers or spec.cache.clusterRef.name. This is the
	// defense-in-depth twin of the items pattern and XValidation rule the shared
	// commonv1.CacheSpec carries, for objects that bypass schema validation.
	allErrs = append(allErrs, validation.CacheNoControlChars(specPath.Child("cache"), &g.Spec.Cache)...)
	allErrs = append(allErrs, validation.SecretStoreRef(specPath.Child("secretStoreRef"), g.Spec.SecretStoreRef)...)
	allErrs = append(allErrs, validation.TargetClusterRef(specPath.Child("targetClusterRef"), g.Spec.TargetClusterRef)...)

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

	// Defense-in-depth importFiltering validation alongside the three
	// XValidation CEL rules and the item markers on ImportFilteringSpec.
	allErrs = append(allErrs, ValidateImportFiltering(specPath.Child("importFiltering"), g.Spec.ImportFiltering)...)

	// Defense-in-depth importPlugins validation alongside the enum and the
	// count/length markers on ImportPluginsSpec, plus the property-name rules no
	// CRD marker can reach — see ValidateImportPlugins.
	allErrs = append(allErrs, ValidateImportPlugins(specPath.Child("importPlugins"), g.Spec.ImportPlugins)...)

	// The decompression plugin turns spec.staging.sizeLimit into the only bound
	// on an unbounded expansion, so enabling it requires that bound to be a
	// choice rather than an inherited default — see
	// ValidateImportDecompressionStaging.
	allErrs = append(allErrs, ValidateImportDecompressionStaging(specPath, g.Spec.ImportPlugins, g.Spec.Staging)...)

	// Defense-in-depth dbPurge validation alongside the Minimum marker on
	// DBPurgeSpec.RetentionDays, plus the cron-grammar check that has no schema
	// counterpart.
	allErrs = append(allErrs, validateDBPurge(specPath.Child("dbPurge"), g.Spec.DBPurge)...)

	// staging validation. Unlike its neighbours this one has no schema
	// counterpart at all — see ValidateStaging.
	allErrs = append(allErrs, ValidateStaging(specPath.Child("staging"), g.Spec.Staging)...)

	// imageCache validation. Like staging it has no schema counterpart at all —
	// see ValidateImageCache.
	allErrs = append(allErrs, ValidateImageCache(specPath.Child("imageCache"), g.Spec.ImageCache)...)

	// The `cache` filter name belongs to the operator while the cache is on: it
	// injects that paste filter into the pipeline and renders its [filter:cache]
	// section, so a user middleware of the same name is not an addition but a
	// collision — one of the two definitions wins and the pipeline silently ends
	// up with wiring nobody wrote. With spec.imageCache nil the operator injects
	// nothing and the name stays free, which is what a CR carrying such a
	// middleware today keeps relying on.
	if g.Spec.ImageCache != nil {
		for i, m := range g.Spec.Middleware {
			if m.Name != imageCacheFilterName {
				continue
			}
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("middleware").Index(i).Child("name"), m.Name,
				fmt.Sprintf("the operator injects the %q paste filter and owns that filter name while spec.imageCache is set: rename this middleware or unset spec.imageCache",
					imageCacheFilterName),
			))
		}
	}

	// extraConfig sanity: reject an empty section name or an empty option key so
	// the rendered glance-api.conf never carries a nameless [<section>] or a
	// bare "= value" line. extraConfig is a preserve-unknown-fields map, so CEL
	// cannot constrain its keys — the webhook is the sole admission-time gate.
	//
	// It also rejects a newline or carriage return in a section name, a key, OR a
	// value. RenderINIMulti writes each verbatim ("[%s]" for the section, "%s = %s"
	// per option), so such a character injects arbitrary additional config lines —
	// smuggling a whole [section]/key past the (section, key)-name-keyed ownership
	// (FindOwnedOverrides) and catalog (FindUnknownOptions) gates, which inspect
	// map structure only and never look inside a value. Same guard, same reasoning
	// as the ControlPlane's validateINIShape over services.glance.extraConfig and
	// as GlanceBackend's extraOptions values.
	for section, opts := range g.Spec.ExtraConfig {
		if section == "" {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("extraConfig"),
				section,
				"extraConfig section name must not be empty",
			))
			continue
		}
		if hasControlChars(section) {
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
			if hasControlChars(key) || hasControlChars(value) {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("extraConfig").Key(section).Key(key),
					key,
					"extraConfig key and value must not contain a newline or carriage return: "+
						"the rendered INI writes them verbatim, so a newline injects arbitrary config lines",
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

// ValidateImportFiltering mirrors the spec.importFiltering schema as defense in
// depth behind the CRD layer: the three mutual-exclusivity CEL rules, the scheme
// enum, the host length bounds, the port range, and the per-list item cap. It
// additionally carries the one check with no schema counterpart — the host
// INI-injection guard in validateImportFilteringHosts. It is nil-safe — an unset
// block carries nothing to validate and is resolved to the operator defaults at
// render time.
//
// It is exported for the ControlPlane webhook, which carries this very type on
// services.glance.importFiltering and must admit exactly what the Glance CRD
// admits: a hand-kept mirror there would let the two drift, and the ControlPlane
// would start admitting values its projected Glance child then rejects. The
// caller supplies its own field path, so the errors name the CR under admission.
func ValidateImportFiltering(fldPath *field.Path, f *ImportFilteringSpec) field.ErrorList {
	if f == nil {
		return nil
	}
	var errs field.ErrorList

	// Glance evaluates the deny-list only while the matching allow-list is
	// empty, so accepting both would silently drop the deny-list. The error is
	// reported on the deny-list, the half that would be ignored.
	if len(f.AllowedSchemes) > 0 && len(f.DisallowedSchemes) > 0 {
		errs = append(errs, field.Invalid(
			fldPath.Child("disallowedSchemes"), f.DisallowedSchemes, importFilteringSchemesExclusiveMsg,
		))
	}
	if len(f.AllowedHosts) > 0 && len(f.DisallowedHosts) > 0 {
		errs = append(errs, field.Invalid(
			fldPath.Child("disallowedHosts"), f.DisallowedHosts, importFilteringHostsExclusiveMsg,
		))
	}
	if len(f.AllowedPorts) > 0 && len(f.DisallowedPorts) > 0 {
		errs = append(errs, field.Invalid(
			fldPath.Child("disallowedPorts"), f.DisallowedPorts, importFilteringPortsExclusiveMsg,
		))
	}

	errs = append(errs, validateImportFilteringSchemes(fldPath.Child("allowedSchemes"), f.AllowedSchemes)...)
	errs = append(errs, validateImportFilteringSchemes(fldPath.Child("disallowedSchemes"), f.DisallowedSchemes)...)
	errs = append(errs, validateImportFilteringHosts(fldPath.Child("allowedHosts"), f.AllowedHosts)...)
	errs = append(errs, validateImportFilteringHosts(fldPath.Child("disallowedHosts"), f.DisallowedHosts)...)
	errs = append(errs, validateImportFilteringPorts(fldPath.Child("allowedPorts"), f.AllowedPorts)...)
	errs = append(errs, validateImportFilteringPorts(fldPath.Child("disallowedPorts"), f.DisallowedPorts)...)
	return errs
}

// validateImportFilteringSchemes checks one scheme list against the item enum
// and the item cap. Glance's URI filter only ever sees http and https imports,
// so any other scheme would be a dead entry.
func validateImportFilteringSchemes(fldPath *field.Path, schemes []string) field.ErrorList {
	var errs field.ErrorList
	if len(schemes) > maxImportFilteringItems {
		errs = append(errs, field.TooMany(fldPath, len(schemes), maxImportFilteringItems))
	}
	for i, scheme := range schemes {
		if scheme != "http" && scheme != "https" {
			errs = append(errs, field.NotSupported(fldPath.Index(i), scheme, []string{"http", "https"}))
		}
	}
	return errs
}

// validateImportFilteringHosts checks one host list against the item length
// bounds, the item cap, and the INI-injection guard. An empty entry would match
// nothing at all, and a host longer than a DNS name can never appear in an
// import URI.
//
// The control-character rejection has no CRD-schema counterpart (the item
// markers bound length only), so unlike the rest of this family it is not
// defense in depth but the only gate. The host lists are the sole free-form
// strings in ImportFilteringSpec — schemes are enum-bound and ports are numeric
// — and they are comma-joined verbatim into the [import_filtering_opts] keys of
// glance-api.conf. A newline or carriage return in a host would therefore render
// additional config lines, smuggling a whole [section] past the extraConfig
// ownership and catalog gates, which inspect map structure only and never look
// inside a value. Same reasoning, same guard as GlanceBackend's extraOptions
// values (hasControlChars) and the ControlPlane's extraConfig INI-shape check.
func validateImportFilteringHosts(fldPath *field.Path, hosts []string) field.ErrorList {
	var errs field.ErrorList
	if len(hosts) > maxImportFilteringItems {
		errs = append(errs, field.TooMany(fldPath, len(hosts), maxImportFilteringItems))
	}
	for i, host := range hosts {
		switch {
		case host == "":
			errs = append(errs, field.Invalid(fldPath.Index(i), host, "host must not be empty"))
		case hasControlChars(host):
			errs = append(errs, field.Invalid(fldPath.Index(i), host,
				"host must not contain newline or carriage-return characters: the rendered "+
					"[import_filtering_opts] value is written verbatim, so a newline injects arbitrary config lines"))
		case len(host) > maxImportFilteringHostLength:
			errs = append(errs, field.Invalid(fldPath.Index(i), host,
				fmt.Sprintf("host must be at most %d characters", maxImportFilteringHostLength)))
		}
	}
	return errs
}

// validateImportFilteringPorts checks one port list against the TCP port range
// and the item cap.
func validateImportFilteringPorts(fldPath *field.Path, ports []int32) field.ErrorList {
	var errs field.ErrorList
	if len(ports) > maxImportFilteringItems {
		errs = append(errs, field.TooMany(fldPath, len(ports), maxImportFilteringItems))
	}
	for i, port := range ports {
		if port < 1 || port > 65535 {
			errs = append(errs, field.Invalid(fldPath.Index(i), port, "port must be between 1 and 65535"))
		}
	}
	return errs
}

// ValidateImportPlugins mirrors the spec.importPlugins schema as defense in
// depth behind the CRD layer — the output-format enum, the property-count
// bounds, and the ignoreUserRoles item markers — and carries the checks the
// schema has no way to express. A map key is not reachable from a CRD marker
// (the position spec.logging.perLoggerLevels is in), so every rule on a
// property name below is the only gate there is.
//
// Those rules guard the oslo Dict syntax of the rendered
// [inject_metadata_properties] inject value, which splits pairs on commas and
// each pair on its first colon: a comma or colon in a key breaks the pair apart
// silently, and a newline injects whole config lines the way an unfiltered
// import-filtering host would — the same reasoning as
// validateImportFilteringHosts.
//
// It is nil-safe. An unset block enables no plugin and carries nothing to
// validate, and every default it leaves open is resolved at render time.
//
// It is exported for the ControlPlane webhook, which carries this very type on
// services.glance.importPlugins and must admit exactly what the Glance CRD
// admits: a hand-kept mirror there would let the two drift, and the ControlPlane
// would start admitting values its projected Glance child then rejects. The
// caller supplies its own field path, so the errors name the CR under admission.
func ValidateImportPlugins(fldPath *field.Path, p *ImportPluginsSpec) field.ErrorList {
	if p == nil {
		return nil
	}
	var errs field.ErrorList

	// An empty outputFormat is how the block asks for the operator default, so
	// only a non-empty value is measured against the enum.
	if p.Conversion != nil && p.Conversion.OutputFormat != "" &&
		!slices.Contains(importConversionOutputFormats, p.Conversion.OutputFormat) {
		errs = append(errs, field.NotSupported(
			fldPath.Child("conversion", "outputFormat"),
			p.Conversion.OutputFormat,
			importConversionOutputFormats,
		))
	}

	errs = append(errs, validateImportInjectMetadata(fldPath.Child("injectMetadata"), p.InjectMetadata)...)
	return errs
}

// validateImportInjectMetadata checks one inject_image_metadata block: the
// property map against its count bounds and the Dict-syntax rules, and the role
// list against the item bounds it shares with the importFiltering lists. It is
// nil-safe, so the caller does not repeat the check.
func validateImportInjectMetadata(fldPath *field.Path, m *ImportInjectMetadataSpec) field.ErrorList {
	if m == nil {
		return nil
	}
	var errs field.ErrorList

	propsPath := fldPath.Child("properties")
	switch {
	case len(m.Properties) == 0:
		// A nil map and an explicit {} are the same shape here, and neither is the
		// "keep the operator default" gesture the optional fields make: the plugin
		// has nothing to inject, so enabling it says nothing.
		errs = append(errs, field.Required(propsPath,
			"at least one property must be set: the inject_image_metadata plugin has nothing to inject otherwise"))
	case len(m.Properties) > maxImportInjectEntries:
		errs = append(errs, field.TooMany(propsPath, len(m.Properties), maxImportInjectEntries))
	}

	// Sort the keys so the aggregated error list is deterministic, the same
	// reason GlanceBackend sorts its extraOptions keys before validating them.
	keys := make([]string, 0, len(m.Properties))
	for k := range m.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		errs = append(errs, validateImportInjectProperty(propsPath, k, m.Properties[k])...)
	}

	rolesPath := fldPath.Child("ignoreUserRoles")
	if len(m.IgnoreUserRoles) > maxImportInjectEntries {
		errs = append(errs, field.TooMany(rolesPath, len(m.IgnoreUserRoles), maxImportInjectEntries))
	}
	for i, role := range m.IgnoreUserRoles {
		switch {
		case role == "":
			errs = append(errs, field.Invalid(rolesPath.Index(i), role, "role must not be empty"))
		case hasControlChars(role):
			errs = append(errs, field.Invalid(rolesPath.Index(i), truncateForError(role),
				"role must not contain newline or carriage-return characters: the rendered "+
					"[inject_metadata_properties] value is written verbatim, so a newline injects arbitrary config lines"))
		case strings.Contains(role, ","):
			errs = append(errs, field.Invalid(rolesPath.Index(i), truncateForError(role),
				"role must not contain a comma: the rendered ignore_user_roles list separates roles on commas, "+
					"so the entry would be read as two roles"))
		case utf8.RuneCountInString(role) > maxImportInjectStringLength:
			errs = append(errs, field.Invalid(rolesPath.Index(i), truncateForError(role),
				fmt.Sprintf("role must be at most %d characters", maxImportInjectStringLength)))
		}
	}
	return errs
}

// validateImportInjectProperty checks one injected property pair against the
// oslo Dict syntax the rendered inject value is parsed with. The key side is
// the strict one: the parser splits pairs on commas and each pair on its first
// colon, so either character in a key silently produces a property nobody
// wrote. A value may carry a colon — everything after the first one belongs to
// it, which is what lets a URL be injected — but not a comma, which would start
// the next pair. Both sides reject the control characters that would inject
// config lines, and both reject surrounding whitespace, which the parser strips
// so the injected property would differ from the one the CR stores.
//
// Both sides are also length-bounded. Every pair is rendered verbatim into one
// [inject_metadata_properties] inject line, so an unbounded value lets a CR that
// etcd accepts (~1.5 MiB) render a glance-api.conf past the 1 MiB ConfigMap
// ceiling — admission would be handing the reconciler an object the API server
// then refuses on every attempt, with the Deployment pinned to the last good
// render because the ConfigMap name carries the content hash.
func validateImportInjectProperty(propsPath *field.Path, key, value string) field.ErrorList {
	var errs field.ErrorList

	// Every error below names the property through its map key, and Path.Key
	// stores that string verbatim as the subscript: field.Error.Field carries it
	// into the response body, the operator log, and a RequestResponse audit entry
	// — the same three places BadValue lands in, and the reason truncateForError
	// exists. So the subscript is truncated once here, and an over-long key
	// cannot be echoed back through the path either. Both halves are truncated
	// wherever they are reported, not only on the length branch: a value that
	// carries a comma or a newline is rejected before its size is ever measured.
	keyPath := propsPath.Key(truncateForError(key))

	// An empty key has no Key() path worth printing, so it is reported on the map
	// itself — the same shape the perLoggerLevels empty-name check uses.
	switch {
	case key == "":
		errs = append(errs, field.Invalid(propsPath, key, "property name must not be empty"))
		return errs
	case hasControlChars(key):
		errs = append(errs, field.Invalid(keyPath, truncateForError(key),
			"property name must not contain newline or carriage-return characters: the rendered "+
				"[inject_metadata_properties] inject value is written verbatim, so a newline injects arbitrary config lines"))
	case strings.Contains(key, ":"):
		errs = append(errs, field.Invalid(keyPath, truncateForError(key),
			"property name must not contain a colon: the oslo Dict syntax splits each pair on the first colon, "+
				"so the name would be silently truncated there and its remainder injected as part of the value"))
	case strings.Contains(key, ","):
		errs = append(errs, field.Invalid(keyPath, truncateForError(key),
			"property name must not contain a comma: the oslo Dict syntax separates pairs on commas, "+
				"so the name would be split into a property of its own"))
	case strings.TrimSpace(key) != key:
		errs = append(errs, field.Invalid(keyPath, truncateForError(key),
			"property name must not start or end with whitespace: the oslo parser strips it, "+
				"so the injected property name would differ from the one stored here"))
	case utf8.RuneCountInString(key) > maxImportInjectStringLength:
		errs = append(errs, field.Invalid(keyPath, truncateForError(key),
			fmt.Sprintf("property name must be at most %d characters", maxImportInjectStringLength)))
	}

	switch {
	case hasControlChars(value):
		errs = append(errs, field.Invalid(keyPath, truncateForError(value),
			"property value must not contain newline or carriage-return characters: the rendered "+
				"[inject_metadata_properties] inject value is written verbatim, so a newline injects arbitrary config lines"))
	case strings.Contains(value, ","):
		errs = append(errs, field.Invalid(keyPath, truncateForError(value),
			"property value must not contain a comma: the oslo Dict syntax separates pairs on commas, "+
				"so the value would be cut short and its remainder read as another property"))
	case strings.TrimSpace(value) != value:
		errs = append(errs, field.Invalid(keyPath, truncateForError(value),
			"property value must not start or end with whitespace: the oslo parser strips it, "+
				"so the injected value would differ from the one stored here"))
	case utf8.RuneCountInString(value) > maxImportInjectStringLength:
		errs = append(errs, field.Invalid(keyPath, truncateForError(value),
			fmt.Sprintf("property value must be at most %d characters", maxImportInjectStringLength)))
	}
	return errs
}

// ValidateImportDecompressionStaging requires a CR that enables the
// image_decompression plugin to size the staging area itself instead of
// inheriting DefaultStagingSizeLimit. The plugin expands the staged file by a
// ratio the caller picks and neither glance nor this operator caps the result,
// which leaves spec.staging.sizeLimit as the only bound in the path — and
// breaching it evicts the whole glance-api pod, taking every concurrent import
// and in-flight request on that replica with it. A bound left at the default was
// sized against the largest DOWNLOAD, which is the wrong number the moment the
// plugin is on, so admission asks for the one thing it can: that somebody picked
// it. spec.staging.unbounded satisfies the rule too — it is the deliberate
// no-bound escape hatch, and choosing it is equally an answer.
//
// It correlates two top-level spec fields, so no marker on either type reaches
// it and admission is the sole gate — the position the imageCache/middleware
// name collision is in.
//
// It is exported and takes the path of the spec that carries both blocks, so the
// ControlPlane webhook enforces this rule on services.glance through the same
// code rather than a copy that could drift.
func ValidateImportDecompressionStaging(specPath *field.Path, p *ImportPluginsSpec, s *StagingSpec) field.ErrorList {
	if p == nil || p.Decompression == nil {
		return nil
	}
	if s != nil && (s.SizeLimit != nil || s.Unbounded) {
		return nil
	}
	return field.ErrorList{field.Required(
		specPath.Child("staging", "sizeLimit"),
		fmt.Sprintf("must be set explicitly while importPlugins.decompression is enabled: the plugin expands "+
			"the staged image by a ratio the caller picks and nothing caps the result, and breaching this bound "+
			"evicts the glance-api pod together with every other in-flight import on that replica, so the %s "+
			"default cannot be assumed to fit the largest UNPACKED image (set staging.unbounded to deliberately "+
			"keep no bound instead)", defaultStagingSizeLimit.String()),
	)}
}

// truncateForError shortens an over-long string before it is echoed back by an
// admission error — as the BadValue, or as the map-key subscript of the field
// path, which field.Error renders alongside it. field.Invalid puts both into the
// response body, which the API server returns to the client, the operator logs,
// and a RequestResponse audit policy records — so a value sized against etcd's
// ~1.5 MiB object ceiling would be reproduced in full three times over for a
// message that is a fixed-length statement. Keeping the bound's worth of prefix
// is enough to recognize which value was rejected.
func truncateForError(s string) string {
	runes := []rune(s)
	if len(runes) <= maxImportInjectStringLength {
		return s
	}
	return string(runes[:maxImportInjectStringLength]) + "…"
}

// validateDBPurge mirrors the spec.dbPurge schema as defense in depth behind the
// CRD layer — the Minimum marker on retentionDays — and carries the one check
// with no schema counterpart: the cron grammar, which DBPurgeSpec.Schedule
// deliberately leaves to the webhook. It is nil-safe, and so is each half: an
// unset field carries nothing to validate and is resolved to the operator
// default at reconcile time.
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

// minStagingSizeLimit is the floor spec.staging.sizeLimit must clear. A bound
// below one mebibyte names no usable scratch budget: the kubelet evicts the
// glance-api pod on the first staged byte, the Deployment replaces it, and the
// next import evicts the replacement — an eviction loop nothing in the CR status
// explains. It is also where the `100m`-for-`100Mi` suffix typo lands, since
// `100m` is a perfectly well-formed Quantity worth a tenth of a byte.
var minStagingSizeLimit = resource.MustParse("1Mi")

// ValidateStaging enforces the floor on spec.staging.sizeLimit and the
// mutual exclusion between it and spec.staging.unbounded. The floor is not
// defense in depth: a resource.Quantity renders as x-kubernetes-int-or-string in
// the CRD schema, and that type carries no Minimum marker, so the webhook is the
// sole gate — Kubernetes core validation rejects only a NEGATIVE
// emptyDir.sizeLimit, and everything from `0` through `100m` reaches the kubelet
// verbatim. The mutual exclusion does have a CEL counterpart on StagingSpec,
// since unbounded is a plain bool the schema can reason about; it is repeated
// here so both gates keep the same wording. It is nil-safe — an unset block, and
// an unset sizeLimit inside a set block, carry nothing to validate and are
// resolved to the operator default at reconcile time.
//
// It is exported for the ControlPlane webhook, which carries this type on
// services.glance.staging and must admit exactly what the Glance CRD admits. The
// caller supplies its own field path, so the errors name the CR under admission.
func ValidateStaging(fldPath *field.Path, s *StagingSpec) field.ErrorList {
	if s == nil {
		return nil
	}
	if s.Unbounded && s.SizeLimit != nil {
		return field.ErrorList{field.Invalid(
			fldPath.Child("unbounded"), s.Unbounded,
			"unbounded and sizeLimit are mutually exclusive: an unbounded staging area has no size limit to configure",
		)}
	}
	if s.SizeLimit == nil {
		return nil
	}
	if s.SizeLimit.Cmp(minStagingSizeLimit) < 0 {
		return field.ErrorList{field.Invalid(
			fldPath.Child("sizeLimit"), s.SizeLimit.String(),
			fmt.Sprintf("must be at least %s", minStagingSizeLimit.String()),
		)}
	}
	return nil
}

// minImageCacheSizeLimit is the floor spec.imageCache.sizeLimit must clear, and
// it is the staging floor's twin: a bound below one mebibyte names no usable
// cache budget, and it is where the `100m`-for-`100Mi` suffix typo lands. Under
// such a bound the derived image_cache_max_size is smaller than any image, so
// every cached download overruns the emptyDir and the kubelet evicts the pod
// that just served it.
var minImageCacheSizeLimit = resource.MustParse("1Mi")

// minImageCacheMaintenanceInterval is the floor
// spec.imageCache.maintenanceInterval must clear. Every tick walks the cache
// directory and the sqlite index, so a sub-minute loop spends the pod's
// local-disk bandwidth on maintenance rather than on the downloads the cache
// exists to accelerate.
const minImageCacheMaintenanceInterval = time.Minute

// ValidateImageCache enforces the floors on spec.imageCache.sizeLimit and
// spec.imageCache.maintenanceInterval. Neither is defense in depth: a
// resource.Quantity renders as x-kubernetes-int-or-string and a metav1.Duration
// as a plain string, and neither shape carries a Minimum marker, so the webhook
// is the sole gate — the same position spec.staging.sizeLimit is in. It is
// nil-safe on both levels: an unset block is how a CR leaves the cache disabled,
// and an unset field inside a set block is how it asks for the operator default,
// so neither is a violation.
//
// It is exported for the ControlPlane webhook, which carries this type on
// services.glance.imageCache and must admit exactly what the Glance CRD admits;
// a hand-kept mirror there would let the two drift. The caller supplies its own
// field path, so the errors name the CR under admission.
func ValidateImageCache(fldPath *field.Path, c *ImageCacheSpec) field.ErrorList {
	if c == nil {
		return nil
	}
	var errs field.ErrorList

	if c.SizeLimit != nil && c.SizeLimit.Cmp(minImageCacheSizeLimit) < 0 {
		errs = append(errs, field.Invalid(
			fldPath.Child("sizeLimit"), c.SizeLimit.String(),
			fmt.Sprintf("must be at least %s", minImageCacheSizeLimit.String()),
		))
	}
	if c.MaintenanceInterval != nil && c.MaintenanceInterval.Duration < minImageCacheMaintenanceInterval {
		errs = append(errs, field.Invalid(
			fldPath.Child("maintenanceInterval"), c.MaintenanceInterval.Duration.String(),
			fmt.Sprintf("must be at least %s", minImageCacheMaintenanceInterval.String()),
		))
	}
	return errs
}

// warnDBPurgeRetention surfaces a retention window that an update shortens.
// Unlike every other knob on the CR, the effect is immediate and irreversible:
// at the next firing the purge hard-deletes every row that fell out of the
// window, and nothing brings those rows back. A typo (3 for 30) is
// indistinguishable from an intended change at admission time, so the reduction
// is echoed back to whoever made it. It stays a warning rather than a rejection
// because shortening the window is a legitimate operational choice.
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

// WarnImportFiltering surfaces the two spec.importFiltering shapes that are
// admissible but do not mean what they look like. Both are warnings, not
// rejections: each is a legitimate deployment choice, and the tempest legs rely
// on the second one.
//
// It is exported and takes a caller-supplied field path for the same reason
// ValidateImportFiltering is: the ControlPlane carries this very type on
// services.glance.importFiltering, and a warning the Glance CR emits but the
// ControlPlane — the primary authoring surface — stays silent about would be
// exactly the drift that sharing the type is meant to prevent.
func WarnImportFiltering(fldPath *field.Path, f *ImportFilteringSpec) admission.Warnings {
	if f == nil {
		return nil
	}
	var warnings admission.Warnings

	// (1) A deny-list glance will never evaluate. It evaluates one only while
	// the matching allow-list is empty, and a nil allowedSchemes/allowedPorts
	// resolves to a non-empty operator default at render time — so the deny half
	// renders into glance-api.conf and stays inert. ValidateImportFiltering
	// rejects the both-non-empty shape; this covers the shape it cannot see.
	// Hosts are exempt: allowedHosts has no default, so a host deny-list on its
	// own is authoritative.
	if len(f.DisallowedSchemes) > 0 && f.AllowedSchemes == nil {
		warnings = append(warnings, fmt.Sprintf(inertDenyListWarningFmt,
			fldPath.Child("disallowedSchemes"), fldPath.Child("allowedSchemes"),
			DefaultImportAllowedSchemes, fldPath.Child("allowedSchemes")))
	}
	if len(f.DisallowedPorts) > 0 && f.AllowedPorts == nil {
		warnings = append(warnings, fmt.Sprintf(inertDenyListWarningFmt,
			fldPath.Child("disallowedPorts"), fldPath.Child("allowedPorts"),
			DefaultImportAllowedPorts, fldPath.Child("allowedPorts")))
	}

	// (2) An allow-list widened past the operator default. The scheme and port
	// pin is what keeps a web-download import off plaintext services and off the
	// link-local metadata endpoint (which answers on http/80); DefaultImport-
	// DisallowedHosts is belt-and-braces behind it, and on its own it is only a
	// literal string match that alternate spellings of the same address evade.
	if widensAllowList(f.AllowedSchemes, DefaultImportAllowedSchemes) {
		warnings = append(warnings, fmt.Sprintf(widenedAllowListWarningFmt,
			fldPath.Child("allowedSchemes"), f.AllowedSchemes, DefaultImportAllowedSchemes))
	}
	if widensAllowList(f.AllowedPorts, DefaultImportAllowedPorts) {
		warnings = append(warnings, fmt.Sprintf(widenedAllowListWarningFmt,
			fldPath.Child("allowedPorts"), f.AllowedPorts, DefaultImportAllowedPorts))
	}
	return warnings
}

// widensAllowList reports whether an explicitly set allow-list relaxes the
// operator default it replaces. A nil list is not set at all — it resolves to
// the default. An explicitly empty list drops the restriction entirely (glance
// falls back to its own permissive default), and any entry the default does not
// carry admits something the default refused.
func widensAllowList[T comparable](set, defaults []T) bool {
	if set == nil {
		return false
	}
	if len(set) == 0 {
		return true
	}
	byDefault := make(map[T]struct{}, len(defaults))
	for _, d := range defaults {
		byDefault[d] = struct{}{}
	}
	for _, v := range set {
		if _, ok := byDefault[v]; !ok {
			return true
		}
	}
	return false
}

// Warning texts for the two admissible-but-misleading importFiltering shapes.
// Both take the field paths as arguments so the same text serves the Glance CR
// and the ControlPlane's services.glance.importFiltering.
const (
	inertDenyListWarningFmt = "%s is set while %s is unset and therefore resolves to the operator default %v. " +
		"Glance evaluates a deny-list only while the matching allow-list is empty, so this list is inert; " +
		"set %s to an explicitly empty list ([]) to make the deny-list authoritative."

	widenedAllowListWarningFmt = "%s is set to %v, widening the operator default %v. The scheme and port pin " +
		"is the primary web-download control — the link-local metadata endpoint answers on http/80 — and once " +
		"widened only the host denylist remains, which matches literally: alternate spellings of the same " +
		"address (an integer or IPv4-mapped form, a raw ClusterIP, a trailing-dot FQDN) are not covered. " +
		"Confine the reachable endpoints with spec.networkPolicy.additionalEgress, or pin allowedHosts to the " +
		"mirrors imports are supposed to use."
)

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
