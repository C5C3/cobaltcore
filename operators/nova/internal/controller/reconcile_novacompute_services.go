// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi"
)

// The reasons of the ServicesReady condition. ComputeAPIError is shared with
// AggregatesReady.
const (
	conditionReasonServicesUp            = "ServicesUp"
	conditionReasonDraining              = "Draining"
	conditionReasonWaitingForServices    = "WaitingForServices"
	conditionReasonServicesDown          = "ServicesDown"
	conditionReasonPodListError          = "PodListError"
	conditionReasonWaitingForHostMapping = "WaitingForHostMapping"
	conditionReasonHostDiscoveryError    = "HostDiscoveryError"
)

// The os-services values the step reads and writes.
const (
	serviceStatusEnabled  = "enabled"
	serviceStatusDisabled = "disabled"
	serviceStateDown      = "down"
)

// reconcileNovaComputeServices reads the compute services from Nova, moves
// every node of the pool along its phase, and sets ServicesReady. Its requeue
// is the pool's periodic poll of Nova.
//
//   - Pending and Active follow whether a service is registered under the node
//     name and its host is mapped into its cell. A Pending node turns Active
//     once Nova lists its host as a hypervisor, which it does only for a
//     mapped host; an Active node is not checked again. While a registered
//     host is unmapped, the step runs the Nova's host discovery Job
//     (ensureHostDiscovery) and polls every RequeueHostDiscoveryPolling.
//   - Draining disables the node's service once, with a reason naming this
//     CR, and waits until Nova counts no server on the host. The satellite
//     never migrates an instance and never enables a service: emptying the
//     host is openstack-hypervisor-operator's eviction, or the owner's.
//   - Releasing waits until the node runs no pod of this pool, then deletes
//     the service, which takes the host out of every aggregate and drops its
//     host mapping. A node another pool took over, or one whose service is
//     already gone, is dropped with no call. A delete Nova refuses because
//     instances came back returns the node to Draining.
//   - Conflict is left alone.
func (r *NovaComputeReconciler) reconcileNovaComputeServices(ctx context.Context, children client.Client,
	cr *novav1alpha1.NovaCompute, pass *novaComputePass,
) (ctrl.Result, error) {
	// The poll is the shortest any node needs, at entry and at exit: a node
	// dropped from Releasing keeps the short interval, so the aggregate it
	// leaves behind is looked at again soon.
	requeue := RequeueComputeServicePolling
	var releasing []string
	for _, entry := range cr.Status.Nodes {
		requeue = min(requeue, phasePollInterval(entry.Phase))
		if entry.Phase == novav1alpha1.NovaComputeNodeReleasing {
			releasing = append(releasing, entry.Name)
		}
	}

	api, err := pass.computeClient(ctx)
	if err != nil {
		return computeAPIFailed(cr, conditionTypeServicesReady, fmt.Errorf("authenticating: %w", err))
	}
	services, err := api.ListComputeServices(ctx)
	if err != nil {
		return computeAPIFailed(cr, conditionTypeServicesReady, fmt.Errorf("listing compute services: %w", err))
	}
	byHost := make(map[string]computeapi.Service, len(services))
	for _, svc := range services {
		byHost[svc.Host] = svc
	}
	// The mapping is read only while a Pending node has a registered service,
	// so a settled pool makes no further call. A failed read leaves those nodes
	// Pending; the walk still moves the other nodes, and the failure is
	// reported after it.
	var mapped map[string]bool
	var mappedErr error
	if slices.ContainsFunc(cr.Status.Nodes, func(entry novav1alpha1.NovaComputeNodeStatus) bool {
		_, registered := byHost[entry.Name]
		return registered && entry.Phase == novav1alpha1.NovaComputeNodePending
	}) {
		mapped, mappedErr = api.ListMappedHosts(ctx)
	}

	podReader, err := commonmulticluster.ResolveChildrenAPIReader(ctx, r.Resolver, r.APIReader, cr.Spec.TargetClusterRef)
	if err != nil {
		novaComputeSkeleton.MarkFailed(cr, conditionTypeServicesReady, commonmulticluster.TargetClusterUnavailable, err)
		return ctrl.Result{RequeueAfter: RequeueComputeDrainPolling}, nil
	}
	if podReader == nil {
		podReader = children
	}

	// podNodes are the nodes a pod of this pool still runs on, listed at the
	// first Releasing node: the pods on that node when it is the only one,
	// else the pool's pods once rather than once per node.
	var podNodes map[string]bool
	podsOn := ""
	if len(releasing) == 1 {
		podsOn = releasing[0]
	}

	nodes := make([]novav1alpha1.NovaComputeNodeStatus, 0, len(cr.Status.Nodes))
	// unmapped are the Pending nodes with a registered service whose host Nova
	// does not list as mapped yet.
	var unmapped []string
	// stepErr stops the walk at the first failed call; the entries not yet
	// walked are kept as they were.
	var stepErr error
	for i, entry := range cr.Status.Nodes {
		if stepErr != nil {
			nodes = append(nodes, cr.Status.Nodes[i:]...)
			break
		}
		svc, registered := byHost[entry.Name]
		keep := true
		switch entry.Phase {
		case novav1alpha1.NovaComputeNodeConflict:
			// Another pool holds the node, and its service with it.
		case novav1alpha1.NovaComputeNodePending, novav1alpha1.NovaComputeNodeActive:
			wasActive := entry.Phase == novav1alpha1.NovaComputeNodeActive
			copyService(&entry, svc, registered)
			entry.Phase = novav1alpha1.NovaComputeNodePending
			switch {
			case registered && (wasActive || mapped[entry.Name]):
				entry.Phase = novav1alpha1.NovaComputeNodeActive
			case registered:
				unmapped = append(unmapped, entry.Name)
			}
		case novav1alpha1.NovaComputeNodeDraining:
			copyService(&entry, svc, registered)
			stepErr = r.drainNode(ctx, api, cr, &entry, registered)
		case novav1alpha1.NovaComputeNodeReleasing:
			copyService(&entry, svc, registered)
			if podNodes == nil {
				if podNodes, stepErr = r.nodesRunningPods(ctx, podReader, cr, podsOn); stepErr != nil {
					break
				}
			}
			if podNodes[entry.Name] {
				break
			}
			keep, stepErr = r.releaseNode(ctx, api, cr, &entry, registered, pass.handover[entry.Name])
		}
		if keep {
			nodes = append(nodes, entry)
		}
	}
	cr.Status.Nodes = nodes
	for _, entry := range nodes {
		requeue = min(requeue, phasePollInterval(entry.Phase))
	}

	if stepErr != nil {
		var podErr podListError
		if errors.As(stepErr, &podErr) {
			novaComputeSkeleton.MarkFailed(cr, conditionTypeServicesReady, conditionReasonPodListError, stepErr)
			return ctrl.Result{}, stepErr
		}
		return computeAPIFailed(cr, conditionTypeServicesReady, stepErr)
	}
	// Both failures below leave a Pending node behind, which holds requeue at
	// RequeueComputeDrainPolling or below. They requeue at that poll rather
	// than return an error, whose growing backoff would stretch the polls of
	// the pool's other nodes too.
	if mappedErr != nil {
		novaComputeSkeleton.MarkFailed(cr, conditionTypeServicesReady, conditionReasonComputeAPIError,
			fmt.Errorf("listing hypervisors: %w", mappedErr))
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	note := ""
	if len(unmapped) > 0 {
		if note, err = r.ensureHostDiscovery(ctx, cr, pass, unmapped); err != nil {
			log.FromContext(ctx).Error(err, "unable to run the host discovery Job")
			novaComputeSkeleton.MarkFailed(cr, conditionTypeServicesReady, conditionReasonHostDiscoveryError, err)
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
		requeue = min(requeue, RequeueHostDiscoveryPolling)
	}

	conditions.SetCondition(&cr.Status.Conditions, servicesCondition(cr, nodes, unmapped, note))
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// copyService records what Nova reports for the node's service on its entry,
// or clears it when none is registered.
func copyService(entry *novav1alpha1.NovaComputeNodeStatus, svc computeapi.Service, registered bool) {
	if !registered {
		entry.ServiceID, entry.ServiceStatus, entry.ServiceState, entry.DisabledReason = "", "", "", ""
		return
	}
	entry.ServiceID = svc.ID
	entry.ServiceStatus = svc.Status
	entry.ServiceState = svc.State
	entry.DisabledReason = svc.DisabledReason
}

// drainNode disables an enabled service once and counts the servers left on
// the host: Draining while there are any, Releasing once there are none. A
// service someone else disabled keeps its reason. With no service registered
// the count still runs, since a server outlives its host's service record.
func (r *NovaComputeReconciler) drainNode(ctx context.Context, api *computeapi.Client, cr *novav1alpha1.NovaCompute,
	entry *novav1alpha1.NovaComputeNodeStatus, registered bool,
) error {
	if registered && entry.ServiceStatus == serviceStatusEnabled {
		reason := fmt.Sprintf("c5c3.io: leaving NovaCompute %s/%s", cr.Namespace, cr.Name)
		if err := api.DisableService(ctx, entry.ServiceID, reason); err != nil {
			return fmt.Errorf("disabling the compute service of %s: %w", entry.Name, err)
		}
		entry.ServiceStatus = serviceStatusDisabled
		entry.DisabledReason = reason
		r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonComputeServiceDisabled,
			"Disabled the compute service of node %s: it left the pool", entry.Name)
	}

	count, err := api.CountServersOnHost(ctx, entry.Name)
	if err != nil {
		return fmt.Errorf("counting the servers on %s: %w", entry.Name, err)
	}
	if count > 0 {
		entry.Instances = ptr.To(int32(min(count, math.MaxInt32)))
		return nil
	}
	entry.Instances = nil
	entry.Phase = novav1alpha1.NovaComputeNodeReleasing
	return nil
}

// releaseNode finishes a Releasing node whose pod is gone. It reports whether
// the entry stays.
func (r *NovaComputeReconciler) releaseNode(ctx context.Context, api *computeapi.Client, cr *novav1alpha1.NovaCompute,
	entry *novav1alpha1.NovaComputeNodeStatus, registered, handover bool,
) (bool, error) {
	// Under a handover the service belongs to the pool taking the node, and
	// without a service (openstack-hypervisor-operator's offboarding deleted
	// it) there is nothing to delete.
	if handover || !registered {
		return false, nil
	}
	err := api.DeleteService(ctx, entry.ServiceID)
	if errors.Is(err, computeapi.ErrServiceHasInstances) {
		entry.Phase = novav1alpha1.NovaComputeNodeDraining
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("deleting the compute service of %s: %w", entry.Name, err)
	}
	r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonComputeServiceDeleted,
		"Deleted the compute service of node %s; Nova dropped its host mapping with it", entry.Name)
	return false, nil
}

// podListError marks a failed pod list, which is a cluster read rather than a
// Nova call.
type podListError struct{ error }

func (e podListError) Unwrap() error { return e.error }

// nodesRunningPods returns the nodes a pod of this pool that has not finished
// runs on. A non-empty node narrows the list to the pods on that node.
func (r *NovaComputeReconciler) nodesRunningPods(ctx context.Context, reader client.Reader,
	cr *novav1alpha1.NovaCompute, node string,
) (map[string]bool, error) {
	opts := []client.ListOption{client.InNamespace(cr.Namespace), client.MatchingLabels(novaComputeSelectorLabels(cr))}
	what := "the nova-compute pods"
	if node != "" {
		opts = append(opts, client.MatchingFields{"spec.nodeName": node})
		what += " on " + node
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, opts...); err != nil {
		return nil, podListError{fmt.Errorf("listing %s: %w", what, err)}
	}
	running := map[string]bool{}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			running[pod.Spec.NodeName] = true
		}
	}
	return running, nil
}

