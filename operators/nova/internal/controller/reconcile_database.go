// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/job"
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/release"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// Condition reason constants for DatabaseReady set on the Nova-specific paths.
// The steady-state and expand-migrate-contract upgrade reasons live in
// internal/common/database as database.Reason*.
const (
	// conditionReasonDatabaseWaitingForConfig is set while no rendered config
	// exists: the migration Jobs mount the config ConfigMap as their whole config
	// directory, so an empty name would render a volume the API server rejects
	// and every pass would fail on the Job create.
	conditionReasonDatabaseWaitingForConfig = "WaitingForConfig"
	// conditionReasonImageReleaseMismatch flags the operator error where the
	// tag-pinned spec.image names a different OpenStack release than
	// spec.openStackRelease. Release tracking and the expand-migrate-contract
	// upgrade detection key on spec.openStackRelease while every migration Job
	// and every workload run spec.image; the reconcile refuses to advance until
	// the two agree rather than promoting an installed-release marker for a
	// release the image is not.
	conditionReasonImageReleaseMismatch = "ImageReleaseMismatch"
)

// workloadRole names the Nova process a built environment or workload belongs
// to. Nova runs five long-lived processes plus the migration Jobs and the
// archive CronJob, and they do not read the same options: only the two HTTP
// front ends read a paste configuration, only the metadata API reads the shared
// secret, and only the console proxy stays away from the nova_api schema.
type workloadRole string

const (
	roleAPI          workloadRole = "api"
	roleMetadata     workloadRole = "metadata"
	roleScheduler    workloadRole = "scheduler"
	roleConductor    workloadRole = "conductor"
	roleConsoleProxy workloadRole = "novncproxy"
	// roleManage is the nova-manage role: the migration Jobs and the archive
	// CronJob. It reads both schemas and no paste configuration.
	roleManage workloadRole = "manage"
)

// dbBlock selects one of the two database blocks of a Nova. Every helper that
// differs between them (the connection cap, the TLS projection) takes it rather
// than a spec pointer, so a caller cannot pair the nova_api schema with the cell
// schema's mount path.
type dbBlock int

const (
	// blockAPI is spec.apiDatabase, the nova_api schema holding the cell map,
	// the flavors and the instance mappings.
	blockAPI dbBlock = iota
	// blockCell is spec.database, the cell schema holding the instances.
	blockCell
)

// The pod volume names the two db-tls client keypairs are projected through
// into the migration Jobs and the workloads. Each schema carries its own
// certificate references, so each gets its own volume.
const (
	apiDBTLSVolumeName  = "api-db-tls"
	cellDBTLSVolumeName = "cell-db-tls"
)

// apiDatabaseSection is the oslo.config group Nova keeps the nova_api schema's
// connection URL in. database.ConnectionEnvVarForSection renders it as the
// OS_API_DATABASE__CONNECTION override.
const apiDatabaseSection = "api_database"

// apiPasteConfigPath is the paste configuration the two HTTP front ends load
// their WSGI pipeline from. The nova image ships it outside the config
// directory, and nova/api/openstack/wsgi_app.py joins every OS_NOVA_CONFIG_FILES
// entry to OS_NOVA_CONFIG_DIR with os.path.join, which returns an absolute entry
// unchanged. The first entry is the one deploy.loadapp reads as the paste
// configuration, so its position in the list is fixed.
const apiPasteConfigPath = "/var/lib/openstack/etc/nova/api-paste.ini"

// The oslo.config env overrides the two HTTP front ends are started with.
// OS_NOVA_CONFIG_DIR names the directory the rendered ConfigMap is mounted at,
// and OS_NOVA_CONFIG_FILES the documents inside it the front end reads, with the
// absolute paste configuration first.
const (
	novaConfigDirEnvVarName   = "OS_NOVA_CONFIG_DIR"
	novaConfigFilesEnvVarName = "OS_NOVA_CONFIG_FILES"
)

// metadataSharedSecretEnvVarName is the oslo.config env override for [neutron]
// metadata_proxy_shared_secret, the value the metadata API verifies a proxied
// request's signature with. The OS_<GROUP>__<OPTION> form wins over the rendered
// file at runtime, so the secret reaches the process from the referenced Secret
// rather than from the ConfigMap every pod mounts.
// #nosec G101 -- an oslo.config env override key, not a credential.
const metadataSharedSecretEnvVarName = "OS_NEUTRON__METADATA_PROXY_SHARED_SECRET"

// Job name suffixes of the steady-state sync and the three upgrade phases. They
// are the shared flow's own vocabulary (internal/common/database), repeated here
// because the phase Job builder, the terminal-state reporter and the cells
// report all key on them.
const (
	dbSyncJobSuffix          = "db-sync"
	upgradeExpandJobSuffix   = "db-expand"
	upgradeMigrateJobSuffix  = "db-migrate"
	upgradeContractJobSuffix = "db-contract"
)

// The dedupe keys of the two reports read off a finished Job's pod. Each reports
// once per Job UID, independently of the terminal metric the same Job emits under
// its own suffix, so each needs a key of its own.
const (
	cellsReportJobSuffix  = dbSyncJobSuffix + "-cells"
	upgradeCheckJobSuffix = upgradeExpandJobSuffix + "-check"
)

// computeCellName is the name of the single real cell the db-sync Job maps and
// the cell name the compute contract hands a nova-compute, which registers into
// it. The two have to agree: a compute handed any other name would look for a
// cell that does not exist.
const computeCellName = "cell1"

