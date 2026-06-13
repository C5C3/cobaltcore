// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/job"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0058

const (
	conditionTypePolicyValidReady              = "PolicyValidReady"
	conditionReasonPolicyValidationNotRequired = "PolicyValidationNotRequired"
	conditionReasonPolicyValidationInProgress  = "PolicyValidationInProgress"
	conditionReasonPolicyValidationPassed      = "PolicyValidationPassed"
	conditionReasonPolicyValidationFailed      = "PolicyValidationFailed"
)

// reconcilePolicyValidation ensures custom oslo.policy overrides are validated
// via oslopolicy-validator before the Deployment is updated. Two lifecycle paths:
//   - spec.policyOverrides nil: delete any existing validation Job, set
//     PolicyValidReady=True/PolicyValidationNotRequired (CC-0058, REQ-003)
//   - spec.policyOverrides set: run validation Job via job.RunJob, track
//     lifecycle through InProgress/Passed/Failed states (CC-0058, REQ-001, REQ-002)
func (r *KeystoneReconciler) reconcilePolicyValidation(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error) {
	jobName := fmt.Sprintf("%s-policy-validation", keystone.Name)

	// Path 1: no policy overrides — delete any existing validation Job and
	// set condition to True/PolicyValidationNotRequired (CC-0058, REQ-003).
	if keystone.Spec.PolicyOverrides == nil {
		if err := deleteValidationJob(ctx, r.Client, keystone.Namespace, jobName); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting validation Job: %w", err)
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypePolicyValidReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonPolicyValidationNotRequired,
			Message:            "No policy overrides configured",
		})
		return ctrl.Result{}, nil
	}

	// Path 2: policy overrides set — run validation Job (CC-0058, REQ-001).
	done, err := job.RunJob(ctx, r.Client, r.Scheme, keystone, buildPolicyValidationJob(keystone, configMapName))
	if err != nil {
		msg := fmt.Sprintf("Policy validation failed: %v", err)
		if errors.Is(err, job.ErrJobFailed) {
			msg = getValidationErrorMessage(ctx, r.Client, jobName, keystone.Namespace)
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypePolicyValidReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonPolicyValidationFailed,
			Message:            msg,
		})
		return ctrl.Result{}, fmt.Errorf("running policy validation: %w", err)
	}
	if !done {
		log.FromContext(ctx).Info("policy validation job in progress, requeuing")
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypePolicyValidReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonPolicyValidationInProgress,
			Message:            "Policy validation job is running",
		})
		return ctrl.Result{RequeueAfter: RequeueValidationWait}, nil
	}

	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               conditionTypePolicyValidReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonPolicyValidationPassed,
		Message:            "Policy validation completed successfully",
	})
	return ctrl.Result{}, nil
}

// getValidationErrorMessage extracts a descriptive error message from the
// failed validation Job's Pod termination message. It lists Pods by the
// job-name label, finds the most recent Pod with a terminated container, and
// returns the termination message (truncated to 500 chars). If no termination
// message is available, it returns a fallback referencing the Job name for
// manual log inspection (CC-0058, REQ-006).
func getValidationErrorMessage(ctx context.Context, c client.Client, jobName, namespace string) string {
	fallback := fmt.Sprintf(
		"Policy validation failed; check Job %s logs: kubectl logs -n %s job/%s",
		jobName, namespace, jobName,
	)

	var pods corev1.PodList
	if err := c.List(
		ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"job-name": jobName},
	); err != nil {
		log.FromContext(ctx).Error(err, "failed to list pods for validation error extraction", "job", jobName)
		return fallback
	}
	if len(pods.Items) == 0 {
		return fallback
	}

	// Sort by creation timestamp descending so the most recent pod is checked first.
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
	})

	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				msg := cs.State.Terminated.Message
				if len(msg) > 500 {
					msg = msg[:500] + "..."
				}
				return fmt.Sprintf("Policy validation failed: %s", msg)
			}
		}
	}

	return fallback
}

// buildPolicyValidationJob constructs the validation Job that runs
// oslopolicy-validator against the rendered policy.yaml in the ConfigMap.
// The Job uses the same container image as the Keystone API Deployment,
// mounts the ConfigMap read-only, and has backoffLimit=2. No
// ttlSecondsAfterFinished is set: the completed Job lingers as the RunJob
// state record so reconciliation does not re-create it in a loop once it
// finishes (CC-0113, #415).
// oslopolicy-validator reads keystone.conf from the mounted config dir to
// resolve [oslo_policy] policy_file (CC-0058, REQ-007).
func buildPolicyValidationJob(keystone *keystonev1alpha1.Keystone, configMapName string) *batchv1.Job {
	backoffLimit := int32(2)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-policy-validation", keystone.Name),
			Namespace: keystone.Namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "validator",
						Image: fmt.Sprintf("%s:%s", keystone.Spec.Image.Repository, keystone.Spec.Image.Tag),
						// TODO(CC-0042): Wire spec.Resources (or a smaller Job-specific default) to
						// this container. Currently runs as BestEffort QoS. See reconcile_deployment.go
						// containerResources() for the pattern used by the keystone container (CC-0095).
						Command: []string{
							"oslopolicy-validator",
							"--namespace", "keystone",
							"--config-dir", "/etc/keystone/keystone.conf.d/",
						},
						SecurityContext:          restrictedSecurityContext(),
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "config",
							MountPath: "/etc/keystone/keystone.conf.d/",
							ReadOnly:  true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "config",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: configMapName,
								},
							},
						},
					}},
				},
			},
		},
	}
}

// deleteValidationJob deletes the validation Job identified by namespace and
// name. It is a no-op if the Job does not exist (CC-0058, REQ-003).
func deleteValidationJob(ctx context.Context, c client.Client, namespace, name string) error {
	j := &batchv1.Job{}
	j.SetName(name)
	j.SetNamespace(namespace)
	propagation := metav1.DeletePropagationBackground
	if err := client.IgnoreNotFound(c.Delete(ctx, j, &client.DeleteOptions{PropagationPolicy: &propagation})); err != nil {
		return fmt.Errorf("deleting Job %s/%s: %w", namespace, name, err)
	}
	return nil
}
