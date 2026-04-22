// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0013

func deployTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = policyv1.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return s
}

func deployTestKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			UID:        "ks-uid",
			Generation: 1,
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db-credentials"},
			},
			Cache: commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: keystonev1alpha1.DefaultMemoryRequest.DeepCopy(),
					corev1.ResourceCPU:    keystonev1alpha1.DefaultCPURequest.DeepCopy(),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: keystonev1alpha1.DefaultMemoryLimit.DeepCopy(),
					corev1.ResourceCPU:    keystonev1alpha1.DefaultCPULimit.DeepCopy(),
				},
			},
		},
	}
}

func newDeployTestReconciler(s *runtime.Scheme, objs ...client.Object) *KeystoneReconciler {
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{})
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

// readyDeployment returns a Deployment that matches what buildKeystoneDeployment
// would produce, but with status indicating it is available and ready.
func readyDeployment(ks *keystonev1alpha1.Keystone, configMapName string) *appsv1.Deployment {
	deploy := buildKeystoneDeployment(ks, configMapName)
	replicas := int32(ks.Spec.Replicas)
	deploy.Spec.Replicas = &replicas
	deploy.Generation = 1
	deploy.Status.ObservedGeneration = 1
	deploy.Status.ReadyReplicas = replicas
	deploy.Status.Conditions = []appsv1.DeploymentCondition{
		{
			Type:   appsv1.DeploymentAvailable,
			Status: corev1.ConditionTrue,
		},
	}
	return deploy
}

// notReadyDeployment returns a Deployment that exists but is not yet available.
func notReadyDeployment(ks *keystonev1alpha1.Keystone, configMapName string) *appsv1.Deployment {
	deploy := buildKeystoneDeployment(ks, configMapName)
	deploy.Generation = 1
	deploy.Status.ObservedGeneration = 1
	deploy.Status.ReadyReplicas = 0
	return deploy
}

func TestReconcileDeployment_DeploymentAndServiceCreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	r := newDeployTestReconciler(s, ks)

	result, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	// Deployment just created, not ready yet — should requeue.
	g.Expect(result.RequeueAfter).To(Equal(RequeueDeploymentPolling))

	// Verify Deployment was created.
	var deploy appsv1.Deployment
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &deploy)).To(Succeed())

	// Verify Service was created.
	var svc corev1.Service
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &svc)).To(Succeed())
}

func TestReconcileDeployment_NotReady_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	deploy := notReadyDeployment(ks, "keystone-config-abc123")
	r := newDeployTestReconciler(s, ks, deploy)

	result, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDeploymentPolling))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDeployment"))
}

func TestReconcileDeployment_Ready_SetsEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	deploy := readyDeployment(ks, "keystone-config-abc123")
	r := newDeployTestReconciler(s, ks, deploy)

	result, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("DeploymentReady"))

	g.Expect(ks.Status.Endpoint).To(Equal("http://test-keystone-api.default.svc.cluster.local:5000/v3"))
}

func TestReconcileDeployment_OwnerReferences(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	r := newDeployTestReconciler(s, ks)

	_, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// Verify Deployment has owner reference.
	var deploy appsv1.Deployment
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &deploy)).To(Succeed())
	g.Expect(deploy.OwnerReferences).To(HaveLen(1))
	g.Expect(deploy.OwnerReferences[0].Name).To(Equal("test-keystone"))

	// Verify Service has owner reference.
	var svc corev1.Service
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &svc)).To(Succeed())
	g.Expect(svc.OwnerReferences).To(HaveLen(1))
	g.Expect(svc.OwnerReferences[0].Name).To(Equal("test-keystone"))
}

