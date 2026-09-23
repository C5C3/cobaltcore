// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// ensureCatalog projects the declared catalog block: one managed K-ORC Service
// for the catalog row, an unmanaged import of the ControlPlane's Keystone region,
// and one managed Endpoint per declared interface, registered in that region.
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

	// Every endpoint is registered in the region K-ORC's clouds.yaml names. A row
	// without a region is invisible to a client that sets region_name, as every
	// nova client section and the neutron notifier do: keystoneauth answers
	// EndpointNotFound. The region is imported rather than the ControlPlane's own
	// Region CR referenced: K-ORC holds a Region CR while an Endpoint names it, so
	// a shared one would tie the plane's teardown to every registration, and an
	// External plane has none.
	region := unmanagedRegionImport(keystoneServiceCatalogRegionRef(ks), keystoneServiceChildNamespace(cp),
		korcRegion(cp), credRef)
	if err := r.ensureKeystoneServiceChild(ctx, ks, region); err != nil {
		fail(reasonKeystoneServiceCatalogError, fmt.Sprintf("applying the catalog Region import: %v", err))
		return ctrl.Result{}, err
	}

	endpoints := make([]*orcv1alpha1.Endpoint, 0, len(catalog.Endpoints))
	for _, ep := range catalog.Endpoints {
		endpoint := managedCatalogEndpointChild(keystoneServiceCatalogEndpointRef(ks, ep.Interface),
			keystoneServiceChildNamespace(cp), string(ep.Interface), ep.URL, service.Name, region.Name, managedCredRef)
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

	// A latched transport error is handed back to K-ORC first, so it retries the
	// create it gave up on; korc_unlatch.go states the policy. Every latch this
	// leaves in place still fails loud below.
	objs := make([]orcv1alpha1.ObjectWithConditions, 0, 2+len(endpoints))
	objs = append(objs, service, region)
	for _, endpoint := range endpoints {
		objs = append(objs, endpoint)
	}
	if err := unlatchKORCTransportErrors(ctx, r.Client, objs...); err != nil {
		fail(conditionReasonTransportErrorRetryFailed, err.Error())
		return ctrl.Result{}, err
	}

	// The Service's and the Region's terminal errors are reported before the
	// Endpoints', so the ROOT stuck dependency surfaces rather than an Endpoint
	// merely blocked on it.
	if termErr := orcv1alpha1.GetTerminalError(service); termErr != nil {
		fail(conditionReasonCatalogFailed, fmt.Sprintf(
			"K-ORC reported a terminal error registering the catalog Service %q: %v", service.Name, termErr,
		))
		return requeue, nil
	}
	if termErr := orcv1alpha1.GetTerminalError(region); termErr != nil {
		fail(conditionReasonCatalogFailed, fmt.Sprintf(
			"K-ORC reported a terminal error importing the catalog Region %q: %v", region.Name, termErr,
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
	if !korcAvailableUpToDate(region) {
		r.keystoneServiceWaitOrClassify(ks, cp, conditionTypeKeystoneServiceCatalogReady,
			conditionReasonWaitingForCatalog,
			fmt.Sprintf("the Keystone region %q the catalog endpoints are registered in is not resolved yet (Region %q)",
				korcRegion(cp), region.Name),
			region)
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

	retiring, err := r.retireLegacyCatalogEndpoints(ctx, ks, keystoneServiceChildNamespace(cp))
	if err != nil {
		fail(reasonKeystoneServiceCatalogError, fmt.Sprintf("retiring the region-less catalog Endpoints: %v", err))
		return ctrl.Result{}, err
	}
	if len(retiring) > 0 {
		fail(conditionReasonWaitingForCatalog, fmt.Sprintf(
			"the region-less catalog Endpoint(s) %s are being removed now that their replacements in region %q are Available",
			strings.Join(retiring, ", "), korcRegion(cp),
		))
		return requeue, nil
	}

	keystoneServiceSetTrue(ks, conditionTypeKeystoneServiceCatalogReady, reasonKeystoneServiceCatalogRegistered,
		fmt.Sprintf("catalog entry %q of type %q is registered with %d endpoint(s) in region %q",
			serviceName, catalog.ServiceType, len(catalog.Endpoints), korcRegion(cp)))
	return ctrl.Result{}, nil
}

// retireLegacyCatalogEndpoints deletes the region-less Endpoint CR an earlier
// version registered for each declared interface, and returns the names still
// present. The caller runs it only once every regioned replacement is Available,
// so the catalog carries each interface throughout: for a moment it carries two
// rows with one URL, which a client resolves the same way whichever it picks.
//
// Deleting the CR is what removes the Keystone row, through K-ORC's finalizer, so
// a name stays listed until that finalizer is gone. An interface the spec no
// longer declares is not this function's: the per-pass sweep removes its
// legacy row together with the regioned one.
func (r *KeystoneServiceReconciler) retireLegacyCatalogEndpoints(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, namespace string,
) ([]string, error) {
	var retiring []string
	for _, ep := range ks.Spec.Catalog.Endpoints {
		name := keystoneServiceLegacyCatalogEndpointRef(ks, ep.Interface)
		legacy := &orcv1alpha1.Endpoint{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, legacy); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("reading Endpoint %q: %w", name, err)
		}
		if !ownsKeystoneServiceChild(ks, legacy) {
			continue
		}
		if legacy.DeletionTimestamp == nil {
			if err := client.IgnoreNotFound(r.Delete(ctx, legacy)); err != nil {
				return nil, fmt.Errorf("deleting Endpoint %q: %w", name, err)
			}
		}
		retiring = append(retiring, name)
	}
	return retiring, nil
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
