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

// credentialTestScheme returns a runtime.Scheme with all types needed for credential tests.
func credentialTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	_ = esov1alpha1.SchemeBuilder.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

// credentialTestKeystone returns a minimal Keystone CR for reconcileCredentialKeys tests.
func credentialTestKeystone() *keystonev1alpha1.Keystone {
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
			CredentialKeys: keystonev1alpha1.CredentialKeysSpec{
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

func TestReconcileCredentialKeys_NoSecret_CreatesSecretAndRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")

	g.Expect(err).NotTo(HaveOccurred())
	// Must requeue to confirm the secret is available before proceeding (CC-0036).
	g.Expect(result).To(Equal(ctrl.Result{Requeue: true}))

	// Verify the Secret was created with the right number of keys.
	var secret corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-keys",
	}, &secret)).To(Succeed())
	g.Expect(secret.Data).To(HaveLen(3))
	g.Expect(secret.OwnerReferences).To(HaveLen(1))
	g.Expect(secret.OwnerReferences[0].Name).To(Equal("test-keystone"))
	g.Expect(secret.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(secret.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(secret.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))

	// CronJob and PushSecret are NOT created on this cycle (early return after secret creation).

	// Verify CredentialKeysReady condition is False (will be True on next reconcile).
	cond := meta.FindStatusCondition(ks.Status.Conditions, "CredentialKeysReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("GeneratingKeys"))

	expectEvent(g, r, "Normal CredentialKeysGenerated")
}

func TestReconcileCredentialKeys_SecretAlreadyExists(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()

	// Pre-create the credential keys Secret.
	credentialSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-credential-keys",
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
		WithObjects(ks, credentialSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Verify the Secret was not re-created (data unchanged).
	var secret corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-keys",
	}, &secret)).To(Succeed())
	g.Expect(string(secret.Data["0"])).To(Equal("existing-key-0"))

	// Verify the script ConfigMap was created (CC-0073).
	var cmList corev1.ConfigMapList
	g.Expect(c.List(context.Background(), &cmList, client.InNamespace("default"))).To(Succeed())
	var scriptCM *corev1.ConfigMap
	for i := range cmList.Items {
		if strings.HasPrefix(cmList.Items[i].Name, "test-keystone-credential-rotate-script-") {
			scriptCM = &cmList.Items[i]
			break
		}
	}
	g.Expect(scriptCM).NotTo(BeNil(), "script ConfigMap with prefix test-keystone-credential-rotate-script- should exist")
	g.Expect(scriptCM.Data).To(HaveKey("credential_rotate.sh"))
	g.Expect(scriptCM.Immutable).NotTo(BeNil())
	g.Expect(*scriptCM.Immutable).To(BeTrue())

	// Verify CronJob and PushSecret were still created.
	var cronJob batchv1.CronJob
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-rotate",
	}, &cronJob)).To(Succeed())
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))

	var ps esov1alpha1.PushSecret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-keys-backup",
	}, &ps)).To(Succeed())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "CredentialKeysReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

	expectNoEvent(g, r)
}

func TestReconcileCredentialKeys_CronJobScheduleUpdated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()

	// Pre-create Secret and CronJob with old schedule.
	credentialSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-credential-keys",
			Namespace: "default",
		},
		Data: map[string][]byte{"0": []byte("key")},
	}

	oldCronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-credential-rotate",
			Namespace: "default",
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 1 * *", // old schedule (monthly)
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers:    []corev1.Container{{Name: "credential-rotate", Image: "old:image"}},
						},
					},
				},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, credentialSecret, oldCronJob).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	// Change the schedule in the spec.
	ks.Spec.CredentialKeys.RotationSchedule = "0 */6 * * *"

	result, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Verify the CronJob schedule was updated.
	var cronJob batchv1.CronJob
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-rotate",
	}, &cronJob)).To(Succeed())
	g.Expect(cronJob.Spec.Schedule).To(Equal("0 */6 * * *"))
}

func TestReconcileCredentialKeys_GeneratedKeysAreValid(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()
	ks.Spec.CredentialKeys.MaxActiveKeys = 5

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var secret corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-keys",
	}, &secret)).To(Succeed())

	g.Expect(secret.Data).To(HaveLen(5))

	for i := 0; i < 5; i++ {
		keyStr := string(secret.Data[strconv.Itoa(i)])
		decoded, err := base64.URLEncoding.DecodeString(keyStr)
		g.Expect(err).NotTo(HaveOccurred(), "key %d should be valid base64url", i)
		g.Expect(decoded).To(HaveLen(32), "key %d should decode to 32 bytes", i)
	}
}

