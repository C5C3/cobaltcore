// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// testConfigMapName is the rendered config ConfigMap name a config step would
// hand to reconcileDeployment.
const testConfigMapName = "test-placement-config-abc123"

// readyPlacementDeployment builds the Deployment buildPlacementDeployment would
// produce and marks it available and fully rolled out so EnsureDeployment
// reports ready and the Service selector latch fires.
func readyPlacementDeployment(placement *placementv1alpha1.Placement) *appsv1.Deployment {
	deploy := buildPlacementDeployment(placement, testConfigMapName, "", "")
	replicas := *deploy.Spec.Replicas
	deploy.Generation = 1
	deploy.Status.ObservedGeneration = 1
	deploy.Status.ReadyReplicas = replicas
	deploy.Status.UpdatedReplicas = replicas
	deploy.Status.Replicas = replicas
	deploy.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
	}
	return deploy
}

// argAfter returns the token following flag in cmd, or ok=false when the flag is
// absent or terminal.
func argAfter(cmd []string, flag string) (string, bool) {
	for i, a := range cmd {
		if a == flag && i+1 < len(cmd) {
			return cmd[i+1], true
		}
	}
	return "", false
}

// TestPlacementUWSGICommand_NilAPIServerUsesDefaults pins the command a CR that
// sets no spec.apiServer produces, including the absence of --pyargv: the
// hand-shipped WSGI entry calls init_application() with no arguments, so a
// --pyargv value would be parsed by nothing and the config would silently come
// from OS_PLACEMENT_CONFIG_DIR alone anyway.
func TestPlacementUWSGICommand_NilAPIServerUsesDefaults(t *testing.T) {
	g := NewGomegaWithT(t)

	cmd := placementUWSGICommand(nil)

	g.Expect(cmd[0]).To(Equal("uwsgi"))
	httpBind, ok := argAfter(cmd, "--http")
	g.Expect(ok).To(BeTrue())
	g.Expect(httpBind).To(Equal(":8778"))
	wsgiFile, ok := argAfter(cmd, "--wsgi-file")
	g.Expect(ok).To(BeTrue())
	g.Expect(wsgiFile).To(Equal("/var/lib/openstack/bin/placement-api"))
	g.Expect(cmd).NotTo(ContainElement("--pyargv"),
		"the WSGI entry parses no argv, so a --pyargv flag would be silently ignored")
	g.Expect(cmd).NotTo(ContainElement("--module"))

	// Webhook defaults apply when spec.apiServer is nil.
	procs, _ := argAfter(cmd, "--processes")
	g.Expect(procs).To(Equal("2"))
	threads, _ := argAfter(cmd, "--threads")
	g.Expect(threads).To(Equal("1"))
	// httpKeepAlive defaults to true, harakiri and keepalive-timeout omitted.
	g.Expect(cmd).To(ContainElement("--http-keepalive"))
	g.Expect(cmd).NotTo(ContainElement("--http-keepalive-timeout"))
	g.Expect(cmd).NotTo(ContainElement("--harakiri"))
	// Request logging is unconditional so log lines reach stderr in every
	// configuration.
	g.Expect(cmd).To(ContainElement("--log-master"))
	logFormat, ok := argAfter(cmd, "--log-format")
	g.Expect(ok).To(BeTrue())
	g.Expect(logFormat).To(Equal(deployment.UWSGILogFormat))
}

// TestPlacementUWSGICommand_EmptyAPIServerUsesDefaults covers the spec shape a
// CR that writes `apiServer: {}` produces: a non-nil APIServerSpec whose UWSGI
// pointer is nil. Dereferencing it unguarded would panic the operator.
func TestPlacementUWSGICommand_EmptyAPIServerUsesDefaults(t *testing.T) {
	g := NewGomegaWithT(t)

	cmd := placementUWSGICommand(&placementv1alpha1.APIServerSpec{})

	procs, _ := argAfter(cmd, "--processes")
	g.Expect(procs).To(Equal("2"))
	threads, _ := argAfter(cmd, "--threads")
	g.Expect(threads).To(Equal("1"))
	g.Expect(cmd).To(ContainElement("--http-keepalive"))
}

