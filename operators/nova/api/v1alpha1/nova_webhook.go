// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/validation"
	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// novaAppName is the app.kubernetes.io/name label value every Nova-owned object
// carries, and the component constants are the app.kubernetes.io/component
// values of the four non-API Deployments. They are duplicated from the
// controller package (which builds the objects) because the api package cannot
// import the controller; the topology-spread selector check is measured against
// the labels they compose.
const (
	novaAppName           = "nova"
	componentMetadata     = "metadata"
	componentScheduler    = "scheduler"
	componentConductor    = "conductor"
	componentConsoleProxy = "novncproxy"
)

// Name-length bounds enforced on metadata.name, driven by the archive CronJob,
// the child object with the tightest name budget.
const (
	// MaxCronJobNameLength is the API server's own cap on a CronJob name:
	// DNS1035LabelMaxLength (63) minus the 11-character "-<timestamp>" suffix its
	// controller appends to every Job it spawns.
	MaxCronJobNameLength = 52
	// dbArchiveNameSuffix is appended to metadata.name to name the archive
	// CronJob. It is duplicated from the controller package (which builds the
	// object) because the api package cannot import the controller.
	dbArchiveNameSuffix = "-db-archive"
	// MaxNovaNameLength is what those two leave for metadata.name.
	MaxNovaNameLength = MaxCronJobNameLength - len(dbArchiveNameSuffix)
)

// childNameSuffixes are the suffixes the operator appends to metadata.name to
// name a child whose kind a Nova also names after its bare metadata.name: the
// nova_api MariaDB Database, User and Grant ("-api", beside the cell schema's
// bare-named ones), the Deployments and Services of the four non-API processes
// and the metadata HTTPRoute (beside the API's), and the console HTTPRoute. A
// Nova named "<x>-api" would therefore share the MariaDB objects and the
// connection Secret of the Nova named "<x>" in the same namespace, and deleting
// either CR would delete them for both. The literals are duplicated from the
// controller package (which builds the objects) because the api package cannot
// import it.
var childNameSuffixes = []string{
	"-api",
	"-" + componentMetadata,
	"-" + componentScheduler,
	"-" + componentConductor,
	"-" + componentConsoleProxy,
	"-console",
}

// cell0ChildSuffix ends the name of the cell0 MariaDB Database and Grant, which
// a Nova derives as "<name>-<database>-cell0" from the SQL schema
// "<database>_cell0". A Nova whose own name ends in it names its cell schema's
// Database, User and Grant exactly like those of the Nova "<name>" whose
// spec.database.database completes the rest, so it takes over that Nova's cell0
// objects. It is duplicated from the database package's derivation for the same
// reason as childNameSuffixes.
const cell0ChildSuffix = "-cell0"

// Database-archive defaults for the recurring nova-manage CronJob, which moves
// the instance rows nova only ever soft-deletes into their shadow tables.
//
// They are consumed by the reconcile-time resolver and deliberately NOT applied
// by the defaulting webhook: a nil or partial spec.dbArchive block keeps tracking
// these operator defaults across upgrades instead of freezing today's values into
// the stored CR.
const (
	// DefaultDBArchiveSchedule runs the archive once a day, resolved when
	// spec.dbArchive.schedule is empty.
	DefaultDBArchiveSchedule = "@daily"
	// DefaultDBArchiveMaxRows bounds one batch to a thousand rows per table,
	// resolved when spec.dbArchive.maxRows is unset. The bound keeps a batch on a
	// long-neglected database from holding table locks for the length of the
	// backlog; a run repeats batches within its time budget, and the next run
	// continues where this one stopped.
	DefaultDBArchiveMaxRows int32 = 1000
	// DefaultDBArchiveSleep waits a second between batches, resolved when
	// spec.dbArchive.sleep is unset, so the archive leaves the database room to
	// serve the API alongside it.
	DefaultDBArchiveSleep int32 = 1
)

// Process-level defaults the defaulting webhook materializes.
const (
	// DefaultComponentReplicas is the replica count the metadata API, the
	// scheduler, the conductor and the console proxy resolve to when their block
	// leaves it unset, rather than the shared default of three. All four are peers
	// that hold nothing between requests, so one is enough to start from and
	// raising it costs only the pods. The ControlPlane projects the same value
	// explicitly, so the two stay one fact.
	DefaultComponentReplicas int32 = 1
	// DefaultWorkers is the nova-scheduler and nova-conductor worker count
	// resolved when the block leaves it unset. Two keeps one request from
	// blocking the next inside a pod without multiplying the database and bus
	// connections the pod holds.
	DefaultWorkers int32 = 2
	// DefaultRPCTerminationGracePeriodSeconds is the graceful-termination window
	// the scheduler and the conductor get instead of the shared default. Both
	// shut down by draining their RPC server, which 33.0.0 completes in 165 to
	// 167 seconds, so the shared 30 seconds would end every rollout in a SIGKILL
	// mid-request.
	DefaultRPCTerminationGracePeriodSeconds int64 = 200
	// DefaultSharedSecretKey is the Secret key the metadata shared secret is read
	// from when spec.metadata.sharedSecretRef.key is empty.
	DefaultSharedSecretKey = "shared_secret"
	// DefaultServiceUserSecretKey is the Secret key the service-user password is
	// read from when spec.serviceUser.secretRef.key is empty.
	DefaultServiceUserSecretKey = "password"
)

