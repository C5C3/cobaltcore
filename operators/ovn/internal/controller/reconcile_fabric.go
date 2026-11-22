// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/gateway"
	"github.com/c5c3/cobaltcore/internal/common/job"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// conditionTypeFabricReady is the condition the Fabric step reports under: the
// node side of the BGP fabric is configured, that is, every selected node has a
// fabric assignment and every one of them that runs frr-k8s carries its
// FRRConfiguration. A chassis without spec.fabric reports it True.
const conditionTypeFabricReady = "FabricReady"

// The condition reasons of the Fabric step.
const (
	conditionReasonFabricConfigured       = "FabricConfigured"
	conditionReasonFabricNotConfigured    = "FabricNotConfigured"
	conditionReasonNodeWithoutFabricEntry = "NodeWithoutFabricEntry"
	conditionReasonFRRK8sNotInstalled     = "FRRK8sNotInstalled"
	conditionReasonFabricError            = "FabricError"
)

// conditionTypeRoutesAdvertised is the condition the route probe reports
// under: whether the Southbound database of the chassis's OVNCentral holds
// advertised routes. It is no sub-condition of Ready, and its name carries no
// Ready suffix for that reason: a fabric without a floating IP advertises
// nothing and is still ready.
const conditionTypeRoutesAdvertised = "RoutesAdvertised"

// The condition reasons of the route probe.
const (
	conditionReasonRoutesAdvertised     = "RoutesAdvertised"
	conditionReasonNoAdvertisedRoutes   = "NoAdvertisedRoutes"
	conditionReasonRouteProbeFailed     = "RouteProbeFailed"
	conditionReasonRouteProbeUnreadable = "RouteProbeUnreadable"
	conditionReasonRouteProbeDeferred   = "RouteProbeDeferred"
	conditionReasonRouteProbePending    = "RouteProbePending"
)

// routeProbeReport is the shape of the route probe's termination message.
var routeProbeReport = regexp.MustCompile(`^advertised=(\d+) learned=(\d+)$`)

// fabricFinalizer holds an OVNChassis while FRRConfigurations of it exist. They
// live in frr-k8s-system rather than in the CR's namespace, so no owner
// reference can tie them to the CR, on the local cluster as little as on a
// target cluster, and no garbage collection removes them when the CR goes.
const fabricFinalizer = "ovn.openstack.c5c3.io/fabric-frrconfigurations"

// frrK8sNamespace is the namespace frr-k8s reads its FRRConfigurations from. It
// is a constant: frr-k8s reads its own namespace only, and both installs this
// repository documents create frr-k8s-system.
const frrK8sNamespace = "frr-k8s-system"

// frrConfigurationGVK is the kind the Fabric step writes. The operator carries
// it unstructured and takes no Go dependency on frr-k8s: the kind is optional on
// every cluster.
var frrConfigurationGVK = schema.GroupVersionKind{
	Group:   "frrk8s.metallb.io",
	Version: "v1beta1",
	Kind:    "FRRConfiguration",
}

// componentFabric is the component-label value of the FRRConfigurations and
// the name segment that marks them.
const componentFabric = "fabric"

// fabricNodeAnnotation names the Node an FRRConfiguration is for. The object
// name carries a hash of the node name only, so the annotation is what tells a
// reader which node it configures.
const fabricNodeAnnotation = "ovn.openstack.c5c3.io/node"

// fabricBFDProfile is the name of the one BFD profile every fabric session
// takes.
const fabricBFDProfile = "fabric"

// frrRawPriority is the priority of the raw block. frr-k8s appends raw blocks
// after the configuration it renders from the typed fields, ordered by
// priority, and the block only adds to that configuration.
const frrRawPriority = 10

// frrRawPrefixListSeqBase is the seq of the first entry the raw block adds to
// an outbound prefix list. frr-k8s v0.0.27 numbers the entries it generates
// from seq 1, one per prefix of the router, and the fabric router lists its
// two loopbacks, so the raw entries stay clear of the generated ones.
const frrRawPrefixListSeqBase = 100

// The FRRConfiguration spec, in the field names of frr-k8s v0.0.27
// (api/v1beta1/frrconfiguration_types.go). Every integer is an int64, which is
// how a live object decodes, so a spec read back compares equal to one built
// here.
type frrConfigurationSpec struct {
	NodeSelector frrNodeSelector `json:"nodeSelector"`
	BGP          frrBGP          `json:"bgp"`
	Raw          *frrRaw         `json:"raw,omitempty"`
}

type frrNodeSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

type frrBGP struct {
	BFDProfiles []frrBFDProfile `json:"bfdProfiles"`
	Routers     []frrRouter     `json:"routers"`
}

type frrBFDProfile struct {
	Name             string `json:"name"`
	ReceiveInterval  int64  `json:"receiveInterval"`
	TransmitInterval int64  `json:"transmitInterval"`
	DetectMultiplier int64  `json:"detectMultiplier"`
}

type frrRouter struct {
	ASN       int64         `json:"asn"`
	ID        string        `json:"id,omitempty"`
	VRF       string        `json:"vrf"`
	Prefixes  []string      `json:"prefixes,omitempty"`
	Imports   []frrImport   `json:"imports,omitempty"`
	Neighbors []frrNeighbor `json:"neighbors,omitempty"`
}

type frrImport struct {
	VRF string `json:"vrf"`
}

type frrNeighbor struct {
	Interface   string       `json:"interface"`
	DynamicASN  string       `json:"dynamicASN"`
	BFDProfile  string       `json:"bfdProfile"`
	ToAdvertise frrAdvertise `json:"toAdvertise"`
	ToReceive   frrReceive   `json:"toReceive"`
}

type frrAdvertise struct {
	Allowed frrAllowedOut `json:"allowed"`
}

type frrAllowedOut struct {
	Mode string `json:"mode"`
}

type frrReceive struct {
	Allowed frrAllowedIn `json:"allowed"`
}

type frrAllowedIn struct {
	Mode     string              `json:"mode"`
	Prefixes []frrPrefixSelector `json:"prefixes"`
}

type frrPrefixSelector struct {
	Prefix string `json:"prefix"`
}

type frrRaw struct {
	Priority  int64  `json:"priority"`
	RawConfig string `json:"rawConfig"`
}

// ovnVRFName is the VRF ovn-controller maintains for a dynamic-routing-vrf-id.
func ovnVRFName(id int64) string {
	return "ovnvrf" + strconv.FormatInt(id, 10)
}

// fabricFRRConfigurationName names the FRRConfiguration of one node. The node
// name is hashed, as in maintenanceJobName, so the name fits the 63 characters
// of a label value that frr-k8s's status objects copy it into.
func fabricFRRConfigurationName(cr *ovnv1alpha1.OVNChassis, node string) string {
	return maintenanceJobName(cr, componentFabric, node)
}

