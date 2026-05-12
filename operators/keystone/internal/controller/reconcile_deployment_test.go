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
		Name: "test-keystone", Namespace: "default",
	}, &deploy)).To(Succeed())

	// Verify Service was created.
	var svc corev1.Service
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
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

	g.Expect(ks.Status.Endpoint).To(Equal("http://test-keystone.default.svc.cluster.local:5000/v3"))
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
		Name: "test-keystone", Namespace: "default",
	}, &deploy)).To(Succeed())
	g.Expect(deploy.OwnerReferences).To(HaveLen(1))
	g.Expect(deploy.OwnerReferences[0].Name).To(Equal("test-keystone"))

	// Verify Service has owner reference.
	var svc corev1.Service
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
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
		Name: "test-keystone", Namespace: "default",
	}, &deploy)).To(Succeed())

	// Verify replicas.
	g.Expect(deploy.Spec.Replicas).NotTo(BeNil())
	g.Expect(*deploy.Spec.Replicas).To(Equal(int32(3)))

	// Verify container spec.
	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
	container := deploy.Spec.Template.Spec.Containers[0]
	g.Expect(container.Name).To(Equal("keystone"))
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/keystone:2025.2"))

	// Verify port.
	g.Expect(container.Ports).To(HaveLen(1))
	g.Expect(container.Ports[0].ContainerPort).To(Equal(int32(5000)))
	g.Expect(container.Ports[0].Name).To(Equal("keystone"))

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
		Name: "test-keystone", Namespace: "default",
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

// Feature: CC-0099 — restrict mode on mounted Fernet/credential key Secret volumes.

// TestBuildKeystoneDeployment_PodSecurityContextSetsFSGroup verifies that the
// Pod template carries SecurityContext.FSGroup = openstackUID so that mounted
// Secret volumes are owned by the openstack group, satisfying the upstream
// Keystone "key_repository is world readable" check (CC-0099, REQ-001, REQ-008).
// All other PodSecurityContext fields must remain nil — Pod-level FSGroup is
// orthogonal to the container-level CC-0045 PSS-Restricted SecurityContext.
func TestBuildKeystoneDeployment_PodSecurityContextSetsFSGroup(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildKeystoneDeployment(deployTestKeystone(), "keystone-config-abc123")

	psc := deploy.Spec.Template.Spec.SecurityContext
	g.Expect(psc).NotTo(BeNil(), "CC-0099: PodSecurityContext must be set so FSGroup applies")
	g.Expect(psc.FSGroup).NotTo(BeNil(), "CC-0099: FSGroup must be set on PodSecurityContext")
	g.Expect(*psc.FSGroup).To(Equal(openstackUID), "CC-0099: FSGroup must equal the openstack UID/GID (42424)")

	// CC-0099: do not set any other Pod-level SecurityContext field. Pod-level
	// RunAs* / Seccomp / SELinux / AppArmor would conflict with or override
	// the container-level CC-0045 SecurityContext.
	g.Expect(psc.RunAsUser).To(BeNil(), "CC-0099: RunAsUser must stay container-level (CC-0045)")
	g.Expect(psc.RunAsGroup).To(BeNil(), "CC-0099: RunAsGroup must stay container-level (CC-0045)")
	g.Expect(psc.RunAsNonRoot).To(BeNil(), "CC-0099: RunAsNonRoot must stay container-level (CC-0045)")
	g.Expect(psc.SeccompProfile).To(BeNil(), "CC-0099: SeccompProfile must stay container-level (CC-0045)")
	g.Expect(psc.FSGroupChangePolicy).To(BeNil(), "CC-0099: FSGroupChangePolicy must remain unset (default Always is intentional)")
	g.Expect(psc.SupplementalGroups).To(BeNil(), "CC-0099: SupplementalGroups must remain unset")
	g.Expect(psc.SELinuxOptions).To(BeNil(), "CC-0099: SELinuxOptions must remain unset")
	g.Expect(psc.WindowsOptions).To(BeNil(), "CC-0099: WindowsOptions must remain unset")
	g.Expect(psc.Sysctls).To(BeNil(), "CC-0099: Sysctls must remain unset")
	g.Expect(psc.AppArmorProfile).To(BeNil(), "CC-0099: AppArmorProfile must remain unset")
}

