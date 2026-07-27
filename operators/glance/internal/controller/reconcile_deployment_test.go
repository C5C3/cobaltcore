// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

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
	"github.com/c5c3/forge/internal/common/deployment"
	"github.com/c5c3/forge/internal/common/keystoneauth"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
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

// TestBuildGlanceDeployment_ImageCacheVolumeAndSidecar pins the whole opt-in
// shape: the bounded cache emptyDir, its read-write mount in the API container,
// and the maintenance sidecar that shares it. The sidecar is deliberately
// spartan — same image, no probes, no env, and only the two mounts its CLIs
// need — so the assertions below also pin what it must NOT grow.
func TestBuildGlanceDeployment_ImageCacheVolumeAndSidecar(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1")
	glance.Spec.ImageCache = &glancev1alpha1.ImageCacheSpec{SizeLimit: ptr.To(resource.MustParse("256Mi"))}
	deploy := buildGlanceDeployment(glance, testArtifacts(), "", "")
	podSpec := deploy.Spec.Template.Spec

	volumesByName := map[string]corev1.Volume{}
	for _, v := range podSpec.Volumes {
		volumesByName[v.Name] = v
	}
	g.Expect(volumesByName).To(HaveKey(imageCacheVolumeName))
	cacheVolume := volumesByName[imageCacheVolumeName].EmptyDir
	g.Expect(cacheVolume).NotTo(BeNil())
	g.Expect(cacheVolume.SizeLimit).NotTo(BeNil())
	// Quantity.DeepCopy drops the cached string form, so compare the canonical
	// string rather than the struct.
	g.Expect(cacheVolume.SizeLimit.String()).To(Equal("256Mi"))
	// The volume must carry its OWN Quantity: sharing the CR's pointer would let
	// a later mutation of either move the value under both.
	g.Expect(cacheVolume.SizeLimit).NotTo(BeIdenticalTo(glance.Spec.ImageCache.SizeLimit),
		"the cache emptyDir must not alias the spec's Quantity")

	// The API container writes the images it caches, so its mount is read-write.
	apiMounts := map[string]corev1.VolumeMount{}
	for _, m := range podSpec.Containers[0].VolumeMounts {
		apiMounts[m.Name] = m
	}
	g.Expect(apiMounts).To(HaveKey(imageCacheVolumeName))
	g.Expect(apiMounts[imageCacheVolumeName].MountPath).To(Equal(glanceImageCachePath))
	g.Expect(apiMounts[imageCacheVolumeName].ReadOnly).To(BeFalse())

	g.Expect(podSpec.Containers).To(HaveLen(2))
	sidecar := podSpec.Containers[1]
	g.Expect(sidecar.Name).To(Equal("cache-maintenance"))
	g.Expect(sidecar.Image).To(Equal(podSpec.Containers[0].Image),
		"the maintenance CLIs ship in the same image as the API")
	g.Expect(sidecar.SecurityContext).To(Equal(deployment.RestrictedSecurityContext()))
	// No probes: a probe on the maintenance loop would put this API replica's
	// Service membership behind the health of the cache it maintains.
	g.Expect(sidecar.LivenessProbe).To(BeNil())
	g.Expect(sidecar.ReadinessProbe).To(BeNil())
	g.Expect(sidecar.StartupProbe).To(BeNil())
	// No env: the CLIs open neither the database nor glance_store.
	g.Expect(sidecar.Env).To(BeEmpty())
	// Requests on BOTH resources are mandatory, not cosmetic: the HPA emits a
	// pod-scoped Resource metric, and kube-controller-manager reports
	// FailedGetResourceMetric for the whole pod as soon as one container lacks a
	// request — silently freezing autoscaling on every CR that enables both
	// spec.autoscaling and spec.imageCache. A namespace ResourceQuota on
	// requests.cpu/requests.memory would reject the pod outright, and a
	// requests==limits API container would drop from Guaranteed to Burstable.
	g.Expect(sidecar.Resources.Requests).To(HaveKey(corev1.ResourceCPU))
	g.Expect(sidecar.Resources.Requests).To(HaveKey(corev1.ResourceMemory))
	g.Expect(sidecar.Resources.Requests.Cpu().IsZero()).To(BeFalse())
	g.Expect(sidecar.Resources.Requests.Memory().IsZero()).To(BeFalse())
	g.Expect(sidecar.Resources.Limits).To(HaveKey(corev1.ResourceMemory),
		"an unbounded poll loop needs a memory limit so it cannot starve the API container")

	sidecarMounts := map[string]corev1.VolumeMount{}
	for _, m := range sidecar.VolumeMounts {
		sidecarMounts[m.Name] = m
	}
	g.Expect(sidecar.VolumeMounts).To(HaveLen(2),
		"the sidecar mounts the config and the cache and nothing else — no backends Secret")
	g.Expect(sidecarMounts).To(HaveKey(configVolumeName))
	g.Expect(sidecarMounts[configVolumeName].MountPath).To(Equal(glanceConfigDir))
	g.Expect(sidecarMounts[configVolumeName].ReadOnly).To(BeTrue())
	g.Expect(sidecarMounts).To(HaveKey(imageCacheVolumeName))
	g.Expect(sidecarMounts[imageCacheVolumeName].MountPath).To(Equal(glanceImageCachePath))
	g.Expect(sidecarMounts[imageCacheVolumeName].ReadOnly).To(BeFalse(),
		"the pruner deletes cache entries, so its mount cannot be read-only")
}

