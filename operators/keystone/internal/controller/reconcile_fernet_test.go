// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// fernetTestScheme returns a runtime.Scheme with all types needed for Fernet tests.
func fernetTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	_ = esov1alpha1.SchemeBuilder.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

// fernetTestKeystone returns a minimal Keystone CR for reconcileFernetKeys tests.
func fernetTestKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			Generation: 1,
			UID:        "test-uid",
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
			},
			Cache: commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			Fernet: keystonev1alpha1.FernetSpec{
				RotationSchedule: "0 0 * * 0",
				MaxActiveKeys:    3,
			},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
		},
	}
}

func TestReconcileFernetKeys_NoSecret_CreatesSecretAndRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")

	g.Expect(err).NotTo(HaveOccurred())
	// Must requeue to confirm the secret is available before proceeding (CC-0013).
	g.Expect(result).To(Equal(ctrl.Result{Requeue: true}))

	// Verify the Secret was created with the right number of keys.
	var secret corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-keys",
	}, &secret)).To(Succeed())
	g.Expect(secret.Data).To(HaveLen(3))
	g.Expect(secret.OwnerReferences).To(HaveLen(1))
	g.Expect(secret.OwnerReferences[0].Name).To(Equal("test-keystone"))
	g.Expect(secret.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(secret.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(secret.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))

	// CronJob and PushSecret are NOT created on this cycle (early return after secret creation).

	// Verify FernetKeysReady condition is False (will be True on next reconcile).
	cond := meta.FindStatusCondition(ks.Status.Conditions, "FernetKeysReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("GeneratingKeys"))

	expectEvent(g, r, "Normal FernetKeysGenerated")
}

func TestReconcileFernetKeys_SecretAlreadyExists(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()

	// Pre-create the fernet keys Secret.
	fernetSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-fernet-keys",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"0": []byte("existing-key-0"),
			"1": []byte("existing-key-1"),
			"2": []byte("existing-key-2"),
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, fernetSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Verify the Secret was not re-created (data unchanged).
	var secret corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-keys",
	}, &secret)).To(Succeed())
	g.Expect(string(secret.Data["0"])).To(Equal("existing-key-0"))

	// Verify the script ConfigMap was created (CC-0073).
	var cmList corev1.ConfigMapList
	g.Expect(c.List(context.Background(), &cmList, client.InNamespace("default"))).To(Succeed())
	var scriptCM *corev1.ConfigMap
	for i := range cmList.Items {
		if strings.HasPrefix(cmList.Items[i].Name, "test-keystone-fernet-rotate-script-") {
			scriptCM = &cmList.Items[i]
			break
		}
	}
	g.Expect(scriptCM).NotTo(BeNil(), "script ConfigMap with prefix test-keystone-fernet-rotate-script- should exist")
	g.Expect(scriptCM.Data).To(HaveKey("fernet_rotate.sh"))
	g.Expect(scriptCM.Immutable).NotTo(BeNil())
	g.Expect(*scriptCM.Immutable).To(BeTrue())

	// Verify CronJob and PushSecret were still created.
	var cronJob batchv1.CronJob
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-rotate",
	}, &cronJob)).To(Succeed())
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))

	var ps esov1alpha1.PushSecret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-keys-backup",
	}, &ps)).To(Succeed())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "FernetKeysReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

	expectNoEvent(g, r)
}

func TestReconcileFernetKeys_CronJobScheduleUpdated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()

	// Pre-create Secret and CronJob with old schedule.
	fernetSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-fernet-keys",
			Namespace: "default",
		},
		Data: map[string][]byte{"0": []byte("key")},
	}

	oldCronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-fernet-rotate",
			Namespace: "default",
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 1 * *", // old schedule (monthly)
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers:    []corev1.Container{{Name: "fernet-rotate", Image: "old:image"}},
						},
					},
				},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, fernetSecret, oldCronJob).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	// Change the schedule in the spec.
	ks.Spec.Fernet.RotationSchedule = "0 */6 * * *"

	result, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Verify the CronJob schedule was updated.
	var cronJob batchv1.CronJob
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-rotate",
	}, &cronJob)).To(Succeed())
	g.Expect(cronJob.Spec.Schedule).To(Equal("0 */6 * * *"))
}