// TestBuildKeystoneDeployment_FernetAndCredentialVolumesSetDefaultMode0400 verifies
// that the fernet-keys and credential-keys Secret volumes mount with file mode
// 0o400 (owner read-only), so the Keystone process running under the openstack
// UID/GID can read the keys while the volume is not group- or world-readable
// (CC-0099, REQ-002). The config ConfigMap volume must NOT receive a
// DefaultMode — it is out of scope and changing it would be scope creep.
func TestBuildKeystoneDeployment_FernetAndCredentialVolumesSetDefaultMode0400(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildKeystoneDeployment(deployTestKeystone(), "keystone-config-abc123")

	var fernetVol, credentialVol, configVol corev1.Volume
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		switch v.Name {
		case "fernet-keys":
			fernetVol = v
		case "credential-keys":
			credentialVol = v
		case "config":
			configVol = v
		}
	}

	g.Expect(fernetVol.Secret).NotTo(BeNil(), "CC-0099: fernet-keys Secret volume source must be set")
	g.Expect(fernetVol.Secret.DefaultMode).NotTo(BeNil(), "CC-0099: fernet-keys must set DefaultMode")
	g.Expect(*fernetVol.Secret.DefaultMode).To(Equal(int32(0o400)), "CC-0099: fernet-keys DefaultMode must be 0o400 (owner read-only)")

	g.Expect(credentialVol.Secret).NotTo(BeNil(), "CC-0099: credential-keys Secret volume source must be set")
	g.Expect(credentialVol.Secret.DefaultMode).NotTo(BeNil(), "CC-0099: credential-keys must set DefaultMode")
	g.Expect(*credentialVol.Secret.DefaultMode).To(Equal(int32(0o400)), "CC-0099: credential-keys DefaultMode must be 0o400 (owner read-only)")

	// Regression guard: do not tighten the config ConfigMap volume. CC-0099 is
	// scoped to the two Fernet-related Secret volumes only.
	g.Expect(configVol.ConfigMap).NotTo(BeNil(), "CC-0099 scope guard: config volume must remain a ConfigMap source")
	g.Expect(configVol.ConfigMap.DefaultMode).To(BeNil(), "CC-0099 scope guard: config ConfigMap DefaultMode must remain unset")
}

// TestBuildKeystoneDeployment_ContainerSecurityContextUnchangedByCC0099 is an
// active regression guard: CC-0099 must NOT touch the container-level
// SecurityContext established by CC-0045. Pod-level FSGroup and container-level
// RunAsUser/RunAsGroup/etc. are independent fields (CC-0099, REQ-008).
func TestBuildKeystoneDeployment_ContainerSecurityContextUnchangedByCC0099(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildKeystoneDeployment(deployTestKeystone(), "keystone-config-abc123")

	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone")
	g.Expect(container).NotTo(BeNil(), "CC-0099: keystone container must exist")
	expectRestrictedSecurityContext(g, container)
}

// TestBuildKeystoneDeployment_NoFSGroupChangePolicyOrUnsupportedFields locks in
// the choice to leave FSGroupChangePolicy unset so the kubelet's default
// "Always" recursive chown applies on every mount. Setting "OnRootMismatch"
// would skip the chown when the volume already has the right group, which is
// brittle for in-place key rotation (CC-0099, REQ-001).
func TestBuildKeystoneDeployment_NoFSGroupChangePolicyOrUnsupportedFields(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildKeystoneDeployment(deployTestKeystone(), "keystone-config-abc123")

	psc := deploy.Spec.Template.Spec.SecurityContext
	g.Expect(psc).NotTo(BeNil(), "CC-0099: PodSecurityContext must be set")
	g.Expect(psc.FSGroupChangePolicy).To(BeNil(), "CC-0099: FSGroupChangePolicy must remain unset (kubelet default Always is intentional)")
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
		Name: "test-keystone", Namespace: "default",
	}, &deploy)).To(Succeed())

	var svc corev1.Service
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
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
	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone")
	g.Expect(container).NotTo(BeNil(), "keystone container must exist")
	mounts := container.VolumeMounts
	g.Expect(mounts).NotTo(BeEmpty(), "keystone container must have volume mounts")
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

