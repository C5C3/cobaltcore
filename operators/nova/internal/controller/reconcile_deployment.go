// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// novaAppName is the app.kubernetes.io/name label value applied to every
// Nova-owned sub-resource. It matches the literal the validating webhook uses
// for its TopologySpreadConstraints selector check, so the two never drift.
const novaAppName = "nova"

// The ports of the two HTTP front ends, the upstream defaults every client and
// every catalog entry assumes. The console proxy listens on novaConsolePort,
// declared beside the [vnc] section that publishes it.
const (
	novaAPIPort      int32 = 8774
	novaMetadataPort int32 = 8775
)

// The uWSGI --module values of the two front ends: the import paths of the WSGI
// application objects the nova image ships. They stay in lockstep with
// images/nova, which installs nova with the same module layout.
const (
	novaAPIWSGIModule      = "nova.wsgi.osapi_compute:application"
	novaMetadataWSGIModule = "nova.wsgi.metadata:application"
)

// Pod volume names shared by the five workloads.
const (
	stateVolumeName      = "state"
	tmpVolumeName        = "tmp"
	rabbitmqCAVolumeName = "rabbitmq-ca"
)

// tmpMountPath is the writable scratch directory every process needs beside its
// state directory: the root filesystem is read-only, and python writes
// intermediate files (an unpacked egg cache, oslo's temporary copies) under
// TMPDIR.
const tmpMountPath = "/tmp"

// Pod-template annotation keys stamped with content digests so an
// env-var-consumed credential change rolls the workload: the value is not
// volume-mounted, so it only takes effect on a Pod restart. Nova opens two
// schemas, so the two connection URLs carry one digest each.
const (
	apiDBConnectionHashAnnotation = "nova.c5c3.io/api-db-connection-hash"
	dbConnectionHashAnnotation    = "nova.c5c3.io/db-connection-hash"
	// #nosec G101 -- annotation key naming a digest, not a credential.
	authTokenHashAnnotation = "nova.c5c3.io/authtoken-hash"
	// #nosec G101 -- annotation key naming a digest, not a credential.
	transportURLHashAnnotation = "nova.c5c3.io/transport-url-hash"
	// metadataSecretHashAnnotation carries the digest of the shared secret the
	// metadata API verifies a proxied request's signature with. Only the metadata
	// pods read that secret, so only they carry the stamp.
	// #nosec G101 -- annotation key naming a digest, not a credential.
	metadataSecretHashAnnotation = "nova.c5c3.io/metadata-secret-hash"
	// installedReleaseAnnotation carries the release the schema is installed at.
	// It is stamped on every process but the API: the metadata API, the
	// scheduler, the conductor and the console proxy cache the RPC versions their
	// peers reported at startup, so after the contract phase of an upgrade they
	// have to restart once more to drop that cache. The API does not carry it
	// because it rolled during the RollingUpdate phase and already holds the new
	// minimum.
	installedReleaseAnnotation = "nova.c5c3.io/installed-release"
)

// Condition reason constants for DeploymentReady.
const (
	conditionReasonDeploymentReady      = "DeploymentReady"
	conditionReasonWaitingForDeployment = "WaitingForDeployment"
)

// workloadDigests are the content digests the credential steps return, stamped
// into the pod templates so a rotated database credential, service-user
// password, broker credential or metadata secret rolls the pods. Each is empty
// on the requeue and error paths upstream, where no derived Secret was
// materialised.
type workloadDigests struct {
	apiDSN         string
	cellDSN        string
	authToken      string
	transport      string
	metadataSecret string
}

// commonLabels returns the standard Kubernetes labels applied to all resources
// owned by this Nova instance, delegating to the shared naming package.
func commonLabels(nova *novav1alpha1.Nova) map[string]string {
	return naming.CommonLabels(novaAppName, nova.Name)
}

// componentLabels returns the pod-template labels for a workload of the given
// component. It is commonLabels plus app.kubernetes.io/component, so the result
// stays a superset of that workload's selector labels.
func componentLabels(nova *novav1alpha1.Nova, component string) map[string]string {
	return naming.ComponentLabels(novaAppName, nova.Name, component)
}

