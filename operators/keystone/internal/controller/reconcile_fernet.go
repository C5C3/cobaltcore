// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/job"
	"github.com/c5c3/forge/internal/common/secrets"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// fernetRotateScript is the shell script executed by the Fernet rotation CronJob.
// It rotates the keys on an emptyDir working copy, then pushes the updated keys
// back to the Kubernetes Secret via the API using the pod's ServiceAccount token.
// Only Python standard library modules are used to avoid image dependencies (CC-0013).
// Extracted to scripts/fernet_rotate.sh for independent linting and testing (CC-0073).
//
//go:embed scripts/fernet_rotate.sh
var fernetRotateScript string

// reconcileFernetKeys ensures that a Fernet keys Secret exists, a rotation
// CronJob is configured, and a PushSecret backs up the keys to OpenBao.
func (r *KeystoneReconciler) reconcileFernetKeys(ctx context.Context,
	keystone *keystonev1alpha1.Keystone, configMapName string,
) (ctrl.Result, error) {
	// 1. Ensure the Fernet keys Secret exists.
	secretName := fmt.Sprintf("%s-fernet-keys", keystone.Name)
	secretKey := client.ObjectKey{Namespace: keystone.Namespace, Name: secretName}

	existing := &corev1.Secret{}
	err := r.Get(ctx, secretKey, existing)
	if apierrors.IsNotFound(err) {
		// Generate initial Fernet keys.
		if err := r.createFernetKeysSecret(ctx, keystone, secretName); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating fernet keys secret: %w", err)
		}
		r.Recorder.Event(keystone, corev1.EventTypeNormal, "FernetKeysGenerated", "Initial Fernet encryption keys have been generated")
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "FernetKeysReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "GeneratingKeys",
			Message:            "Initial Fernet keys have been generated",
		})
		// Requeue to confirm the secret is available before proceeding (CC-0013).
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting fernet keys secret: %w", err)
	}

	// 2. Ensure the staging Secret exists for the rotation CronJob to PATCH
	//    into (CC-0081). The operator owns the lifecycle (labels, owner ref)
	//    and the CronJob owns the Data — this is the split-compute-write
	//    boundary that keeps token-forgery primitives out of the CronJob's
	//    RBAC on the production Secret.
	if err := r.ensureFernetStagingSecret(ctx, keystone); err != nil {
		return ctrl.Result{}, err
	}

	// Refresh the key_rotation_age gauge from the rotation-completed
	// annotation (CC-0089, REQ-003). The helper reads the production Secret
	// first (durable across the inter-rotation steady state) and falls back
	// to the staging Secret to cover the very-first-rotation pre-apply
	// window. Called BEFORE applyRotationOutput so that, when the apply path
	// runs and re-stamps the production annotation, the next reconcile picks
	// up the freshest timestamp.
	r.observeRotationAge(ctx, keystone, secretName, fernetStagingSecretName(keystone), "fernet")

	// 3. Apply any completed rotation staged by the CronJob (CC-0081,
	//    REQ-005, REQ-006). On a valid apply we short-circuit the rest of
	//    the step chain and requeue so the next pass re-enters the happy
	//    path with the production Secret already updated.
	applied, err := r.applyRotationOutput(ctx, keystone,
		fernetStagingSecretName(keystone),
		secretName,
		"FernetKeysRotated",
		3,
		normalizedFernetMaxActiveKeys(keystone)+1,
	)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("applying fernet rotation output: %w", err)
	}
	if applied {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "FernetKeysReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             "FernetKeysRotated",
			Message:            "rotation applied; staging secret cleared",
		})
		return ctrl.Result{Requeue: true}, nil
	}

	// 4. Ensure the RBAC resources for the rotation CronJob exist.
	if err := r.ensureFernetRotationRBAC(ctx, keystone, secretName); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring fernet rotation RBAC: %w", err)
	}

	// 5. Create the immutable ConfigMap containing the rotation script (CC-0073).
	scriptConfigMapName, err := config.CreateImmutableConfigMap(ctx, r.Client, r.Scheme, keystone,
		fmt.Sprintf("%s-fernet-rotate-script", keystone.Name), keystone.Namespace,
		map[string]string{"fernet_rotate.sh": fernetRotateScript})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating fernet rotate script ConfigMap: %w", err)
	}

	// 6. Ensure the rotation CronJob exists.
	cronJob := fernetRotationCronJob(keystone, configMapName, scriptConfigMapName)
	if err := job.EnsureCronJob(ctx, r.Client, r.Scheme, keystone, cronJob); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring fernet rotation cronjob: %w", err)
	}

	// 7. Ensure the PushSecret for OpenBao backup exists.
	ps := fernetKeysPushSecret(keystone)
	if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, keystone, ps); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring fernet keys pushsecret: %w", err)
	}

	// 8. Set the FernetKeysReady condition.
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "FernetKeysReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "FernetKeysAvailable",
		Message:            "Fernet keys Secret exists and rotation CronJob is configured",
	})

	return ctrl.Result{}, nil
}

