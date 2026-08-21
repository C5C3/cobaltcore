// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
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

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// placementAppName is the app.kubernetes.io/name label value applied to every
// Placement-owned sub-resource. It matches the literal the validating webhook
// uses for its TopologySpreadConstraints selector check, so the two never drift.
const placementAppName = "placement"

// placementAPIPort is the container/Service port the Placement API listens on.
// It is the single source of truth for the container port, the Service port, the
// HTTPRoute backend port, the readiness/liveness probes, and the status URL.
const placementAPIPort int32 = 8778

// placementConfigMountPath is placementConfigDir without its trailing slash: the
// in-pod mount point of the config volume and the value of the
// OS_PLACEMENT_CONFIG_DIR env var the WSGI entry reads. Both are derived from
// the one constant, so the directory the ConfigMap lands in and the directory
// placement looks for placement.conf in cannot drift apart.
var placementConfigMountPath = strings.TrimSuffix(placementConfigDir, "/")

// Pod volume names.
const (
	// configVolumeName backs the rendered config ConfigMap, mounted read-only as
	// the whole /etc/placement directory.
	configVolumeName = "config"
	// dbTLSVolumeName backs the projected db-tls client keypair volume.
	dbTLSVolumeName = "db-tls"
)

// placementWSGIScriptPath is the uWSGI entry script shipped by images/placement.
// The image generates it by hand under the upstream name because 2025.2 declares
// it as a PBR wsgi_scripts entry the install mode does not materialise and
// 2026.1 declares no WSGI script at all; the two paths must stay in lockstep
// with the image.
const placementWSGIScriptPath = "/var/lib/openstack/bin/placement-api"

// Pod-template annotation keys stamped with content digests so an env-var-
// consumed credential change rolls the Deployment (the value is not
// volume-mounted, so it only takes effect on a Pod restart). Both follow the
// placement.c5c3.io/<x>-hash annotation-key style of the sibling operators.
const (
	// dbConnectionHashAnnotation carries the SHA-256 of the assembled DSN so a
	// rotated (e.g. Dynamic engine-issued) database credential rolls the pods.
	// The DSN is consumed via OS_PLACEMENT_DATABASE__CONNECTION, not a mounted
	// volume.
	dbConnectionHashAnnotation = "placement.c5c3.io/db-connection-hash"
	// authTokenHashAnnotation carries the SHA-256 of the service-user password so
	// a rotation at the OpenBao source rolls the pods. The password is consumed
	// via OS_KEYSTONE_AUTHTOKEN__PASSWORD, not a mounted volume.
	authTokenHashAnnotation = "placement.c5c3.io/authtoken-hash"
)

// Condition reason constants for DeploymentReady.
const (
	conditionReasonDeploymentReady      = "DeploymentReady"
	conditionReasonWaitingForDeployment = "WaitingForDeployment"
)

// subResourceName returns the canonical name for Placement operator-managed
// sub-resources (Deployment, Service, PodDisruptionBudget, HPA, NetworkPolicy,
// HTTPRoute). Centralised here so the naming convention is defined in one
// place: the bare CR name with no suffix.
func subResourceName(placement *placementv1alpha1.Placement) string {
	return placement.Name
}

// commonLabels returns the standard Kubernetes labels applied to all resources
// owned by this Placement instance, delegating to the shared naming package.
func commonLabels(placement *placementv1alpha1.Placement) map[string]string {
	return naming.CommonLabels(placementAppName, placement.Name)
}

// selectorLabels returns the minimal label set used as the Deployment pod
// selector. It is a subset of commonLabels and must remain stable for the
// lifetime of a Deployment (selectors are immutable after creation).
func selectorLabels(placement *placementv1alpha1.Placement) map[string]string {
	return naming.SelectorLabels(placementAppName, placement.Name)
}

