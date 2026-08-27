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
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// neutronAppName is the app.kubernetes.io/name label value applied to every
// Neutron-owned sub-resource. It matches the literal the validating webhook uses
// for its TopologySpreadConstraints selector check, so the two never drift.
const neutronAppName = "neutron"

// neutronAPIPort is the TCP port neutron-server serves its API on, the upstream
// default every client and every catalog entry assumes.
const neutronAPIPort int32 = 9696

// neutronWSGIModule is the uWSGI --module value: the import path of the WSGI
// application object the neutron image ships. It has to stay in lockstep with
// images/neutron, which installs neutron with the same module layout.
const neutronWSGIModule = "neutron.wsgi.api"

// Pod volume names. The config volume carries the name the shared migration-Job
// builder uses for its own config mount, so a Job and a pod of the same CR
// describe the same file at the same path under the same volume name.
const (
	configVolumeName     = "config"
	ovnTLSVolumeName     = "ovn-tls"
	stateVolumeName      = "state"
	dbTLSVolumeName      = "db-tls"
	rabbitmqCAVolumeName = "rabbitmq-ca"
)

// Pod-template annotation keys stamped with content digests so an env-var-
// consumed credential change rolls the workload: the value is not
// volume-mounted, so it only takes effect on a Pod restart. The OVN client
// identity is mounted rather than env-injected, but a re-issued keypair replaces
// the Secret's content under an unchanged name, and the kubelet's projection
// refresh does not restart the process holding the old certificate open.
const (
	dbConnectionHashAnnotation = "neutron.c5c3.io/db-connection-hash"
	// #nosec G101 -- annotation key naming a digest, not a credential.
	authTokenHashAnnotation = "neutron.c5c3.io/authtoken-hash"
	// #nosec G101 -- annotation key naming a digest, not a credential.
	transportURLHashAnnotation = "neutron.c5c3.io/transport-url-hash"
	ovnClientHashAnnotation    = "neutron.c5c3.io/ovn-client-hash"
)

// Condition reason constants for DeploymentReady.
const (
	conditionReasonDeploymentReady      = "DeploymentReady"
	conditionReasonWaitingForDeployment = "WaitingForDeployment"
)

// commonLabels returns the standard Kubernetes labels applied to all resources
// owned by this Neutron instance, delegating to the shared naming package.
func commonLabels(neutron *neutronv1alpha1.Neutron) map[string]string {
	return naming.CommonLabels(neutronAppName, neutron.Name)
}

// componentLabels returns the pod-template labels for a workload of the given
// component. It is commonLabels plus app.kubernetes.io/component, so the result
// stays a superset of the selector labels of every workload of this CR.
func componentLabels(neutron *neutronv1alpha1.Neutron, component string) map[string]string {
	return naming.ComponentLabels(neutronAppName, neutron.Name, component)
}

// apiSelectorLabels returns the pod selector of the API Deployment, its Service
// and its PodDisruptionBudget: the shared selector labels narrowed by
// app.kubernetes.io/component=api.
//
// It is the component-narrowed set rather than the plain naming.SelectorLabels
// the sibling operators put on their Deployment selector, because one Neutron
// owns three Deployments: the API and the two workers. A selector without the
// component key matches all of their pods, so each Deployment would count and
// adopt the others' pods, and the Service would route API traffic to a worker
// that serves no HTTP. The immutability argument that keeps the siblings on the
// wide selector does not apply here: no Neutron Deployment predating the
// component label exists, so there is no selector to migrate.
func apiSelectorLabels(neutron *neutronv1alpha1.Neutron) map[string]string {
	return naming.APISelectorLabels(neutronAppName, neutron.Name)
}

// internalNeutronURL is the cluster-local Service URL of the Neutron API: what
// status.endpoint reports and what an in-cluster client dials.
func internalNeutronURL(neutron *neutronv1alpha1.Neutron) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", neutron.Name, neutron.Namespace, neutronAPIPort)
}

