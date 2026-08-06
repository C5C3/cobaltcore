// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"path"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/deployment"
	"github.com/c5c3/forge/internal/common/keystoneauth"
	"github.com/c5c3/forge/internal/common/naming"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
)

// barbicanAppName is the app.kubernetes.io/name label value applied to every
// Barbican-owned sub-resource. It matches the literal the validating webhook
// uses for its TopologySpreadConstraints selector check, so the two never drift.
const barbicanAppName = "barbican"

// barbicanConfigMountPath is barbicanConfigDir without its trailing slash: the
// in-pod mount point of the config volume, and the directory oslo.config finds
// barbican.conf and barbican-api-paste.ini in. Both are derived from the one
// constant, so the directory the Secret lands in and the directory barbican
// searches cannot drift apart.
var barbicanConfigMountPath = strings.TrimSuffix(barbicanConfigDir, "/")

// secretStoreCAMountPath is the directory the secret store's CA bundle is
// projected into. It is derived from secretStoreCAFilePath — the value rendered
// as [vault_plugin] ssl_ca_crt_file — so the mount point and the option the
// plugin opens can only be changed together.
var secretStoreCAMountPath = path.Dir(secretStoreCAFilePath)

// barbicanWSGIModule is the uWSGI --module value: the import path of the WSGI
// application object images/barbican ships. Both supported releases install
// barbican/wsgi/api.py with a module-level `application` and no entry script (the
// PBR wsgi_scripts entry is not materialised by the install mode), so the module
// path is the only launch mode available. It has to stay in lockstep with
// images/barbican/Dockerfile, which records the same path.
const barbicanWSGIModule = "barbican.wsgi.api:application"

// barbicanHealthcheckPath is the oslo_middleware healthcheck app the paste
// composite routes outside the authtoken pipeline. It answers without a token
// and without touching the database, so it is both the container probe target
// and the operator's own health check.
const barbicanHealthcheckPath = "/healthcheck"

// Pod volume names.
const (
	// dbTLSVolumeName backs the projected db-tls client keypair volume.
	dbTLSVolumeName = "db-tls"
	// secretStoreCAVolumeName backs the secret store's CA bundle volume.
	secretStoreCAVolumeName = "secret-store-ca"
)

// Pod-template annotation keys stamped with content digests so an env-var-
// consumed credential change rolls the Deployment (the value is not
// volume-mounted, so it only takes effect on a Pod restart). All three follow
// the barbican.c5c3.io/<x>-hash annotation-key style of the sibling operators.
const (
	// dbConnectionHashAnnotation carries the SHA-256 of the assembled DSN so a
	// rotated (e.g. Dynamic engine-issued) database credential rolls the pods.
	// The DSN is consumed via OS_DATABASE__CONNECTION, not a mounted volume.
	dbConnectionHashAnnotation = "barbican.c5c3.io/db-connection-hash"
	// authTokenHashAnnotation carries the SHA-256 of the service-user password so
	// a rotation at the OpenBao source rolls the pods. The password is consumed
	// via OS_KEYSTONE_AUTHTOKEN__PASSWORD, not a mounted volume.
	// #nosec G101 -- annotation key naming a digest, not a credential.
	authTokenHashAnnotation = "barbican.c5c3.io/authtoken-hash"
	// secretStoreCredentialsHashAnnotation carries the SHA-256 of the AppRole
	// secret ID. A re-mint by the BarbicanSecretStore controller changes the
	// digest and rolls the pods, which is what makes the new secret ID take
	// effect: it is consumed via OS_VAULT_PLUGIN__APPROLE_SECRET_ID, so a running
	// pod keeps authenticating with the value it started with.
	// #nosec G101 -- annotation key naming a digest, not a credential.
	secretStoreCredentialsHashAnnotation = "barbican.c5c3.io/secret-store-credentials-hash"
)

// secretIDEnvVarName is the oslo.config override that delivers the AppRole
// secret ID to the vault plugin. The secret ID deliberately never enters the
// rendered config: barbican.conf is mounted into every API pod and read back by
// the store controller, while this env var keeps the only copy in the Secret the
// store itself owns.
// #nosec G101 -- env var name, not a credential.
const secretIDEnvVarName = "OS_VAULT_PLUGIN__APPROLE_SECRET_ID"