func TestPlacementUWSGICommand_CustomKnobsHonored(t *testing.T) {
	g := NewGomegaWithT(t)

	apiServer := &placementv1alpha1.APIServerSpec{
		UWSGI: &placementv1alpha1.UWSGISpec{
			Processes:            4,
			Threads:              8,
			HTTPKeepAlive:        ptr.To(true),
			HTTPKeepAliveTimeout: ptr.To(int32(30)),
			Harakiri:             ptr.To(int32(45)),
		},
	}
	cmd := placementUWSGICommand(apiServer)

	procs, _ := argAfter(cmd, "--processes")
	g.Expect(procs).To(Equal("4"))
	threads, _ := argAfter(cmd, "--threads")
	g.Expect(threads).To(Equal("8"))
	timeout, ok := argAfter(cmd, "--http-keepalive-timeout")
	g.Expect(ok).To(BeTrue())
	g.Expect(timeout).To(Equal("30"))
	harakiri, ok := argAfter(cmd, "--harakiri")
	g.Expect(ok).To(BeTrue())
	g.Expect(harakiri).To(Equal("45"))
	g.Expect(cmd).NotTo(ContainElement("--pyargv"))
}

// TestPlacementUWSGICommand_KeepAliveDisabledDropsTimeout pins that the timeout
// flag disappears with its parent feature: --http-keepalive-timeout has no
// meaning without --http-keepalive, and the webhook rejects the combination at
// admission.
func TestPlacementUWSGICommand_KeepAliveDisabledDropsTimeout(t *testing.T) {
	g := NewGomegaWithT(t)

	apiServer := &placementv1alpha1.APIServerSpec{
		UWSGI: &placementv1alpha1.UWSGISpec{
			Processes:            commonv1.DefaultUWSGIProcesses,
			Threads:              commonv1.DefaultUWSGIThreads,
			HTTPKeepAlive:        ptr.To(false),
			HTTPKeepAliveTimeout: ptr.To(int32(30)),
		},
	}
	cmd := placementUWSGICommand(apiServer)

	g.Expect(cmd).NotTo(ContainElement("--http-keepalive"))
	g.Expect(cmd).NotTo(ContainElement("--http-keepalive-timeout"))
	// The spec's worker knobs are honored verbatim (the webhook does the
	// defaulting).
	procs, _ := argAfter(cmd, "--processes")
	g.Expect(procs).To(Equal("2"))
}

// TestBuildPlacementDeployment_EnvVars pins the three env vars the container
// carries and nothing else. The config directory, the DSN, and the service-user
// password are all oslo.config env overrides, so a missing one leaves placement
// reading the image's empty /etc/placement or the DSN placeholder in the
// rendered config.
func TestBuildPlacementDeployment_EnvVars(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildPlacementDeployment(testPlacement(), testConfigMapName, "", "")
	env := deploy.Spec.Template.Spec.Containers[0].Env

	names := []string{}
	byName := map[string]corev1.EnvVar{}
	for _, e := range env {
		names = append(names, e.Name)
		byName[e.Name] = e
	}
	g.Expect(names).To(Equal([]string{
		"OS_PLACEMENT_CONFIG_DIR",
		"OS_PLACEMENT_DATABASE__CONNECTION",
		keystoneauth.PasswordEnvVarName,
	}))

	// The config dir must be the mount point, with no trailing slash and no
	// drift from placementConfigDir.
	g.Expect(byName["OS_PLACEMENT_CONFIG_DIR"].Value).To(Equal("/etc/placement"))
	g.Expect(byName["OS_PLACEMENT_CONFIG_DIR"].Value).To(Equal(placementConfigMountPath))

	// The DSN comes from the derived connection Secret, keyed on the
	// [placement_database] section rather than the [database] section the sibling
	// services use.
	dsn := byName["OS_PLACEMENT_DATABASE__CONNECTION"]
	g.Expect(dsn.ValueFrom).NotTo(BeNil())
	g.Expect(dsn.ValueFrom.SecretKeyRef.Name).To(Equal(database.ConnectionSecretName("test-placement")))
	g.Expect(dsn.ValueFrom.SecretKeyRef.Key).To(Equal(database.ConnectionSecretKey))

	// The service-user password is sourced from the referenced Secret.
	pw := byName[keystoneauth.PasswordEnvVarName]
	g.Expect(pw.ValueFrom).NotTo(BeNil())
	g.Expect(pw.ValueFrom.SecretKeyRef.Name).To(Equal("placement-service-user"))
	g.Expect(pw.ValueFrom.SecretKeyRef.Key).To(Equal("password"))
}