// reconcileDeployment ensures the Neutron API Deployment, Service, and PDB exist
// with the correct spec. It sets the DeploymentReady condition and stamps the
// status endpoint when the Deployment becomes available.
//
// configMapName names the rendered config ConfigMap the pods mount. It is never
// empty here: the config step returns a name or an error, and an error
// short-circuits the pipeline ahead of this step.
//
// The four digests are stamped into pod-template annotations so a rotated
// credential or a re-issued OVN client certificate rolls the pods. Each
// annotation is omitted when its digest is empty, which is what the requeue and
// error paths upstream return.
func (r *NeutronReconciler) reconcileDeployment(ctx context.Context, children client.Client,
	neutron *neutronv1alpha1.Neutron, configMapName, dsnDigest, authtokenDigest, transportDigest, ovnClientDigest string,
) (ctrl.Result, error) {
	deploy := buildNeutronDeployment(neutron, configMapName, dsnDigest, authtokenDigest, transportDigest, ovnClientDigest)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, neutron, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Deployment: %w", err)
	}

	// The Service selects the API component from the first pass, with none of the
	// two-phase narrowing the sibling operators need. Their wide-then-narrow
	// migration exists for pods a pre-component-label operator created; this
	// operator has never rendered a template without the label, so the narrow
	// selector matches every pod that can exist.
	svc := buildNeutronService(neutron)
	if err := deployment.EnsureService(ctx, children, r.Scheme, neutron, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Service: %w", err)
	}

	pdb := buildPodDisruptionBudget(neutron)
	if err := deployment.EnsurePDB(ctx, children, r.Scheme, neutron, pdb); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring PodDisruptionBudget: %w", err)
	}

	if !ready {
		log.FromContext(ctx).Info("Neutron API deployment not ready, requeuing")
		conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: neutron.Generation,
			Reason:             conditionReasonWaitingForDeployment,
			Message:            "Neutron API deployment is not yet available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	// Hold the RollingUpdate → Contracting flip until the Deployment has FULLY
	// converged onto the target-release image. The `ready` signal above comes from
	// deployment.IsDeploymentReady, which is surge-tolerant: under the default
	// MaxSurge=1/MaxUnavailable=0 strategy a new pod is added before an old one is
	// removed, so it turns true as soon as the first new-image pod is Ready while
	// old-image pods still serve. The contract phase then drops what those old
	// pods still read — so gate the flip on every replica being updated, ready,
	// and counted, and requeue to wait otherwise. Only the RollingUpdate upgrade
	// phase is stricter here; steady-state rollouts keep the surge-tolerant
	// readiness above.
	if neutron.Status.UpgradePhase == commonv1.UpgradePhaseRollingUpdate && !neutronDeploymentRolledOut(deploy) {
		conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: neutron.Generation,
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
	if database.CompleteRollingUpdate(ctx, r.upgradeFlowParams(ctx, children, neutron, configMapName)) {
		conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: neutron.Generation,
			Reason:             conditionReasonDeploymentReady,
			Message:            "Neutron API deployment is available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	neutron.Status.Endpoint = internalNeutronURL(neutron)
	conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
		Type:               "DeploymentReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: neutron.Generation,
		Reason:             conditionReasonDeploymentReady,
		Message:            "Neutron API deployment is available",
	})
	return ctrl.Result{}, nil
}