// upgradePhaseJobBackoffLimit is how often an expand/migrate/contract Job is
// retried before it is declared failed. Against an unreachable database one
// attempt takes about three and a half minutes, so four retries cover a restart
// of the database without wedging the phase on a permanent failure.
const upgradePhaseJobBackoffLimit int32 = 4

// terminationLogPath is the file the kubelet reads a terminated container's
// message off. The db-sync Job writes the cell report there and the expand phase
// the nova-status exit code, which is how reportCells and reportUpgradeCheck get
// them without streaming the pod's logs.
const terminationLogPath = "/dev/termination-log"

// cellsReportAwk turns the "nova-manage cell_v2 list_cells" table into one
// "<name>=<uuid>" line per cell. The table is ASCII art: rows 1 to 3 are the top
// rule, the header and the rule under it, every data row carries the name in
// column 2 and the UUID in column 3 of a pipe-separated line, and the bottom
// rule carries no pipe at all, which leaves its column 2 empty and skipped.
const cellsReportAwk = `NR>3 && $2 !~ /^ *-/ {gsub(/ /,"",$2); gsub(/ /,"",$3); if ($2 != "") print $2"="$3}`

// cellMappedAwk exits 0 when the "nova-manage cell_v2 list_cells" table already
// maps the real cell, and 1 when it does not. A row counts when its name (column
// 2) is the name the Job creates the cell under, or when its database connection
// (column 5, the expanded URL with only the password masked) names the cell
// schema: a brownfield cell mapped under another name is still the cell this
// schema holds, and a second mapping onto it would list every instance twice.
// The schema is compared on the URL path alone, with the query stripped, so
// cell0's "<schema>_cell0" never matches it. Matching whole fields rather than
// grepping the table matters: the expanded URLs carry the user, the host and the
// vhost, any of which may contain the cell name as a word.
const cellMappedAwk = `NR>3 {n=$2; d=$5; gsub(/ /,"",n); gsub(/ /,"",d); sub(/[?].*/,"",d); ` +
	`if (n == name || (length(d) > length(schema) && substr(d, length(d)-length(schema)+1) == schema)) f=1} ` +
	`END {exit !f}`

// cellsReportPattern matches one line of that report. The UUID is matched
// strictly so a stray line of nova-manage output can never be mistaken for a
// cell the operator then publishes in status.
var cellsReportPattern = regexp.MustCompile(`^([A-Za-z0-9_-]+)=([0-9a-f-]{36})$`)

// The transport-URL and database-connection templates the cell mapping is
// written with. nova expands the placeholders from the connection URLs the Job
// already runs with, so the operator never has to assemble a URL carrying a
// password: {path} is the broker's vhost, and the schema is the only part of the
// database URL that differs per cell.
const cellTransportURLTemplate = "{scheme}://{username}:{password}@{hostname}:{port}/{path}?{query}"

// cellDatabaseTemplate is the database-connection template of one cell, the
// running connection with its schema replaced.
func cellDatabaseTemplate(schema string) string {
	return "{scheme}://{username}:{password}@{hostname}:{port}/" + schema + "?{query}"
}

// The expand phase's command, split at its destination so the script test can
// run these exact bytes against a path it is allowed to write. Nothing but the
// tests uses the two halves on their own.
const (
	upgradeExpandScriptHead = "rc=0; nova-status --config-dir " + novaConfigDir + " upgrade check || rc=$?; " +
		"echo \"nova-status upgrade check exit $rc\" > "
	upgradeExpandScriptTail = "; case \"$rc\" in 0|1) ;; *) exit \"$rc\";; esac; " +
		"nova-manage --config-dir " + novaConfigDir + " api_db sync; " +
		"nova-manage --config-dir " + novaConfigDir + " db sync"

	// upgradeExpandScript runs nova-status upgrade check before the two schema
	// migrations of the expand phase. Its exit code is a severity rather than a
	// success flag: 0 is clean and 1 carries warnings, which a deployment without
	// Cinder keeps because the volume-attachment checks report on an integration
	// it does not run. 2 and above mean a check failed, typically because the
	// cell mapping the db-sync Job writes is not there yet, and those must stop
	// the phase rather than migrate against a half-set-up deployment. The real
	// exit code travels in the termination log, which reportUpgradeCheck turns
	// into an event on the Nova.
	upgradeExpandScript = upgradeExpandScriptHead + terminationLogPath + upgradeExpandScriptTail
)

// upgradeMigrateCommand is the migrate phase's command. Nova has no migrate verb
// of its own: expand has already applied both schemas' migrations, so the phase
// is a gate that the new code can read the nova_api schema it just migrated.
// list_cells is that read, and the shared flow builds a Job for every Job phase,
// so the gate is a command rather than a skipped phase.
var upgradeMigrateCommand = []string{"nova-manage", "--config-dir", novaConfigDir, "cell_v2", "list_cells"}

// onlineDataMigrationBatch is the --max-count of one online data migration
// batch. A thousand rows keeps each batch's transactions short enough to share
// the tables with the fleet the rolling update left serving, and the contract
// loop repeats batches until nothing is left.
const onlineDataMigrationBatch = "1000"