// phasePollInterval is how soon a node in phase needs Nova polled again.
func phasePollInterval(phase novav1alpha1.NovaComputeNodePhase) time.Duration {
	switch phase {
	case novav1alpha1.NovaComputeNodeReleasing:
		return RequeueComputeReleasePolling
	case novav1alpha1.NovaComputeNodeDraining, novav1alpha1.NovaComputeNodePending:
		return RequeueComputeDrainPolling
	case novav1alpha1.NovaComputeNodeActive, novav1alpha1.NovaComputeNodeConflict:
	}
	return RequeueComputeServicePolling
}

// servicesCondition derives ServicesReady from the walked nodes. unmapped are
// the Pending nodes the walk found registered but not mapped, the hosts the
// discovery Job was run for: each waits for its host mapping, and
// discoveryNote says what the Job is doing about it. Any other Pending node
// waits for its service to register.
func servicesCondition(cr *novav1alpha1.NovaCompute, nodes []novav1alpha1.NovaComputeNodeStatus,
	unmapped []string, discoveryNote string,
) metav1.Condition {
	var pending, down, leaving []string
	for _, entry := range nodes {
		switch entry.Phase {
		case novav1alpha1.NovaComputeNodePending:
			if !slices.Contains(unmapped, entry.Name) {
				pending = append(pending, entry.Name)
			}
		case novav1alpha1.NovaComputeNodeActive:
			if entry.ServiceState == serviceStateDown {
				down = append(down, entry.Name)
			}
		case novav1alpha1.NovaComputeNodeDraining, novav1alpha1.NovaComputeNodeReleasing:
			leaving = append(leaving, fmt.Sprintf("%s: %d instances", entry.Name, ptr.Deref(entry.Instances, 0)))
		case novav1alpha1.NovaComputeNodeConflict:
			// NodesReady reports a conflict.
		}
	}

	condition := metav1.Condition{Type: conditionTypeServicesReady, ObservedGeneration: cr.Generation}
	switch {
	case len(pending) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditionReasonWaitingForServices
		condition.Message = "Waiting for a compute service to register on " + strings.Join(pending, ", ")
	case len(unmapped) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditionReasonWaitingForHostMapping
		condition.Message = "Waiting for the host mapping of " + strings.Join(unmapped, ", ") + ": " + discoveryNote
	case len(down) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditionReasonServicesDown
		condition.Message = "Nova reports the compute service down on " + strings.Join(down, ", ")
	case len(leaving) > 0:
		condition.Status = metav1.ConditionTrue
		condition.Reason = conditionReasonDraining
		condition.Message = "Draining " + strings.Join(leaving, ", ")
	default:
		condition.Status = metav1.ConditionTrue
		condition.Reason = conditionReasonServicesUp
		condition.Message = "Every node's compute service is registered and up"
	}
	return condition
}

// computeAPIFailed sets conditionType False for a failed Keystone or Nova call
// and requeues without an error, so the retry keeps its pace.
func computeAPIFailed(cr *novav1alpha1.NovaCompute, conditionType string, err error) (ctrl.Result, error) {
	novaComputeSkeleton.MarkFailed(cr, conditionType, conditionReasonComputeAPIError, err)
	return ctrl.Result{RequeueAfter: RequeueComputeDrainPolling}, nil
}