// TestCacheMaintenanceContainer_Loop pins the two triggers the loop runs on,
// what its size poll measures, and that no failure of it can end the loop.
//
// The high-water mark is the load-bearing one: glance consults
// image_cache_max_size in the pruner alone, never in the API's cache filter, so
// a loop that only fired every maintenanceInterval would let concurrent
// downloads pile into the emptyDir between two passes, cross the bound the
// kubelet enforces, and get the pod evicted. Deriving it from the same
// imageCacheMaxSizeBytes the config renders keeps the sidecar and glance
// agreeing on when the cache is over its target.
func TestCacheMaintenanceContainer_Loop(t *testing.T) {
	tests := []struct {
		name          string
		imageCache    *glancev1alpha1.ImageCacheSpec
		wantInterval  string
		wantHighWater string
	}{
		{
			// The empty block is the documented opt-in shape, so BOTH defaults have
			// to resolve here — 5m and 10Gi, the latter giving 8589934592/1024 KiB.
			name:          "empty block resolves both defaults",
			imageCache:    &glancev1alpha1.ImageCacheSpec{},
			wantInterval:  "interval=300",
			wantHighWater: "high_water_kib=8388608",
		},
		{
			name: "explicit interval and size are honored",
			imageCache: &glancev1alpha1.ImageCacheSpec{
				SizeLimit:           ptr.To(resource.MustParse("256Mi")),
				MaintenanceInterval: &metav1.Duration{Duration: time.Minute},
			},
			wantInterval: "interval=60",
			// 214748360 bytes / 1024, the same threshold the rendered
			// image_cache_max_size carries.
			wantHighWater: "high_water_kib=209715",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			glance := deployGlance("2026.1")
			glance.Spec.ImageCache = tc.imageCache

			container := cacheMaintenanceContainer(glance, effectiveImageCache(glance.Spec.ImageCache))
			g.Expect(container.Command).To(HaveLen(4))
			g.Expect(container.Command[:3]).To(Equal([]string{"/bin/sh", "-eu", "-c"}))
			script := container.Command[3]

			g.Expect(script).To(ContainSubstring(tc.wantInterval + "\n"))
			g.Expect(script).To(ContainSubstring(tc.wantHighWater + "\n"))
			// The poll has to be shorter than the shortest admissible interval, or
			// the size trigger never fires before the clock one does.
			g.Expect(script).To(ContainSubstring("poll=30\n"))
			g.Expect(int64(cacheMaintenancePollInterval)).To(BeNumerically("<", int64(time.Minute)))
			// The poll must measure what the pruner can reclaim. incomplete/ and
			// invalid/ hold entries only glance-cache-cleaner may touch, and only
			// after image_cache_stall_time (a day) — counting them would latch the
			// size trigger on for that long over bytes no prune could release, so
			// the loop would fork both CLIs every poll to no effect.
			g.Expect(script).To(ContainSubstring(
				`used_kib=$(du -sk --exclude=incomplete --exclude=invalid --exclude=queue "$dir" | cut -f1)`,
			))
			// A measurement that yields nothing must NOT read as an empty cache:
			// that fails open, silently disabling the size trigger and dropping the
			// loop back to the time-driven behaviour the poll exists to replace.
			// du's stderr stays visible for the same reason.
			g.Expect(script).NotTo(ContainSubstring("2>/dev/null"))
			g.Expect(script).To(ContainSubstring(`if [ -z "$used_kib" ]; then`))
			g.Expect(script).To(ContainSubstring("used_kib=$high_water_kib"))
			g.Expect(script).NotTo(ContainSubstring("${used_kib:-0}"),
				"an unmeasured cache must be handled explicitly, not defaulted to 0")
			g.Expect(script).To(ContainSubstring("dir=" + glanceImageCachePath + "\n"))
			g.Expect(script).To(ContainSubstring("conf=" + glanceConfigDir + "\n"))

			// Both commands run on every pass, and neither aborts the loop on its
			// own: under `sh -e` an unguarded non-zero exit would end it outright,
			// turning one failed pruner run into a NotReady pod.
			g.Expect(script).To(ContainSubstring(`glance-cache-pruner --config-dir "$conf" || rc=1`))
			g.Expect(script).To(ContainSubstring(`glance-cache-cleaner --config-dir "$conf" || rc=1`))

			// No failure may end the loop. The sidecar shares the pod with
			// glance-api, so an exiting loop is a CrashLoopBackOff, a NotReady pod
			// and an API replica dropped from the Service — and a broken pruner is a
			// property of the image and the shared config, so it would take every
			// replica out at once.
			g.Expect(script).NotTo(ContainSubstring("exit "),
				"a cache-maintenance failure must never pull the API replica out of the Service")

			// Absorbing the failure makes the log the ONLY signal there is: no
			// Event, no condition, no metric, and a container that stays Running
			// throughout. A permanently broken pruner ends in every replica being
			// evicted off its emptyDir bound, so the line an alert has to key on
			// must be a stable token instead of prose, and the recovery line is
			// what lets that alert close again. Both go to stderr so they stay
			// apart from the pruner's and cleaner's own output on stdout.
			g.Expect(script).To(ContainSubstring(
				`echo "glance-cache-maintenance failed: $failures consecutive, ` +
					`cache at $used_kib KiB of $high_water_kib" >&2`,
			))
			g.Expect(script).To(ContainSubstring(
				`echo "glance-cache-maintenance recovered: after $failures consecutive failures" >&2`,
			))
			g.Expect(script).To(ContainSubstring(`if [ "$failures" -ne 0 ]; then`),
				"the recovery line must be conditional, or every quiet pass logs one")
			g.Expect(script).To(ContainSubstring(
				`echo "glance-cache-maintenance unmeasured: could not measure $dir; running a pass anyway" >&2`,
			))
		})
	}
}