// TestBuildPlacementDeployment_CustomServiceUserKey covers the CR that names a
// non-default key on the service-user Secret; a builder that hardcoded
// "password" would mount an env var pointing at a key that does not exist.
func TestBuildPlacementDeployment_CustomServiceUserKey(t *testing.T) {
	g := NewGomegaWithT(t)

	placement := testPlacement()
	placement.Spec.ServiceUser.SecretRef.Key = "service-password"
	deploy := buildPlacementDeployment(placement, testConfigMapName, "", "")

	var pw *corev1.EnvVar
	for i, e := range deploy.Spec.Template.Spec.Containers[0].Env {
		if e.Name == keystoneauth.PasswordEnvVarName {
			pw = &deploy.Spec.Template.Spec.Containers[0].Env[i]
		}
	}
	g.Expect(pw).NotTo(BeNil())
	g.Expect(pw.ValueFrom.SecretKeyRef.Key).To(Equal("service-password"))
}

// TestBuildPlacementDeployment_ProbesAndSecurityContext pins both probes on the
// version document, the absence of a startup probe, and the restricted security
// context the API container runs under.
func TestBuildPlacementDeployment_ProbesAndSecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildPlacementDeployment(testPlacement(), testConfigMapName, "", "")
	c := deploy.Spec.Template.Spec.Containers[0]

	g.Expect(c.Name).To(Equal("placement-api"))
	g.Expect(c.Ports).To(HaveLen(1))
	g.Expect(c.Ports[0].Name).To(Equal("placement-api"))
	g.Expect(c.Ports[0].ContainerPort).To(Equal(placementAPIPort))

	g.Expect(c.ReadinessProbe).NotTo(BeNil())
	g.Expect(c.ReadinessProbe.HTTPGet).NotTo(BeNil())
	g.Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/"))
	g.Expect(c.ReadinessProbe.HTTPGet.Port.IntVal).To(Equal(placementAPIPort))

	g.Expect(c.LivenessProbe).NotTo(BeNil())
	g.Expect(c.LivenessProbe.HTTPGet).NotTo(BeNil())
	g.Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/"))
	g.Expect(c.LivenessProbe.HTTPGet.Port.IntVal).To(Equal(placementAPIPort))

	// No startup probe: the readiness probe's own delay covers the WSGI app
	// coming up, and adding one would gate liveness behind a second timing knob.
	g.Expect(c.StartupProbe).To(BeNil())

	g.Expect(c.SecurityContext).To(Equal(deployment.RestrictedSecurityContext()))
	g.Expect(deploy.Spec.Template.Spec.SecurityContext).NotTo(BeNil())
	g.Expect(deploy.Spec.Template.Spec.SecurityContext.FSGroup).To(HaveValue(Equal(deployment.OpenStackUID)))
}

// TestBuildPlacementDeployment_ConfigVolume pins that the rendered ConfigMap is
// the whole /etc/placement directory and read-only. Mounting it anywhere else
// would leave the image's empty config directory in place, and a writable mount
// would let a compromised pod rewrite its own configuration.
func TestBuildPlacementDeployment_ConfigVolume(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildPlacementDeployment(testPlacement(), testConfigMapName, "", "")
	podSpec := deploy.Spec.Template.Spec

	g.Expect(podSpec.Volumes).To(HaveLen(1))
	g.Expect(podSpec.Volumes[0].Name).To(Equal(configVolumeName))
	g.Expect(podSpec.Volumes[0].ConfigMap).NotTo(BeNil())
	g.Expect(podSpec.Volumes[0].ConfigMap.Name).To(Equal(testConfigMapName))
	g.Expect(podSpec.Volumes[0].ConfigMap.Items).To(BeEmpty(),
		"the ConfigMap mounts as a whole volume, not as selected keys")

	mounts := podSpec.Containers[0].VolumeMounts
	g.Expect(mounts).To(HaveLen(1))
	g.Expect(mounts[0].Name).To(Equal(configVolumeName))
	g.Expect(mounts[0].MountPath).To(Equal("/etc/placement"))
	g.Expect(mounts[0].ReadOnly).To(BeTrue())
	g.Expect(mounts[0].SubPath).To(BeEmpty())
}

