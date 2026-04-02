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
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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

	// DECISION: I-001 — Backported metadata reconciliation from EnsurePDB/EnsureHPA
	// to align sibling Ensure* functions in the same package. DeepEqual spec guard
	// is NOT added because the mandatory pattern for mutable resources (Deployments)
	// specifies unconditional spec updates to avoid maintaining a normalization
	// layer. Reviewer: please verify. (CC-0038)

	// Ensure controller owner reference is enforced so garbage collection
	// behaves correctly even if the ref was removed out-of-band (CC-0038).
	if err := controllerutil.SetControllerReference(owner, existing, scheme); err != nil {
		return false, fmt.Errorf("updating owner reference on Deployment %s/%s: %w", existing.Namespace, existing.Name, err)
	}

	// Merge desired labels into existing labels; extra user-added keys are
	// preserved, keys present on the desired Deployment are authoritative (CC-0038).
	if deploy.Labels != nil {
		if existing.Labels == nil {
			existing.Labels = make(map[string]string, len(deploy.Labels))
		}
		for k, v := range deploy.Labels {
			existing.Labels[k] = v
		}
	}

	// Merge desired annotations into existing annotations (CC-0038).
	if deploy.Annotations != nil {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string, len(deploy.Annotations))
		}
		for k, v := range deploy.Annotations {
			existing.Annotations[k] = v
		}
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

	// DECISION: I-001 — Backported metadata reconciliation from EnsurePDB/EnsureHPA.
	// DeepEqual spec guard is NOT added — see EnsureDeployment. (CC-0038)

	// Ensure controller owner reference is enforced so garbage collection
	// behaves correctly even if the ref was removed out-of-band (CC-0038).
	if err := controllerutil.SetControllerReference(owner, existing, scheme); err != nil {
		return fmt.Errorf("updating owner reference on Service %s/%s: %w", existing.Namespace, existing.Name, err)
	}

	// Merge desired labels into existing labels (CC-0038).
	if svc.Labels != nil {
		if existing.Labels == nil {
			existing.Labels = make(map[string]string, len(svc.Labels))
		}
		for k, v := range svc.Labels {
			existing.Labels[k] = v
		}
	}

	// Merge desired annotations into existing annotations (CC-0038).
	if svc.Annotations != nil {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string, len(svc.Annotations))
		}
		for k, v := range svc.Annotations {
			existing.Annotations[k] = v
		}
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

// EnsurePDB creates a PodDisruptionBudget if it does not exist or updates its
// spec and metadata if it already exists. An owner reference is set on the PDB
// so that it is garbage-collected when the owning resource is deleted. On the
// update path, owner references, labels, and annotations are reconciled to
// correct any out-of-band drift (CC-0037).
func EnsurePDB(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, pdb *policyv1.PodDisruptionBudget) error {
	existing := &policyv1.PodDisruptionBudget{}
	err := c.Get(ctx, client.ObjectKeyFromObject(pdb), existing)

	// PDB does not exist yet: set owner reference and create.
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, pdb, scheme); err != nil {
			return fmt.Errorf("setting owner reference on PodDisruptionBudget %s/%s: %w", pdb.Namespace, pdb.Name, err)
		}
		if err := c.Create(ctx, pdb); err != nil {
			return fmt.Errorf("creating PodDisruptionBudget %s/%s: %w", pdb.Namespace, pdb.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting PodDisruptionBudget %s/%s: %w", pdb.Namespace, pdb.Name, err)
	}

	// PDB exists: reconcile metadata (ownerRefs/labels/annotations) and spec.
	// Snapshot before mutations to detect whether an update is necessary (CC-0037).
	before := existing.DeepCopy()

	// Ensure controller owner reference is enforced so garbage collection
	// behaves correctly even if the ref was removed out-of-band (CC-0037).
	if err := controllerutil.SetControllerReference(owner, existing, scheme); err != nil {
		return fmt.Errorf("updating owner reference on PodDisruptionBudget %s/%s: %w", existing.Namespace, existing.Name, err)
	}

	// Merge desired labels into existing labels; extra user-added keys are
	// preserved, keys present on the desired PDB are authoritative (CC-0037).
	if pdb.Labels != nil {
		if existing.Labels == nil {
			existing.Labels = make(map[string]string, len(pdb.Labels))
		}
		for k, v := range pdb.Labels {
			existing.Labels[k] = v
		}
	}

	// Merge desired annotations into existing annotations (CC-0037).
	if pdb.Annotations != nil {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string, len(pdb.Annotations))
		}
		for k, v := range pdb.Annotations {
			existing.Annotations[k] = v
		}
	}

	// Reconcile spec to the desired state (CC-0037).
	existing.Spec = pdb.Spec

	// Only issue an API update when something actually changed to avoid
	// unnecessary write load and events (CC-0037).
	if !apiequality.Semantic.DeepEqual(existing.Spec, before.Spec) ||
		!apiequality.Semantic.DeepEqual(normalizeMap(existing.Labels), normalizeMap(before.Labels)) ||
		!apiequality.Semantic.DeepEqual(normalizeMap(existing.Annotations), normalizeMap(before.Annotations)) ||
		!apiequality.Semantic.DeepEqual(existing.OwnerReferences, before.OwnerReferences) {
		if err := c.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating PodDisruptionBudget %s/%s: %w", existing.Namespace, existing.Name, err)
		}
	}

	return nil
}