func TestReconcileFernetKeys_GeneratedKeysAreValid(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()
	ks.Spec.Fernet.MaxActiveKeys = 5

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var secret corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-keys",
	}, &secret)).To(Succeed())

	g.Expect(secret.Data).To(HaveLen(5))

	for i := range 5 {
		keyStr := string(secret.Data[strconv.Itoa(i)])
		decoded, err := base64.URLEncoding.DecodeString(keyStr)
		g.Expect(err).NotTo(HaveOccurred(), "key %d should be valid base64url", i)
		g.Expect(decoded).To(HaveLen(32), "key %d should decode to 32 bytes", i)
	}
}

func TestReconcileFernetKeys_CronJobScheduleMatchesSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()
	ks.Spec.Fernet.RotationSchedule = "30 2 * * 1"

	fernetSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("key")},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, fernetSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-rotate",
	}, &cronJob)).To(Succeed())
	g.Expect(cronJob.Spec.Schedule).To(Equal("30 2 * * 1"))
}

func TestReconcileFernetKeys_PushSecretReferencesCorrectSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()

	fernetSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("key")},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, fernetSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var ps esov1alpha1.PushSecret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-keys-backup",
	}, &ps)).To(Succeed())

	g.Expect(ps.Spec.Selector.Secret).NotTo(BeNil())
	g.Expect(ps.Spec.Selector.Secret.Name).To(Equal("test-keystone-fernet-keys"))
	g.Expect(ps.Spec.SecretStoreRefs).To(HaveLen(1))
	g.Expect(ps.Spec.SecretStoreRefs[0].Kind).To(Equal("ClusterSecretStore"))
	g.Expect(ps.Spec.SecretStoreRefs[0].Name).To(Equal("openbao-cluster-store"))
	g.Expect(ps.Spec.Data).To(HaveLen(1))
	g.Expect(ps.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal("openstack/keystone/fernet-keys"))
}

func TestGenerateFernetKey_Valid(t *testing.T) {
	g := NewGomegaWithT(t)

	key, err := generateFernetKey()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(key).NotTo(BeEmpty())

	decoded, err := base64.URLEncoding.DecodeString(key)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decoded).To(HaveLen(32))
}

func TestGenerateFernetKey_Unique(t *testing.T) {
	g := NewGomegaWithT(t)

	key1, err := generateFernetKey()
	g.Expect(err).NotTo(HaveOccurred())

	key2, err := generateFernetKey()
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(key1).NotTo(Equal(key2))
}

func TestReconcileFernetKeys_MinActiveKeysFloor(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()
	// Set MaxActiveKeys below the floor of 3.
	ks.Spec.Fernet.MaxActiveKeys = 1

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var secret corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-keys",
	}, &secret)).To(Succeed())

	// Even with MaxActiveKeys=1, at least 3 keys should be generated.
	g.Expect(secret.Data).To(HaveLen(3))

	for i := range 3 {
		g.Expect(secret.Data).To(HaveKey(strconv.Itoa(i)))
	}
}