// buildFRRConfiguration builds the FRRConfiguration of one fabric node: the
// fabric router in the fabric VRF, peering unnumbered over every peer bridge,
// announcing the node's loopbacks and importing every advertised ovnvrf<id>;
// one router per advertised id, whose routes a raw block redistributes from the
// kernel; and one router per learned id that imports the fabric VRF, so the
// leaf defaults reach ovn-controller.
//
// frr-k8s generates the outbound prefix lists of a neighbor from the typed
// fields, and they permit the router's own prefixes only. The raw block adds
// one entry per announceable range to each of them, under the list names of
// frr-k8s v0.0.27 (internal/frr/config.go), so a host route OVN installs from a
// range reaches the leaf and nothing else does. The n-th range of a family
// takes seq frrRawPrefixListSeqBase+n-1 in every list of that family.
//
// Every value of the raw block is an integer, a pattern-bound name or a prefix
// this function parsed and printed, so no spec value is spliced into the FRR
// configuration as it was written.
func buildFRRConfiguration(cr *ovnv1alpha1.OVNChassis, node string, entry nodeEntry,
	ownerLabels map[string]string,
) (*unstructured.Unstructured, error) {
	fabric := cr.Spec.Fabric
	vrf := fabric.VRF.Name

	fabricRouter := frrRouter{
		ASN:      entry.asn,
		ID:       entry.fabric.loopbackV4,
		VRF:      vrf,
		Prefixes: []string{entry.fabric.loopbackV4 + "/32", entry.fabric.loopbackV6 + "/128"},
	}
	for _, id := range fabric.AdvertiseVRFIDs {
		fabricRouter.Imports = append(fabricRouter.Imports, frrImport{VRF: ovnVRFName(id)})
	}
	for _, uplink := range fabric.Uplinks {
		fabricRouter.Neighbors = append(fabricRouter.Neighbors, frrNeighbor{
			Interface:   uplink.Bridge,
			DynamicASN:  "external",
			BFDProfile:  fabricBFDProfile,
			ToAdvertise: frrAdvertise{Allowed: frrAllowedOut{Mode: "all"}},
			ToReceive: frrReceive{Allowed: frrAllowedIn{
				Mode:     "filtered",
				Prefixes: []frrPrefixSelector{{Prefix: "0.0.0.0/0"}, {Prefix: "::/0"}},
			}},
		})
	}

	routers := []frrRouter{fabricRouter}
	for _, id := range fabric.AdvertiseVRFIDs {
		routers = append(routers, frrRouter{ASN: entry.asn, VRF: ovnVRFName(id)})
	}
	for _, id := range fabric.LearnVRFIDs {
		routers = append(routers, frrRouter{ASN: entry.asn, VRF: ovnVRFName(id), Imports: []frrImport{{VRF: vrf}}})
	}

	spec := frrConfigurationSpec{
		NodeSelector: frrNodeSelector{MatchLabels: map[string]string{corev1.LabelHostname: entry.hostname}},
		BGP: frrBGP{
			BFDProfiles: []frrBFDProfile{{
				Name:             fabricBFDProfile,
				ReceiveInterval:  int64(fabric.BFD.ReceiveIntervalMs),
				TransmitInterval: int64(fabric.BFD.TransmitIntervalMs),
				DetectMultiplier: int64(fabric.BFD.DetectMultiplier),
			}},
			Routers: routers,
		},
	}
	if len(fabric.AdvertiseVRFIDs) > 0 {
		spec.Raw = &frrRaw{Priority: frrRawPriority, RawConfig: fabricRawConfig(fabric, entry.asn)}
	}

	rawSpec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		return nil, fmt.Errorf("converting the FRRConfiguration of node %s: %w", node, err)
	}

	obj := &unstructured.Unstructured{Object: map[string]any{"spec": rawSpec}}
	obj.SetGroupVersionKind(frrConfigurationGVK)
	obj.SetName(fabricFRRConfigurationName(cr, node))
	obj.SetNamespace(frrK8sNamespace)
	labels := naming.ComponentLabels(chassisAppName, cr.Name, componentFabric)
	maps.Copy(labels, ownerLabels)
	obj.SetLabels(labels)
	obj.SetAnnotations(map[string]string{fabricNodeAnnotation: node})
	return obj, nil
}

// fabricRawConfig renders the raw block of a fabric node's FRRConfiguration:
// the redistribution of the kernel routes ovn-controller installs into each
// advertised ovnvrf<id>, and the entries that let host routes from the
// announceable ranges through the outbound lists of every neighbor.
func fabricRawConfig(fabric *ovnv1alpha1.OVNFabricSpec, asn int64) string {
	var b strings.Builder
	for _, id := range fabric.AdvertiseVRFIDs {
		fmt.Fprintf(&b, "router bgp %d vrf %s\n", asn, ovnVRFName(id))
		for _, family := range []string{"ipv4", "ipv6"} {
			fmt.Fprintf(&b, " address-family %s unicast\n  redistribute kernel\n exit-address-family\n", family)
		}
		b.WriteString("exit\n")
	}

	var v4, v6 []netip.Prefix
	for _, value := range fabric.AnnounceableRanges {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			continue
		}
		if prefix = prefix.Masked(); prefix.Addr().Is4() {
			v4 = append(v4, prefix)
		} else {
			v6 = append(v6, prefix)
		}
	}
	for _, family := range []struct {
		keyword, suffix string
		ranges          []netip.Prefix
		hostBits        int
	}{
		{"ip", "ipv4", v4, 32},
		{"ipv6", "ipv6", v6, 128},
	} {
		for _, uplink := range fabric.Uplinks {
			for n, prefix := range family.ranges {
				fmt.Fprintf(&b, "%s prefix-list %s-%s-allowed-%s seq %d permit %s ge %d\n",
					family.keyword, uplink.Bridge, fabric.VRF.Name, family.suffix, frrRawPrefixListSeqBase+n,
					prefix, family.hostBits)
			}
		}
	}
	return b.String()
}

// fabricNodes counts the selected nodes of a fabric chassis that have a fabric
// assignment, and names, sorted, those among them that run frr-k8s and the
// selected nodes that have none. Leaving nodes are in none of the three.
func fabricNodes(nodes renderedNodes) (attached int, frrK8s, unattached []string) {
	for _, name := range slices.Sorted(maps.Keys(nodes)) {
		entry := nodes[name]
		switch {
		case entry.leaving:
		case entry.fabric == nil:
			unattached = append(unattached, name)
		default:
			attached++
			if entry.frrK8s {
				frrK8s = append(frrK8s, name)
			}
		}
	}
	return attached, frrK8s, unattached
}

