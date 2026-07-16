// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/apply"
	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// CatalogReady reasons shared by BOTH branches of reconcileCatalog. The
// managed-only reasons stay inline at their single call site; these two are
// stamped from reconcile_catalog_external.go as well, so a literal in each file
// would be a drift hazard.
const (
	// conditionReasonCatalogFailed reports a TERMINAL K-ORC failure on a catalog
	// child CR: an unrecoverable/invalid-configuration error K-ORC will not retry.
	conditionReasonCatalogFailed = "CatalogFailed"

	// conditionReasonWaitingForCatalog reports that the catalog children are
	// reconciled but not yet Available for their current generation.
	conditionReasonWaitingForCatalog = "WaitingForCatalog"
)

// reconcileCatalog drives the CatalogReady condition. What it does depends on
// the Keystone mode, and the two postures are opposites:
//
//   - Managed — the ControlPlane OWNS the catalog. It registers the OpenStack
//     service catalog entries for Keystone (an identity Service plus its public
//     Endpoint) as managed K-ORC CRs, which K-ORC creates in Keystone.
//   - External — the catalog belongs to the pre-existing installation, so the
//     ControlPlane IMPORTS it instead (reconcileCatalogExternal). Creating
//     entries against a populated catalog would duplicate rows Keystone does not
//     deduplicate, so it never happens by default.
//
// Both branches are GATED on AdminCredentialReady: the admin credential must be
// available before K-ORC can talk to Keystone at all. Child CRs are
// create-or-updated idempotently. K-ORC is a hard CRD dependency (see
// reconcileKORC), so a missing Service/Endpoint CRD never reaches here — no-match
// errors fall through to the generic error returns below (#476).
func (r *ControlPlaneReconciler) reconcileCatalog(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fail := conditionFailer(cp, conditionTypeCatalogReady)

	// Gate on AdminCredentialReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeAdminCredentialReady) {
		logger.Info("AdminCredential not ready, deferring catalog registration")
		fail("WaitingForAdminCredential", "AdminCredentialReady is not True; catalog registration deferred")
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	secretName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	// Fall back to the conventional name when SecretName is empty, matching
	// reconcileAdminCredential and ensureKORCCloudsYAMLExternalSecret so a
	// webhook-bypass CR resolves to the same clouds.yaml Secret name everywhere
	// (#476). Without this the catalog Service/Endpoint would reference an empty
	// CloudCredentialsRef.SecretName.
	if secretName == "" {
		secretName = korcCloudsYamlSecretName
	}
	cloudName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName
	credRef := orcv1alpha1.CloudCredentialsReference{SecretName: secretName, CloudName: cloudName}

	// Fork on the mode discriminator. Everything above is mode-agnostic (the gate
	// and the admin credential K-ORC authenticates with); everything below owns
	// the catalog and therefore only runs in Managed mode.
	if cp.IsExternalKeystone() {
		return r.reconcileCatalogExternal(ctx, cp, credRef)
	}

	// Register every managed catalog row's Service and its Endpoints as owned K-ORC
	// CRs, then gate CatalogReady on their readiness. The desired spec of each child
	// is a pure projection of cp.Spec, so it is applied via Server-Side Apply under
	// the shared field manager rather than read-modify-write.
	//
	// The catalog is driven from managedCatalogRows so a second service is a table
	// row, not a copied literal. Today that is exactly the identity (Keystone) row:
	// an identity-type Service named "keystone" and a single public Endpoint whose
	// URL defaults to the in-cluster Keystone Service URL and rises to the external
	// publicEndpoint when Keystone is exposed via a Gateway (see keystoneCatalogURL).
	type appliedCatalogRow struct {
		row       managedCatalogServiceRow
		service   *orcv1alpha1.Service
		endpoints []*orcv1alpha1.Endpoint
	}
	rows := managedCatalogRows(cp)
	applied := make([]appliedCatalogRow, 0, len(rows))
	for _, row := range rows {
		service := managedCatalogService(cp, credRef, row)
		if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, service, apply.FieldManager); err != nil {
			fail("ServiceError", fmt.Sprintf("applying %s Service: %v", row.serviceType, err))
			return ctrl.Result{}, err
		}
		endpoints := make([]*orcv1alpha1.Endpoint, 0, len(row.endpoints))
		for _, ep := range row.endpoints {
			endpoint := managedCatalogEndpoint(cp, credRef, row, ep)
			if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, endpoint, apply.FieldManager); err != nil {
				fail("EndpointError", fmt.Sprintf("applying %s Endpoint: %v", row.serviceType, err))
				return ctrl.Result{}, err
			}
			endpoints = append(endpoints, endpoint)
		}
		applied = append(applied, appliedCatalogRow{row: row, service: service, endpoints: endpoints})
	}

	// Gate CatalogReady on EVERY child CR reporting Available, and surface a TERMINAL
	// K-ORC failure distinctly: registering the Service/Endpoint CRs only instructs
	// K-ORC to create the catalog entries — it does not mean the entries exist in
	// Keystone. The documented failure class (wrong clouds.yaml endpoint, K-ORC
	// swallowing list errors, an import hung on "created externally") otherwise lets
	// CatalogReady (and the aggregate Ready) report True while the catalog is empty.
	//
	// A row's Service terminal error is reported before its Endpoints' so the ROOT
	// stuck dependency surfaces rather than an Endpoint merely blocked on it; every
	// row's terminal errors precede the availability waits.
	for _, ar := range applied {
		if termErr := orcv1alpha1.GetTerminalError(ar.service); termErr != nil {
			return r.catalogTerminalError(cp, ar.row.serviceType+" Service", ar.service.Name, termErr), nil
		}
		for _, endpoint := range ar.endpoints {
			if termErr := orcv1alpha1.GetTerminalError(endpoint); termErr != nil {
				return r.catalogTerminalError(cp, ar.row.serviceType+" Endpoint", endpoint.Name, termErr), nil
			}
		}
	}
	for _, ar := range applied {
		ready := korcAvailableUpToDate(ar.service)
		for _, endpoint := range ar.endpoints {
			ready = ready && korcAvailableUpToDate(endpoint)
		}
		if !ready {
			logger.Info("catalog Service/Endpoint not yet Available, requeuing", "serviceType", ar.row.serviceType)
			fail(conditionReasonWaitingForCatalog, fmt.Sprintf(
				"the %s catalog Service and Endpoint CRs are registered but not yet Available", ar.row.serviceType,
			))
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCatalogReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "CatalogRegistered",
		Message: fmt.Sprintf(
			"%d catalog entry/entries registered as K-ORC CRs and Available", len(rows),
		),
	})
	return ctrl.Result{}, nil
}