func TestReconcileDeployment_DeploymentSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	r := newDeployTestReconciler(s, ks)

	_, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var deploy appsv1.Deployment
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &deploy)).To(Succeed())

	// Verify replicas.
	g.Expect(deploy.Spec.Replicas).NotTo(BeNil())
	g.Expect(*deploy.Spec.Replicas).To(Equal(int32(3)))

	// Verify container spec.
	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
	container := deploy.Spec.Template.Spec.Containers[0]
	g.Expect(container.Name).To(Equal("keystone-api"))
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/keystone:2025.2"))

	// Verify port.
	g.Expect(container.Ports).To(HaveLen(1))
	g.Expect(container.Ports[0].ContainerPort).To(Equal(int32(5000)))
	g.Expect(container.Ports[0].Name).To(Equal("keystone-api"))

	// Verify liveness probe uses TCPSocket (CC-0062): a TCP-only check ensures
	// the uWSGI process is alive without exercising the database code path,
	// preventing unnecessary pod restarts during transient DB outages.
	g.Expect(container.LivenessProbe).NotTo(BeNil())
	g.Expect(container.LivenessProbe.TCPSocket).NotTo(BeNil(), "liveness probe must use TCPSocket")
	g.Expect(container.LivenessProbe.TCPSocket.Port.IntValue()).To(Equal(5000))
	g.Expect(container.LivenessProbe.HTTPGet).To(BeNil(), "liveness probe must not use HTTPGet")
	g.Expect(container.LivenessProbe.InitialDelaySeconds).To(Equal(int32(15)))
	g.Expect(container.LivenessProbe.PeriodSeconds).To(Equal(int32(20)))

	// Verify readiness probe.
	g.Expect(container.ReadinessProbe).NotTo(BeNil())
	g.Expect(container.ReadinessProbe.HTTPGet).NotTo(BeNil())
	g.Expect(container.ReadinessProbe.HTTPGet.Path).To(Equal("/v3"))
	g.Expect(container.ReadinessProbe.HTTPGet.Port.IntValue()).To(Equal(5000))
	g.Expect(container.ReadinessProbe.InitialDelaySeconds).To(Equal(int32(5)))
	g.Expect(container.ReadinessProbe.PeriodSeconds).To(Equal(int32(10)))

	// Verify startup probe (CC-0063, REQ-003): httpGet /v3 port 5000 with generous
	// failure threshold to survive slow cold starts (large DB, cold caches).
	g.Expect(container.StartupProbe).NotTo(BeNil())
	g.Expect(container.StartupProbe.HTTPGet).NotTo(BeNil())
	g.Expect(container.StartupProbe.HTTPGet.Path).To(Equal("/v3"))
	g.Expect(container.StartupProbe.HTTPGet.Port.IntValue()).To(Equal(5000))
	g.Expect(container.StartupProbe.FailureThreshold).To(Equal(int32(30)))
	g.Expect(container.StartupProbe.PeriodSeconds).To(Equal(int32(10)))

	// Verify preStop lifecycle hook (CC-0063, REQ-001): 5-second sleep before
	// SIGTERM gives kube-proxy time to propagate endpoint removal.
	g.Expect(container.Lifecycle).NotTo(BeNil())
	g.Expect(container.Lifecycle.PreStop).NotTo(BeNil())
	g.Expect(container.Lifecycle.PreStop.Exec).NotTo(BeNil())
	g.Expect(container.Lifecycle.PreStop.Exec.Command).To(Equal([]string{"/bin/sh", "-c", "sleep 5"}))
	g.Expect(container.Lifecycle.PreStop.HTTPGet).To(BeNil(), "preStop must use exec, not httpGet")

	// Verify terminationGracePeriodSeconds (CC-0063, REQ-002): 30s gives 5s for
	// preStop sleep + 25s for uWSGI to drain in-flight requests.
	g.Expect(deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).NotTo(BeNil())
	g.Expect(*deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).To(Equal(int64(30)))

	// Verify volume mounts.
	g.Expect(container.VolumeMounts).To(HaveLen(3))
	var configMount, fernetMount, credentialMount corev1.VolumeMount
	for _, vm := range container.VolumeMounts {
		switch vm.Name {
		case "config":
			configMount = vm
		case "fernet-keys":
			fernetMount = vm
		case "credential-keys":
			credentialMount = vm
		}
	}
	g.Expect(configMount.MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(configMount.ReadOnly).To(BeTrue())
	g.Expect(fernetMount.MountPath).To(Equal("/etc/keystone/fernet-keys/"))
	g.Expect(fernetMount.ReadOnly).To(BeTrue())
	g.Expect(credentialMount.MountPath).To(Equal("/etc/keystone/credential-keys/"))
	g.Expect(credentialMount.ReadOnly).To(BeTrue())

	// Verify SecurityContext satisfies PSS Restricted profile (CC-0045).
	expectRestrictedSecurityContext(g, &container)

	// Verify volumes.
	g.Expect(deploy.Spec.Template.Spec.Volumes).To(HaveLen(3))
	var configVol, fernetVol, credentialVol corev1.Volume
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		switch v.Name {
		case "config":
			configVol = v
		case "fernet-keys":
			fernetVol = v
		case "credential-keys":
			credentialVol = v
		}
	}
	g.Expect(configVol.ConfigMap).NotTo(BeNil())
	g.Expect(configVol.ConfigMap.Name).To(Equal("keystone-config-abc123"))
	g.Expect(fernetVol.Secret).NotTo(BeNil())
	g.Expect(fernetVol.Secret.SecretName).To(Equal("test-keystone-fernet-keys"))
	g.Expect(credentialVol.Secret).NotTo(BeNil())
	g.Expect(credentialVol.Secret.SecretName).To(Equal("test-keystone-credential-keys"))
	g.Expect(credentialVol.Secret.Optional).To(BeNil(), "credential-keys volume should not be optional now that credential key management is implemented (CC-0036)")

	// Verify labels on Deployment ObjectMeta, pod template, and selector.
	g.Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))
	g.Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(deploy.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(deploy.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))

	// Verify Service spec.
	var svc corev1.Service
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &svc)).To(Succeed())
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(5000)))
	g.Expect(svc.Spec.Ports[0].TargetPort.IntValue()).To(Equal(5000))
	g.Expect(svc.Spec.Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
	g.Expect(svc.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(svc.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(svc.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
}

func TestReconcileDeployment_NotReady_ConditionMessageAndGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	deploy := notReadyDeployment(ks, "keystone-config-abc123")
	r := newDeployTestReconciler(s, ks, deploy)

	result, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDeploymentPolling))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDeployment"))
	g.Expect(cond.Message).To(Equal("Keystone API deployment is not yet available"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

func TestReconcileDeployment_Ready_ConditionMessageAndGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	deploy := readyDeployment(ks, "keystone-config-abc123")
	r := newDeployTestReconciler(s, ks, deploy)

	result, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("DeploymentReady"))
	g.Expect(cond.Message).To(Equal("Keystone API deployment is available"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

func TestReconcileDeployment_ServiceCreatedAlongsideDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	// Only pre-create the Keystone CR, not the Deployment or Service.
	r := newDeployTestReconciler(s, ks)

	_, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// Verify both Deployment and Service exist after a single reconcile call.
	var deploy appsv1.Deployment
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &deploy)).To(Succeed())

	var svc corev1.Service
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &svc)).To(Succeed())

	// Verify the Service targets the Deployment's pods.
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))

	// Both should have owner references.
	g.Expect(deploy.OwnerReferences).To(HaveLen(1))
	g.Expect(svc.OwnerReferences).To(HaveLen(1))
}

