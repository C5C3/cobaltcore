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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/job"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0057, CC-0096

// reconcileTrustFlush ensures the trust flush CronJob matches the desired state.
// Two lifecycle paths:
//   - spec.trustFlush set (production): create or update CronJob running
//     keystone-manage trust_flush (CC-0057, REQ-001).
//   - spec.trustFlush nil (legacy bypass): admission webhooks did not materialize
//     the field — either the cluster has no webhook configured, or a programmatic
//     caller (envtest/unit test) invoked reconcileTrustFlush directly without
//     going through admission. Delete any existing CronJob and emit a Warning
//     Event so the bypass posture is visible in `kubectl describe`
//     (CC-0057, CC-0096, REQ-002).
func (r *KeystoneReconciler) reconcileTrustFlush(ctx context.Context,
	keystone *keystonev1alpha1.Keystone, configMapName string,
) (ctrl.Result, error) {
	cronJobName := fmt.Sprintf("%s-trust-flush", keystone.Name)

	// Legacy bypass: runs when admission webhooks did not materialize the field —
	// either the cluster has no webhook configured, or a programmatic caller
	// (envtest/unit test) invoked reconcileTrustFlush directly without going
	// through admission. Delete any existing CronJob, log the bypass for log
	// pipelines, and emit a Warning Event for operator-visible signal in
	// `kubectl describe` (CC-0057, CC-0096, REQ-002).
	if keystone.Spec.TrustFlush == nil {
		// reason="webhook-bypass" is the greppable sentinel for log pipelines that
		// differentiate the bypass path from the production (defaulted) path
		// (CC-0096, REQ-002).
		log.FromContext(ctx).Info(
			"trust flush legacy bypass: spec.trustFlush is nil — webhook defaulting did not run; deleting any existing CronJob (CC-0096, REQ-002)",
			"reason", "webhook-bypass",
			"namespace", keystone.Namespace,
			"name", keystone.Name,
		)
		// logr has no Warning level, so surface the operationally significant
		// bypass as a Kubernetes Warning Event on the Keystone CR. This matches
		// the pattern used by other reconcilers in this package (see
		// reconcile_database.go) and makes the bypass visible via
		// `kubectl describe keystone <name>` (CC-0096, REQ-002).
		r.Recorder.Event(keystone, corev1.EventTypeWarning, "TrustFlushBypass",
			"Trust flush legacy bypass: spec.trustFlush is nil (webhook defaulting did not run); existing CronJob deleted")
		if err := deleteCronJob(ctx, r.Client, keystone.Namespace, cronJobName); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting trust flush CronJob: %w", err)
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "TrustFlushReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             "TrustFlushNotRequired",
			Message:            "Trust flush bypass: spec.trustFlush is nil (webhook did not default this object)",
		})
		return ctrl.Result{}, nil
	}

	// Production path: trust flush configured — create or update CronJob (CC-0057, REQ-001).
	cronJob := trustFlushCronJob(keystone, configMapName)
	if err := job.EnsureCronJob(ctx, r.Client, r.Scheme, keystone, cronJob); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring trust flush CronJob: %w", err)
	}
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "TrustFlushReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "TrustFlushReady",
		Message:            "Trust flush CronJob is configured",
	})
	return ctrl.Result{}, nil
}

// trustFlushCronJob builds the CronJob that periodically purges expired trust
// delegations. The CronJob runs keystone-manage trust_flush against the
// database via the mounted keystone configuration (CC-0057, REQ-004, REQ-005, REQ-006).
func trustFlushCronJob(keystone *keystonev1alpha1.Keystone, configMapName string) *batchv1.CronJob {
	image := fmt.Sprintf("%s:%s", keystone.Spec.Image.Repository, keystone.Spec.Image.Tag)
	fernetSecretName := fmt.Sprintf("%s-fernet-keys", keystone.Name)
	credentialSecretName := fmt.Sprintf("%s-credential-keys", keystone.Name)

	cmd := append([]string{"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "trust_flush"}, keystone.Spec.TrustFlush.Args...)

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-trust-flush", keystone.Name),
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: keystone.Spec.TrustFlush.Schedule,
			Suspend:  ptr.To(keystone.Spec.TrustFlush.Suspend),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: commonLabels(keystone),
						},
						Spec: corev1.PodSpec{
							PriorityClassName: priorityClassName(keystone),
							RestartPolicy:     corev1.RestartPolicyOnFailure,
							Containers: []corev1.Container{{
								Name:            "trust-flush",
								Image:           image,
								Command:         cmd,
								SecurityContext: restrictedSecurityContext(),
								// Override [database].connection via oslo.config env-var so the
								// trust-flush CronJob reads the DB URL from the derived Secret
								// instead of the ConfigMap (CC-0080, REQ-004).
								Env: []corev1.EnvVar{buildDBConnectionEnvVar(keystone)},
								VolumeMounts: []corev1.VolumeMount{
									{Name: "config", MountPath: "/etc/keystone/keystone.conf.d/", ReadOnly: true},
									{Name: "fernet-keys", MountPath: "/etc/keystone/fernet-keys", ReadOnly: true},
									{Name: "credential-keys", MountPath: "/etc/keystone/credential-keys", ReadOnly: true},
								},
							}},
							Volumes: []corev1.Volume{
								{
									Name: "config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
										},
									},
								},
								{
									Name: "fernet-keys",
									VolumeSource: corev1.VolumeSource{
										Secret: &corev1.SecretVolumeSource{SecretName: fernetSecretName},
									},
								},
								{
									Name: "credential-keys",
									VolumeSource: corev1.VolumeSource{
										Secret: &corev1.SecretVolumeSource{SecretName: credentialSecretName},
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

// deleteCronJob deletes the CronJob identified by namespace and name. It is a
// no-op if the CronJob does not exist (CC-0057, REQ-002). After CC-0096 reframed
// the reconciler's nil-spec branch as a legacy bypass for envtest/webhook-less
// callers, this helper is invoked solely from that bypass path to clear any
// stray CronJob left over before the webhook materialized spec.trustFlush.
func deleteCronJob(ctx context.Context, c client.Client, namespace, name string) error {
	cj := &batchv1.CronJob{}
	cj.SetName(name)
	cj.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, cj)); err != nil {
		return fmt.Errorf("deleting CronJob %s/%s: %w", namespace, name, err)
	}
	return nil
}
