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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/keystoneauth"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// deployGlance returns a Glance for the deployment tests at the given release,
// with a concrete replica count so the built Deployment is deterministic.
func deployGlance(release string) *glancev1alpha1.Glance {
	gl := testGlance()
	gl.Spec.OpenStackRelease = release
	gl.Spec.Deployment.Replicas = 2
	return gl
}

// testArtifacts returns the config/backends artefact names a rendered config
// step would hand to reconcileDeployment.
func testArtifacts() configArtifacts {
	return configArtifacts{configMapName: "glance-config-abc123", backendsSecretName: "glance-backends-def456"}
}

// readyGlanceDeployment builds the Deployment buildGlanceDeployment would
// produce and marks it available so EnsureDeployment reports ready.
func readyGlanceDeployment(glance *glancev1alpha1.Glance, art configArtifacts) *appsv1.Deployment {
	deploy := buildGlanceDeployment(glance, art, "", "")
	replicas := int32(glance.Spec.Deployment.Replicas)
	deploy.Spec.Replicas = &replicas
	deploy.Generation = 1
	deploy.Status.ObservedGeneration = 1
	deploy.Status.ReadyReplicas = replicas
	// A ready Deployment in these tests is also fully rolled out — every replica
	// updated and counted, no surge pod left — so the RollingUpdate → Contracting
	// flip (which gates on full convergence, not surge-tolerant readiness) fires.
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

func TestGlanceLaunchCommand_EventletBelow2026(t *testing.T) {
	g := NewGomegaWithT(t)

	cmd := glanceLaunchCommand(deployGlance("2025.2"))
	g.Expect(cmd).To(Equal([]string{
		"glance-api",
		"--config-dir", glanceConfigDir,
		"--config-dir", glanceBackendsConfigDir,
	}), "eventlet mode launches glance-api with both config dirs and no uWSGI flags")
}

func TestGlanceLaunchCommand_UWSGIFrom2026(t *testing.T) {
	g := NewGomegaWithT(t)

	cmd := glanceLaunchCommand(deployGlance("2026.1"))
	g.Expect(cmd[0]).To(Equal("uwsgi"))
	// Fixed uWSGI flags for the WSGI launch mode. The entry point is the
	// image-shipped shim, NOT glance.wsgi.api:application: the stock module
	// ignores sys.argv (and so --pyargv), reading only
	// $OS_GLANCE_CONFIG_DIR/glance-api.conf — the shim consumes the
	// --config-dir flags asserted below.
	g.Expect(cmd).To(ContainElements("--wsgi-file", glanceWSGIScriptPath))
	g.Expect(cmd).NotTo(ContainElement("--module"))
	g.Expect(cmd).To(ContainElements("--http-auto-chunked", "--http-chunked-input"))
	httpBind, ok := argAfter(cmd, "--http")
	g.Expect(ok).To(BeTrue())
	g.Expect(httpBind).To(Equal(":9292"))
	pyargv, ok := argAfter(cmd, "--pyargv")
	g.Expect(ok).To(BeTrue())
	g.Expect(pyargv).To(Equal("--config-dir " + glanceConfigDir + " --config-dir " + glanceBackendsConfigDir))
	// Webhook defaults apply when spec.apiServer is nil.
	procs, _ := argAfter(cmd, "--processes")
	g.Expect(procs).To(Equal("2"))
	threads, _ := argAfter(cmd, "--threads")
	g.Expect(threads).To(Equal("1"))
	// httpKeepAlive defaults to true, harakiri and keepalive-timeout omitted.
	g.Expect(cmd).To(ContainElement("--http-keepalive"))
	g.Expect(cmd).NotTo(ContainElement("--http-keepalive-timeout"))
	g.Expect(cmd).NotTo(ContainElement("--harakiri"))
}

func TestGlanceUWSGICommand_CustomKnobsHonored(t *testing.T) {
	g := NewGomegaWithT(t)

	apiServer := &glancev1alpha1.APIServerSpec{
		UWSGI: &glancev1alpha1.UWSGISpec{
			Processes:            4,
			Threads:              8,
			HTTPKeepAlive:        ptr.To(true),
			HTTPKeepAliveTimeout: ptr.To(int32(30)),
			Harakiri:             ptr.To(int32(45)),
		},
	}
	cmd := glanceUWSGICommand(apiServer)

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
}

func TestGlanceUWSGICommand_KeepAliveDisabledDropsTimeout(t *testing.T) {
	g := NewGomegaWithT(t)

	apiServer := &glancev1alpha1.APIServerSpec{
		UWSGI: &glancev1alpha1.UWSGISpec{
			// A webhook-defaulted spec that explicitly disables keep-alive.
			Processes:            glancev1alpha1.DefaultUWSGIProcesses,
			Threads:              glancev1alpha1.DefaultUWSGIThreads,
			HTTPKeepAlive:        ptr.To(false),
			HTTPKeepAliveTimeout: ptr.To(int32(30)),
		},
	}
	cmd := glanceUWSGICommand(apiServer)

	// With keep-alive disabled, neither the flag nor its timeout are emitted.
	g.Expect(cmd).NotTo(ContainElement("--http-keepalive"))
	g.Expect(cmd).NotTo(ContainElement("--http-keepalive-timeout"))
	// The spec's worker knobs are honored verbatim (webhook does the defaulting).
	procs, _ := argAfter(cmd, "--processes")
	g.Expect(procs).To(Equal("2"))
}

func TestGlanceUWSGICommand_NilAPIServerUsesDefaults(t *testing.T) {
	g := NewGomegaWithT(t)

	cmd := glanceUWSGICommand(nil)
	procs, _ := argAfter(cmd, "--processes")
	g.Expect(procs).To(Equal("2"))
	threads, _ := argAfter(cmd, "--threads")
	g.Expect(threads).To(Equal("1"))
	g.Expect(cmd).To(ContainElement("--http-keepalive"))
	g.Expect(cmd).NotTo(ContainElement("--harakiri"))
}

func TestBuildGlanceDeployment_Volumes(t *testing.T) {
	g := NewGomegaWithT(t)

	art := testArtifacts()
	deploy := buildGlanceDeployment(deployGlance("2026.1"), art, "", "")
	volumesByName := map[string]corev1.Volume{}
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		volumesByName[v.Name] = v
	}

	// config ConfigMap volume.
	g.Expect(volumesByName).To(HaveKey(configVolumeName))
	g.Expect(volumesByName[configVolumeName].ConfigMap).NotTo(BeNil())
	g.Expect(volumesByName[configVolumeName].ConfigMap.Name).To(Equal(art.configMapName))
	// backends Secret volume — read back by the GlanceBackend controller.
	g.Expect(volumesByName).To(HaveKey(backendsVolumeName))
	g.Expect(volumesByName[backendsVolumeName].Secret).NotTo(BeNil())
	g.Expect(volumesByName[backendsVolumeName].Secret.SecretName).To(Equal(art.backendsSecretName))
	// Reserved-store emptyDirs, both bounded by the operator default when
	// spec.staging is unset — an unbounded scratch volume lets one import fill
	// the node's disk.
	defaultLimit := resource.MustParse("10Gi")
	g.Expect(volumesByName).To(HaveKey(stagingVolumeName))
	g.Expect(volumesByName[stagingVolumeName].EmptyDir).NotTo(BeNil())
	g.Expect(volumesByName[stagingVolumeName].EmptyDir.SizeLimit).NotTo(BeNil())
	g.Expect(*volumesByName[stagingVolumeName].EmptyDir.SizeLimit).To(Equal(defaultLimit))
	g.Expect(volumesByName).To(HaveKey(tasksVolumeName))
	g.Expect(volumesByName[tasksVolumeName].EmptyDir).NotTo(BeNil())
	g.Expect(volumesByName[tasksVolumeName].EmptyDir.SizeLimit).NotTo(BeNil())
	g.Expect(*volumesByName[tasksVolumeName].EmptyDir.SizeLimit).To(Equal(defaultLimit))
	// No db-tls volume when TLS is disabled.
	g.Expect(volumesByName).NotTo(HaveKey(dbTLSVolumeName))

	// Mount paths line up with the config dirs and store paths.
	mountsByName := map[string]corev1.VolumeMount{}
	for _, m := range deploy.Spec.Template.Spec.Containers[0].VolumeMounts {
		mountsByName[m.Name] = m
	}
	g.Expect(mountsByName[configVolumeName].MountPath).To(Equal(glanceConfigDir))
	g.Expect(mountsByName[backendsVolumeName].MountPath).To(Equal(glanceBackendsConfigDir))
	g.Expect(mountsByName[stagingVolumeName].MountPath).To(Equal(glanceStagingStorePath))
	g.Expect(mountsByName[tasksVolumeName].MountPath).To(Equal(glanceTasksStorePath))
}

