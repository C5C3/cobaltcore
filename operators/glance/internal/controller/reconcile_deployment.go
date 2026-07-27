// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/deployment"
	"github.com/c5c3/forge/internal/common/keystoneauth"
	"github.com/c5c3/forge/internal/common/naming"
	"github.com/c5c3/forge/internal/common/release"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// glanceBackendsConfigDir is the in-pod directory the rendered backends Secret
// is mounted at. It is a second oslo.config --config-dir so the [<backend>]
// store sections load alongside glance-api.conf without colliding with the
// immutable config ConfigMap: the two artefacts have independent content hashes
// and must mount separately. The launch command references it as the second
// --config-dir in both launch modes.
const glanceBackendsConfigDir = "/etc/glance/backends.conf.d/"

// glanceAppName is the app.kubernetes.io/name label value applied to every
// Glance-owned sub-resource. It matches the literal the validating webhook uses
// for its TopologySpreadConstraints selector check, so the two never drift.
const glanceAppName = "glance"

// glanceAPIPort is the container/Service port the Glance API listens on. It is
// the single source of truth for the container port, the Service port, the
// HTTPRoute backend port, the readiness/liveness probes, the NetworkPolicy
// ingress port, and the status/health-check URLs.
const glanceAPIPort int32 = 9292

// Pod-template annotation keys stamped with content digests so an env-var-
// consumed credential change rolls the Deployment (the value is not
// volume-mounted, so it only takes effect on a Pod restart). Both follow the
// glance.c5c3.io/<x>-hash annotation-key style of the sibling operators.
const (
	// dbConnectionHashAnnotation carries the SHA-256 of the assembled DSN so a
	// rotated (e.g. Dynamic engine-issued) database credential rolls the pods —
	// the DSN is consumed via OS_DATABASE__CONNECTION, not a mounted volume.
	//
	//nolint:gosec // G101 false positive: annotation key name, not credential material.
	dbConnectionHashAnnotation = "glance.c5c3.io/db-connection-hash"
	// authTokenHashAnnotation carries the SHA-256 of the service-user password so
	// a rotation at the OpenBao source rolls the pods — the password is consumed
	// via OS_KEYSTONE_AUTHTOKEN__PASSWORD, not a mounted volume.
	//
	//nolint:gosec // G101 false positive: annotation key name, not credential material.
	authTokenHashAnnotation = "glance.c5c3.io/authtoken-hash"
)

// Pod volume names. The config and backends volumes carry a naming contract
// shared with the GlanceBackend controller (which reads the backends volume off
// the live Deployment) and reconcileConfig (which recovers the last-good
// artefact names off the live Deployment's config/backends volumes).
const (
	// stagingVolumeName backs the reserved import-staging store emptyDir.
	stagingVolumeName = "staging"
	// tasksVolumeName backs the reserved async-task-work store emptyDir.
	tasksVolumeName = "tasks-work"
	// dbTLSVolumeName backs the projected db-tls client keypair volume.
	dbTLSVolumeName = "db-tls"
	// imageCacheVolumeName backs the per-replica image-cache emptyDir, mounted
	// into the API container and the cache-maintenance sidecar alike.
	imageCacheVolumeName = "image-cache"
)

// uwsgiLogFormat is the uWSGI --log-format literal shared with the sibling
// operators so every request line reaches stderr in the same shape.
const uwsgiLogFormat = "%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto) %(status))"

// glanceUWSGIPyargv is the argument vector uWSGI forwards to the WSGI entry
// script via --pyargv: both oslo.config --config-dir entries (the immutable
// config ConfigMap and the backends Secret), so the WSGI app loads the same
// config the eventlet launch command loads. Glance's stock module path
// (glance.wsgi.api:application) ignores sys.argv — wsgi_app.init_app() parses
// with CONF([], ...) and reads only $OS_GLANCE_CONFIG_DIR/glance-api.conf —
// which is why the launch command loads glanceWSGIScriptPath instead: that
// image-shipped script consumes these flags and redirects glance's config
// discovery to them.
var glanceUWSGIPyargv = "--config-dir " + glanceConfigDir + " --config-dir " + glanceBackendsConfigDir