// TestBuildKeystoneDeployment_DBConnectionEnv verifies that the keystone
// container has the OS_DATABASE__CONNECTION env var wired to the derived
// connection Secret so oslo.config overrides the [database] connection value
// (CC-0080, REQ-003, REQ-007). Volumes and mounts must remain unchanged.
func TestBuildKeystoneDeployment_DBConnectionEnv(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Env).To(ContainElement(buildDBConnectionEnvVar(ks)),
		"keystone container must consume DB connection via OS_DATABASE__CONNECTION (CC-0080, REQ-003)")

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
		Name: "test-keystone", Namespace: "default",
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
		Name: "test-keystone", Namespace: "default",
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
		Name: "test-keystone", Namespace: "default",
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
		Name: "test-keystone", Namespace: "default",
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
		Name: "test-keystone", Namespace: "default",
	}, &pdb)).To(Succeed())
	g.Expect(pdb.Spec.MinAvailable).NotTo(BeNil())

	// Change to replicas=1 and re-reconcile → maxUnavailable=1.
	ks.Spec.Replicas = 1
	_, err = r.reconcileDeployment(ctx, ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
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
		Name: "test-keystone", Namespace: "default",
	}, &deploy)).To(Succeed())

	var pdb policyv1.PodDisruptionBudget
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
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
	g.Expect(ks.Status.Endpoint).To(Equal("http://test-keystone.default.svc.cluster.local:5000/v3"))

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
	g.Expect(ks.Status.Endpoint).To(Equal("http://test-keystone.default.svc.cluster.local:5000/v3"))

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

// Feature: CC-0084

// TestBuildKeystoneDeployment_TerminationGracePeriodDefault verifies that when
// spec.TerminationGracePeriodSeconds is nil, the reconciler falls back to the
// shared DefaultTerminationGracePeriodSeconds constant — the same value the
// validating webhook resolves nil against for cross-field arithmetic. Pinning
// both sides to the shared constant guarantees the reconciler and webhook
// cannot silently drift on REQ-007's drain-window invariant (CC-0084, REQ-001).
func TestBuildKeystoneDeployment_TerminationGracePeriodDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	ks.Spec.TerminationGracePeriodSeconds = nil

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).NotTo(BeNil())
	g.Expect(*deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).
		To(Equal(keystonev1alpha1.DefaultTerminationGracePeriodSeconds))
}

// TestBuildKeystoneDeployment_TerminationGracePeriodCustom verifies that a set
// spec.TerminationGracePeriodSeconds propagates verbatim to the PodSpec
// (CC-0084, REQ-001).
func TestBuildKeystoneDeployment_TerminationGracePeriodCustom(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	custom := int64(90)
	ks.Spec.TerminationGracePeriodSeconds = &custom

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).NotTo(BeNil())
	g.Expect(*deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).To(Equal(int64(90)))
}

// TestBuildKeystoneDeployment_PreStopSleepDefault verifies that when
// spec.PreStopSleepSeconds is nil the reconciler falls back to the shared
// DefaultPreStopSleepSeconds constant — the same value the validating webhook
// resolves nil against for cross-field arithmetic. Pinning both sides to the
// shared constant guarantees the reconciler and webhook cannot silently drift
// on REQ-007's drain-window invariant (CC-0084, REQ-002, REQ-009).
func TestBuildKeystoneDeployment_PreStopSleepDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	ks.Spec.PreStopSleepSeconds = nil

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Lifecycle).NotTo(BeNil())
	g.Expect(container.Lifecycle.PreStop).NotTo(BeNil())
	g.Expect(container.Lifecycle.PreStop.Exec).NotTo(BeNil())
	g.Expect(container.Lifecycle.PreStop.Exec.Command).To(Equal(
		[]string{"/bin/sh", "-c", fmt.Sprintf("sleep %d", keystonev1alpha1.DefaultPreStopSleepSeconds)},
	))
}

// TestBuildKeystoneDeployment_PreStopSleepCustom verifies that a set
// spec.PreStopSleepSeconds propagates into the preStop exec command as
// "sleep <n>" (CC-0084, REQ-002, REQ-010).
func TestBuildKeystoneDeployment_PreStopSleepCustom(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	custom := int64(12)
	ks.Spec.PreStopSleepSeconds = &custom

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Lifecycle.PreStop.Exec.Command).To(Equal([]string{"/bin/sh", "-c", "sleep 12"}))
}