// TestEffectiveStagingSizeLimit pins the three ways of saying "use the operator
// default" — no block, an empty block, an explicitly nil sizeLimit — against the
// two ways of overriding it: a larger bound, and unbounded. All of them are
// resolved at reconcile time, so none depends on the defaulting webhook having
// run. Unbounded resolving to nil is what keeps a deployment that predates the
// bound able to stay unbounded across the operator upgrade that introduces it.
func TestEffectiveStagingSizeLimit(t *testing.T) {
	tests := []struct {
		name    string
		staging *glancev1alpha1.StagingSpec
		want    *resource.Quantity
	}{
		{
			name:    "nil block resolves the default",
			staging: nil,
			want:    ptr.To(glancev1alpha1.DefaultStagingSizeLimit()),
		},
		{
			name:    "empty block resolves the default",
			staging: &glancev1alpha1.StagingSpec{},
			want:    ptr.To(glancev1alpha1.DefaultStagingSizeLimit()),
		},
		{
			name:    "explicit nil sizeLimit resolves the default",
			staging: &glancev1alpha1.StagingSpec{SizeLimit: nil},
			want:    ptr.To(glancev1alpha1.DefaultStagingSizeLimit()),
		},
		{
			name:    "explicit sizeLimit is honored",
			staging: &glancev1alpha1.StagingSpec{SizeLimit: ptr.To(resource.MustParse("50Gi"))},
			want:    ptr.To(resource.MustParse("50Gi")),
		},
		{
			name:    "unbounded resolves to no limit",
			staging: &glancev1alpha1.StagingSpec{Unbounded: true},
			want:    nil,
		},
		{
			// unbounded: false is the zero value and must not be mistaken for a
			// third way of asking for the default's absence.
			name:    "explicit unbounded false resolves the default",
			staging: &glancev1alpha1.StagingSpec{Unbounded: false},
			want:    ptr.To(glancev1alpha1.DefaultStagingSizeLimit()),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			glance := deployGlance("2026.1")
			glance.Spec.Staging = tc.staging

			got := effectiveStagingSizeLimit(glance)
			if tc.want == nil {
				g.Expect(got).To(BeNil())
				return
			}
			g.Expect(got).NotTo(BeNil())
			g.Expect(*got).To(Equal(*tc.want))
		})
	}
}