func TestReconcileFernetKeys_ConditionMessages(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()

	fernetSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("key"), "1": []byte("key"), "2": []byte("key")},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, fernetSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// The final condition should be FernetKeysAvailable with the correct message.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "FernetKeysReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("FernetKeysAvailable"))
	g.Expect(cond.Message).To(Equal("Fernet keys Secret exists and rotation CronJob is configured"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

// TestFernetRotationCronJob_SecurityContext verifies that both containers in the
// Fernet rotation CronJob have a restricted SecurityContext (CC-0045).
func TestFernetRotationCronJob_SecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := fernetTestKeystone()

	cronJob := fernetRotationCronJob(ks, "test-keystone-config-abc123", "test-keystone-fernet-rotate-script-abc123")

	podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec

	// Verify init container "copy-keys" SecurityContext (REQ-001 through REQ-004).
	expectRestrictedSecurityContext(g, findContainerByName(podSpec.InitContainers, "copy-keys"))

	// Verify main container "fernet-rotate" SecurityContext (REQ-001 through REQ-004).
	expectRestrictedSecurityContext(g, findContainerByName(podSpec.Containers, "fernet-rotate"))
}

func TestReconcileFernetKeys_CronJobSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()

	fernetSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("key")},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, fernetSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-rotate",
	}, &cronJob)).To(Succeed())

	// Verify labels on CronJob ObjectMeta and pod template.
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))
	podTemplate := cronJob.Spec.JobTemplate.Spec.Template
	g.Expect(podTemplate.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(podTemplate.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(podTemplate.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))

	podSpec := podTemplate.Spec

	// Verify ServiceAccount (CC-0013).
	g.Expect(podSpec.ServiceAccountName).To(Equal("test-keystone-fernet-rotate"))

	// Verify init container copies keys from read-only Secret to writable emptyDir (CC-0013).
	g.Expect(podSpec.InitContainers).To(HaveLen(1))
	initContainer := podSpec.InitContainers[0]
	g.Expect(initContainer.Name).To(Equal("copy-keys"))
	g.Expect(initContainer.Image).To(Equal("ghcr.io/c5c3/keystone:2025.2"))
	g.Expect(initContainer.VolumeMounts).To(HaveLen(2))

	// Verify main container uses shell script for rotation + K8s API push (CC-0013).
	container := podSpec.Containers[0]
	g.Expect(container.Name).To(Equal("fernet-rotate"))
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/keystone:2025.2"))
	g.Expect(container.Command).To(Equal([]string{"/scripts/fernet_rotate.sh"}))

	// Verify env vars for Secret update via K8s API and oslo.config override (CC-0013).
	// SECRET_NAME points at the staging Secret — CronJob SA is forbidden from
	// patching the production Secret (CC-0081).
	g.Expect(container.Env).To(HaveLen(3))
	g.Expect(container.Env[0].Name).To(Equal("SECRET_NAME"))
	g.Expect(container.Env[0].Value).To(Equal("test-keystone-fernet-keys-rotation"))
	g.Expect(container.Env[1].Name).To(Equal("SECRET_NAMESPACE"))
	g.Expect(container.Env[2].Name).To(Equal("OS_fernet_tokens__max_active_keys"))
	g.Expect(container.Env[2].Value).To(Equal("3"))

	// Verify volume mounts on main container: fernet-keys + credential-keys (read-only) + config + scripts (CC-0073).
	g.Expect(container.VolumeMounts).To(HaveLen(4))
	var fernetMount, credMount, cfgMount, scriptsMount corev1.VolumeMount
	for _, vm := range container.VolumeMounts {
		switch vm.Name {
		case "fernet-keys":
			fernetMount = vm
		case "credential-keys":
			credMount = vm
		case "config":
			cfgMount = vm
		case "scripts":
			scriptsMount = vm
		}
	}
	g.Expect(fernetMount.MountPath).To(Equal("/etc/keystone/fernet-keys"))
	g.Expect(credMount.MountPath).To(Equal("/etc/keystone/credential-keys"))
	g.Expect(credMount.ReadOnly).To(BeTrue())
	g.Expect(cfgMount.MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(cfgMount.ReadOnly).To(BeTrue())
	g.Expect(scriptsMount.MountPath).To(Equal("/scripts"))
	g.Expect(scriptsMount.ReadOnly).To(BeTrue())

	// Verify volumes: fernet-keys-src (Secret), fernet-keys (emptyDir), credential-keys (Secret), config (ConfigMap), scripts (ConfigMap) (CC-0073).
	g.Expect(podSpec.Volumes).To(HaveLen(5))
	var srcVol, workVol, credVol, cfgVol, scriptsVol corev1.Volume
	for _, v := range podSpec.Volumes {
		switch v.Name {
		case "fernet-keys-src":
			srcVol = v
		case "fernet-keys":
			workVol = v
		case "credential-keys":
			credVol = v
		case "config":
			cfgVol = v
		case "scripts":
			scriptsVol = v
		}
	}
	g.Expect(srcVol.Secret).NotTo(BeNil())
	g.Expect(srcVol.Secret.SecretName).To(Equal("test-keystone-fernet-keys"))
	g.Expect(workVol.EmptyDir).NotTo(BeNil())
	g.Expect(credVol.Secret).NotTo(BeNil())
	g.Expect(credVol.Secret.SecretName).To(Equal("test-keystone-credential-keys"))
	g.Expect(cfgVol.ConfigMap).NotTo(BeNil())
	g.Expect(cfgVol.ConfigMap.Name).To(Equal("test-keystone-config-abc123"))
	g.Expect(scriptsVol.ConfigMap).NotTo(BeNil())
	g.Expect(scriptsVol.ConfigMap.Name).To(HavePrefix("test-keystone-fernet-rotate-script-"))
	g.Expect(scriptsVol.ConfigMap.DefaultMode).NotTo(BeNil())
	g.Expect(*scriptsVol.ConfigMap.DefaultMode).To(Equal(int32(0o555)))
}

// TestFernetRotateScript_EmbeddedContent verifies that the go:embed directive
// correctly loads scripts/fernet_rotate.sh into the fernetRotateScript variable.
// A broken or missing embed silently produces an empty string, which would cause
// the rotation CronJob pod to fail at runtime (CC-0073, CC-0081, REQ-007).
func TestFernetRotateScript_EmbeddedContent(t *testing.T) {
	g := NewWithT(t)

	// Guard against broken go:embed producing an empty variable.
	g.Expect(fernetRotateScript).NotTo(BeEmpty(), "fernetRotateScript must not be empty — check go:embed directive")

	// Verify POSIX shebang for standalone execution (REQ-001).
	g.Expect(fernetRotateScript).To(HavePrefix("#!/bin/sh\n"))

	// Verify SPDX Apache-2.0 license header (mandatory pattern).
	g.Expect(fernetRotateScript).To(ContainSubstring("SPDX-License-Identifier: Apache-2.0"))

	// Verify shell error propagation is enabled.
	g.Expect(fernetRotateScript).To(ContainSubstring("set -e"))

	// Verify the keystone-manage fernet_rotate command is present.
	g.Expect(fernetRotateScript).To(ContainSubstring("fernet_rotate"))

	// Verify the Python heredoc for K8s API Secret patching is present.
	// Deeper assertions on the embedded Python source are intentionally omitted:
	// they are brittle against trivial reformatting of the script. The Python
	// block's behavior is exercised by higher-level integration tests instead.
	g.Expect(fernetRotateScript).To(ContainSubstring("python3 << 'PYTHON'"))
}

// TestReconcileFernetKeys_ConditionObservedGeneration verifies that
// ObservedGeneration is set on the FernetKeysReady condition for both
// the False (GeneratingKeys) and True (FernetKeysAvailable) paths
// with distinct generation values (CC-0072, REQ-002, REQ-003).
func TestReconcileFernetKeys_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()

	// Test ObservedGeneration for the GeneratingKeys path (no existing secret).
	ks := fernetTestKeystone()
	ks.Generation = 7

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "FernetKeysReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))

	// Test ObservedGeneration for the FernetKeysAvailable path (secret exists).
	ks2 := fernetTestKeystone()
	ks2.Generation = 12

	fernetSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data: map[string][]byte{
			"0": []byte("existing-key-0"),
			"1": []byte("existing-key-1"),
			"2": []byte("existing-key-2"),
		},
	}

	c2 := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks2, fernetSecret).
		Build()

	r2 := &KeystoneReconciler{
		Client:   c2,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err = r2.reconcileFernetKeys(context.Background(), ks2, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, "FernetKeysReady")
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(12)))
}