// ensureFernetRotationRBAC creates the ServiceAccount, Role, and RoleBinding
// needed by the Fernet rotation CronJob. The Role is split into two
// PolicyRules (CC-0081): read-only `get` on the production fernet keys Secret
// and `get`+`patch` scoped to the dedicated staging Secret. The operator, not
// the CronJob, writes the production Secret — removing the token-forgery
// primitive from the CronJob's attack surface.
func (r *KeystoneReconciler) ensureFernetRotationRBAC(ctx context.Context, keystone *keystonev1alpha1.Keystone, secretName string) error {
	saName := fmt.Sprintf("%s-fernet-rotate", keystone.Name)

	// ServiceAccount
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: keystone.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetControllerReference(keystone, sa, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring ServiceAccount %s: %w", saName, err)
	}

	// Role split into two PolicyRules (CC-0081):
	//   1. `get` on the production fernet keys Secret (read-only; operator owns writes).
	//   2. `get`+`patch` on the staging Secret only; no `create`/`delete` because
	//      the operator manages the staging Secret's lifecycle.
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: keystone.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				Verbs:         []string{"get"},
				ResourceNames: []string{secretName},
			},
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				Verbs:         []string{"get", "patch"},
				ResourceNames: []string{fernetStagingSecretName(keystone)},
			},
		}
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

// ensureFernetStagingSecret ensures the Fernet staging Secret exists with the
// `fernet-keys` rotation-target label (CC-0081). Thin wrapper over the shared
// ensureStagingSecret helper; see rotation_staging.go for the field-ownership
// contract.
func (r *KeystoneReconciler) ensureFernetStagingSecret(ctx context.Context, keystone *keystonev1alpha1.Keystone) error {
	return r.ensureStagingSecret(ctx, keystone, fernetStagingSecretName(keystone), "fernet-keys")
}

// normalizedFernetMaxActiveKeys returns the effective maximum number of active
// Fernet keys, applying a minimum floor of 3. The webhook defaults 0 to 3, but
// this provides defense-in-depth for the reconciler (CC-0013).
func normalizedFernetMaxActiveKeys(keystone *keystonev1alpha1.Keystone) int {
	return max(int(keystone.Spec.Fernet.MaxActiveKeys), 3)
}

// createFernetKeysSecret generates Fernet keys and creates a Secret to store them.
func (r *KeystoneReconciler) createFernetKeysSecret(ctx context.Context,
	keystone *keystonev1alpha1.Keystone, secretName string,
) error {
	numKeys := normalizedFernetMaxActiveKeys(keystone)

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
		return fmt.Errorf("setting owner reference on fernet keys secret: %w", err)
	}

	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("creating fernet keys secret: %w", err)
	}

	return nil
}

// generateFernetKey generates a Fernet-compatible key (32 bytes, base64url-encoded with standard padding).
// Keystone (via Python's cryptography.fernet.Fernet) expects 44-char base64url strings WITH padding (CC-0013).
func generateFernetKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generating fernet key: %w", err)
	}
	return base64.URLEncoding.EncodeToString(key), nil
}