// TestBuildKeystoneDeployment_PreStopSleepZero verifies that setting
// spec.PreStopSleepSeconds=0 emits "sleep 0" rather than falling back to the
// default — zero is a permitted opt-out value (CC-0084, REQ-002, REQ-010).
func TestBuildKeystoneDeployment_PreStopSleepZero(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	zero := int64(0)
	ks.Spec.PreStopSleepSeconds = &zero

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Lifecycle.PreStop.Exec.Command).To(Equal([]string{"/bin/sh", "-c", "sleep 0"}))
}

// TestReconcileAndWebhookDefaultsAgree pins the reconciler's nil-default output
// for terminationGracePeriodSeconds and preStopSleepSeconds to the shared
// keystonev1alpha1.Default* constants that the validating webhook uses for
// cross-field arithmetic. If a future refactor re-introduces a literal on
// either side, this test fails — protecting REQ-007's drain-window invariant
// (preStopSleep < terminationGracePeriod) from silent drift (CC-0084, REQ-001,
// REQ-002, REQ-007).
func TestReconcileAndWebhookDefaultsAgree(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	ks.Spec.TerminationGracePeriodSeconds = nil
	ks.Spec.PreStopSleepSeconds = nil

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).NotTo(BeNil())
	g.Expect(*deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).
		To(Equal(keystonev1alpha1.DefaultTerminationGracePeriodSeconds),
			"reconciler nil-default for TerminationGracePeriodSeconds must equal the shared webhook constant")

	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Lifecycle).NotTo(BeNil())
	g.Expect(container.Lifecycle.PreStop).NotTo(BeNil())
	g.Expect(container.Lifecycle.PreStop.Exec).NotTo(BeNil())
	g.Expect(container.Lifecycle.PreStop.Exec.Command).To(Equal(
		[]string{"/bin/sh", "-c", fmt.Sprintf("sleep %d", keystonev1alpha1.DefaultPreStopSleepSeconds)},
	), "reconciler nil-default for PreStopSleepSeconds must equal the shared webhook constant")

	g.Expect(keystonev1alpha1.DefaultPreStopSleepSeconds).
		To(BeNumerically("<", keystonev1alpha1.DefaultTerminationGracePeriodSeconds),
			"shared defaults must satisfy the REQ-007 invariant preStopSleep < terminationGracePeriod")
}

// TestUwsgiCommand_HarakiriSet verifies that a non-nil UWSGISpec.Harakiri
// appends "--harakiri <n>" to the command (CC-0084, REQ-003).
func TestUwsgiCommand_HarakiriSet(t *testing.T) {
	g := NewGomegaWithT(t)
	harakiri := int32(25)

	cmd := uwsgiCommand(&keystonev1alpha1.UWSGISpec{
		Processes:     2,
		Threads:       1,
		HTTPKeepAlive: true,
		Harakiri:      &harakiri,
	})

	idx := indexOf(cmd, "--harakiri")
	g.Expect(idx).NotTo(Equal(-1))
	g.Expect(cmd[idx+1]).To(Equal("25"))
}

// TestUwsgiCommand_HarakiriNilOmitted verifies that when UWSGISpec.Harakiri
// is nil, the --harakiri flag is not emitted — the field is an explicit opt-in
// with no hidden default (CC-0084, REQ-003).
func TestUwsgiCommand_HarakiriNilOmitted(t *testing.T) {
	g := NewGomegaWithT(t)

	cmd := uwsgiCommand(&keystonev1alpha1.UWSGISpec{
		Processes:     2,
		Threads:       1,
		HTTPKeepAlive: true,
	})

	g.Expect(cmd).NotTo(ContainElement("--harakiri"))
}

// TestUwsgiCommand_KeepAliveTimeoutSet verifies that a non-nil
// UWSGISpec.HTTPKeepAliveTimeout combined with HTTPKeepAlive=true appends
// "--http-keepalive-timeout <n>" to the command (CC-0084, REQ-004).
func TestUwsgiCommand_KeepAliveTimeoutSet(t *testing.T) {
	g := NewGomegaWithT(t)
	timeout := int32(4)

	cmd := uwsgiCommand(&keystonev1alpha1.UWSGISpec{
		Processes:            2,
		Threads:              1,
		HTTPKeepAlive:        true,
		HTTPKeepAliveTimeout: &timeout,
	})

	idx := indexOf(cmd, "--http-keepalive-timeout")
	g.Expect(idx).NotTo(Equal(-1))
	g.Expect(cmd[idx+1]).To(Equal("4"))
}

