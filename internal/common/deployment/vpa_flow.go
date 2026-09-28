// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"slices"
	"strings"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// VPAGVK is the kind ReconcileVPAs writes. The operators latch it at setup and
// list it among their remote child kinds.
var VPAGVK = vpav1.SchemeGroupVersion.WithKind("VerticalPodAutoscaler")

// Condition reason constants for the VerticalPodAutoscaler readiness
// condition, shared so every operator's condition uses the same vocabulary.
const (
	ReasonVPAReady        = "VPAReady"
	ReasonVPANotRequired  = "VPANotRequired"
	ReasonVPANotInstalled = "VPANotInstalled"
	ReasonVPAError        = "VPAError"
)

// VPATarget is one long-running workload of a CR that may opt into a
// VerticalPodAutoscaler. Kind is Deployment, StatefulSet or DaemonSet, Name is
// the workload's name and the VPA's, and a nil Spec means the workload does
// not opt in.
type VPATarget struct {
	Kind string
	Name string
	Spec *commonv1.VerticalAutoscalingSpec
}

// VPAFlowParams carries everything ReconcileVPAs needs. The target list is the
// operator's; the flow itself is identical across operators.
type VPAFlowParams struct {
	// Targets are every long-running workload the CR renders in this pass.
	Targets []VPATarget
	// Namespace is the namespace of the workloads and their VPAs.
	Namespace string
	// Labels are the CR's common labels. Every VPA of the CR carries them, and
	// the prune step selects by them.
	Labels map[string]string
	// LocalAvailable is the setup-time latch: whether the management cluster
	// serves the VerticalPodAutoscaler kind. Children on a target cluster are
	// probed against that cluster's RESTMapper on every pass instead.
	LocalAvailable bool
	// Conditions is the CR's condition slice, mutated in place.
	Conditions *[]metav1.Condition
	// Generation is stamped onto every condition the flow writes.
	Generation int64
	// ConditionType is the readiness condition the flow reports on.
	ConditionType string
}

// BuildVPA renders the VerticalPodAutoscaler of one opted-in target. The VPA
// takes its workload's name, as the HPA and the PDB take the Deployment's. It
// writes one container policy for every container ("*"), so sidecars are
// controlled like the main container, and it controls requests only: the
// memory limit caps the memory recommendation, and nothing but maxAllowed caps
// the CPU one. An unset minReplicas, minAllowed or maxAllowed renders no key,
// so the VPA default applies.
func BuildVPA(namespace string, labels map[string]string, t VPATarget) *vpav1.VerticalPodAutoscaler {
	policy := vpav1.ContainerResourcePolicy{
		ContainerName:       vpav1.DefaultContainerResourcePolicy,
		ControlledValues:    ptr.To(vpav1.ContainerControlledValuesRequestsOnly),
		ControlledResources: ptr.To([]corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}),
		MinAllowed:          t.Spec.MinAllowed.DeepCopy(),
		MaxAllowed:          t.Spec.MaxAllowed.DeepCopy(),
	}

	vpa := &vpav1.VerticalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t.Name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: vpav1.VerticalPodAutoscalerSpec{
			TargetRef: &autoscalingv1.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       t.Kind,
				Name:       t.Name,
			},
			UpdatePolicy: &vpav1.PodUpdatePolicy{
				UpdateMode: ptr.To(vpav1.UpdateMode(t.Spec.UpdateMode)),
			},
			ResourcePolicy: &vpav1.PodResourcePolicy{
				ContainerPolicies: []vpav1.ContainerResourcePolicy{policy},
			},
		},
	}
	if t.Spec.MinReplicas != nil {
		vpa.Spec.UpdatePolicy.MinReplicas = ptr.To(*t.Spec.MinReplicas)
	}
	return vpa
}