// apiSelectorLabels returns the pod selector of the API Deployment, its Service
// and its PodDisruptionBudget: the shared selector labels narrowed by
// app.kubernetes.io/component=api.
//
// One Nova owns five Deployments, so a selector without the component key would
// match all of their pods: the Deployments would adopt each other's, and the
// Service would route API requests to a conductor that serves no HTTP. Every
// selector is narrow from the first pass; no Nova Deployment predating the
// component label exists, so there is nothing to migrate off.
func apiSelectorLabels(nova *novav1alpha1.Nova) map[string]string {
	return naming.APISelectorLabels(novaAppName, nova.Name)
}

// componentSelectorLabels returns the pod selector of the Deployment of one
// non-API component: the shared selector labels narrowed by that component. It
// is the twin of the api-package helper of the same name, which the webhook
// measures a custom topology-spread selector against.
func componentSelectorLabels(nova *novav1alpha1.Nova, component string) map[string]string {
	labels := naming.SelectorLabels(novaAppName, nova.Name)
	labels[naming.LabelKeyComponent] = component
	return labels
}

// internalNovaURL is the cluster-local Service URL of the Nova API: what
// status.endpoint reports without a gateway, and what an in-cluster client
// dials.
func internalNovaURL(nova *novav1alpha1.Nova) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", nova.Name, nova.Namespace, novaAPIPort)
}

// novaStatusEndpoint is the URL clients read off the CR: the gateway hostname
// while the API is exposed through one, and the cluster-local Service address
// otherwise. It renders the same shape as the sibling operators, the public
// scheme with a trailing slash.
func novaStatusEndpoint(nova *novav1alpha1.Nova) string {
	if nova.Spec.Gateway != nil {
		return fmt.Sprintf("https://%s/", nova.Spec.Gateway.Hostname)
	}
	return internalNovaURL(nova)
}

// effectiveInstalledRelease returns the release the pods of this CR run against:
// status.installedRelease once a db-sync has promoted it, and
// spec.openStackRelease while it is still empty. Falling back to the spec keeps
// a fresh install from stamping an empty annotation that would change to the
// real release on the very next pass and roll every pod for nothing.
func effectiveInstalledRelease(nova *novav1alpha1.Nova) string {
	if nova.Status.InstalledRelease != "" {
		return nova.Status.InstalledRelease
	}
	return nova.Spec.OpenStackRelease
}

// reconcileDeployment projects the Nova API Deployment, its Service and its
// PodDisruptionBudget. It sets the DeploymentReady condition and stamps the
// status endpoint once the Deployment is available.
//
// art names the rendered config ConfigMap and its keys; the API projects the
// shared document and the logging file out of it and leaves the four role
// overlays to the processes that own them.
func (r *NovaReconciler) reconcileDeployment(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, art configArtifacts, digests workloadDigests,
) (ctrl.Result, error) {
	deploy := buildAPIDeployment(nova, art, digests)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, nova, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Deployment: %w", err)
	}

	svc := buildAPIService(nova)
	if err := deployment.EnsureService(ctx, children, r.Scheme, nova, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Service: %w", err)
	}

	pdb := buildPodDisruptionBudget(nova)
	if err := deployment.EnsurePDB(ctx, children, r.Scheme, nova, pdb); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring PodDisruptionBudget: %w", err)
	}

	if !ready {
		log.FromContext(ctx).Info("Nova API deployment not ready, requeuing")
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonWaitingForDeployment,
			Message:            "Nova API deployment is not yet available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	// Hold the RollingUpdate to Contracting flip until every role has fully
	// converged onto the target-release image. The `ready` signal above comes
	// from deployment.IsDeploymentReady, which is surge-tolerant: under the
	// default MaxSurge=1/MaxUnavailable=0 strategy a new pod is added before an
	// old one is removed, so it turns true as soon as the first new-image pod is
	// Ready while old-image pods still serve. The contract phase then runs the
	// data migrations those old pods have no code for, and nova spreads that code
	// over five processes rather than one: the conductor writes the rows the API
	// reads, so a lagging conductor is as dangerous as a lagging API. Only the
	// RollingUpdate phase is this strict; steady-state rollouts keep the
	// surge-tolerant readiness above.
	if nova.Status.UpgradePhase == commonv1.UpgradePhaseRollingUpdate {
		drained, err := novaDeploymentsRolledOut(ctx, children, nova)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !novaDeploymentRolledOut(deploy) || !drained {
			conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
				Type:               "DeploymentReady",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: nova.Generation,
				Reason:             conditionReasonWaitingForDeployment,
				Message: "Waiting for every role to finish rolling out the upgraded image " +
					"before contracting the database schema",
			})
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
		}
	}

	// Transition from RollingUpdate to Contracting when the rollout has drained
	// the old image. The shared flow advances the phase, emits the
	// DeploymentRolloutComplete event, and logs; this step stamps its own
	// DeploymentReady condition and requeues so ReconcileUpgrade runs the
	// contract phase on the next pass. The endpoint is deliberately NOT stamped
	// on the flip pass, matching the sibling operators.
	if database.CompleteRollingUpdate(ctx, r.upgradeFlowParams(ctx, children, nova, art.configMapName)) {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonDeploymentReady,
			Message:            "Nova API deployment is available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	nova.Status.Endpoint = novaStatusEndpoint(nova)
	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:               "DeploymentReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: nova.Generation,
		Reason:             conditionReasonDeploymentReady,
		Message:            "Nova API deployment is available",
	})
	return ctrl.Result{}, nil
}