// TestUwsgiCommand_KeepAliveTimeoutNilOmitted verifies that when
// UWSGISpec.HTTPKeepAliveTimeout is nil, the flag is not emitted (CC-0084, REQ-004).
func TestUwsgiCommand_KeepAliveTimeoutNilOmitted(t *testing.T) {
	g := NewGomegaWithT(t)

	cmd := uwsgiCommand(&keystonev1alpha1.UWSGISpec{
		Processes:     2,
		Threads:       1,
		HTTPKeepAlive: true,
	})

	g.Expect(cmd).NotTo(ContainElement("--http-keepalive-timeout"))
}

// TestUwsgiCommand_KeepAliveTimeoutIgnoredWhenKeepAliveDisabled verifies that
// HTTPKeepAliveTimeout is silently ignored when HTTPKeepAlive=false — the flag
// is meaningless without the parent feature, and the webhook forbids this
// combination, so the command builder just omits it defensively (CC-0084, REQ-011).
func TestUwsgiCommand_KeepAliveTimeoutIgnoredWhenKeepAliveDisabled(t *testing.T) {
	g := NewGomegaWithT(t)
	timeout := int32(4)

	cmd := uwsgiCommand(&keystonev1alpha1.UWSGISpec{
		Processes:            2,
		Threads:              1,
		HTTPKeepAlive:        false,
		HTTPKeepAliveTimeout: &timeout,
	})

	g.Expect(cmd).NotTo(ContainElement("--http-keepalive"))
	g.Expect(cmd).NotTo(ContainElement("--http-keepalive-timeout"))
}

// TestUwsgiCommand_IncludesLogMasterAndLogFormat verifies that uwsgiCommand
// always emits --log-master and --log-format <literal> between the
// --http-keepalive[-timeout] block and the --wsgi-file line, regardless of
// HTTPKeepAlive/HTTPKeepAliveTimeout state. REQ-006 requires uWSGI master
// logging to be unconditionally on so request lines reach stderr in every
// configuration (CC-0098, REQ-006).
func TestUwsgiCommand_IncludesLogMasterAndLogFormat(t *testing.T) {
	const expectedFormat = "%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto) %(status))"

	timeout := int32(4)
	cases := []struct {
		name string
		spec *keystonev1alpha1.UWSGISpec
	}{
		{
			name: "keepalive enabled without timeout",
			spec: &keystonev1alpha1.UWSGISpec{
				Processes:     2,
				Threads:       1,
				HTTPKeepAlive: true,
			},
		},
		{
			name: "keepalive enabled with timeout",
			spec: &keystonev1alpha1.UWSGISpec{
				Processes:            2,
				Threads:              1,
				HTTPKeepAlive:        true,
				HTTPKeepAliveTimeout: &timeout,
			},
		},
		{
			name: "keepalive disabled",
			spec: &keystonev1alpha1.UWSGISpec{
				Processes:     2,
				Threads:       1,
				HTTPKeepAlive: false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			cmd := uwsgiCommand(tc.spec)

			logMasterIdx := indexOf(cmd, "--log-master")
			logFormatIdx := indexOf(cmd, "--log-format")
			wsgiFileIdx := indexOf(cmd, "--wsgi-file")

			g.Expect(logMasterIdx).NotTo(Equal(-1), "--log-master must be present (CC-0098, REQ-006)")
			g.Expect(logFormatIdx).NotTo(Equal(-1), "--log-format must be present (CC-0098, REQ-006)")
			g.Expect(wsgiFileIdx).NotTo(Equal(-1))

			// --log-format must be followed immediately by the exact literal value
			// as a single argument (not split).
			g.Expect(cmd[logFormatIdx+1]).To(Equal(expectedFormat),
				"--log-format value must be the exact literal format string (CC-0098, REQ-006)")

			// Insertion point: after --http-keepalive[-timeout] block (when
			// present) and before --wsgi-file.
			g.Expect(logMasterIdx).To(BeNumerically("<", wsgiFileIdx),
				"--log-master must precede --wsgi-file")
			g.Expect(logFormatIdx).To(BeNumerically("<", wsgiFileIdx),
				"--log-format must precede --wsgi-file")

			if tc.spec.HTTPKeepAlive {
				keepAliveIdx := indexOf(cmd, "--http-keepalive")
				g.Expect(keepAliveIdx).NotTo(Equal(-1))
				g.Expect(logMasterIdx).To(BeNumerically(">", keepAliveIdx),
					"--log-master must follow --http-keepalive")
				if tc.spec.HTTPKeepAliveTimeout != nil {
					timeoutIdx := indexOf(cmd, "--http-keepalive-timeout")
					g.Expect(timeoutIdx).NotTo(Equal(-1))
					g.Expect(logMasterIdx).To(BeNumerically(">", timeoutIdx),
						"--log-master must follow --http-keepalive-timeout pair")
				}
			}
		})
	}
}

