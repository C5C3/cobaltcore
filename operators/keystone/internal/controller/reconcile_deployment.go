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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/deployment"
	"github.com/c5c3/forge/internal/common/naming"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Condition reason constants for DeploymentReady.
const conditionReasonDeploymentRolloutComplete = "DeploymentRolloutComplete"

// buildDBConnectionEnvVar returns the EnvVar that overrides
// [database].connection in keystone.conf by sourcing the URL from the derived
// <keystone.Name>-db-connection Secret produced by reconcileDBConnectionSecret.
// Every pod-spec builder that needs database access uses this helper to avoid
// string duplication; it delegates to the shared database.ConnectionEnvVar.
func buildDBConnectionEnvVar(keystone *keystonev1alpha1.Keystone) corev1.EnvVar {
	return database.ConnectionEnvVar(keystone.Name)
}

// dbReadinessProbeConnectTimeoutSeconds bounds the TCP connect attempt in
// dbReadinessProbeScript. It is deliberately generous: it must sit above the
// worst-case handshake under a *degraded but reachable* database (the chaos
// latency scenario injects ~10s + 2s jitter, so the handshake completes in
// ~12s) and below the unbounded wait of a genuine partition. A reachable-but-
// slow database therefore keeps the Pod Ready, while a partitioned one trips
// the probe (SC-CHAOS-006 vs. SC-CHAOS-007).
const dbReadinessProbeConnectTimeoutSeconds = 20

// dbReadinessProbeScript opens a short-lived TCP connection to the database host
// and port parsed out of the OS_DATABASE__CONNECTION URL that the keystone
// container already consumes. It uses only the Python standard library (present
// in every keystone image) so the probe carries no extra dependency, and it
// connects from the keystone Pod itself — the only network vantage point that
// shares keystone's fate. A keystone-side loss of database connectivity (e.g. a
// network partition that leaves the database CR Ready but cuts keystone off)
// therefore fails the per-Pod readiness check and removes the Pod from the
// Service, surfacing as DeploymentReady=False / Ready=False instead of a silent
// blind spot (SC-CHAOS-006).
var dbReadinessProbeScript = fmt.Sprintf(
	`python3 -c "import os,socket; from urllib.parse import urlparse; raw=os.environ.get('OS_DATABASE__CONNECTION',''); u=urlparse('//'+raw.split('://',1)[-1]); socket.create_connection((u.hostname, u.port or 3306), %d).close()"`,
	dbReadinessProbeConnectTimeoutSeconds,
)

// dbReadinessProbe returns the keystone container readiness probe. Readiness —
// not liveness — depends on database reachability so a database outage depools
// the Pod from the Service without restarting it; liveness stays a plain
// TCP check on the API port so uWSGI is only killed when it is genuinely dead.
// The /v3 version document, by contrast, is served without touching the
// database, so an HTTP probe against it cannot observe a lost DB connection.
//
// Timings tolerate a slow-but-reachable database without depooling: the probe
// timeout sits above the connect timeout, the period above the probe timeout
// (so probes never overlap), and three consecutive failures (~90s of sustained
// unreachability) are required before the Pod leaves the Service.
func dbReadinessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/sh", "-c", dbReadinessProbeScript},
			},
		},
		InitialDelaySeconds: 10,
		PeriodSeconds:       30,
		TimeoutSeconds:      25,
		FailureThreshold:    3,
	}
}