// Condition reason constants for DeploymentReady.
const (
	conditionReasonDeploymentReady      = "DeploymentReady"
	conditionReasonWaitingForDeployment = "WaitingForDeployment"
)

// commonLabels returns the standard Kubernetes labels applied to all resources
// owned by this Barbican instance, delegating to the shared naming package.
func commonLabels(barbican *barbicanv1alpha1.Barbican) map[string]string {
	return naming.CommonLabels(barbicanAppName, barbican.Name)
}

// selectorLabels returns the minimal label set used as the Deployment pod
// selector. It is a subset of commonLabels and must remain stable for the
// lifetime of a Deployment (selectors are immutable after creation).
func selectorLabels(barbican *barbicanv1alpha1.Barbican) map[string]string {
	return naming.SelectorLabels(barbicanAppName, barbican.Name)
}

// componentLabels returns the pod-template labels for a workload of the given
// component. It is commonLabels plus app.kubernetes.io/component, so the result
// stays a superset of selectorLabels and keeps matching the Deployment pod
// selector. It delegates to the shared naming package to stay in sync across
// operators.
func componentLabels(barbican *barbicanv1alpha1.Barbican, component string) map[string]string {
	return naming.ComponentLabels(barbicanAppName, barbican.Name, component)
}

// reconcileDeployment ensures the Barbican API Deployment, Service, and PDB
// exist with the correct spec. It sets the DeploymentReady condition and stamps
// the status endpoint when the Deployment becomes available.
//
// projection is the secret-store aggregation. While it is invalid no workload is
// created at all: barbican resolves its secret store at process start and exits
// when none is configured, so a Deployment built without one would crash-loop
// rather than serve degraded. The step therefore reports
// DeploymentReady=False/WaitingForSecretStores and returns neither an error nor
// a requeue — a store flipping to ready re-enqueues this Barbican through the
// BarbicanSecretStore watch.
//
// configSecretName names the rendered config Secret the pods mount. dsnDigest,
// authtokenDigest, and the projection's secret-ID digest are stamped into
// pod-template annotations so a rotated credential rolls the pods; all three are
// env-var-consumed, so they only take effect on a Pod restart. Each annotation
// is omitted when its digest is empty (the requeue/error paths upstream).
func (r *BarbicanReconciler) reconcileDeployment(
	ctx context.Context, barbican *barbicanv1alpha1.Barbican,
	projection secretStoreProjection, configSecretName, dsnDigest, authtokenDigest string,
) (ctrl.Result, error) {
	if !projection.valid {
		conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: barbican.Generation,
			Reason:             conditionReasonWaitingForSecretStores,
			Message:            "Waiting for a credential-ready default secret store before creating the Barbican API Deployment",
		})
		return ctrl.Result{}, nil
	}

	deploy := buildBarbicanDeployment(barbican, projection, configSecretName, dsnDigest, authtokenDigest)
	ready, err := deployment.EnsureDeployment(ctx, r.Client, r.Scheme, barbican, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Deployment: %w", err)
	}

	// Narrow the Service selector to the API component only once every live pod
	// carries that label. EnsureDeployment is a Server-Side Apply: it returns as
	// soon as the API server accepts the pod template, long before the rollout
	// finishes. Applying the narrowed selector in the same pass would drop every
	// pod still running from before the upgrade out of the EndpointSlices while
	// it is still the only thing serving, and a rollout that never completes
	// would never repool them.
	//
	// Once narrowed, stay narrowed. The migration is one-way: only a template
	// built by an operator predating the component label produces pods the
	// narrow selector misses. Re-widening on a later rollout would re-admit
	// db-clean pods as endpoints for exactly as long as it lasts, reopening the
	// race this narrowing closes. The latch reads the live Service through the
	// uncached APIReader, so it cannot be decided from a cache that still
	// predates the narrowing write (see deployment.APISelectorNarrowed);
	// r.apiReader is nil only in unit tests, whose fake client is
	// read-your-writes anyway.
	narrowSelector := deployment.TemplateConverged(deploy)
	if !narrowSelector {
		reader := client.Reader(r.Client)
		if r.apiReader != nil {
			reader = r.apiReader
		}
		narrowSelector, err = deployment.APISelectorNarrowed(ctx, reader, barbican.Namespace, subResourceName(barbican))
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	svc := buildBarbicanService(barbican, narrowSelector)
	if err := deployment.EnsureService(ctx, r.Client, r.Scheme, barbican, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Service: %w", err)
	}

	pdb := buildPodDisruptionBudget(barbican)
	if err := deployment.EnsurePDB(ctx, r.Client, r.Scheme, barbican, pdb); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring PodDisruptionBudget: %w", err)
	}

	if !ready {
		log.FromContext(ctx).Info("Barbican API deployment not ready, requeuing")
		conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: barbican.Generation,
			Reason:             conditionReasonWaitingForDeployment,
			Message:            "Barbican API deployment is not yet available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	// Status.Endpoint is the same URL the rendered [DEFAULT] host_href carries,
	// so what barbican advertises in its API links and what the CR reports are
	// one value: the gateway-aware public URL when spec.gateway is set, the
	// cluster-local Service URL otherwise.
	barbican.Status.Endpoint = barbicanPublicEndpoint(barbican)
	conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
		Type:               "DeploymentReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: barbican.Generation,
		Reason:             conditionReasonDeploymentReady,
		Message:            "Barbican API deployment is available",
	})
	return ctrl.Result{}, nil
}