// TestUwsgiCommand_FlagOrderDeterministic verifies that the command ordering
// is deterministic for the same input, so the pod template hash is stable
// across reconciles. The relative-order assertions intentionally mirror
// TestUwsgiCommand_IncludesLogMasterAndLogFormat: pinning the canonical
// indices (e.g. --log-master at 6, --log-format at 7) would make this test
// brittle to any future flag inserted before --log-master even when the
// layout is still semantically correct (CC-0084, CC-0098, REQ-003, REQ-004,
// REQ-006).
func TestUwsgiCommand_FlagOrderDeterministic(t *testing.T) {
	g := NewGomegaWithT(t)
	harakiri := int32(25)
	timeout := int32(4)
	spec := &keystonev1alpha1.UWSGISpec{
		Processes:            2,
		Threads:              1,
		HTTPKeepAlive:        true,
		Harakiri:             &harakiri,
		HTTPKeepAliveTimeout: &timeout,
	}

	first := uwsgiCommand(spec)
	second := uwsgiCommand(spec)

	g.Expect(first).To(Equal(second))

	// Assert the layout invariant by relative position rather than absolute
	// index: --log-master/--log-format must precede --wsgi-file, and
	// --log-format must immediately follow --log-master so that --log-format's
	// literal value is its argument (CC-0098, REQ-006).
	g.Expect(indexOf(first, "--log-master")).NotTo(Equal(-1),
		"--log-master must be present (CC-0098, REQ-006)")
	g.Expect(indexOf(first, "--log-format")).NotTo(Equal(-1),
		"--log-format must be present (CC-0098, REQ-006)")
	g.Expect(indexOf(first, "--log-master")).To(BeNumerically("<", indexOf(first, "--wsgi-file")),
		"--log-master must precede --wsgi-file (CC-0098, REQ-006)")
	g.Expect(indexOf(first, "--log-format")).To(Equal(indexOf(first, "--log-master")+1),
		"--log-format must immediately follow --log-master so its literal value is the next argv element (CC-0098, REQ-006)")
}

// TestBuildKeystoneDeployment_DefaultRollingUpdateStrategy verifies that when
// spec.Strategy is nil, the reconciler injects a RollingUpdate strategy with
// MaxUnavailable=0 and MaxSurge=1 so available capacity never dips below
// spec.replicas during an image-tag patch (CC-0084, REQ-005).
func TestBuildKeystoneDeployment_DefaultRollingUpdateStrategy(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	ks.Spec.Strategy = nil

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
	g.Expect(deploy.Spec.Strategy.RollingUpdate).NotTo(BeNil())
	g.Expect(deploy.Spec.Strategy.RollingUpdate.MaxUnavailable).NotTo(BeNil())
	g.Expect(*deploy.Spec.Strategy.RollingUpdate.MaxUnavailable).To(Equal(intstr.FromInt32(0)))
	g.Expect(deploy.Spec.Strategy.RollingUpdate.MaxSurge).NotTo(BeNil())
	g.Expect(*deploy.Spec.Strategy.RollingUpdate.MaxSurge).To(Equal(intstr.FromInt32(1)))
}