// reconcileDeployment ensures the Keystone API Deployment and Service exist
// with the correct spec. It sets the DeploymentReady condition and the
// status endpoint when the Deployment becomes available.
//
// Fernet + credential key rotation is handled in-place via kubelet Secret
// projection, so those Secret data changes do not carry pod-template hash
// annotations and never trigger Deployment rollouts. The database connection
// string is the exception: it is consumed via the OS_DATABASE__CONNECTION env
// var (not a mounted volume), so a rotated credential only takes effect on a
// Pod restart. In Dynamic credentials mode the DSN changes each time ESO
// re-issues the engine credential (the dbCredentialRefreshInterval cadence, kept
// well below the lease TTL), so dbConnectionHash is stamped into a pod-template
// annotation to roll the Deployment when the engine-issued credential changes.
func (r *KeystoneReconciler) reconcileDeployment(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName, dbConnectionHash, domainsSecretName string, fed *federationProjection) (ctrl.Result, error) {
	deploy := buildKeystoneDeployment(keystone, configMapName, dbConnectionHash, domainsSecretName, fed)
	ready, err := deployment.EnsureDeployment(ctx, r.Client, r.Scheme, keystone, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Deployment: %w", err)
	}

	svc := buildKeystoneService(keystone, fed != nil)
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

	// Transition from RollingUpdate to Contracting when Deployment is ready.
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
	// cluster-local URL otherwise.
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
// owned by this Keystone instance. It delegates to the shared naming package
// (internal/common/naming), the single source of truth for the label
// convention across operators.
func commonLabels(keystone *keystonev1alpha1.Keystone) map[string]string {
	return naming.CommonLabels(keystonev1alpha1.AppName, keystone.Name)
}

// selectorLabels returns the minimal label set used as the Deployment pod
// selector. It is a subset of commonLabels and must remain stable for the
// lifetime of a Deployment (selectors are immutable after creation). It
// delegates to the shared naming package to stay in sync with webhook TSC
// validation across operators.
func selectorLabels(keystone *keystonev1alpha1.Keystone) map[string]string {
	return naming.SelectorLabels(keystonev1alpha1.AppName, keystone.Name)
}

// deploymentReplicas returns the desired .spec.replicas for the Keystone API
// Deployment. When spec.autoscaling is set, it returns nil so the field is left
// unmanaged and the HorizontalPodAutoscaler owns the replica count; otherwise
// it returns the effective replica count. Pinning replicas while an HPA also
// targets the Deployment causes the operator and the HPA to fight over the
// field, and each write re-triggers reconciliation in a scale-up/scale-down
// loop (issue #462). EnsureDeployment preserves the live count when this is nil.
func deploymentReplicas(keystone *keystonev1alpha1.Keystone) *int32 {
	return deployment.DeploymentReplicas(&keystone.Spec.Deployment, keystone.Spec.Autoscaling)
}

// dbConnectionHashAnnotation is the pod-template annotation key stamped with the
// SHA-256 of the DSN in Dynamic credentials mode so a rotated engine-issued
// credential rolls the Deployment (the DSN is env-var-consumed, not volume-
// mounted, so it only takes effect on a Pod restart). It follows the historical
// keystone.c5c3.io/<x>-hash annotation-key style.
const dbConnectionHashAnnotation = "keystone.c5c3.io/db-connection-hash"

func buildKeystoneDeployment(keystone *keystonev1alpha1.Keystone, configMapName, dbConnectionHash, domainsSecretName string, fed *federationProjection) *appsv1.Deployment {
	selector := selectorLabels(keystone)
	labels := commonLabels(keystone)
	federationActive := fed != nil
	fernetSecretName := fmt.Sprintf("%s-fernet-keys", keystone.Name)
	credentialSecretName := fmt.Sprintf("%s-credential-keys", keystone.Name)
	// Roll the Deployment when a Dynamic (engine-issued) credential rotates: the
	// DSN is consumed via OS_DATABASE__CONNECTION, so a changed credential only
	// takes effect on Pod restart. Static/brownfield modes leave the annotation
	// absent so their (rare) credential changes keep the existing no-rollout
	// behavior, consistent with the fernet/credential in-place reload design.
	var podAnnotations map[string]string
	if dbConnectionHash != "" && keystone.Spec.Database.CredentialsMode == commonv1.CredentialsModeDynamic {
		podAnnotations = map[string]string{dbConnectionHashAnnotation: dbConnectionHash}
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subResourceName(keystone),
			Namespace: keystone.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: deploymentReplicas(keystone),
			Selector: &metav1.LabelSelector{
				MatchLabels: selector,
			},
			Strategy: deploymentStrategy(keystone),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(terminationGracePeriodSeconds(keystone)),
					TopologySpreadConstraints:     topologySpreadConstraints(keystone),
					PriorityClassName:             priorityClassName(keystone),
					SecurityContext:               &corev1.PodSecurityContext{FSGroup: ptr.To(deployment.OpenStackUID)},
					Containers: []corev1.Container{{
						Name:            "keystone",
						Image:           keystone.Spec.Image.Reference(),
						Resources:       containerResources(keystone),
						SecurityContext: deployment.RestrictedSecurityContext(),
						Command:         uwsgiCommand(keystone.Spec.UWSGI, federationActive),
						Env:             []corev1.EnvVar{buildDBConnectionEnvVar(keystone)},
						Ports: []corev1.ContainerPort{{
							Name:          "keystone",
							ContainerPort: 5000,
						}},
						LivenessProbe:  keystoneLivenessProbe(federationActive),
						ReadinessProbe: dbReadinessProbe(),
						StartupProbe:   keystoneStartupProbe(federationActive),
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
									SecretName:  fernetSecretName,
									DefaultMode: ptr.To(int32(0o400)),
								},
							},
						},
						{
							Name: "credential-keys",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  credentialSecretName,
									DefaultMode: ptr.To(int32(0o400)),
								},
							},
						},
					},
				},
			},
		},
	}
	// project the db-tls client keypair into the API pod when DB
	// TLS is enabled; the gate is centralised in dbTLSEnabled so deployment
	// and job builders decide identically.
	if dbTLSEnabled(keystone) {
		tlsVol, tlsMount := dbTLSVolumeAndMount(keystone)
		deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, tlsVol)
		deploy.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			deploy.Spec.Template.Spec.Containers[0].VolumeMounts, tlsMount,
		)
	}
	// Mount the per-domain identity-backend config Secret when at least one
	// backend is projected. The Secret name is content-hashed, so attaching,
	// detaching, or re-rendering a backend rolls the Deployment.
	if domainsSecretName != "" {
		domVol, domMount := domainsVolumeAndMount(domainsSecretName)
		deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, domVol)
		deploy.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			deploy.Spec.Template.Spec.Containers[0].VolumeMounts, domMount,
		)
	}
	// Project the mod_auth_openidc sidecar when at least one OIDC backend is
	// attached. The federation Secret name is content-hashed, so a config,
	// client-secret, metadata, or passphrase change rolls the Deployment.
	if federationActive {
		deploy.Spec.Template.Spec.Containers = append(deploy.Spec.Template.Spec.Containers,
			buildFederationProxyContainer(fed))
		deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes,
			buildFederationVolumes(fed)...)
	}
	return deploy
}