// TestFernetRotationCronJob_PriorityClassNameSet verifies that when spec.PriorityClassName
// is set, the fernet rotation CronJob PodSpec includes the configured priority class (CC-0075).
func TestFernetRotationCronJob_PriorityClassNameSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := fernetTestKeystone()
	pcn := "system-cluster-critical"
	ks.Spec.PriorityClassName = &pcn

	cronJob := fernetRotationCronJob(ks, "test-keystone-config-abc123", "test-keystone-fernet-rotate-script-abc123")

	g.Expect(cronJob.Spec.JobTemplate.Spec.Template.Spec.PriorityClassName).To(Equal("system-cluster-critical"))
}

// TestFernetRotationCronJob_PriorityClassNameNil verifies that when spec.PriorityClassName
// is nil, the fernet rotation CronJob PodSpec has an empty priority class name (CC-0075).
func TestFernetRotationCronJob_PriorityClassNameNil(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := fernetTestKeystone()

	cronJob := fernetRotationCronJob(ks, "test-keystone-config-abc123", "test-keystone-fernet-rotate-script-abc123")

	g.Expect(cronJob.Spec.JobTemplate.Spec.Template.Spec.PriorityClassName).To(BeEmpty())
}

// findRuleForResource returns the first PolicyRule whose ResourceNames matches
// the given resource name exactly. Helper for CC-0081 RBAC split tests.
func findRuleForResource(rules []rbacv1.PolicyRule, resourceName string) *rbacv1.PolicyRule {
	for i, rule := range rules {
		if len(rule.ResourceNames) == 1 && rule.ResourceNames[0] == resourceName {
			return &rules[i]
		}
	}
	return nil
}