// glanceWSGIScriptPath is the uWSGI entry script shipped by images/glance
// (COPY glance-wsgi-api). It parses the --pyargv --config-dir flags that
// glance's own WSGI module ignores; the two paths must stay in lockstep with
// the image.
const glanceWSGIScriptPath = "/var/lib/openstack/bin/glance-wsgi-api"

// Condition reason constants for DeploymentReady.
const (
	conditionReasonDeploymentReady           = "DeploymentReady"
	conditionReasonWaitingForDeployment      = "WaitingForDeployment"
	conditionReasonDeploymentWaitingBackends = "WaitingForBackends"
)

// commonLabels returns the standard Kubernetes labels applied to all resources
// owned by this Glance instance, delegating to the shared naming package.
func commonLabels(glance *glancev1alpha1.Glance) map[string]string {
	return naming.CommonLabels(glanceAppName, glance.Name)
}

// selectorLabels returns the minimal label set used as the Deployment pod
// selector. It is a subset of commonLabels and must remain stable for the
// lifetime of a Deployment (selectors are immutable after creation).
func selectorLabels(glance *glancev1alpha1.Glance) map[string]string {
	return naming.SelectorLabels(glanceAppName, glance.Name)
}

// reconcileDeployment ensures the Glance API Deployment, Service, and PDB exist
// with the correct spec. It sets the DeploymentReady condition and stamps the
// status endpoint when the Deployment becomes available.
//
// art carries the config ConfigMap and backends Secret names. An invalid
// backends projection surfaces here as an empty art.configMapName only on first
// install (no live Deployment to recover last-good names from): the config step
// returns the live Deployment's mounted names whenever one exists, so a
// non-empty art re-applies the last-good config (spec knobs still converge)
// while an empty art means there is nothing to run yet — DeploymentReady=False /
// WaitingForBackends, no creation, no error.
//
// dsnDigest and authtokenDigest are stamped into pod-template annotations so a
// rotated database credential or service-user password rolls the pods; both are
// env-var-consumed, so they only take effect on a Pod restart. Each annotation
// is omitted when its digest is empty (the requeue/error paths upstream).
func (r *GlanceReconciler) reconcileDeployment(ctx context.Context, glance *glancev1alpha1.Glance, art configArtifacts, dsnDigest, authtokenDigest string) (ctrl.Result, error) {
	// Invalid projection on first install: no rendered config exists and no live
	// Deployment mounts a last-good one. Wait for a ready default backend rather
	// than creating a Deployment that would crash-loop on an empty config.
	if art.configMapName == "" {
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: glance.Generation,
			Reason:             conditionReasonDeploymentWaitingBackends,
			Message:            "Waiting for a ready default backend before creating the Glance API Deployment",
		})
		return ctrl.Result{}, nil
	}

	deploy := buildGlanceDeployment(glance, art, dsnDigest, authtokenDigest)
	ready, err := deployment.EnsureDeployment(ctx, r.Client, r.Scheme, glance, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Deployment: %w", err)
	}

	svc := buildGlanceService(glance)
	if err := deployment.EnsureService(ctx, r.Client, r.Scheme, glance, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Service: %w", err)
	}

	pdb := buildPodDisruptionBudget(glance)
	if err := deployment.EnsurePDB(ctx, r.Client, r.Scheme, glance, pdb); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring PodDisruptionBudget: %w", err)
	}

	if !ready {
		log.FromContext(ctx).Info("Glance API deployment not ready, requeuing")
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: glance.Generation,
			Reason:             conditionReasonWaitingForDeployment,
			Message:            "Glance API deployment is not yet available",
		})
		return ctrl.Result{RequeueAfter: RequeueDeploymentPolling}, nil
	}

	// Hold the RollingUpdate → Contracting flip until the Deployment has FULLY
	// converged onto the target-release image. The `ready` signal above comes from
	// deployment.IsDeploymentReady, which is surge-tolerant: under the default
	// MaxSurge=1/MaxUnavailable=0 strategy a new pod is added before an old one is
	// removed, so it turns true as soon as the first new-image pod is Ready while
	// old-image pods still serve. The contract phase then drops the now-unused
	// columns those old pods still read — so gate the flip on every replica being
	// updated, ready, and counted (glanceDeploymentRolledOut), and requeue to wait
	// otherwise. Only the RollingUpdate upgrade phase is stricter here; steady-state
	// rollouts keep the surge-tolerant readiness above.
	if glance.Status.UpgradePhase == commonv1.UpgradePhaseRollingUpdate && !glanceDeploymentRolledOut(deploy) {
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: glance.Generation,
			Reason:             conditionReasonWaitingForDeployment,
			Message:            "Waiting for the upgraded image to finish rolling out before contracting the database schema",
		})
		return ctrl.Result{RequeueAfter: RequeueDeploymentPolling}, nil
	}

	// Transition from RollingUpdate to Contracting when the Deployment is ready.
	// The shared flow advances the phase, emits the DeploymentRolloutComplete
	// event, and logs; glance stamps its own DeploymentReady condition and
	// requeues so ReconcileUpgrade runs the contract phase on the next pass. The
	// endpoint is deliberately NOT stamped on this flip pass (keystone parity).
	if database.CompleteRollingUpdate(ctx, r.upgradeFlowParams(ctx, glance, art.configMapName)) {
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: glance.Generation,
			Reason:             conditionReasonDeploymentReady,
			Message:            "Glance API deployment is available",
		})
		return ctrl.Result{Requeue: true}, nil
	}

	// Status.Endpoint derivation is delegated to glanceStatusEndpoint so the
	// gateway-aware public URL is used when spec.gateway is set, and the
	// cluster-local URL otherwise.
	glance.Status.Endpoint = glanceStatusEndpoint(glance)
	conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
		Type:               "DeploymentReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: glance.Generation,
		Reason:             conditionReasonDeploymentReady,
		Message:            "Glance API deployment is available",
	})
	return ctrl.Result{}, nil
}