// novaDeploymentRolledOut reports whether a Deployment has fully converged onto
// its current pod template: the deployment controller has observed the latest
// generation and every replica is updated, ready, and counted, with no surge or
// old-template pod still present. It is stricter than the surge-tolerant
// readiness deployment.EnsureDeployment reports, so the RollingUpdate to
// Contracting flip waits for the old image to drain before the schema is
// contracted.
func novaDeploymentRolledOut(deploy *appsv1.Deployment) bool {
	if deploy.Status.ObservedGeneration < deploy.Generation {
		return false
	}
	desired := desiredReplicas(deploy)
	return deploy.Status.UpdatedReplicas == desired &&
		deploy.Status.ReadyReplicas == desired &&
		deploy.Status.Replicas == desired
}

// novaDeploymentsRolledOut reports whether every non-API role has converged onto
// the image spec.image names. The four roles run their own Deployments, each
// reconciled by its own step earlier in the pass, so the API step reads them
// back off the cluster rather than rebuilding them.
//
// A role whose Deployment does not exist yet is not rolled out: on a fresh
// upgrade pass the object is created by the step ahead of this one, and treating
// an absent Deployment as converged would let the contract phase run against
// processes that never restarted. The image is compared beside the convergence
// check because a Deployment can be fully converged onto the OLD template while
// its own step is still applying the new one.
func novaDeploymentsRolledOut(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) (bool, error) {
	names := []string{conductorName(nova), schedulerName(nova), metadataName(nova)}
	if nova.Spec.ConsoleProxyEnabled() {
		names = append(names, consoleProxyName(nova))
	}

	image := nova.Spec.Image.Reference()
	for _, name := range names {
		key := client.ObjectKey{Namespace: nova.Namespace, Name: name}
		var deploy appsv1.Deployment
		if err := children.Get(ctx, key, &deploy); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("fetching Deployment %s for the rollout gate: %w", key, err)
		}
		if !novaDeploymentRolledOut(&deploy) {
			return false, nil
		}
		containers := deploy.Spec.Template.Spec.Containers
		if len(containers) == 0 || containers[0].Image != image {
			return false, nil
		}
	}
	return true, nil
}

// desiredReplicas is the replica count a Deployment converges to: its own
// .spec.replicas, or one for an HPA-owned count the operator leaves unset (the
// value EnsureDeployment writes on create).
func desiredReplicas(deploy *appsv1.Deployment) int32 {
	if deploy.Spec.Replicas != nil {
		return *deploy.Spec.Replicas
	}
	return 1
}