// TestBuildPlacementDeployment_DBTLSVolume pins that the client keypair is
// projected only when spec.database.tls asks for it, and that it lands outside
// /etc/placement: that path is the read-only ConfigMap mount, and the runtime
// cannot create a mountpoint directory inside a read-only tmpfs.
func TestBuildPlacementDeployment_DBTLSVolume(t *testing.T) {
	g := NewGomegaWithT(t)

	// Disabled: no volume, no mount.
	plain := buildPlacementDeployment(testPlacement(), testConfigMapName, "", "")
	for _, v := range plain.Spec.Template.Spec.Volumes {
		g.Expect(v.Name).NotTo(Equal(dbTLSVolumeName))
	}

	placement := testPlacement()
	placement.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "placement-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "placement-db-client"},
	}
	deploy := buildPlacementDeployment(placement, testConfigMapName, "", "")

	var tlsVolume *corev1.Volume
	for i, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == dbTLSVolumeName {
			tlsVolume = &deploy.Spec.Template.Spec.Volumes[i]
		}
	}
	g.Expect(tlsVolume).NotTo(BeNil(), "db-tls volume must be projected when database TLS is enabled")
	g.Expect(tlsVolume.Projected).NotTo(BeNil())
	g.Expect(tlsVolume.Projected.Sources).To(HaveLen(2))

	var tlsMount *corev1.VolumeMount
	for i, m := range deploy.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == dbTLSVolumeName {
			tlsMount = &deploy.Spec.Template.Spec.Containers[0].VolumeMounts[i]
		}
	}
	g.Expect(tlsMount).NotTo(BeNil())
	g.Expect(tlsMount.MountPath).To(Equal(dbTLSMountPath))
	g.Expect(tlsMount.MountPath).NotTo(HavePrefix(placementConfigMountPath+"/"),
		"a mount nested inside the read-only config volume could not be created")
}

// TestBuildPlacementDeployment_HashAnnotations pins that each digest is stamped
// only when non-empty: the requeue and error paths upstream return an empty
// digest, and stamping it would roll every pod on a transient credential read
// failure.
func TestBuildPlacementDeployment_HashAnnotations(t *testing.T) {
	g := NewGomegaWithT(t)

	both := buildPlacementDeployment(testPlacement(), testConfigMapName, "dsn-digest", "auth-digest")
	g.Expect(both.Spec.Template.Annotations).To(HaveKeyWithValue(dbConnectionHashAnnotation, "dsn-digest"))
	g.Expect(both.Spec.Template.Annotations).To(HaveKeyWithValue(authTokenHashAnnotation, "auth-digest"))
	// Stable across rebuilds: an annotation that moved would roll the pods on
	// every reconcile.
	again := buildPlacementDeployment(testPlacement(), testConfigMapName, "dsn-digest", "auth-digest")
	g.Expect(again.Spec.Template.Annotations).To(Equal(both.Spec.Template.Annotations))

	// Both omitted when empty, so no annotation and no spurious rollout.
	neither := buildPlacementDeployment(testPlacement(), testConfigMapName, "", "")
	g.Expect(neither.Spec.Template.Annotations).To(BeEmpty())

	// Only the populated digest is stamped.
	authOnly := buildPlacementDeployment(testPlacement(), testConfigMapName, "", "auth-digest")
	g.Expect(authOnly.Spec.Template.Annotations).To(HaveKeyWithValue(authTokenHashAnnotation, "auth-digest"))
	g.Expect(authOnly.Spec.Template.Annotations).NotTo(HaveKey(dbConnectionHashAnnotation))

	dsnOnly := buildPlacementDeployment(testPlacement(), testConfigMapName, "dsn-digest", "")
	g.Expect(dsnOnly.Spec.Template.Annotations).To(HaveKeyWithValue(dbConnectionHashAnnotation, "dsn-digest"))
	g.Expect(dsnOnly.Spec.Template.Annotations).NotTo(HaveKey(authTokenHashAnnotation))
}