// TestBuildGlanceDeployment_UnboundedStaging pins the opt-out end to end: both
// scratch emptyDirs must render with no sizeLimit at all, the shape a Glance
// carried before the bound existed. Asserting on the built Deployment rather
// than only on the resolver catches a later change that re-derives a limit from
// the nil, which would silently re-impose eviction on a deployment that opted
// out of it.
func TestBuildGlanceDeployment_UnboundedStaging(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1")
	glance.Spec.Staging = &glancev1alpha1.StagingSpec{Unbounded: true}
	deploy := buildGlanceDeployment(glance, testArtifacts(), "", "")

	volumesByName := map[string]corev1.Volume{}
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		volumesByName[v.Name] = v
	}

	for _, name := range []string{stagingVolumeName, tasksVolumeName} {
		emptyDir := volumesByName[name].EmptyDir
		g.Expect(emptyDir).NotTo(BeNil())
		g.Expect(emptyDir.SizeLimit).To(BeNil(),
			"%s must carry no sizeLimit when spec.staging.unbounded is set", name)
	}
}

// TestBuildGlanceDeployment_ExplicitStagingSizeLimit pins that the configured
// bound reaches both scratch volumes, and that each gets its own Quantity: a
// shared pointer would make the two emptyDirs alias one value that a later
// mutation of either would move under both.
func TestBuildGlanceDeployment_ExplicitStagingSizeLimit(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1")
	glance.Spec.Staging = &glancev1alpha1.StagingSpec{SizeLimit: ptr.To(resource.MustParse("25Gi"))}
	deploy := buildGlanceDeployment(glance, testArtifacts(), "", "")

	volumesByName := map[string]corev1.Volume{}
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		volumesByName[v.Name] = v
	}

	staging := volumesByName[stagingVolumeName].EmptyDir
	tasks := volumesByName[tasksVolumeName].EmptyDir
	g.Expect(staging).NotTo(BeNil())
	g.Expect(tasks).NotTo(BeNil())
	g.Expect(staging.SizeLimit).NotTo(BeNil())
	g.Expect(tasks.SizeLimit).NotTo(BeNil())
	g.Expect(*staging.SizeLimit).To(Equal(resource.MustParse("25Gi")))
	g.Expect(*tasks.SizeLimit).To(Equal(resource.MustParse("25Gi")))
	g.Expect(staging.SizeLimit).NotTo(BeIdenticalTo(tasks.SizeLimit),
		"each emptyDir must carry its own Quantity, not a shared pointer")
}

