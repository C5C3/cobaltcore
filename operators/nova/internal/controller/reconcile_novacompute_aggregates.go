// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi"
)

// The reasons of the AggregatesReady condition. ComputeAPIError is shared with
// ServicesReady.
const (
	conditionReasonAggregatesEnsured     = "AggregatesEnsured"
	conditionReasonNodesWithoutZone      = "NodesWithoutZone"
	conditionReasonAggregateZoneMismatch = "AggregateZoneMismatch"
	conditionReasonWaitingForAggregate   = "WaitingForAggregate"
	conditionReasonComputeAPIError       = "ComputeAPIError"
	conditionReasonNovaComputeListError  = "NovaComputeListError"
)

// aggregateMarkerKey is the metadata key the satellite stamps on an aggregate
// it created, with "<namespace>/<nova>" as the value. Nova restricts metadata
// keys to [a-zA-Z0-9-_:. ], which is why the key has no slash. An aggregate
// without the marker was created by someone else and is never deleted.
const aggregateMarkerKey = "c5c3.io:nova"

// tenantFilterTestsAggregate is the aggregate openstack-hypervisor-operator puts
// every onboarding host into beside its zone's. It fails the onboarding when
// the aggregate is missing and creates none itself.
const tenantFilterTestsAggregate = "tenant_filter_tests"

// reconcileNovaComputeAggregates keeps the host aggregates the pool's nodes are
// onboarded into, and sets AggregatesReady.
//
// It ensures one aggregate per zone of this pool's Pending and Active nodes,
// named after the zone and carrying it as its availability zone, and the
// zone-less tenant_filter_tests while this pool is not being deleted. An
// aggregate it creates carries the marker; one that exists already is used as
// it is. A marked aggregate that no NovaCompute of the Nova needs any more is
// deleted once no host is left in it, unless it carries metadata the satellite
// did not write, such as a tenant isolation: deleting the aggregate would drop
// that metadata for good, so it is kept.
func (r *NovaComputeReconciler) reconcileNovaComputeAggregates(ctx context.Context, cr *novav1alpha1.NovaCompute,
	pass *novaComputePass,
) (ctrl.Result, error) {
	deleting := !cr.DeletionTimestamp.IsZero()
	marker := cr.Namespace + "/" + cr.Spec.NovaRef.Name

	var zones, withoutZone []string
	for _, entry := range cr.Status.Nodes {
		if entry.Phase != novav1alpha1.NovaComputeNodePending && entry.Phase != novav1alpha1.NovaComputeNodeActive {
			continue
		}
		if entry.Zone == "" {
			withoutZone = append(withoutZone, entry.Name)
			continue
		}
		if !slices.Contains(zones, entry.Zone) {
			zones = append(zones, entry.Zone)
		}
	}
	slices.Sort(zones)

	// The Nodes step hands the siblings over. A deleting pool that holds no node
	// skips that step (see teardownSteps) and lists them here: without them no
	// other pool would seem to need an aggregate.
	siblings := pass.siblings
	if siblings == nil {
		listed, err := novaComputesOfNova(ctx, r.Client, cr.Namespace, cr.Spec.NovaRef.Name)
		if err != nil {
			novaComputeSkeleton.MarkFailed(cr, conditionTypeAggregatesReady, conditionReasonNovaComputeListError, err)
			return ctrl.Result{}, err
		}
		siblings = listed
	}
	required := requiredAggregateNames(cr, siblings)

	api, err := pass.computeClient(ctx)
	if err != nil {
		return computeAPIFailed(cr, conditionTypeAggregatesReady, fmt.Errorf("authenticating: %w", err))
	}
	aggregates, err := api.ListAggregates(ctx)
	if err != nil {
		return computeAPIFailed(cr, conditionTypeAggregatesReady, fmt.Errorf("listing aggregates: %w", err))
	}

	ensure := make([]string, 0, len(zones)+1)
	ensure = append(ensure, zones...)
	if !deleting {
		ensure = append(ensure, tenantFilterTestsAggregate)
	}

	var mismatches []string
	for _, name := range ensure {
		zone := name
		if name == tenantFilterTestsAggregate {
			zone = ""
		}
		idx := slices.IndexFunc(aggregates, func(a computeapi.Aggregate) bool { return a.Name == name })
		if idx >= 0 {
			if got := aggregates[idx].AvailabilityZone; got != zone {
				mismatches = append(mismatches, fmt.Sprintf("aggregate %s has availability zone %q", name, got))
			}
			continue
		}

		created, err := api.CreateAggregate(ctx, name, zone)
		if errors.Is(err, computeapi.ErrAggregateExists) {
			// Another creator won the race between the list and the create.
			// The next pass lists it.
			conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
				Type:               conditionTypeAggregatesReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cr.Generation,
				Reason:             conditionReasonWaitingForAggregate,
				Message:            fmt.Sprintf("Aggregate %s was created concurrently; reading it back", name),
			})
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
		}
		if err != nil {
			return computeAPIFailed(cr, conditionTypeAggregatesReady, fmt.Errorf("creating aggregate %s: %w", name, err))
		}
		if err := api.SetAggregateMetadata(ctx, created.ID, map[string]string{aggregateMarkerKey: marker}); err != nil {
			// Unmarked, the aggregate would read as someone else's on the next
			// pass and never be deleted, so it goes again; the next pass creates
			// it anew. This is best effort: when the delete fails as well, or a
			// create that timed out was committed by Nova, the aggregate stays
			// unmarked, is used as it is and outlives the Nova's last pool.
			err = fmt.Errorf("marking aggregate %s: %w", name, err)
			if delErr := api.DeleteAggregate(ctx, created.ID); delErr != nil {
				err = errors.Join(err, fmt.Errorf("deleting the unmarked aggregate %s: %w", name, delErr))
			}
			return computeAPIFailed(cr, conditionTypeAggregatesReady, err)
		}
		r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonAggregateCreated,
			"Created host aggregate %s%s", name, zoneSuffix(zone))
	}

	for _, agg := range aggregates {
		if agg.Metadata[aggregateMarkerKey] != marker || required[agg.Name] || len(agg.Hosts) > 0 {
			continue
		}
		if foreign := foreignMetadataKeys(agg); len(foreign) > 0 {
			r.Recorder.Eventf(cr, corev1.EventTypeWarning, eventReasonAggregateKept,
				"Kept empty host aggregate %s: it carries metadata the operator did not set (%s)",
				agg.Name, strings.Join(foreign, ", "))
			continue
		}
		if err := api.DeleteAggregate(ctx, agg.ID); err != nil {
			return computeAPIFailed(cr, conditionTypeAggregatesReady, fmt.Errorf("deleting aggregate %s: %w", agg.Name, err))
		}
		r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonAggregateDeleted,
			"Deleted host aggregate %s: no NovaCompute of Nova %s needs it", agg.Name, cr.Spec.NovaRef.Name)
	}

	condition := metav1.Condition{Type: conditionTypeAggregatesReady, ObservedGeneration: cr.Generation}
	switch {
	case len(mismatches) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditionReasonAggregateZoneMismatch
		condition.Message = strings.Join(mismatches, "; ")
	case len(withoutZone) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditionReasonNodesWithoutZone
		condition.Message = fmt.Sprintf("Nodes without the %s label cannot be onboarded: %s",
			zoneLabel, strings.Join(withoutZone, ", "))
	default:
		condition.Status = metav1.ConditionTrue
		condition.Reason = conditionReasonAggregatesEnsured
		condition.Message = aggregatesEnsuredMessage(ensure)
		pass.aggregatesEnsured = true
	}
	conditions.SetCondition(&cr.Status.Conditions, condition)
	return ctrl.Result{}, nil
}

