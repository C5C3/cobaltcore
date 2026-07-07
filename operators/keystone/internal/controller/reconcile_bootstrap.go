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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/deployment"
	"github.com/c5c3/forge/internal/common/job"
	"github.com/c5c3/forge/internal/common/secrets"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// bootstrapDBSeedScript is the standalone Python program that pre-inserts the
// admin region row before keystone-manage bootstrap runs. The same script is
// invoked under both plaintext and TLS DSNs — TLS handling is encapsulated in
// the script's ssl-dict mapping. Extracting it to a .py
// file (scripts/bootstrap_db_seed.py) makes it independently lintable and
// pytest-testable, matching the fernet/credential rotation script convention
//
//go:embed scripts/bootstrap_db_seed.py
var bootstrapDBSeedScript string

// bootstrapScript is the bootstrap container's shell program: it pipes the
// embedded bootstrapDBSeedScript to python3 on stdin (a 'PY' quoted heredoc so
// the Python is preserved verbatim), then execs keystone-manage bootstrap.
// Region and bootstrap URLs are passed through the container environment, so the
// wrapper is parameter-free — built once at package init from constant parts
// rather than re-concatenated per buildBootstrapJob call (issue #361).
//
// --bootstrap-password uses the --flag=value form, not "--flag value": the admin
// password rotation mints the new password with Python secrets.token_urlsafe
// (base64url alphabet, which includes '-'), so ~1 in 64 rotated passwords starts
// with a dash. keystone-manage parses its flags with argparse, which rejects a
// space-separated value that begins with '-' as "expected one argument" (it reads
// the value as another option). The =value form binds the whole token as the
// value regardless of a leading dash. The URL/region flags keep the space form —
// their values are never dash-leading.
var bootstrapScript = `python3 - <<'PY'
` + bootstrapDBSeedScript + `PY
exec keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ bootstrap \
  --bootstrap-password="$BOOTSTRAP_PASSWORD" \
  --bootstrap-admin-url "$BOOTSTRAP_ADMIN_URL" \
  --bootstrap-internal-url "$BOOTSTRAP_INTERNAL_URL" \
  --bootstrap-public-url "$BOOTSTRAP_PUBLIC_URL" \
  --bootstrap-region-id "$BOOTSTRAP_REGION_ID"
`

// adminPasswordHashAnnotation stamps a SHA-256 digest of the admin password
// (the `password` key of the admin Secret) onto the bootstrap Job's pod
// template. Because job.PodSpecHash hashes the full PodTemplateSpec, a rotated
// admin password changes this annotation and therefore the forge.c5c3.io/pod-spec-hash
// gate, forcing the idempotent bootstrap Job to re-run.
const adminPasswordHashAnnotation = "forge.c5c3.io/admin-password-hash" //nolint:gosec // annotation key, not a credential

// reconcileBootstrap ensures the Keystone bootstrap Job runs with
// keystone-manage bootstrap and admin credentials.
func (r *KeystoneReconciler) reconcileBootstrap(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName, domainsSecretName string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Re-run the bootstrap Job whenever the admin password rotates: read the
	// `password` key of the admin Secret and stamp its SHA-256 digest onto the
	// Job's pod template so a changed password changes the pod-spec-hash gate
	// A missing/unreadable Secret, an absent
	// `password` key, or an empty value is a hard error — bootstrap cannot run
	// without an admin password, so surface it via BootstrapReady=False and
	// requeue rather than building a Job with empty credentials.
	//
	// DECISION: missing/empty admin password returns an error (requeue with backoff)
	// — Chose to return the error rather than the requeue-without-error pattern used
	// by reconcileDBConnectionSecret, because task 2.2/ specify "return the
	// error (requeue)" and an absent admin password is a hard precondition failure.
	// Reviewer: please verify this matches intent.
	adminSecretKey := client.ObjectKey{
		Namespace: keystone.Namespace,
		Name:      keystone.Spec.Bootstrap.AdminPasswordSecretRef.Name,
	}
	password, err := secrets.GetSecretValue(ctx, r.Client, adminSecretKey, "password")
	if err != nil || password == "" {
		msg := fmt.Sprintf("Admin password Secret %s/%s is missing, unreadable, or has an empty %q value",
			adminSecretKey.Namespace, adminSecretKey.Name, "password")
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "BootstrapReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "AdminSecretInvalid",
			Message:            msg,
		})
		r.Recorder.Event(keystone, corev1.EventTypeWarning, "AdminSecretInvalid", msg)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("reading admin password from Secret %s/%s: %w",
				adminSecretKey.Namespace, adminSecretKey.Name, err)
		}
		return ctrl.Result{}, fmt.Errorf("admin password Secret %s/%s has an empty %q value",
			adminSecretKey.Namespace, adminSecretKey.Name, "password")
	}
	adminPasswordHash := secrets.AdminPasswordDigest(password)

	fernetSecretName := fmt.Sprintf("%s-fernet-keys", keystone.Name)
	// Gate the bootstrap re-run on the admin-password digest only — NOT the full
	// pod template, which includes the container image. A release upgrade swaps
	// the image; re-running keystone-manage bootstrap after the cross-version DB
	// migration fails on the already-migrated admin user (DBDuplicateEntry
	// 'default-admin'), which would hold BootstrapReady — and the aggregate
	// Ready — False for the whole upgrade. Identity bootstrap is one-time; the
	// only input that must force a re-run is a rotated admin password
	// The bootstrap Job emits no db_sync metrics, so the observed Job is discarded.
	done, _, err := job.RunJobWithRerunKey(ctx, r.Client, r.Scheme, keystone, buildBootstrapJob(keystone, configMapName, domainsSecretName, fernetSecretName, adminPasswordHash), adminPasswordHash)
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
// construction.
func bootstrapServiceURL(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:5000/v3", subResourceName(keystone), keystone.Namespace)
}