// upgradeContractScript runs the online data migrations to exhaustion, for the
// cell schema and then for cell0. nova reports "there is more to do" with exit
// code 1 and "nothing left" with 0, one bounded batch at a time, so the loop is
// what finishes the backfill the contract phase owes the new schema. The
// "|| rc=$?" form keeps the shell's -e from aborting the loop on the 1 that means
// progress; anything above 1 is a failure and fails the Job with its own code.
//
// online_data_migrations works on the one cell database [database] connection
// names and, unlike db sync, does not fan out to cell0. cell0 holds the instances
// that failed scheduling, which nova-api reads on every instance list, so a row
// written under an older release that never got its backfill breaks the listing
// once a later release drops the code that reads the old format. The second pass
// reuses the running connection with "_cell0" appended to the schema, which is
// how cell0 is provisioned: the query, and with it the TLS parameters, stays.
const upgradeContractScript = "odm() { while :; do rc=0; nova-manage --config-dir " + novaConfigDir +
	" db online_data_migrations --max-count " + onlineDataMigrationBatch + " || rc=$?; " +
	"case \"$rc\" in 0) return 0;; 1) ;; *) return \"$rc\";; esac; done; }; " +
	"odm; " +
	"base=\"${OS_DATABASE__CONNECTION%%[?]*}\"; " +
	"OS_DATABASE__CONNECTION=\"${base}_cell0${OS_DATABASE__CONNECTION#\"$base\"}\"; " +
	"export OS_DATABASE__CONNECTION; odm"

// cell0Schema is the SQL schema of cell0, the holding pen nova records an
// instance in that never reached a cell. It is derived from the cell schema
// rather than configured: nova-manage maps it by convention, and both names are
// provisioned on the same block's user.
func cell0Schema(nova *novav1alpha1.Nova) string {
	return nova.Spec.Database.Database + "_cell0"
}

// blockDatabase returns the DatabaseSpec of one block.
func blockDatabase(nova *novav1alpha1.Nova, block dbBlock) *commonv1.DatabaseSpec {
	if block == blockAPI {
		return &nova.Spec.APIDatabase
	}
	return &nova.Spec.Database
}

// novaMaxUserConnections sizes one block's SQL user max_user_connections cap for
// the CR's own topology. Every term is a fleet member's steady-state connection
// count, doubled, because oslo.db keeps a second pooled connection open behind
// the one a request is served on:
//
//   - the API, pods × processes × threads, with pods being the autoscaling
//     ceiling when an HPA owns the replica count;
//   - the metadata API, the same product over its own Deployment and uWSGI
//     block, which is sized by the instance population rather than by API
//     traffic;
//   - the scheduler and the conductor, replicas × worker processes each, since
//     a worker is a process with a pool of its own.
//
// The +1 on every replica count is the rolling-update surge: the strategy runs a
// full extra pod's workers alongside the fleet during an update. The trailing +4
// is the migration Jobs' headroom, two connections for each of the two schemas a
// db-sync or a phase Job opens while the fleet keeps serving.
//
// The console proxy is on the cell block alone: it reads the console tokens out
// of the cell schema and never opens nova_api.
//
// A cap below the fleet's steady state does not degrade gracefully: the last
// processes to start fail their pool with MySQL error 1226 and crash-loop their
// pod. Left unsized, the mariadb-operator CRD default of 10 applies, which the
// default topology exceeds before a single request is served.
func novaMaxUserConnections(nova *novav1alpha1.Nova, block dbBlock) int32 {
	spec := &nova.Spec

	apiPods := deployment.EffectiveReplicas(&spec.API.Deployment)
	if spec.Autoscaling != nil {
		apiPods = spec.Autoscaling.MaxReplicas
	}
	apiProcesses, apiThreads := deployment.EffectiveUWSGIConcurrency(spec.API.UWSGI)
	metadataProcesses, metadataThreads := deployment.EffectiveUWSGIConcurrency(spec.Metadata.UWSGI)

	total := (apiPods+1)*apiProcesses*apiThreads +
		(deployment.EffectiveReplicas(&spec.Metadata.Deployment)+1)*metadataProcesses*metadataThreads +
		(deployment.EffectiveReplicas(&spec.Scheduler.Deployment)+1)*effectiveWorkers(spec.Scheduler.Workers) +
		(deployment.EffectiveReplicas(&spec.Conductor.Deployment)+1)*effectiveWorkers(spec.Conductor.Workers)
	total = 2*total + 4

	if block == blockCell && spec.ConsoleProxyEnabled() {
		total += 2 * (consoleProxyReplicas(nova) + 1)
	}
	return total
}

// consoleProxyReplicas resolves the console proxy's replica count. The block is
// a pointer the defaulting webhook only materializes while the proxy is enabled,
// so an absent block is the one replica it would have been given; a present
// block resolves the way the workload builder resolves it.
func consoleProxyReplicas(nova *novav1alpha1.Nova) int32 {
	if nova.Spec.ConsoleProxy.Deployment == nil {
		return 1
	}
	return deployment.EffectiveReplicas(nova.Spec.ConsoleProxy.Deployment)
}

