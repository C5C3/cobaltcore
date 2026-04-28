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
	"github.com/c5c3/forge/internal/common/deployment"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0013

// Condition reason constants for DeploymentReady.
const conditionReasonDeploymentRolloutComplete = "DeploymentRolloutComplete"

// dbConnectionEnvVarName is the oslo.config env override key for
// [database].connection. The OS_<GROUP>__<OPTION> form wins over the ConfigMap
// value at runtime, so keystone containers read the real DB URL from the
// derived Secret instead of from the ConfigMap (CC-0080, REQ-003).
const dbConnectionEnvVarName = "OS_DATABASE__CONNECTION"

// buildDBConnectionEnvVar returns the EnvVar that overrides
// [database].connection in keystone.conf by sourcing the URL from the derived
// <keystone.Name>-db-connection Secret produced by reconcileDBConnectionSecret
// (CC-0080, REQ-003). Every pod-spec builder that needs database access uses
// this helper to avoid string duplication.
func buildDBConnectionEnvVar(keystone *keystonev1alpha1.Keystone) corev1.EnvVar {
	return corev1.EnvVar{
		Name: dbConnectionEnvVarName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: fmt.Sprintf("%s-db-connection", keystone.Name),
				},
				Key: dbConnectionSecretKey,
			},
		},
	}
}

// reconcileDeployment ensures the Keystone API Deployment and Service exist
// with the correct spec. It sets the DeploymentReady condition and the
// status endpoint when the Deployment becomes available (CC-0013, REQ-006, REQ-012).
//
// Key rotation (fernet + credential) is handled in-place via kubelet Secret
// projection. The pod template does not include hash annotations, so Secret
// data changes do not trigger Deployment rollouts (CC-0074).
func (r *KeystoneReconciler) reconcileDeployment(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error) {
	deploy := buildKeystoneDeployment(keystone, configMapName)
	ready, err := deployment.EnsureDeployment(ctx, r.Client, r.Scheme, keystone, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Deployment: %w", err)
	}

	svc := buildKeystoneService(keystone)
	if err := deployment.EnsureService(ctx, r.Client, r.Scheme, keystone, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Service: %w", err)
	}

	pdb := buildPodDisruptionBudget(keystone)
	if err := deployment.EnsurePDB(ctx, r.Client, r.Scheme, keystone, pdb); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring PodDisruptionBudget: %w", err)
	}

	if !ready {
		log.FromContext(ctx).Info("Keystone API deployment not ready, requeuing")
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "WaitingForDeployment",
			Message:            "Keystone API deployment is not yet available",
		})
		return ctrl.Result{RequeueAfter: RequeueDeploymentPolling}, nil
	}

	// Transition from RollingUpdate to Contracting when Deployment is ready (CC-0056, REQ-004).
	if keystone.Status.UpgradePhase == keystonev1alpha1.UpgradePhaseRollingUpdate {
		keystone.Status.UpgradePhase = keystonev1alpha1.UpgradePhaseContracting
		r.Recorder.Eventf(keystone, corev1.EventTypeNormal, conditionReasonDeploymentRolloutComplete, "Deployment rollout complete during upgrade %s \u2192 %s", keystone.Status.InstalledRelease, keystone.Status.TargetRelease)
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             "DeploymentReady",
			Message:            "Keystone API deployment is available",
		})
		log.FromContext(ctx).Info("Deployment rollout complete, transitioning to contract phase",
			"from", keystone.Status.InstalledRelease, "to", keystone.Status.TargetRelease)
		return ctrl.Result{Requeue: true}, nil
	}

	// Status.Endpoint derivation is delegated to keystoneStatusEndpoint so that
	// the gateway-aware public URL is used when spec.gateway is set, and the
	// cluster-local URL otherwise (CC-0065, REQ-004).
	keystone.Status.Endpoint = keystoneStatusEndpoint(keystone)
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "DeploymentReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "DeploymentReady",
		Message:            "Keystone API deployment is available",
	})
	return ctrl.Result{}, nil
}