func TestReconcileCredentialKeys_CronJobScheduleMatchesSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()
	ks.Spec.CredentialKeys.RotationSchedule = "30 2 * * 1"

	credentialSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-credential-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("key")},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, credentialSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-rotate",
	}, &cronJob)).To(Succeed())
	g.Expect(cronJob.Spec.Schedule).To(Equal("30 2 * * 1"))
}

func TestReconcileCredentialKeys_PushSecretReferencesCorrectSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()

	credentialSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-credential-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("key")},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, credentialSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var ps esov1alpha1.PushSecret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-keys-backup",
	}, &ps)).To(Succeed())

	g.Expect(ps.Spec.Selector.Secret).NotTo(BeNil())
	g.Expect(ps.Spec.Selector.Secret.Name).To(Equal("test-keystone-credential-keys"))
	g.Expect(ps.Spec.SecretStoreRefs).To(HaveLen(1))
	g.Expect(ps.Spec.SecretStoreRefs[0].Kind).To(Equal("ClusterSecretStore"))
	g.Expect(ps.Spec.SecretStoreRefs[0].Name).To(Equal("openbao-cluster-store"))
	g.Expect(ps.Spec.Data).To(HaveLen(1))
	g.Expect(ps.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal("openstack/keystone/credential-keys"))
}

func TestCredentialKeyGeneration_Valid(t *testing.T) {
	g := NewGomegaWithT(t)

	// Credential keys reuse generateFernetKey (same 32-byte base64url format) (CC-0036).
	key, err := generateFernetKey()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(key).NotTo(BeEmpty())

	decoded, err := base64.URLEncoding.DecodeString(key)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decoded).To(HaveLen(32))
}

func TestCredentialKeyGeneration_Unique(t *testing.T) {
	g := NewGomegaWithT(t)

	// Credential keys reuse generateFernetKey (same 32-byte base64url format) (CC-0036).
	key1, err := generateFernetKey()
	g.Expect(err).NotTo(HaveOccurred())

	key2, err := generateFernetKey()
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(key1).NotTo(Equal(key2))
}

func TestReconcileCredentialKeys_MinActiveKeysFloor(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()
	// Set MaxActiveKeys below the floor of 3.
	ks.Spec.CredentialKeys.MaxActiveKeys = 1

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var secret corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-keys",
	}, &secret)).To(Succeed())

	// Even with MaxActiveKeys=1, at least 3 keys should be generated (CC-0036, REQ-009).
	g.Expect(secret.Data).To(HaveLen(3))

	for i := 0; i < 3; i++ {
		g.Expect(secret.Data).To(HaveKey(strconv.Itoa(i)))
	}
}