// buildBarbicanDeployment constructs the desired Barbican API Deployment. The
// rendered config Secret mounts read-only as the whole barbicanConfigMountPath
// directory, shadowing the image's own /etc/barbican, and both the secret
// store's CA bundle and the db-tls client keypair are projected only when their
// inputs exist. The database URL, the service-user password, and the AppRole
// secret ID are injected via env vars so no credential material beyond the role
// ID enters the config document.
func buildBarbicanDeployment(
	barbican *barbicanv1alpha1.Barbican, projection secretStoreProjection,
	configSecretName, dsnDigest, authtokenDigest string,
) *appsv1.Deployment {
	deploy := deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      barbican.Namespace,
		Name:           subResourceName(barbican),
		Labels:         componentLabels(barbican, naming.ComponentAPI),
		SelectorLabels: selectorLabels(barbican),
		PodAnnotations: barbicanPodAnnotations(dsnDigest, authtokenDigest, projection.secretIDDigest),
		Deployment:     &barbican.Spec.Deployment,
		Autoscaling:    barbican.Spec.Autoscaling,
		Container: deployment.ContainerParams{
			Name:    "barbican-api",
			Image:   barbican.Spec.Image.Reference(),
			Command: barbicanUWSGICommand(barbican.Spec.APIServer),
			Env: []corev1.EnvVar{
				database.ConnectionEnvVar(barbican.Name),
				keystoneauth.PasswordEnvVar(barbican.Spec.ServiceUser.SecretRef.Name, effectiveServiceUserKey(barbican)),
				secretStoreSecretIDEnvVar(projection),
			},
			Ports: []corev1.ContainerPort{{
				Name:          "barbican-api",
				ContainerPort: barbicanAPIPort,
			}},
			// Readiness AND liveness hit the healthcheck app, which the paste
			// composite routes outside the authtoken pipeline, so the probes need no
			// token and touch no database. Barbican has no startup probe: the
			// readiness probe's own delay covers the WSGI app coming up.
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        barbicanHealthcheckProbeHandler(),
				InitialDelaySeconds: 15,
				PeriodSeconds:       20,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        barbicanHealthcheckProbeHandler(),
				InitialDelaySeconds: 10,
				PeriodSeconds:       15,
				TimeoutSeconds:      10,
				FailureThreshold:    3,
			},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      configVolumeName,
				MountPath: barbicanConfigMountPath,
				ReadOnly:  true,
			}},
		},
		Volumes: []corev1.Volume{{
			Name: configVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: configSecretName,
				},
			},
		}},
	})

	// Project the trust bundle authenticating the OpenBao server when the store
	// carries one. Without a bundle the pods verify the server against their
	// system trust store, and the rendered config then omits ssl_ca_crt_file, so
	// the mount would point at nothing.
	if projection.caSecretName != "" {
		caVol, caMount := secretStoreCAVolumeAndMount(projection)
		deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, caVol)
		deploy.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			deploy.Spec.Template.Spec.Containers[0].VolumeMounts, caMount,
		)
	}

	// Project the db-tls client keypair into the API pod when database TLS is
	// enabled; the gate is centralised in barbicanDBTLSEnabled so the deployment
	// and the db-sync Job builder decide identically.
	if barbicanDBTLSEnabled(barbican) {
		tlsVol, tlsMount := barbicanDBTLSVolumeAndMount(barbican)
		deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, tlsVol)
		deploy.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			deploy.Spec.Template.Spec.Containers[0].VolumeMounts, tlsMount,
		)
	}
	return deploy
}