// Feature: CC-0074

// TestBuildKeystoneDeployment_StablePodTemplate verifies that two calls to
// buildKeystoneDeployment with the same Keystone CR and configMapName return
// Deployments with deeply equal Spec.Template fields. This asserts that
// buildKeystoneDeployment is deterministic for identical inputs and that no
// new fields (e.g., hashes or timestamps) are added to the pod template that
// could cause spurious rollouts (CC-0074). It does not exercise scenarios
// with differing Secret contents as described in REQ-002; those must be
// covered by higher-level reconciliation tests.
func TestBuildKeystoneDeployment_StablePodTemplate(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	deploy1 := buildKeystoneDeployment(ks, "keystone-config-abc123")
	deploy2 := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy1.Spec.Template).To(Equal(deploy2.Spec.Template),
		"pod template must be stable across invocations (CC-0074)")
}

// TestBuildKeystoneDeployment_VolumesMaintained verifies that buildKeystoneDeployment
// returns a Deployment with config, fernet-keys, and credential-keys volumes and
// matching volume mounts at the expected paths, confirming that these mounts
// survive hash removal without relying on a fixed volume or container count
// (CC-0074, REQ-003).
func TestBuildKeystoneDeployment_VolumesMaintained(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	// Verify expected volumes are present.
	g.Expect(deploy.Spec.Template.Spec.Volumes).NotTo(BeEmpty())
	volumeMap := make(map[string]bool, len(deploy.Spec.Template.Spec.Volumes))
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		volumeMap[v.Name] = true
	}
	g.Expect(volumeMap).To(HaveKey("config"))
	g.Expect(volumeMap).To(HaveKey("fernet-keys"))
	g.Expect(volumeMap).To(HaveKey("credential-keys"))

	// Verify expected volume mounts at correct paths (name-based lookup to avoid
	// brittleness if sidecars are added in the future).
	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone-api")
	g.Expect(container).NotTo(BeNil(), "keystone-api container must exist")
	mounts := container.VolumeMounts
	g.Expect(mounts).NotTo(BeEmpty(), "keystone-api container must have volume mounts")
	mountPaths := make(map[string]string, len(mounts))
	for _, m := range mounts {
		mountPaths[m.Name] = m.MountPath
	}
	g.Expect(mountPaths).To(HaveKeyWithValue("config", "/etc/keystone/keystone.conf.d/"))
	g.Expect(mountPaths).To(HaveKeyWithValue("fernet-keys", "/etc/keystone/fernet-keys/"))
	g.Expect(mountPaths).To(HaveKeyWithValue("credential-keys", "/etc/keystone/credential-keys/"))
}

// Feature: CC-0080