// buildAPIDeployment constructs the desired Nova API Deployment: the compute API
// as a WSGI application under uWSGI, reading the rendered config and holding no
// state of its own beyond the scratch directories every process needs.
func buildAPIDeployment(nova *novav1alpha1.Nova, art configArtifacts,
	digests workloadDigests,
) *appsv1.Deployment {
	volumes, mounts := novaWorkloadVolumes(nova, art, roleAPI)
	apiProcesses, apiThreads := deployment.EffectiveUWSGIConcurrency(nova.Spec.API.UWSGI)
	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      nova.Namespace,
		Name:           nova.Name,
		Labels:         componentLabels(nova, naming.ComponentAPI),
		SelectorLabels: apiSelectorLabels(nova),
		PodAnnotations: novaPodAnnotations(digests),
		Deployment:     &nova.Spec.API.Deployment,
		Autoscaling:    nova.Spec.Autoscaling,
		DefaultMemory:  commonv1.MemoryForProcesses(commonv1.DefaultMemoryPerProcess(), apiProcesses, apiThreads),
		Container: deployment.ContainerParams{
			Name:    "nova-api",
			Image:   nova.Spec.Image.Reference(),
			Command: novaUWSGICommand(nova.Spec.API.UWSGI, novaAPIPort, novaAPIWSGIModule),
			Env:     novaWorkloadEnv(nova, roleAPI),
			Ports: []corev1.ContainerPort{{
				Name:          "nova-api",
				ContainerPort: novaAPIPort,
			}},
			// All three probes GET /, the version document the compute API answers
			// without a token. Nova registers no oslo healthcheck middleware in its
			// paste pipeline, so / is the cheapest route that proves the WSGI
			// application is loaded. The timings are the sibling operators'.
			StartupProbe: novaUWSGIStartupProbe(novaAPIPort),
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        novaUWSGIProbeHandler(novaAPIPort),
				InitialDelaySeconds: 15,
				PeriodSeconds:       20,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        novaUWSGIProbeHandler(novaAPIPort),
				InitialDelaySeconds: 10,
				PeriodSeconds:       15,
				TimeoutSeconds:      10,
				FailureThreshold:    3,
			},
			VolumeMounts: mounts,
		},
		Volumes: volumes,
	})
}

// novaUWSGICommand constructs the uWSGI container command of one HTTP front
// end. Token emission and default resolution are owned by
// deployment.BuildUWSGICommand; this function only assembles the nova
// parameters.
//
// The launch mode is --module: the image ships no entry script, so the WSGI
// application is imported. It carries no --pyargv, unlike the sibling
// operators: nova's WSGI entry points take no arguments and read the documents
// they load from OS_NOVA_CONFIG_DIR and OS_NOVA_CONFIG_FILES, which
// novaWorkloadEnv sets for both front ends.
func novaUWSGICommand(uwsgi *novav1alpha1.UWSGISpec, port int32, module string) []string {
	return deployment.BuildUWSGICommand(deployment.UWSGICommandParams{
		UWSGI:  uwsgi,
		Bind:   fmt.Sprintf(":%d", port),
		Module: module,
	})
}

// novaUWSGIProbeHandler returns the probe handler of an HTTP front end: a GET of
// / on its own port. The route answers the version document unauthenticated,
// and it is the only one both front ends share.
func novaUWSGIProbeHandler(port int32) corev1.ProbeHandler {
	return corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{
			Path: "/",
			Port: intstr.FromInt32(port),
		},
	}
}

// novaUWSGIStartupProbe returns the startup probe of an HTTP front end. It
// carries the cold-start window: every uWSGI worker imports nova under the
// container's CPU limit, which measured 40 to 78 seconds on a CI node, while the
// liveness probe alone gives up 55 seconds after the container started and
// restarts a front end that is still loading. The timings are the sibling
// operators': 30x10s of startup budget, and an 8s timeout because a
// cold-starting WSGI app can hold even a plain HTTP GET past the kubelet's 1s
// default.
func novaUWSGIStartupProbe(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:     novaUWSGIProbeHandler(port),
		FailureThreshold: 30,
		PeriodSeconds:    10,
		TimeoutSeconds:   8,
	}
}

// buildAPIService builds the Nova API Service on the API port, selecting the API
// component so only the API pods can become its endpoints.
func buildAPIService(nova *novav1alpha1.Nova) *corev1.Service {
	return deployment.BuildService(nova.Namespace, nova.Name, commonLabels(nova),
		apiSelectorLabels(nova), novaAPIPort, novaAPIPort)
}