// neutronDeploymentRolledOut reports whether the Neutron API Deployment has
// fully converged onto its current pod template: the deployment controller has
// observed the latest generation and every replica is updated, ready, and
// counted, with no surge or old-template pod still present. It is stricter than
// the surge-tolerant readiness deployment.EnsureDeployment reports, so the
// shared upgrade flow's RollingUpdate → Contracting flip waits for the old image
// to drain before the schema is contracted.
func neutronDeploymentRolledOut(deploy *appsv1.Deployment) bool {
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

// buildNeutronDeployment constructs the desired Neutron API Deployment. The
// rendered config ConfigMap mounts read-only as the whole neutronConfigMountPath
// directory, shadowing the image's own /etc/neutron; the OVN client identity and
// the writable state directory mount beside it; and the database URL, the
// transport URL, and the service-user password are injected via env vars so no
// credential material enters the config document.
func buildNeutronDeployment(neutron *neutronv1alpha1.Neutron,
	configMapName, dsnDigest, authtokenDigest, transportDigest, ovnClientDigest string,
) *appsv1.Deployment {
	volumes, mounts := neutronWorkloadVolumes(neutron, configMapName)
	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      neutron.Namespace,
		Name:           neutron.Name,
		Labels:         componentLabels(neutron, naming.ComponentAPI),
		SelectorLabels: apiSelectorLabels(neutron),
		PodAnnotations: neutronPodAnnotations(dsnDigest, authtokenDigest, transportDigest, ovnClientDigest),
		Deployment:     &neutron.Spec.Deployment,
		Autoscaling:    neutron.Spec.Autoscaling,
		Container: deployment.ContainerParams{
			Name:    "neutron-api",
			Image:   neutron.Spec.Image.Reference(),
			Command: neutronUWSGICommand(neutron.Spec.APIServer),
			Env:     neutronAPIEnv(neutron),
			Ports: []corev1.ContainerPort{{
				Name:          "neutron-api",
				ContainerPort: neutronAPIPort,
			}},
			// All three probes GET the API root, which serves the version document
			// without a token and without touching the database. The startup probe
			// carries the cold-start window: every uWSGI worker imports the whole
			// plugin stack under the container's CPU limit, which stretches past the
			// liveness budget once spec.apiServer.uwsgi.processes rises above the
			// default. The timings are the sibling operators': 30x10s of startup
			// budget, and an 8s timeout because a cold-starting WSGI app can hold even
			// a plain HTTP GET past the kubelet's 1s default.
			StartupProbe: &corev1.Probe{
				ProbeHandler:     neutronAPIProbeHandler(),
				FailureThreshold: 30,
				PeriodSeconds:    10,
				TimeoutSeconds:   8,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        neutronAPIProbeHandler(),
				InitialDelaySeconds: 15,
				PeriodSeconds:       20,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        neutronAPIProbeHandler(),
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

// neutronAPIEnv returns the environment of the API container: where the
// configuration is, and the three credentials that never enter it.
//
// The two OS_NEUTRON_* variables are how a uWSGI-imported application finds its
// configuration. There is no argv to carry --config-file, so the directory and
// the file list travel in the environment instead, naming the same two files, in
// the same order, that the migration Jobs and the workers pass on their command
// line.
func neutronAPIEnv(neutron *neutronv1alpha1.Neutron) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "OS_NEUTRON_CONFIG_DIR", Value: neutronConfigMountPath},
		{Name: "OS_NEUTRON_CONFIG_FILES", Value: neutronConfDataKey + ";" + ml2ConfDataKey},
	}
	env = append(env, neutronWorkloadEnv(neutron)...)
	return append(env, keystoneauth.PasswordEnvVar(
		neutron.Spec.ServiceUser.SecretRef.Name, effectiveServiceUserKey(neutron)))
}

// neutronUWSGICommand constructs the uWSGI container command for the Neutron API
// container from the given apiServer spec. Token emission and default resolution
// are owned by deployment.BuildUWSGICommand; this function only assembles the
// neutron parameters.
//
// The launch mode is --module: the image ships no entry script, so the WSGI
// application is imported from neutronWSGIModule. The trailing --ini names the
// uwsgi.ini the config step renders into the same ConfigMap, which carries the
// start-time marker the API reports its uptime from.
func neutronUWSGICommand(apiServer *neutronv1alpha1.APIServerSpec) []string {
	var uwsgi *neutronv1alpha1.UWSGISpec
	if apiServer != nil {
		uwsgi = apiServer.UWSGI
	}

	return deployment.BuildUWSGICommand(deployment.UWSGICommandParams{
		UWSGI:        uwsgi,
		Bind:         fmt.Sprintf(":%d", neutronAPIPort),
		Module:       neutronWSGIModule,
		TrailingArgs: []string{"--ini", neutronConfigMountPath + "/" + uwsgiConfDataKey},
	})
}

// neutronAPIProbeHandler returns the shared startup/readiness/liveness probe
// handler: an HTTP GET of the API root on the API port.
func neutronAPIProbeHandler() corev1.ProbeHandler {
	return corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{
			Path: "/",
			Port: intstr.FromInt32(neutronAPIPort),
		},
	}
}