// glanceDeploymentRolledOut reports whether the Glance API Deployment has fully
// converged onto its current pod template: the deployment controller has observed
// the latest generation and every replica is updated, ready, and counted, with no
// surge or old-template pod still present. It is stricter than the surge-tolerant
// readiness deployment.EnsureDeployment reports (which turns true as soon as the
// first new-image pod is Ready while old-image pods still serve), so the shared
// upgrade flow's RollingUpdate → Contracting flip waits for the old image to drain
// before the schema is contracted.
func glanceDeploymentRolledOut(deploy *appsv1.Deployment) bool {
	if deploy.Status.ObservedGeneration < deploy.Generation {
		return false
	}
	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}
	return deploy.Status.UpdatedReplicas == desired &&
		deploy.Status.ReadyReplicas == desired &&
		deploy.Status.Replicas == desired
}

// effectiveStagingSizeLimit returns the size limit stamped on both scratch
// emptyDirs, resolving the operator default whenever spec.staging leaves it
// unset — a nil block and an empty one behave identically. It returns nil for
// spec.staging.unbounded, which is what a Deployment carrying no
// emptyDir.sizeLimit at all needs: the deliberate opt-out back to the
// pre-bound behaviour, distinct from "unset" precisely so the default keeps
// applying to everyone who did not ask. It is pure and total: no input can
// fail, so there is no error path.
//
// Resolving here rather than in the defaulting webhook keeps an unset field
// tracking the operator default across upgrades instead of freezing today's
// value into the stored CR — the same contract effectiveDBPurge and
// effectiveImportFiltering follow.
func effectiveStagingSizeLimit(glance *glancev1alpha1.Glance) *resource.Quantity {
	if glance.Spec.Staging == nil {
		return ptr.To(glancev1alpha1.DefaultStagingSizeLimit())
	}
	if glance.Spec.Staging.Unbounded {
		return nil
	}
	if glance.Spec.Staging.SizeLimit == nil {
		return ptr.To(glancev1alpha1.DefaultStagingSizeLimit())
	}
	return ptr.To(glance.Spec.Staging.SizeLimit.DeepCopy())
}

