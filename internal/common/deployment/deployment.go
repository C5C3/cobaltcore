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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Feature: CC-0005

// EnsureDeployment creates a Deployment if it does not exist or updates its
// spec if it already exists. It returns (true, nil) when all replicas are
// available, (false, nil) when the Deployment exists but is not yet ready,
// and (false, error) on unexpected failures (CC-0005).
func EnsureDeployment(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, deploy *appsv1.Deployment) (bool, error) {
	existing := &appsv1.Deployment{}
	err := c.Get(ctx, client.ObjectKeyFromObject(deploy), existing)

	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, deploy, scheme); err != nil {
			return false, fmt.Errorf("setting owner reference on Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
		}
		if err := c.Create(ctx, deploy); err != nil {
			return false, fmt.Errorf("creating Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
	}

	// If the selector has changed, the Deployment must be deleted and re-created:
	// Kubernetes rejects .spec.selector mutations as immutable after creation.
	// Returning (false, nil) causes the reconciler to requeue; on the next pass
	// the Deployment no longer exists and will be created fresh (CC-0005).
	if !reflect.DeepEqual(existing.Spec.Selector, deploy.Spec.Selector) {
		if err := c.Delete(ctx, existing); err != nil {
			return false, fmt.Errorf("deleting Deployment %s/%s for selector migration: %w", deploy.Namespace, deploy.Name, err)
		}
		return false, nil
	}

	// Always update the spec to the desired state. This avoids maintaining
	// a normalization layer to replicate API-server defaulting logic, which
	// would be an unmanageable maintenance burden (CC-0005).
	existing.Spec = deploy.Spec
	if err := c.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("updating Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
	}
	// Re-fetch to get the server-assigned Generation after the update (CC-0005).
	if err := c.Get(ctx, client.ObjectKeyFromObject(deploy), existing); err != nil {
		return false, fmt.Errorf("re-fetching Deployment %s/%s after update: %w", deploy.Namespace, deploy.Name, err)
	}

	return IsDeploymentReady(existing), nil
}

// EnsureService creates a Service if it does not exist or updates its spec
// if it already exists. Server-assigned fields (ClusterIP, ClusterIPs,
// IPFamilies) are preserved on updates. If the desired spec explicitly sets
// any of these fields to a value that differs from the existing service, an
// error is returned to signal an API usage problem (CC-0005).
func EnsureService(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, svc *corev1.Service) error {
	existing := &corev1.Service{}
	err := c.Get(ctx, client.ObjectKeyFromObject(svc), existing)

	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, svc, scheme); err != nil {
			return fmt.Errorf("setting owner reference on Service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
		if err := c.Create(ctx, svc); err != nil {
			return fmt.Errorf("creating Service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting Service %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	// Fail fast if the desired spec explicitly sets immutable/server-assigned
	// fields to values that conflict with what the API server has already
	// assigned. This indicates a programming error in the caller (CC-0005).
	if err := validateImmutableServiceFields(svc, existing); err != nil {
		return err
	}

	// Work on a copy of the desired spec so the caller's svc is never
	// mutated — consistent with the other Ensure* functions (CC-0005).
	newSpec := *svc.Spec.DeepCopy()
	// Preserve the ClusterIP assigned by the API server.
	newSpec.ClusterIP = existing.Spec.ClusterIP
	newSpec.ClusterIPs = existing.Spec.ClusterIPs
	newSpec.IPFamilies = existing.Spec.IPFamilies // preserve API-server-assigned IP families (CC-0005)
	// Preserve NodePort values assigned by the API server when the desired
	// spec does not explicitly set them. Matching falls back to Port-only
	// when Protocol is unset, since the API server defaults it to "TCP"
	// (CC-0005).
	for i := range newSpec.Ports {
		if newSpec.Ports[i].NodePort != 0 {
			continue
		}
		for _, ep := range existing.Spec.Ports {
			if newSpec.Ports[i].Port == ep.Port && (newSpec.Ports[i].Protocol == "" || newSpec.Ports[i].Protocol == ep.Protocol) {
				newSpec.Ports[i].NodePort = ep.NodePort
				break
			}
		}
	}
	// Always update the spec to the desired state (CC-0005).
	existing.Spec = newSpec
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating Service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	return nil
}

// validateImmutableServiceFields returns an error if the desired Service spec
// explicitly sets ClusterIP, ClusterIPs, or IPFamilies to values that differ
// from the existing Service. These fields are immutable after creation and
// silently overwriting them would hide API usage problems (CC-0005).
func validateImmutableServiceFields(desired, existing *corev1.Service) error {
	if desired.Spec.ClusterIP != "" && existing.Spec.ClusterIP != "" && desired.Spec.ClusterIP != existing.Spec.ClusterIP {
		return fmt.Errorf(
			"desired ClusterIP %q conflicts with existing %q on Service %s/%s: ClusterIP is immutable after creation (CC-0005)",
			desired.Spec.ClusterIP, existing.Spec.ClusterIP, desired.Namespace, desired.Name,
		)
	}
	if len(desired.Spec.ClusterIPs) > 0 && len(existing.Spec.ClusterIPs) > 0 && !slices.Equal(desired.Spec.ClusterIPs, existing.Spec.ClusterIPs) {
		return fmt.Errorf(
			"desired ClusterIPs %v conflict with existing %v on Service %s/%s: ClusterIPs are immutable after creation (CC-0005)",
			desired.Spec.ClusterIPs, existing.Spec.ClusterIPs, desired.Namespace, desired.Name,
		)
	}
	if len(desired.Spec.IPFamilies) > 0 && len(existing.Spec.IPFamilies) > 0 && !slices.Equal(desired.Spec.IPFamilies, existing.Spec.IPFamilies) {
		return fmt.Errorf(
			"desired IPFamilies %v conflict with existing %v on Service %s/%s: IPFamilies are immutable after creation (CC-0005)",
			desired.Spec.IPFamilies, existing.Spec.IPFamilies, desired.Namespace, desired.Name,
		)
	}
	return nil
}

// IsDeploymentReady returns true if the Deployment has an Available condition
// set to True and its ready replicas meet the desired replica count (CC-0005).
func IsDeploymentReady(deploy *appsv1.Deployment) bool {
	// Guard against stale status: after a spec update the API server
	// increments Generation, but the deployment controller only bumps
	// ObservedGeneration once it has processed the new spec (CC-0005).
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