// neutronPodAnnotations assembles the pod-template annotations, stamping each
// content digest only when non-empty so the requeue and error paths (which
// return an empty digest) leave the annotation off and cause no spurious
// rollout. Returns nil when every digest is empty so the pod template carries no
// annotations.
func neutronPodAnnotations(dsnDigest, authtokenDigest, transportDigest, ovnClientDigest string) map[string]string {
	annotations := map[string]string{}
	if dsnDigest != "" {
		annotations[dbConnectionHashAnnotation] = dsnDigest
	}
	if authtokenDigest != "" {
		annotations[authTokenHashAnnotation] = authtokenDigest
	}
	if transportDigest != "" {
		annotations[transportURLHashAnnotation] = transportDigest
	}
	if ovnClientDigest != "" {
		annotations[ovnClientHashAnnotation] = ovnClientDigest
	}
	if len(annotations) == 0 {
		return nil
	}
	return annotations
}

// neutronWorkloadVolumes returns the volumes and the matching container mounts
// every Neutron workload carries: the rendered config, the OVN client identity
// the mechanism driver presents to both databases, the writable state directory
// [DEFAULT] state_path names, and the two TLS projections that exist only while
// their spec block does. The API Deployment, the two worker Deployments, and the
// ovn-db-sync CronJob share it, so the file layout a pod sees is the layout the
// rendered config describes, whichever workload reads it.
func neutronWorkloadVolumes(neutron *neutronv1alpha1.Neutron, configMapName string) ([]corev1.Volume, []corev1.VolumeMount) {
	configVol, configMount := configVolumeAndMount(configMapName)
	ovnVol, ovnMount := ovnTLSVolumeAndMount(neutron)
	stateVol, stateMount := stateVolumeAndMount()

	volumes := []corev1.Volume{configVol, ovnVol, stateVol}
	mounts := []corev1.VolumeMount{configMount, ovnMount, stateMount}

	if neutronDBTLSEnabled(neutron) {
		tlsVol, tlsMount := neutronDBTLSVolumeAndMount(neutron)
		volumes = append(volumes, tlsVol)
		mounts = append(mounts, tlsMount)
	}
	if neutron.Spec.Messaging.TLS != nil {
		caVol, caMount := rabbitmqCAVolumeAndMount(neutron)
		volumes = append(volumes, caVol)
		mounts = append(mounts, caMount)
	}
	return volumes, mounts
}

// configVolumeAndMount projects the rendered config ConfigMap read-only as the
// whole neutronConfigMountPath directory, shadowing the image's own
// /etc/neutron.
func configVolumeAndMount(configMapName string) (corev1.Volume, corev1.VolumeMount) {
	volume := corev1.Volume{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      configVolumeName,
		MountPath: neutronConfigMountPath,
		ReadOnly:  true,
	}
	return volume, mount
}

// ovnTLSVolumeAndMount projects the mirrored OVN client Secret at
// ovnTLSMountPath. Its three keys are the files the [ovn] section of
// ml2_conf.ini names: the keypair the mechanism driver identifies itself with
// and the CA bundle it verifies both databases against.
func ovnTLSVolumeAndMount(neutron *neutronv1alpha1.Neutron) (corev1.Volume, corev1.VolumeMount) {
	volume := corev1.Volume{
		Name: ovnTLSVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  ovnClientSecretName(neutron),
				DefaultMode: ptr.To(int32(0o400)),
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      ovnTLSVolumeName,
		MountPath: ovnTLSMountPath,
		ReadOnly:  true,
	}
	return volume, mount
}