// reconcileFabric runs the Fabric step: the FRRConfigurations of the fabric
// nodes, reported in FabricReady, and while spec.fabric is set the route probe,
// reported in RoutesAdvertised. A pass that gets through the first half ends
// with RequeueRouteProbe, the schedule of the probe; with spec.fabric unset it
// removes RoutesAdvertised and ends with a zero result.
func (r *OVNChassisReconciler) reconcileFabric(ctx context.Context, children client.Client,
	cr *ovnv1alpha1.OVNChassis, central resolvedCentral, nodes renderedNodes,
) (ctrl.Result, error) {
	if res, err := r.reconcileFRRConfigurations(ctx, children, cr, nodes); err != nil || !res.IsZero() {
		return res, err
	}
	if cr.Spec.Fabric == nil {
		meta.RemoveStatusCondition(&cr.Status.Conditions, conditionTypeRoutesAdvertised)
		return ctrl.Result{}, nil
	}
	r.probeRoutes(ctx, children, cr, central)
	return ctrl.Result{RequeueAfter: RequeueRouteProbe}, nil
}

// reconcileFRRConfigurations generates one FRRConfiguration per fabric node
// that runs frr-k8s, in frr-k8s-system on the cluster the children land on, and
// reports in FabricReady whether the node side of the fabric is configured.
//
// Its objects carry the ownership labels
// rather than an owner reference, because they live in another namespace than
// the CR, and fabricFinalizer holds the CR until they are gone. There is no
// watch on the kind: it is optional on every cluster, so a drifted object is
// written back on the next pass.
//
// A chassis that never had a fabric node on frr-k8s and carries no finalizer
// issues no request for the kind at all.
func (r *OVNChassisReconciler) reconcileFRRConfigurations(ctx context.Context, children client.Client,
	cr *ovnv1alpha1.OVNChassis, nodes renderedNodes,
) (ctrl.Result, error) {
	var (
		attached           int
		wanted, unattached []string
	)
	if cr.Spec.Fabric != nil {
		attached, wanted, unattached = fabricNodes(nodes)
	}

	if len(wanted) == 0 && !controllerutil.ContainsFinalizer(cr, fabricFinalizer) {
		setFabricCondition(cr, attached, 0, unattached)
		return ctrl.Result{}, nil
	}

	fail := func(err error) (ctrl.Result, error) {
		chassisSkeleton.MarkFailed(cr, conditionTypeFabricReady, conditionReasonFabricError, err)
		return ctrl.Result{}, err
	}

	if len(wanted) > 0 {
		served, err := r.frrConfigurationServed(children)
		if err != nil {
			err = fmt.Errorf("probing for %s: %w", frrConfigurationGVK.Kind, err)
			chassisSkeleton.MarkFailed(cr, conditionTypeFabricReady, commonmulticluster.CapabilityProbeFailed, err)
			return ctrl.Result{}, err
		}
		if !served {
			conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
				Type:               conditionTypeFabricReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cr.Generation,
				Reason:             conditionReasonFRRK8sNotInstalled,
				Message: fmt.Sprintf("The cluster of nodes %s, which carry %s=true, does not serve %s %s; "+
					"install frr-k8s there", strings.Join(wanted, ", "), frrK8sNodeLabel,
					frrConfigurationGVK.GroupVersion(), frrConfigurationGVK.Kind),
			})
			return ctrl.Result{}, nil
		}
	}

	// The list goes ahead of the finalizer: a cluster that refuses it, such as
	// a target whose access chart grants no FRRConfiguration access, gets no
	// finalizer, which would hold the CR's deletion on the same refusal while
	// guarding no object. The finalizer still goes on before the first write.
	live, err := listFRRConfigurations(ctx, children)
	if err != nil {
		return fail(err)
	}
	if len(wanted) > 0 {
		if added, err := r.addFabricFinalizer(ctx, cr); err != nil {
			return fail(err)
		} else if added {
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
		}
	}
	ownerLabels, err := commonmulticluster.OwnerLabels(r.Scheme, cr)
	if err != nil {
		return fail(err)
	}

	desired := make(map[string]struct{}, len(wanted))
	var refused []string
	for _, node := range wanted {
		obj, err := buildFRRConfiguration(cr, node, nodes[node], ownerLabels)
		if err != nil {
			return fail(err)
		}
		desired[obj.GetName()] = struct{}{}

		current := live[obj.GetName()]
		if current != nil && !r.controls(cr, current) {
			// Somebody else's object under the name this node wants. Writing it
			// would reshape their peering, and the prune below would delete it.
			refused = append(refused, obj.GetName())
			continue
		}
		if current != nil && frrConfigurationContains(current, obj) {
			continue
		}
		if err := apply.EnsureUnownedObject(ctx, children, r.Scheme, obj, apply.FieldManager); err != nil {
			return fail(fmt.Errorf("applying FRRConfiguration %s: %w", obj.GetName(), err))
		}
	}

	for _, name := range slices.Sorted(maps.Keys(live)) {
		if _, keep := desired[name]; keep || !r.controls(cr, live[name]) {
			continue
		}
		if err := deleteFRRConfiguration(ctx, children, live[name]); err != nil {
			return fail(err)
		}
	}

	if len(desired) == 0 {
		if err := r.removeFabricFinalizer(ctx, cr); err != nil {
			return fail(err)
		}
	}

	if len(refused) > 0 {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeFabricReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonFabricError,
			Message: fmt.Sprintf("FRRConfiguration %s exists and is not owned by this OVNChassis",
				strings.Join(refused, ", ")),
		})
		return ctrl.Result{}, nil
	}
	setFabricCondition(cr, attached, len(desired), unattached)
	return ctrl.Result{}, nil
}