// commonLabels returns the standard Kubernetes labels applied to all resources
// owned by this Keystone instance.
func commonLabels(keystone *keystonev1alpha1.Keystone) map[string]string {
	return map[string]string{
		keystonev1alpha1.LabelKeyName:     keystonev1alpha1.AppName,
		keystonev1alpha1.LabelKeyInstance: keystone.Name,
		"app.kubernetes.io/managed-by":    "keystone-operator",
	}
}

// selectorLabels returns the minimal label set used as the Deployment pod
// selector. It is a subset of commonLabels and must remain stable for the
// lifetime of a Deployment (selectors are immutable after creation).
// The label keys and app name are sourced from exported constants in the API
// package (CC-0075) to stay in sync with webhook TSC validation.
func selectorLabels(keystone *keystonev1alpha1.Keystone) map[string]string {
	return map[string]string{
		keystonev1alpha1.LabelKeyName:     keystonev1alpha1.AppName,
		keystonev1alpha1.LabelKeyInstance: keystone.Name,
	}
}

func buildKeystoneDeployment(keystone *keystonev1alpha1.Keystone, configMapName string) *appsv1.Deployment {
	selector := selectorLabels(keystone)
	labels := commonLabels(keystone)
	replicas := int32(keystone.Spec.Replicas)
	fernetSecretName := fmt.Sprintf("%s-fernet-keys", keystone.Name)
	credentialSecretName := fmt.Sprintf("%s-credential-keys", keystone.Name)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subResourceName(keystone),
			Namespace: keystone.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: selector,
			},
			Strategy: deploymentStrategy(keystone),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(terminationGracePeriodSeconds(keystone)),
					TopologySpreadConstraints:     topologySpreadConstraints(keystone),
					PriorityClassName:             priorityClassName(keystone),
					Containers: []corev1.Container{{
						Name:            "keystone",
						Image:           fmt.Sprintf("%s:%s", keystone.Spec.Image.Repository, keystone.Spec.Image.Tag),
						Resources:       containerResources(keystone),
						SecurityContext: restrictedSecurityContext(),
						Command:         uwsgiCommand(keystone.Spec.UWSGI),
						Env:             []corev1.EnvVar{buildDBConnectionEnvVar(keystone)},
						Ports: []corev1.ContainerPort{{
							Name:          "keystone",
							ContainerPort: 5000,
						}},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{
									Port: intstr.FromInt32(5000),
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       20,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/v3",
									Port: intstr.FromInt32(5000),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
						StartupProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/v3",
									Port: intstr.FromInt32(5000),
								},
							},
							FailureThreshold: 30,
							PeriodSeconds:    10,
						},
						Lifecycle: &corev1.Lifecycle{
							PreStop: &corev1.LifecycleHandler{
								Exec: &corev1.ExecAction{
									Command: preStopSleepCommand(keystone),
								},
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "config",
								MountPath: "/etc/keystone/keystone.conf.d/",
								ReadOnly:  true,
							},
							{
								Name:      "fernet-keys",
								MountPath: "/etc/keystone/fernet-keys/",
								ReadOnly:  true,
							},
							{
								Name:      "credential-keys",
								MountPath: "/etc/keystone/credential-keys/",
								ReadOnly:  true,
							},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: configMapName,
									},
								},
							},
						},
						{
							Name: "fernet-keys",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: fernetSecretName,
								},
							},
						},
						{
							Name: "credential-keys",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: credentialSecretName,
								},
							},
						},
					},
				},
			},
		},
	}
}

// Feature: CC-0037