// federationProxyTmpVolumeName is the emptyDir scratch space the sidecar's
// Apache writes its pid file, mutexes, and runtime state into (the image's
// httpd-base.conf pins every runtime path to /tmp so the root filesystem can
// stay read-only).
const federationProxyTmpVolumeName = "federation-proxy-tmp"

// keystoneLivenessProbe returns the keystone container liveness probe: a
// plain check that uWSGI still answers on its API port. With federation
// active uWSGI binds 127.0.0.1 only, which kubelet TCP probes (they target
// the pod IP) can no longer reach — the probe becomes an exec-form localhost
// connect instead, keeping identical timing and semantics.
func keystoneLivenessProbe(federationActive bool) *corev1.Probe {
	probe := &corev1.Probe{
		InitialDelaySeconds: 15,
		PeriodSeconds:       20,
	}
	if federationActive {
		probe.ProbeHandler = corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{
				"/bin/sh", "-c",
				`python3 -c "import socket; socket.create_connection(('127.0.0.1', 5000), 5).close()"`,
			}},
		}
		return probe
	}
	probe.ProbeHandler = corev1.ProbeHandler{
		TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(5000)},
	}
	return probe
}

// keystoneStartupProbe returns the keystone container startup probe (GET /v3
// until the WSGI app answers). Same localhost-bind reasoning as
// keystoneLivenessProbe: kubelet HTTP probes target the pod IP, so the
// federation-active variant fetches /v3 from inside the container.
func keystoneStartupProbe(federationActive bool) *corev1.Probe {
	probe := &corev1.Probe{
		FailureThreshold: 30,
		PeriodSeconds:    10,
	}
	if federationActive {
		probe.ProbeHandler = corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{
				"/bin/sh", "-c",
				`python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:5000/v3', timeout=5)"`,
			}},
		}
		return probe
	}
	probe.ProbeHandler = corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: "/v3", Port: intstr.FromInt32(5000)},
	}
	return probe
}