// TestBuildDBConnectionEnvVar verifies that the helper emits an EnvVar named
// OS_DATABASE__CONNECTION sourcing its value from the derived
// <keystone.Name>-db-connection Secret's "connection" key (CC-0080, REQ-003).
func TestBuildDBConnectionEnvVar(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	env := buildDBConnectionEnvVar(ks)

	g.Expect(env.Name).To(Equal("OS_DATABASE__CONNECTION"))
	g.Expect(env.Value).To(BeEmpty(),
		"value must come from a SecretKeyRef, never an inline plaintext string")
	g.Expect(env.ValueFrom).NotTo(BeNil())
	g.Expect(env.ValueFrom.SecretKeyRef).NotTo(BeNil())
	g.Expect(env.ValueFrom.SecretKeyRef.Name).To(Equal("test-keystone-db-connection"))
	g.Expect(env.ValueFrom.SecretKeyRef.Key).To(Equal(dbConnectionSecretKey))
}

// TestBuildKeystoneDeployment_DBConnectionEnv verifies that the keystone-api
// container has the OS_DATABASE__CONNECTION env var wired to the derived
// connection Secret so oslo.config overrides the [database] connection value
// (CC-0080, REQ-003, REQ-007). Volumes and mounts must remain unchanged.
func TestBuildKeystoneDeployment_DBConnectionEnv(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone-api")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Env).To(ContainElement(buildDBConnectionEnvVar(ks)),
		"keystone-api must consume DB connection via OS_DATABASE__CONNECTION (CC-0080, REQ-003)")

	// Volumes/mounts must remain unchanged (REQ-007).
	volumeNames := make([]string, 0, len(deploy.Spec.Template.Spec.Volumes))
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		volumeNames = append(volumeNames, v.Name)
	}
	g.Expect(volumeNames).To(ConsistOf("config", "fernet-keys", "credential-keys"))
}

// TestReconcileDeployment_NoSecretReadRequired verifies that reconcileDeployment
// succeeds and creates a Deployment even when fernet-keys and credential-keys
// Secrets do not exist (CC-0074, REQ-001).
func TestReconcileDeployment_NoSecretReadRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()

	// Track whether any Secret Get calls are made.
	secretGetCalled := false
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					secretGetCalled = true
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDeploymentPolling))
	g.Expect(secretGetCalled).To(BeFalse(),
		"reconcileDeployment must not read Secrets after hash removal (CC-0074)")
}

// Feature: CC-0037

func TestReconcileDeployment_PDBCreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	ks.Spec.Replicas = 3 // explicit: PDB expectations depend on this value (CC-0037)
	r := newDeployTestReconciler(s, ks)

	_, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var pdb policyv1.PodDisruptionBudget
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &pdb)).To(Succeed())

	g.Expect(pdb.OwnerReferences).To(HaveLen(1))
	g.Expect(pdb.OwnerReferences[0].Name).To(Equal("test-keystone"))
}

func TestReconcileDeployment_PDBLabelsAndSelector(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	r := newDeployTestReconciler(s, ks)

	_, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var pdb policyv1.PodDisruptionBudget
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &pdb)).To(Succeed())

	// PDB labels match commonLabels (CC-0037).
	g.Expect(pdb.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(pdb.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(pdb.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))

	// PDB selector matches selectorLabels (CC-0037).
	g.Expect(pdb.Spec.Selector).NotTo(BeNil())
	g.Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
}

func TestReconcileDeployment_PDBMinAvailableForMultipleReplicas(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	ks.Spec.Replicas = 3 // explicit: PDB expectations depend on this value (CC-0037)
	r := newDeployTestReconciler(s, ks)

	_, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var pdb policyv1.PodDisruptionBudget
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &pdb)).To(Succeed())

	g.Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
	g.Expect(*pdb.Spec.MinAvailable).To(Equal(intstr.FromInt32(1)))
	g.Expect(pdb.Spec.MaxUnavailable).To(BeNil())
}

func TestReconcileDeployment_PDBMaxUnavailableForSingleReplica(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	ks.Spec.Replicas = 1
	r := newDeployTestReconciler(s, ks)

	_, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var pdb policyv1.PodDisruptionBudget
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &pdb)).To(Succeed())

	g.Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
	g.Expect(*pdb.Spec.MaxUnavailable).To(Equal(intstr.FromInt32(1)))
	g.Expect(pdb.Spec.MinAvailable).To(BeNil())
}

func TestReconcileDeployment_PDBUpdatedOnReplicaChange(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	ks.Spec.Replicas = 3 // explicit: PDB expectations depend on this value (CC-0037)
	r := newDeployTestReconciler(s, ks)

	ctx := context.Background()

	// First reconcile with replicas=3 → minAvailable=1.
	_, err := r.reconcileDeployment(ctx, ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var pdb policyv1.PodDisruptionBudget
	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &pdb)).To(Succeed())
	g.Expect(pdb.Spec.MinAvailable).NotTo(BeNil())

	// Change to replicas=1 and re-reconcile → maxUnavailable=1.
	ks.Spec.Replicas = 1
	_, err = r.reconcileDeployment(ctx, ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &pdb)).To(Succeed())
	g.Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
	g.Expect(*pdb.Spec.MaxUnavailable).To(Equal(intstr.FromInt32(1)))
	g.Expect(pdb.Spec.MinAvailable).To(BeNil())
}