func TestBuildGlanceDeployment_DBTLSVolumeWhenEnabled(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1")
	glance.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "glance-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "glance-db-client"},
	}
	deploy := buildGlanceDeployment(glance, testArtifacts(), "", "")

	var found bool
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == dbTLSVolumeName {
			found = true
			g.Expect(v.Projected).NotTo(BeNil())
		}
	}
	g.Expect(found).To(BeTrue(), "db-tls volume must be projected when database TLS is enabled")

	var mounted bool
	for _, m := range deploy.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == dbTLSVolumeName {
			mounted = true
			g.Expect(m.MountPath).To(Equal(dbTLSMountPath))
		}
	}
	g.Expect(mounted).To(BeTrue())
}

func TestBuildGlanceDeployment_EnvVars(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildGlanceDeployment(deployGlance("2026.1"), testArtifacts(), "", "")
	env := deploy.Spec.Template.Spec.Containers[0].Env
	names := map[string]corev1.EnvVar{}
	for _, e := range env {
		names[e.Name] = e
	}
	g.Expect(names).To(HaveKey(database.ConnectionEnvVarName))
	g.Expect(names).To(HaveKey(keystoneauth.PasswordEnvVarName))
	// The service-user password is sourced from the referenced Secret.
	pw := names[keystoneauth.PasswordEnvVarName]
	g.Expect(pw.ValueFrom).NotTo(BeNil())
	g.Expect(pw.ValueFrom.SecretKeyRef.Name).To(Equal("glance-service-user"))
	g.Expect(pw.ValueFrom.SecretKeyRef.Key).To(Equal("password"))
}

func TestBuildGlanceDeployment_HashAnnotations(t *testing.T) {
	g := NewGomegaWithT(t)

	// Both digests stamped when non-empty.
	deploy := buildGlanceDeployment(deployGlance("2026.1"), testArtifacts(), "dsn-digest", "auth-digest")
	ann := deploy.Spec.Template.Annotations
	g.Expect(ann).To(HaveKeyWithValue(dbConnectionHashAnnotation, "dsn-digest"))
	g.Expect(ann).To(HaveKeyWithValue(authTokenHashAnnotation, "auth-digest"))

	// Both omitted when empty — no annotation, no spurious rollout.
	deployEmpty := buildGlanceDeployment(deployGlance("2026.1"), testArtifacts(), "", "")
	g.Expect(deployEmpty.Spec.Template.Annotations).To(BeEmpty())

	// Only the populated digest is stamped.
	deployAuthOnly := buildGlanceDeployment(deployGlance("2026.1"), testArtifacts(), "", "auth-digest")
	g.Expect(deployAuthOnly.Spec.Template.Annotations).To(HaveKeyWithValue(authTokenHashAnnotation, "auth-digest"))
	g.Expect(deployAuthOnly.Spec.Template.Annotations).NotTo(HaveKey(dbConnectionHashAnnotation))
}