// NovaWebhook implements defaulting and validation webhooks for the Nova CRD.
// Client is injected at startup for cluster-scoped resource lookups (e.g.
// PriorityClass validation). Production wiring injects mgr.GetAPIReader(), a
// direct, uncached reader, so admission never rejects a just-created object from
// a stale informer cache and no lazy informer start happens inside the webhook
// timeout.
// +kubebuilder:object:generate=false
type NovaWebhook struct {
	commonwebhook.NoopDeleteValidator[*Nova]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*Nova] = &NovaWebhook{}
	_ admission.Validator[*Nova] = &NovaWebhook{}
)

// +kubebuilder:webhook:path=/mutate-nova-openstack-c5c3-io-v1alpha1-nova,mutating=true,failurePolicy=fail,sideEffects=None,groups=nova.openstack.c5c3.io,resources=novas,verbs=create;update,versions=v1alpha1,name=mnova.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-nova-openstack-c5c3-io-v1alpha1-nova,mutating=false,failurePolicy=fail,sideEffects=None,groups=nova.openstack.c5c3.io,resources=novas,verbs=create;update,versions=v1alpha1,name=vnova.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *NovaWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*Nova](mgr, &Nova{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*Nova]. It sets spec fields to their
// documented defaults when they carry zero values, following the keystone/glance
// non-mutating discipline: optional pointer blocks are only partially filled when
// explicitly present, except spec.logging and the two uwsgi blocks, which are
// materialized so downstream reconciler code never sees a nil pointer, and
// spec.consoleProxy.deployment, which is materialized while the proxy is enabled
// and removed while it is not.
//
// The one-replica defaults for the metadata, scheduler and conductor Deployments
// reach the absent-block case only. commonv1.DeploymentSpec.Replicas carries
// +kubebuilder:default=3, which the API server materializes as soon as the CR
// carries a deployment object at all, before any mutating webhook runs, so a
// block that sets anything else arrives here with replicas already at 3, and
// rewriting that to 1 would silently overwrite a value the submitter can see in
// their own manifest.
func (w *NovaWebhook) Default(_ context.Context, obj *Nova) error {
	// The three non-API Deployments resolve to one replica rather than the shared
	// default of three. All three are peers that hold nothing between requests
	// and may be raised; the count is set before the shared Default() runs, which
	// would otherwise fill the absent block with three.
	if obj.Spec.Metadata.Deployment.Replicas == 0 {
		obj.Spec.Metadata.Deployment.Replicas = DefaultComponentReplicas
	}
	if obj.Spec.Scheduler.Deployment.Replicas == 0 {
		obj.Spec.Scheduler.Deployment.Replicas = DefaultComponentReplicas
	}
	if obj.Spec.Conductor.Deployment.Replicas == 0 {
		obj.Spec.Conductor.Deployment.Replicas = DefaultComponentReplicas
	}

	// The console-proxy block is materialized only while the proxy is projected.
	// A disabled proxy keeps a nil Deployment, which is what the console rule on
	// NovaSpec and its webhook twin measure: filling it here would make the
	// operator itself produce the shape both gates reject. The same reason
	// removes the block once the proxy is switched off: the one this webhook
	// materialized on create is still stored on the CR, so a patch that only sets
	// enabled to false would otherwise carry a deployment its submitter never
	// wrote into both gates. A disabled proxy has no Deployment to size, so
	// nothing the operator reads is lost.
	if obj.Spec.ConsoleProxyEnabled() {
		if obj.Spec.ConsoleProxy.Deployment == nil {
			obj.Spec.ConsoleProxy.Deployment = &DeploymentSpec{Replicas: DefaultComponentReplicas}
		}
		obj.Spec.ConsoleProxy.Deployment.Default()
	} else {
		obj.Spec.ConsoleProxy.Deployment = nil
	}

	// Shared-type defaults (replicas) are applied by the
	// commonv1.DeploymentSpec Default method so they cannot drift across
	// operators. Every process gets them: they are sized independently.
	obj.Spec.API.Deployment.Default()
	obj.Spec.Metadata.Deployment.Default()
	obj.Spec.Scheduler.Deployment.Default()
	obj.Spec.Conductor.Deployment.Default()

	// The scheduler and the conductor shut down by draining their RPC server,
	// which takes nova 33.0.0 between 165 and 167 seconds. Under the shared
	// 30-second window every rollout would end in a SIGKILL with requests still
	// in flight, so both get a window that covers the drain. An explicit value is
	// left alone.
	for _, d := range []*commonv1.DeploymentSpec{
		&obj.Spec.Scheduler.Deployment,
		&obj.Spec.Conductor.Deployment,
	} {
		if d.TerminationGracePeriodSeconds == nil {
			d.TerminationGracePeriodSeconds = ptr.To(DefaultRPCTerminationGracePeriodSeconds)
		}
	}
	if obj.Spec.Scheduler.Workers == nil {
		obj.Spec.Scheduler.Workers = ptr.To(DefaultWorkers)
	}
	if obj.Spec.Conductor.Workers == nil {
		obj.Spec.Conductor.Workers = ptr.To(DefaultWorkers)
	}

	// Both HTTP front ends always run under uWSGI, so the blocks are materialized
	// and their leaf defaults applied by the commonv1.UWSGISpec Default method,
	// which keeps them from drifting across operators.
	if obj.Spec.API.UWSGI == nil {
		obj.Spec.API.UWSGI = &UWSGISpec{}
	}
	obj.Spec.API.UWSGI.Default()
	if obj.Spec.Metadata.UWSGI == nil {
		obj.Spec.Metadata.UWSGI = &UWSGISpec{}
	}
	obj.Spec.Metadata.UWSGI.Default()

	// The console proxy is projected unless the CR says otherwise. The switch is
	// materialized after the block above has read it, so both arms see the same
	// answer from ConsoleProxyEnabled.
	if obj.Spec.ConsoleProxy.Enabled == nil {
		obj.Spec.ConsoleProxy.Enabled = ptr.To(true)
	}

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

	// ServiceUser identity defaults. The block is required, so only its fields
	// are filled, and each only when empty so an explicit value is never
	// clobbered. A minimal block need only supply the password Secret reference.
	su := &obj.Spec.ServiceUser
	if su.Username == "" {
		su.Username = "nova"
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
		su.SecretRef.Key = DefaultServiceUserSecretKey
	}

	if obj.Spec.Metadata.SharedSecretRef.Key == "" {
		obj.Spec.Metadata.SharedSecretRef.Key = DefaultSharedSecretKey
	}

	// The remote transport URL is read under the key a brownfield bus Secret
	// uses. The block itself is never materialized: the remote contract is
	// opt-in.
	if rc := obj.Spec.RemoteCompute; rc != nil && rc.TransportURLSecretRef.Key == "" {
		rc.TransportURLSecretRef.Key = commonv1.DefaultTransportURLSecretKey
	}
	return nil
}

