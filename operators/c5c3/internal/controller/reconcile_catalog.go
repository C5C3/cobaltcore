// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
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
//     Endpoint) as managed K-ORC CRs, which K-ORC creates in Keystone. It also
//     adopts the region the keystone bootstrap inserted as a managed Region CR
//     ({cp.Name}-region) carrying spec.regionDescription: the CR adopts the region
//     first and describes it on a later pass, and it detaches on delete instead of
//     removing the Keystone row (see managedCatalogRegion).
//   - External — the catalog belongs to the pre-existing installation, so the
//     ControlPlane IMPORTS it instead (reconcileCatalogExternal). Creating
//     entries against a populated catalog would duplicate rows Keystone does not
//     deduplicate, so it never happens by default.
//
// Both branches are GATED on AdminCredentialReady: the admin credential must be
// available before K-ORC can talk to Keystone at all. Child CRs are
// create-or-updated idempotently, and apply.EnsureObject decodes the apply
// response back into the object it was handed, so each child carries live status
// after its own apply. That is what the terminal-error and availability checks
// below read, without a second Get. K-ORC is a hard CRD dependency (see
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
	// The catalog is driven from managedCatalogRows, whose one row is the identity
	// (Keystone) service: an identity-type Service named "keystone" and a single
	// public Endpoint whose URL defaults to the in-cluster Keystone Service URL and
	// rises to the external publicEndpoint when Keystone is exposed via a Gateway
	// (see keystoneCatalogURL). The built-in services register their own catalog
	// entries through the KeystoneService children the ControlPlane projects for
	// them, so none of them adds a row here.
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

	// Adopt the Keystone region the keystone bootstrap inserted as an owned managed
	// Region CR, in two phases: the CR is applied WITHOUT a description until K-ORC
	// reports an id for it, and only a later pass adds spec.regionDescription (see
	// managedCatalogRegion for why that order is fixed).
	//
	// The live CR is read ONCE and exactly one apply follows, so a pass either adopts
	// or describes. Applying twice in one pass, first without the description to adopt
	// and then with it, would drop and re-add the field on every pass, bump the CR's
	// generation on every pass, and leave K-ORC clearing and re-setting the Keystone
	// description forever. The catalog rows are applied before the Region so a
	// Service or Endpoint failure keeps reporting under its own reason.
	live := &orcv1alpha1.Region{}
	adopted := false
	switch err := r.Get(ctx, client.ObjectKey{Name: keystoneRegionName(cp), Namespace: childNamespace(cp)}, live); {
	case err == nil:
		adopted = live.Status.ID != nil && *live.Status.ID != ""
	case apierrors.IsNotFound(err):
		// The apply below creates the CR; nothing is adopted yet. This is also the
		// last pass before K-ORC first writes to the Keystone region, so it is where
		// the destructive shape has to be recorded: K-ORC reads an absent
		// spec.resource.description as the empty string, so an empty
		// spec.regionDescription CLEARS a description an admin set on that region by
		// hand. An operator upgrade adopts the region without anyone editing the
		// ControlPlane, so admission never runs on that path and this event is the
		// only notice the cluster carries.
		//
		// CatalogReady gates it because only an upgrade can lose anything here. A
		// fresh install creates this CR on the same pass that first registers the
		// catalog, and CatalogReady only turns True once THIS Region reports
		// Available — so True while the CR is absent means the catalog was
		// registered by a version that had no Region CR, which is exactly the
		// population whose region may carry a hand-set description. On a fresh
		// install the bootstrap row has no description, nothing is cleared, and a
		// Warning on every install would be noise that buries the real one. The gate
		// also holds the emitted-once promise: a failed Region apply below stamps
		// CatalogReady False (RegionError), so the retry pass stays silent too.
		if cp.Spec.RegionDescription == "" && r.Recorder != nil &&
			conditions.AllTrue(cp.Status.Conditions, conditionTypeCatalogReady) {
			r.Recorder.Event(cp, "Warning", "RegionDescriptionCleared", fmt.Sprintf(
				"adopting Keystone region %q as the managed Region %q with spec.regionDescription empty: "+
					"K-ORC asserts an empty description, clearing any description set on that region by "+
					"hand. Set spec.regionDescription to keep one.",
				korcRegion(cp), keystoneRegionName(cp)))
		}
	default:
		fail("RegionError", fmt.Sprintf("reading Region %q: %v", keystoneRegionName(cp), err))
		return ctrl.Result{}, err
	}
	region := managedCatalogRegion(cp, credRef, adopted)
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, region, apply.FieldManager); err != nil {
		fail("RegionError", fmt.Sprintf("applying Region %q: %v", region.Name, err))
		return ctrl.Result{}, err
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
	if termErr := orcv1alpha1.GetTerminalError(region); termErr != nil {
		return r.catalogTerminalError(cp, "Region", region.Name, termErr), nil
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
	// The description apply bumps the Region CR's generation, so this generation-aware
	// gate also holds the second phase: CatalogReady stays False until K-ORC has
	// pushed the description and reported Available for that generation.
	if !korcAvailableUpToDate(region) {
		logger.Info("catalog Region not yet Available, requeuing", "region", region.Name)
		fail(conditionReasonWaitingForCatalog, fmt.Sprintf(
			"the Region CR %q for region %q is registered but not yet Available", region.Name, korcRegion(cp),
		))
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCatalogReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "CatalogRegistered",
		Message: fmt.Sprintf(
			"region %q adopted and %d catalog entry/entries registered as K-ORC CRs and Available",
			korcRegion(cp), len(rows),
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
// The identity row keeps its established CR names ("{cp}-identity-service" /
// "{cp}-identity-endpoint", via keystoneServiceName / keystoneEndpointName):
// renaming a live CR would delete and re-add its catalog row on upgrade.
//
// endpoints is a list so a row can register several interfaces. The identity row,
// the only row today, exercises the default posture alone: a single public entry
// whose URL falls back to the in-cluster Keystone Service URL.
type managedCatalogServiceRow struct {
	serviceType string
	serviceName string
	crName      string
	endpoints   []managedCatalogEndpointRow
}

// managedCatalogRows returns the managed service-catalog rows the ControlPlane
// registers via K-ORC: the identity (Keystone) service with a single public
// Endpoint, keyed on the legacy CR names so the live catalog rows are never
// renamed (see managedCatalogServiceRow), and nothing else. A declared image,
// placement or key-manager service adds no row here — each built-in carries its
// catalog entry on the KeystoneService child the ControlPlane projects for it, and
// the KeystoneService controller registers that entry under the child's own names.
// It is mode-independent: reconcileDelete enumerates the same row to tear down the
// CRs in both keystone modes.
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

// internalCatalogURL picks the URL a catalog row advertises on its INTERNAL
// interface: inCluster for a co-located service, public for one placed on a
// target cluster.
//
// A co-located service is reached by every consumer over its in-cluster Service
// DNS name, the cheap path that never leaves the cluster. That name resolves
// nowhere else, so once the service is placed the same entry would hand every
// consumer outside its cluster — K-ORC on the management cluster among them — an
// address it cannot connect to. Admission requires a placed catalog service to
// carry a publicEndpoint or a gateway, so the public URL is never empty here.
//
// The identity row needs no such choice: it registers a public interface only,
// and keystoneCatalogURL already prefers the public URL over the in-cluster one.
func internalCatalogURL(ref *commonv1.TargetClusterRefSpec, inCluster, public string) string {
	if ref != nil {
		return public
	}
	return inCluster
}

// managedCatalogService builds the MANAGED K-ORC Service CR for one catalog row.
// The desired spec is a pure projection of cp.Spec, so it is applied via
// Server-Side Apply under the shared field manager rather than read-modify-write;
// the owner reference is stamped by apply.EnsureObject at apply time.
func managedCatalogService(
	cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference, row managedCatalogServiceRow,
) *orcv1alpha1.Service {
	// The K-ORC CRs are ControlPlane-scoped, not service-scoped: they stay in the
	// ControlPlane's namespace, owner-referenced, however the services are placed.
	// Only the URL they register follows the service.
	return managedCatalogServiceChild(row.crName, childNamespace(cp), row.serviceType, row.serviceName, credRef)
}

// managedCatalogEndpoint builds the MANAGED K-ORC Endpoint CR for one endpoint of
// a catalog row. Same SSA projection as managedCatalogService; its Interface comes
// from the endpoint row (today always "public") and its ServiceRef points at the
// row's Service CR.
func managedCatalogEndpoint(
	cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference,
	row managedCatalogServiceRow, ep managedCatalogEndpointRow,
) *orcv1alpha1.Endpoint {
	return managedCatalogEndpointChild(ep.crName, childNamespace(cp), ep.iface, ep.url, row.crName, credRef)
}

// managedCatalogRegion builds the MANAGED K-ORC Region CR adopting the Keystone
// region named by spec.region (korcRegion), the region the keystone bootstrap
// inserted. Same SSA projection as managedCatalogService; two properties of the
// Keystone region API decide its shape.
//
// ADOPT FIRST, DESCRIBE SECOND: K-ORC's adoption filter matches on the description
// as well whenever the spec carries one, and the bootstrap's region row has an empty
// description. A CR asking for a description on its FIRST pass therefore adopts
// nothing, falls through to create, and Keystone answers 409 Conflict, which K-ORC
// classifies as terminal. The description is set only once the region is adopted
// (adopted, i.e. the live CR reports a status.id). An empty spec.regionDescription
// leaves the field nil rather than pointing at the empty string, which K-ORC's
// MinLength of 1 rejects.
//
// DETACH ON DELETE: Keystone answers 403 for a region that still has endpoints, and
// the identity endpoints registered above reference this very region. With
// onDelete: detach a Delete of the CR removes the CR and leaves the Keystone row.
func managedCatalogRegion(
	cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference, adopted bool,
) *orcv1alpha1.Region {
	resource := &orcv1alpha1.RegionResourceSpec{Name: ptr.To(orcv1alpha1.OpenStackName(korcRegion(cp)))}
	if adopted && cp.Spec.RegionDescription != "" {
		resource.Description = ptr.To(cp.Spec.RegionDescription)
	}
	return &orcv1alpha1.Region{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneRegionName(cp), Namespace: childNamespace(cp)},
		Spec: orcv1alpha1.RegionSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			ManagedOptions:      &orcv1alpha1.ManagedOptions{OnDelete: orcv1alpha1.OnDeleteDetach},
			CloudCredentialsRef: credRef,
			Resource:            resource,
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

// keystoneRegionName returns the deterministic name of the owned K-ORC Region CR
// adopting the region the keystone bootstrap inserted.
func keystoneRegionName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-region"
}

// keystoneEndpointURL derives the in-cluster Keystone identity URL from the
// projected Keystone Service — keystoneName(cp) = "{cp.Name}-keystone" — in the
// namespace the Keystone service is placed in (see DECISION on Endpoint URL in
// reconcileCatalog). It must NOT hard-code "keystone": the keystone-operator
// names the Service after the projected Keystone CR, so a fixed name would not
// resolve. This is the URL K-ORC authenticates against (the seeded clouds.yaml
// auth_url) as long as Keystone shares its cluster: a consumer inside the cluster
// uses the Service DNS, never the external endpoint. A Keystone placed on a
// target cluster is the exception the name cannot serve — see korcAuthURL.
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

// glanceCatalogURL returns the URL registered for the K-ORC image catalog PUBLIC
// Endpoint. Per D6 the image service registers both a public and an internal
// endpoint from the start; the internal endpoint advertises the in-cluster
// Service URL (glanceEndpointURL) as long as the service is co-located (see
// internalCatalogURL), while the public one prefers an explicit
// services.glance.publicEndpoint (the only way to advertise a non-443 external
// port), then the externally routable gateway hostname ("https://{gateway.hostname}"),
// and falls back to that same in-cluster URL when Glance is not exposed via a
// Gateway. Unlike keystoneCatalogURL there is no "/v3" path suffix — the Glance
// API is served at the root.
func glanceCatalogURL(cp *c5c3v1alpha1.ControlPlane) string {
	if pe := cp.Spec.Services.Glance.PublicEndpoint; pe != "" {
		return pe
	}
	if gw := cp.Spec.Services.Glance.Gateway; gw != nil {
		return fmt.Sprintf("https://%s", gw.Hostname)
	}
	return glanceEndpointURL(cp)
}

// placementCatalogURL returns the URL registered for the K-ORC placement catalog
// PUBLIC Endpoint. Like the image row, the placement service registers both a
// public and an internal endpoint from the start; the internal endpoint
// advertises the in-cluster Service URL (placementEndpointURL) as long as the
// service is co-located (see internalCatalogURL), while the public one prefers an
// explicit services.placement.publicEndpoint (the only way to
// advertise a non-443 external port), then the externally routable gateway
// hostname ("https://{gateway.hostname}"), and falls back to that same in-cluster
// URL when Placement is not exposed via a Gateway. Unlike keystoneCatalogURL there
// is no "/v3" path suffix — the Placement API is served at the root.
func placementCatalogURL(cp *c5c3v1alpha1.ControlPlane) string {
	if pe := cp.Spec.Services.Placement.PublicEndpoint; pe != "" {
		return pe
	}
	if gw := cp.Spec.Services.Placement.Gateway; gw != nil {
		return fmt.Sprintf("https://%s", gw.Hostname)
	}
	return placementEndpointURL(cp)
}

// barbicanCatalogURL returns the URL registered for the K-ORC key-manager catalog
// PUBLIC Endpoint. Like the image and placement rows, the key-manager service
// registers both a public and an internal endpoint from the start; the internal
// endpoint advertises the in-cluster Service URL (barbicanEndpointURL) as long as
// the service is co-located (see internalCatalogURL), while the public one
// prefers an explicit services.barbican.publicEndpoint (the
// only way to advertise a non-443 external port), then the externally routable
// gateway hostname ("https://{gateway.hostname}"), and falls back to that same
// in-cluster URL when Barbican is not exposed via a Gateway. Unlike
// keystoneCatalogURL there is no "/v3" path suffix: the Barbican API is served at
// the root.
func barbicanCatalogURL(cp *c5c3v1alpha1.ControlPlane) string {
	if pe := cp.Spec.Services.Barbican.PublicEndpoint; pe != "" {
		return pe
	}
	if gw := cp.Spec.Services.Barbican.Gateway; gw != nil {
		return fmt.Sprintf("https://%s", gw.Hostname)
	}
	return barbicanEndpointURL(cp)
}

// neutronCatalogURL returns the URL registered for the K-ORC network catalog
// PUBLIC Endpoint. Like the image, placement and key-manager rows, the network
// service registers both a public and an internal endpoint from the start; the
// internal endpoint advertises the in-cluster Service URL (neutronEndpointURL) as
// long as the service is co-located (see internalCatalogURL), while the public one
// prefers an explicit services.neutron.publicEndpoint (the only way to advertise a
// non-443 external port), then the externally routable gateway hostname
// ("https://{gateway.hostname}"), and falls back to that same in-cluster URL when
// Neutron is not exposed via a Gateway. Unlike keystoneCatalogURL there is no
// "/v3" path suffix: the Neutron API is served at the root, and clients discover
// "/v2.0" themselves.
func neutronCatalogURL(cp *c5c3v1alpha1.ControlPlane) string {
	if pe := cp.Spec.Services.Neutron.PublicEndpoint; pe != "" {
		return pe
	}
	if gw := cp.Spec.Services.Neutron.Gateway; gw != nil {
		return fmt.Sprintf("https://%s", gw.Hostname)
	}
	return neutronEndpointURL(cp)
}
