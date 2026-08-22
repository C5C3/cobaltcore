// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// ensureCatalog projects the declared catalog block: one managed K-ORC Service
// for the catalog row plus one managed Endpoint per declared interface.
//
// It is MODE-INDEPENDENT, and deliberately unlike the ControlPlane's own catalog
// reconciler: that one imports rather than creates in External mode, because the
// identity row it owns already exists in a brownfield catalog. A KeystoneService
// registers a service the plane does NOT carry, which is the whole point of the
// CR, so it creates its row against a Managed and an External Keystone alike.
//
// The collision posture is the account block's, not the create-only opt-in the
// ControlPlane's External-mode entries use: K-ORC's service actuator matches an
// existing row on name and type, so a managed create silently adopts an exact
// match — and deleting the CR would then delete a catalog row the operator never
// created. A probe therefore decides exists/absent first, and a takeover needs
// explicit adopt consent.
func (r *KeystoneServiceReconciler) ensureCatalog(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	credRef, managedCredRef orcv1alpha1.CloudCredentialsReference,
) (ctrl.Result, error) {
	fail := keystoneServiceFail(ks, conditionTypeKeystoneServiceCatalogReady)
	catalog := ks.Spec.Catalog
	requeue := ctrl.Result{RequeueAfter: korcRequeueAfter}
	serviceName := keystoneServiceCatalogName(ks)

	status := &c5c3v1alpha1.KeystoneServiceCatalogStatus{}
	ks.Status.Catalog = status

	proceed, err := r.keystoneServiceCatalogCollisionGate(ctx, ks, cp, credRef, serviceName)
	if err != nil {
		fail(reasonKeystoneServiceCatalogError, fmt.Sprintf("probing for a pre-existing catalog entry: %v", err))
		return ctrl.Result{}, err
	}
	if !proceed {
		return requeue, nil
	}

	// The desired children are pure projections of the spec, so they are applied
	// via Server-Side Apply. They authenticate through the admin PASSWORD cloud,
	// not the spec's application credential: K-ORC must still reach Keystone to
	// DELETE the row at teardown, and the application credential is revoked by its
	// own finalizer while that delete is in flight.
	service := managedCatalogServiceChild(keystoneServiceCatalogServiceRef(ks), keystoneServiceChildNamespace(cp),
		catalog.ServiceType, serviceName, managedCredRef)
	if err := r.ensureKeystoneServiceChild(ctx, ks, service); err != nil {
		fail(reasonKeystoneServiceCatalogError, fmt.Sprintf("applying the catalog Service: %v", err))
		return ctrl.Result{}, err
	}
	if service.Status.ID != nil {
		status.ServiceID = *service.Status.ID
	}

	endpoints := make([]*orcv1alpha1.Endpoint, 0, len(catalog.Endpoints))
	for _, ep := range catalog.Endpoints {
		endpoint := managedCatalogEndpointChild(keystoneServiceCatalogEndpointRef(ks, ep.Interface),
			keystoneServiceChildNamespace(cp), string(ep.Interface), ep.URL, service.Name, managedCredRef)
		if err := r.ensureKeystoneServiceChild(ctx, ks, endpoint); err != nil {
			fail(reasonKeystoneServiceCatalogError, fmt.Sprintf("applying the %q catalog Endpoint: %v", ep.Interface, err))
			return ctrl.Result{}, err
		}
		endpoints = append(endpoints, endpoint)
		row := c5c3v1alpha1.KeystoneServiceEndpointStatus{Interface: ep.Interface}
		if endpoint.Status.ID != nil {
			row.ID = *endpoint.Status.ID
		}
		status.Endpoints = append(status.Endpoints, row)
	}

	// The Service's terminal error is reported before its Endpoints', so the ROOT
	// stuck dependency surfaces rather than an Endpoint merely blocked on it.
	if termErr := orcv1alpha1.GetTerminalError(service); termErr != nil {
		fail(conditionReasonCatalogFailed, fmt.Sprintf(
			"K-ORC reported a terminal error registering the catalog Service %q: %v", service.Name, termErr,
		))
		return requeue, nil
	}
	for _, endpoint := range endpoints {
		if termErr := orcv1alpha1.GetTerminalError(endpoint); termErr != nil {
			fail(conditionReasonCatalogFailed, fmt.Sprintf(
				"K-ORC reported a terminal error registering the catalog Endpoint %q: %v", endpoint.Name, termErr,
			))
			return requeue, nil
		}
	}

	// Registering the CRs only instructs K-ORC to create the rows; it does not
	// mean they exist in Keystone. Gate on every child reporting Available for its
	// current generation, or a failing registration would report Ready while the
	// catalog stays empty.
	if !korcAvailableUpToDate(service) {
		r.keystoneServiceWaitOrClassify(ks, cp, conditionTypeKeystoneServiceCatalogReady,
			conditionReasonWaitingForCatalog,
			fmt.Sprintf("the catalog Service %q is registered but not yet Available", service.Name),
			service)
		return requeue, nil
	}
	for _, endpoint := range endpoints {
		if !korcAvailableUpToDate(endpoint) {
			r.keystoneServiceWaitOrClassify(ks, cp, conditionTypeKeystoneServiceCatalogReady,
				conditionReasonWaitingForCatalog,
				fmt.Sprintf("the catalog Endpoint %q is registered but not yet Available", endpoint.Name),
				endpoint)
			return requeue, nil
		}
	}

	keystoneServiceSetTrue(ks, conditionTypeKeystoneServiceCatalogReady, reasonKeystoneServiceCatalogRegistered,
		fmt.Sprintf("catalog entry %q of type %q is registered with %d endpoint(s)",
			serviceName, catalog.ServiceType, len(catalog.Endpoints)))
	return ctrl.Result{}, nil
}

