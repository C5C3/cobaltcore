// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package rotation

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// StagingSecretLabelKey labels staging Secrets so operator watches and
// consumers can distinguish them from the production key Secrets.
const StagingSecretLabelKey = "forge.c5c3.io/rotation-target" //nolint:gosec // label key, not a credential

// CompletedAnnotation is the RFC3339 UTC timestamp the rotation CronJob writes
// atomically with its staging Secret PATCH. Its presence is the single-shot
// commit marker gating the operator's apply path.
const CompletedAnnotation = "forge.c5c3.io/rotation-completed-at" //nolint:gosec // annotation key, not a credential

// CompletedAt parses the CompletedAnnotation off the given Secret. It returns
// (zero, false) when the Secret is nil, the annotation is missing, or the
// timestamp does not parse as RFC3339 — all valid steady-state observations for
// the gauge caller. It performs no client reads.
func CompletedAt(secret *corev1.Secret) (time.Time, bool) {
	if secret == nil {
		return time.Time{}, false
	}
	completedAt := secret.Annotations[CompletedAnnotation]
	if completedAt == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, completedAt)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// ObserveAge invokes record with the rotation-completed timestamp, preferring
// the production (main) Secret — where CommitStaged stamps it durably — and
// falling back to the staging Secret to cover the post-PATCH/pre-apply window.
// It is best-effort: an absent annotation on both (either Secret may be nil) or
// a malformed timestamp is silently skipped, and record's error is ignored, so
// a metric refresh never surfaces as a reconcile failure.
func ObserveAge(mainSecret, stagingSecret *corev1.Secret, record func(completedAt time.Time) error) {
	if completedAt, ok := CompletedAt(mainSecret); ok {
		_ = record(completedAt)
		return
	}
	if completedAt, ok := CompletedAt(stagingSecret); ok {
		_ = record(completedAt)
	}
}

// EnsureStagingSecret creates (or ensures) an empty staging Secret that the
// rotation CronJob PATCHes rotated payload into. The operator owns the object's
// metadata and lifecycle — labels, owner reference — while the CronJob owns the
// `.data` field via a narrow get+patch RBAC grant. Data is deliberately left nil
// on creation and untouched on update so the CronJob's PATCH is never clobbered
// by a reconcile. labels is the complete label set applied on every call
// (rebuilt so operator-owned labels stay authoritative).
//
// It returns the ensured Secret so the caller can thread it into ObserveAge and
// CommitStaged without re-reading it; its UID/ResourceVersion drive the commit's
// delete preconditions.
func EnsureStagingSecret(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, name string, labels map[string]string) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: owner.GetNamespace(),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		secret.Labels = labels
		// Do NOT touch secret.Data — the CronJob's PATCH owns that field.
		return controllerutil.SetControllerReference(owner, secret, scheme)
	}); err != nil {
		return nil, fmt.Errorf("ensuring staging secret %s: %w", name, err)
	}
	return secret, nil
}

// CommitSpec parameterises CommitStaged for one rotation flavour. Every
// difference between flavours is captured here; the commit sequence is identical.
type CommitSpec struct {
	// TargetNoun names the target Secret in returned error messages.
	TargetNoun string
	// Validate enforces the per-flavour staging-payload contract; a non-nil
	// error rejects the commit.
	Validate func(data map[string][]byte) error
	// ClearStagingOnReject, when true, wipes the staged .data and completion
	// annotation when Validate rejects the payload, so the next CronJob
	// strategic-merge PATCH starts from an empty base rather than accumulating
	// leftover entries over the rejected payload.
	ClearStagingOnReject bool
	// AnnotationInvalidReason / RejectedReason are the Warning event reasons
	// emitted when the completion annotation is malformed or the payload is
	// rejected.
	AnnotationInvalidReason string
	RejectedReason          string
	// AppliedReason / AppliedMessage build the Normal event emitted on a
	// successful commit.
	AppliedReason  string
	AppliedMessage func(stagingSecretName string, data map[string][]byte) string
}