// buildPodDisruptionBudget constructs the desired PDB for the Keystone API
// deployment. When replicas > 1, minAvailable=1 guarantees at least one pod
// remains during voluntary disruptions. When replicas == 1, maxUnavailable=1
// is used instead to avoid drain deadlock (a PDB with minAvailable=1 on a
// single-replica deployment would block all evictions). When replicas == 0
// (e.g. during scale-down), maxUnavailable=1 is set explicitly for clarity
// even though it has no practical effect with zero pods (CC-0037).
func buildPodDisruptionBudget(keystone *keystonev1alpha1.Keystone) *policyv1.PodDisruptionBudget {
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subResourceName(keystone),
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels(keystone),
			},
		},
	}

	if keystone.Spec.Replicas > 1 {
		minAvailable := intstr.FromInt32(1)
		pdb.Spec.MinAvailable = &minAvailable
	} else {
		maxUnavailable := intstr.FromInt32(1)
		pdb.Spec.MaxUnavailable = &maxUnavailable
	}

	return pdb
}

// uwsgiCommand constructs the uWSGI container command from the given spec.
// When uwsgi is nil, hardcoded defaults (processes=2, threads=1,
// httpKeepAlive=true) are used. Fixed flags (--http :5000, --wsgi-file,
// --master, --lazy-apps, --need-app, --pyargv) are always included regardless
// of configuration (CC-0040, REQ-004).
//
// Optional graceful-termination tuning (CC-0084): when UWSGISpec.Harakiri is
// non-nil, "--harakiri <n>" is appended so a single stuck request cannot hold
// a worker past the shutdown envelope (REQ-003). When HTTPKeepAliveTimeout is
// non-nil AND httpKeepAlive is true, "--http-keepalive-timeout <n>" is
// appended so idle keep-alive sockets close before SIGTERM (REQ-004). The
// timeout flag is silently dropped when keep-alive is disabled — the flag has
// no meaning without the parent feature, and the webhook rejects this
// combination at admission (REQ-011).
//
// Always-on uWSGI request logging (CC-0098, REQ-006): "--log-master" and
// "--log-format <literal>" are appended unconditionally between the
// --http-keepalive[-timeout] block and --wsgi-file so request lines reach
// stderr in every configuration, including when keep-alive is disabled.
func uwsgiCommand(uwsgi *keystonev1alpha1.UWSGISpec) []string {
	processes := int32(2)
	threads := int32(1)
	httpKeepAlive := true

	if uwsgi != nil {
		processes = uwsgi.Processes
		threads = uwsgi.Threads
		httpKeepAlive = uwsgi.HTTPKeepAlive
	}

	cmd := []string{
		"uwsgi",
		"--http", ":5000",
	}
	if httpKeepAlive {
		cmd = append(cmd, "--http-keepalive")
		if uwsgi != nil && uwsgi.HTTPKeepAliveTimeout != nil {
			cmd = append(cmd, "--http-keepalive-timeout", strconv.Itoa(int(*uwsgi.HTTPKeepAliveTimeout)))
		}
	}
	// Unconditional: REQ-006 makes uWSGI master logging always-on so request
	// lines reach stderr in every configuration, regardless of keep-alive
	// (CC-0098, REQ-006).
	cmd = append(cmd,
		"--log-master",
		"--log-format", "%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto) %(status))",
	)
	cmd = append(cmd,
		"--wsgi-file", "/var/lib/openstack/bin/keystone-wsgi-public",
		"--master",
		"--lazy-apps",
		"--need-app",
		"--processes", strconv.Itoa(int(processes)),
		"--threads", strconv.Itoa(int(threads)),
		"--pyargv=--config-dir=/etc/keystone/keystone.conf.d/",
	)
	if uwsgi != nil && uwsgi.Harakiri != nil {
		cmd = append(cmd, "--harakiri", strconv.Itoa(int(*uwsgi.Harakiri)))
	}
	return cmd
}

// containerResources returns the ResourceRequirements for the keystone
// container. It dereferences spec.Resources if set, falling back to a zero
// value if nil (safe fallback for CRs that bypassed the webhook, e.g.
// pre-existing CRs during operator upgrade) (CC-0042, CC-0095).
func containerResources(keystone *keystonev1alpha1.Keystone) corev1.ResourceRequirements {
	if keystone.Spec.Resources != nil {
		return *keystone.Spec.Resources
	}
	return corev1.ResourceRequirements{}
}