// ValidateCreate implements admission.Validator[*Nova].
//
// The two metadata.name rules are enforced here rather than in validate(), which
// update shares: the name is immutable, so on update a rule could only ever fire
// against an object a pre-upgrade operator already admitted, and the validating
// webhook also sees the finalizer-removal update reconcileDelete issues, so
// rejecting it would wedge that CR in Terminating with no field left to edit to
// repair it.
func (w *NovaWebhook) ValidateCreate(ctx context.Context, obj *Nova) (admission.Warnings, error) {
	warnings, createErrs := validateExtraConfigOptions(
		field.NewPath("spec"), obj.Spec.OpenStackRelease, obj.Spec.ExtraConfig, OwnedConfigKeys)
	createErrs = append(createErrs, validateNameLength(obj.Name)...)
	createErrs = append(createErrs, validateNameSuffix(obj.Name)...)
	return warnings, w.validate(ctx, obj, createErrs)
}

// validateNameLength bounds metadata.name by the child object with the tightest
// name budget, the "{name}-db-archive" CronJob: the API server rejects a CronJob
// name longer than MaxCronJobNameLength.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateNameLength(name string) field.ErrorList {
	if len(name) <= MaxNovaNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"), name,
		fmt.Sprintf("name must be at most %d characters: the db-archive CronJob appends %q and Kubernetes caps CronJob names at %d characters",
			MaxNovaNameLength, dbArchiveNameSuffix, MaxCronJobNameLength),
	)}
}

// validateNameSuffix rejects a metadata.name ending in one of the
// childNameSuffixes: the Nova named by the rest of the name would build a child
// under this name, so the two CRs would write, and on deletion delete, each
// other's objects. A name ending in cell0ChildSuffix is rejected for the same
// reason, although the Nova it collides with is not named by the rest of the
// name alone: its spec.database.database makes up the part before the suffix.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateNameSuffix(name string) field.ErrorList {
	for _, suffix := range childNameSuffixes {
		if sibling, found := strings.CutSuffix(name, suffix); found {
			return field.ErrorList{field.Invalid(
				field.NewPath("metadata", "name"), name,
				fmt.Sprintf("name must not end in %q: a Nova named %q in this namespace names one of its own children %q, so the two CRs would share and delete each other's objects",
					suffix, sibling, name),
			)}
		}
	}
	if strings.HasSuffix(name, cell0ChildSuffix) {
		return field.ErrorList{field.Invalid(
			field.NewPath("metadata", "name"), name,
			fmt.Sprintf("name must not end in %q: a Nova in this namespace names its cell0 MariaDB Database and Grant <name>-<database>-cell0, which can be %q, so the two CRs would share and delete each other's objects",
				cell0ChildSuffix, name),
		)}
	}
	return nil
}