// buildGlanceDeployment constructs the desired Glance API Deployment. The
// rendered config ConfigMap mounts at glanceConfigDir and the backends Secret at
// glanceBackendsConfigDir (both oslo.config --config-dir roots). Reserved
// staging/tasks stores get emptyDir volumes because import staging and async
// task work always land on local disk regardless of the image store — both
// bounded by effectiveStagingSizeLimit, the best-effort eviction threshold
// described on StagingSpec — and the db-tls client keypair is projected only
// when database TLS is enabled. The database URL and the service-user password
// are injected via env vars so no credential material enters the ConfigMap.
//
// spec.imageCache adds two more pieces, and only then: an emptyDir bounded by
// its resolved sizeLimit mounted read-write into the API container at
// glanceImageCachePath, and the cacheMaintenanceContainer sidecar that prunes
// and cleans it. With the cache off the pod template is byte-identical to the
// one built before the cache existed.
func buildGlanceDeployment(glance *glancev1alpha1.Glance, art configArtifacts, dsnDigest, authtokenDigest string) *appsv1.Deployment {
	selector := selectorLabels(glance)
	labels := commonLabels(glance)

	// Both scratch emptyDirs carry the same resolved bound, each with its own
	// Quantity so the two volumes never share a pointer. Both stay nil when the
	// bound is opted out of, leaving the emptyDirs unbounded.
	stagingLimit := effectiveStagingSizeLimit(glance)
	var tasksLimit *resource.Quantity
	if stagingLimit != nil {
		tasksLimit = ptr.To(stagingLimit.DeepCopy())
	}

	deploy := deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      glance.Namespace,
		Name:           subResourceName(glance),
		Labels:         labels,
		SelectorLabels: selector,
		PodAnnotations: glancePodAnnotations(dsnDigest, authtokenDigest),
		Deployment:     &glance.Spec.Deployment,
		Autoscaling:    glance.Spec.Autoscaling,
		Container: deployment.ContainerParams{
			Name:    "glance-api",
			Image:   glance.Spec.Image.Reference(),
			Command: glanceLaunchCommand(glance),
			Env: []corev1.EnvVar{
				database.ConnectionEnvVar(glance.Name),
				keystoneauth.PasswordEnvVar(glance.Spec.ServiceUser.SecretRef.Name, effectiveServiceUserKey(glance)),
			},
			Ports: []corev1.ContainerPort{{
				Name:          "glance-api",
				ContainerPort: glanceAPIPort,
			}},
			// Readiness AND liveness hit the same /healthcheck endpoint (served by
			// the oslo healthcheck middleware without touching the database) on the
			// API port, identical in both launch modes. Glance has no startup probe:
			// the readiness probe's own delay covers the WSGI app coming up.
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        glanceHealthcheckProbeHandler(),
				InitialDelaySeconds: 15,
				PeriodSeconds:       20,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        glanceHealthcheckProbeHandler(),
				InitialDelaySeconds: 10,
				PeriodSeconds:       15,
				TimeoutSeconds:      10,
				FailureThreshold:    3,
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      configVolumeName,
					MountPath: glanceConfigDir,
					ReadOnly:  true,
				},
				{
					Name:      backendsVolumeName,
					MountPath: glanceBackendsConfigDir,
					ReadOnly:  true,
				},
				{
					Name:      stagingVolumeName,
					MountPath: glanceStagingStorePath,
				},
				{
					Name:      tasksVolumeName,
					MountPath: glanceTasksStorePath,
				},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: configVolumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: art.configMapName,
						},
					},
				},
			},
			{
				Name: backendsVolumeName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: art.backendsSecretName,
					},
				},
			},
			{
				Name: stagingVolumeName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: stagingLimit},
				},
			},
			{
				Name: tasksVolumeName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: tasksLimit},
				},
			},
		},
	})

	// Project the db-tls client keypair into the API pod when database TLS is
	// enabled; the gate is centralised in glanceDBTLSEnabled so the deployment
	// and the db-sync Job builder decide identically.
	if glanceDBTLSEnabled(glance) {
		tlsVol, tlsMount := glanceDBTLSVolumeAndMount(glance)
		deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, tlsVol)
		deploy.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			deploy.Spec.Template.Spec.Containers[0].VolumeMounts, tlsMount,
		)
	}

	// Mount the per-replica image cache and run its maintenance loop when
	// spec.imageCache opts in. The emptyDir gets its own Quantity, never the CR's
	// pointer — the same aliasing rule the scratch volumes above follow.
	if imageCache := effectiveImageCache(glance.Spec.ImageCache); imageCache != nil {
		deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: imageCacheVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(imageCache.SizeLimit.DeepCopy())},
			},
		})
		deploy.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			deploy.Spec.Template.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: imageCacheVolumeName, MountPath: glanceImageCachePath},
		)
		deploy.Spec.Template.Spec.Containers = append(
			deploy.Spec.Template.Spec.Containers, cacheMaintenanceContainer(glance, imageCache),
		)
	}
	return deploy
}