// TestBuildKeystoneDeployment_StrategyStableAcrossReconciles verifies that two
// calls to buildKeystoneDeployment with identical input produce deeply equal
// Strategy blocks (CC-0084, REQ-005, REQ-006).
//
// Drift note: EnsureDeployment unconditionally assigns `existing.Spec =
// deploy.Spec` (CC-0005), so it does not gate the update on a drift check.
// The stability contract we need is that buildKeystoneDeployment returns the
// same Strategy for the same input — this guarantees the repeated Update
// calls produce a no-op spec diff at the API server, which in turn keeps the
// Deployment controller from triggering new rollouts on each reconcile. The
// one-time convergence scenario (pre-existing Deployment built by an older
// operator that never set Strategy, so the API server defaulted it to 25%/25%)
// is covered by TestEnsureDeployment_StrategyConvergesFromServerDefault below.
func TestBuildKeystoneDeployment_StrategyStableAcrossReconciles(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	first := buildKeystoneDeployment(ks, "keystone-config-abc123")
	second := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(first.Spec.Strategy).To(Equal(second.Spec.Strategy))
}

// TestEnsureDeployment_StrategyConvergesFromServerDefault verifies the
// one-time convergence case: an existing Deployment was created by an older
// operator version that did not set Strategy, so the API server defaulted it
// to RollingUpdate 25%/25%. After the upgrade, buildKeystoneDeployment emits
// the new 0/1 default, and a single reconcile must overwrite the server
// default without error. A second reconcile must then produce a stable spec
// (no further Strategy changes) (CC-0084, REQ-005).
func TestEnsureDeployment_StrategyConvergesFromServerDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	s := deployTestScheme()
	ks := deployTestKeystone()

	// Seed an existing Deployment that mimics the server-defaulted Strategy
	// (25%/25%) — as if created by an older operator that omitted the field.
	existing := buildKeystoneDeployment(ks, "keystone-config-abc123")
	serverDefaultUnavailable := intstr.FromString("25%")
	serverDefaultSurge := intstr.FromString("25%")
	existing.Spec.Strategy = appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &serverDefaultUnavailable,
			MaxSurge:       &serverDefaultSurge,
		},
	}
	r := newDeployTestReconciler(s, ks, existing)

	ctx := context.Background()

	// First reconcile: the default 0/1 strategy overwrites the server default.
	_, err := r.reconcileDeployment(ctx, ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var afterFirst appsv1.Deployment
	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &afterFirst)).To(Succeed())
	g.Expect(afterFirst.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
	g.Expect(*afterFirst.Spec.Strategy.RollingUpdate.MaxUnavailable).To(Equal(intstr.FromInt32(0)))
	g.Expect(*afterFirst.Spec.Strategy.RollingUpdate.MaxSurge).To(Equal(intstr.FromInt32(1)))

	// Second reconcile: the Strategy block must remain identical — no further
	// drift-triggered rollout (the stability contract from
	// TestBuildKeystoneDeployment_StrategyStableAcrossReconciles held end-to-end).
	_, err = r.reconcileDeployment(ctx, ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var afterSecond appsv1.Deployment
	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &afterSecond)).To(Succeed())
	g.Expect(afterSecond.Spec.Strategy).To(Equal(afterFirst.Spec.Strategy))
}

// TestBuildKeystoneDeployment_StrategyOverrideRollingCustomPercents verifies
// that a user-provided RollingUpdate strategy with percentage-based surge and
// unavailable values passes through verbatim (CC-0084, REQ-006).
func TestBuildKeystoneDeployment_StrategyOverrideRollingCustomPercents(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	maxUnavailable := intstr.FromString("25%")
	maxSurge := intstr.FromString("50%")
	ks.Spec.Strategy = &appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		},
	}

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
	g.Expect(deploy.Spec.Strategy.RollingUpdate).NotTo(BeNil())
	g.Expect(*deploy.Spec.Strategy.RollingUpdate.MaxUnavailable).To(Equal(intstr.FromString("25%")))
	g.Expect(*deploy.Spec.Strategy.RollingUpdate.MaxSurge).To(Equal(intstr.FromString("50%")))
}

// TestBuildKeystoneDeployment_StrategyOverrideRecreate verifies that a
// user-provided Recreate strategy passes through verbatim without the default
// RollingUpdate block being layered on top (CC-0084, REQ-006).
func TestBuildKeystoneDeployment_StrategyOverrideRecreate(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()
	ks.Spec.Strategy = &appsv1.DeploymentStrategy{
		Type: appsv1.RecreateDeploymentStrategyType,
	}

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
	g.Expect(deploy.Spec.Strategy.RollingUpdate).To(BeNil())
}