// setFabricCondition reports the outcome of a pass that wrote what it had to:
// the nodes left without a fabric assignment first, then the attached ones.
func setFabricCondition(cr *ovnv1alpha1.OVNChassis, attached, generated int, unattached []string) {
	cond := metav1.Condition{
		Type:               conditionTypeFabricReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
	}
	switch {
	case cr.Spec.Fabric == nil:
		cond.Reason = conditionReasonFabricNotConfigured
		cond.Message = "spec.fabric is not set; no node is attached to the BGP fabric"
	case len(unattached) > 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = conditionReasonNodeWithoutFabricEntry
		cond.Message = fmt.Sprintf("Nodes %s are selected but have no usable row in spec.fabric.nodes, "+
			"so they stay unattached", strings.Join(unattached, ", "))
	default:
		cond.Reason = conditionReasonFabricConfigured
		cond.Message = fmt.Sprintf("Rendered %d fabric nodes; %d carry an FRRConfiguration in %s",
			attached, generated, frrK8sNamespace)
	}
	conditions.SetCondition(&cr.Status.Conditions, cond)
}

// frrConfigurationServed reports whether the cluster the children land on
// serves FRRConfiguration. The local cluster is asked on every pass rather than
// once at setup, because frr-k8s can be installed after the operator started.
func (r *OVNChassisReconciler) frrConfigurationServed(children client.Client) (bool, error) {
	local := false
	if !commonmulticluster.IsRemote(children) {
		var err error
		if local, err = gateway.IsGVKAvailable(r.RESTMapper(), frrConfigurationGVK); err != nil {
			return false, err
		}
	}
	return commonmulticluster.ChildrenServeKind(children, local, frrConfigurationGVK)
}

// controls reports whether obj is one of this CR's FRRConfigurations. An error
// resolving the CR's own kind cannot happen for a CR the scheme decoded, and
// reads as not owned, which writes and deletes nothing.
func (r *OVNChassisReconciler) controls(cr *ovnv1alpha1.OVNChassis, obj client.Object) bool {
	owned, err := commonmulticluster.Controls(r.Scheme, cr, obj)
	return err == nil && owned
}

// listFRRConfigurations reads the FRRConfigurations of frr-k8s-system, keyed by
// name, through the live reader of the children's cluster. No manager of this
// repository caches unstructured objects, so the list is a live read on the
// local cluster too and needs no watch verb. A cluster that does not serve the
// kind has none.
func listFRRConfigurations(ctx context.Context, children client.Client) (map[string]*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(frrConfigurationGVK.GroupVersion().WithKind(frrConfigurationGVK.Kind + "List"))
	err := commonmulticluster.LiveReader(children).List(ctx, list, client.InNamespace(frrK8sNamespace))
	switch {
	case meta.IsNoMatchError(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("listing FRRConfigurations in %s: %w", frrK8sNamespace, err)
	}
	live := make(map[string]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		live[list.Items[i].GetName()] = &list.Items[i]
	}
	return live, nil
}

// deleteFRRConfiguration deletes one FRRConfiguration. One that is already gone
// is what the delete was for.
func deleteFRRConfiguration(ctx context.Context, children client.Client, obj *unstructured.Unstructured) error {
	if err := children.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting FRRConfiguration %s: %w", obj.GetName(), err)
	}
	return nil
}