// topologySpreadConstraints returns the topology spread constraints for the
// Keystone API pods. If spec.TopologySpreadConstraints is non-nil, those are
// used verbatim (an empty slice disables defaults). Otherwise, two default
// constraints are injected: one zone-spread and one hostname-spread, both with
// ScheduleAnyway to distribute pods across zones and nodes (CC-0075).
func topologySpreadConstraints(keystone *keystonev1alpha1.Keystone) []corev1.TopologySpreadConstraint {
	if keystone.Spec.TopologySpreadConstraints != nil {
		return keystone.Spec.TopologySpreadConstraints
	}
	ls := &metav1.LabelSelector{MatchLabels: selectorLabels(keystone)}
	return []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     ls,
		},
		{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     ls,
		},
	}
}

// priorityClassName returns the priority class name for the Keystone API pods.
// If spec.PriorityClassName is set, that value is used. Otherwise, an empty
// string is returned, leaving the cluster default in effect (CC-0075).
func priorityClassName(keystone *keystonev1alpha1.Keystone) string {
	if keystone.Spec.PriorityClassName != nil {
		return *keystone.Spec.PriorityClassName
	}
	return ""
}

// terminationGracePeriodSeconds returns the PodSpec TerminationGracePeriodSeconds
// value. When spec.TerminationGracePeriodSeconds is nil (existing CR, pre-CC-0084
// upgrade), it falls back to keystonev1alpha1.DefaultTerminationGracePeriodSeconds,
// the shared constant that the validating webhook also resolves against for
// cross-field arithmetic. Routing both sides through the same constant prevents
// silent drift (CC-0084, REQ-001).
func terminationGracePeriodSeconds(keystone *keystonev1alpha1.Keystone) int64 {
	if keystone.Spec.TerminationGracePeriodSeconds != nil {
		return *keystone.Spec.TerminationGracePeriodSeconds
	}
	return keystonev1alpha1.DefaultTerminationGracePeriodSeconds
}

// preStopSleepCommand returns the preStop exec command. When
// spec.PreStopSleepSeconds is nil, it falls back to
// keystonev1alpha1.DefaultPreStopSleepSeconds, the shared constant that the
// validating webhook also resolves against for cross-field arithmetic. Zero is
// a permitted opt-out value and emits "sleep 0" verbatim. Routing both sides
// through the same constant prevents silent drift (CC-0084, REQ-002, REQ-009,
// REQ-010).
func preStopSleepCommand(keystone *keystonev1alpha1.Keystone) []string {
	seconds := keystonev1alpha1.DefaultPreStopSleepSeconds
	if keystone.Spec.PreStopSleepSeconds != nil {
		seconds = *keystone.Spec.PreStopSleepSeconds
	}
	return []string{"/bin/sh", "-c", fmt.Sprintf("sleep %d", seconds)}
}

// deploymentStrategy returns the Deployment rollout strategy. When
// spec.Strategy is non-nil, it is returned verbatim (a deep copy, so callers
// cannot mutate the CR). Otherwise, a RollingUpdate strategy with
// MaxUnavailable=0 and MaxSurge=1 is synthesized so available capacity never
// drops below spec.replicas during a rolling image-tag patch — the default
// surge-before-remove behavior that guards Keystone's rolling-update SLO
// (CC-0084, REQ-005, REQ-006).
func deploymentStrategy(keystone *keystonev1alpha1.Keystone) appsv1.DeploymentStrategy {
	if keystone.Spec.Strategy != nil {
		return *keystone.Spec.Strategy.DeepCopy()
	}
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		},
	}
}

func buildKeystoneService(keystone *keystonev1alpha1.Keystone) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subResourceName(keystone),
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Spec: corev1.ServiceSpec{
			Selector: selectorLabels(keystone),
			Ports: []corev1.ServicePort{{
				Port:       5000,
				TargetPort: intstr.FromInt32(5000),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}