func TestBuildGlanceDeployment_ProbesOnHealthcheck(t *testing.T) {
	g := NewGomegaWithT(t)

	for _, release := range []string{"2025.2", "2026.1"} {
		deploy := buildGlanceDeployment(deployGlance(release), testArtifacts(), "", "")
		c := deploy.Spec.Template.Spec.Containers[0]
		g.Expect(c.Name).To(Equal("glance-api"))
		g.Expect(c.Ports[0].ContainerPort).To(Equal(glanceAPIPort))

		g.Expect(c.ReadinessProbe).NotTo(BeNil())
		g.Expect(c.ReadinessProbe.HTTPGet).NotTo(BeNil())
		g.Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/healthcheck"))
		g.Expect(c.ReadinessProbe.HTTPGet.Port.IntVal).To(Equal(glanceAPIPort))

		g.Expect(c.LivenessProbe).NotTo(BeNil())
		g.Expect(c.LivenessProbe.HTTPGet).NotTo(BeNil())
		g.Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/healthcheck"))
		g.Expect(c.LivenessProbe.HTTPGet.Port.IntVal).To(Equal(glanceAPIPort))
	}
}

func TestReconcileDeployment_ServiceAndPDBEnsured(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1")
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDeployment(context.Background(), glance, testArtifacts(), "", "")
	g.Expect(err).NotTo(HaveOccurred())
	// Fresh Deployment is not ready yet — requeue.
	g.Expect(res.RequeueAfter).To(Equal(RequeueDeploymentPolling))
	cond := meta.FindStatusCondition(glance.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDeployment))

	var svc corev1.Service
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"}, &svc)).To(Succeed())
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(glanceAPIPort))

	var deploy appsv1.Deployment
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"}, &deploy)).To(Succeed())
}

func TestReconcileDeployment_ReadyStampsEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1")
	art := testArtifacts()
	r := newGlanceTestReconciler(glance, readyGlanceDeployment(glance, art))

	res, err := r.reconcileDeployment(context.Background(), glance, art, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(glance.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentReady))
	// Cluster-local endpoint when no gateway is configured.
	g.Expect(glance.Status.Endpoint).To(Equal("http://test-glance.default.svc.cluster.local:9292/"))
}

func TestReconcileDeployment_GatewayEndpointStamped(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1")
	glance.Spec.Gateway = &glancev1alpha1.GatewaySpec{
		ParentRef: glancev1alpha1.GatewayParentRefSpec{Name: "openstack-gw", Namespace: "envoy-gateway-system"},
		Hostname:  "glance.127-0-0-1.nip.io",
	}
	art := testArtifacts()
	r := newGlanceTestReconciler(glance, readyGlanceDeployment(glance, art))

	_, err := r.reconcileDeployment(context.Background(), glance, art, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(glance.Status.Endpoint).To(Equal("https://glance.127-0-0-1.nip.io/"))
}

func TestReconcileDeployment_InvalidProjectionNoDeploymentWaitsForBackends(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1")
	r := newGlanceTestReconciler(glance)

	// Empty artefacts (invalid projection, first install): no Deployment yet.
	res, err := r.reconcileDeployment(context.Background(), glance, configArtifacts{}, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(glance.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentWaitingBackends))

	// No Deployment was created.
	var deploy appsv1.Deployment
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"}, &deploy)
	g.Expect(err).To(HaveOccurred(), "invalid projection with no live Deployment must not create one")
}

func TestReconcileDeployment_InvalidProjectionLiveDeploymentReAppliesLastGood(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1")
	// A live Deployment exists; the config step recovered its last-good names.
	lastGood := configArtifacts{configMapName: "glance-config-lastgood", backendsSecretName: "glance-backends-lastgood"}
	r := newGlanceTestReconciler(glance, readyGlanceDeployment(glance, lastGood))

	_, err := r.reconcileDeployment(context.Background(), glance, lastGood, "", "")
	g.Expect(err).NotTo(HaveOccurred())

	var deploy appsv1.Deployment
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"}, &deploy)).To(Succeed())
	var configVol *corev1.Volume
	for i := range deploy.Spec.Template.Spec.Volumes {
		if deploy.Spec.Template.Spec.Volumes[i].Name == configVolumeName {
			configVol = &deploy.Spec.Template.Spec.Volumes[i]
		}
	}
	g.Expect(configVol).NotTo(BeNil())
	g.Expect(configVol.ConfigMap.Name).To(Equal("glance-config-lastgood"),
		"an invalid projection with a live Deployment keeps the last-good config pinned")
}

// TestReconcileDeployment_RollingUpdateFlipsToContracting verifies that when the
// Deployment becomes ready during the RollingUpdate upgrade phase,
// reconcileDeployment advances the phase to Contracting, emits
// DeploymentRolloutComplete, sets DeploymentReady=True, and requeues immediately
// without stamping the endpoint (keystone parity).
func TestReconcileDeployment_RollingUpdateFlipsToContracting(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := upgradingGlance(commonv1.UpgradePhaseRollingUpdate)
	glance.Spec.Deployment.Replicas = 2
	art := testArtifacts()
	r := newGlanceTestReconciler(glance, readyGlanceDeployment(glance, art))

	res, err := r.reconcileDeployment(context.Background(), glance, art, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{Requeue: true}))

	g.Expect(glance.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseContracting))
	// The endpoint is deliberately NOT stamped on the flip pass.
	g.Expect(glance.Status.Endpoint).To(BeEmpty())

	cond := meta.FindStatusCondition(glance.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentReady))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Normal DeploymentRolloutComplete")))
}