// frrConfigurationContains reports whether the live object already carries
// everything desired sets: its labels, its annotation and its spec. Fields the
// API server or another writer added are ignored, the way
// equality.Semantic.DeepDerivative ignores them, so a steady pass applies
// nothing. Lists are compared item by item and by length, which DeepDerivative
// does not do: it accepts a desired list that is shorter than the live one, so
// a removed uplink or VRF id would never be written.
func frrConfigurationContains(live, desired *unstructured.Unstructured) bool {
	for key, value := range desired.GetLabels() {
		if live.GetLabels()[key] != value {
			return false
		}
	}
	for key, value := range desired.GetAnnotations() {
		if live.GetAnnotations()[key] != value {
			return false
		}
	}
	return containsDesired(desired.Object["spec"], live.Object["spec"])
}

// containsDesired is the comparison frrConfigurationContains applies to the
// spec: every key of a desired map must be present in the live map and contain
// its value, every desired list must have the length of the live one and
// contain it item by item, and scalars must be equal.
func containsDesired(desired, live any) bool {
	switch want := desired.(type) {
	case map[string]any:
		have, ok := live.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range want {
			if !containsDesired(value, have[key]) {
				return false
			}
		}
		return true
	case []any:
		have, ok := live.([]any)
		if !ok || len(have) != len(want) {
			return false
		}
		for i := range want {
			if !containsDesired(want[i], have[i]) {
				return false
			}
		}
		return true
	default:
		return equality.Semantic.DeepEqual(desired, live)
	}
}

// addFabricFinalizer installs fabricFinalizer, reporting whether it was added.
// The write goes through a copy of the CR, and only the finalizers and the
// resourceVersion are carried back: an Update decodes the persisted object into
// the one it is handed, which would discard the status this pass has set so
// far, status.nodes[].fabric among it.
func (r *OVNChassisReconciler) addFabricFinalizer(ctx context.Context, cr *ovnv1alpha1.OVNChassis) (bool, error) {
	updated := cr.DeepCopy()
	added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, updated, fabricFinalizer)
	if err != nil || !added {
		return false, err
	}
	cr.Finalizers = updated.Finalizers
	cr.ResourceVersion = updated.ResourceVersion
	return true, nil
}

// removeFabricFinalizer releases fabricFinalizer when the CR carries it, in the
// same way addFabricFinalizer installs it.
func (r *OVNChassisReconciler) removeFabricFinalizer(ctx context.Context, cr *ovnv1alpha1.OVNChassis) error {
	if !controllerutil.ContainsFinalizer(cr, fabricFinalizer) {
		return nil
	}
	updated := cr.DeepCopy()
	controllerutil.RemoveFinalizer(updated, fabricFinalizer)
	if err := r.Update(ctx, updated); err != nil {
		return fmt.Errorf("removing finalizer %q: %w", fabricFinalizer, err)
	}
	cr.Finalizers = updated.Finalizers
	cr.ResourceVersion = updated.ResourceVersion
	return nil
}

// reconcileDeleteFabric deletes the FRRConfigurations of a CR that is being
// deleted and releases fabricFinalizer. It runs before the remote-children
// sweep.
//
// A nil children client is a target cluster the CR was given up on: the
// finalizer is released without a request, and the RemoteChildrenAbandoned
// event of the sweep that follows covers what stays behind. A cluster that no
// longer serves the kind has nothing left to delete.
func (r *OVNChassisReconciler) reconcileDeleteFabric(ctx context.Context, children client.Client,
	cr *ovnv1alpha1.OVNChassis,
) error {
	if !controllerutil.ContainsFinalizer(cr, fabricFinalizer) {
		return nil
	}
	if children != nil {
		live, err := listFRRConfigurations(ctx, children)
		if err != nil {
			return err
		}
		for _, name := range slices.Sorted(maps.Keys(live)) {
			if !r.controls(cr, live[name]) {
				continue
			}
			if err := deleteFRRConfiguration(ctx, children, live[name]); err != nil {
				return err
			}
		}
	}
	controllerutil.RemoveFinalizer(cr, fabricFinalizer)
	if err := r.Update(ctx, cr); err != nil {
		return fmt.Errorf("removing finalizer %q: %w", fabricFinalizer, err)
	}
	return nil
}