func TestReconcileCredentialKeys_ConditionMessages(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()

	credentialSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-credential-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("key"), "1": []byte("key"), "2": []byte("key")},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, credentialSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// The final condition should be CredentialKeysAvailable with the correct message.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "CredentialKeysReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("CredentialKeysAvailable"))
	g.Expect(cond.Message).To(Equal("Credential keys Secret exists and rotation CronJob is configured"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

// TestCredentialRotationCronJob_SecurityContext verifies that both containers in the
// credential rotation CronJob have a restricted SecurityContext (CC-0045).
func TestCredentialRotationCronJob_SecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := credentialTestKeystone()

	cronJob := credentialRotationCronJob(ks, "test-keystone-config-abc123", "test-keystone-credential-rotate-script-abc123")

	podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec

	// Verify init container "copy-keys" SecurityContext (REQ-001 through REQ-004).
	expectRestrictedSecurityContext(g, findContainerByName(podSpec.InitContainers, "copy-keys"))

	// Verify main container "credential-rotate" SecurityContext (REQ-001 through REQ-004).
	expectRestrictedSecurityContext(g, findContainerByName(podSpec.Containers, "credential-rotate"))
}

func TestReconcileCredentialKeys_CronJobSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()

	credentialSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-credential-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("key")},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, credentialSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-rotate",
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

	// Verify ServiceAccount (CC-0036).
	g.Expect(podSpec.ServiceAccountName).To(Equal("test-keystone-credential-rotate"))

	// Verify init container copies keys from read-only Secret to writable emptyDir (CC-0036).
	g.Expect(podSpec.InitContainers).To(HaveLen(1))
	initContainer := podSpec.InitContainers[0]
	g.Expect(initContainer.Name).To(Equal("copy-keys"))
	g.Expect(initContainer.Image).To(Equal("ghcr.io/c5c3/keystone:2025.2"))
	g.Expect(initContainer.VolumeMounts).To(HaveLen(2))

	// Verify main container uses script from versioned ConfigMap (CC-0073).
	container := podSpec.Containers[0]
	g.Expect(container.Name).To(Equal("credential-rotate"))
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/keystone:2025.2"))
	g.Expect(container.Command).To(Equal([]string{"/scripts/credential_rotate.sh"}))

	// Verify env vars for Secret update via K8s API and oslo.config override (CC-0036).
	// SECRET_NAME points at the staging Secret — CronJob SA is forbidden from
	// patching the production Secret (CC-0081).
	g.Expect(container.Env).To(HaveLen(3))
	g.Expect(container.Env[0].Name).To(Equal("SECRET_NAME"))
	g.Expect(container.Env[0].Value).To(Equal("test-keystone-credential-keys-rotation"))
	g.Expect(container.Env[1].Name).To(Equal("SECRET_NAMESPACE"))
	g.Expect(container.Env[2].Name).To(Equal("OS_credential__max_active_keys"))
	g.Expect(container.Env[2].Value).To(Equal("3"))

	// Verify volume mounts on main container: credential-keys + fernet-keys (read-only) + config + scripts (CC-0073).
	g.Expect(container.VolumeMounts).To(HaveLen(4))
	var credMount, fernetMount, cfgMount, scriptsMount corev1.VolumeMount
	for _, vm := range container.VolumeMounts {
		switch vm.Name {
		case "credential-keys":
			credMount = vm
		case "fernet-keys":
			fernetMount = vm
		case "config":
			cfgMount = vm
		case "scripts":
			scriptsMount = vm
		}
	}
	g.Expect(credMount.MountPath).To(Equal("/etc/keystone/credential-keys"))
	g.Expect(fernetMount.MountPath).To(Equal("/etc/keystone/fernet-keys"))
	g.Expect(fernetMount.ReadOnly).To(BeTrue())
	g.Expect(cfgMount.MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(cfgMount.ReadOnly).To(BeTrue())
	g.Expect(scriptsMount.MountPath).To(Equal("/scripts"))
	g.Expect(scriptsMount.ReadOnly).To(BeTrue())

	// Verify volumes: credential-keys-src (Secret), credential-keys (emptyDir), fernet-keys (Secret), config (ConfigMap), scripts (ConfigMap) (CC-0073).
	g.Expect(podSpec.Volumes).To(HaveLen(5))
	var srcVol, workVol, fernetVol, cfgVol, scriptsVol corev1.Volume
	for _, v := range podSpec.Volumes {
		switch v.Name {
		case "credential-keys-src":
			srcVol = v
		case "credential-keys":
			workVol = v
		case "fernet-keys":
			fernetVol = v
		case "config":
			cfgVol = v
		case "scripts":
			scriptsVol = v
		}
	}
	g.Expect(srcVol.Secret).NotTo(BeNil())
	g.Expect(srcVol.Secret.SecretName).To(Equal("test-keystone-credential-keys"))
	g.Expect(workVol.EmptyDir).NotTo(BeNil())
	g.Expect(fernetVol.Secret).NotTo(BeNil())
	g.Expect(fernetVol.Secret.SecretName).To(Equal("test-keystone-fernet-keys"))
	g.Expect(cfgVol.ConfigMap).NotTo(BeNil())
	g.Expect(cfgVol.ConfigMap.Name).To(Equal("test-keystone-config-abc123"))
	g.Expect(scriptsVol.ConfigMap).NotTo(BeNil())
	g.Expect(scriptsVol.ConfigMap.Name).To(HavePrefix("test-keystone-credential-rotate-script-"))
	g.Expect(scriptsVol.ConfigMap.DefaultMode).NotTo(BeNil())
	g.Expect(*scriptsVol.ConfigMap.DefaultMode).To(Equal(int32(0o555)))
}

