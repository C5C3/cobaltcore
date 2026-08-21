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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
)

// horizonAPIPort is the container/Service port the dashboard listens on.
const horizonAPIPort int32 = 8080

// dashboardLoginPath is the login-page path used by the readiness/startup
// probes and the HealthCheck sub-reconciler. Rendering it exercises Django
// URL routing, template rendering, and the static-asset manifest without
// requiring a live Keystone — Keystone is only contacted on login submit.
const dashboardLoginPath = "/auth/login/"

// probeHostHeader is the fixed Host header the kubelet HTTP readiness and
// startup probes send (see probeHostHeaders). Pinning it — instead of letting
// kubelet default the probe Host to the dynamic pod IP — keeps Django's
// ALLOWED_HOSTS a small closed set (see allowedHosts) rather than forcing a
// "*" wildcard that disables Host-header validation.
const probeHostHeader = "localhost"

// secretKeyHashAnnotation is the pod-template annotation key stamped with the
// SHA-256 of the Django SECRET_KEY so a rotated key rolls the Deployment (the
// key is env-var-consumed, not volume-mounted, so it only takes effect on a
// Pod restart). Follows the keystone.c5c3.io/<x>-hash annotation-key style.
//
//nolint:gosec // G101 false positive: annotation key name, not credential material.
const secretKeyHashAnnotation = "horizon.c5c3.io/secret-key-hash"

// subResourceName returns the canonical name for Horizon operator-managed
// sub-resources (Deployment, HPA, Service, PodDisruptionBudget,
// NetworkPolicy, HTTPRoute). Centralised here so the naming convention is
// defined in one place — the bare CR name with no suffix.
func subResourceName(horizon *horizonv1alpha1.Horizon) string {
	return horizon.Name
}

// commonLabels returns the standard Kubernetes labels applied to all
// resources owned by this Horizon instance, delegating to the shared naming
// package.
func commonLabels(horizon *horizonv1alpha1.Horizon) map[string]string {
	return naming.CommonLabels(horizonv1alpha1.AppName, horizon.Name)
}

// selectorLabels returns the minimal label set used as the Deployment pod
// selector. It is a subset of commonLabels and must remain stable for the
// lifetime of a Deployment (selectors are immutable after creation).
func selectorLabels(horizon *horizonv1alpha1.Horizon) map[string]string {
	return naming.SelectorLabels(horizonv1alpha1.AppName, horizon.Name)
}

// reconcileDeployment ensures the dashboard Deployment, Service, and PDB
// exist with the correct spec. It sets the DeploymentReady condition and the
// status endpoint when the Deployment becomes available.
func (r *HorizonReconciler) reconcileDeployment(ctx context.Context, children client.Client, horizon *horizonv1alpha1.Horizon, configMapName, secretKeyHash string) (ctrl.Result, error) {
	deploy := buildHorizonDeployment(horizon, configMapName, secretKeyHash)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, horizon, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Deployment: %w", err)
	}

	// Narrow the Service selector to the API component only once every live pod
	// carries that label. EnsureDeployment is a Server-Side Apply: it returns as
	// soon as the API server accepts the pod template, long before the rollout
	// finishes. Applying the narrowed selector in the same pass would drop every
	// pod still running from before the upgrade out of the EndpointSlices while
	// it is still the only thing serving — and a rollout that never completes
	// would never repool them.
	//
	// Once narrowed, stay narrowed. The migration is one-way: only a template
	// built by an operator predating the component label produces pods the
	// narrow selector misses. Re-widening on a later rollout would put the
	// Service selector back on a wider set than the dashboard pods for exactly
	// as long as it lasts. The latch reads the live Service
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
			ctx, r.Resolver, r.apiReader, horizon.Spec.TargetClusterRef)
		if err != nil {
			return ctrl.Result{}, err
		}
		if reader == nil {
			// No manager behind this reconciler (unit tests): the fake client
			// is read-your-writes, so it is a sound latch source.
			reader = children
		}
		narrowSelector, err = deployment.APISelectorNarrowed(ctx, reader, horizon.Namespace, subResourceName(horizon))
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	svc := buildHorizonService(horizon, narrowSelector)
	if err := deployment.EnsureService(ctx, children, r.Scheme, horizon, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Service: %w", err)
	}

	pdb := buildPodDisruptionBudget(horizon)
	if err := deployment.EnsurePDB(ctx, children, r.Scheme, horizon, pdb); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring PodDisruptionBudget: %w", err)
	}

	if !ready {
		log.FromContext(ctx).Info("Horizon dashboard deployment not ready, requeuing")
		conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: horizon.Generation,
			Reason:             "WaitingForDeployment",
			Message:            "Horizon dashboard deployment is not yet available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	// Status.Endpoint derivation is delegated to horizonStatusEndpoint so
	// that the gateway-aware public URL is used when spec.gateway is set, and
	// the cluster-local URL otherwise.
	horizon.Status.Endpoint = horizonStatusEndpoint(horizon)
	conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
		Type:               "DeploymentReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: horizon.Generation,
		Reason:             "DeploymentReady",
		Message:            "Horizon dashboard deployment is available",
	})
	return ctrl.Result{}, nil
}