const (
	// cacheMaintenancePollInterval is how often the maintenance sidecar re-reads
	// the size of the cache directory. It is NOT the prune cadence — see
	// cacheMaintenanceContainer — but it is the upper bound on how long the cache
	// may keep growing after it has crossed image_cache_max_size. The admission
	// floor on spec.imageCache.maintenanceInterval is a minute, so at least one
	// poll always fits inside an interval.
	cacheMaintenancePollInterval = 30 * time.Second
)

// cacheMaintenanceScript is the maintenance sidecar's loop, formatted with the
// resolved maintenance interval, the poll interval, the high-water mark in KiB
// (the unit `du -sk` reports), the cache directory and the config directory, in
// that order. See cacheMaintenanceContainer for what each of those buys.
const cacheMaintenanceScript = `interval=%d
poll=%d
high_water_kib=%d
dir=%s
conf=%s
elapsed=0
failures=0
while true; do
  sleep "$poll"
  elapsed=$((elapsed + poll))
  used_kib=$(du -sk --exclude=incomplete --exclude=invalid --exclude=queue "$dir" | cut -f1)
  if [ -z "$used_kib" ]; then
    echo "glance-cache-maintenance unmeasured: could not measure $dir; running a pass anyway" >&2
    used_kib=$high_water_kib
  fi
  if [ "$elapsed" -lt "$interval" ] && [ "$used_kib" -lt "$high_water_kib" ]; then
    continue
  fi
  elapsed=0
  rc=0
  glance-cache-pruner --config-dir "$conf" || rc=1
  glance-cache-cleaner --config-dir "$conf" || rc=1
  if [ "$rc" -eq 0 ]; then
    if [ "$failures" -ne 0 ]; then
      echo "glance-cache-maintenance recovered: after $failures consecutive failures" >&2
    fi
    failures=0
    continue
  fi
  failures=$((failures + 1))
  echo "glance-cache-maintenance failed: $failures consecutive, cache at $used_kib KiB of $high_water_kib" >&2
done
`