// buildFederationProxyContainer builds the mod_auth_openidc sidecar. The
// resources are fixed modest constants (a reverse proxy for an identity API);
// a spec knob is deferred until a deployment demonstrates the need. The
// readiness probe fetches /v3 THROUGH the proxy so a pod only enters the
// Service when the whole proxy→uWSGI chain answers; startup/liveness stay
// plain TCP so Apache is only restarted when it is genuinely dead.
func buildFederationProxyContainer(fed *federationProjection) corev1.Container {
	return corev1.Container{
		Name:            "federation-proxy",
		Image:           fed.ProxyImage.Reference(),
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
		Command: []string{"apache2", "-DFOREGROUND", "-f", "/etc/keystone-federation-proxy/httpd-base.conf"},
		Ports: []corev1.ContainerPort{{
			Name:          "proxy",
			ContainerPort: federationProxyPort,
		}},
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(federationProxyPort)},
			},
			FailureThreshold: 30,
			PeriodSeconds:    2,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(federationProxyPort)},
			},
			InitialDelaySeconds: 15,
			PeriodSeconds:       20,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/v3", Port: intstr.FromInt32(federationProxyPort)},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		VolumeMounts: buildFederationProxyVolumeMounts(fed),
	}
}

// buildFederationProxyVolumeMounts assembles the sidecar's volume mounts. The
// OIDC metadata mount is present only when an OIDC backend is projected and the
// mellon mount only when a SAML backend is projected — a SAML-only projection
// must not mount an item-less metadata Secret volume (an empty Items list
// projects ALL keys, leaking the SP key material into OIDCMetadataDir).
func buildFederationProxyVolumeMounts(fed *federationProjection) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{{
		Name:      federationProxyConfigVolumeName,
		MountPath: federationProxyConfMountPath,
		ReadOnly:  true,
	}}
	if len(fed.MetadataItems) > 0 {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      federationMetadataVolumeName,
			MountPath: federationMetadataMountPath,
			ReadOnly:  true,
		})
	}
	if len(fed.MellonItems) > 0 {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      federationMellonVolumeName,
			MountPath: federationMellonMountPath,
			ReadOnly:  true,
		})
	}
	mounts = append(mounts, corev1.VolumeMount{
		Name:      federationProxyTmpVolumeName,
		MountPath: "/tmp",
	})
	return mounts
}