// secretStoreSecretIDEnvVar sources the AppRole secret ID from the Secret the
// store owns. Callers only reach it with a valid projection, which always names
// a credentials Secret.
func secretStoreSecretIDEnvVar(projection secretStoreProjection) corev1.EnvVar {
	return corev1.EnvVar{
		Name: secretIDEnvVarName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: projection.credentialsSecretName,
				},
				Key: barbicanv1alpha1.OpenBaoSecretIDKey,
			},
		},
	}
}

// secretStoreCAVolumeAndMount builds the Volume + VolumeMount pair projecting
// the store's CA bundle into the API pod. Both modes read the bundle under the
// fixed contract key barbicanv1alpha1.OpenBaoCAKey — SecretNameRefSpec carries no
// key field, so a store cannot name its own — and that key is also the file name
// secretStoreCAFilePath ends in, which is where the rendered ssl_ca_crt_file
// points. DefaultMode 0o444 keeps the bundle world-readable — it is a public
// certificate — while nothing can write it.
func secretStoreCAVolumeAndMount(projection secretStoreProjection) (corev1.Volume, corev1.VolumeMount) {
	volume := corev1.Volume{
		Name: secretStoreCAVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  projection.caSecretName,
				DefaultMode: ptr.To(int32(0o444)),
				Items: []corev1.KeyToPath{
					{Key: barbicanv1alpha1.OpenBaoCAKey, Path: path.Base(secretStoreCAFilePath)},
				},
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      secretStoreCAVolumeName,
		MountPath: secretStoreCAMountPath,
		ReadOnly:  true,
	}
	return volume, mount
}

// barbicanPodAnnotations assembles the pod-template annotations, stamping each
// content digest only when non-empty so the requeue/error paths (which return an
// empty digest) leave the annotation off and cause no spurious rollout. Returns
// nil when every digest is empty so the pod template carries no annotations.
func barbicanPodAnnotations(dsnDigest, authtokenDigest, secretIDDigest string) map[string]string {
	annotations := map[string]string{}
	if dsnDigest != "" {
		annotations[dbConnectionHashAnnotation] = dsnDigest
	}
	if authtokenDigest != "" {
		annotations[authTokenHashAnnotation] = authtokenDigest
	}
	if secretIDDigest != "" {
		annotations[secretStoreCredentialsHashAnnotation] = secretIDDigest
	}
	if len(annotations) == 0 {
		return nil
	}
	return annotations
}

// barbicanHealthcheckProbeHandler returns the shared readiness/liveness probe
// handler: an HTTP GET of the healthcheck app on the API port.
func barbicanHealthcheckProbeHandler() corev1.ProbeHandler {
	return corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{
			Path: barbicanHealthcheckPath,
			Port: intstr.FromInt32(barbicanAPIPort),
		},
	}
}