// TestCredentialRotateScript_EmbeddedContent verifies that the go:embed directive
// correctly loads scripts/credential_rotate.sh into the credentialRotateScript variable.
// A broken or missing embed silently produces an empty string, which would cause
// the rotation CronJob pod to fail at runtime (CC-0073, CC-0081, REQ-007).
func TestCredentialRotateScript_EmbeddedContent(t *testing.T) {
	g := NewWithT(t)

	// Guard against broken go:embed producing an empty variable.
	g.Expect(credentialRotateScript).NotTo(BeEmpty(), "credentialRotateScript must not be empty — check go:embed directive")

	// Verify POSIX shebang for standalone execution (REQ-001).
	g.Expect(credentialRotateScript).To(HavePrefix("#!/bin/sh\n"))

	// Verify SPDX Apache-2.0 license header (mandatory pattern).
	g.Expect(credentialRotateScript).To(ContainSubstring("SPDX-License-Identifier: Apache-2.0"))

	// Verify shell error propagation is enabled.
	g.Expect(credentialRotateScript).To(ContainSubstring("set -e"))

	// Verify both credential rotation commands are present (CC-0036).
	g.Expect(credentialRotateScript).To(ContainSubstring("credential_rotate"))
	g.Expect(credentialRotateScript).To(ContainSubstring("credential_migrate"))

	// Verify the Python heredoc for K8s API Secret patching is present.
	// Deeper assertions on the embedded Python source are intentionally omitted:
	// they are brittle against trivial reformatting of the script. The Python
	// block's behavior is exercised by higher-level integration tests instead.
	g.Expect(credentialRotateScript).To(ContainSubstring("python3 << 'PYTHON'"))
}

// TestCredentialRotateScript_RotateBeforeMigrate verifies that the credential
// rotation script invokes keystone-manage credential_rotate before
// credential_migrate, and that the Python K8s API PATCH block is present.
// credential_rotate must precede credential_migrate so that the active keyset
// is rotated first before existing credentials are re-encrypted under the
// new keys (CC-0036, CC-0081, REQ-008).
func TestCredentialRotateScript_RotateBeforeMigrate(t *testing.T) {
	g := NewWithT(t)

	g.Expect(credentialRotateScript).NotTo(BeEmpty(), "credentialRotateScript must not be empty — check go:embed directive")

	rotateIdx := strings.Index(credentialRotateScript, "credential_rotate")
	migrateIdx := strings.Index(credentialRotateScript, "credential_migrate")

	g.Expect(rotateIdx).To(BeNumerically(">=", 0), "credential_rotate invocation must be present")
	g.Expect(migrateIdx).To(BeNumerically(">=", 0), "credential_migrate invocation must be present")
	g.Expect(strings.Index(credentialRotateScript, "python3 << 'PYTHON'")).To(BeNumerically(">=", 0), "python3 PATCH heredoc must be present")

	g.Expect(rotateIdx).To(BeNumerically("<", migrateIdx),
		"credential_rotate must run before credential_migrate so the active keyset is rotated first")
}

// TestReconcileCredentialKeys_ConditionObservedGeneration verifies that
// ObservedGeneration is set on the CredentialKeysReady condition for both
// the False (GeneratingKeys) and True (CredentialKeysAvailable) paths
// with distinct generation values (CC-0072, REQ-002, REQ-003).
func TestReconcileCredentialKeys_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()

	// Test ObservedGeneration for the GeneratingKeys path (no existing secret).
	ks := credentialTestKeystone()
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

	_, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "CredentialKeysReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))

	// Test ObservedGeneration for the CredentialKeysAvailable path (secret exists).
	ks2 := credentialTestKeystone()
	ks2.Generation = 12

	credentialSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-credential-keys", Namespace: "default"},
		Data: map[string][]byte{
			"0": []byte("existing-key-0"),
			"1": []byte("existing-key-1"),
			"2": []byte("existing-key-2"),
		},
	}

	c2 := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks2, credentialSecret).
		Build()

	r2 := &KeystoneReconciler{
		Client:   c2,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err = r2.reconcileCredentialKeys(context.Background(), ks2, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, "CredentialKeysReady")
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(12)))
}

// TestCredentialRotationCronJob_PriorityClassNameSet verifies that when spec.PriorityClassName
// is set, the credential rotation CronJob PodSpec includes the configured priority class (CC-0075).
func TestCredentialRotationCronJob_PriorityClassNameSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := credentialTestKeystone()
	pcn := "system-cluster-critical"
	ks.Spec.PriorityClassName = &pcn

	cronJob := credentialRotationCronJob(ks, "test-keystone-config-abc123", "test-keystone-credential-rotate-script-abc123")

	g.Expect(cronJob.Spec.JobTemplate.Spec.Template.Spec.PriorityClassName).To(Equal("system-cluster-critical"))
}