// ValidateUpdate implements admission.Validator[*Nova].
//
// The extraConfig option-catalog check is re-run only when one of its inputs
// changed (extraConfig or spec.openStackRelease). This keeps an unrelated update
// (scaling replicas, say) from retroactively rejecting a CR whose extraConfig was
// accepted at create time but has since been invalidated by a regenerated
// catalog.
//
// spec.targetClusterRef is compared across both revisions here, the webhook-layer
// twin of the two transition CEL rules on NovaSpec, and so are the schema name
// and the connection mode of both database blocks, the twins of the transition
// rules on spec.apiDatabase and spec.database.
//
// An update to a CR that is being deleted and leaves its spec alone is admitted
// without validation. That is the finalizer removal reconcileDelete issues, and
// the rules below can reject an unchanged spec that was admitted earlier: a
// PriorityClass deleted since, or an operator upgrade that rejects an owned key
// the CR still carries. Rejecting the removal would hold the CR, and the target
// cluster children its finalizer guards, in Terminating with nothing left to
// edit.
func (w *NovaWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *Nova) (admission.Warnings, error) {
	if newObj.DeletionTimestamp != nil && equality.Semantic.DeepEqual(oldObj.Spec, newObj.Spec) {
		return nil, nil
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
	updateErrs = append(updateErrs, validateDatabaseImmutable(
		field.NewPath("spec", "apiDatabase"), "apiDatabase", &oldObj.Spec.APIDatabase, &newObj.Spec.APIDatabase,
		"the schema holds the cell map and every instance mapping")...)
	updateErrs = append(updateErrs, validateDatabaseImmutable(
		field.NewPath("spec", "database"), "database", &oldObj.Spec.Database, &newObj.Spec.Database,
		"the cell mappings store the schema name")...)
	warnings = append(warnings, warnDBArchiveRetention(oldObj.Spec.DBArchive, newObj.Spec.DBArchive)...)
	return warnings, w.validate(ctx, newObj, updateErrs)
}

// validateDatabaseImmutable is the webhook-layer twin of the two transition
// rules on a database block: the schema name and the managed (clusterRef) versus
// brownfield (host) mode stay what they were at create. block names the field in
// the messages, which repeat the CEL rules' text, and reason is what the renamed
// schema would leave behind.
func validateDatabaseImmutable(fldPath *field.Path, block string, oldDB, newDB *commonv1.DatabaseSpec,
	reason string,
) field.ErrorList {
	var errs field.ErrorList
	if oldDB.Database != newDB.Database {
		errs = append(errs, field.Invalid(fldPath.Child("database"), newDB.Database,
			fmt.Sprintf("%s.database is immutable: %s", block, reason)))
	}
	if (oldDB.ClusterRef != nil) != (newDB.ClusterRef != nil) {
		errs = append(errs, field.Invalid(fldPath.Child("clusterRef"), newDB.ClusterRef,
			fmt.Sprintf("%s mode (managed clusterRef vs brownfield host) is immutable", block)))
	}
	return errs
}

// validate runs all validation rules against the Nova spec, accumulating every
// violation so users see the full list in one admission response.
// ctx is required for cluster-scoped lookups (PriorityClass validation).
// extra carries the errors accumulated by the caller (the extraConfig
// option-catalog check, on create the metadata.name bound, and on update the
// targetClusterRef immutability check) so they aggregate into the single Invalid
// error alongside the rest.
func (w *NovaWebhook) validate(ctx context.Context, n *Nova, extra field.ErrorList) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Defense-in-depth image checks alongside the +kubebuilder:validation markers
	// and the XValidation rule on commonv1.ImageSpec.
	allErrs = append(allErrs, validateImage(specPath.Child("image"), n.Spec.Image)...)

	// Defense-in-depth database/cache/messaging mutual-exclusivity and
	// Dynamic-requires-clusterRef checks alongside the
	// +kubebuilder:validation:XValidation CEL rules on the shared commonv1 types,
	// via the shared validators. Both database blocks are checked: each carries
	// its own connection and its own credential.
	apiDatabasePath := specPath.Child("apiDatabase")
	databasePath := specPath.Child("database")
	allErrs = append(allErrs, validation.DatabaseXOR(apiDatabasePath, &n.Spec.APIDatabase)...)
	allErrs = append(allErrs, validation.DynamicCredentialsRequireClusterRef(apiDatabasePath, &n.Spec.APIDatabase)...)
	allErrs = append(allErrs, validation.DatabaseXOR(databasePath, &n.Spec.Database)...)
	allErrs = append(allErrs, validation.DynamicCredentialsRequireClusterRef(databasePath, &n.Spec.Database)...)

	// Defense-in-depth twins of the database CEL rules on NovaSpec and on
	// spec.database. The messages repeat the rules' text so both gates say the
	// same thing.
	if n.Spec.APIDatabase.Database != "" && n.Spec.APIDatabase.Database == n.Spec.Database.Database {
		allErrs = append(allErrs, field.Invalid(
			apiDatabasePath.Child("database"), n.Spec.APIDatabase.Database,
			"apiDatabase and database must name different schemas",
		))
	}
	if n.Spec.APIDatabase.Database != "" && n.Spec.APIDatabase.Database == n.Spec.Database.Database+"_cell0" {
		allErrs = append(allErrs, field.Invalid(
			apiDatabasePath.Child("database"), n.Spec.APIDatabase.Database,
			"apiDatabase must not name the cell0 schema derived from database",
		))
	}
	if len(n.Spec.Database.Database) > maxCellDatabaseNameLength {
		allErrs = append(allErrs, field.Invalid(
			databasePath.Child("database"), n.Spec.Database.Database,
			"database.database must be at most 58 characters: cell0 is provisioned as <database>_cell0 under the 64-character schema limit",
		))
	}
	if effectiveCredentialsMode(&n.Spec.APIDatabase) != effectiveCredentialsMode(&n.Spec.Database) {
		allErrs = append(allErrs, field.Invalid(
			apiDatabasePath.Child("credentialsMode"), n.Spec.APIDatabase.CredentialsMode,
			"apiDatabase and database must use the same credentialsMode",
		))
	}

	// Defense-in-depth twin of the console CEL rule on NovaSpec: a disabled proxy
	// has no Deployment to size.
	if !n.Spec.ConsoleProxyEnabled() && n.Spec.ConsoleProxy.Deployment != nil {
		allErrs = append(allErrs, field.Forbidden(
			specPath.Child("consoleProxy", "deployment"),
			"consoleProxy.deployment must not be set when consoleProxy.enabled is false",
		))
	}

	allErrs = append(allErrs, validation.CacheXOR(specPath.Child("cache"), &n.Spec.Cache)...)
	// spec.cache reaches the verbatim INI renderer the same way the typed fields
	// below do: cache.ResolveServers derives the [cache] memcache_servers and
	// [keystone_authtoken] memcached_servers from spec.cache.servers or
	// spec.cache.clusterRef.name. This is the defense-in-depth twin of the items
	// pattern and XValidation rule the shared commonv1.CacheSpec carries, for
	// objects that bypass schema validation.
	allErrs = append(allErrs, validation.CacheNoControlChars(specPath.Child("cache"), &n.Spec.Cache)...)
	allErrs = append(allErrs, validation.MessagingXOR(specPath.Child("messaging"), &n.Spec.Messaging)...)
	allErrs = append(allErrs, validation.SecretStoreRef(specPath.Child("secretStoreRef"), n.Spec.SecretStoreRef)...)
	allErrs = append(allErrs, validation.TargetClusterRef(specPath.Child("targetClusterRef"), n.Spec.TargetClusterRef)...)

	// The five process Deployments carry the same pod-level knobs and are
	// validated by the same rules; only the API and the metadata front end
	// additionally run under uWSGI. Each block also carries the pod selector of
	// the Deployment it configures, which is what a topology-spread constraint on
	// that block has to name.
	blocks := []struct {
		path       *field.Path
		deployment *commonv1.DeploymentSpec
		selector   map[string]string
	}{
		{
			specPath.Child("api", "deployment"), &n.Spec.API.Deployment,
			APIPodSelector(n.Name),
		},
		{
			specPath.Child("metadata", "deployment"), &n.Spec.Metadata.Deployment,
			MetadataPodSelector(n.Name),
		},
		{
			specPath.Child("scheduler", "deployment"), &n.Spec.Scheduler.Deployment,
			SchedulerPodSelector(n.Name),
		},
		{
			specPath.Child("conductor", "deployment"), &n.Spec.Conductor.Deployment,
			ConductorPodSelector(n.Name),
		},
	}
	// The console block is validated only while it exists and the proxy is
	// projected: a disabled proxy's block is already rejected above, and pushing
	// it through the pod-level rules as well would report a replica floor on a
	// Deployment the operator never creates.
	if n.Spec.ConsoleProxyEnabled() && n.Spec.ConsoleProxy.Deployment != nil {
		blocks = append(blocks, struct {
			path       *field.Path
			deployment *commonv1.DeploymentSpec
			selector   map[string]string
		}{
			specPath.Child("consoleProxy", "deployment"), n.Spec.ConsoleProxy.Deployment,
			ConsoleProxyPodSelector(n.Name),
		})
	}
	for _, block := range blocks {
		allErrs = append(allErrs, w.validateDeploymentBlock(ctx, block.path, block.deployment, block.selector)...)
	}
	// The spec.jobs block: requests within limits, an existing priority
	// class, and its own placement.
	allErrs = append(allErrs, validation.Job(ctx, w.Client, specPath.Child("jobs"), n.Spec.Jobs)...)
	allErrs = append(allErrs, validateUWSGIHarakiri(
		specPath.Child("api", "uwsgi"), n.Spec.API.UWSGI, &n.Spec.API.Deployment)...)
	allErrs = append(allErrs, validateUWSGIHarakiri(
		specPath.Child("metadata", "uwsgi"), n.Spec.Metadata.UWSGI, &n.Spec.Metadata.Deployment)...)

	// Defense-in-depth worker floors alongside the
	// +kubebuilder:validation:Minimum=1 markers. A zero worker count starts a pod
	// that serves nothing and reports no error of its own.
	for _, block := range []struct {
		path    *field.Path
		workers *int32
	}{
		{specPath.Child("scheduler", "workers"), n.Spec.Scheduler.Workers},
		{specPath.Child("conductor", "workers"), n.Spec.Conductor.Workers},
	} {
		if block.workers != nil && *block.workers < 1 {
			allErrs = append(allErrs, field.Invalid(
				block.path, *block.workers, "workers must be at least 1",
			))
		}
	}

	// Keystone is not optional for Nova: an instance boot needs a Placement
	// allocation, a Neutron port and a Glance image, and all three are
	// authenticated calls. Defense in depth behind the MinLength marker.
	keystonePath := specPath.Child("keystoneEndpoint")
	if n.Spec.KeystoneEndpoint == "" {
		allErrs = append(allErrs, field.Required(keystonePath, "keystoneEndpoint must be set"))
	} else {
		allErrs = append(allErrs, validateEndpointURL(keystonePath, n.Spec.KeystoneEndpoint)...)
	}

	// Every endpoint is handed to a client library verbatim, so an unparseable
	// URL or a missing host would surface as a connection failure at runtime
	// rather than at admission. Each is optional here, so only a non-empty value
	// is measured. The schema-layer twin is the ^https?:// pattern each field
	// carries.
	endpointsPath := specPath.Child("endpoints")
	for _, endpoint := range []struct {
		path  *field.Path
		value string
	}{
		{specPath.Child("keystonePublicEndpoint"), n.Spec.KeystonePublicEndpoint},
		{endpointsPath.Child("placement", "override"), n.Spec.Endpoints.Placement.Override},
		{endpointsPath.Child("neutron", "override"), n.Spec.Endpoints.Neutron.Override},
		{endpointsPath.Child("glance", "override"), n.Spec.Endpoints.Glance.Override},
		{endpointsPath.Child("cinder", "override"), n.Spec.Endpoints.Cinder.Override},
		{endpointsPath.Child("barbican", "override"), n.Spec.Endpoints.Barbican.Override},
	} {
		if endpoint.value != "" {
			allErrs = append(allErrs, validateEndpointURL(endpoint.path, endpoint.value)...)
		}
	}

	allErrs = append(allErrs, validateRemoteCompute(specPath, &n.Spec)...)

	// Typed spec fields reach the same verbatim INI renderer as extraConfig: each
	// value below is rendered as "%s = %s" into nova.conf and into the
	// nova-compute.conf fragment of the compute-config Secret. A newline or
	// carriage return therefore injects an additional config line, smuggling a
	// whole section past the (section, key)-keyed ownership and catalog gates,
	// and into the fragment every hypervisor loads, which spec.extraConfig never
	// reaches. The URL fields need no entry here: validateEndpointURL's url.Parse
	// rejects control bytes, and spec.cache and spec.logging have their own
	// checks. The console gateway hostname is on the list because it is rendered
	// into [vnc] novncproxy_base_url.
	for _, f := range []struct {
		path  *field.Path
		value string
	}{
		{specPath.Child("region"), n.Spec.Region},
		{specPath.Child("serviceUser", "username"), n.Spec.ServiceUser.Username},
		{specPath.Child("serviceUser", "projectName"), n.Spec.ServiceUser.ProjectName},
		{specPath.Child("serviceUser", "userDomainName"), n.Spec.ServiceUser.UserDomainName},
		{specPath.Child("serviceUser", "projectDomainName"), n.Spec.ServiceUser.ProjectDomainName},
		{specPath.Child("consoleProxy", "gateway", "hostname"), consoleGatewayHostname(n)},
	} {
		if validation.HasControlChars(f.value) {
			allErrs = append(allErrs, field.Invalid(f.path, f.value,
				"value must not contain a newline or carriage return: it is rendered verbatim into "+
					"nova.conf and nova-compute.conf, so a newline injects arbitrary config lines"))
		}
	}

	// Defense in depth behind the MinLength marker on commonv1.SecretRefSpec.Name.
	// Both Secrets carry a value the operator cannot invent: the service-user
	// password, and the shared secret the Neutron metadata agent signs with.
	if n.Spec.ServiceUser.SecretRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("serviceUser", "secretRef", "name"),
			"secretRef.name must be set: it carries the Keystone service-user password",
		))
	}
	if n.Spec.Metadata.SharedSecretRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("metadata", "sharedSecretRef", "name"),
			"sharedSecretRef.name must be set: it carries the secret the Neutron metadata agent signs proxied requests with",
		))
	}

	// Defense-in-depth gateway validation alongside the
	// +kubebuilder:validation:MinLength=1 markers on GatewaySpec.Hostname and
	// GatewayParentRefSpec.Name. Nova publishes up to three hostnames: the API,
	// the metadata front end, and the console proxy, which the browser opens a
	// WebSocket against on a host of its own.
	allErrs = append(allErrs, validateGateway(specPath.Child("gateway"), n.Spec.Gateway)...)
	allErrs = append(allErrs, validateGateway(specPath.Child("metadata", "gateway"), n.Spec.Metadata.Gateway)...)
	allErrs = append(allErrs, validateGateway(specPath.Child("consoleProxy", "gateway"), n.Spec.ConsoleProxy.Gateway)...)
	// The console route carries no path prefix. The console URL the API hands a
	// browser names the page at the root of the console hostname, and the noVNC
	// client opens its WebSocket on the root as well, so a prefix match would
	// route neither while the route itself reports Accepted.
	if g := n.Spec.ConsoleProxy.Gateway; g != nil && g.Path != "" && g.Path != "/" {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("consoleProxy", "gateway", "path"), g.Path,
			"path must be empty or \"/\": the console page and its WebSocket are served from the root of the console hostname",
		))
	}

	allErrs = append(allErrs, validateLogging(specPath.Child("logging"), n.Spec.Logging, "nova.conf")...)

	// Defense-in-depth autoscaling validation alongside kubebuilder markers and
	// CEL rules. The HPA targets the API Deployment alone, so every cross-field
	// rule below reads spec.api.deployment.replicas.
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
		// When minReplicas is unset, the reconciler defaults it to the API
		// Deployment's replica count. Reject configurations where the implicit
		// default would exceed maxReplicas, which would produce an HPA rejected by
		// the API server.
		if n.Spec.Autoscaling.MinReplicas == nil && n.Spec.API.Deployment.Replicas > n.Spec.Autoscaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("maxReplicas"),
				n.Spec.Autoscaling.MaxReplicas,
				fmt.Sprintf("maxReplicas must be >= spec.api.deployment.replicas (%d) when minReplicas is not set, because minReplicas defaults to spec.api.deployment.replicas", n.Spec.API.Deployment.Replicas),
			))
		}
		// Defense-in-depth lower bound for utilization targets alongside the
		// +kubebuilder:validation:Minimum=1 markers. There is no upper bound,
		// as in autoscaling/v2: a target the API pod can never reach is
		// rejected below.
		if n.Spec.Autoscaling.TargetCPUUtilization != nil && *n.Spec.Autoscaling.TargetCPUUtilization < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetCPUUtilization"),
				*n.Spec.Autoscaling.TargetCPUUtilization,
				"targetCPUUtilization must be at least 1",
			))
		}
		if n.Spec.Autoscaling.TargetMemoryUtilization != nil && *n.Spec.Autoscaling.TargetMemoryUtilization < 1 {
			allErrs = append(allErrs, field.Invalid(
				autoscalingPath.Child("targetMemoryUtilization"),
				*n.Spec.Autoscaling.TargetMemoryUtilization,
				"targetMemoryUtilization must be at least 1",
			))
		}
		if n.Spec.Autoscaling.TargetCPUUtilization == nil && n.Spec.Autoscaling.TargetMemoryUtilization == nil {
			allErrs = append(allErrs, field.Required(
				autoscalingPath,
				"at least one of targetCPUUtilization or targetMemoryUtilization must be set",
			))
		}
		allErrs = append(allErrs, validation.AutoscalingBehavior(autoscalingPath.Child("behavior"), n.Spec.Autoscaling.Behavior)...)
	}

	// An HPA utilization target is measured against the summed requests of
	// every container in the API pod, so a zero request under a target either
	// fails the metric or inflates it. The render-time default fills a positive
	// request when the block names none.
	allErrs = append(allErrs, validation.AutoscalingTargetRequests(specPath.Child("api", "deployment", "resources"), n.Spec.API.Deployment.Resources, n.Spec.Autoscaling)...)
	// A target above 100 needs a container of the API pod that may use more
	// than it requests, so a target the containers' limits make unreachable
	// is rejected.
	allErrs = append(allErrs, validation.AutoscalingTargetsReachable(specPath.Child("autoscaling"), n.Spec.Autoscaling, n.Spec.API.Deployment.Resources)...)

	// Defense-in-depth networkPolicy ingress check alongside the
	// +kubebuilder:validation:XValidation CEL rule on NetworkPolicySpec.
	if n.Spec.NetworkPolicy != nil && len(n.Spec.NetworkPolicy.Ingress) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("networkPolicy", "ingress"),
			"at least one ingress source must be specified",
		))
	}

	allErrs = append(allErrs, validateDBArchive(specPath.Child("dbArchive"), n.Spec.DBArchive)...)
	allErrs = append(allErrs, validateExtraConfigShape(specPath, n.Spec.ExtraConfig, OwnedConfigKeys)...)

	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Nova"},
			n.Name,
			allErrs,
		)
	}
	return nil
}