// reconcileDatabase provisions and migrates Nova's two database schemas and
// tracks the installed OpenStack release. It runs the shared provisioning flow
// twice, once per block: the nova_api schema first, then the cell schema
// together with cell0, which is an additional schema on the cell block's user
// rather than a block of its own. A release transition (a non-patch change from
// the installed release) runs the shared expand-migrate-contract upgrade flow,
// walking Expanding → Migrating → RollingUpdate → Contracting; fresh installs
// and patch bumps stay on the single-pass db-sync path.
//
// configMapName names the rendered config ConfigMap the migration Jobs mount.
func (r *NovaReconciler) reconcileDatabase(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, configMapName string,
) (ctrl.Result, error) {
	// The nova_api block: MariaDB cluster gate, Database/User/Grant ensure,
	// Dynamic-credentials skip of the User/Grant. A non-zero result means the
	// flow set a not-ready condition and we must return it unchanged.
	res, err := database.ReconcileProvision(ctx, database.ProvisionFlowParams{
		Client:             children,
		Scheme:             r.Scheme,
		Owner:              nova,
		InstanceName:       apiInstanceName(nova),
		Namespace:          nova.Namespace,
		Database:           &nova.Spec.APIDatabase,
		Conditions:         &nova.Status.Conditions,
		Generation:         nova.Generation,
		ConditionType:      "DatabaseReady",
		RequeueAfter:       RequeueDatabaseWait,
		MaxUserConnections: novaMaxUserConnections(nova, blockAPI),
	})
	if err != nil || !res.IsZero() {
		return res, err
	}

	// The cell block. cell0 travels as an additional schema so it is created and
	// granted on the same user: nova-manage maps it into the cell map with the
	// running credentials, and a separate user would leave the conductor unable
	// to read the instances it holds.
	res, err = database.ReconcileProvision(ctx, database.ProvisionFlowParams{
		Client:                  children,
		Scheme:                  r.Scheme,
		Owner:                   nova,
		InstanceName:            nova.Name,
		Namespace:               nova.Namespace,
		Database:                &nova.Spec.Database,
		AdditionalDatabaseNames: []string{cell0Schema(nova)},
		Conditions:              &nova.Status.Conditions,
		Generation:              nova.Generation,
		ConditionType:           "DatabaseReady",
		RequeueAfter:            RequeueDatabaseWait,
		MaxUserConnections:      novaMaxUserConnections(nova, blockCell),
	})
	if err != nil || !res.IsZero() {
		return res, err
	}

	// No config yet means nothing has been rendered, and the migration Jobs mount
	// that ConfigMap as their whole config directory. Wait rather than migrate
	// against an empty mount. Mid-upgrade an empty name is unreachable anyway:
	// the config step recovers the last-good name off the live Deployment.
	if configMapName == "" {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "DatabaseReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonDatabaseWaitingForConfig,
			Message:            "Waiting for the rendered config before provisioning the database schema",
		})
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}

	// Active upgrade: the shared flow handles the abort (spec.openStackRelease
	// reverted to the installed release), the target-changed guard, and the phase
	// dispatch.
	if nova.Status.UpgradePhase != "" {
		// Enforce the decoupled-field contract mid-upgrade too. The shared flow's
		// target-changed guard only watches spec.openStackRelease, so a lone
		// spec.image.tag edit to an inconsistent release would otherwise slip
		// through and dispatch phase Jobs built from the wrong image (every phase
		// Job runs spec.image). Skip the check when spec.openStackRelease has been
		// reverted to the installed release: that is the shared flow's abort
		// trigger (SpecRelease == InstalledRelease in ReconcileUpgrade), which must
		// stay reachable even while the two fields disagree so a wedged upgrade can
		// always be unstuck.
		if nova.Spec.OpenStackRelease != nova.Status.InstalledRelease {
			if res, blocked := checkImageReleaseMismatch(nova); blocked {
				return res, nil
			}
		}
		return database.ReconcileUpgrade(ctx, r.upgradeFlowParams(ctx, children, nova, configMapName))
	}

	// Enforce the decoupled-field contract before either the upgrade or the
	// steady-state path advances release tracking (see checkImageReleaseMismatch).
	if res, blocked := checkImageReleaseMismatch(nova); blocked {
		return res, nil
	}

	// Detect a release upgrade (patch-only and same-release changes stay on the
	// steady-state path). A spec release older than the installed one reaches
	// InitiateUpgrade too, which reports DowngradeNotSupported.
	if database.IsUpgrade(nova.Spec.OpenStackRelease, nova.Status.InstalledRelease) {
		return database.InitiateUpgrade(ctx, r.upgradeFlowParams(ctx, children, nova, configMapName))
	}

	// Steady-state db-sync. The Job migrates both schemas and maps the cells in
	// one pass, so there is no schema-check step (SchemaCheckCommand nil).
	// InstalledRelease is promoted to spec.openStackRelease on Job success.
	return database.ReconcileSyncJobs(ctx, database.SyncFlowParams{
		Client:   children,
		Scheme:   r.Scheme,
		Recorder: r.Recorder,
		Owner:    nova,
		Jobs:     novaJobSetParams(nova, configMapName),
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, nova, jobSuffix, observed)
			// The cell report is the tail of the same Job: a completed db-sync has
			// written the cell map, and its termination log is the only place the
			// UUIDs nova generated can be read back from.
			if jobSuffix == dbSyncJobSuffix && observed != nil &&
				job.TerminalCondition(observed) == batchv1.JobComplete {
				r.reportCells(ctx, children, nova, observed)
			}
		},
		Conditions:       &nova.Status.Conditions,
		Generation:       nova.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueDatabaseWait,
		InstalledRelease: &nova.Status.InstalledRelease,
		// The release the marker is promoted to on success. It is
		// spec.openStackRelease rather than spec.image.tag so a digest-pinned image
		// still tracks the release the operator was told to converge to.
		ImageTag: nova.Spec.OpenStackRelease,
	})
}