// keystoneServiceCatalogCollisionGate implements decision D6's fail-loudly
// default for a pre-existing catalog row. It returns proceed=true when the
// managed Service may be created: the operator already owns it, adoption was
// requested, or a probe confirmed no row of this type and name exists.
//
// The probe covers the SERVICE row only. An endpoint is scoped to its service, so
// once the service row is ours the endpoints under it are ours too; probing them
// separately would only report collisions with rows this registration is about to
// own anyway.
func (r *KeystoneServiceReconciler) keystoneServiceCatalogCollisionGate(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	credRef orcv1alpha1.CloudCredentialsReference, serviceName string,
) (bool, error) {
	// The filter carries the type AND the effective name, so the probe answers the
	// question K-ORC's own adoption asks — it matches a row on exactly that pair.
	probe := unmanagedServiceImport(keystoneServiceCatalogServiceProbeRef(ks), keystoneServiceChildNamespace(cp),
		ks.Spec.Catalog.ServiceType, serviceName, credRef)
	proceed, verdict, err := managedChildProbeGate(ctx, r.Client, managedChildProbeInput{
		kind:             "Service",
		managed:          &orcv1alpha1.Service{},
		managedName:      keystoneServiceCatalogServiceRef(ks),
		namespace:        keystoneServiceChildNamespace(cp),
		adopt:            ks.Spec.Catalog.Adopt,
		probe:            probe,
		dropProbeOnOwned: true,
		ensure:           r.keystoneServiceEnsure(ks),
	})
	if err != nil || proceed {
		return proceed, err
	}
	switch verdict {
	case probeResolved:
		keystoneServiceFail(ks, conditionTypeKeystoneServiceCatalogReady)(reasonKeystoneServiceCatalogCollision, fmt.Sprintf(
			"a catalog entry of type %q named %q already exists in Keystone; the operator will not silently take over "+
				"a row it did not create — set catalog.adopt=true to take it over (and own its deletion), or change "+
				"catalog.serviceName",
			ks.Spec.Catalog.ServiceType, serviceName,
		))
	case probePending:
		r.keystoneServiceWaitOrClassify(ks, cp, conditionTypeKeystoneServiceCatalogReady,
			reasonProbingForCollision,
			fmt.Sprintf("probing whether a catalog entry of type %q named %q already exists in Keystone",
				ks.Spec.Catalog.ServiceType, serviceName),
			probe)
	case probeAbsent:
		// Unreachable: an absent probe proceeds.
	}
	return false, nil
}