// cacheMaintenanceContainer builds the sidecar that keeps the image cache
// bounded: glance-cache-pruner evicts entries back down to image_cache_max_size,
// glance-cache-cleaner removes the stalled entries an interrupted download leaves
// behind. It is a sidecar rather than the CronJob shape reconcile_dbpurge.go
// uses because the cache is a pod-local emptyDir: only a container in the same
// pod can reach the files the pruner has to delete.
//
// The loop is driven by cache size first and by the clock second. glance applies
// no write barrier of its own — image_cache_max_size is read by the pruner
// alone, never by the API's cache filter — so a purely time-driven loop would
// let an unbounded number of concurrent downloads pile into the emptyDir between
// two passes, cross the bound the kubelet DOES enforce, and get the pod evicted
// mid-request. The loop therefore re-reads the directory every
// cacheMaintenancePollInterval and prunes as soon as it sits above
// image_cache_max_size, whatever maintenanceInterval says; that interval stays
// the cadence for the passes that run while the cache is under the mark, which
// is what bounds how long a stalled entry survives.
//
// That poll measures what the pruner can actually reclaim, which is narrower
// than the directory: glance's own accounting — the number the pruner compares
// against image_cache_max_size — covers the cache entries alone, while the
// incomplete/, invalid/ and queue/ subdirectories hold entries only
// glance-cache-cleaner may touch, and only once image_cache_stall_time (a day,
// and deliberately not rendered — see reconcile_config.go) has passed. Counting
// them would leave the size trigger latched on for that whole day over bytes no
// prune could release, so they are excluded. A measurement that yields nothing
// at all is the failure not to paper over in the other direction: reading an
// unreadable directory as an empty cache would silently drop the loop back to
// the purely time-driven behaviour this poll exists to replace, so the empty
// value is logged and the pass runs anyway. du's stderr stays visible for the
// same reason. (The pipeline's status is cut's, not du's, so a failing du never
// trips `sh -e` — the empty value is the only thing that catches it.)
//
// No maintenance failure takes the pod down. Capturing `rc` is what buys that:
// under `sh -e` an unguarded non-zero exit would end the loop outright, and this
// container shares the pod with glance-api, so a stopped loop means
// CrashLoopBackOff, a NotReady pod, and an API replica dropped from the Service.
// A pruner that can never run (a corrupted sqlite index, a CLI renamed by an
// image bump) is a property of the image and the shared config, so it fails
// identically on every replica at once, and escalating it to the pod would turn
// a degraded cache into an immediate Glance outage. Absorbing it is not free
// either: nothing else enforces image_cache_max_size, so every replica's cache
// climbs to the emptyDir bound and the kubelet evicts it under ephemeral-storage
// pressure. That eviction is a node-local decision that never passes through the
// eviction API, so the PodDisruptionBudget this reconciler creates does not
// stagger it and replicas under even traffic cross their identical bound at
// roughly the same time. The trade is a delayed, repeating outage against an
// immediate one — not an outage against none.
//
// That leaves the log as the only signal, so it carries a stable token rather
// than prose: `glance-cache-maintenance failed` is what an alert keys on, the
// consecutive count is what separates a transient lost sqlite lock from a pruner
// that can never run, and the matching `recovered` line is what lets that alert
// close again. All three go to stderr, apart from the two CLIs' own output.
// Nothing reaches the control plane — no Event, no condition, no metric: the
// sidecar has no API access, and every in-pod escalation path (exiting, a
// probe, a container restart) drops the API replica from the Service exactly as
// the loop-ending exit would. It is also why the container carries no probes.
//
// It needs no env. Both CLIs load their config through glance's
// parse_cache_args and touch only the cache directory and the sqlite metadata
// file inside it, so they open neither the database nor glance_store — no DSN,
// no service-user password, and no backends Secret mount. They make no network
// calls either, so the NetworkPolicy needs no rule for them.
//
// Its resources are fixed modest constants for a prune loop over a pod-local
// directory, the same shape keystone's federation-proxy sidecar uses; a spec
// knob is deferred until a deployment demonstrates the need. The requests are
// mandatory rather than cosmetic: the HPA's Resource metric aggregates across
// every container in the pod, so one container without a cpu request makes the
// whole metric unavailable (FailedGetResourceMetric) and silently freezes
// autoscaling, and a namespace ResourceQuota on requests.cpu rejects the pod
// outright.
func cacheMaintenanceContainer(glance *glancev1alpha1.Glance, imageCache *glancev1alpha1.ImageCacheSpec) corev1.Container {
	script := fmt.Sprintf(
		cacheMaintenanceScript,
		int64(imageCache.MaintenanceInterval.Duration/time.Second),
		int64(cacheMaintenancePollInterval/time.Second),
		imageCacheMaxSizeBytes(*imageCache.SizeLimit)/1024,
		glanceImageCachePath,
		glanceConfigDir,
	)

	return corev1.Container{
		Name:            "cache-maintenance",
		Image:           glance.Spec.Image.Reference(),
		Command:         []string{"/bin/sh", "-eu", "-c", script},
		SecurityContext: deployment.RestrictedSecurityContext(),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("25m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: configVolumeName, MountPath: glanceConfigDir, ReadOnly: true},
			{Name: imageCacheVolumeName, MountPath: glanceImageCachePath},
		},
	}
}