// checkImageReleaseMismatch enforces the decoupled-field contract between
// spec.openStackRelease and spec.image. spec.openStackRelease drives release
// tracking and the upgrade detection, but every migration Job and every workload
// runs spec.image. The two fields are deliberately separate (digest pinning) and
// nothing else enforces they agree. A tag-pinned image whose tag names a
// different OpenStack release would run the wrong nova-manage binary against a
// schema already at its own HEAD: the expand/migrate/contract Jobs exit 0 as
// no-ops while the flow promotes status.installedRelease to a release the pods
// neither run nor migrated to.
//
// When the two disagree it sets DatabaseReady=False/ImageReleaseMismatch and
// returns (requeue, true) so the caller refuses to advance until the image is
// bumped in lockstep. A digest-pinned image (Tag == "") or a tag that does not
// parse as a release carries no comparable release string and is left to the
// operator's explicit spec.openStackRelease declaration, matching the
// digest-disables-tracking contract; the patch suffix is ignored so a patched
// image build (e.g. tag 2026.1-p1) still matches release 2026.1.
func checkImageReleaseMismatch(nova *novav1alpha1.Nova) (ctrl.Result, bool) {
	if nova.Spec.Image.Tag == "" {
		return ctrl.Result{}, false
	}
	tagRel, tagErr := release.ParseRelease(nova.Spec.Image.Tag)
	specRel, specErr := release.ParseRelease(nova.Spec.OpenStackRelease)
	if tagErr != nil || specErr != nil {
		return ctrl.Result{}, false
	}
	if tagRel.Year == specRel.Year && tagRel.Minor == specRel.Minor {
		return ctrl.Result{}, false
	}
	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: nova.Generation,
		Reason:             conditionReasonImageReleaseMismatch,
		Message: fmt.Sprintf("spec.image.tag %q names OpenStack release %d.%d but spec.openStackRelease is %q; "+
			"bump spec.image in lockstep with spec.openStackRelease",
			nova.Spec.Image.Tag, tagRel.Year, tagRel.Minor, nova.Spec.OpenStackRelease),
	})
	return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, true
}

// novaWorkloadEnv is the environment one Nova process runs with. Every role gets
// the message-bus transport URL, the cell database URL and the four Keystone
// passwords; what the role adds on top is what separates the processes:
//
//   - every role but the console proxy gets the nova_api URL as well. The proxy
//     reads console tokens out of the cell schema alone, and an override for a
//     section it does not configure would only be an unused Secret reference;
//   - the metadata API gets the shared secret it verifies a proxied request's
//     signature with;
//   - the two HTTP front ends get the config directory and the document list
//     their WSGI application loads, the metadata API with its own overlay behind
//     the shared document.
//
// All of them are oslo.config OS_<GROUP>__<OPTION> overrides sourced from
// Secrets, which is what keeps the credentials out of the rendered config the
// pods mount. The order is fixed (bus, databases, passwords, role extras) so a
// rebuilt pod template is byte-identical to the live one and no workload rolls
// on a reordered slice.
func novaWorkloadEnv(nova *novav1alpha1.Nova, role workloadRole) []corev1.EnvVar {
	secretName := nova.Spec.ServiceUser.SecretRef.Name
	key := effectiveServiceUserKey(nova)

	env := []corev1.EnvVar{
		messaging.TransportURLEnvVar(nova.Name),
		database.ConnectionEnvVar(nova.Name),
	}
	if role != roleConsoleProxy {
		env = append(env, database.ConnectionEnvVarForSection(apiInstanceName(nova), apiDatabaseSection))
	}
	env = append(env,
		keystoneauth.PasswordEnvVar(secretName, key),
		keystoneauth.ServiceUserPasswordEnvVar(secretName, key),
		keystoneauth.ClientPasswordEnvVar("placement", secretName, key),
		keystoneauth.ClientPasswordEnvVar("neutron", secretName, key))
	// Glance and Barbican carry no password of their own: nova reaches both with
	// the token of the request it is serving, backed by the outgoing service
	// token.
	if nova.Spec.Endpoints.Cinder.Enabled {
		env = append(env, keystoneauth.ClientPasswordEnvVar("cinder", secretName, key))
	}

	switch role {
	case roleAPI:
		env = append(env, novaConfigDirEnvVar(), novaConfigFilesEnvVar(novaConfDataKey))
	case roleMetadata:
		env = append(env, metadataSharedSecretEnvVar(nova),
			novaConfigDirEnvVar(), novaConfigFilesEnvVar(novaConfDataKey, metadataConfDataKey))
	case roleScheduler, roleConductor, roleConsoleProxy, roleManage:
		// The four non-HTTP roles read the whole config directory through
		// --config-dir and have no paste pipeline to load.
	}
	return env
}

// novaConfigDirEnvVar points the WSGI application at the mounted config
// directory.
func novaConfigDirEnvVar() corev1.EnvVar {
	return corev1.EnvVar{Name: novaConfigDirEnvVarName, Value: novaConfigDir}
}

// novaConfigFilesEnvVar lists the documents a WSGI application loads, the
// absolute paste configuration first and the given ConfigMap keys behind it.
func novaConfigFilesEnvVar(dataKeys ...string) corev1.EnvVar {
	return corev1.EnvVar{
		Name:  novaConfigFilesEnvVarName,
		Value: strings.Join(append([]string{apiPasteConfigPath}, dataKeys...), ";"),
	}
}

// metadataSharedSecretEnvVar sources the metadata shared secret from the Secret
// the CR references.
func metadataSharedSecretEnvVar(nova *novav1alpha1.Nova) corev1.EnvVar {
	return corev1.EnvVar{
		Name: metadataSharedSecretEnvVarName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: nova.Spec.Metadata.SharedSecretRef.Name,
				},
				Key: effectiveSharedSecretKey(nova),
			},
		},
	}
}

