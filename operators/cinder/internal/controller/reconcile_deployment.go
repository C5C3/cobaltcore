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
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// cinderAppName is the app.kubernetes.io/name label value applied to every
// Cinder-owned sub-resource. It matches the literal the validating webhook uses
// for its TopologySpreadConstraints selector check, so the two never drift.
const cinderAppName = "cinder"

// cinderAPIPort is the TCP port cinder-api serves the block-storage API on, the
// upstream default every client and every catalog entry assumes.
const cinderAPIPort int32 = 8776

// cinderWSGIModule is the uWSGI --module value: the import path of the WSGI
// application object the cinder image ships. It has to stay in lockstep with
// images/cinder, which installs cinder with the same module layout.
const cinderWSGIModule = "cinder.wsgi.api:application"

// Pod volume names shared by the four workloads. The config volume carries the
// name the shared migration-Job builder uses for its own config mount, so a Job
// and a pod of the same CR describe the same file at the same path under the
// same volume name; reconcileConfig also reads the config volume back off the
// live API Deployment to recover the last-good artefact names.
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

// Pod-template annotation keys stamped with content digests so an env-var-
// consumed credential change rolls the workload: the value is not
// volume-mounted, so it only takes effect on a Pod restart.
const (
	dbConnectionHashAnnotation = "cinder.c5c3.io/db-connection-hash"
	// #nosec G101 -- annotation key naming a digest, not a credential.
	authTokenHashAnnotation = "cinder.c5c3.io/authtoken-hash"
	// #nosec G101 -- annotation key naming a digest, not a credential.
	transportURLHashAnnotation = "cinder.c5c3.io/transport-url-hash"
	// installedReleaseAnnotation carries the release the schema is installed at.
	// It is stamped on the scheduler, volume and backup pods alone: those three
	// cache the RPC versions their peers reported when they started, so after the
	// contract phase of an upgrade they have to restart once more to drop that
	// cache. The API does not carry it because it rolled during the RollingUpdate
	// phase and already holds the new minimum.
	installedReleaseAnnotation = "cinder.c5c3.io/installed-release"
)

// Condition reason constants for DeploymentReady.
const (
	conditionReasonDeploymentReady      = "DeploymentReady"
	conditionReasonWaitingForDeployment = "WaitingForDeployment"
)

// workloadDigests are the content digests the credential steps return, stamped
// into the pod templates so a rotated database credential, service-user password
// or broker credential rolls the pods. Each is empty on the requeue and error
// paths upstream, where no derived Secret was materialised.
type workloadDigests struct {
	dsn       string
	authToken string
	transport string
}

// commonLabels returns the standard Kubernetes labels applied to all resources
// owned by this Cinder instance, delegating to the shared naming package.
func commonLabels(cinder *cinderv1alpha1.Cinder) map[string]string {
	return naming.CommonLabels(cinderAppName, cinder.Name)
}

// selectorLabels returns the name+instance label pair every pod of this Cinder
// carries. It is not a pod selector on its own: one Cinder owns four kinds of
// Deployment, so every selector narrows it by the component (see
// componentSelectorLabels).
func selectorLabels(cinder *cinderv1alpha1.Cinder) map[string]string {
	return naming.SelectorLabels(cinderAppName, cinder.Name)
}

// componentLabels returns the pod-template labels for a workload of the given
// component. It is commonLabels plus app.kubernetes.io/component, so the result
// stays a superset of that workload's selector labels.
func componentLabels(cinder *cinderv1alpha1.Cinder, component string) map[string]string {
	return naming.ComponentLabels(cinderAppName, cinder.Name, component)
}

// apiSelectorLabels returns the pod selector of the API Deployment, its Service
// and its PodDisruptionBudget: the shared selector labels narrowed by
// app.kubernetes.io/component=api.
//
// It is narrowed from the first pass, with none of the two-phase migration the
// single-Deployment operators need. One Cinder owns the API, the scheduler, one
// volume service per backend and the backup service, so a selector without the
// component key would match all of their pods: the Deployments would adopt each
// other's, and the Service would route API requests to a scheduler that serves
// no HTTP. No Cinder Deployment predating the component label exists, so there
// is no selector to migrate off.
func apiSelectorLabels(cinder *cinderv1alpha1.Cinder) map[string]string {
	return naming.APISelectorLabels(cinderAppName, cinder.Name)
}