// glancePodAnnotations assembles the pod-template annotations, stamping each
// content digest only when non-empty so the requeue/error paths (which return an
// empty digest) leave the annotation off and cause no spurious rollout. Returns
// nil when both digests are empty so the pod template carries no annotations.
func glancePodAnnotations(dsnDigest, authtokenDigest string) map[string]string {
	annotations := map[string]string{}
	if dsnDigest != "" {
		annotations[dbConnectionHashAnnotation] = dsnDigest
	}
	if authtokenDigest != "" {
		annotations[authTokenHashAnnotation] = authtokenDigest
	}
	if len(annotations) == 0 {
		return nil
	}
	return annotations
}

// glanceHealthcheckProbeHandler returns the shared readiness/liveness probe
// handler: an HTTP GET of /healthcheck on the API port.
func glanceHealthcheckProbeHandler() corev1.ProbeHandler {
	return corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{
			Path: "/healthcheck",
			Port: intstr.FromInt32(glanceAPIPort),
		},
	}
}

// glanceLaunchCommand returns the container command for the Glance API,
// switching on spec.openStackRelease: the eventlet glance-api server below
// 2026.1 (worker count comes from [DEFAULT] workers in the config, not the CLI),
// uWSGI from 2026.1 onward. Both launch modes load the same two --config-dir
// roots so the rendered config and the projected backends stores apply
// identically.
func glanceLaunchCommand(glance *glancev1alpha1.Glance) []string {
	if glanceUsesUWSGI(glance) {
		return glanceUWSGICommand(glance.Spec.APIServer)
	}
	return []string{"glance-api", "--config-dir", glanceConfigDir, "--config-dir", glanceBackendsConfigDir}
}

// glanceUsesUWSGI reports whether the Glance API launches under uWSGI (release
// 2026.1 or later) rather than the eventlet glance-api server. An unparseable
// release (a CR that bypassed the CRD pattern) falls back to the eventlet launch
// mode, the pre-2026.1 default.
func glanceUsesUWSGI(glance *glancev1alpha1.Glance) bool {
	rel, err := release.ParseRelease(glance.Spec.OpenStackRelease)
	if err != nil {
		return false
	}
	return rel.Year > 2026 || (rel.Year == 2026 && rel.Minor >= 1)
}