// novaDBTLSVolumeAndMount builds the Volume + VolumeMount pair projecting one
// block's client TLS material (ca.crt from caBundleSecretRef; tls.crt + tls.key
// from clientCertSecretRef) at that block's mount path, so the
// ssl_ca/ssl_cert/ssl_key DSN paths derived from that mount point stay a single
// source of truth. Callers must only invoke it while the block's tls is enabled.
// DefaultMode 0o400 lets the openstack UID read the material while group and
// world have no access.
func novaDBTLSVolumeAndMount(nova *novav1alpha1.Nova, block dbBlock) (corev1.Volume, corev1.VolumeMount) {
	name, mountPath := cellDBTLSVolumeName, cellDBTLSMountPath
	if block == blockAPI {
		name, mountPath = apiDBTLSVolumeName, apiDBTLSMountPath
	}
	tlsSpec := blockDatabase(nova, block).TLS

	volume := corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: ptr.To(int32(0o400)),
				Sources: []corev1.VolumeProjection{
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: tlsSpec.CABundleSecretRef.Name,
							},
							Items: []corev1.KeyToPath{
								{Key: database.TLSCAFileName, Path: database.TLSCAFileName},
							},
						},
					},
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: tlsSpec.ClientCertSecretRef.Name,
							},
							Items: []corev1.KeyToPath{
								{Key: database.TLSCertFileName, Path: database.TLSCertFileName},
								{Key: database.TLSKeyFileName, Path: database.TLSKeyFileName},
							},
						},
					},
				},
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      name,
		MountPath: mountPath,
		ReadOnly:  true,
	}
	return volume, mount
}

// novaDBTLSVolumesAndMounts projects the client TLS material of every block that
// enables it, nova_api first. A process reads both schemas, and each DSN names
// ssl_ca/ssl_cert/ssl_key paths under its own mount point, so a missing half
// leaves every connection to that schema unopenable.
func novaDBTLSVolumesAndMounts(nova *novav1alpha1.Nova) ([]corev1.Volume, []corev1.VolumeMount) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	for _, block := range []dbBlock{blockAPI, blockCell} {
		if !blockDatabase(nova, block).TLS.IsEnabled() {
			continue
		}
		volume, mount := novaDBTLSVolumeAndMount(nova, block)
		volumes = append(volumes, volume)
		mounts = append(mounts, mount)
	}
	return volumes, mounts
}

// novaJobPod resolves the pod settings of every Nova Job and CronJob:
// spec.jobs, with spec.api.deployment as the fallback.
func novaJobPod(nova *novav1alpha1.Nova) job.PodSettings {
	return job.ResolvePodSettings(nova.Spec.Jobs, &nova.Spec.API.Deployment)
}

// novaJobSetParams derives the shared migration-Job inputs from the Nova CR: the
// config mount, the two db-tls keypairs, the nova-manage environment, the Job
// pod settings, and the db-sync script. The steady-state sync flow (database.ReconcileSyncJobs) and
// the upgrade-phase builders (upgradeFlowParams.BuildPhaseJob) both consume it,
// so a seeded Job carries the same pod spec as the desired one.
//
// The whole ConfigMap is mounted at novaConfigDir rather than a per-key subset:
// the Jobs need no file selection, and the role overlays they see beside
// nova.conf set worker counts and listen addresses nova-manage does not read.
func novaJobSetParams(nova *novav1alpha1.Nova, configMapName string) database.JobSetParams {
	extraVolumes, extraMounts := novaDBTLSVolumesAndMounts(nova)
	return database.JobSetParams{
		InstanceName:      nova.Name,
		Namespace:         nova.Namespace,
		Image:             nova.Spec.Image.Reference(),
		ConfigMapName:     configMapName,
		ConfigMountPath:   novaConfigDir,
		Env:               novaWorkloadEnv(nova, roleManage),
		ExtraVolumes:      extraVolumes,
		ExtraVolumeMounts: extraMounts,
		Pod:               novaJobPod(nova),
		SyncCommand:       []string{"/bin/sh", "-eu", "-c", dbSyncScript(nova)},
		// No schema-check: every nova-manage step the sync script runs is
		// idempotent, so a second read-only Job would assert nothing the sync
		// itself has not already established. Release upgrades instead run the
		// shared expand-migrate-contract phase machine (see upgradeFlowParams).
		SchemaCheckCommand: nil,
	}
}

// dbSyncScript is the steady-state migration: it migrates the nova_api schema,
// maps cell0, creates the single real cell unless it is already mapped,
// migrates the cell schema, and reports the mapped cells into the termination
// log.
//
// The cellMappedAwk guard is what makes the Job idempotent. A second
// create_cell with the same templates does not fail and does not update the
// existing row: nova compares the stored, expanded URLs against the templates,
// never finds them equal, and maps a second cell, and from then on every
// instance boots into whichever of the two the scheduler was handed. Each
// listing is captured before it is read, so a list_cells that fails stops the
// script instead of looking like an empty table: in the guard that would create
// the duplicate cell, and in the report it would complete the Job with no cells.
//
// The order matters as much as the steps. cell0 is mapped before the cell
// schema is migrated because nova-manage reads the cell map to find the schema,
// and the real cell is created before "db sync" so the migration runs against a
// cell the API can already address.
func dbSyncScript(nova *novav1alpha1.Nova) string {
	return dbSyncScriptHead(nova) + terminationLogPath
}

