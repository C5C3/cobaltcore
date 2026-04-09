// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/job"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0013

// reconcileBootstrap ensures the Keystone bootstrap Job runs with
// keystone-manage bootstrap and admin credentials (REQ-007).
func (r *KeystoneReconciler) reconcileBootstrap(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	fernetSecretName := fmt.Sprintf("%s-fernet-keys", keystone.Name)
	done, err := job.RunJob(ctx, r.Client, r.Scheme, keystone, buildBootstrapJob(keystone, configMapName, fernetSecretName))
	if err != nil {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "BootstrapReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "BootstrapFailed",
			Message:            fmt.Sprintf("Keystone bootstrap job failed: %v", err),
		})
		return ctrl.Result{}, fmt.Errorf("running bootstrap: %w", err)
	}
	if !done {
		logger.Info("bootstrap job in progress, requeuing")
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "BootstrapReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "BootstrapInProgress",
			Message:            "Keystone bootstrap job is running",
		})
		return ctrl.Result{RequeueAfter: RequeueBootstrapWait}, nil
	}

	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "BootstrapReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "BootstrapComplete",
		Message:            "Keystone bootstrap completed successfully",
	})
	return ctrl.Result{}, nil
}

// bootstrapServiceURL returns the cluster-local service URL for the Keystone
// API service associated with this CR.
func bootstrapServiceURL(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("http://%s-api.%s.svc.cluster.local:5000/v3", keystone.Name, keystone.Namespace)
}

func buildBootstrapJob(keystone *keystonev1alpha1.Keystone, configMapName string, fernetSecretName string) *batchv1.Job {
	backoffLimit := int32(4)
	ttl := int32(300)

	internalURL := bootstrapServiceURL(keystone)
	publicURL := internalURL
	if keystone.Spec.Bootstrap.PublicEndpoint != "" {
		publicURL = keystone.Spec.Bootstrap.PublicEndpoint
	}

	bootstrapScript := fmt.Sprintf(`python3 -c '
import configparser, glob, pymysql
from urllib.parse import urlparse, parse_qs
conf = configparser.RawConfigParser()
for f in sorted(glob.glob("/etc/keystone/keystone.conf.d/*.conf")):
    conf.read(f)
url = urlparse(conf.get("database", "connection"))
db = url.path.lstrip("/")
qs = parse_qs(url.query)
charset = qs.get("charset", ["utf8"])[0]
conn = pymysql.connect(host=url.hostname, port=url.port or 3306,
    user=url.username, password=url.password, database=db, charset=charset)
cur = conn.cursor()
cur.execute("INSERT IGNORE INTO region (id, description, extra) VALUES (%%s, %%s, %%s)", ("%s", "", "{}"))
conn.commit()
conn.close()
'
exec keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ bootstrap \
  --bootstrap-password "$BOOTSTRAP_PASSWORD" \
  --bootstrap-admin-url %s \
  --bootstrap-internal-url %s \
  --bootstrap-public-url %s \
  --bootstrap-region-id %s
`, keystone.Spec.Bootstrap.Region, internalURL, internalURL, publicURL, keystone.Spec.Bootstrap.Region)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-bootstrap", keystone.Name),
			Namespace: keystone.Namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "bootstrap",
						Image: fmt.Sprintf("%s:%s", keystone.Spec.Image.Repository, keystone.Spec.Image.Tag),
						// TODO(CC-0042): Wire spec.Resources (or a smaller Job-specific default) to
						// this container. Currently runs as BestEffort QoS. See reconcile_deployment.go
						// containerResources() for the pattern used by the keystone-api container.
						Command: []string{"/bin/sh", "-eu", "-c", bootstrapScript},
						Env: []corev1.EnvVar{{
							Name: "BOOTSTRAP_PASSWORD",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: keystone.Spec.Bootstrap.AdminPasswordSecretRef.Name,
									},
									Key: "password",
								},
							},
						}},
						SecurityContext: restrictedSecurityContext(),
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
					},
				},
			},
		},
	}
}