// componentLabels returns the pod-template labels for a workload of the given
// component. It is commonLabels plus app.kubernetes.io/component, so the result
// stays a superset of selectorLabels and keeps matching the Deployment pod
// selector. It delegates to the shared naming package to stay in sync across
// operators.
func componentLabels(placement *placementv1alpha1.Placement, component string) map[string]string {
	return naming.ComponentLabels(placementAppName, placement.Name, component)
}

// reconcileDeployment ensures the Placement API Deployment, Service, and PDB
// exist with the correct spec. It sets the DeploymentReady condition and stamps
// the status endpoint when the Deployment becomes available.
//
// configMapName names the rendered config ConfigMap the pods mount. The config
// step short-circuits the pipeline on every path that produces no artefact, so
// this step only ever runs with a name.
//
// dsnDigest and authtokenDigest are stamped into pod-template annotations so a
// rotated database credential or service-user password rolls the pods; both are
// env-var-consumed, so they only take effect on a Pod restart. Each annotation
// is omitted when its digest is empty (the requeue/error paths upstream).
func (r *PlacementReconciler) reconcileDeployment(ctx context.Context, children client.Client, placement *placementv1alpha1.Placement, configMapName, dsnDigest, authtokenDigest string) (ctrl.Result, error) {
	deploy := buildPlacementDeployment(placement, configMapName, dsnDigest, authtokenDigest)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, placement, deploy)
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
	// maintenance pods as endpoints for exactly as long as it lasts, reopening
	// the race this narrowing closes. The latch reads the live Service
	// through the uncached APIReader of whichever cluster holds it, so it cannot
	// be decided from a cache that still predates the narrowing write (see
	// deployment.APISelectorNarrowed). A CR that names a target cluster latches
	// off that cluster's own API reader: its cache lags after a relist or a
	// fresh write exactly like the local one, and a latch decided from it would
	// re-widen the selector. The reader is nil only in unit tests, whose fake
	// client is read-your-writes anyway.
	narrowSelector := deployment.TemplateConverged(deploy)
	if !narrowSelector {
		reader, err := commonmulticluster.ResolveChildrenAPIReader(
			ctx, r.Resolver, r.apiReader, placement.Spec.TargetClusterRef)
		if err != nil {
			return ctrl.Result{}, err
		}
		if reader == nil {
			// No manager behind this reconciler (unit tests): the fake client
			// is read-your-writes, so it is a sound latch source.
			reader = children
		}
		narrowSelector, err = deployment.APISelectorNarrowed(ctx, reader, placement.Namespace, subResourceName(placement))
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	svc := buildPlacementService(placement, narrowSelector)
	if err := deployment.EnsureService(ctx, children, r.Scheme, placement, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Service: %w", err)
	}

	pdb := buildPodDisruptionBudget(placement)
	if err := deployment.EnsurePDB(ctx, children, r.Scheme, placement, pdb); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring PodDisruptionBudget: %w", err)
	}

	if !ready {
		log.FromContext(ctx).Info("Placement API deployment not ready, requeuing")
		conditions.SetCondition(&placement.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: placement.Generation,
			Reason:             conditionReasonWaitingForDeployment,
			Message:            "Placement API deployment is not yet available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	// Status.Endpoint derivation is delegated to placementStatusEndpoint so the
	// gateway-aware public URL is used when spec.gateway is set, and the
	// cluster-local URL otherwise.
	placement.Status.Endpoint = placementStatusEndpoint(placement)
	conditions.SetCondition(&placement.Status.Conditions, metav1.Condition{
		Type:               "DeploymentReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: placement.Generation,
		Reason:             conditionReasonDeploymentReady,
		Message:            "Placement API deployment is available",
	})
	return ctrl.Result{}, nil
}

// buildPlacementDeployment constructs the desired Placement API Deployment. The
// rendered config ConfigMap mounts read-only as the whole
// placementConfigMountPath directory, shadowing the image's own /etc/placement,
// and the db-tls client keypair is projected only when database TLS is enabled.
// The database URL and the service-user password are injected via env vars so no
// credential material enters the ConfigMap.
func buildPlacementDeployment(placement *placementv1alpha1.Placement, configMapName, dsnDigest, authtokenDigest string) *appsv1.Deployment {
	deploy := deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      placement.Namespace,
		Name:           subResourceName(placement),
		Labels:         componentLabels(placement, naming.ComponentAPI),
		SelectorLabels: selectorLabels(placement),
		PodAnnotations: placementPodAnnotations(dsnDigest, authtokenDigest),
		Deployment:     &placement.Spec.Deployment,
		Autoscaling:    placement.Spec.Autoscaling,
		Container: deployment.ContainerParams{
			Name:    "placement-api",
			Image:   placement.Spec.Image.Reference(),
			Command: placementUWSGICommand(placement.Spec.APIServer),
			Env: []corev1.EnvVar{
				// The WSGI entry's init_application() resolves its config as
				// $OS_PLACEMENT_CONFIG_DIR/placement.conf, so this is what points
				// placement at the mounted ConfigMap.
				{Name: "OS_PLACEMENT_CONFIG_DIR", Value: placementConfigMountPath},
				database.ConnectionEnvVarForSection(placement.Name, "placement_database"),
				keystoneauth.PasswordEnvVar(placement.Spec.ServiceUser.SecretRef.Name, effectiveServiceUserKey(placement)),
			},
			Ports: []corev1.ContainerPort{{
				Name:          "placement-api",
				ContainerPort: placementAPIPort,
			}},
			// Readiness AND liveness hit "/", the version document placement serves
			// without authentication and without touching the database. Placement
			// has no startup probe: the readiness probe's own delay covers the WSGI
			// app coming up.
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        placementRootProbeHandler(),
				InitialDelaySeconds: 15,
				PeriodSeconds:       20,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        placementRootProbeHandler(),
				InitialDelaySeconds: 10,
				PeriodSeconds:       15,
				TimeoutSeconds:      10,
				FailureThreshold:    3,
			},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      configVolumeName,
				MountPath: placementConfigMountPath,
				ReadOnly:  true,
			}},
		},
		Volumes: []corev1.Volume{{
			Name: configVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName,
					},
				},
			},
		}},
	})

	// Project the db-tls client keypair into the API pod when database TLS is
	// enabled; the gate is centralised in placementDBTLSEnabled so the deployment
	// and the db-sync Job builder decide identically.
	if placementDBTLSEnabled(placement) {
		tlsVol, tlsMount := placementDBTLSVolumeAndMount(placement)
		deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, tlsVol)
		deploy.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			deploy.Spec.Template.Spec.Containers[0].VolumeMounts, tlsMount,
		)
	}
	return deploy
}