// managedCatalogEndpointRow is one Endpoint of a managed catalog service row: an
// interface, the deterministic name of the K-ORC Endpoint CR that registers it,
// and the URL to advertise.
type managedCatalogEndpointRow struct {
	iface  string
	crName string
	url    string
}

// managedCatalogServiceRow is one entry in the managed service catalog: an
// OpenStack service (type and name), the deterministic name of the K-ORC Service
// CR that registers it, and the Endpoint rows registered under it.
//
// NAMING CONVENTION for FUTURE rows: a service of type {type} gets a Service CR
// named "{cp}-{type}-service" and, per interface, an Endpoint CR named
// "{cp}-{type}-endpoint-{iface}". The identity row is the ONE exception — it
// keeps its legacy CR names ("{cp}-identity-service" / "{cp}-identity-endpoint",
// via keystoneServiceName / keystoneEndpointName) because renaming a live CR
// would delete and re-add its catalog row on upgrade. Every new row follows the
// generic convention from the start, so only identity carries the legacy shape.
//
// endpoints is a list so a row can register several interfaces, but the identity
// row exercises only the default posture: a single public entry whose URL falls
// back to the in-cluster Keystone Service URL. Per-interface endpoint lists are
// supported by the type and not yet exercised.
type managedCatalogServiceRow struct {
	serviceType string
	serviceName string
	crName      string
	endpoints   []managedCatalogEndpointRow
}

// managedCatalogRows returns the managed service-catalog rows the ControlPlane
// registers via K-ORC. Today it is exactly one row — the identity (Keystone)
// service with a single public Endpoint — keyed on the legacy CR names so the
// live catalog rows are never renamed (see managedCatalogServiceRow). A future
// second service (e.g. type "image", name "glance") is added here as another
// row, not by copying the builder call sites. It is mode-independent: reconcileDelete
// enumerates the same rows to tear down the identity CRs in both keystone modes.
func managedCatalogRows(cp *c5c3v1alpha1.ControlPlane) []managedCatalogServiceRow {
	return []managedCatalogServiceRow{{
		serviceType: "identity",
		serviceName: "keystone",
		crName:      keystoneServiceName(cp),
		endpoints: []managedCatalogEndpointRow{{
			iface:  "public",
			crName: keystoneEndpointName(cp),
			url:    keystoneCatalogURL(cp),
		}},
	}}
}

// managedCatalogService builds the MANAGED K-ORC Service CR for one catalog row.
// The desired spec is a pure projection of cp.Spec, so it is applied via
// Server-Side Apply under the shared field manager rather than read-modify-write;
// the owner reference is stamped by apply.EnsureObject at apply time.
func managedCatalogService(
	cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference, row managedCatalogServiceRow,
) *orcv1alpha1.Service {
	return &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			// The K-ORC CRs are ControlPlane-scoped, not service-scoped: they stay in
			// the ControlPlane's namespace, owner-referenced, however the services are
			// placed. Only the URL they register follows the service.
			Name:      row.crName,
			Namespace: childNamespace(cp),
		},
		Spec: orcv1alpha1.ServiceSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: credRef,
			Resource: &orcv1alpha1.ServiceResourceSpec{
				Type:    row.serviceType,
				Name:    ptr.To(orcv1alpha1.OpenStackName(row.serviceName)),
				Enabled: ptr.To(true),
			},
		},
	}
}