// buildPodDisruptionBudget constructs the desired PDB for the API Deployment,
// delegating to the shared builder (minAvailable=1 for multi-replica,
// maxUnavailable=1 for single-replica to avoid drain deadlock).
//
// The selector is the API component plus the absence of the Job name label
// (naming.ExcludeJobPods): the migration and archive pods carry no readiness
// probe, so they are Ready from their first moment, and a budget that counted
// them would raise disruptionsAllowed and let a drain evict the API pods the
// budget exists to protect.
//
// The API is the only role with a budget. The other four take their work off the
// message bus or serve a console session that reconnects, so a drain that
// removes their last pod costs no request.
func buildPodDisruptionBudget(nova *novav1alpha1.Nova) *policyv1.PodDisruptionBudget {
	pdb := deployment.BuildPDB(nova.Namespace, nova.Name, commonLabels(nova),
		apiSelectorLabels(nova), &nova.Spec.API.Deployment)
	pdb.Spec.Selector.MatchExpressions = naming.ExcludeJobPods()
	return pdb
}

// novaPodAnnotations assembles the pod-template annotations every Nova workload
// carries, stamping each content digest only when non-empty so the requeue and
// error paths upstream leave the annotation off and cause no spurious rollout.
// It returns nil when every digest is empty, so the pod template carries no
// annotations at all.
func novaPodAnnotations(digests workloadDigests) map[string]string {
	annotations := map[string]string{}
	if digests.apiDSN != "" {
		annotations[apiDBConnectionHashAnnotation] = digests.apiDSN
	}
	if digests.cellDSN != "" {
		annotations[dbConnectionHashAnnotation] = digests.cellDSN
	}
	if digests.authToken != "" {
		annotations[authTokenHashAnnotation] = digests.authToken
	}
	if digests.transport != "" {
		annotations[transportURLHashAnnotation] = digests.transport
	}
	if len(annotations) == 0 {
		return nil
	}
	return annotations
}

// novaRPCPodAnnotations are the pod-template annotations of the four processes
// behind the API: the digests every workload carries plus the installed release.
//
// The release annotation is what rolls them once more after the contract phase
// of an upgrade. The scheduler and the conductor cache the RPC version their
// peers announced when they started, and that cache pins the wire format for the
// life of the process, so a scheduler that started before the upgrade keeps
// addressing the conductor in the old one; the metadata API and the console
// proxy read rows the contract phase rewrites. The API needs no such stamp: it
// rolled during the RollingUpdate phase and came up holding the new minimum.
func novaRPCPodAnnotations(nova *novav1alpha1.Nova, digests workloadDigests) map[string]string {
	annotations := novaPodAnnotations(digests)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[installedReleaseAnnotation] = effectiveInstalledRelease(nova)
	return annotations
}

// novaMetadataPodAnnotations are the metadata API's annotations: the bus set
// plus the digest of the shared secret. The metadata API is the only process
// that reads that secret, and it reads it from an env override rather than from
// a mounted file, so a rotation reaches the running pods through this stamp.
func novaMetadataPodAnnotations(nova *novav1alpha1.Nova, digests workloadDigests) map[string]string {
	annotations := novaRPCPodAnnotations(nova, digests)
	if digests.metadataSecret != "" {
		annotations[metadataSecretHashAnnotation] = digests.metadataSecret
	}
	return annotations
}

// roleOverlayKeys names the ConfigMap key each console-script role reads its own
// overlay from. The three processes are started with two --config-dir arguments,
// the shared directory and one of their own, so a worker count or a listen
// address reaches exactly the process that owns it. The API and the metadata API
// are absent: both are WSGI applications that name the documents they load
// through OS_NOVA_CONFIG_FILES instead.
var roleOverlayKeys = map[workloadRole]string{
	roleScheduler:    schedulerConfDataKey,
	roleConductor:    conductorConfDataKey,
	roleConsoleProxy: novncproxyConfDataKey,
}

// roleOverlayDir is the directory one console-script role reads its overlay
// from: the key it holds plus the ".d" suffix, which is the shape novaConfigDir
// has for nova.conf. Deriving it from the key keeps the projected file and the
// directory that holds it from drifting apart.
func roleOverlayDir(role workloadRole) string {
	return "/etc/nova/" + roleOverlayKeys[role] + ".d"
}