// countRulesForResource counts PolicyRules exactly scoped to the given
// single resource name (CC-0081).
func countRulesForResource(rules []rbacv1.PolicyRule, resourceName string) int {
	n := 0
	for _, rule := range rules {
		if len(rule.ResourceNames) == 1 && rule.ResourceNames[0] == resourceName {
			n++
		}
	}
	return n
}

// TestEnsureFernetRotationRBAC_MainSecretIsReadOnly verifies that the Role
// created by ensureFernetRotationRBAC grants only `get` on the production
// fernet keys Secret — no patch, update, create, delete, list, watch, or
// wildcard verbs (CC-0081 REQ-001).
func TestEnsureFernetRotationRBAC_MainSecretIsReadOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	g.Expect(r.ensureFernetRotationRBAC(context.Background(), ks, "test-keystone-fernet-keys")).To(Succeed())

	var role rbacv1.Role
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-rotate",
	}, &role)).To(Succeed())

	// Exactly one PolicyRule is scoped to the production Secret.
	g.Expect(countRulesForResource(role.Rules, "test-keystone-fernet-keys")).To(Equal(1))
	mainRule := findRuleForResource(role.Rules, "test-keystone-fernet-keys")
	g.Expect(mainRule).NotTo(BeNil())

	// Verbs on the production Secret are exactly {"get"} — no write verbs.
	g.Expect(mainRule.Verbs).To(Equal([]string{"get"}))

	// Defense-in-depth: scan every rule for forbidden verbs on the main Secret.
	forbidden := []string{"patch", "update", "create", "delete", "deletecollection", "list", "watch", "*"}
	for _, rule := range role.Rules {
		// Ignore rules that don't touch the production Secret.
		if len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "test-keystone-fernet-keys" {
			continue
		}
		for _, v := range rule.Verbs {
			for _, f := range forbidden {
				g.Expect(v).NotTo(Equal(f), "main Secret rule must not grant verb %q", f)
			}
		}
	}
}

// TestEnsureFernetRotationRBAC_StagingSecretHasGetPatchOnly verifies that the
// Role grants `get`+`patch` scoped to the staging Secret and nothing else
// (no create/delete/list/watch/update/wildcard) (CC-0081 REQ-003).
func TestEnsureFernetRotationRBAC_StagingSecretHasGetPatchOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	g.Expect(r.ensureFernetRotationRBAC(context.Background(), ks, "test-keystone-fernet-keys")).To(Succeed())

	var role rbacv1.Role
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-rotate",
	}, &role)).To(Succeed())

	g.Expect(countRulesForResource(role.Rules, "test-keystone-fernet-keys-rotation")).To(Equal(1))
	stagingRule := findRuleForResource(role.Rules, "test-keystone-fernet-keys-rotation")
	g.Expect(stagingRule).NotTo(BeNil())

	// Verbs order-independent comparison: exactly {"get", "patch"}.
	verbs := append([]string{}, stagingRule.Verbs...)
	sort.Strings(verbs)
	g.Expect(verbs).To(Equal([]string{"get", "patch"}))

	// APIGroups + Resources must match core/secrets.
	g.Expect(stagingRule.APIGroups).To(Equal([]string{""}))
	g.Expect(stagingRule.Resources).To(Equal([]string{"secrets"}))
}

// TestReconcileFernetKeys_CreatesEmptyStagingSecret verifies that
// reconcileFernetKeys creates an empty staging Secret for the Fernet rotation
// CronJob to PATCH into (CC-0081). The Secret's Data must be left nil/empty
// on creation; the operator owns the lifecycle while the CronJob owns Data.
func TestReconcileFernetKeys_CreatesEmptyStagingSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()

	// Pre-create the production fernet keys Secret so reconcileFernetKeys
	// proceeds past the initial creation+requeue step.
	fernetSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-fernet-keys",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"0": []byte("existing-key-0"),
			"1": []byte("existing-key-1"),
			"2": []byte("existing-key-2"),
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, fernetSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// Staging Secret must exist with empty Data and the correct labels and owner.
	var staging corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-keys-rotation",
	}, &staging)).To(Succeed())

	g.Expect(staging.Data).To(BeEmpty())

	g.Expect(staging.Labels).To(HaveKeyWithValue(StagingSecretLabelKey, "fernet-keys"))
	g.Expect(staging.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(staging.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(staging.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))

	g.Expect(staging.OwnerReferences).To(HaveLen(1))
	g.Expect(staging.OwnerReferences[0].Name).To(Equal("test-keystone"))
}