// TestCredentialRotationCronJob_PriorityClassNameNil verifies that when spec.PriorityClassName
// is nil, the credential rotation CronJob PodSpec has an empty priority class name (CC-0075).
func TestCredentialRotationCronJob_PriorityClassNameNil(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := credentialTestKeystone()

	cronJob := credentialRotationCronJob(ks, "test-keystone-config-abc123", "test-keystone-credential-rotate-script-abc123")

	g.Expect(cronJob.Spec.JobTemplate.Spec.Template.Spec.PriorityClassName).To(BeEmpty())
}

// TestEnsureCredentialRotationRBAC_MainSecretIsReadOnly verifies that the Role
// created by ensureCredentialRotationRBAC grants only `get` on the production
// credential keys Secret — no patch, update, create, delete, list, watch, or
// wildcard verbs (CC-0081 REQ-002).
func TestEnsureCredentialRotationRBAC_MainSecretIsReadOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	g.Expect(r.ensureCredentialRotationRBAC(context.Background(), ks, "test-keystone-credential-keys")).To(Succeed())

	var role rbacv1.Role
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-rotate",
	}, &role)).To(Succeed())

	// Exactly one PolicyRule is scoped to the production Secret.
	g.Expect(countRulesForResource(role.Rules, "test-keystone-credential-keys")).To(Equal(1))
	mainRule := findRuleForResource(role.Rules, "test-keystone-credential-keys")
	g.Expect(mainRule).NotTo(BeNil())

	// Verbs on the production Secret are exactly {"get"} — no write verbs.
	g.Expect(mainRule.Verbs).To(Equal([]string{"get"}))

	// Defense-in-depth: scan every rule for forbidden verbs on the main Secret.
	forbidden := []string{"patch", "update", "create", "delete", "deletecollection", "list", "watch", "*"}
	for _, rule := range role.Rules {
		if len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "test-keystone-credential-keys" {
			continue
		}
		for _, v := range rule.Verbs {
			for _, f := range forbidden {
				g.Expect(v).NotTo(Equal(f), "main Secret rule must not grant verb %q", f)
			}
		}
	}
}

// TestEnsureCredentialRotationRBAC_StagingSecretHasGetPatchOnly verifies that
// the Role grants `get`+`patch` scoped to the staging Secret and nothing else
// (no create/delete/list/watch/update/wildcard) (CC-0081 REQ-003).
func TestEnsureCredentialRotationRBAC_StagingSecretHasGetPatchOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	g.Expect(r.ensureCredentialRotationRBAC(context.Background(), ks, "test-keystone-credential-keys")).To(Succeed())

	var role rbacv1.Role
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-rotate",
	}, &role)).To(Succeed())

	g.Expect(countRulesForResource(role.Rules, "test-keystone-credential-keys-rotation")).To(Equal(1))
	stagingRule := findRuleForResource(role.Rules, "test-keystone-credential-keys-rotation")
	g.Expect(stagingRule).NotTo(BeNil())

	// Verbs order-independent comparison: exactly {"get", "patch"}.
	verbs := append([]string{}, stagingRule.Verbs...)
	sort.Strings(verbs)
	g.Expect(verbs).To(Equal([]string{"get", "patch"}))

	// APIGroups + Resources must match core/secrets.
	g.Expect(stagingRule.APIGroups).To(Equal([]string{""}))
	g.Expect(stagingRule.Resources).To(Equal([]string{"secrets"}))

	// The staging rule must NOT grant create or delete (operator owns lifecycle).
	forbidden := []string{"create", "delete", "deletecollection", "update", "list", "watch", "*"}
	for _, v := range stagingRule.Verbs {
		for _, f := range forbidden {
			g.Expect(v).NotTo(Equal(f), "staging Secret rule must not grant verb %q", f)
		}
	}
}

// TestReconcileCredentialKeys_CreatesEmptyStagingSecret verifies that
// reconcileCredentialKeys ensures a dedicated staging Secret exists for the
// credential key rotation handoff. The Secret is created empty (no Data) so
// that only the rotation CronJob populates it via PATCH (CC-0081 REQ-004).
func TestReconcileCredentialKeys_CreatesEmptyStagingSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()

	// Pre-create the production credential keys Secret so the flow reaches
	// step 2+ (staging Secret + RBAC + CronJob + PushSecret).
	credentialSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-credential-keys",
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
		WithObjects(ks, credentialSecret).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the staging Secret exists with the expected name (CC-0081).
	var staging corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-keys-rotation",
	}, &staging)).To(Succeed())

	// Data must be nil/empty; only the CronJob writes via PATCH.
	g.Expect(staging.Data).To(BeEmpty())

	// Labels include the rotation-target marker plus all three commonLabels.
	g.Expect(staging.Labels).To(HaveKeyWithValue("forge.c5c3.io/rotation-target", "credential-keys"))
	g.Expect(staging.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(staging.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(staging.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))

	// Exactly one OwnerReference pointing at the Keystone CR.
	g.Expect(staging.OwnerReferences).To(HaveLen(1))
	g.Expect(staging.OwnerReferences[0].Name).To(Equal("test-keystone"))
	g.Expect(staging.OwnerReferences[0].Kind).To(Equal("Keystone"))
}

