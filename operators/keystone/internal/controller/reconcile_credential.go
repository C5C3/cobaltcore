// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/job"
	"github.com/c5c3/forge/internal/common/secrets"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// credentialRotateScript is the shell script executed by the credential rotation CronJob.
// It rotates credential keys, migrates existing encrypted credentials in the database
// to use the new primary key, then pushes the updated keys back to the Kubernetes
// Secret via the API using the pod's ServiceAccount token.
// The credential_migrate step is critical: without it, credentials encrypted with the
// old primary key become inaccessible once that key is purged (CC-0036).
const credentialRotateScript = `set -e
keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ credential_rotate
keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ credential_migrate
python3 << 'PYTHON'
import os, json, base64, glob, ssl, http.client
data = {}
for f in sorted(glob.glob("/etc/keystone/credential-keys/*")):
    if os.path.isfile(f):
        with open(f, "rb") as fh:
            data[os.path.basename(f)] = base64.b64encode(fh.read()).decode()
with open("/var/run/secrets/kubernetes.io/serviceaccount/token") as f:
    token = f.read().strip()
ctx = ssl.create_default_context(cafile="/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
conn = http.client.HTTPSConnection("kubernetes.default.svc", context=ctx)
conn.request("PATCH",
    "/api/v1/namespaces/{}/secrets/{}".format(os.environ["SECRET_NAMESPACE"], os.environ["SECRET_NAME"]),
    json.dumps({"data": data}),
    {"Authorization": "Bearer " + token, "Content-Type": "application/strategic-merge-patch+json"})
resp = conn.getresponse()
if resp.status >= 300:
    raise RuntimeError("Secret update failed: {} {}".format(resp.status, resp.read().decode()))
print("Credential keys Secret updated successfully")
PYTHON`

// normalizedCredentialMaxActiveKeys returns the effective maximum number of active
// credential keys, applying a minimum floor of 3. The webhook defaults 0 to 3, but
// this provides defense-in-depth for the reconciler (CC-0036).
func normalizedCredentialMaxActiveKeys(keystone *keystonev1alpha1.Keystone) int {
	return max(int(keystone.Spec.CredentialKeys.MaxActiveKeys), 3)
}

// reconcileCredentialKeys ensures that a credential keys Secret exists, a rotation
// CronJob is configured, and a PushSecret backs up the keys to OpenBao (CC-0036).
func (r *KeystoneReconciler) reconcileCredentialKeys(ctx context.Context,
	keystone *keystonev1alpha1.Keystone, configMapName string,
) (ctrl.Result, error) {
	// 1. Ensure the credential keys Secret exists.
	secretName := fmt.Sprintf("%s-credential-keys", keystone.Name)

	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: keystone.Namespace, Name: secretName}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.createCredentialKeysSecret(ctx, keystone, secretName); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating credential keys secret: %w", err)
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "CredentialKeysReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "GeneratingKeys",
			Message:            "Initial credential keys have been generated",
		})
		// Requeue to confirm the secret is available before proceeding (CC-0036).
		return ctrl.Result{Requeue: true}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting credential keys secret: %w", err)
	}

	// 2. Ensure the RBAC resources for the rotation CronJob exist.
	if err := r.ensureCredentialRotationRBAC(ctx, keystone, secretName); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring credential rotation RBAC: %w", err)
	}

	// 3. Ensure the rotation CronJob exists.
	cronJob := credentialRotationCronJob(keystone, configMapName)
	if err := job.EnsureCronJob(ctx, r.Client, r.Scheme, keystone, cronJob); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring credential rotation cronjob: %w", err)
	}

	// 4. Ensure the PushSecret for OpenBao backup exists.
	ps := credentialKeysPushSecret(keystone)
	if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, keystone, ps); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring credential keys pushsecret: %w", err)
	}

	// 5. Set the CredentialKeysReady condition.
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "CredentialKeysReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "CredentialKeysAvailable",
		Message:            "Credential keys Secret exists and rotation CronJob is configured",
	})

	return ctrl.Result{}, nil
}

// ensureCredentialRotationRBAC creates the ServiceAccount, Role, and RoleBinding
// needed by the credential rotation CronJob to update the credential keys Secret
// via the Kubernetes API (CC-0036).
func (r *KeystoneReconciler) ensureCredentialRotationRBAC(ctx context.Context, keystone *keystonev1alpha1.Keystone, secretName string) error {
	saName := fmt.Sprintf("%s-credential-rotate", keystone.Name)

	// ServiceAccount
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: keystone.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetControllerReference(keystone, sa, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring ServiceAccount %s: %w", saName, err)
	}

	// Role with minimal permissions: get+patch on the specific credential keys Secret.
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: keystone.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			Verbs:         []string{"get", "patch"},
			ResourceNames: []string{secretName},
		}}
		return controllerutil.SetControllerReference(keystone, role, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring Role %s: %w", saName, err)
	}

	// RoleBinding
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: keystone.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: keystone.Namespace,
		}}
		// RoleRef is immutable after creation; only set on new objects.
		if rb.RoleRef.Name == "" {
			rb.RoleRef = rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     saName,
			}
		}
		return controllerutil.SetControllerReference(keystone, rb, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring RoleBinding %s: %w", saName, err)
	}

	return nil
}