// placementPodAnnotations assembles the pod-template annotations, stamping each
// content digest only when non-empty so the requeue/error paths (which return an
// empty digest) leave the annotation off and cause no spurious rollout. Returns
// nil when both digests are empty so the pod template carries no annotations.
func placementPodAnnotations(dsnDigest, authtokenDigest string) map[string]string {
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

// placementRootProbeHandler returns the shared readiness/liveness probe handler:
// an HTTP GET of "/" on the API port.
func placementRootProbeHandler() corev1.ProbeHandler {
	return corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{
			Path: "/",
			Port: intstr.FromInt32(placementAPIPort),
		},
	}
}

// placementUWSGICommand constructs the uWSGI container command for the
// Placement API container from the given apiServer spec. Token emission and
// default resolution are owned by deployment.BuildUWSGICommand; this function
// only assembles the placement parameters.
//
// There is deliberately no --pyargv. The hand-shipped WSGI entry
// (placementWSGIScriptPath) calls init_application() with no arguments and
// placement's own _get_config_files never reads sys.argv, so a --pyargv value
// would be parsed by nothing and silently ignored. The config location travels
// in the OS_PLACEMENT_CONFIG_DIR env var instead.
func placementUWSGICommand(apiServer *placementv1alpha1.APIServerSpec) []string {
	var uwsgi *placementv1alpha1.UWSGISpec
	if apiServer != nil {
		uwsgi = apiServer.UWSGI
	}

	return deployment.BuildUWSGICommand(deployment.UWSGICommandParams{
		UWSGI:        uwsgi,
		Bind:         fmt.Sprintf(":%d", placementAPIPort),
		WSGIFilePath: placementWSGIScriptPath,
	})
}