// TestReconcileFernetKeys_AppliesStagedKeysWhenAnnotationPresent verifies that
// reconcileFernetKeys applies a completed staging Secret onto the production
// fernet keys Secret, deletes the staging Secret, and short-circuits with
// Requeue=true when the rotation-completed annotation is present (CC-0081,
// REQ-005, REQ-006).
func TestReconcileFernetKeys_AppliesStagedKeysWhenAnnotationPresent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()

	// Pre-create the production fernet keys Secret with 3 old keys.
	oldKeys := make(map[string][]byte, 3)
	for i := range 3 {
		k, err := generateFernetKey()
		g.Expect(err).NotTo(HaveOccurred())
		oldKeys[strconv.Itoa(i)] = []byte(k)
	}
	fernetSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-fernet-keys",
			Namespace: "default",
		},
		Data: oldKeys,
	}

	// Pre-create the staging Secret with 3 freshly-generated keys and a
	// valid RFC3339 UTC rotation-completed annotation (CC-0081).
	newKeys := make(map[string][]byte, 3)
	for i := range 3 {
		k, err := generateFernetKey()
		g.Expect(err).NotTo(HaveOccurred())
		newKeys[strconv.Itoa(i)] = []byte(k)
	}
	stagingLabels := commonLabels(ks)
	stagingLabels[StagingSecretLabelKey] = "fernet-keys"
	stagingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-fernet-keys-rotation",
			Namespace: "default",
			Labels:    stagingLabels,
			Annotations: map[string]string{
				RotationCompletedAnnotation: "2026-01-01T00:00:00Z",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "keystone.c5c3.io/v1alpha1",
				Kind:       "Keystone",
				Name:       ks.Name,
				UID:        ks.UID,
			}},
		},
		Data: newKeys,
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, fernetSecret, stagingSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileFernetKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{Requeue: true}))

	// Production Secret now contains the exact data from staging.
	var updated corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-keys",
	}, &updated)).To(Succeed())
	g.Expect(updated.Data).To(Equal(newKeys))

	// Staging Secret no longer exists.
	var staging corev1.Secret
	getErr := c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-keys-rotation",
	}, &staging)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "staging Secret should be deleted after apply")

	// FernetKeysRotated event was emitted.
	expectEvent(g, r, "Normal FernetKeysRotated")

	// FernetKeysReady flipped to True with FernetKeysRotated reason on the
	// apply-success short-circuit path; the message reflects the just-applied
	// rotation rather than the steady-state text (CC-0081).
	cond := meta.FindStatusCondition(ks.Status.Conditions, "FernetKeysReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("FernetKeysRotated"))
	g.Expect(cond.Message).To(Equal("rotation applied; staging secret cleared"))
}

// TestEnsureFernetRotationRBAC_IsIdempotent_CC0081 verifies that calling
// ensureFernetRotationRBAC twice produces the same Role Rules, matching the
// manual-get/create/update pattern used throughout the package.
func TestEnsureFernetRotationRBAC_IsIdempotent_CC0081(t *testing.T) {
	g := NewGomegaWithT(t)
	s := fernetTestScheme()
	ks := fernetTestKeystone()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	g.Expect(r.ensureFernetRotationRBAC(context.Background(), ks, "test-keystone-fernet-keys")).To(Succeed())
	var first rbacv1.Role
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-rotate",
	}, &first)).To(Succeed())
	rulesFirst := append([]rbacv1.PolicyRule{}, first.Rules...)

	g.Expect(r.ensureFernetRotationRBAC(context.Background(), ks, "test-keystone-fernet-keys")).To(Succeed())
	var second rbacv1.Role
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-fernet-rotate",
	}, &second)).To(Succeed())

	g.Expect(second.Rules).To(Equal(rulesFirst))
}