// buildFederationVolumes builds the pod volumes backing the sidecar: the config
// volumes source the one content-hashed federation Secret (0o400 — it carries
// client secrets, the crypto passphrase, and the SAML SP key) with different
// KeyToPath item sets, plus the emptyDir scratch space for Apache's runtime
// files. The metadata volume renders only when an OIDC backend is projected and
// the mellon volume only when a SAML backend is projected: mounting an
// item-less Secret volume would project ALL keys, leaking one federation type's
// material into the other's mount path.
func buildFederationVolumes(fed *federationProjection) []corev1.Volume {
	volumes := []corev1.Volume{{
		Name: federationProxyConfigVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  fed.SecretName,
				DefaultMode: ptr.To(int32(0o400)),
				Items:       []corev1.KeyToPath{{Key: "proxy.conf", Path: "proxy.conf"}},
			},
		},
	}}
	if len(fed.MetadataItems) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: federationMetadataVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  fed.SecretName,
					DefaultMode: ptr.To(int32(0o400)),
					Items:       fed.MetadataItems,
				},
			},
		})
	}
	if len(fed.MellonItems) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: federationMellonVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  fed.SecretName,
					DefaultMode: ptr.To(int32(0o400)),
					Items:       fed.MellonItems,
				},
			},
		})
	}
	volumes = append(volumes, corev1.Volume{
		Name:         federationProxyTmpVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	return volumes
}

// buildPodDisruptionBudget constructs the desired PDB for the Keystone API
// deployment. It branches on deployment.EffectiveReplicas — so a zero-valued
// spec.deployment.replicas normalizes to the default, matching the Deployment's
// own replica count — rather than the raw spec value. When the effective count
// is > 1, minAvailable=1 guarantees at least one pod remains during voluntary
// disruptions. When it is 1, maxUnavailable=1 is used instead to avoid drain
// deadlock (a PDB with minAvailable=1 on a single-replica deployment would block
// all evictions).
func buildPodDisruptionBudget(keystone *keystonev1alpha1.Keystone) *policyv1.PodDisruptionBudget {
	return deployment.BuildPDB(keystone.Namespace, subResourceName(keystone), commonLabels(keystone), selectorLabels(keystone), &keystone.Spec.Deployment)
}

// uwsgiCommand constructs the uWSGI container command from the given spec.
// When uwsgi is nil, hardcoded defaults (processes=2, threads=1,
// httpKeepAlive=true) are used. Fixed flags (--http, --wsgi-file,
// --master, --lazy-apps, --need-app, --pyargv) are always included regardless
// of configuration.
//
// federationActive rebinds uWSGI to 127.0.0.1:5000 — only the sidecar may
// reach it, so no in-cluster client can bypass the header-stripping proxy —
// and appends --buffer-size 65535: the spike measured claim headers plus the
// ~4 KiB client-cookie session blowing the 4096-byte default (502s).
//
// Optional graceful-termination tuning when UWSGISpec.Harakiri is
// non-nil, "--harakiri <n>" is appended so a single stuck request cannot hold
// a worker past the shutdown envelope. When HTTPKeepAliveTimeout is
// non-nil AND httpKeepAlive is true, "--http-keepalive-timeout <n>" is
// appended so idle keep-alive sockets close before SIGTERM. The
// timeout flag is silently dropped when keep-alive is disabled — the flag has
// no meaning without the parent feature, and the webhook rejects this
// combination at admission.
//
// Always-on uWSGI request logging "--log-master" and
// "--log-format <literal>" are appended unconditionally between the
// --http-keepalive[-timeout] block and --wsgi-file so request lines reach
// stderr in every configuration, including when keep-alive is disabled.
func uwsgiCommand(uwsgi *keystonev1alpha1.UWSGISpec, federationActive bool) []string {
	processes := keystonev1alpha1.DefaultUWSGIProcesses
	threads := keystonev1alpha1.DefaultUWSGIThreads
	httpKeepAlive := keystonev1alpha1.DefaultUWSGIHTTPKeepAlive

	if uwsgi != nil {
		processes = uwsgi.Processes
		threads = uwsgi.Threads
		// HTTPKeepAlive is a nil-preserving *bool: nil means "unset", which keeps
		// the default (true); an explicit true/false is honored verbatim.
		if uwsgi.HTTPKeepAlive != nil {
			httpKeepAlive = *uwsgi.HTTPKeepAlive
		}
	}

	bind := ":5000"
	if federationActive {
		bind = "127.0.0.1:5000"
	}
	cmd := []string{
		"uwsgi",
		"--http", bind,
	}
	if federationActive {
		cmd = append(cmd, "--buffer-size", strconv.Itoa(uwsgiFederationBufferSize))
	}
	if httpKeepAlive {
		cmd = append(cmd, "--http-keepalive")
		if uwsgi != nil && uwsgi.HTTPKeepAliveTimeout != nil {
			cmd = append(cmd, "--http-keepalive-timeout", strconv.Itoa(int(*uwsgi.HTTPKeepAliveTimeout)))
		}
	}
	// Unconditional: makes uWSGI master logging always-on so request
	// lines reach stderr in every configuration, regardless of keep-alive
	cmd = append(
		cmd,
		"--log-master",
		"--log-format", "%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto) %(status))",
	)
	cmd = append(
		cmd,
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
// pre-existing CRs during operator upgrade).
func containerResources(keystone *keystonev1alpha1.Keystone) corev1.ResourceRequirements {
	return deployment.ContainerResources(&keystone.Spec.Deployment)
}

// topologySpreadConstraints returns the topology spread constraints for the
// Keystone API pods. If spec.TopologySpreadConstraints is non-nil, those are
// used verbatim (an empty slice disables defaults). Otherwise, two default
// constraints are injected: one zone-spread and one hostname-spread, both with
// ScheduleAnyway to distribute pods across zones and nodes.
func topologySpreadConstraints(keystone *keystonev1alpha1.Keystone) []corev1.TopologySpreadConstraint {
	return deployment.TopologySpreadConstraints(&keystone.Spec.Deployment, selectorLabels(keystone))
}

// priorityClassName returns the priority class name for the Keystone API pods.
// If spec.PriorityClassName is set, that value is used. Otherwise, an empty
// string is returned, leaving the cluster default in effect.
func priorityClassName(keystone *keystonev1alpha1.Keystone) string {
	return deployment.PriorityClassName(&keystone.Spec.Deployment)
}

// terminationGracePeriodSeconds returns the PodSpec TerminationGracePeriodSeconds
// value. When spec.TerminationGracePeriodSeconds is nil (existing CR, pre- upgrade), it falls back to keystonev1alpha1.DefaultTerminationGracePeriodSeconds,
// the shared constant that the validating webhook also resolves against for
// cross-field arithmetic. Routing both sides through the same constant prevents
// silent drift.
func terminationGracePeriodSeconds(keystone *keystonev1alpha1.Keystone) int64 {
	return deployment.TerminationGracePeriodSeconds(&keystone.Spec.Deployment)
}

// preStopSleepCommand returns the preStop exec command. When
// spec.PreStopSleepSeconds is nil, it falls back to
// keystonev1alpha1.DefaultPreStopSleepSeconds, the shared constant that the
// validating webhook also resolves against for cross-field arithmetic. Zero is
// a permitted opt-out value and emits "sleep 0" verbatim. Routing both sides
// through the same constant prevents silent drift.
func preStopSleepCommand(keystone *keystonev1alpha1.Keystone) []string {
	return deployment.PreStopSleepCommand(&keystone.Spec.Deployment)
}

// deploymentStrategy returns the Deployment rollout strategy. When
// spec.Strategy is non-nil, it is returned verbatim (a deep copy, so callers
// cannot mutate the CR). Otherwise, a RollingUpdate strategy with
// MaxUnavailable=0 and MaxSurge=1 is synthesized so available capacity never
// drops below spec.replicas during a rolling image-tag patch — the default
// surge-before-remove behavior that guards Keystone's rolling-update SLO
func deploymentStrategy(keystone *keystonev1alpha1.Keystone) appsv1.DeploymentStrategy {
	return deployment.Strategy(&keystone.Spec.Deployment)
}

// buildKeystoneService builds the Keystone API Service. The Service port
// stays 5000 in every configuration — internalAPIURL, the status endpoint,
// the HTTPRoute backend, and the health check all key on it — but with
// federation active the targetPort switches to the sidecar so every request
// traverses the header-stripping proxy.
func buildKeystoneService(keystone *keystonev1alpha1.Keystone, federationActive bool) *corev1.Service {
	targetPort := int32(5000)
	if federationActive {
		targetPort = federationProxyPort
	}
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
				TargetPort: intstr.FromInt32(targetPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}
