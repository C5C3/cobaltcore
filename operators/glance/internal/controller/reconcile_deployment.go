// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
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

// deploymentReplicas returns the desired .spec.replicas for the Glance API
// Deployment. When spec.autoscaling is set, it returns nil so the field is left
// unmanaged and the HorizontalPodAutoscaler owns the replica count.
func deploymentReplicas(glance *glancev1alpha1.Glance) *int32 {
	return deployment.DeploymentReplicas(&glance.Spec.Deployment, glance.Spec.Autoscaling)
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

// buildGlanceDeployment constructs the desired Glance API Deployment. The
// rendered config ConfigMap mounts at glanceConfigDir and the backends Secret at
// glanceBackendsConfigDir (both oslo.config --config-dir roots). Reserved
// staging/tasks stores get emptyDir volumes because import staging and async
// task work always land on local disk regardless of the image store, and the
// db-tls client keypair is projected only when database TLS is enabled. The
// database URL and the service-user password are injected via env vars so no
// credential material enters the ConfigMap.
func buildGlanceDeployment(glance *glancev1alpha1.Glance, art configArtifacts, dsnDigest, authtokenDigest string) *appsv1.Deployment {
	selector := selectorLabels(glance)
	labels := commonLabels(glance)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subResourceName(glance),
			Namespace: glance.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: deploymentReplicas(glance),
			Selector: &metav1.LabelSelector{
				MatchLabels: selector,
			},
			Strategy: deployment.Strategy(&glance.Spec.Deployment),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: glancePodAnnotations(dsnDigest, authtokenDigest),
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(deployment.TerminationGracePeriodSeconds(&glance.Spec.Deployment)),
					TopologySpreadConstraints:     deployment.TopologySpreadConstraints(&glance.Spec.Deployment, selector),
					PriorityClassName:             deployment.PriorityClassName(&glance.Spec.Deployment),
					SecurityContext:               &corev1.PodSecurityContext{FSGroup: ptr.To(deployment.OpenStackUID)},
					Containers: []corev1.Container{{
						Name:            "glance-api",
						Image:           glance.Spec.Image.Reference(),
						Resources:       deployment.ContainerResources(&glance.Spec.Deployment),
						SecurityContext: deployment.RestrictedSecurityContext(),
						Command:         glanceLaunchCommand(glance),
						Env: []corev1.EnvVar{
							database.ConnectionEnvVar(glance.Name),
							keystoneauth.PasswordEnvVar(glance.Spec.ServiceUser.SecretRef.Name, effectiveServiceUserKey(glance)),
						},
						Ports: []corev1.ContainerPort{{
							Name:          "glance-api",
							ContainerPort: glanceAPIPort,
						}},
						// Readiness AND liveness hit the same /healthcheck endpoint
						// (served by the oslo healthcheck middleware without touching
						// the database) on the API port, identical in both launch
						// modes.
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
						Lifecycle: &corev1.Lifecycle{
							PreStop: &corev1.LifecycleHandler{
								Exec: &corev1.ExecAction{
									Command: deployment.PreStopSleepCommand(&glance.Spec.Deployment),
								},
							},
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
					}},
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
							Name:         stagingVolumeName,
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
						{
							Name:         tasksVolumeName,
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
				},
			},
		},
	}

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
	return deploy
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
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subResourceName(glance),
			Namespace: glance.Namespace,
			Labels:    commonLabels(glance),
		},
		Spec: corev1.ServiceSpec{
			Selector: selectorLabels(glance),
			Ports: []corev1.ServicePort{{
				Port:       glanceAPIPort,
				TargetPort: intstr.FromInt32(glanceAPIPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}