// requiredAggregateNames is the set of aggregates some NovaCompute of the Nova
// still needs: the zones in the status of every non-deleting one, this CR's
// taken from the Nodes step of this pass, and tenant_filter_tests while any of
// them is not being deleted. siblings are the NovaComputes of the same Nova in
// the namespace, on any cluster; this CR among them is ignored in favor of cr.
func requiredAggregateNames(cr *novav1alpha1.NovaCompute, siblings []novav1alpha1.NovaCompute) map[string]bool {
	required := map[string]bool{}
	add := func(nc *novav1alpha1.NovaCompute) {
		if !nc.DeletionTimestamp.IsZero() {
			return
		}
		required[tenantFilterTestsAggregate] = true
		for _, entry := range nc.Status.Nodes {
			if entry.Zone != "" {
				required[entry.Zone] = true
			}
		}
	}
	add(cr)
	for i := range siblings {
		if siblings[i].Name != cr.Name {
			add(&siblings[i])
		}
	}
	return required
}

// foreignMetadataKeys returns the sorted metadata keys of agg the satellite did
// not write: everything but its marker and the availability zone.
func foreignMetadataKeys(agg computeapi.Aggregate) []string {
	var keys []string
	for key := range agg.Metadata {
		if key != aggregateMarkerKey && key != "availability_zone" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

func zoneSuffix(zone string) string {
	if zone == "" {
		return ""
	}
	return " in availability zone " + zone
}

func aggregatesEnsuredMessage(ensured []string) string {
	if len(ensured) == 0 {
		return "No aggregate is required"
	}
	return "Host aggregates in place: " + strings.Join(ensured, ", ")
}
