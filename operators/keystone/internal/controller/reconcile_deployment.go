// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/deployment"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0013

// fernetKeysHash reads the fernet-keys Secret for the given Keystone instance
// and returns a deterministic SHA-256 hex digest of its Data map. If the Secret
// does not exist, the not-found error is returned to the caller (CC-0015).
func (r *KeystoneReconciler) fernetKeysHash(ctx context.Context, keystone *keystonev1alpha1.Keystone) (string, error) {
	secretName := fmt.Sprintf("%s-fernet-keys", keystone.Name)
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: keystone.Namespace,
	}, &secret); err != nil {
		return "", fmt.Errorf("getting fernet-keys Secret %s/%s: %w", keystone.Namespace, secretName, err)
	}
	// json.Marshal sorts map keys, so the hash is deterministic (CC-0015).
	data, _ := json.Marshal(secret.Data)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// reconcileDeployment ensures the Keystone API Deployment and Service exist
// with the correct spec. It sets the DeploymentReady condition and the
// status endpoint when the Deployment becomes available (CC-0013, REQ-006, REQ-012).
func (r *KeystoneReconciler) reconcileDeployment(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error) {
	hash, err := r.fernetKeysHash(ctx, keystone)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("computing fernet-keys hash: %w", err)
	}
	deploy := buildKeystoneDeployment(keystone, configMapName, hash)
	ready, err := deployment.EnsureDeployment(ctx, r.Client, r.Scheme, keystone, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Deployment: %w", err)
	}

	svc := buildKeystoneService(keystone)
	if err := deployment.EnsureService(ctx, r.Client, r.Scheme, keystone, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Service: %w", err)
	}

	if !ready {
		log.FromContext(ctx).Info("Keystone API deployment not ready, requeuing")
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "DeploymentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "WaitingForDeployment",
			Message:            "Keystone API deployment is not yet available",
		})
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	keystone.Status.Endpoint = fmt.Sprintf("http://%s-api.%s.svc.cluster.local:5000/v3", keystone.Name, keystone.Namespace)
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "DeploymentReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "DeploymentReady",
		Message:            "Keystone API deployment is available",
	})
	return ctrl.Result{}, nil
}

// keystoneAppLabel returns the label value used to select pods for this Keystone instance.
func keystoneAppLabel(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-api", keystone.Name)
}

func buildKeystoneDeployment(keystone *keystonev1alpha1.Keystone, configMapName string, fernetKeysHash string) *appsv1.Deployment {
	appLabel := keystoneAppLabel(keystone)
	labels := map[string]string{"app": appLabel}
	replicas := int32(keystone.Spec.Replicas)
	fernetSecretName := fmt.Sprintf("%s-fernet-keys", keystone.Name)
	credentialSecretName := fmt.Sprintf("%s-credential-keys", keystone.Name)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-api", keystone.Name),
			Namespace: keystone.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"keystone.c5c3.io/fernet-keys-hash": fernetKeysHash,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "keystone-api",
						Image: fmt.Sprintf("%s:%s", keystone.Spec.Image.Repository, keystone.Spec.Image.Tag),
						Command: []string{
							"uwsgi",
							"--http", ":5000",
							"--wsgi-file", "/var/lib/openstack/bin/keystone-wsgi-public",
							"--master",
							"--lazy-apps",
							"--need-app",
							"--processes", "2",
							"--threads", "2",
							"--pyargv=--config-dir=/etc/keystone/keystone.conf.d/",
						},
						Ports: []corev1.ContainerPort{{
							Name:          "keystone-api",
							ContainerPort: 5000,
						}},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/v3",
									Port: intstr.FromInt32(5000),
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       20,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/v3",
									Port: intstr.FromInt32(5000),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "config",
								MountPath: "/etc/keystone/keystone.conf.d/",
								ReadOnly:  true,
							},
							{
								Name:      "fernet-keys",
								MountPath: "/etc/keystone/fernet-keys/",
								ReadOnly:  true,
							},
							{
								Name:      "credential-keys",
								MountPath: "/etc/keystone/credential-keys/",
								ReadOnly:  true,
							},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: configMapName,
									},
								},
							},
						},
						{
							Name: "fernet-keys",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: fernetSecretName,
								},
							},
						},
						{
							Name: "credential-keys",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: credentialSecretName,
								// TODO(CC-0013): Credential key management is deferred to a future feature.
								// Optional=true prevents MountVolume.SetUp errors until the secret is created
								// by a dedicated reconcileCredentialKeys sub-reconciler.
								Optional: ptr.To(true),
							},
							},
						},
					},
				},
			},
		},
	}
}

func buildKeystoneService(keystone *keystonev1alpha1.Keystone) *corev1.Service {
	appLabel := keystoneAppLabel(keystone)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-api", keystone.Name),
			Namespace: keystone.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": appLabel},
			Ports: []corev1.ServicePort{{
				Port:       5000,
				TargetPort: intstr.FromInt32(5000),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}