// fernetRotationCronJob builds the CronJob that rotates Fernet keys and persists
// the result back to the Kubernetes Secret via the API. The CronJob:
//  1. Mounts the existing fernet keys Secret as a read-only volume.
//  2. Uses an init container to copy keys to a writable emptyDir.
//  3. Mounts the rotation script from a versioned ConfigMap at /scripts/ (CC-0073).
//  4. Runs /scripts/fernet_rotate.sh against the emptyDir.
//  5. Pushes the updated keys to the K8s API using the pod's ServiceAccount (CC-0013).
func fernetRotationCronJob(keystone *keystonev1alpha1.Keystone, configMapName string, scriptConfigMapName string) *batchv1.CronJob {
	secretName := fmt.Sprintf("%s-fernet-keys", keystone.Name)
	stagingSecretName := fernetStagingSecretName(keystone)
	credentialSecretName := fmt.Sprintf("%s-credential-keys", keystone.Name)
	saName := fmt.Sprintf("%s-fernet-rotate", keystone.Name)
	image := fmt.Sprintf("%s:%s", keystone.Spec.Image.Repository, keystone.Spec.Image.Tag)

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-fernet-rotate", keystone.Name),
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: keystone.Spec.Fernet.RotationSchedule,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: commonLabels(keystone),
						},
						Spec: corev1.PodSpec{
							ServiceAccountName: saName,
							RestartPolicy:      corev1.RestartPolicyOnFailure,
							PriorityClassName:  priorityClassName(keystone),
							InitContainers: []corev1.Container{{
								Name:            "copy-keys",
								Image:           image,
								Command:         []string{"sh", "-c", "cp /fernet-keys-src/* /etc/keystone/fernet-keys/"},
								SecurityContext: restrictedSecurityContext(),
								VolumeMounts: []corev1.VolumeMount{
									{Name: "fernet-keys-src", MountPath: "/fernet-keys-src", ReadOnly: true},
									{Name: "fernet-keys", MountPath: "/etc/keystone/fernet-keys"},
								},
							}},
							Containers: []corev1.Container{{
								Name:  "fernet-rotate",
								Image: image,
								// TODO(CC-0042): Wire spec.Resources (or a smaller Job-specific default) to
								// this container. Currently runs as BestEffort QoS. See reconcile_deployment.go
								// containerResources() for the pattern used by the keystone container (CC-0095).
								Command:         []string{"/scripts/fernet_rotate.sh"},
								SecurityContext: restrictedSecurityContext(),
								Env: []corev1.EnvVar{
									// SECRET_NAME points at the staging Secret — the CronJob SA
									// is only permitted to patch the staging Secret, never the
									// production Secret (CC-0081).
									{Name: "SECRET_NAME", Value: stagingSecretName},
									{Name: "SECRET_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
									}},
									// oslo.config honours OS_<GROUP>__<KEY> env var overrides, so this
									// takes precedence over the compiled-in default (3) without needing
									// to mount the ConfigMap. Uses the normalized value to stay
									// consistent with the Secret's minimum floor of 3 (CC-0013).
									{
										Name:  "OS_fernet_tokens__max_active_keys",
										Value: strconv.Itoa(normalizedFernetMaxActiveKeys(keystone)),
									},
									// Override [database].connection via oslo.config env-var so the
									// fernet-rotate CronJob reads the DB URL from the derived Secret
									// instead of the ConfigMap (CC-0080, REQ-004).
									buildDBConnectionEnvVar(keystone),
								},
								VolumeMounts: []corev1.VolumeMount{
									{Name: "fernet-keys", MountPath: "/etc/keystone/fernet-keys"},
									{Name: "credential-keys", MountPath: "/etc/keystone/credential-keys", ReadOnly: true},
									{Name: "config", MountPath: "/etc/keystone/keystone.conf.d/", ReadOnly: true},
									{Name: "scripts", MountPath: "/scripts", ReadOnly: true},
								},
							}},
							Volumes: []corev1.Volume{
								{
									Name: "fernet-keys-src",
									VolumeSource: corev1.VolumeSource{
										Secret: &corev1.SecretVolumeSource{SecretName: secretName},
									},
								},
								{
									Name: "fernet-keys",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									// keystone-manage reads the full config which references both
									// key repositories; mount credential-keys read-only so the
									// directory exists even though this job only rotates fernet keys.
									Name: "credential-keys",
									VolumeSource: corev1.VolumeSource{
										Secret: &corev1.SecretVolumeSource{SecretName: credentialSecretName},
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
								{
									Name: "scripts",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{Name: scriptConfigMapName},
											DefaultMode:          ptr.To(int32(0o555)),
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

// fernetKeysPushSecret builds the PushSecret CR that backs up Fernet keys to OpenBao.
//
// The RemoteKey embeds keystone.Name as a path segment
// (kv-v2/openstack/keystone/{keystone.Name}/fernet-keys) so two Keystone CRs
// in the same namespace never share a backing OpenBao object (CC-0093,
// REQ-001).
//
// DeletionPolicy=Delete wires the backup PushSecret into the OpenBao finalizer
// flow: when the keystone.openstack.c5c3.io/openbao-finalizer handler deletes
// this PushSecret, ESO purges the remote kv-v2/openstack/keystone/{keystone.Name}/fernet-keys
// path before letting the PushSecret object be garbage-collected (CC-0079,
// REQ-008).
func fernetKeysPushSecret(keystone *keystonev1alpha1.Keystone) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-fernet-keys-backup", keystone.Name),
			Namespace: keystone.Namespace,
		},
		Spec: esov1alpha1.PushSecretSpec{
			DeletionPolicy: esov1alpha1.PushSecretDeletionPolicyDelete,
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{{
				Kind: "ClusterSecretStore",
				Name: "openbao-cluster-store",
			}},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{
					Name: fmt.Sprintf("%s-fernet-keys", keystone.Name),
				},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{
						RemoteKey: "openstack/keystone/" + keystone.Name + "/fernet-keys",
					},
				},
			}},
		},
	}
}