// createCredentialKeysSecret generates credential keys and creates a Secret to store them.
// Credential keys use the same format as Fernet keys (32 bytes, base64url-encoded) (CC-0036).
func (r *KeystoneReconciler) createCredentialKeysSecret(ctx context.Context,
	keystone *keystonev1alpha1.Keystone, secretName string,
) error {
	numKeys := normalizedCredentialMaxActiveKeys(keystone)

	data := make(map[string][]byte, numKeys)
	for i := 0; i < numKeys; i++ {
		key, err := generateFernetKey()
		if err != nil {
			return err
		}
		data[strconv.Itoa(i)] = []byte(key)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Data: data,
	}

	if err := controllerutil.SetControllerReference(keystone, secret, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on credential keys secret: %w", err)
	}

	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("creating credential keys secret: %w", err)
	}

	return nil
}

// credentialRotationCronJob builds the CronJob that rotates credential keys, migrates
// existing encrypted credentials, and persists the result back to the Kubernetes Secret
// via the API. The CronJob:
//  1. Mounts the existing credential keys Secret as a read-only volume.
//  2. Uses an init container to copy keys to a writable emptyDir.
//  3. Runs keystone-manage credential_rotate followed by credential_migrate.
//  4. Pushes the updated keys to the K8s API using the pod's ServiceAccount (CC-0036).
func credentialRotationCronJob(keystone *keystonev1alpha1.Keystone, configMapName string) *batchv1.CronJob {
	secretName := fmt.Sprintf("%s-credential-keys", keystone.Name)
	fernetSecretName := fmt.Sprintf("%s-fernet-keys", keystone.Name)
	saName := fmt.Sprintf("%s-credential-rotate", keystone.Name)
	image := fmt.Sprintf("%s:%s", keystone.Spec.Image.Repository, keystone.Spec.Image.Tag)

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-credential-rotate", keystone.Name),
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: keystone.Spec.CredentialKeys.RotationSchedule,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: commonLabels(keystone),
						},
						Spec: corev1.PodSpec{
							ServiceAccountName: saName,
							RestartPolicy:      corev1.RestartPolicyOnFailure,
							InitContainers: []corev1.Container{{
								Name:            "copy-keys",
								Image:           image,
								Command:         []string{"sh", "-c", "cp /credential-keys-src/* /etc/keystone/credential-keys/"},
								SecurityContext: restrictedSecurityContext(),
								VolumeMounts: []corev1.VolumeMount{
									{Name: "credential-keys-src", MountPath: "/credential-keys-src", ReadOnly: true},
									{Name: "credential-keys", MountPath: "/etc/keystone/credential-keys"},
								},
							}},
							Containers: []corev1.Container{{
								Name:  "credential-rotate",
								Image: image,
								// TODO(CC-0042): Wire spec.Resources (or a smaller Job-specific default) to
								// this container. Currently runs as BestEffort QoS. See reconcile_deployment.go
								// containerResources() for the pattern used by the keystone-api container.
								Command:         []string{"sh", "-c", credentialRotateScript},
								SecurityContext: restrictedSecurityContext(),
								Env: []corev1.EnvVar{
									{Name: "SECRET_NAME", Value: secretName},
									{Name: "SECRET_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
									}},
									// oslo.config honours OS_<GROUP>__<KEY> env var overrides, so this
									// takes precedence over the compiled-in default (3) without needing
									// to mount the ConfigMap. Uses the normalized value to stay
									// consistent with the Secret's minimum floor of 3 (CC-0036).
									{
										Name:  "OS_credential__max_active_keys",
										Value: strconv.Itoa(normalizedCredentialMaxActiveKeys(keystone)),
									},
								},
								VolumeMounts: []corev1.VolumeMount{
									{Name: "credential-keys", MountPath: "/etc/keystone/credential-keys"},
									{Name: "fernet-keys", MountPath: "/etc/keystone/fernet-keys", ReadOnly: true},
									{Name: "config", MountPath: "/etc/keystone/keystone.conf.d/", ReadOnly: true},
								},
							}},
							Volumes: []corev1.Volume{
								{
									Name: "credential-keys-src",
									VolumeSource: corev1.VolumeSource{
										Secret: &corev1.SecretVolumeSource{SecretName: secretName},
									},
								},
								{
									Name: "credential-keys",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									// keystone-manage reads the full config which references both
									// key repositories; mount fernet-keys read-only so the directory
									// exists even though this job only rotates credential keys.
									Name: "fernet-keys",
									VolumeSource: corev1.VolumeSource{
										Secret: &corev1.SecretVolumeSource{SecretName: fernetSecretName},
									},
								},
								{
									Name: "config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// credentialKeysPushSecret builds the PushSecret CR that backs up credential keys to OpenBao.
func credentialKeysPushSecret(keystone *keystonev1alpha1.Keystone) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-credential-keys-backup", keystone.Name),
			Namespace: keystone.Namespace,
		},
		Spec: esov1alpha1.PushSecretSpec{
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{{
				Kind: "ClusterSecretStore",
				Name: "openbao-cluster-store",
			}},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{
					Name: fmt.Sprintf("%s-credential-keys", keystone.Name),
				},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{
						RemoteKey: "kv-v2/data/openstack/keystone/credential-keys",
					},
				},
			}},
		},
	}
}
