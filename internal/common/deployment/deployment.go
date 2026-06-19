// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/apply"
)

// EnsureDeployment creates a Deployment if it does not exist or applies its
// desired state via Server-Side Apply if it already exists. It returns
// (true, nil) when all replicas are available, (false, nil) when the Deployment
// exists but is not yet ready, and (false, error) on unexpected failures.
func EnsureDeployment(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, deploy *appsv1.Deployment) (bool, error) {
	existing := &appsv1.Deployment{}
	err := c.Get(ctx, client.ObjectKeyFromObject(deploy), existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("getting Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
	}

	if apierrors.IsNotFound(err) {
		// Write an explicit replica count on create so the API server does not
		// implicitly default a nil .spec.replicas to 1. A nil here means the
		// caller deferred ownership to an HPA (see the update path below); the
		// operator picks an explicit starting value of 1 and the HPA scales it
		// up to its minReplicas on its first sync (issue #462).
		if deploy.Spec.Replicas == nil {
			replicas := int32(1)
			deploy.Spec.Replicas = &replicas
		}
	} else {
		// If the selector has changed, the Deployment must be deleted and
		// re-created: Kubernetes rejects .spec.selector mutations as immutable
		// after creation. Returning (false, nil) causes the reconciler to
		// requeue; on the next pass the Deployment no longer exists and is
		// created fresh.
		if !reflect.DeepEqual(existing.Spec.Selector, deploy.Spec.Selector) {
			if err := c.Delete(ctx, existing); err != nil {
				return false, fmt.Errorf("deleting Deployment %s/%s for selector migration: %w", deploy.Namespace, deploy.Name, err)
			}
			return false, nil
		}

		// Preserve the live replica count when the caller leaves it unset. A nil
		// desired .spec.replicas signals that an HPA owns the field, so the
		// operator must not reset it to a static value on every reconcile —
		// otherwise each write fights the HPA and the resulting watch event
		// re-triggers reconciliation, producing a scale-up/scale-down loop with
		// real pod churn (issue #462). Writing back the live value keeps the
		// Server-Side Apply a no-op while the HPA is in control.
		if deploy.Spec.Replicas == nil {
			deploy.Spec.Replicas = existing.Spec.Replicas
		}
	}

	// Server-Side Apply manages only the fields the builder sets, so server
	// defaults are never clobbered and a converged spec is applied without a
	// write. The apply response is decoded back into deploy, so readiness is
	// read from server-fresh state without a re-Get that the reconciler's cache
	// could answer with a stale (pre-update) object during an upgrade.
	if err := apply.EnsureObject(ctx, c, scheme, owner, deploy, apply.FieldManager); err != nil {
		return false, err
	}

	return IsDeploymentReady(deploy), nil
}

// EnsureService creates a Service if it does not exist or applies its desired
// state via Server-Side Apply if it already exists. Server-assigned fields
// (ClusterIP, ClusterIPs, IPFamilies, NodePort) are left unset by the builder,
// so the operator's field manager never owns them and the API server keeps the
// values it assigned. If the desired spec explicitly sets any of these fields
// to a value that differs from the existing Service, an error is returned to
// signal an API usage problem before the apply is attempted.
func EnsureService(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, svc *corev1.Service) error {
	existing := &corev1.Service{}
	err := c.Get(ctx, client.ObjectKeyFromObject(svc), existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting Service %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	if err == nil {
		// Fail fast if the desired spec explicitly sets immutable/server-assigned
		// fields to values that conflict with what the API server has already
		// assigned. This indicates a programming error in the caller.
		if err := validateImmutableServiceFields(svc, existing); err != nil {
			return err
		}
	}

	return apply.EnsureObject(ctx, c, scheme, owner, svc, apply.FieldManager)
}

// EnsurePDB creates a PodDisruptionBudget if it does not exist or applies its
// desired state via Server-Side Apply if it already exists. An owner reference
// is set so the PDB is garbage-collected when the owning resource is deleted,
// and the reference is re-enforced on every apply so out-of-band drift is
// corrected.
func EnsurePDB(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, pdb *policyv1.PodDisruptionBudget) error {
	return apply.EnsureObject(ctx, c, scheme, owner, pdb, apply.FieldManager)
}

// EnsureHPA creates a HorizontalPodAutoscaler if it does not exist or applies
// its desired state via Server-Side Apply if it already exists. An owner
// reference is set so the HPA is garbage-collected when the owning resource is
// deleted, and the reference is re-enforced on every apply so out-of-band drift
// is corrected.
func EnsureHPA(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, hpa *autoscalingv2.HorizontalPodAutoscaler) error {
	return apply.EnsureObject(ctx, c, scheme, owner, hpa, apply.FieldManager)
}

// DeleteHPA deletes the HorizontalPodAutoscaler identified by namespace and
// name. It is a no-op if the HPA does not exist.
func DeleteHPA(ctx context.Context, c client.Client, namespace, name string) error {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	hpa.SetName(name)
	hpa.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, hpa)); err != nil {
		return fmt.Errorf("deleting HorizontalPodAutoscaler %s/%s: %w", namespace, name, err)
	}
	return nil
}

// validateImmutableServiceFields returns an error if the desired Service spec
// explicitly sets ClusterIP, ClusterIPs, or IPFamilies to values that differ
// from the existing Service. These fields are immutable after creation and
// silently overwriting them would hide API usage problems.
func validateImmutableServiceFields(desired, existing *corev1.Service) error {
	if desired.Spec.ClusterIP != "" && existing.Spec.ClusterIP != "" && desired.Spec.ClusterIP != existing.Spec.ClusterIP {
		return fmt.Errorf(
			"desired ClusterIP %q conflicts with existing %q on Service %s/%s: ClusterIP is immutable after creation",
			desired.Spec.ClusterIP, existing.Spec.ClusterIP, desired.Namespace, desired.Name,
		)
	}
	if len(desired.Spec.ClusterIPs) > 0 && len(existing.Spec.ClusterIPs) > 0 && !slices.Equal(desired.Spec.ClusterIPs, existing.Spec.ClusterIPs) {
		return fmt.Errorf(
			"desired ClusterIPs %v conflict with existing %v on Service %s/%s: ClusterIPs are immutable after creation",
			desired.Spec.ClusterIPs, existing.Spec.ClusterIPs, desired.Namespace, desired.Name,
		)
	}
	if len(desired.Spec.IPFamilies) > 0 && len(existing.Spec.IPFamilies) > 0 && !slices.Equal(desired.Spec.IPFamilies, existing.Spec.IPFamilies) {
		return fmt.Errorf(
			"desired IPFamilies %v conflict with existing %v on Service %s/%s: IPFamilies are immutable after creation",
			desired.Spec.IPFamilies, existing.Spec.IPFamilies, desired.Namespace, desired.Name,
		)
	}
	return nil
}

// IsDeploymentReady returns true if the Deployment has an Available condition
// set to True and its ready replicas meet the desired replica count.
func IsDeploymentReady(deploy *appsv1.Deployment) bool {
	// Guard against stale status: after a spec update the API server
	// increments Generation, but the deployment controller only bumps
	// ObservedGeneration once it has processed the new spec.
	if deploy.Status.ObservedGeneration < deploy.Generation {
		return false
	}
	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}
	if deploy.Status.ReadyReplicas < desired {
		return false
	}
	for _, c := range deploy.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