// TestBuildPlacementDeployment_AutoscalingLeavesReplicasUnset pins that an HPA
// owns the replica count. Pinning .spec.replicas while an HPA also targets the
// Deployment makes the two fight over the field, and each write re-triggers a
// reconcile.
func TestBuildPlacementDeployment_AutoscalingLeavesReplicasUnset(t *testing.T) {
	g := NewGomegaWithT(t)

	placement := testPlacement()
	g.Expect(buildPlacementDeployment(placement, testConfigMapName, "", "").Spec.Replicas).NotTo(BeNil())

	placement.Spec.Autoscaling = &placementv1alpha1.AutoscalingSpec{MaxReplicas: 5}
	g.Expect(buildPlacementDeployment(placement, testConfigMapName, "", "").Spec.Replicas).To(BeNil())
}

func TestReconcileDeployment_ServiceAndPDBEnsured(t *testing.T) {
	g := NewGomegaWithT(t)

	placement := testPlacement()
	r := newPlacementTestReconciler(placement)

	res, err := r.reconcileDeployment(context.Background(), r.Client, placement, testConfigMapName, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	// A fresh Deployment is not ready yet, so the step requeues.
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
	cond := meta.FindStatusCondition(placement.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDeployment))
	g.Expect(placement.Status.Endpoint).To(BeEmpty(),
		"an unavailable Deployment must not advertise an endpoint")

	var svc corev1.Service
	g.Expect(r.Get(context.Background(), placementRequest.NamespacedName, &svc)).To(Succeed())
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(placementAPIPort))
	g.Expect(svc.Spec.Ports[0].TargetPort.IntVal).To(Equal(placementAPIPort))
	// The component key is absent while the freshly created Deployment has not
	// rolled out. See TestReconcileDeployment_SelectorNarrowsOnlyAfterRollout.
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{
		"app.kubernetes.io/name":     "placement",
		"app.kubernetes.io/instance": "test-placement",
	}))

	var deploy appsv1.Deployment
	g.Expect(r.Get(context.Background(), placementRequest.NamespacedName, &deploy)).To(Succeed())
	g.Expect(deploy.Labels).To(HaveKeyWithValue(naming.LabelKeyComponent, naming.ComponentAPI))
	g.Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue(naming.LabelKeyComponent, naming.ComponentAPI))
	// The Deployment selector is immutable, so the component key must stay out
	// of it: adding one would force a delete/recreate of every live Deployment.
	g.Expect(deploy.Spec.Selector.MatchLabels).NotTo(HaveKey(naming.LabelKeyComponent))
}

// TestReconcileDeployment_PDBSelectorCoversAPIPodsOnly verifies the budget
// covers every API pod and no Job pod. The exclusion must not be keyed on
// app.kubernetes.io/component=api: that label arrives with this operator build,
// so on the operator upgrade a component-keyed budget matches none of the API
// pods actually serving, and the eviction API only consults budgets matching
// the pod being evicted, so those pods would have no budget at all until the
// migration rollout lands, and forever if it wedges.
func TestReconcileDeployment_PDBSelectorCoversAPIPodsOnly(t *testing.T) {
	g := NewGomegaWithT(t)

	placement := testPlacement()
	r := newPlacementTestReconciler(placement)

	_, err := r.reconcileDeployment(context.Background(), r.Client, placement, testConfigMapName, "", "")
	g.Expect(err).NotTo(HaveOccurred())

	var pdb policyv1.PodDisruptionBudget
	g.Expect(r.Get(context.Background(), placementRequest.NamespacedName, &pdb)).To(Succeed())

	apiPodLabels := buildPlacementDeployment(placement, testConfigMapName, "", "").Spec.Template.Labels
	g.Expect(pdbSelects(g, &pdb, apiPodLabels)).To(BeTrue(),
		"the budget must cover the API pods")
	g.Expect(pdbSelects(g, &pdb, commonLabels(placement))).To(BeTrue(),
		"the budget must cover API pods from a template predating the component label")

	// A db-sync pod carries the CR's labels plus the Job controller's own
	// job-name label, and no readiness probe, so a budget that matched it would
	// count it as healthy from its first moment and raise disruptionsAllowed for
	// the API pods.
	jobPod := map[string]string{}
	for k, v := range apiPodLabels {
		jobPod[k] = v
	}
	jobPod[naming.LabelKeyJobName] = "test-placement-db-sync"
	g.Expect(pdbSelects(g, &pdb, jobPod)).To(BeFalse(),
		"a Job-created pod must never count towards the budget")
}