// TestBuildGlanceDeployment_ImageCacheEmptyBlockDefaults builds the Deployment
// from the documented opt-in shape — `imageCache: {}` — rather than from a CR
// that spells sizeLimit out. Every other build-side case sets the field, so the
// default would only ever be exercised through cacheMaintenanceContainer; if the
// volume builder ever read glance.Spec.ImageCache.SizeLimit instead of the
// resolved copy, that nil would panic the operator with no unit test turning
// red, and the e2e suite always sets 256Mi so it does not backstop this either.
func TestBuildGlanceDeployment_ImageCacheEmptyBlockDefaults(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1")
	glance.Spec.ImageCache = &glancev1alpha1.ImageCacheSpec{}
	podSpec := buildGlanceDeployment(glance, testArtifacts(), "", "").Spec.Template.Spec

	var cacheVolume *corev1.EmptyDirVolumeSource
	for _, v := range podSpec.Volumes {
		if v.Name == imageCacheVolumeName {
			cacheVolume = v.EmptyDir
		}
	}
	g.Expect(cacheVolume).NotTo(BeNil(), "an empty block is the opt-in, so the volume must be mounted")
	g.Expect(cacheVolume.SizeLimit).NotTo(BeNil(),
		"the resolved default must reach the emptyDir; an unbounded cache volume defeats the feature")
	wantDefault := glancev1alpha1.DefaultImageCacheSizeLimit()
	g.Expect(cacheVolume.SizeLimit.String()).To(Equal(wantDefault.String()))

	g.Expect(podSpec.Containers).To(HaveLen(2))
	g.Expect(podSpec.Containers[1].Name).To(Equal("cache-maintenance"))

	// Nothing was written back into the CR: the unset field keeps tracking the
	// operator default across upgrades.
	g.Expect(glance.Spec.ImageCache.SizeLimit).To(BeNil())
	g.Expect(glance.Spec.ImageCache.MaintenanceInterval).To(BeNil())
}

// TestBuildGlanceDeployment_ImageCacheDisabledUnchanged pins the pod template a
// Glance without spec.imageCache must keep producing: one container and exactly
// the pre-feature volume set. It is the guard against a later change that mounts
// the cache or starts the sidecar unconditionally.
func TestBuildGlanceDeployment_ImageCacheDisabledUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := deployGlance("2026.1")
	glance.Spec.ImageCache = nil
	podSpec := buildGlanceDeployment(glance, testArtifacts(), "", "").Spec.Template.Spec

	g.Expect(podSpec.Containers).To(HaveLen(1))
	g.Expect(podSpec.Containers[0].Name).To(Equal("glance-api"))

	volumeNames := []string{}
	for _, v := range podSpec.Volumes {
		volumeNames = append(volumeNames, v.Name)
	}
	g.Expect(volumeNames).To(Equal([]string{
		configVolumeName, backendsVolumeName, stagingVolumeName, tasksVolumeName,
	}), "the disabled-cache pod template must carry exactly the pre-feature volumes")

	for _, m := range podSpec.Containers[0].VolumeMounts {
		g.Expect(m.Name).NotTo(Equal(imageCacheVolumeName))
	}
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
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
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
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
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