// routeProbeJob builds the Job that counts the Southbound routes. It is a
// central client Job like chassis-del, with the client Secret mounted, no node
// pin and no host network, and it addresses the Southbound database rather
// than the relay.
func routeProbeJob(cr *ovnv1alpha1.OVNChassis, central resolvedCentral) *batchv1.Job {
	return maintenanceJob(cr, cr.Name+"-route-probe", routeProbeScriptKey,
		[]corev1.EnvVar{{Name: "SB_ADDR", Value: central.sbAddress}},
		[]corev1.Volume{{Name: tlsVolumeName, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: central.clientSecretName},
		}}},
		[]corev1.VolumeMount{{Name: tlsVolumeName, MountPath: ovnTLSDir, ReadOnly: true}},
		"", false)
}

// routeProbeRerunKey is the rerun key of the route probe Job: the index of the
// RequeueRouteProbe-long window now falls into. Every pass inside one window
// reads the same Job, and the first pass of the next window runs a new one.
func routeProbeRerunKey(now time.Time) string {
	return strconv.FormatInt(now.Unix()/int64(RequeueRouteProbe/time.Second), 10)
}

// now is the reconciler's clock.
func (r *OVNChassisReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// probeRoutes runs the route probe Job of the current window and reports its
// finding in RoutesAdvertised. No outcome of the probe is an error of the pass:
// a failed Job is reported on the condition, and the next window runs a new
// one. While a Job runs, the condition keeps its previous value.
func (r *OVNChassisReconciler) probeRoutes(ctx context.Context, children client.Client,
	cr *ovnv1alpha1.OVNChassis, central resolvedCentral,
) {
	set := func(status metav1.ConditionStatus, reason, message string) {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeRoutesAdvertised,
			Status:             status,
			ObservedGeneration: cr.Generation,
			Reason:             reason,
			Message:            message,
		})
	}

	if central.sbAddress == "" {
		set(metav1.ConditionUnknown, conditionReasonRouteProbeDeferred,
			fmt.Sprintf("Waiting for OVNCentral %s to publish its Southbound address", cr.Spec.CentralRef.Name))
		return
	}

	done, observed, err := job.RunJobWithRerunKey(ctx, children, r.Scheme, cr, routeProbeJob(cr, central),
		routeProbeRerunKey(r.now()))
	switch {
	case err != nil:
		set(metav1.ConditionFalse, conditionReasonRouteProbeFailed,
			fmt.Sprintf("The route probe Job failed: %v; inspect its pod logs", err))
		return
	case !done:
		if meta.FindStatusCondition(cr.Status.Conditions, conditionTypeRoutesAdvertised) == nil {
			set(metav1.ConditionUnknown, conditionReasonRouteProbePending, "No route probe Job has finished yet")
		}
		return
	}

	message, err := job.TerminationMessage(ctx, commonmulticluster.LiveReader(children), observed)
	if err != nil {
		set(metav1.ConditionFalse, conditionReasonRouteProbeFailed,
			fmt.Sprintf("Reading the report of the route probe Job failed: %v", err))
		return
	}
	match := routeProbeReport.FindStringSubmatch(message)
	var advertised, learned int64
	if match != nil {
		advertised, err = strconv.ParseInt(match[1], 10, 64)
		if err == nil {
			learned, err = strconv.ParseInt(match[2], 10, 64)
		}
	}
	if match == nil || err != nil {
		set(metav1.ConditionUnknown, conditionReasonRouteProbeUnreadable,
			fmt.Sprintf("The route probe Job reported %q, not \"advertised=<n> learned=<n>\"", message))
		return
	}

	counts := fmt.Sprintf("The Southbound database of OVNCentral %s holds %d advertised and %d learned routes",
		cr.Spec.CentralRef.Name, advertised, learned)
	if advertised > 0 {
		set(metav1.ConditionTrue, conditionReasonRoutesAdvertised, counts)
		return
	}
	set(metav1.ConditionFalse, conditionReasonNoAdvertisedRoutes, counts)
}