// barbicanUWSGICommand constructs the uWSGI container command for the Barbican
// API container from the given apiServer spec. Token emission and default
// resolution are owned by deployment.BuildUWSGICommand; this function only
// assembles the barbican parameters.
//
// The launch mode is --module rather than --wsgi-file: images/barbican ships no
// entry script, so the WSGI application is imported from barbicanWSGIModule.
// There is deliberately no --pyargv either — oslo.config resolves barbican.conf
// from its fixed search path, which barbicanConfigMountPath is on.
func barbicanUWSGICommand(apiServer *barbicanv1alpha1.APIServerSpec) []string {
	var uwsgi *barbicanv1alpha1.UWSGISpec
	if apiServer != nil {
		uwsgi = apiServer.UWSGI
	}

	return deployment.BuildUWSGICommand(deployment.UWSGICommandParams{
		UWSGI:  uwsgi,
		Bind:   fmt.Sprintf(":%d", barbicanAPIPort),
		Module: barbicanWSGIModule,
	})
}

// barbicanDBTLSEnabled reports whether the Barbican CR requests TLS to the
// database; the helper centralises the nil/disabled gate so the deployment, the
// db-sync Job, and the db-clean CronJob builders decide identically.
func barbicanDBTLSEnabled(barbican *barbicanv1alpha1.Barbican) bool {
	return barbican.Spec.Database.TLS.IsEnabled()
}

// barbicanDBTLSVolumeAndMount builds the Volume + VolumeMount pair projecting
// the client TLS material (ca.crt from caBundleSecretRef; tls.crt + tls.key from
// clientCertSecretRef) into the Barbican API pod, merged onto dbTLSMountPath via
// a projected volume so the ssl_ca/ssl_cert/ssl_key DSN paths derived from that
// mount point stay a single source of truth. Callers must only invoke it when
// barbicanDBTLSEnabled(barbican) is true. DefaultMode 0o400 lets the openstack
// UID read the material while group/world have no access.
func barbicanDBTLSVolumeAndMount(barbican *barbicanv1alpha1.Barbican) (corev1.Volume, corev1.VolumeMount) {
	tlsSpec := barbican.Spec.Database.TLS
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

// buildPodDisruptionBudget constructs the desired PDB for the Barbican API
// deployment, delegating to the shared builder (minAvailable=1 for
// multi-replica, maxUnavailable=1 for single-replica to avoid drain deadlock).
//
// The selector keeps the stable name and instance labels and excludes the
// db-sync and db-clean pods by the absence of the Job name label
// (naming.ExcludeJobPods) rather than by the API component. Both keep Job pods
// from inflating currentHealthy: the disruption controller counts every matching
// pod, and Job pods carry no readiness probe, so they are Ready from their first
// moment, which raises disruptionsAllowed and lets a drain evict the API pods the
// budget exists to protect. Only this one also covers the API pods of a template
// predating the component label: a pod no budget selects is not protected at all,
// and the Service's two-phase narrowing has no counterpart here that would bound
// that gap.
func buildPodDisruptionBudget(barbican *barbicanv1alpha1.Barbican) *policyv1.PodDisruptionBudget {
	pdb := deployment.BuildPDB(barbican.Namespace, subResourceName(barbican), commonLabels(barbican), selectorLabels(barbican), &barbican.Spec.Deployment)
	pdb.Spec.Selector.MatchExpressions = naming.ExcludeJobPods()
	return pdb
}

// buildBarbicanService builds the Barbican API Service on the API port. With
// narrowSelector the selector narrows to the API component, so only the API pods
// can become endpoints (see apiSelector for when that is safe).
func buildBarbicanService(barbican *barbicanv1alpha1.Barbican, narrowSelector bool) *corev1.Service {
	return deployment.BuildService(barbican.Namespace, subResourceName(barbican), commonLabels(barbican), apiSelector(barbican, narrowSelector), barbicanAPIPort, barbicanAPIPort)
}

// apiSelector returns the selector of the API Service. It narrows to
// app.kubernetes.io/component=api only when narrowSelector is set, which the
// caller derives from deployment.TemplateConverged and then latches: until every
// live pod runs the labelled template, the wide selector is the only one that
// still matches them.
func apiSelector(barbican *barbicanv1alpha1.Barbican, narrowSelector bool) map[string]string {
	if narrowSelector {
		return naming.APISelectorLabels(barbicanAppName, barbican.Name)
	}
	return selectorLabels(barbican)
}