func TestReconcileDeployment_PDBSelectorMatchesDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	r := newDeployTestReconciler(s, ks)

	_, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var deploy appsv1.Deployment
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &deploy)).To(Succeed())

	var pdb policyv1.PodDisruptionBudget
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone-api", Namespace: "default",
	}, &pdb)).To(Succeed())

	g.Expect(pdb.Spec.Selector.MatchLabels).To(Equal(deploy.Spec.Selector.MatchLabels))
}

func TestBuildPodDisruptionBudget_BoundaryReplicas2(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	ks.Spec.Replicas = 2

	pdb := buildPodDisruptionBudget(ks)

	g.Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
	g.Expect(*pdb.Spec.MinAvailable).To(Equal(intstr.FromInt32(1)))
	g.Expect(pdb.Spec.MaxUnavailable).To(BeNil())
}

func TestBuildPodDisruptionBudget_ZeroReplicas(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	ks.Spec.Replicas = 0

	pdb := buildPodDisruptionBudget(ks)

	// Zero replicas explicitly sets MaxUnavailable=1 for clarity (CC-0037).
	g.Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
	g.Expect(*pdb.Spec.MaxUnavailable).To(Equal(intstr.FromInt32(1)))
	g.Expect(pdb.Spec.MinAvailable).To(BeNil())
}

// Feature: CC-0042

func TestReconcileDeployment_ContainerResources(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
	container := deploy.Spec.Template.Spec.Containers[0]
	g.Expect(container.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, keystonev1alpha1.DefaultMemoryRequest))
	g.Expect(container.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, keystonev1alpha1.DefaultCPURequest))
	g.Expect(container.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, keystonev1alpha1.DefaultMemoryLimit))
	g.Expect(container.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, keystonev1alpha1.DefaultCPULimit))
}

func TestReconcileDeployment_CustomResources(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	ks.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceCPU:    resource.MustParse("200m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("2Gi"),
			corev1.ResourceCPU:    resource.MustParse("1"),
		},
	}

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
	container := deploy.Spec.Template.Spec.Containers[0]
	g.Expect(container.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("1Gi")))
	g.Expect(container.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("200m")))
	g.Expect(container.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("2Gi")))
	g.Expect(container.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("1")))
}

// TestReconcileDeployment_NilResources verifies the nil-safety fallback in
// containerResources(): when spec.Resources is nil (e.g. pre-existing CRs that
// bypassed the webhook), the container gets a zero-value ResourceRequirements
// instead of a nil-pointer panic (CC-0042).
func TestReconcileDeployment_NilResources(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	ks.Spec.Resources = nil

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
	container := deploy.Spec.Template.Spec.Containers[0]
	g.Expect(container.Resources).To(Equal(corev1.ResourceRequirements{}))
}

func TestReconcileDeployment_PDBEnsureError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	ks.Spec.Replicas = 3 // explicit: PDB expectations depend on this value (CC-0037)

	// Use an interceptor to inject an error when creating a PodDisruptionBudget (CC-0037).
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*policyv1.PodDisruptionBudget); ok {
					return fmt.Errorf("simulated PDB creation error")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ensuring PodDisruptionBudget"))
	g.Expect(err.Error()).To(ContainSubstring("simulated PDB creation error"))
}

// Feature: CC-0040

// TestUWSGICommand_NilUWSGI verifies that uwsgiCommand(nil) returns the command
// with hardcoded defaults: --processes 2 --threads 1 --http-keepalive (CC-0040, REQ-004).
func TestUWSGICommand_NilUWSGI(t *testing.T) {
	g := NewGomegaWithT(t)

	cmd := uwsgiCommand(nil)

	g.Expect(cmd).To(ContainElement("--processes"))
	g.Expect(cmd).To(ContainElement("2"))
	g.Expect(cmd).To(ContainElement("--threads"))
	g.Expect(cmd).To(ContainElement("--http-keepalive"))

	// Verify processes=2, threads=1 by checking positional pairs.
	processesIdx := indexOf(cmd, "--processes")
	g.Expect(processesIdx).NotTo(Equal(-1))
	g.Expect(cmd[processesIdx+1]).To(Equal("2"))

	threadsIdx := indexOf(cmd, "--threads")
	g.Expect(threadsIdx).NotTo(Equal(-1))
	g.Expect(cmd[threadsIdx+1]).To(Equal("1"))
}