// glanceUWSGICommand constructs the uWSGI container command from the given
// apiServer spec, mirroring keystone's uwsgiCommand flag-by-flag. When apiServer
// or apiServer.uwsgi is nil the webhook defaults (processes=2, threads=1,
// httpKeepAlive=true) apply. Fixed flags (--http, --wsgi-file, --master,
// --lazy-apps, --need-app, --pyargv, and the always-on request logging) are
// always included; --http-keepalive[-timeout] and --harakiri are conditional on
// the uWSGI spec exactly as keystone emits them.
func glanceUWSGICommand(apiServer *glancev1alpha1.APIServerSpec) []string {
	processes := glancev1alpha1.DefaultUWSGIProcesses
	threads := glancev1alpha1.DefaultUWSGIThreads
	httpKeepAlive := glancev1alpha1.DefaultUWSGIHTTPKeepAlive

	var uwsgi *glancev1alpha1.UWSGISpec
	if apiServer != nil {
		uwsgi = apiServer.UWSGI
	}
	if uwsgi != nil {
		processes = uwsgi.Processes
		threads = uwsgi.Threads
		// HTTPKeepAlive is a nil-preserving *bool: nil means "unset", which keeps
		// the default (true); an explicit true/false is honored verbatim.
		if uwsgi.HTTPKeepAlive != nil {
			httpKeepAlive = *uwsgi.HTTPKeepAlive
		}
	}

	cmd := []string{
		"uwsgi",
		"--http", fmt.Sprintf(":%d", glanceAPIPort),
		// Glance streams image uploads and downloads with chunked transfer
		// encoding, so uWSGI must accept chunked request bodies and re-chunk
		// responses. These two flags are always on regardless of the tuning knobs.
		"--http-auto-chunked",
		"--http-chunked-input",
	}
	if httpKeepAlive {
		cmd = append(cmd, "--http-keepalive")
		if uwsgi != nil && uwsgi.HTTPKeepAliveTimeout != nil {
			cmd = append(cmd, "--http-keepalive-timeout", strconv.Itoa(int(*uwsgi.HTTPKeepAliveTimeout)))
		}
	}
	// Unconditional: makes uWSGI master logging always-on so request lines reach
	// stderr in every configuration, regardless of keep-alive.
	cmd = append(
		cmd,
		"--log-master",
		"--log-format", uwsgiLogFormat,
	)
	cmd = append(
		cmd,
		"--wsgi-file", glanceWSGIScriptPath,
		"--master",
		"--lazy-apps",
		"--need-app",
		"--processes", strconv.Itoa(int(processes)),
		"--threads", strconv.Itoa(int(threads)),
		"--pyargv", glanceUWSGIPyargv,
	)
	if uwsgi != nil && uwsgi.Harakiri != nil {
		cmd = append(cmd, "--harakiri", strconv.Itoa(int(*uwsgi.Harakiri)))
	}
	return cmd
}

// glanceDBTLSEnabled reports whether the Glance CR requests TLS to the database;
// the helper centralises the nil/disabled gate so the deployment and db-sync Job
// builders decide identically.
func glanceDBTLSEnabled(glance *glancev1alpha1.Glance) bool {
	return glance.Spec.Database.TLS.IsEnabled()
}

// glanceDBTLSVolumeAndMount builds the Volume + VolumeMount pair projecting the
// client TLS material (ca.crt from caBundleSecretRef; tls.crt + tls.key from
// clientCertSecretRef) into the Glance API pod, merged onto dbTLSMountPath via a
// projected volume so the ssl_ca/ssl_cert/ssl_key DSN paths derived from that
// mount point stay a single source of truth. Callers must only invoke it when
// glanceDBTLSEnabled(glance) is true. DefaultMode 0o400 lets the openstack UID
// read the material while group/world have no access. It mirrors keystone's
// dbTLSVolumeAndMount.
func glanceDBTLSVolumeAndMount(glance *glancev1alpha1.Glance) (corev1.Volume, corev1.VolumeMount) {
	tlsSpec := glance.Spec.Database.TLS
	volume := corev1.Volume{
		Name: dbTLSVolumeName,
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
		Name:      dbTLSVolumeName,
		MountPath: dbTLSMountPath,
		ReadOnly:  true,
	}
	return volume, mount
}

// buildPodDisruptionBudget constructs the desired PDB for the Glance API
// deployment, delegating to the shared builder (minAvailable=1 for multi-replica,
// maxUnavailable=1 for single-replica to avoid drain deadlock).
func buildPodDisruptionBudget(glance *glancev1alpha1.Glance) *policyv1.PodDisruptionBudget {
	return deployment.BuildPDB(glance.Namespace, subResourceName(glance), commonLabels(glance), selectorLabels(glance), &glance.Spec.Deployment)
}

// buildGlanceService builds the Glance API Service on the API port.
func buildGlanceService(glance *glancev1alpha1.Glance) *corev1.Service {
	return deployment.BuildService(glance.Namespace, subResourceName(glance), commonLabels(glance), selectorLabels(glance), glanceAPIPort, glanceAPIPort)
}