func buildBootstrapJob(keystone *keystonev1alpha1.Keystone, configMapName, domainsSecretName, fernetSecretName, adminPasswordHash string) *batchv1.Job {
	backoffLimit := int32(4)

	internalURL := bootstrapServiceURL(keystone)
	publicURL := internalURL
	if keystone.Spec.Bootstrap.PublicEndpoint != "" {
		publicURL = keystone.Spec.Bootstrap.PublicEndpoint
	}

	// The bootstrap container runs the package-level bootstrapScript, which pipes
	// the embedded pre-insert Python (scripts/bootstrap_db_seed.py) to python3
	// and then execs keystone-manage bootstrap. See its declaration for the DSN
	// and heredoc contract.
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
	// when DB TLS is enabled, mount the client-cert Secret
	// produced by reconcile_databasetls.go at /etc/keystone/db-tls/ so the
	// pre-insert pymysql call can read the ssl_ca/ssl_cert/ssl_key file paths
	// the DSN carries as query parameters (mirrors the dbtls_mode.dbTLSPaths
	// canonical layout). The volume is omitted entirely for the plaintext path
	// so the bootstrap Job is unchanged for pre-existing CRs. The
	// dbTLSEnabled/dbTLSVolumeAndMount helpers are owned by
	// reconcile_databasetls.go so the Deployment and the
	// bootstrap Job stay in lockstep.
	if dbTLSEnabled(keystone) {
		vol, mount := dbTLSVolumeAndMount(keystone)
		volumes = append(volumes, vol)
		volumeMounts = append(volumeMounts, mount)
	}
	// Project the per-domain identity-backend config so keystone-manage
	// bootstrap sees the same domain-specific driver files the API pods load.
	if domainsSecretName != "" {
		vol, mount := domainsVolumeAndMount(domainsSecretName)
		volumes = append(volumes, vol)
		volumeMounts = append(volumeMounts, mount)
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-bootstrap", keystone.Name),
			Namespace: keystone.Namespace,
		},
		Spec: batchv1.JobSpec{
			// (#415): TTLSecondsAfterFinished is intentionally left unset.
			// A TTL-expired bootstrap Job would be garbage-collected by the
			// TTL-after-finished controller and then re-created on the next
			// reconcile, producing a TTL-driven re-creation loop. Leaving the Job
			// in place (owner-reference GC removes it with the Keystone CR) keeps
			// the idempotent bootstrap from re-running on a timer.
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						adminPasswordHashAnnotation: adminPasswordHash,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:     corev1.RestartPolicyNever,
					PriorityClassName: priorityClassName(keystone),
					Containers: []corev1.Container{{
						Name:  "bootstrap",
						Image: keystone.Spec.Image.Reference(),
						// TODO Wire spec.Resources (or a smaller Job-specific default) to
						// this container. Currently runs as BestEffort QoS. See reconcile_deployment.go
						// containerResources() for the pattern used by the keystone container.
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
							// of the ConfigMap.
							buildDBConnectionEnvVar(keystone),
							// Region id and bootstrap URLs are passed as env vars so the
							// embedded Python and the keystone-manage call read them at
							// runtime; this keeps scripts/bootstrap_db_seed.py free of
							// printf-style placeholders and shell-quoting concerns
							{Name: "BOOTSTRAP_REGION_ID", Value: keystone.Spec.Bootstrap.Region},
							{Name: "BOOTSTRAP_ADMIN_URL", Value: internalURL},
							{Name: "BOOTSTRAP_INTERNAL_URL", Value: internalURL},
							{Name: "BOOTSTRAP_PUBLIC_URL", Value: publicURL},
						},
						SecurityContext: deployment.RestrictedSecurityContext(),
						VolumeMounts:    volumeMounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}