// pdbSelects reports whether the budget's selector (MatchLabels and
// MatchExpressions together) matches a pod carrying podLabels.
func pdbSelects(g *WithT, pdb *policyv1.PodDisruptionBudget, podLabels map[string]string) bool {
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	g.Expect(err).NotTo(HaveOccurred())
	return selector.Matches(labels.Set(podLabels))
}

// TestReconcileDeployment_SelectorNarrowsOnlyAfterRollout pins the two-phase
// narrowing of the API Service selector. EnsureDeployment is a Server-Side Apply
// that returns as soon as the API server accepts the pod template, so narrowing
// to app.kubernetes.io/component=api in the same pass would drop every pod still
// running from the pre-upgrade template out of the EndpointSlices. The Service
// would have no backends at all until the first re-rolled pod passed its probes,
// and a rollout that wedges would never repool them. The PDB selector, by
// contrast, covers the API pods in every phase.
func TestReconcileDeployment_SelectorNarrowsOnlyAfterRollout(t *testing.T) {
	// surgingDeployment is the state the narrowing must survive: the first
	// new-template pod is Ready, so EnsureDeployment already reports ready, while
	// the old-template pods, the ones without the component label, are still
	// counted and still serving.
	surgingDeployment := func(placement *placementv1alpha1.Placement) *appsv1.Deployment {
		deploy := readyPlacementDeployment(placement)
		deploy.Status.UpdatedReplicas = 1
		deploy.Status.Replicas = *deploy.Spec.Replicas + 1
		return deploy
	}

	cases := []struct {
		name     string
		deploy   func(*placementv1alpha1.Placement) *appsv1.Deployment
		narrowed bool
	}{
		{name: "rollout in flight", deploy: surgingDeployment, narrowed: false},
		{name: "fully rolled out", deploy: readyPlacementDeployment, narrowed: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			placement := testPlacement()
			r := newPlacementTestReconciler(placement, tc.deploy(placement))

			_, err := r.reconcileDeployment(context.Background(), r.Client, placement, testConfigMapName, "", "")
			g.Expect(err).NotTo(HaveOccurred())

			var svc corev1.Service
			g.Expect(r.Get(context.Background(), placementRequest.NamespacedName, &svc)).To(Succeed())
			var pdb policyv1.PodDisruptionBudget
			g.Expect(r.Get(context.Background(), placementRequest.NamespacedName, &pdb)).To(Succeed())

			if tc.narrowed {
				g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(naming.LabelKeyComponent, naming.ComponentAPI))
			} else {
				g.Expect(svc.Spec.Selector).NotTo(HaveKey(naming.LabelKeyComponent),
					"pods from the pre-upgrade template must stay in the EndpointSlices until the rollout completes")
			}
			// Either way the API pods must satisfy the Service selector, or it has
			// no backends at all.
			apiPodLabels := buildPlacementDeployment(placement, testConfigMapName, "", "").Spec.Template.Labels
			g.Expect(labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(apiPodLabels))).To(BeTrue())
			// The budget covers the API pods in BOTH phases, the pre-upgrade ones
			// included.
			g.Expect(pdbSelects(g, &pdb, apiPodLabels)).To(BeTrue())
			g.Expect(pdbSelects(g, &pdb, commonLabels(placement))).To(BeTrue())
		})
	}
}