// TestBuildKeystoneDeployment_ContainerNameIsKeystone verifies that the sole
// container in the Keystone Deployment is named "keystone".
// Symmetric with Service/<cr-name> and ensures `kubectl logs <pod> -c keystone`
// resolves without falling back to the legacy name.
// CC-0095 legacy: pre-rename name documented for traceability (REQ-003).
func TestBuildKeystoneDeployment_ContainerNameIsKeystone(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1),
		"Deployment must define exactly one container (CC-0095, REQ-003)")
	g.Expect(deploy.Spec.Template.Spec.Containers[0].Name).To(Equal("keystone"),
		"container Name must be 'keystone' (renamed by CC-0095, REQ-003)") // CC-0095 legacy: legacy name "keystone-api" referenced for traceability.
}

// TestBuildKeystoneDeployment_NamedPortIsKeystone verifies that the
// container's named port is "keystone" with ContainerPort 5000.
// The rename is local-cosmetic: Service targetPort and HTTPRoute backendRef.port
// continue to reference the int 5000, so no cross-resource changes are required.
// CC-0095 legacy: pre-rename name documented for traceability (REQ-003).
func TestBuildKeystoneDeployment_NamedPortIsKeystone(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
	ports := deploy.Spec.Template.Spec.Containers[0].Ports
	g.Expect(ports).To(HaveLen(1),
		"container must define exactly one named port (CC-0095, REQ-003)")
	g.Expect(ports[0].Name).To(Equal("keystone"),
		"named port must be 'keystone' (renamed by CC-0095, REQ-003)") // CC-0095 legacy: legacy name "keystone-api" referenced for traceability.
	g.Expect(ports[0].ContainerPort).To(Equal(int32(5000)),
		"ContainerPort must remain 5000 — the rename is name-only (CC-0095, REQ-003)")
}

// TestBuildKeystoneDeployment_NameMatchesCR pins the Deployment ObjectMeta.Name
// to the bare CR name (no `-api` suffix). Symmetric with the Service, PDB, and
// HPA name guards: together they assert the operator emits sub-resources at
// `<cr-name>` rather than the legacy `<cr-name>-api` (CC-0095, REQ-003, // CC-0095 legacy: pre-rename name referenced for traceability.
// REQ-004).
func TestBuildKeystoneDeployment_NameMatchesCR(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	deploy := buildKeystoneDeployment(ks, "keystone-config-abc123")

	g.Expect(deploy.Name).To(Equal(ks.Name),
		"Deployment Name must equal the CR name (CC-0095, REQ-003)")
	g.Expect(deploy.Name).NotTo(HaveSuffix("-api"),
		"Deployment Name must not carry the legacy `-api` suffix (CC-0095, REQ-003)")
}

// TestBuildKeystoneService_NameMatchesCR pins the Service ObjectMeta.Name to
// the bare CR name. The cluster-internal Keystone URL is
// `http://<cr-name>.<ns>.svc...:5000/v3`, so any drift here would silently
// break in-cluster clients that follow the documented DNS form (CC-0095,
// REQ-004, REQ-005).
func TestBuildKeystoneService_NameMatchesCR(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	svc := buildKeystoneService(ks)

	g.Expect(svc.Name).To(Equal(ks.Name),
		"Service Name must equal the CR name (CC-0095, REQ-004)")
	g.Expect(svc.Name).NotTo(HaveSuffix("-api"),
		"Service Name must not carry the legacy `-api` suffix (CC-0095, REQ-004)")
}

// TestBuildPodDisruptionBudget_NameMatchesCR pins the PDB ObjectMeta.Name to
// the bare CR name. Chaos e2e tests look up the PDB by `<cr-name>`, so any
// drift here would break the chaos suite's PDB-availability assertion
// (CC-0095, REQ-004).
func TestBuildPodDisruptionBudget_NameMatchesCR(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := deployTestKeystone()

	pdb := buildPodDisruptionBudget(ks)

	g.Expect(pdb.Name).To(Equal(ks.Name),
		"PodDisruptionBudget Name must equal the CR name (CC-0095, REQ-004)")
	g.Expect(pdb.Name).NotTo(HaveSuffix("-api"),
		"PodDisruptionBudget Name must not carry the legacy `-api` suffix (CC-0095, REQ-004)")
}