// EnsureHPA creates a HorizontalPodAutoscaler if it does not exist or updates
// its spec and metadata if it already exists. An owner reference is set on the
// HPA so that it is garbage-collected when the owning resource is deleted. On
// the update path, owner references, labels, and annotations are reconciled to
// correct any out-of-band drift (CC-0038).
func EnsureHPA(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, hpa *autoscalingv2.HorizontalPodAutoscaler) error {
	existing := &autoscalingv2.HorizontalPodAutoscaler{}
	err := c.Get(ctx, client.ObjectKeyFromObject(hpa), existing)

	// HPA does not exist yet: set owner reference and create.
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, hpa, scheme); err != nil {
			return fmt.Errorf("setting owner reference on HorizontalPodAutoscaler %s/%s: %w", hpa.Namespace, hpa.Name, err)
		}
		if err := c.Create(ctx, hpa); err != nil {
			return fmt.Errorf("creating HorizontalPodAutoscaler %s/%s: %w", hpa.Namespace, hpa.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting HorizontalPodAutoscaler %s/%s: %w", hpa.Namespace, hpa.Name, err)
	}

	// HPA exists: reconcile metadata (ownerRefs/labels/annotations) and spec.
	// Snapshot before mutations to detect whether an update is necessary (CC-0038).
	before := existing.DeepCopy()

	// Ensure controller owner reference is enforced so garbage collection
	// behaves correctly even if the ref was removed out-of-band (CC-0038).
	if err := controllerutil.SetControllerReference(owner, existing, scheme); err != nil {
		return fmt.Errorf("updating owner reference on HorizontalPodAutoscaler %s/%s: %w", existing.Namespace, existing.Name, err)
	}

	// Merge desired labels into existing labels; extra user-added keys are
	// preserved, keys present on the desired HPA are authoritative (CC-0038).
	if hpa.Labels != nil {
		if existing.Labels == nil {
			existing.Labels = make(map[string]string, len(hpa.Labels))
		}
		for k, v := range hpa.Labels {
			existing.Labels[k] = v
		}
	}

	// Merge desired annotations into existing annotations (CC-0038).
	if hpa.Annotations != nil {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string, len(hpa.Annotations))
		}
		for k, v := range hpa.Annotations {
			existing.Annotations[k] = v
		}
	}

	// Reconcile spec to the desired state (CC-0038).
	existing.Spec = hpa.Spec

	// Only issue an API update when something actually changed to avoid
	// unnecessary write load and events (CC-0038).
	if !apiequality.Semantic.DeepEqual(existing.Spec, before.Spec) ||
		!apiequality.Semantic.DeepEqual(normalizeMap(existing.Labels), normalizeMap(before.Labels)) ||
		!apiequality.Semantic.DeepEqual(normalizeMap(existing.Annotations), normalizeMap(before.Annotations)) ||
		!apiequality.Semantic.DeepEqual(existing.OwnerReferences, before.OwnerReferences) {
		if err := c.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating HorizontalPodAutoscaler %s/%s: %w", existing.Namespace, existing.Name, err)
		}
	}

	return nil
}

// DeleteHPA deletes the HorizontalPodAutoscaler identified by namespace and
// name. It is a no-op if the HPA does not exist (CC-0038).
func DeleteHPA(ctx context.Context, c client.Client, namespace, name string) error {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	hpa.SetName(name)
	hpa.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, hpa)); err != nil {
		return fmt.Errorf("deleting HorizontalPodAutoscaler %s/%s: %w", namespace, name, err)
	}
	return nil
}

// normalizeMap converts empty maps to nil so apiequality.Semantic.DeepEqual
// does not report spurious diffs between nil and empty maps (CC-0038).
func normalizeMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
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