// maxCellDatabaseNameLength bounds spec.database.database so the cell0 schema
// derived from it, "<database>_cell0", still fits the 64-character schema limit
// the shared DatabaseSpec pattern enforces.
const maxCellDatabaseNameLength = 64 - len("_cell0")

// consoleGatewayHostname returns the console gateway's hostname, or the empty
// string when the console proxy is not exposed through a gateway.
func consoleGatewayHostname(n *Nova) string {
	if n.Spec.ConsoleProxy.Gateway == nil {
		return ""
	}
	return n.Spec.ConsoleProxy.Gateway.Hostname
}

// effectiveCredentialsMode resolves the credential mode a database block runs
// with: the explicit value, or Static when the field is empty. The two database
// blocks are compared through it so an empty field on one and a spelled-out
// "Static" on the other read as the same mode, exactly as the CEL rule on
// NovaSpec resolves them.
func effectiveCredentialsMode(db *commonv1.DatabaseSpec) string {
	if db.CredentialsMode == "" {
		return commonv1.CredentialsModeStatic
	}
	return db.CredentialsMode
}

// validateDeploymentBlock runs the pod-level rules every Nova Deployment block
// shares: the replica floor, the graceful-termination arithmetic, the strategy
// sanity check, the request/limit ordering, and the two cluster-scoped lookups.
// selector is the pod selector of the Deployment this block configures, which a
// topology-spread constraint on it has to name exactly.
//
// ctx is required for the PriorityClass lookup, which is skipped when no reader
// is injected (a programmatically constructed webhook), mirroring the shared
// validator's own behavior.
func (w *NovaWebhook) validateDeploymentBlock(
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

	// Node selector grammar and tolerations of the Deployment.
	errs = append(errs, validation.NodePlacement(fldPath, &d.NodePlacementSpec)...)

	// Validate that custom TopologySpreadConstraints use the correct
	// LabelSelector matching the Deployment's selector labels.
	if d.TopologySpreadConstraints != nil {
		errs = append(errs, validation.TopologySpreadSelector(
			fldPath.Child("topologySpreadConstraints"),
			d.TopologySpreadConstraints,
			selector,
		)...)
	}
	return errs
}