// ReconcileVPAs makes the VerticalPodAutoscalers of a CR match its opted-in
// targets. It is the shared body of every operator's reconcileVPA
// sub-reconciler:
//
//   - The children's cluster could not be probed for the kind: the condition
//     turns False/CapabilityProbeFailed and the error is returned.
//   - The cluster does not serve the kind: no VPA can exist there, so nothing is
//     listed or deleted. The condition is True/VPANotRequired without an
//     opt-in and False/VPANotInstalled with one.
//   - The cluster serves the kind: every opted-in target's VPA is applied via
//     SSA, every other VPA carrying the CR's labels that the CR controls
//     (multicluster.Controls) is deleted, and the condition is True/VPAReady
//     (or VPANotRequired without an opt-in).
//
// A pass on a serving cluster costs one cached list plus one server-side apply
// per opted-in target, a no-op when nothing changed. On a target cluster that
// does not serve the kind it costs one discovery round-trip. The recommender
// rewrites every VPA's status.recommendation about once a minute, so the
// operators watch their VPAs through predicate.GenerationChangedPredicate and a
// status write never wakes the owning CR. The flow never requeues on its own.
func ReconcileVPAs(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, p VPAFlowParams) (ctrl.Result, error) {
	setCondition := func(status metav1.ConditionStatus, reason, message string) {
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             status,
			ObservedGeneration: p.Generation,
			Reason:             reason,
			Message:            message,
		})
	}
	const notRequiredMsg = "no workload opts into vertical autoscaling"

	serves, err := multicluster.ChildrenServeKind(c, p.LocalAvailable, VPAGVK)
	if err != nil {
		setCondition(metav1.ConditionFalse, multicluster.CapabilityProbeFailed,
			fmt.Sprintf("Probing the target cluster for the VerticalPodAutoscaler kind failed: %v", err))
		return ctrl.Result{}, fmt.Errorf("probing the target cluster for the VerticalPodAutoscaler kind: %w", err)
	}

	var wanted []string
	for _, t := range p.Targets {
		if t.Spec != nil {
			wanted = append(wanted, t.Name)
		}
	}

	if !serves {
		if len(wanted) == 0 {
			setCondition(metav1.ConditionTrue, ReasonVPANotRequired, notRequiredMsg)
			return ctrl.Result{}, nil
		}
		// A local answer comes from the setup-time latch, which only a restart
		// refreshes; a remote one from a probe on every pass.
		msg := fmt.Sprintf("the cluster does not serve autoscaling.k8s.io/v1 VerticalPodAutoscaler; "+
			"opted-in workloads: %s (install the VerticalPodAutoscaler and restart the operator)",
			strings.Join(wanted, ", "))
		if multicluster.IsRemote(c) {
			msg = fmt.Sprintf("the target cluster does not serve autoscaling.k8s.io/v1 VerticalPodAutoscaler; "+
				"opted-in workloads: %s (the operator re-checks on every reconcile; rotate the cluster's "+
				"kubeconfig Secret to restore the VerticalPodAutoscaler drift watch)", strings.Join(wanted, ", "))
		}
		setCondition(metav1.ConditionFalse, ReasonVPANotInstalled, msg)
		return ctrl.Result{}, nil
	}

	var list vpav1.VerticalPodAutoscalerList
	if err := c.List(ctx, &list, client.InNamespace(p.Namespace), client.MatchingLabels(p.Labels)); err != nil {
		setCondition(metav1.ConditionFalse, ReasonVPAError, fmt.Sprintf("Listing VerticalPodAutoscalers failed: %v", err))
		return ctrl.Result{}, fmt.Errorf("listing VerticalPodAutoscalers: %w", err)
	}

	for _, t := range p.Targets {
		if t.Spec == nil {
			continue
		}
		if err := apply.EnsureObject(ctx, c, scheme, owner, BuildVPA(p.Namespace, p.Labels, t), apply.FieldManager); err != nil {
			setCondition(metav1.ConditionFalse, ReasonVPAError,
				fmt.Sprintf("Ensuring VerticalPodAutoscaler %s failed: %v", t.Name, err))
			return ctrl.Result{}, fmt.Errorf("ensuring VerticalPodAutoscaler %s/%s: %w", p.Namespace, t.Name, err)
		}
	}

	for i := range list.Items {
		vpa := &list.Items[i]
		if slices.Contains(wanted, vpa.Name) {
			continue
		}
		// The labels select, ownership decides: a VPA whose labels were copied
		// from the workload is somebody else's and stays.
		owned, err := multicluster.Controls(scheme, owner, vpa)
		if err != nil {
			setCondition(metav1.ConditionFalse, ReasonVPAError,
				fmt.Sprintf("Checking ownership of VerticalPodAutoscaler %s failed: %v", vpa.Name, err))
			return ctrl.Result{}, fmt.Errorf("checking ownership of VerticalPodAutoscaler %s/%s: %w", vpa.Namespace, vpa.Name, err)
		}
		if !owned {
			continue
		}
		if err := client.IgnoreNotFound(c.Delete(ctx, vpa)); err != nil {
			setCondition(metav1.ConditionFalse, ReasonVPAError,
				fmt.Sprintf("Deleting VerticalPodAutoscaler %s failed: %v", vpa.Name, err))
			return ctrl.Result{}, fmt.Errorf("deleting VerticalPodAutoscaler %s/%s: %w", vpa.Namespace, vpa.Name, err)
		}
	}

	if len(wanted) == 0 {
		setCondition(metav1.ConditionTrue, ReasonVPANotRequired, notRequiredMsg)
		return ctrl.Result{}, nil
	}
	setCondition(metav1.ConditionTrue, ReasonVPAReady,
		fmt.Sprintf("VerticalPodAutoscalers are configured: %s", strings.Join(wanted, ", ")))
	return ctrl.Result{}, nil
}