// placementDBTLSEnabled reports whether the Placement CR requests TLS to the
// database; the helper centralises the nil/disabled gate so the deployment and
// db-sync Job builders decide identically.
func placementDBTLSEnabled(placement *placementv1alpha1.Placement) bool {
	return placement.Spec.Database.TLS.IsEnabled()
}

// placementDBTLSVolumeAndMount builds the Volume + VolumeMount pair projecting
// the client TLS material (ca.crt from caBundleSecretRef; tls.crt + tls.key from
// clientCertSecretRef) into the Placement API pod, merged onto dbTLSMountPath
// via a projected volume so the ssl_ca/ssl_cert/ssl_key DSN paths derived from
// that mount point stay a single source of truth. Callers must only invoke it
// when placementDBTLSEnabled(placement) is true. DefaultMode 0o400 lets the
// openstack UID read the material while group/world have no access.
func placementDBTLSVolumeAndMount(placement *placementv1alpha1.Placement) (corev1.Volume, corev1.VolumeMount) {
	tlsSpec := placement.Spec.Database.TLS
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

// buildPodDisruptionBudget constructs the desired PDB for the Placement API
// deployment, delegating to the shared builder (minAvailable=1 for
// multi-replica, maxUnavailable=1 for single-replica to avoid drain deadlock).
//
// The selector keeps the stable name and instance labels and excludes the
// db-sync pods by the absence of the Job name label (naming.ExcludeJobPods)
// rather than by the API component. Both keep Job pods from inflating
// currentHealthy: the disruption controller counts every matching pod, and Job
// pods carry no readiness probe, so they are Ready from their first moment,
// which raises disruptionsAllowed and lets a drain evict the API pods the budget
// exists to protect. Only this one also covers the API pods of a template
// predating the component label: a pod no budget selects is not protected at
// all, and the Service's two-phase narrowing has no counterpart here that would
// bound that gap.
func buildPodDisruptionBudget(placement *placementv1alpha1.Placement) *policyv1.PodDisruptionBudget {
	pdb := deployment.BuildPDB(placement.Namespace, subResourceName(placement), commonLabels(placement), selectorLabels(placement), &placement.Spec.Deployment)
	pdb.Spec.Selector.MatchExpressions = naming.ExcludeJobPods()
	return pdb
}

// buildPlacementService builds the Placement API Service on the API port. With
// narrowSelector the selector narrows to the API component, so only the API pods
// can become endpoints (see apiSelector for when that is safe).
func buildPlacementService(placement *placementv1alpha1.Placement, narrowSelector bool) *corev1.Service {
	return deployment.BuildService(placement.Namespace, subResourceName(placement), commonLabels(placement), apiSelector(placement, narrowSelector), placementAPIPort, placementAPIPort)
}

// apiSelector returns the selector of the API Service. It narrows to
// app.kubernetes.io/component=api only when narrowSelector is set, which the
// caller derives from deployment.TemplateConverged and then latches: until every
// live pod runs the labelled template, the wide selector is the only one that
// still matches them.
func apiSelector(placement *placementv1alpha1.Placement, narrowSelector bool) map[string]string {
	if narrowSelector {
		return naming.APISelectorLabels(placementAppName, placement.Name)
	}
	return selectorLabels(placement)
}