// managedCatalogEndpoint builds the MANAGED K-ORC Endpoint CR for one endpoint of
// a catalog row. Same SSA projection as managedCatalogService; its Interface comes
// from the endpoint row (today always "public") and its ServiceRef points at the
// row's Service CR.
func managedCatalogEndpoint(
	cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference,
	row managedCatalogServiceRow, ep managedCatalogEndpointRow,
) *orcv1alpha1.Endpoint {
	return &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ep.crName,
			Namespace: childNamespace(cp),
		},
		Spec: orcv1alpha1.EndpointSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: credRef,
			Resource: &orcv1alpha1.EndpointResourceSpec{
				Interface:  ep.iface,
				URL:        ep.url,
				ServiceRef: orcv1alpha1.KubernetesNameRef(row.crName),
				Enabled:    ptr.To(true),
			},
		},
	}
}

// catalogTerminalError records a terminal K-ORC catalog failure: it sets
// CatalogReady=False/CatalogFailed naming the failing child CR. It requeues so a
// fixed configuration (e.g. a corrected clouds.yaml) is re-evaluated rather than
// leaving the catalog wedged.
func (r *ControlPlaneReconciler) catalogTerminalError(cp *c5c3v1alpha1.ControlPlane, kind, name string, termErr error) ctrl.Result {
	conditionFailer(cp, conditionTypeCatalogReady)(conditionReasonCatalogFailed,
		fmt.Sprintf("K-ORC reported a terminal error registering the %s %q: %v", kind, name, termErr))
	return ctrl.Result{RequeueAfter: korcRequeueAfter}
}

// korcAvailableUpToDate reports whether a K-ORC resource is Available for its
// CURRENT generation: the Available condition exists, is True, AND its
// ObservedGeneration matches the object's generation. Unlike orcv1alpha1.IsAvailable
// — which is generation-blind — it refuses to treat a stale Available condition
// (left over from before a spec edit, e.g. a changed publicEndpoint/region that
// moved keystoneCatalogURL, that K-ORC has not yet re-reconciled) as a live result.
// This mirrors the generation gate orcv1alpha1.GetTerminalError already applies via
// its Progressing check, so a gate cannot flip True advertising a value K-ORC has
// not yet applied.
func korcAvailableUpToDate(obj orcv1alpha1.ObjectWithConditions) bool {
	for _, c := range obj.GetConditions() {
		if c.Type == orcv1alpha1.ConditionAvailable {
			return c.Status == metav1.ConditionTrue && c.ObservedGeneration == obj.GetGeneration()
		}
	}
	return false
}

// keystoneServiceName / keystoneEndpointName return the deterministic names of
// the owned K-ORC Service/Endpoint CRs registering the identity catalog entry.
func keystoneServiceName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-identity-service"
}

func keystoneEndpointName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-identity-endpoint"
}

// keystoneEndpointURL derives the in-cluster Keystone identity URL from the
// projected Keystone Service — keystoneName(cp) = "{cp.Name}-keystone" — in the
// namespace the Keystone service is placed in (see DECISION on Endpoint URL in
// reconcileCatalog). It must NOT hard-code "keystone": the keystone-operator
// names the Service after the projected Keystone CR, so a fixed name would not
// resolve. This is the URL K-ORC authenticates against (the seeded clouds.yaml
// auth_url): K-ORC runs in-cluster, so it must always use the Service DNS, never
// the external endpoint.
//
// The namespace-qualified Service DNS is the WHOLE cross-namespace
// service-discovery mechanism: a Keystone placed in a namespace of its own is
// still reachable from the ControlPlane's namespace (K-ORC) and from the
// dashboard's (spec.keystoneEndpoint), because ClusterIP Service DNS resolves
// across namespaces unchanged. What does NOT come for free is reachability —
// namespaces are where NetworkPolicy is attached, so a default-deny namespace
// must explicitly allow this flow.
func keystoneEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return managedServiceURL(keystoneName(cp), cp.KeystoneNamespace(), 5000, "/v3")
}

// managedServiceURL renders the in-cluster URL of a projected Service by
// convention: http://{name}.{namespace}.svc:{port}{path}. Deriving the address
// top-down from the naming convention rather than reading a producing CR's
// status is the cross-service endpoint contract (see internal/common/naming),
// so a second service (e.g. glance-api on 9292) registers its catalog URL the
// same way without a status watch.
func managedServiceURL(name, namespace string, port int32, path string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d%s", name, namespace, port, path)
}

// keystoneCatalogURL returns the URL registered for the K-ORC identity catalog
// Endpoint. It prefers the externally routable publicEndpoint (keystonePublicEndpoint)
// so the catalog matches what Keystone's own bootstrap advertises when exposed
// via a Gateway; absent external exposure it falls back to the in-cluster
// Service URL (keystoneEndpointURL).
func keystoneCatalogURL(cp *c5c3v1alpha1.ControlPlane) string {
	if pe := keystonePublicEndpoint(cp.Spec.Services.Keystone); pe != "" {
		return pe
	}
	return keystoneEndpointURL(cp)
}
