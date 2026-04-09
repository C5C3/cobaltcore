// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/base64"
	"strconv"
	"testing"

	. "github.com/onsi/gomega"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	g.Expect(ps.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal("kv-v2/data/openstack/keystone/credential-keys"))
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

	cronJob := credentialRotationCronJob(ks, "test-keystone-config-abc123")

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

	// Verify main container uses shell script for rotation + migration + K8s API push (CC-0036).
	container := podSpec.Containers[0]
	g.Expect(container.Name).To(Equal("credential-rotate"))
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/keystone:2025.2"))
	g.Expect(container.Command).To(Equal([]string{"sh", "-c", credentialRotateScript}))

	// Verify the rotation script includes both credential_rotate and credential_migrate (CC-0036).
	g.Expect(credentialRotateScript).To(ContainSubstring("credential_rotate"))
	g.Expect(credentialRotateScript).To(ContainSubstring("credential_migrate"))

	// Verify env vars for Secret update via K8s API and oslo.config override (CC-0036).
	g.Expect(container.Env).To(HaveLen(3))
	g.Expect(container.Env[0].Name).To(Equal("SECRET_NAME"))
	g.Expect(container.Env[0].Value).To(Equal("test-keystone-credential-keys"))
	g.Expect(container.Env[1].Name).To(Equal("SECRET_NAMESPACE"))
	g.Expect(container.Env[2].Name).To(Equal("OS_credential__max_active_keys"))
	g.Expect(container.Env[2].Value).To(Equal("3"))

	// Verify volume mounts on main container: credential-keys + fernet-keys (read-only) + config.
	g.Expect(container.VolumeMounts).To(HaveLen(3))
	g.Expect(container.VolumeMounts[0].Name).To(Equal("credential-keys"))
	g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/credential-keys"))
	g.Expect(container.VolumeMounts[1].Name).To(Equal("fernet-keys"))
	g.Expect(container.VolumeMounts[1].MountPath).To(Equal("/etc/keystone/fernet-keys"))
	g.Expect(container.VolumeMounts[1].ReadOnly).To(BeTrue())
	g.Expect(container.VolumeMounts[2].Name).To(Equal("config"))
	g.Expect(container.VolumeMounts[2].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(container.VolumeMounts[2].ReadOnly).To(BeTrue())

	// Verify volumes: credential-keys-src (Secret), credential-keys (emptyDir), fernet-keys (Secret), config (ConfigMap).
	g.Expect(podSpec.Volumes).To(HaveLen(4))
	var srcVol, workVol, fernetVol, cfgVol corev1.Volume
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
		}
	}
	g.Expect(srcVol.Secret).NotTo(BeNil())
	g.Expect(srcVol.Secret.SecretName).To(Equal("test-keystone-credential-keys"))
	g.Expect(workVol.EmptyDir).NotTo(BeNil())
	g.Expect(fernetVol.Secret).NotTo(BeNil())
	g.Expect(fernetVol.Secret.SecretName).To(Equal("test-keystone-fernet-keys"))
	g.Expect(cfgVol.ConfigMap).NotTo(BeNil())
	g.Expect(cfgVol.ConfigMap.Name).To(Equal("test-keystone-config-abc123"))
}