// TestEnsureCredentialRotationRBAC_IsIdempotent_CC0081 verifies that calling
// ensureCredentialRotationRBAC twice produces the same Role Rules, matching the
// manual-get/create/update pattern used throughout the package.
func TestEnsureCredentialRotationRBAC_IsIdempotent_CC0081(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	g.Expect(r.ensureCredentialRotationRBAC(context.Background(), ks, "test-keystone-credential-keys")).To(Succeed())
	var first rbacv1.Role
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-rotate",
	}, &first)).To(Succeed())
	rulesFirst := append([]rbacv1.PolicyRule{}, first.Rules...)

	g.Expect(r.ensureCredentialRotationRBAC(context.Background(), ks, "test-keystone-credential-keys")).To(Succeed())
	var second rbacv1.Role
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-rotate",
	}, &second)).To(Succeed())

	g.Expect(second.Rules).To(Equal(rulesFirst))
}

// TestReconcileCredentialKeys_AppliesStagedKeysWhenAnnotationPresent verifies
// that reconcileCredentialKeys wires applyRotationOutput into step 3 so that a
// completed staging Secret (RotationCompletedAnnotation set to a well-formed
// RFC3339 timestamp, valid Data) is applied to the production Secret, the
// staging Secret is deleted, a Normal "CredentialKeysRotated" event is
// emitted, CredentialKeysReady flips to True, and the reconcile short-circuits
// with Requeue: true (CC-0081 REQ-005, REQ-006).
func TestReconcileCredentialKeys_AppliesStagedKeysWhenAnnotationPresent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := credentialTestScheme()
	ks := credentialTestKeystone()

	// Pre-create production credential keys Secret with 3 old keys.
	prod := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-credential-keys",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"0": []byte("old-key-0"),
			"1": []byte("old-key-1"),
			"2": []byte("old-key-2"),
		},
	}

	// Build 3 valid Fernet-format keys for the staged payload.
	stagedData := make(map[string][]byte, 3)
	for i := 0; i < 3; i++ {
		k, err := generateFernetKey()
		g.Expect(err).NotTo(HaveOccurred())
		stagedData[strconv.Itoa(i)] = []byte(k)
	}

	// Pre-create the staging Secret the operator normally ensures, with the
	// RotationCompletedAnnotation already set (simulating the CronJob having
	// finished its PATCH).
	labels := commonLabels(ks)
	labels[StagingSecretLabelKey] = "credential-keys"
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-credential-keys-rotation",
			Namespace: "default",
			Labels:    labels,
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
		Data: stagedData,
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, prod, staging).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileCredentialKeys(context.Background(), ks, "test-keystone-config-abc123")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Requeue).To(BeTrue())

	// Production Secret data was swapped for the staged data.
	var gotProd corev1.Secret
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-keys",
	}, &gotProd)).To(Succeed())
	g.Expect(gotProd.Data).To(HaveLen(len(stagedData)))
	for k, v := range stagedData {
		g.Expect(gotProd.Data).To(HaveKeyWithValue(k, v))
	}

	// Staging Secret deleted.
	var gotStaging corev1.Secret
	getErr := c.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "test-keystone-credential-keys-rotation",
	}, &gotStaging)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "staging Secret should be deleted after apply")

	// Normal CredentialKeysRotated event emitted.
	expectEvent(g, r, "Normal CredentialKeysRotated")

	// CredentialKeysReady flipped to True with CredentialKeysRotated reason on
	// the apply-success short-circuit path; the message reflects the just-
	// applied rotation rather than the steady-state text (CC-0081).
	cond := meta.FindStatusCondition(ks.Status.Conditions, "CredentialKeysReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("CredentialKeysRotated"))
	g.Expect(cond.Message).To(Equal("rotation applied; staging secret cleared"))
}