// TestUWSGICommand_CustomValues verifies that uwsgiCommand with processes=4,
// threads=8 returns --processes 4 --threads 8 in the command (CC-0040, REQ-004).
func TestUWSGICommand_CustomValues(t *testing.T) {
	g := NewGomegaWithT(t)

	cmd := uwsgiCommand(&keystonev1alpha1.UWSGISpec{
		Processes:     4,
		Threads:       8,
		HTTPKeepAlive: true,
	})

	processesIdx := indexOf(cmd, "--processes")
	g.Expect(processesIdx).NotTo(Equal(-1))
	g.Expect(cmd[processesIdx+1]).To(Equal("4"))

	threadsIdx := indexOf(cmd, "--threads")
	g.Expect(threadsIdx).NotTo(Equal(-1))
	g.Expect(cmd[threadsIdx+1]).To(Equal("8"))

	g.Expect(cmd).To(ContainElement("--http-keepalive"))
}

// TestUWSGICommand_KeepAliveDisabled verifies that uwsgiCommand with
// httpKeepAlive=false omits --http-keepalive from the command (CC-0040, REQ-004).
func TestUWSGICommand_KeepAliveDisabled(t *testing.T) {
	g := NewGomegaWithT(t)

	cmd := uwsgiCommand(&keystonev1alpha1.UWSGISpec{
		Processes:     2,
		Threads:       2,
		HTTPKeepAlive: false,
	})

	g.Expect(cmd).NotTo(ContainElement("--http-keepalive"))
}

// TestUWSGICommand_KeepAliveEnabled verifies that uwsgiCommand with
// httpKeepAlive=true includes --http-keepalive in the command (CC-0040, REQ-004).
func TestUWSGICommand_KeepAliveEnabled(t *testing.T) {
	g := NewGomegaWithT(t)

	cmd := uwsgiCommand(&keystonev1alpha1.UWSGISpec{
		Processes:     2,
		Threads:       2,
		HTTPKeepAlive: true,
	})

	g.Expect(cmd).To(ContainElement("--http-keepalive"))
}

// TestUWSGICommand_FixedFlagsAlwaysPresent verifies that regardless of uwsgi
// config, the command always includes --http :5000, --wsgi-file, --master,
// --lazy-apps, --need-app, and --pyargv (CC-0040, REQ-004).
func TestUWSGICommand_FixedFlagsAlwaysPresent(t *testing.T) {
	g := NewGomegaWithT(t)

	configs := []*keystonev1alpha1.UWSGISpec{
		nil,
		{Processes: 4, Threads: 8, HTTPKeepAlive: true},
		{Processes: 1, Threads: 1, HTTPKeepAlive: false},
	}

	for _, cfg := range configs {
		cmd := uwsgiCommand(cfg)

		g.Expect(cmd[0]).To(Equal("uwsgi"), "first element must be 'uwsgi'")
		g.Expect(cmd).To(ContainElement("--http"))
		g.Expect(cmd).To(ContainElement(":5000"))
		g.Expect(cmd).To(ContainElement("--wsgi-file"))
		g.Expect(cmd).To(ContainElement("/var/lib/openstack/bin/keystone-wsgi-public"))
		g.Expect(cmd).To(ContainElement("--master"))
		g.Expect(cmd).To(ContainElement("--lazy-apps"))
		g.Expect(cmd).To(ContainElement("--need-app"))
		g.Expect(cmd).To(ContainElement("--pyargv=--config-dir=/etc/keystone/keystone.conf.d/"))
	}
}

// Feature: CC-0075

// TestBuildKeystoneDeployment_DefaultTopologySpreadConstraints verifies that when
// spec.TopologySpreadConstraints is nil, the deployment builder injects two default
// constraints: zone-spread and hostname-spread, both with ScheduleAnyway (CC-0075).
func TestBuildKeystoneDeployment_DefaultTopologySpreadConstraints(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	tscs := deploy.Spec.Template.Spec.TopologySpreadConstraints
	g.Expect(tscs).To(HaveLen(2))

	// First default: zone-spread.
	g.Expect(tscs[0].MaxSkew).To(Equal(int32(1)))
	g.Expect(tscs[0].TopologyKey).To(Equal("topology.kubernetes.io/zone"))
	g.Expect(tscs[0].WhenUnsatisfiable).To(Equal(corev1.ScheduleAnyway))
	g.Expect(tscs[0].LabelSelector).NotTo(BeNil())
	g.Expect(tscs[0].LabelSelector.MatchLabels).To(Equal(selectorLabels(ks)))

	// Second default: hostname-spread.
	g.Expect(tscs[1].MaxSkew).To(Equal(int32(1)))
	g.Expect(tscs[1].TopologyKey).To(Equal("kubernetes.io/hostname"))
	g.Expect(tscs[1].WhenUnsatisfiable).To(Equal(corev1.ScheduleAnyway))
	g.Expect(tscs[1].LabelSelector).NotTo(BeNil())
	g.Expect(tscs[1].LabelSelector.MatchLabels).To(Equal(selectorLabels(ks)))
}