// stateVolumeAndMount backs [DEFAULT] state_path with an emptyDir. The container
// root filesystem is read-only, and neutron needs a writable directory for its
// oslo.concurrency lock files, so the path the rendered config names has to be a
// volume. Its contents are per-pod scratch: the logical model lives in the OVN
// databases, so nothing here survives a restart and nothing needs to.
func stateVolumeAndMount() (corev1.Volume, corev1.VolumeMount) {
	volume := corev1.Volume{
		Name:         stateVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	mount := corev1.VolumeMount{
		Name:      stateVolumeName,
		MountPath: neutronStatePath,
	}
	return volume, mount
}

// neutronDBTLSEnabled reports whether the Neutron CR requests TLS to the
// database; the helper centralises the nil/disabled gate so the workloads and
// the migration Jobs decide identically.
func neutronDBTLSEnabled(neutron *neutronv1alpha1.Neutron) bool {
	return neutron.Spec.Database.TLS.IsEnabled()
}

// neutronDBTLSVolumeAndMount builds the Volume + VolumeMount pair projecting the
// client TLS material (ca.crt from caBundleSecretRef; tls.crt + tls.key from
// clientCertSecretRef) into a Neutron pod, merged onto dbTLSMountPath via a
// projected volume so the ssl_ca/ssl_cert/ssl_key DSN paths derived from that
// mount point stay a single source of truth. Callers must only invoke it when
// neutronDBTLSEnabled(neutron) is true. DefaultMode 0o400 lets the openstack UID
// read the material while group and world have no access.
func neutronDBTLSVolumeAndMount(neutron *neutronv1alpha1.Neutron) (corev1.Volume, corev1.VolumeMount) {
	tlsSpec := neutron.Spec.Database.TLS
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

// rabbitmqCAVolumeAndMount projects the broker's CA bundle at
// rabbitmqCAMountPath under the fixed file name rabbitmqCAFilePath ends in,
// which is what [oslo_messaging_rabbit] ssl_ca_file points at. The projection
// renames whichever key the CR names to that file, so a bundle stored under any
// key lands where the rendered config expects it. Callers must only invoke it
// when spec.messaging.tls is set.
func rabbitmqCAVolumeAndMount(neutron *neutronv1alpha1.Neutron) (corev1.Volume, corev1.VolumeMount) {
	ref := neutron.Spec.Messaging.TLS.CABundleSecretRef
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

// buildNeutronService builds the Neutron API Service on the API port, selecting
// the API component so no worker or Job pod can become an endpoint of it.
func buildNeutronService(neutron *neutronv1alpha1.Neutron) *corev1.Service {
	return deployment.BuildService(neutron.Namespace, neutron.Name, commonLabels(neutron),
		apiSelectorLabels(neutron), neutronAPIPort, neutronAPIPort)
}

// buildPodDisruptionBudget constructs the desired PDB for the Neutron API
// Deployment, delegating to the shared builder (minAvailable=1 for
// multi-replica, maxUnavailable=1 for single-replica to avoid drain deadlock).
//
// The selector is the API component's, plus the absence of the Job name label.
// The component key keeps the worker pods out of the budget, which protects the
// API pods alone; without it a drain could take every API replica at once while
// the budget still counted enough healthy worker pods. The Job-name requirement
// keeps the migration and ovn-db-sync pods out for the same reason: they carry
// no readiness probe, so they are Ready from their first moment and would raise
// disruptionsAllowed.
func buildPodDisruptionBudget(neutron *neutronv1alpha1.Neutron) *policyv1.PodDisruptionBudget {
	pdb := deployment.BuildPDB(neutron.Namespace, neutron.Name, commonLabels(neutron),
		apiSelectorLabels(neutron), &neutron.Spec.Deployment)
	pdb.Spec.Selector.MatchExpressions = naming.ExcludeJobPods()
	return pdb
}