// novaWorkloadVolumes returns the volumes and the matching container mounts of
// one Nova workload: the config files its role reads, the writable state
// directory [DEFAULT] state_path names, a writable /tmp, and the TLS
// projections that exist only while their spec blocks do.
//
// The config ConfigMap is one object for the whole CR, and every pod mounts it,
// so the projection is what keeps the roles apart. oslo.config reads every file
// in a --config-dir, so a process that saw another role's overlay would take
// over its worker count or its listen address.
func novaWorkloadVolumes(nova *novav1alpha1.Nova, art configArtifacts,
	role workloadRole,
) ([]corev1.Volume, []corev1.VolumeMount) {
	volumes := []corev1.Volume{{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: art.configMapName},
				Items:                novaConfigItems(art, role),
			},
		},
	}}
	mounts := []corev1.VolumeMount{
		{Name: configVolumeName, MountPath: novaConfigDir, ReadOnly: true},
	}

	if key, ok := roleOverlayKeys[role]; ok {
		name := string(role) + "-overlay"
		volumes = append(volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: art.configMapName},
					Items:                []corev1.KeyToPath{{Key: key, Path: key}},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name: name, MountPath: roleOverlayDir(role), ReadOnly: true,
		})
	}

	volumes = append(volumes,
		corev1.Volume{
			Name:         stateVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		corev1.Volume{
			Name:         tmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	mounts = append(mounts,
		corev1.VolumeMount{Name: stateVolumeName, MountPath: novaStatePath},
		corev1.VolumeMount{Name: tmpVolumeName, MountPath: tmpMountPath})

	tlsVolumes, tlsMounts := novaDBTLSVolumesAndMounts(nova)
	volumes = append(volumes, tlsVolumes...)
	mounts = append(mounts, tlsMounts...)

	if nova.Spec.Messaging.TLS != nil {
		caVolume, caMount := rabbitmqCAVolumeAndMount(nova)
		volumes = append(volumes, caVolume)
		mounts = append(mounts, caMount)
	}
	return volumes, mounts
}

// novaConfigItems names the keys of the rendered ConfigMap one role projects
// into novaConfigDir: the shared document, the logging file when this render
// produced one, and the metadata overlay for the metadata API alone. The
// metadata API finds its overlay here rather than in a directory of its own
// because it is a WSGI application, and OS_NOVA_CONFIG_FILES names documents
// inside OS_NOVA_CONFIG_DIR.
//
// The keys are filtered out of art.dataKeys rather than listed, so a render that
// produced no logging.ini projects none.
func novaConfigItems(art configArtifacts, role workloadRole) []corev1.KeyToPath {
	projected := map[string]bool{novaConfDataKey: true, loggingINIDataKey: true}
	if role == roleMetadata {
		projected[metadataConfDataKey] = true
	}

	items := make([]corev1.KeyToPath, 0, len(projected))
	for _, key := range art.dataKeys {
		if projected[key] {
			items = append(items, corev1.KeyToPath{Key: key, Path: key})
		}
	}
	return items
}

// rabbitmqCAVolumeAndMount projects the broker's CA bundle at
// rabbitmqCAMountPath under the fixed file name rabbitmqCAFilePath ends in,
// which is what [oslo_messaging_rabbit] ssl_ca_file points at. The projection
// renames whichever key the CR names to that file, so a bundle stored under any
// key lands where the rendered config expects it. Callers must only invoke it
// when spec.messaging.tls is set.
func rabbitmqCAVolumeAndMount(nova *novav1alpha1.Nova) (corev1.Volume, corev1.VolumeMount) {
	ref := nova.Spec.Messaging.TLS.CABundleSecretRef
	volume := corev1.Volume{
		Name: rabbitmqCAVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  ref.Name,
				DefaultMode: ptr.To(int32(0o444)),
				Items: []corev1.KeyToPath{
					{Key: effectiveMessagingCAKey(nova), Path: database.TLSCAFileName},
				},
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      rabbitmqCAVolumeName,
		MountPath: rabbitmqCAMountPath,
		ReadOnly:  true,
	}
	return volume, mount
}