// TestBuildKeystoneDeployment_EmptyTopologySpreadConstraintsDisablesDefaults verifies
// that setting spec.TopologySpreadConstraints to an empty non-nil slice disables
// default TSC injection, resulting in no constraints on the pod spec (CC-0075).
func TestBuildKeystoneDeployment_EmptyTopologySpreadConstraintsDisablesDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	ks.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{}

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Template.Spec.TopologySpreadConstraints).To(BeEmpty())
}

// TestBuildKeystoneDeployment_DefaultTopologySpreadConstraints_LabelSelectorMatchesSelectorLabels
// explicitly verifies that the default TSC LabelSelector equals selectorLabels(ks),
// ensuring the TSC targets the correct pods (CC-0075).
func TestBuildKeystoneDeployment_DefaultTopologySpreadConstraints_LabelSelectorMatchesSelectorLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")
	expected := selectorLabels(ks)

	for i, tsc := range deploy.Spec.Template.Spec.TopologySpreadConstraints {
		g.Expect(tsc.LabelSelector).NotTo(BeNil(), "TSC[%d] must have a LabelSelector", i)
		g.Expect(tsc.LabelSelector.MatchLabels).To(Equal(expected),
			"TSC[%d] LabelSelector.MatchLabels must equal selectorLabels()", i)
	}
}

// TestBuildKeystoneDeployment_CustomTopologySpreadConstraints verifies that when
// spec.TopologySpreadConstraints is set, the deployment uses those constraints
// verbatim without merging with defaults (CC-0075).
func TestBuildKeystoneDeployment_CustomTopologySpreadConstraints(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	ks.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           2,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "keystone"}},
		},
		{
			MaxSkew:           3,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "keystone"}},
		},
	}

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	tscs := deploy.Spec.Template.Spec.TopologySpreadConstraints
	g.Expect(tscs).To(HaveLen(2))
	g.Expect(tscs[0].MaxSkew).To(Equal(int32(2)))
	g.Expect(tscs[0].TopologyKey).To(Equal("kubernetes.io/hostname"))
	g.Expect(tscs[0].WhenUnsatisfiable).To(Equal(corev1.DoNotSchedule))
	g.Expect(tscs[1].MaxSkew).To(Equal(int32(3)))
	g.Expect(tscs[1].TopologyKey).To(Equal("topology.kubernetes.io/zone"))
	g.Expect(tscs[1].WhenUnsatisfiable).To(Equal(corev1.ScheduleAnyway))
}

// TestBuildKeystoneDeployment_PriorityClassNameSet verifies that when
// spec.PriorityClassName is set, the deployment PodSpec includes the
// configured priority class name (CC-0075).
func TestBuildKeystoneDeployment_PriorityClassNameSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	pcn := "system-cluster-critical"
	ks.Spec.PriorityClassName = &pcn

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Template.Spec.PriorityClassName).To(Equal("system-cluster-critical"))
}

// TestBuildKeystoneDeployment_PriorityClassNameNil verifies that when
// spec.PriorityClassName is nil, the deployment PodSpec has an empty
// priority class name, deferring to the cluster default (CC-0075).
func TestBuildKeystoneDeployment_PriorityClassNameNil(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Template.Spec.PriorityClassName).To(BeEmpty())
}

// Feature: CC-0056

// TestReconcileDeployment_RollingUpdate_ReadyDeployment_TransitionsToContracting verifies that
// when the Deployment becomes ready during an active upgrade in the RollingUpdate phase,
// reconcileDeployment transitions UpgradePhase to Contracting and requeues immediately (CC-0056, REQ-004).
func TestReconcileDeployment_RollingUpdate_ReadyDeployment_TransitionsToContracting(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	ks.Status.InstalledRelease = "2025.2"
	ks.Status.TargetRelease = "2026.1"
	ks.Status.UpgradePhase = keystonev1alpha1.UpgradePhaseRollingUpdate

	deploy := readyDeployment(ks, "keystone-config-abc123")
	r := newDeployTestReconciler(s, ks, deploy)

	result, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// Must requeue immediately (not RequeueAfter) so the next reconcile enters reconcileContract.
	g.Expect(result).To(Equal(ctrl.Result{Requeue: true}), "expected immediate requeue for phase transition")

	// UpgradePhase must transition to Contracting.
	g.Expect(ks.Status.UpgradePhase).To(Equal(keystonev1alpha1.UpgradePhaseContracting))

	// DeploymentReady condition must be True (the deployment IS ready).
	cond := meta.FindStatusCondition(ks.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("DeploymentReady"))

	// Endpoint should NOT be set during upgrade phase transition (deferred to normal path).
	g.Expect(ks.Status.Endpoint).To(BeEmpty())

	expectEvent(g, r, "Normal DeploymentRolloutComplete")
}

