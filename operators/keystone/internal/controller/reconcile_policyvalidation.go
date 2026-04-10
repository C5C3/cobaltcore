// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/job"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0058

// conditionTypePolicyValidReady is the condition type for policy validation
// readiness, consistent with conditionTypeNetworkPolicyReady (CC-0058).
const conditionTypePolicyValidReady = "PolicyValidReady"

// reconcilePolicyValidation validates policy overrides before allowing
// Deployment rollout. Three lifecycle paths:
//   - policyOverrides nil: delete any existing Job, set PolicyValidReady=True/NotRequired (REQ-003)
//   - policyOverrides set: build and run validation Job via job.RunJob (REQ-001, REQ-002)
//   - error: propagate errors from job lifecycle (REQ-002)
func (r *KeystoneReconciler) reconcilePolicyValidation(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error) {
	jobName := fmt.Sprintf("%s-policy-validation", keystone.Name)

	// Path 1: no policy overrides — clean up any existing Job and mark
	// validation as not required (CC-0058, REQ-003).
	if keystone.Spec.PolicyOverrides == nil {
		existing := &batchv1.Job{}
		existing.SetName(jobName)
		existing.SetNamespace(keystone.Namespace)
		// Background propagation (the default) is acceptable here: this delete
		// path only fires when policyOverrides is removed entirely, so lingering
		// Pods are harmless and will be garbage-collected by the Job controller.
		if err := client.IgnoreNotFound(r.Delete(ctx, existing)); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting policy validation job %s/%s: %w", keystone.Namespace, jobName, err)
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypePolicyValidReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             "NotRequired",
			Message:            "No policy overrides configured",
		})
		return ctrl.Result{}, nil
	}

	// Path 2: policy overrides set — run validation Job (CC-0058, REQ-001).
	logger := log.FromContext(ctx)
	done, err := job.RunJob(ctx, r.Client, r.Scheme, keystone, buildPolicyValidationJob(keystone, configMapName))
	if err != nil {
		msg := fmt.Sprintf("Policy validation job failed: %v", err)
		if errors.Is(err, job.ErrJobFailed) {
			// Reuse the existing condition message if we already extracted a
			// failure message on a previous reconcile, avoiding repeated Pod
			// list calls against the API server (CC-0058).
			if existing := meta.FindStatusCondition(keystone.Status.Conditions, conditionTypePolicyValidReady); existing != nil &&
				existing.Reason == "PolicyValidationFailed" && existing.Message != "" {
				msg = existing.Message
			} else {
				msg = extractJobFailureMessage(ctx, r.Client, jobName, keystone.Namespace)
			}
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypePolicyValidReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "PolicyValidationFailed",
			Message:            msg,
		})
		return ctrl.Result{}, fmt.Errorf("running policy validation: %w", err)
	}
	if !done {
		logger.Info("policy validation job in progress, requeuing")
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypePolicyValidReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "PolicyValidationInProgress",
			Message:            "Policy validation job is running",
		})
		return ctrl.Result{RequeueAfter: RequeueValidationWait}, nil
	}

	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               conditionTypePolicyValidReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "PolicyValidationPassed",
		Message:            "Policy validation completed successfully",
	})
	return ctrl.Result{}, nil
}

// maxConditionMessageLen is the practical upper limit for Kubernetes condition
// messages. Messages exceeding this length are truncated to avoid oversized
// status objects (CC-0058, REQ-006).
const maxConditionMessageLen = 1024

// extractJobFailureMessage retrieves a descriptive error message from the
// termination state of the "validator" container in Pods belonging to the
// given Job. If no termination message is available, it returns a fallback
// message referencing the Job name and namespace for manual log inspection
// (CC-0058, REQ-006).
func extractJobFailureMessage(ctx context.Context, c client.Client, jobName, namespace string) string {
	fallback := fmt.Sprintf("check logs: kubectl logs -l job-name=%s -n %s", jobName, namespace)

	var podList corev1.PodList
	if err := c.List(ctx, &podList, client.InNamespace(namespace), client.MatchingLabels{"job-name": jobName}); err != nil {
		log.FromContext(ctx).V(1).Info("failed to list pods for error extraction", "error", err)
		return fallback
	}

	for i := range podList.Items {
		for _, cs := range podList.Items[i].Status.ContainerStatuses {
			if cs.Name != "validator" {
				continue
			}
			msg := terminationMessage(cs)
			if msg != "" {
				if len(msg) > maxConditionMessageLen {
					return msg[:maxConditionMessageLen]
				}
				return msg
			}
		}
	}
	return fallback
}

// terminationMessage returns the termination message from the container status,
// checking State.Terminated first, then LastTerminationState.Terminated.
func terminationMessage(cs corev1.ContainerStatus) string {
	if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
		return cs.State.Terminated.Message
	}
	if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Message != "" {
		return cs.LastTerminationState.Terminated.Message
	}
	return ""
}

// buildPolicyValidationJob constructs the desired validation Job that runs
// oslopolicy-validator against the rendered policy.yaml in the ConfigMap.
// The Job uses the same Keystone container image as the API Deployment
// (CC-0058, REQ-007).
//
// oslopolicy-validator (not oslopolicy-checker) is the correct tool here:
// oslopolicy-checker evaluates a specific access token against a rule and
// requires --access; oslopolicy-validator cross-checks the rules in
// policy.yaml against the keystone oslo.policy entry-point namespace and
// exits non-zero on unknown rules or malformed check strings. The validator
// resolves the policy_file path from keystone.conf, so it is pointed at the
// keystone.conf shipped in the mounted ConfigMap (CC-0058).
func buildPolicyValidationJob(keystone *keystonev1alpha1.Keystone, configMapName string) *batchv1.Job {
	// backoffLimit is 2 (vs. 4 for db-sync/bootstrap) because policy validation
	// is a deterministic syntax check — if the policy file is malformed, retries
	// will not produce a different result. Two attempts cover transient container
	// startup failures while failing fast on genuine policy errors (CC-0058).
	backoffLimit := int32(2)
	ttl := int32(300)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-policy-validation", keystone.Name),
			Namespace: keystone.Namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "validator",
						Image: fmt.Sprintf("%s:%s", keystone.Spec.Image.Repository, keystone.Spec.Image.Tag),
						Command: []string{
							"oslopolicy-validator",
							"--namespace", "keystone",
							"--config-file", "/etc/keystone/keystone.conf.d/keystone.conf",
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
