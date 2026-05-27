// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	_ "embed"
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

// bootstrapDBSeedScript is the standalone Python program that pre-inserts the
// admin region row before keystone-manage bootstrap runs. The same script is
// invoked under both plaintext and TLS DSNs — TLS handling is encapsulated in
// the script's ssl-dict mapping (CC-0106, REQ-005). Extracting it to a .py
// file (scripts/bootstrap_db_seed.py) makes it independently lintable and
// pytest-testable, matching the fernet/credential rotation script convention
// (CC-0073).
//
//go:embed scripts/bootstrap_db_seed.py
var bootstrapDBSeedScript string

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
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, "BootstrapFailed", "Keystone bootstrap job failed: %v", err)
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
	r.Recorder.Event(keystone, corev1.EventTypeNormal, "BootstrapComplete", "Keystone bootstrap completed successfully")
	return ctrl.Result{}, nil
}

// bootstrapServiceURL returns the cluster-local service URL for the Keystone
// Service associated with this CR. The host segment is composed via
// subResourceName(keystone) so the bootstrap-seeded catalog URL automatically
// tracks any future change to the sub-resource naming convention — divergence
// between the bootstrap URL and the actual Service hostname is impossible by
// construction (CC-0095, REQ-002).
func bootstrapServiceURL(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:5000/v3", subResourceName(keystone), keystone.Namespace)
}

func buildBootstrapJob(keystone *keystonev1alpha1.Keystone, configMapName string, fernetSecretName string) *batchv1.Job {
	backoffLimit := int32(4)
	ttl := int32(300)

	internalURL := bootstrapServiceURL(keystone)
	publicURL := internalURL
	if keystone.Spec.Bootstrap.PublicEndpoint != "" {
		publicURL = keystone.Spec.Bootstrap.PublicEndpoint
	}

	// The pre-insert script seeds the admin region row before keystone-manage
	// bootstrap runs (CC-0080, REQ-004). Its full contract — DSN resolution
	// precedence, ssl-dict mapping for CC-0106 REQ-005 — lives in the
	// standalone scripts/bootstrap_db_seed.py file embedded as
	// bootstrapDBSeedScript. We invoke it via `python3 -` with the embedded
	// source piped on stdin so a 'PY' quoted heredoc preserves the Python
	// content verbatim (no shell interpolation, no need to escape quotes or
	// percent signs). Region and bootstrap URLs are passed through the
	// container environment so the wrapper string is parameter-free.
	bootstrapScript := `python3 - <<'PY'
` + bootstrapDBSeedScript + `PY
exec keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ bootstrap \
  --bootstrap-password "$BOOTSTRAP_PASSWORD" \
  --bootstrap-admin-url "$BOOTSTRAP_ADMIN_URL" \
  --bootstrap-internal-url "$BOOTSTRAP_INTERNAL_URL" \
  --bootstrap-public-url "$BOOTSTRAP_PUBLIC_URL" \
  --bootstrap-region-id "$BOOTSTRAP_REGION_ID"
`

	volumeMounts := []corev1.VolumeMount{
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
	}
	volumes := []corev1.Volume{
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
	}
	// CC-0106, REQ-005: when DB TLS is enabled, mount the client-cert Secret
	// produced by reconcile_databasetls.go at /etc/keystone/db-tls/ so the
	// pre-insert pymysql call can read the ssl_ca/ssl_cert/ssl_key file paths
	// the DSN carries as query parameters (mirrors the dbtls_mode.dbTLSPaths
	// canonical layout). The volume is omitted entirely for the plaintext path
	// so the bootstrap Job is unchanged for pre-CC-0106 CRs. The
	// dbTLSEnabled/dbTLSVolumeAndMount helpers are owned by
	// reconcile_databasetls.go (CC-0106 task 4.2) so the Deployment and the
	// bootstrap Job stay in lockstep.
	if dbTLSEnabled(keystone) {
		vol, mount := dbTLSVolumeAndMount(keystone)
		volumes = append(volumes, vol)
		volumeMounts = append(volumeMounts, mount)
	}

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
					RestartPolicy:     corev1.RestartPolicyNever,
					PriorityClassName: priorityClassName(keystone),
					Containers: []corev1.Container{{
						Name:  "bootstrap",
						Image: fmt.Sprintf("%s:%s", keystone.Spec.Image.Repository, keystone.Spec.Image.Tag),
						// TODO(CC-0042): Wire spec.Resources (or a smaller Job-specific default) to
						// this container. Currently runs as BestEffort QoS. See reconcile_deployment.go
						// containerResources() for the pattern used by the keystone container (CC-0095).
						Command: []string{"/bin/sh", "-eu", "-c", bootstrapScript},
						Env: []corev1.EnvVar{
							{
								Name: "BOOTSTRAP_PASSWORD",
								ValueFrom: &corev1.EnvVarSource{
									SecretKeyRef: &corev1.SecretKeySelector{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: keystone.Spec.Bootstrap.AdminPasswordSecretRef.Name,
										},
										Key: "password",
									},
								},
							},
							// Override [database].connection via oslo.config env-var so the
							// bootstrap Job reads the DB URL from the derived Secret instead
							// of the ConfigMap (CC-0080, REQ-004).
							buildDBConnectionEnvVar(keystone),
							// Region id and bootstrap URLs are passed as env vars so the
							// embedded Python and the keystone-manage call read them at
							// runtime; this keeps scripts/bootstrap_db_seed.py free of
							// printf-style placeholders and shell-quoting concerns
							// (CC-0095, REQ-002; CC-0106, REQ-005).
							{Name: "BOOTSTRAP_REGION_ID", Value: keystone.Spec.Bootstrap.Region},
							{Name: "BOOTSTRAP_ADMIN_URL", Value: internalURL},
							{Name: "BOOTSTRAP_INTERNAL_URL", Value: internalURL},
							{Name: "BOOTSTRAP_PUBLIC_URL", Value: publicURL},
						},
						SecurityContext: restrictedSecurityContext(),
						VolumeMounts:    volumeMounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}