// TestReconcileDeployment_NarrowedServiceSelectorNeverWidens pins the latch on
// the two-phase narrowing. deployment.TemplateConverged turns false on every
// later rollout, so deriving the selector from it on every pass would not
// migrate the Service once but oscillate it, re-admitting Job pods as endpoints
// for the whole window.
func TestReconcileDeployment_NarrowedServiceSelectorNeverWidens(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	placement := testPlacement()
	r := newPlacementTestReconciler(placement, readyPlacementDeployment(placement))
	key := placementRequest.NamespacedName

	// Pass 1: the Deployment is fully converged, so the migration completes and
	// the Service selector narrows.
	_, err := r.reconcileDeployment(ctx, r.Client, placement, testConfigMapName, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	var svc corev1.Service
	g.Expect(r.Get(ctx, key, &svc)).To(Succeed())
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(naming.LabelKeyComponent, naming.ComponentAPI))

	// Pass 2: the Deployment drops back below full convergence. The Deployment is
	// watched without a generation predicate, so this status transition drives a
	// reconcile of its own.
	var deploy appsv1.Deployment
	g.Expect(r.Get(ctx, key, &deploy)).To(Succeed())
	deploy.Status.UpdatedReplicas = 0
	deploy.Status.ReadyReplicas = 0
	g.Expect(r.Status().Update(ctx, &deploy)).To(Succeed())

	_, err = r.reconcileDeployment(ctx, r.Client, placement, testConfigMapName, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, key, &svc)).To(Succeed())
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(naming.LabelKeyComponent, naming.ComponentAPI),
		"an already narrowed Service selector must never widen again: every pod template this operator builds carries the component label")
}

func TestReconcileDeployment_ReadyStampsClusterLocalEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)

	placement := testPlacement()
	r := newPlacementTestReconciler(placement, readyPlacementDeployment(placement))

	res, err := r.reconcileDeployment(context.Background(), r.Client, placement, testConfigMapName, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(placement.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentReady))
	g.Expect(placement.Status.Endpoint).To(Equal("http://test-placement.default.svc.cluster.local:8778/"))
}

func TestReconcileDeployment_GatewayEndpointStamped(t *testing.T) {
	g := NewGomegaWithT(t)

	placement := testPlacement()
	placement.Spec.Gateway = placementGatewaySpec()
	r := newPlacementTestReconciler(placement, readyPlacementDeployment(placement))

	_, err := r.reconcileDeployment(context.Background(), r.Client, placement, testConfigMapName, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(placement.Status.Endpoint).To(Equal("https://placement.127-0-0-1.nip.io/"))
}

// TestReconcileDeployment_ServiceKeepsServerAssignedClusterIP covers the update
// path over a live Service: the builder leaves ClusterIP unset, so the operator's
// field manager never owns it and the API server keeps the address it assigned.
// A builder that stamped an empty ClusterIP would make every reconcile fight an
// immutable field.
func TestReconcileDeployment_ServiceKeepsServerAssignedClusterIP(t *testing.T) {
	g := NewGomegaWithT(t)

	placement := testPlacement()
	live := buildPlacementService(placement, false)
	live.Spec.ClusterIP = "10.0.0.1"
	r := newPlacementTestReconciler(placement, live)

	_, err := r.reconcileDeployment(context.Background(), r.Client, placement, testConfigMapName, "", "")
	g.Expect(err).NotTo(HaveOccurred())

	var svc corev1.Service
	g.Expect(r.Get(context.Background(), placementRequest.NamespacedName, &svc)).To(Succeed())
	g.Expect(svc.Spec.ClusterIP).To(Equal("10.0.0.1"))
}

// TestReconcileDeployment_SelectorChangeRecreatesDeployment covers the migration
// path off an incompatible selector: Deployment selectors are immutable, so
// EnsureDeployment deletes the live object and reports not-ready rather than
// failing the reconcile forever.
func TestReconcileDeployment_SelectorChangeRecreatesDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	placement := testPlacement()
	stale := readyPlacementDeployment(placement)
	stale.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "legacy"}}
	r := newPlacementTestReconciler(placement, stale)

	res, err := r.reconcileDeployment(ctx, r.Client, placement, testConfigMapName, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))

	var deploy appsv1.Deployment
	err = r.Get(ctx, types.NamespacedName{Namespace: "default", Name: "test-placement"}, &deploy)
	g.Expect(err).To(HaveOccurred(), "the incompatible Deployment must be deleted for re-creation")
}