// componentSelectorLabels returns the pod selector of the Deployment of one
// non-API component: the shared selector labels narrowed by that component.
func componentSelectorLabels(cinder *cinderv1alpha1.Cinder, component string) map[string]string {
	labels := selectorLabels(cinder)
	labels[naming.LabelKeyComponent] = component
	return labels
}

// internalCinderURL is the cluster-local Service URL of the Cinder API: what
// status.endpoint reports and what an in-cluster client dials.
func internalCinderURL(cinder *cinderv1alpha1.Cinder) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", cinder.Name, cinder.Namespace, cinderAPIPort)
}

// effectiveInstalledRelease returns the release the pods of this CR run against:
// status.installedRelease once a db-sync has promoted it, and
// spec.openStackRelease while it is still empty. Falling back to the spec keeps
// a fresh install from stamping an empty annotation that would change to the
// real release on the very next pass and roll every pod for nothing.
func effectiveInstalledRelease(cinder *cinderv1alpha1.Cinder) string {
	if cinder.Status.InstalledRelease != "" {
		return cinder.Status.InstalledRelease
	}
	return cinder.Spec.OpenStackRelease
}

// reconcileDeployment ensures the Cinder API Deployment, Service and PDB exist
// with the correct spec. It sets the DeploymentReady condition and stamps the
// status endpoint once the Deployment is available.
//
// art names the rendered config ConfigMap and its keys; the pods mount every key
// but scheduler.conf, which carries the scheduler's own host identity.
func (r *CinderReconciler) reconcileDeployment(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, art configArtifacts, digests workloadDigests,
) (ctrl.Result, error) {
	deploy := buildCinderDeployment(cinder, art, digests)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, cinder, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Deployment: %w", err)
	}

	svc := buildCinderService(cinder)
	if err := deployment.EnsureService(ctx, children, r.Scheme, cinder, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Service: %w", err)
	}

	pdb := buildPodDisruptionBudget(cinder)
	if err := deployment.EnsurePDB(ctx, children, r.Scheme, cinder, pdb); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring PodDisruptionBudget: %w", err)
	}

	if !ready {
		log.FromContext(ctx).Info("Cinder API deployment not ready, requeuing")
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonWaitingForDeployment,
			Message:            "Cinder API deployment is not yet available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	// Hold the RollingUpdate → Contracting flip until the Deployment has FULLY
	// converged onto the target-release image. The `ready` signal above comes from
	// deployment.IsDeploymentReady, which is surge-tolerant: under the default
	// MaxSurge=1/MaxUnavailable=0 strategy a new pod is added before an old one is
	// removed, so it turns true as soon as the first new-image pod is Ready while
	// old-image pods still serve. The contract phase then runs the data migrations
	// those old pods have no code for. Gate the flip on every replica being
	// updated, ready, and counted, and requeue to wait otherwise. Only the
	// RollingUpdate upgrade phase is stricter here; steady-state rollouts keep the
	// surge-tolerant readiness above.
	if cinder.Status.UpgradePhase == commonv1.UpgradePhaseRollingUpdate &&
		!cinderDeploymentRolledOut(deploy) {
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonWaitingForDeployment,
			Message:            "Waiting for the upgraded image to finish rolling out before contracting the database schema",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	// Transition from RollingUpdate to Contracting when the rollout has drained
	// the old image. The shared flow advances the phase, emits the
	// DeploymentRolloutComplete event, and logs; this step stamps its own
	// DeploymentReady condition and requeues so ReconcileUpgrade runs the contract
	// phase on the next pass. The endpoint is deliberately NOT stamped on the flip
	// pass, matching the sibling operators.
	if database.CompleteRollingUpdate(ctx, r.upgradeFlowParams(ctx, children, cinder, art.configMapName)) {
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonDeploymentReady,
			Message:            "Cinder API deployment is available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	// Status.Endpoint derivation is delegated to cinderStatusEndpoint so the
	// gateway-aware public URL is used when spec.gateway is set, and the
	// cluster-local URL otherwise.
	cinder.Status.Endpoint = cinderStatusEndpoint(cinder)
	conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
		Type:               "DeploymentReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cinder.Generation,
		Reason:             conditionReasonDeploymentReady,
		Message:            "Cinder API deployment is available",
	})
	return ctrl.Result{}, nil
}