// TestReconcileDeployment_RollingUpdate_NotReady_Requeues verifies that when the Deployment
// is not ready during an active upgrade in the RollingUpdate phase, the operator requeues
// with the standard polling interval and does NOT transition phases (CC-0056).
func TestReconcileDeployment_RollingUpdate_NotReady_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	ks.Status.InstalledRelease = "2025.2"
	ks.Status.TargetRelease = "2026.1"
	ks.Status.UpgradePhase = keystonev1alpha1.UpgradePhaseRollingUpdate

	deploy := notReadyDeployment(ks, "keystone-config-abc123")
	r := newDeployTestReconciler(s, ks, deploy)

	result, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// Must requeue with the standard polling interval.
	g.Expect(result.RequeueAfter).To(Equal(RequeueDeploymentPolling))

	// UpgradePhase must remain RollingUpdate — no transition when not ready.
	g.Expect(ks.Status.UpgradePhase).To(Equal(keystonev1alpha1.UpgradePhaseRollingUpdate))

	// DeploymentReady condition must be False.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDeployment"))
}

// TestReconcileDeployment_NoUpgrade_Ready_SetsEndpoint verifies that when there is no active
// upgrade (empty UpgradePhase), the normal ready path sets the endpoint and DeploymentReady=True
// without any phase transition (CC-0056 regression guard).
func TestReconcileDeployment_NoUpgrade_Ready_SetsEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	// UpgradePhase is empty — no upgrade in progress.

	deploy := readyDeployment(ks, "keystone-config-abc123")
	r := newDeployTestReconciler(s, ks, deploy)

	result, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// Normal path: no requeue.
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Endpoint must be set.
	g.Expect(ks.Status.Endpoint).To(Equal("http://test-keystone-api.default.svc.cluster.local:5000/v3"))

	// UpgradePhase must remain empty.
	g.Expect(ks.Status.UpgradePhase).To(Equal(keystonev1alpha1.UpgradePhase("")))

	// DeploymentReady condition must be True.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("DeploymentReady"))

	expectNoEvent(g, r)
}

// TestReconcileDeployment_OtherPhase_Ready_SetsEndpoint verifies that when an upgrade is
// in a phase OTHER than RollingUpdate (e.g. Expanding), the deployment-ready path follows
// the normal flow: sets endpoint, DeploymentReady=True, no phase transition. Only
// RollingUpdate triggers the Contracting transition (CC-0056).
func TestReconcileDeployment_OtherPhase_Ready_SetsEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()
	ks.Status.InstalledRelease = "2025.2"
	ks.Status.TargetRelease = "2026.1"
	ks.Status.UpgradePhase = keystonev1alpha1.UpgradePhaseExpanding

	deploy := readyDeployment(ks, "keystone-config-abc123")
	r := newDeployTestReconciler(s, ks, deploy)

	result, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// Normal path: no requeue.
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Endpoint must be set (normal ready path).
	g.Expect(ks.Status.Endpoint).To(Equal("http://test-keystone-api.default.svc.cluster.local:5000/v3"))

	// UpgradePhase must remain Expanding — no transition.
	g.Expect(ks.Status.UpgradePhase).To(Equal(keystonev1alpha1.UpgradePhaseExpanding))

	// DeploymentReady condition must be True.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("DeploymentReady"))
}

// TestReconcileDeployment_ConditionObservedGeneration verifies that
// ObservedGeneration is set on the DeploymentReady condition for both
// the False (WaitingForDeployment) and True (DeploymentReady) paths
// with distinct generation values (CC-0072, REQ-002, REQ-003).
func TestReconcileDeployment_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()

	// Test ObservedGeneration for the WaitingForDeployment path.
	ks := deployTestKeystone()
	ks.Generation = 7

	r := newDeployTestReconciler(s, ks)

	_, err := r.reconcileDeployment(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))

	// Test ObservedGeneration for the DeploymentReady path.
	ks2 := deployTestKeystone()
	ks2.Generation = 12

	deploy := readyDeployment(ks2, "keystone-config-abc123")
	r2 := newDeployTestReconciler(s, ks2, deploy)

	_, err = r2.reconcileDeployment(context.Background(), ks2, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, "DeploymentReady")
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(12)))
}

// indexOf returns the index of the first occurrence of s in slice, or -1.
func indexOf(slice []string, s string) int {
	for i, v := range slice {
		if v == s {
			return i
		}
	}
	return -1
}