// dbSyncScriptHead is dbSyncScript without its destination, split so the script
// test can run these exact bytes against a path it is allowed to write. Nothing
// but the tests uses it on its own.
//
// Both schema names are interpolated into single-quoted shell words. Neither
// can carry a quote: the shared provisioning flow enforces the
// ^[A-Za-z0-9_]{1,64}$ pattern on the primary schema and on cell0's derived
// name before anything is applied.
func dbSyncScriptHead(nova *novav1alpha1.Nova) string {
	manage := "nova-manage --config-dir " + novaConfigDir + " "
	return manage + "api_db sync && " +
		manage + "cell_v2 map_cell0 --database_connection '" + cellDatabaseTemplate(cell0Schema(nova)) + "' && " +
		"cells=\"$(" + manage + "cell_v2 list_cells)\" && " +
		"(printf '%s\\n' \"$cells\" | awk -F'|' -v name='" + computeCellName + "' -v schema='/" +
		nova.Spec.Database.Database + "' '" + cellMappedAwk + "' || " +
		manage + "cell_v2 create_cell --name " + computeCellName +
		" --transport-url '" + cellTransportURLTemplate + "'" +
		" --database_connection '" + cellDatabaseTemplate(nova.Spec.Database.Database) + "') && " +
		manage + "db sync && " +
		"cells=\"$(" + manage + "cell_v2 list_cells)\" && " +
		"printf '%s\\n' \"$cells\" | awk -F'|' '" + cellsReportAwk + "' > "
}

// upgradeFlowParams assembles the shared expand-migrate-contract upgrade-flow
// inputs for this Nova CR. The service-specific parts — the owner CR, the
// "DatabaseReady" condition vocabulary, spec.openStackRelease as the requested
// release, the per-phase Job builder, and the terminal callback — are bound
// here; the phase choreography itself lives in internal/common/database. The
// three status pointers (UpgradePhase, InstalledRelease, TargetRelease) are
// mutated in place by the flow and persisted by the caller after it returns.
func (r *NovaReconciler) upgradeFlowParams(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, configMapName string,
) database.UpgradeFlowParams {
	return database.UpgradeFlowParams{
		Client:           children,
		Scheme:           r.Scheme,
		Recorder:         r.Recorder,
		Owner:            nova,
		Conditions:       &nova.Status.Conditions,
		Generation:       nova.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueUpgradeWait,
		Phase:            &nova.Status.UpgradePhase,
		InstalledRelease: &nova.Status.InstalledRelease,
		TargetRelease:    &nova.Status.TargetRelease,
		SpecRelease:      nova.Spec.OpenStackRelease,
		// Each phase Job runs spec.image: the operator's contract is that the image
		// tag is bumped together with spec.openStackRelease (checkImageReleaseMismatch
		// enforces it), so the new release's migration tree owns the schema deltas.
		//
		// Nova splits the work differently from the phase names: the expand phase
		// runs the readiness check and both schemas' migrations, because
		// "nova-manage db sync" is additive and the old release keeps running
		// against the widened schema. The migrate phase is a read that proves the
		// new code can address the migrated nova_api schema, and the contract phase
		// runs the online data migrations the rolling update before it made safe.
		BuildPhaseJob: func(phase commonv1.UpgradePhase) *batchv1.Job {
			params := novaJobSetParams(nova, configMapName)
			image := nova.Spec.Image.Reference()
			switch phase {
			case commonv1.UpgradePhaseExpanding:
				return database.BuildJob(params, image, upgradeExpandJobSuffix,
					[]string{"/bin/sh", "-eu", "-c", upgradeExpandScript}, upgradePhaseJobBackoffLimit)
			case commonv1.UpgradePhaseMigrating:
				return database.BuildJob(params, image, upgradeMigrateJobSuffix,
					upgradeMigrateCommand, upgradePhaseJobBackoffLimit)
			case commonv1.UpgradePhaseContracting:
				return database.BuildJob(params, image, upgradeContractJobSuffix,
					[]string{"/bin/sh", "-eu", "-c", upgradeContractScript}, upgradePhaseJobBackoffLimit)
			case commonv1.UpgradePhaseRollingUpdate:
				// RollingUpdate builds no Job; the Deployment rollout drives it.
				return nil
			default:
				// The empty steady-state phase never reaches BuildPhaseJob.
				return nil
			}
		},
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordUpgradePhaseTerminal(ctx, children, nova, jobSuffix, observed)
		},
	}
}

// recordUpgradePhaseTerminal emits the terminal-state metric of a finished
// upgrade-phase Job and, for the expand phase, reports what nova-status upgrade
// check found. The metric is the same one the steady-state db-sync emits; the
// check report is the expand phase's own, because the check runs there, ahead of
// the migrations it clears the way for, and its exit code is a severity the Job
// deliberately swallows.
func (r *NovaReconciler) recordUpgradePhaseTerminal(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, jobSuffix string, observed *batchv1.Job,
) {
	r.recordDBJobTerminalState(ctx, nova, jobSuffix, observed)
	if jobSuffix == upgradeExpandJobSuffix {
		r.reportUpgradeCheck(ctx, children, nova, observed)
	}
}