// CommitStaged copies a completed staging Secret onto an operator-owned target
// Secret and deletes the staging Secret. It is the operator-side commit for the
// split-compute-write rotation architecture, shared by every rotation flavour
// via CommitSpec:
//
//  1. Require CompletedAnnotation present and parseable as RFC3339; a malformed
//     annotation emits spec.AnnotationInvalidReason and is retried next run.
//  2. Validate the staged Data via spec.Validate; a rejection emits
//     spec.RejectedReason and, when spec.ClearStagingOnReject, clears the staged
//     Data and completion annotation.
//  3. Replace the target's Data verbatim, stamp the annotation, and Update —
//     full replacement realises the atomic swap.
//  4. Delete the staging Secret under UID+ResourceVersion preconditions from the
//     caller's read; tolerate NotFound and Conflict.
//  5. Emit a Normal event from spec.AppliedReason / spec.AppliedMessage.
//
// staging is the object EnsureStagingSecret returned; target is the
// operator-owned Secret the caller already read. Both are threaded in rather
// than re-read. A nil staging is a no-op.
func CommitStaged(ctx context.Context, c client.Client, recorder record.EventRecorder, owner client.Object, staging, target *corev1.Secret, spec CommitSpec) (applied bool, err error) {
	if staging == nil {
		return false, nil
	}
	stagingName := staging.Name

	completedAt := staging.Annotations[CompletedAnnotation]
	if completedAt == "" {
		if len(staging.Data) > 0 {
			log.FromContext(ctx).V(1).Info(
				"staging secret has data without completion annotation; "+
					"skipping apply until next CronJob run writes the annotation",
				"staging", stagingName,
				"annotation", CompletedAnnotation,
				"dataKeys", len(staging.Data),
			)
		}
		return false, nil
	}
	if _, parseErr := time.Parse(time.RFC3339, completedAt); parseErr != nil {
		recorder.Eventf(owner, corev1.EventTypeWarning, spec.AnnotationInvalidReason,
			"staging secret %s has malformed %s annotation: %v",
			stagingName, CompletedAnnotation, parseErr)
		return false, nil
	}

	if valErr := spec.Validate(staging.Data); valErr != nil {
		recorder.Eventf(owner, corev1.EventTypeWarning, spec.RejectedReason,
			"staging secret %s rejected: %v", stagingName, valErr)
		if spec.ClearStagingOnReject {
			staging.Data = nil
			delete(staging.Annotations, CompletedAnnotation)
			if updErr := c.Update(ctx, staging); updErr != nil {
				if !apierrors.IsConflict(updErr) && !apierrors.IsNotFound(updErr) {
					return false, fmt.Errorf("clearing rejected staging secret %s: %w", stagingName, updErr)
				}
				log.FromContext(ctx).V(1).Info(
					"rejected staging secret changed since read; clear retried next reconcile",
					"staging", stagingName,
				)
			}
		}
		return false, nil
	}

	// Skip the target Update and the success event when the target already holds
	// this exact payload and completion timestamp: a prior pass committed it and
	// only its staging delete is outstanding. Step 4 still runs so the staging
	// Secret is cleaned up.
	alreadyCommitted := maps.EqualFunc(target.Data, staging.Data, bytes.Equal) &&
		target.Annotations[CompletedAnnotation] == completedAt

	if !alreadyCommitted {
		target.Data = staging.Data
		if target.Annotations == nil {
			target.Annotations = map[string]string{}
		}
		target.Annotations[CompletedAnnotation] = completedAt
		if updateErr := c.Update(ctx, target); updateErr != nil {
			return false, fmt.Errorf("updating %s %s: %w", spec.TargetNoun, target.Name, updateErr)
		}
	}

	// Delete the staging Secret under UID+ResourceVersion preconditions so a
	// concurrent CronJob PATCH surfaces as Conflict rather than being silently
	// deleted uncommitted; the newer payload commits next reconcile. Conflict and
	// NotFound are both tolerated — this run's payload is already on the target.
	delOpts := client.Preconditions{UID: &staging.UID, ResourceVersion: &staging.ResourceVersion}
	if delErr := c.Delete(ctx, staging, delOpts); delErr != nil {
		if !apierrors.IsNotFound(delErr) && !apierrors.IsConflict(delErr) {
			return false, fmt.Errorf("deleting staging secret %s: %w", stagingName, delErr)
		}
		log.FromContext(ctx).V(1).Info(
			"staging secret changed since read; newer rotation output applied next reconcile",
			"staging", stagingName,
		)
	}

	if alreadyCommitted {
		return false, nil
	}

	recorder.Event(owner, corev1.EventTypeNormal, spec.AppliedReason,
		spec.AppliedMessage(stagingName, staging.Data))
	return true, nil
}

// EnsureRBAC creates the ServiceAccount, Role, and RoleBinding a
// split-compute-write rotation CronJob runs under, all named saName and owned by
// owner. The Role is split into two least-privilege rules: read-only get on
// readSecret (the operator-owned source the CronJob reads), and get+patch scoped
// by resourceNames to stagingSecret (the CronJob's write target; create/delete
// are withheld because the operator owns the staging Secret's lifecycle). RoleRef
// is immutable after creation, so it is only set on new RoleBindings.
func EnsureRBAC(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, saName, readSecret, stagingSecret string) error {
	namespace := owner.GetNamespace()

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, sa, func() error {
		return controllerutil.SetControllerReference(owner, sa, scheme)
	}); err != nil {
		return fmt.Errorf("ensuring ServiceAccount %s: %w", saName, err)
	}

	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				Verbs:         []string{"get"},
				ResourceNames: []string{readSecret},
			},
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				Verbs:         []string{"get", "patch"},
				ResourceNames: []string{stagingSecret},
			},
		}
		return controllerutil.SetControllerReference(owner, role, scheme)
	}); err != nil {
		return fmt.Errorf("ensuring Role %s: %w", saName, err)
	}

	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, rb, func() error {
		rb.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: namespace,
		}}
		if rb.RoleRef.Name == "" {
			rb.RoleRef = rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     saName,
			}
		}
		return controllerutil.SetControllerReference(owner, rb, scheme)
	}); err != nil {
		return fmt.Errorf("ensuring RoleBinding %s: %w", saName, err)
	}

	return nil
}

// CronJobParams carries the inputs of BuildCronJob: the CronJob identity, its
// schedule, and the fully-assembled Pod (the operator supplies the
// service-specific init/main containers and volumes).
type CronJobParams struct {
	Name      string
	Namespace string
	Labels    map[string]string
	Schedule  string
	// PodLabels are set on the Job's Pod template.
	PodLabels map[string]string
	// PodSpec is the fully-assembled rotation Pod spec.
	PodSpec corev1.PodSpec
}

// BuildCronJob wraps the caller's Pod spec in the CronJob/JobTemplate/PodTemplate
// boilerplate shared by every rotation CronJob.
func BuildCronJob(p CronJobParams) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.Labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: p.Schedule,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: p.PodLabels},
						Spec:       p.PodSpec,
					},
				},
			},
		},
	}
}