// buildHorizonDeployment constructs the desired dashboard Deployment. The
// rendered local_settings.py ConfigMap mounts at /etc/openstack-dashboard/
// (the image symlinks the packaged local_settings.py there), and the Django
// SECRET_KEY is injected via the HORIZON_SECRET_KEY env var so key material
// never enters the ConfigMap.
func buildHorizonDeployment(horizon *horizonv1alpha1.Horizon, configMapName, secretKeyHash string) *appsv1.Deployment {
	selector := selectorLabels(horizon)
	labels := naming.ComponentLabels(horizonv1alpha1.AppName, horizon.Name, naming.ComponentAPI)
	// Roll the Deployment when the SECRET_KEY rotates: the key is consumed
	// via HORIZON_SECRET_KEY, so a changed value only takes effect on Pod
	// restart.
	var podAnnotations map[string]string
	if secretKeyHash != "" {
		podAnnotations = map[string]string{secretKeyHashAnnotation: secretKeyHash}
	}
	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      horizon.Namespace,
		Name:           subResourceName(horizon),
		Labels:         labels,
		SelectorLabels: selector,
		PodAnnotations: podAnnotations,
		Deployment:     &horizon.Spec.Deployment,
		Autoscaling:    horizon.Spec.Autoscaling,
		Container: deployment.ContainerParams{
			Name:    "horizon",
			Image:   horizon.Spec.Image.Reference(),
			Command: uwsgiCommand(),
			Env: []corev1.EnvVar{{
				Name: secretKeyEnvVarName,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: horizon.Spec.SecretKeyRef.Name,
						},
						Key: effectiveSecretKeyKey(horizon),
					},
				},
			}},
			Ports: []corev1.ContainerPort{{
				Name:          "horizon",
				ContainerPort: horizonAPIPort,
			}},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{
						Port: intstr.FromInt32(horizonAPIPort),
					},
				},
				InitialDelaySeconds: 15,
				PeriodSeconds:       20,
			},
			// Readiness renders the login page: Django URL routing,
			// templates, and the offline-compression manifest are all
			// exercised without a live Keystone.
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:        dashboardLoginPath,
						Port:        intstr.FromInt32(horizonAPIPort),
						HTTPHeaders: probeHostHeaders(),
					},
				},
				InitialDelaySeconds: 10,
				PeriodSeconds:       15,
				TimeoutSeconds:      10,
				FailureThreshold:    3,
			},
			StartupProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:        dashboardLoginPath,
						Port:        intstr.FromInt32(horizonAPIPort),
						HTTPHeaders: probeHostHeaders(),
					},
				},
				FailureThreshold: 30,
				PeriodSeconds:    10,
			},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "config",
				MountPath: horizonLocalSettingsMountedPath,
				ReadOnly:  true,
			}},
		},
		Volumes: []corev1.Volume{{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName,
					},
				},
			},
		}},
	})
}

// probeHostHeaders pins the Host header the kubelet HTTP probes send to
// probeHostHeader so the probe requests satisfy Django's ALLOWED_HOSTS check
// without the operator having to allow-list the dynamic pod IP.
func probeHostHeaders() []corev1.HTTPHeader {
	return []corev1.HTTPHeader{{Name: "Host", Value: probeHostHeader}}
}

// uwsgiCommand constructs the uWSGI container command. Horizon has no
// per-CR uWSGI knobs in v1 (design decision D5): uwsgi loads
// openstack_dashboard.wsgi directly (the module ships `application`), serves
// the pre-built static assets via --static-map, and mirrors keystone's
// always-on request logging.
func uwsgiCommand() []string {
	return []string{
		"uwsgi",
		"--http", fmt.Sprintf(":%d", horizonAPIPort),
		"--module", "openstack_dashboard.wsgi",
		"--master",
		"--die-on-term",
		"--need-app",
		"--processes", "2",
		"--threads", "1",
		"--static-map", "/static=" + horizonStaticRoot,
		"--log-master",
		"--log-format", "%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto) %(status))",
	}
}

// buildPodDisruptionBudget constructs the desired PDB for the dashboard
// deployment, delegating to the shared builder (minAvailable=1 for
// multi-replica, maxUnavailable=1 for single-replica to avoid drain
// deadlock).
//
// The selector keeps the stable name and instance labels and excludes
// Job-created pods (naming.ExcludeJobPods) rather than narrowing to the API
// component. Horizon runs no maintenance Job today, so the requirement is a
// no-op here and only keeps the budget shaped like the other operators' — but
// unlike the component key it also covers the dashboard pods of a template
// predating that label: a pod no budget selects is not protected at all, and the
// Service's two-phase narrowing has no counterpart here that would bound that
// gap.
func buildPodDisruptionBudget(horizon *horizonv1alpha1.Horizon) *policyv1.PodDisruptionBudget {
	pdb := deployment.BuildPDB(horizon.Namespace, subResourceName(horizon), commonLabels(horizon), selectorLabels(horizon), &horizon.Spec.Deployment)
	pdb.Spec.Selector.MatchExpressions = naming.ExcludeJobPods()
	return pdb
}

// buildHorizonService builds the dashboard Service on the dashboard port. With
// narrowSelector the selector narrows to the API component, so only the
// dashboard pods can become endpoints (see apiSelector for when that is safe).
func buildHorizonService(horizon *horizonv1alpha1.Horizon, narrowSelector bool) *corev1.Service {
	return deployment.BuildService(horizon.Namespace, subResourceName(horizon), commonLabels(horizon), apiSelector(horizon, narrowSelector), horizonAPIPort, horizonAPIPort)
}

// apiSelector returns the selector of the dashboard Service. It narrows to
// app.kubernetes.io/component=api only when narrowSelector is set, which the
// caller derives from deployment.TemplateConverged and then latches: until every
// live pod runs the labelled template, the wide selector is the only one that
// still matches them.
func apiSelector(horizon *horizonv1alpha1.Horizon, narrowSelector bool) map[string]string {
	if narrowSelector {
		return naming.APISelectorLabels(horizonv1alpha1.AppName, horizon.Name)
	}
	return selectorLabels(horizon)
}