// reportUpgradeCheck turns the expand Job's termination log into an event on the
// Nova: Normal UpgradeCheckCompleted for a clean run, Warning
// UpgradeCheckWarnings carrying the message otherwise. Exit 1 are warnings a
// deployment keeps (the checks report on integrations it may not run), and the
// script itself stops the phase on anything above. A pod the operator cannot
// read the message off leaves a Normal event saying so rather than a silent gap.
//
// It is best-effort throughout: the message is diagnostic, so a List failure is
// logged and reported as unavailable rather than failing the reconcile that is
// otherwise done. The shared per-Job-UID dedupe keeps it to one event per Job.
func (r *NovaReconciler) reportUpgradeCheck(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, observed *batchv1.Job,
) {
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, nova,
		upgradeCheckJobSuffix, observed, "UpgradeCheckEventEmissionDeferred",
		func(string, time.Duration) {
			message := r.jobPodTerminationMessage(ctx, children, nova, observed.Name)
			switch {
			case message == "":
				r.Recorder.Event(nova, corev1.EventTypeNormal, "UpgradeCheckCompleted",
					"nova-status upgrade check exit code unavailable")
			case strings.HasSuffix(message, "exit 0"):
				r.Recorder.Event(nova, corev1.EventTypeNormal, "UpgradeCheckCompleted", message)
			default:
				r.Recorder.Event(nova, corev1.EventTypeWarning, "UpgradeCheckWarnings", message)
			}
		})
}

// reportCells publishes the cells the db-sync Job mapped. The Job's last stage
// writes one "<name>=<uuid>" line per cell into its termination log, which is
// the only place the UUIDs are readable from: nova generates them at map time,
// and a per-cell "nova-manage cell_v2" command addresses a cell by UUID rather
// than by name.
//
// A report the operator cannot read leaves status.cells as it is and emits a
// Normal CellsReportUnavailable event: the cells are mapped either way, and
// dropping a previously published UUID would be worse than reporting a stale
// one. The shared per-Job-UID dedupe keeps it to one write per Job.
func (r *NovaReconciler) reportCells(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, observed *batchv1.Job,
) {
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, nova,
		cellsReportJobSuffix, observed, "CellsReportEmissionDeferred",
		func(string, time.Duration) {
			cells := parseCellsReport(r.jobPodTerminationMessage(ctx, children, nova, observed.Name))
			if len(cells) == 0 {
				r.Recorder.Event(nova, corev1.EventTypeNormal, "CellsReportUnavailable",
					"the db-sync Job reported no cell map; status.cells is left unchanged")
				return
			}
			nova.Status.Cells = cells
		})
}

// parseCellsReport turns the db-sync Job's termination log into the status
// entries. Every line the awk stage wrote is a "<name>=<uuid>" pair, in the
// order list_cells printed them, which is cell0 first. Anything else on the
// stream is nova-manage output that reached the same file and is dropped rather
// than published as a cell.
func parseCellsReport(message string) []novav1alpha1.NovaCellStatus {
	var cells []novav1alpha1.NovaCellStatus
	for _, line := range strings.Split(message, "\n") {
		match := cellsReportPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		cells = append(cells, novav1alpha1.NovaCellStatus{Name: match[1], UUID: match[2]})
	}
	return cells
}

// jobPodTerminationMessage reads the termination message of a Job's pod: the
// pod that succeeded when there is one, otherwise the newest the Job labels, its
// first container, the current termination state or the previous one when the
// container has already been restarted. It returns "" when there is no pod, no
// message, or the read failed — the cases the callers report as unavailable. A
// read error is logged rather than returned: neither the upgrade nor the
// migration depends on the message.
//
// The pick matters because a migration Job runs its retries as new pods: a
// db-sync that failed once leaves the failed attempt beside the one that
// completed, and the failed one wrote no report. The list comes back in no
// fixed order, and the per-Job-UID dedupe makes the first read the only one.
//
// The pods are read through the uncached API reader of the cluster that holds
// them. The operator reads one pod's message once per Job and never watches
// pods: a read through the cached client would start an informer holding every
// pod of the cluster for the life of the process, and one the operator's RBAC
// denies the watch verb.
func (r *NovaReconciler) jobPodTerminationMessage(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, jobName string,
) string {
	reader, err := commonmulticluster.ResolveChildrenAPIReader(ctx, r.Resolver, r.apiReader, nova.Spec.TargetClusterRef)
	if err != nil {
		log.FromContext(ctx).Info("resolving the reader for the pods of a migration Job failed; "+
			"its termination message is reported as unavailable",
			"job", jobName, "err", err.Error())
		return ""
	}
	if reader == nil {
		// No manager behind this reconciler (unit tests).
		reader = children
	}

	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(nova.Namespace),
		client.MatchingLabels{"batch.kubernetes.io/job-name": jobName}); err != nil {
		log.FromContext(ctx).Info("listing the pods of a migration Job failed; "+
			"its termination message is reported as unavailable",
			"job", jobName, "err", err.Error())
		return ""
	}
	if len(pods.Items) == 0 {
		return ""
	}
	pod := &pods.Items[0]
	for i := range pods.Items {
		candidate := &pods.Items[i]
		if candidate.Status.Phase == corev1.PodSucceeded {
			pod = candidate
			break
		}
		if candidate.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = candidate
		}
	}
	statuses := pod.Status.ContainerStatuses
	if len(statuses) == 0 {
		return ""
	}
	if terminated := statuses[0].State.Terminated; terminated != nil && terminated.Message != "" {
		return terminated.Message
	}
	if terminated := statuses[0].LastTerminationState.Terminated; terminated != nil {
		return terminated.Message
	}
	return ""
}