// TestReconcileDeployment_RollingUpdateSurgeReadyHoldsContract verifies the
// rollout-convergence gate: during the RollingUpdate upgrade phase a merely
// surge-ready Deployment — Available with ReadyReplicas at the desired count but
// not every replica updated (an old-image pod still serving under the default
// MaxSurge=1/MaxUnavailable=0 strategy) — MUST NOT advance to Contracting, because
// the contract phase would drop columns that old pod still reads. The phase stays
// RollingUpdate and the reconcile requeues to wait for full convergence. Regression
// guard: with the surge-tolerant IsDeploymentReady gate this flipped prematurely.
func TestReconcileDeployment_RollingUpdateSurgeReadyHoldsContract(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := upgradingGlance(commonv1.UpgradePhaseRollingUpdate)
	glance.Spec.Deployment.Replicas = 2
	art := testArtifacts()

	// Surge-ready but not converged: Available=True and ReadyReplicas at the
	// desired count (so IsDeploymentReady is true) while UpdatedReplicas lags and a
	// surge pod is still counted — the exact window IsDeploymentReady tolerates.
	deploy := buildGlanceDeployment(glance, art, "", "")
	replicas := int32(glance.Spec.Deployment.Replicas)
	deploy.Spec.Replicas = &replicas
	deploy.Generation = 1
	deploy.Status.ObservedGeneration = 1
	deploy.Status.ReadyReplicas = replicas
	deploy.Status.UpdatedReplicas = replicas - 1
	deploy.Status.Replicas = replicas + 1
	deploy.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
	}
	r := newGlanceTestReconciler(glance, deploy)

	res, err := r.reconcileDeployment(context.Background(), glance, art, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	// Holds in RollingUpdate and requeues to wait for the old image to drain.
	g.Expect(res.RequeueAfter).To(Equal(RequeueDeploymentPolling))
	g.Expect(glance.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseRollingUpdate),
		"a surge-ready-but-not-converged Deployment must not flip to Contracting")

	cond := meta.FindStatusCondition(glance.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDeployment))

	// No premature rollout-complete event while an old-image pod still serves.
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		NotTo(ContainElement(ContainSubstring("DeploymentRolloutComplete")))
}

// TestReconcileDeployment_EmptyPhaseNoFlipStampsEndpoint verifies that with no
// active upgrade (empty phase) the ready path stamps the endpoint and does NOT
// fire the RollingUpdate flip (no DeploymentRolloutComplete event).
func TestReconcileDeployment_EmptyPhaseNoFlipStampsEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1") // empty UpgradePhase
	art := testArtifacts()
	r := newGlanceTestReconciler(glance, readyGlanceDeployment(glance, art))

	res, err := r.reconcileDeployment(context.Background(), glance, art, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	g.Expect(glance.Status.UpgradePhase).To(BeEmpty())
	g.Expect(glance.Status.Endpoint).To(Equal("http://test-glance.default.svc.cluster.local:9292/"))

	cond := meta.FindStatusCondition(glance.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentReady))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		NotTo(ContainElement(ContainSubstring("DeploymentRolloutComplete")))
}