// cinderDeploymentRolledOut reports whether a Deployment has fully converged
// onto its current pod template: the deployment controller has observed the
// latest generation and every replica is updated, ready, and counted, with no
// surge or old-template pod still present. It is stricter than the
// surge-tolerant readiness deployment.EnsureDeployment reports, so the shared
// upgrade flow's RollingUpdate → Contracting flip waits for the old image to
// drain before the schema is contracted.
func cinderDeploymentRolledOut(deploy *appsv1.Deployment) bool {
	if deploy.Status.ObservedGeneration < deploy.Generation {
		return false
	}
	desired := desiredReplicas(deploy)
	return deploy.Status.UpdatedReplicas == desired &&
		deploy.Status.ReadyReplicas == desired &&
		deploy.Status.Replicas == desired
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

// buildCinderDeployment constructs the desired Cinder API Deployment: the WSGI
// application under uWSGI, reading the rendered config and holding no state of
// its own beyond the scratch directories every process needs.
func buildCinderDeployment(cinder *cinderv1alpha1.Cinder, art configArtifacts,
	digests workloadDigests,
) *appsv1.Deployment {
	volumes, mounts := cinderWorkloadVolumes(cinder, art)
	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      cinder.Namespace,
		Name:           cinder.Name,
		Labels:         componentLabels(cinder, naming.ComponentAPI),
		SelectorLabels: apiSelectorLabels(cinder),
		PodAnnotations: cinderPodAnnotations(digests),
		Deployment:     &cinder.Spec.API.Deployment,
		Autoscaling:    cinder.Spec.Autoscaling,
		Container: deployment.ContainerParams{
			Name:    "cinder-api",
			Image:   cinder.Spec.Image.Reference(),
			Command: cinderUWSGICommand(cinder),
			Env:     cinderWorkloadEnv(cinder),
			Ports: []corev1.ContainerPort{{
				Name:          "cinder-api",
				ContainerPort: cinderAPIPort,
			}},
			// Both probes GET /healthcheck, served by the oslo healthcheck
			// middleware without touching the database or the message bus. The
			// timings are the sibling operators'.
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        cinderHealthcheckProbeHandler(),
				InitialDelaySeconds: 15,
				PeriodSeconds:       20,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        cinderHealthcheckProbeHandler(),
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

// cinderUWSGICommand constructs the uWSGI container command for the API
// container. Token emission and default resolution are owned by
// deployment.BuildUWSGICommand; this function only assembles the cinder
// parameters.
//
// The launch mode is --module: the image ships no entry script, so the WSGI
// application is imported from cinderWSGIModule. cinder's WSGI entry point
// parses the arguments uWSGI forwards through --pyargv, which is how the
// imported application learns the config directory it has no argv to carry.
func cinderUWSGICommand(cinder *cinderv1alpha1.Cinder) []string {
	return deployment.BuildUWSGICommand(deployment.UWSGICommandParams{
		UWSGI:        cinder.Spec.API.UWSGI,
		Bind:         fmt.Sprintf(":%d", cinderAPIPort),
		Module:       cinderWSGIModule,
		TrailingArgs: []string{"--pyargv", "--config-dir " + cinderConfigDir},
	})
}

// cinderHealthcheckProbeHandler returns the shared readiness/liveness probe
// handler of the API container: an HTTP GET of /healthcheck on the API port.
func cinderHealthcheckProbeHandler() corev1.ProbeHandler {
	return corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{
			Path: "/healthcheck",
			Port: intstr.FromInt32(cinderAPIPort),
		},
	}
}

// buildCinderService builds the Cinder API Service on the API port, selecting
// the API component so only the API pods can become its endpoints.
func buildCinderService(cinder *cinderv1alpha1.Cinder) *corev1.Service {
	return deployment.BuildService(cinder.Namespace, cinder.Name, commonLabels(cinder),
		apiSelectorLabels(cinder), cinderAPIPort, cinderAPIPort)
}

// buildPodDisruptionBudget constructs the desired PDB for the API Deployment,
// delegating to the shared builder (minAvailable=1 for multi-replica,
// maxUnavailable=1 for single-replica to avoid drain deadlock).
//
// The selector is the API component plus the absence of the Job name label
// (naming.ExcludeJobPods): the db-purge and service-remove pods carry no
// readiness probe, so they are Ready from their first moment, and a budget that
// counted them would raise disruptionsAllowed and let a drain evict the API pods
// the budget exists to protect.
func buildPodDisruptionBudget(cinder *cinderv1alpha1.Cinder) *policyv1.PodDisruptionBudget {
	pdb := deployment.BuildPDB(cinder.Namespace, cinder.Name, commonLabels(cinder),
		apiSelectorLabels(cinder), &cinder.Spec.API.Deployment)
	pdb.Spec.Selector.MatchExpressions = naming.ExcludeJobPods()
	return pdb
}

// cinderPodAnnotations assembles the pod-template annotations every Cinder
// workload carries, stamping each content digest only when non-empty so the
// requeue and error paths upstream leave the annotation off and cause no
// spurious rollout. It returns nil when every digest is empty, so the pod
// template carries no annotations at all.
func cinderPodAnnotations(digests workloadDigests) map[string]string {
	annotations := map[string]string{}
	if digests.dsn != "" {
		annotations[dbConnectionHashAnnotation] = digests.dsn
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

// cinderRPCPodAnnotations are the pod-template annotations of the three
// processes that talk to their peers over the message bus: the digests every
// workload carries plus the installed release.
//
// The release annotation is what rolls them once more after the contract phase
// of an upgrade. Each of them caches the RPC version its peers announced when it
// started, and that cache pins the wire format for the life of the process, so a
// scheduler that started before the upgrade keeps addressing the volume services
// in the old one. The API needs no such stamp: it rolled during the
// RollingUpdate phase and came up holding the new minimum.
func cinderRPCPodAnnotations(cinder *cinderv1alpha1.Cinder, digests workloadDigests) map[string]string {
	annotations := cinderPodAnnotations(digests)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[installedReleaseAnnotation] = effectiveInstalledRelease(cinder)
	return annotations
}

// cinderWorkloadVolumes returns the volumes and the matching container mounts
// every Cinder workload carries: the rendered config, the writable state
// directory [DEFAULT] state_path names, a writable /tmp, and the two TLS
// projections that exist only while their spec block does. The API, the
// scheduler, the volume services and the backup service share it, so the file
// layout a pod sees is the layout the rendered config describes, whichever
// workload reads it.
func cinderWorkloadVolumes(cinder *cinderv1alpha1.Cinder, art configArtifacts) ([]corev1.Volume, []corev1.VolumeMount) {
	configVol, configMount := configVolumeAndMount(art)

	volumes := []corev1.Volume{
		configVol,
		{
			Name:         stateVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name:         tmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	mounts := []corev1.VolumeMount{
		configMount,
		{Name: stateVolumeName, MountPath: cinderStatePath},
		{Name: tmpVolumeName, MountPath: tmpMountPath},
	}

	if cinder.Spec.Database.TLS.IsEnabled() {
		tlsVol, tlsMount := cinderDBTLSVolumeAndMount(cinder)
		volumes = append(volumes, tlsVol)
		mounts = append(mounts, tlsMount)
	}
	if cinder.Spec.Messaging.TLS != nil {
		caVol, caMount := rabbitmqCAVolumeAndMount(cinder)
		volumes = append(volumes, caVol)
		mounts = append(mounts, caMount)
	}
	return volumes, mounts
}

// configVolumeAndMount projects the rendered config ConfigMap read-only at
// cinderConfigDir, naming every key it carries except scheduler.conf: that file
// is the scheduler's own [DEFAULT] host overlay, and oslo.config reads every
// file in a --config-dir, so a pod that mounted it would register under the
// scheduler's identity.
func configVolumeAndMount(art configArtifacts) (corev1.Volume, corev1.VolumeMount) {
	items := make([]corev1.KeyToPath, 0, len(art.dataKeys))
	for _, key := range art.dataKeys {
		if key == schedulerConfDataKey {
			continue
		}
		items = append(items, corev1.KeyToPath{Key: key, Path: key})
	}
	volume := corev1.Volume{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: art.configMapName},
				Items:                items,
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      configVolumeName,
		MountPath: cinderConfigDir,
		ReadOnly:  true,
	}
	return volume, mount
}

// rabbitmqCAVolumeAndMount projects the broker's CA bundle at
// rabbitmqCAMountPath under the fixed file name rabbitmqCAFilePath ends in,
// which is what [oslo_messaging_rabbit] ssl_ca_file points at. The projection
// renames whichever key the CR names to that file, so a bundle stored under any
// key lands where the rendered config expects it. Callers must only invoke it
// when spec.messaging.tls is set.
func rabbitmqCAVolumeAndMount(cinder *cinderv1alpha1.Cinder) (corev1.Volume, corev1.VolumeMount) {
	ref := cinder.Spec.Messaging.TLS.CABundleSecretRef
	key := ref.Key
	if key == "" {
		key = database.TLSCAFileName
	}
	volume := corev1.Volume{
		Name: rabbitmqCAVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  ref.Name,
				DefaultMode: ptr.To(int32(0o444)),
				Items: []corev1.KeyToPath{
					{Key: key, Path: database.TLSCAFileName},
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