// APIPodSelector returns the pod selector of the API Deployment of the Nova
// named name, which the labelSelector of every custom topology spread
// constraint on spec.api.deployment must equal. The ControlPlane reads it and
// its component twins below to complete the spread constraints it projects.
func APIPodSelector(name string) map[string]string {
	return naming.APISelectorLabels(novaAppName, name)
}

// MetadataPodSelector is the APIPodSelector twin for spec.metadata.deployment.
func MetadataPodSelector(name string) map[string]string {
	return componentSelectorLabels(name, componentMetadata)
}

// SchedulerPodSelector is the APIPodSelector twin for spec.scheduler.deployment.
func SchedulerPodSelector(name string) map[string]string {
	return componentSelectorLabels(name, componentScheduler)
}

// ConductorPodSelector is the APIPodSelector twin for spec.conductor.deployment.
func ConductorPodSelector(name string) map[string]string {
	return componentSelectorLabels(name, componentConductor)
}

// ConsoleProxyPodSelector is the APIPodSelector twin for
// spec.consoleProxy.deployment.
func ConsoleProxyPodSelector(name string) map[string]string {
	return componentSelectorLabels(name, componentConsoleProxy)
}

// componentSelectorLabels returns the pod selector of the Deployment of one
// non-API component: the shared selector labels narrowed by that component. It
// is the api-package twin of the controller helper of the same name.
func componentSelectorLabels(novaName, component string) map[string]string {
	labels := naming.SelectorLabels(novaAppName, novaName)
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
// It reaches the two HTTP front ends alone: the scheduler, conductor and console
// proxy run no application server. It is nil-safe on both arguments' optional
// halves.
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

// validateEndpointURL checks that a non-empty endpoint parses cleanly, uses an
// http(s) scheme, and carries a host, the same shape the sibling operators
// enforce on their endpoints. It is applied to every URL field on NovaSpec.
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

// validateRemoteCompute checks the remote compute block while it is set. The
// Keystone URL gets the shape check every URL field on NovaSpec gets; its
// url.Parse also refuses a control character, so a newline cannot reach the
// remote fragment through it. A well-formed Keystone URL must then use https,
// the twin of the field's ^https:// pattern. The Secret name is the defense in
// depth behind the MinLength marker on commonv1.SecretRefSpec.Name, and the TLS
// check is the twin of the remote-compute CEL rule on NovaSpec, with the
// message repeating the rule's reason.
func validateRemoteCompute(specPath *field.Path, spec *NovaSpec) field.ErrorList {
	rc := spec.RemoteCompute
	if rc == nil {
		return nil
	}
	var errs field.ErrorList
	rcPath := specPath.Child("remoteCompute")
	ksPath := rcPath.Child("keystoneEndpoint")
	if urlErrs := validateEndpointURL(ksPath, rc.KeystoneEndpoint); len(urlErrs) > 0 {
		errs = append(errs, urlErrs...)
	} else if u, _ := url.Parse(rc.KeystoneEndpoint); u.Scheme != "https" {
		errs = append(errs, field.Invalid(ksPath, rc.KeystoneEndpoint,
			"must use scheme https: every compute on another cluster sends the nova service-user password "+
				"to this URL across a cluster boundary"))
	}
	if rc.TransportURLSecretRef.Name == "" {
		errs = append(errs, field.Required(rcPath.Child("transportURLSecretRef", "name"),
			"transportURLSecretRef.name must be set: it carries the transport URL of the broker's external listener"))
	}
	if spec.Messaging.TLS == nil {
		errs = append(errs, field.Required(specPath.Child("messaging", "tls"),
			"is required when spec.remoteCompute is set: a compute on another cluster verifies the broker "+
				"against this CA bundle"))
	}
	return errs
}

// validateGateway requires the two halves an HTTPRoute cannot be built without:
// the hostname it matches and the Gateway it attaches to. fldPath names the
// block under validation, so the same rules report against spec.gateway,
// spec.metadata.gateway and spec.consoleProxy.gateway. A nil block carries
// nothing to validate.
func validateGateway(fldPath *field.Path, gateway *commonv1.GatewaySpec) field.ErrorList {
	if gateway == nil {
		return nil
	}
	var errs field.ErrorList
	if gateway.Hostname == "" {
		errs = append(errs, field.Required(
			fldPath.Child("hostname"),
			fmt.Sprintf("hostname must be set when %s is configured", fldPath),
		))
	}
	if gateway.ParentRef.Name == "" {
		errs = append(errs, field.Required(
			fldPath.Child("parentRef", "name"),
			fmt.Sprintf("parentRef.name must be set when %s is configured", fldPath),
		))
	}
	return errs
}

// validateDBArchive mirrors the spec.dbArchive schema as defense in depth behind
// the CRD layer (the Minimum markers) and carries the one check with no schema
// counterpart: the cron grammar, which DBArchiveSpec.Schedule deliberately leaves
// to the webhook. It is nil-safe, and so is each half: an unset field carries
// nothing to validate and is resolved to the operator default at reconcile time.
func validateDBArchive(fldPath *field.Path, a *DBArchiveSpec) field.ErrorList {
	if a == nil {
		return nil
	}
	var errs field.ErrorList

	if a.Schedule != "" {
		errs = append(errs, validation.CronSchedule(fldPath.Child("schedule"), a.Schedule)...)
	}
	if a.MaxRows != nil && *a.MaxRows < 1 {
		errs = append(errs, field.Invalid(
			fldPath.Child("maxRows"), *a.MaxRows, "maxRows must be at least 1",
		))
	}
	if a.Sleep != nil && *a.Sleep < 0 {
		errs = append(errs, field.Invalid(
			fldPath.Child("sleep"), *a.Sleep, "sleep must be non-negative",
		))
	}
	if a.RetentionDays != nil && *a.RetentionDays < 1 {
		errs = append(errs, field.Invalid(
			fldPath.Child("retentionDays"), *a.RetentionDays, "retentionDays must be at least 1",
		))
	}
	return errs
}

// warnDBArchiveRetention surfaces an update that widens what the next archive
// run moves out of the live tables: a shortened window, or a window dropped
// after it was set, which leaves every soft-deleted row eligible. The rows land
// in the shadow tables rather than disappearing, so this stays a warning, but
// the scope change is echoed back to whoever made it because a typo (3 for 30)
// is indistinguishable from an intended edit at admission time.
//
// Setting a window where there was none narrows the scope and is silent, and so
// is raising one.
func warnDBArchiveRetention(oldArchive, newArchive *DBArchiveSpec) admission.Warnings {
	oldDays := effectiveRetentionDays(oldArchive)
	if oldDays == nil {
		return nil
	}
	newDays := effectiveRetentionDays(newArchive)
	if newDays == nil {
		return admission.Warnings{fmt.Sprintf(
			"spec.dbArchive.retentionDays removed (was %d): the next archive run moves every soft-deleted row into the shadow tables, including the ones deleted today",
			*oldDays,
		)}
	}
	if *newDays >= *oldDays {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.dbArchive.retentionDays reduced %d to %d: the next archive run also moves the rows soft-deleted between %d and %d days ago",
		*oldDays, *newDays, *newDays, *oldDays,
	)}
}

// effectiveRetentionDays resolves the retention a spec.dbArchive block runs
// with. Unlike the sibling knobs it has no operator default: nil means no
// --before argument and therefore no window at all, which is why the result is a
// pointer rather than a number.
func effectiveRetentionDays(a *DBArchiveSpec) *int32 {
	if a == nil {
		return nil
	}
	return a.RetentionDays
}
