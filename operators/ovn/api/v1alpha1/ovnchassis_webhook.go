// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/validation"
	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// updateStrategyOnDelete is the DaemonSet update strategy that hands the rollout
// pace to whoever deletes the pods. maxUnavailable has no meaning under it.
const updateStrategyOnDelete = "OnDelete"

// integrationBridge is the OVS bridge ovn-controller creates and owns. A fabric
// peer bridge of that name would hand the integration bridge to the fabric
// node script, which enslaves it to the fabric VRF and deletes it at teardown.
const integrationBridge = "br-int"

// ovnVRFPrefix is the prefix of the VRF devices ovn-controller maintains, one
// ovnvrf<id> per dynamic-routing-vrf-id. A fabric VRF carrying it could collide
// with one of them, and ovn-controller deletes a maintained VRF it no longer
// needs.
const ovnVRFPrefix = "ovnvrf"

// OVNChassisWebhook implements defaulting and validation webhooks for the
// OVNChassis CRD. Client is injected at startup for cluster-scoped resource
// lookups. Production wiring injects mgr.GetAPIReader(), a direct uncached
// reader, so admission never rejects a just-created object from a stale informer
// cache and no lazy informer start happens inside the webhook timeout.
// +kubebuilder:object:generate=false
type OVNChassisWebhook struct {
	commonwebhook.NoopDeleteValidator[*OVNChassis]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*OVNChassis] = &OVNChassisWebhook{}
	_ admission.Validator[*OVNChassis] = &OVNChassisWebhook{}
)

// +kubebuilder:webhook:path=/mutate-ovn-openstack-c5c3-io-v1alpha1-ovnchassis,mutating=true,failurePolicy=fail,sideEffects=None,groups=ovn.openstack.c5c3.io,resources=ovnchassis,verbs=create;update,versions=v1alpha1,name=movnchassis.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-ovn-openstack-c5c3-io-v1alpha1-ovnchassis,mutating=false,failurePolicy=fail,sideEffects=None,groups=ovn.openstack.c5c3.io,resources=ovnchassis,verbs=create;update,versions=v1alpha1,name=vovnchassis.kb.io,admissionReviewVersions=v1
// The validating webhook reads the PriorityClasses the spec names
// (validation.PriorityClassExists).
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get,roleName=ovn-webhook

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *OVNChassisWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*OVNChassis](mgr, &OVNChassis{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*OVNChassis]. It leaves the object
// untouched: every OVNChassis default is either a +kubebuilder:default the API
// server applies from the CRD schema, or a value the operator resolves at
// reconcile time (the image, and the maxUnavailable of 1) so an unset field
// keeps tracking the operator default across upgrades instead of freezing
// today's value into the stored CR.
//
// The mutating webhook is registered nonetheless, so a default that has to be
// materialized later can be added without changing the deployed webhook
// configuration.
func (w *OVNChassisWebhook) Default(_ context.Context, _ *OVNChassis) error {
	return nil
}

// ValidateCreate implements admission.Validator[*OVNChassis].
//
// The metadata.name bound is enforced here rather than in validate(), which
// update shares: the name is immutable, so on update the rule could only ever
// fire against an object a pre-upgrade operator already admitted, and the
// validating webhook also sees the finalizer-removal update reconcileDelete
// issues, so rejecting it would wedge that CR in Terminating with no field left
// to edit to repair it.
func (w *OVNChassisWebhook) ValidateCreate(ctx context.Context, obj *OVNChassis) (admission.Warnings, error) {
	return nil, w.validate(ctx, obj, validateOVNChassisNameLength(obj.Name))
}

// validateOVNChassisNameLength bounds metadata.name by the child object with the
// tightest name budget, the per-node "{name}-chassis-del-{8 hex}" Job.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateOVNChassisNameLength(name string) field.ErrorList {
	if len(name) <= MaxOVNChassisNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"), name,
		fmt.Sprintf("name must be at most %d characters: the per-node Jobs append up to 21 characters and Kubernetes caps object names at 63",
			MaxOVNChassisNameLength),
	)}
}

// ValidateUpdate implements admission.Validator[*OVNChassis].
//
// spec.targetClusterRef and spec.centralRef are compared across both revisions
// here, the webhook-layer twin of the three transition CEL rules on
// OVNChassisSpec.
//
// An update to a CR that is being deleted and leaves its spec alone is admitted
// without validation. That is the finalizer removal reconcileDelete issues, and
// the rules below can reject an unchanged spec that was admitted earlier, such
// as a PriorityClass deleted since. Rejecting the removal would hold the CR in
// Terminating. A deleting CR whose spec changes is still validated. Default
// leaves the object untouched, so the two specs compare as sent.
func (w *OVNChassisWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *OVNChassis) (admission.Warnings, error) {
	if newObj.DeletionTimestamp != nil && equality.Semantic.DeepEqual(oldObj.Spec, newObj.Spec) {
		return nil, nil
	}

	updateErrs := validation.TargetClusterRefImmutable(
		field.NewPath("spec", "targetClusterRef"),
		oldObj.Spec.TargetClusterRef,
		newObj.Spec.TargetClusterRef,
	)

	// Repointing a live chassis leaves its registration behind in the old
	// Southbound database, where it keeps claiming the ports of workloads that
	// have moved on.
	if oldObj.Spec.CentralRef.Name != newObj.Spec.CentralRef.Name {
		updateErrs = append(updateErrs, field.Invalid(
			field.NewPath("spec", "centralRef", "name"),
			newObj.Spec.CentralRef.Name,
			"centralRef is immutable",
		))
	}

	return nil, w.validate(ctx, newObj, updateErrs)
}

// validate runs all validation rules against the OVNChassis spec, accumulating
// every violation so users see the full list in one admission response. extra
// carries the errors accumulated by the caller (on create the metadata.name
// bound, on update the targetClusterRef and centralRef immutability checks) so
// they aggregate into the single Invalid error alongside the rest. ctx is
// required for the PriorityClass lookup of spec.jobs, which is skipped when no
// reader is injected.
func (w *OVNChassisWebhook) validate(ctx context.Context, c *OVNChassis, extra field.ErrorList) error {
	specPath := field.NewPath("spec")

	allErrs := validation.TargetClusterRef(specPath.Child("targetClusterRef"), c.Spec.TargetClusterRef)
	allErrs = append(allErrs, validateImage(specPath.Child("image"), c.Spec.Image)...)
	allErrs = append(allErrs, validation.JobBase(ctx, w.Client, specPath.Child("jobs"), c.Spec.Jobs)...)
	allErrs = append(allErrs, validateBridgeMappings(specPath.Child("bridgeMappings"), c.Spec.BridgeMappings)...)
	allErrs = append(allErrs, validateFabric(specPath.Child("fabric"), c.Spec.Fabric)...)
	allErrs = append(allErrs, validateUpdateStrategy(specPath.Child("updateStrategy"), c.Spec.UpdateStrategy)...)
	// Defense-in-depth twins of the verticalAutoscaling markers and CEL rules,
	// plus the minAllowed-above-maxAllowed rule CEL cannot express.
	allErrs = append(allErrs, validation.VerticalAutoscaling(specPath.Child("verticalAutoscaling"), c.Spec.VerticalAutoscaling)...)

	if c.Spec.CentralRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("centralRef", "name"), "centralRef.name must be set",
		))
	}

	// Defense-in-depth mirrors of the two +kubebuilder:validation:MinProperties=1
	// markers. An empty selector matches every node rather than none, so the
	// permissive reading of "unset" is the dangerous one in both places.
	if len(c.Spec.NodeSelector) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("nodeSelector"), "nodeSelector must carry at least one label",
		))
	}
	if c.Spec.Gateway != nil && len(c.Spec.Gateway.NodeSelector) == 0 {
		allErrs = append(allErrs, field.Required(
			specPath.Child("gateway", "nodeSelector"), "gateway.nodeSelector must carry at least one label",
		))
	}

	// Defense-in-depth mirror of the +kubebuilder:validation:Minimum=1 marker:
	// ovs-vswitchd needs at least one revalidator to expire datapath flows.
	if c.Spec.OVS != nil && c.Spec.OVS.RevalidatorThreads != nil && *c.Spec.OVS.RevalidatorThreads < 1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("ovs", "revalidatorThreads"), *c.Spec.OVS.RevalidatorThreads,
			"revalidatorThreads must be at least 1",
		))
	}

	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "OVNChassis"},
			c.Name,
			allErrs,
		)
	}
	return nil
}

// validateBridgeMappings rejects a bridge or a physical network that appears
// twice. The +listMapKey already makes physicalNetwork unique per list, but the
// bridge side has no schema-level counterpart, and a repeated bridge renders an
// ovn-bridge-mappings string whose second entry silently shadows the first.
func validateBridgeMappings(fldPath *field.Path, mappings []OVNBridgeMapping) field.ErrorList {
	var errs field.ErrorList
	seenNetworks := make(map[string]struct{}, len(mappings))
	seenBridges := make(map[string]struct{}, len(mappings))

	for i, m := range mappings {
		if _, dup := seenNetworks[m.PhysicalNetwork]; dup {
			errs = append(errs, field.Duplicate(fldPath.Index(i).Child("physicalNetwork"), m.PhysicalNetwork))
		}
		seenNetworks[m.PhysicalNetwork] = struct{}{}

		if _, dup := seenBridges[m.Bridge]; dup {
			errs = append(errs, field.Duplicate(fldPath.Index(i).Child("bridge"), m.Bridge))
		}
		seenBridges[m.Bridge] = struct{}{}
	}
	return errs
}

// validateFabric runs the rules of spec.fabric that no schema marker expresses.
// Each compares two fields, or reads a value as an address or a prefix, so the
// schema bounds of the block (patterns, ranges, formats, list sizes) get no twin
// here: the API server enforces them whether or not this webhook runs.
//
// Loopbacks are parsed with netip.ParseAddr and ranges with netip.ParsePrefix.
// A value that does not parse is skipped, because the schema format has already
// rejected it. A nil block returns nothing.
func validateFabric(fldPath *field.Path, fabric *OVNFabricSpec) field.ErrorList {
	if fabric == nil {
		return nil
	}
	var errs field.ErrorList
	vrfPath := fldPath.Child("vrf")

	interfaces := make(map[string]struct{}, len(fabric.Uplinks))
	for _, uplink := range fabric.Uplinks {
		interfaces[uplink.Interface] = struct{}{}
	}

	seenBridges := make(map[string]struct{}, len(fabric.Uplinks))
	for i, uplink := range fabric.Uplinks {
		uplinkPath := fldPath.Child("uplinks").Index(i)
		bridgePath := uplinkPath.Child("bridge")
		if _, dup := seenBridges[uplink.Bridge]; dup {
			errs = append(errs, field.Duplicate(bridgePath, uplink.Bridge))
		}
		seenBridges[uplink.Bridge] = struct{}{}

		// The script adds the NIC as a port of the bridge, and a bridge named
		// after an uplink would take that uplink's own name in the kernel.
		if _, taken := interfaces[uplink.Bridge]; taken {
			errs = append(errs, field.Invalid(bridgePath, uplink.Bridge,
				"a peer bridge cannot carry the name of an uplink interface"))
		}
		if uplink.Bridge == integrationBridge {
			errs = append(errs, field.Invalid(bridgePath, uplink.Bridge,
				"br-int is the integration bridge of ovn-controller"))
		}
		// One kernel namespace holds the VRF device, the bridges and the NICs.
		if uplink.Bridge == fabric.VRF.Name {
			errs = append(errs, field.Invalid(bridgePath, uplink.Bridge,
				"the name is taken by the fabric VRF device"))
		}
		if uplink.Interface == fabric.VRF.Name {
			errs = append(errs, field.Invalid(uplinkPath.Child("interface"), uplink.Interface,
				"the name is taken by the fabric VRF device"))
		}
	}

	if strings.HasPrefix(fabric.VRF.Name, ovnVRFPrefix) {
		errs = append(errs, field.Invalid(vrfPath.Child("name"), fabric.VRF.Name,
			"the prefix ovnvrf is reserved for the VRFs ovn-controller maintains"))
	}

	switch fabric.VRF.Table {
	case 253, 254, 255:
		errs = append(errs, field.Invalid(vrfPath.Child("table"), fabric.VRF.Table,
			"tables 253, 254 and 255 are the kernel's default, main and local tables"))
	}
	// ovn-controller creates ovnvrf<id> with table <id>, so a fabric VRF on that
	// table would share its routes with one of the OVN VRFs.
	if slices.Contains(fabric.AdvertiseVRFIDs, fabric.VRF.Table) || slices.Contains(fabric.LearnVRFIDs, fabric.VRF.Table) {
		errs = append(errs, field.Invalid(vrfPath.Child("table"), fabric.VRF.Table,
			"ovn-controller uses a VRF id as the table of ovnvrf<id>"))
	}

	// Importing in both directions between one ovnvrf<id> and the fabric VRF
	// is untested.
	for i, id := range fabric.LearnVRFIDs {
		if slices.Contains(fabric.AdvertiseVRFIDs, id) {
			errs = append(errs, field.Invalid(fldPath.Child("learnVRFIDs").Index(i), id,
				"a VRF id is either advertised from or learned into, not both"))
		}
	}

	errs = append(errs, validateFabricNodes(fldPath.Child("nodes"), fabric.Nodes)...)

	rangesPath := fldPath.Child("announceableRanges")
	for i, value := range fabric.AnnounceableRanges {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			continue
		}
		// The FRRConfiguration prints every range masked, so a range with host
		// bits would permit another network than the one written here.
		if masked := prefix.Masked(); masked != prefix {
			errs = append(errs, field.Invalid(rangesPath.Index(i), value,
				"must be a network address; did you mean "+masked.String()))
		}
	}
	if len(fabric.AdvertiseVRFIDs) > 0 && len(fabric.AnnounceableRanges) == 0 {
		errs = append(errs, field.Required(rangesPath,
			"advertiseVRFIDs needs at least one announceable range"))
	}
	return errs
}

// validateFabricNodes rejects an ASN or a loopback two rows of the node table
// share. The list-map key makes the node names unique already. A BGP speaker
// drops a route whose AS path carries its own ASN, so two nodes sharing one
// would drop each other's routes, and two nodes sharing a loopback announce the
// same host route.
func validateFabricNodes(fldPath *field.Path, nodes []OVNFabricNode) field.ErrorList {
	var errs field.ErrorList
	seenASNs := make(map[int64]struct{}, len(nodes))
	seenLoopbacks := make(map[netip.Addr]struct{}, 2*len(nodes))

	for i, node := range nodes {
		nodePath := fldPath.Index(i)
		if _, dup := seenASNs[node.ASN]; dup {
			errs = append(errs, field.Duplicate(nodePath.Child("asn"), node.ASN))
		}
		seenASNs[node.ASN] = struct{}{}

		for _, loopback := range []struct {
			name, value string
		}{
			{"loopbackIPv4", node.LoopbackIPv4},
			{"loopbackIPv6", node.LoopbackIPv6},
		} {
			// Compared as parsed addresses, so 2001:db8::1 and 2001:DB8:0::1 are
			// the same loopback.
			addr, err := netip.ParseAddr(loopback.value)
			if err != nil {
				continue
			}
			if _, dup := seenLoopbacks[addr]; dup {
				errs = append(errs, field.Duplicate(nodePath.Child(loopback.name), loopback.value))
			}
			seenLoopbacks[addr] = struct{}{}
		}
	}
	return errs
}

// validateUpdateStrategy rejects the maxUnavailable shapes the DaemonSet would
// accept but not act on: one paired with OnDelete, where nothing reads it, and
// any value that does not resolve to at least one pod.
//
// The field is an IntOrString, so the schema carries x-kubernetes-int-or-string
// and no bound of its own, and chassisUpdateStrategy copies the value through
// verbatim with no maxSurge beside it. Kubernetes' own DaemonSet validation
// then rejects a negative value, a malformed percentage and a percentage that
// scales to zero, and the operator has nowhere to report that: it would fail
// the apply on every pass while OVSReady sat at DaemonSetError. Resolving the
// value here is what turns all of those into one admission message.
//
// The percentage is scaled against 100 rather than against the node count: the
// nodes the DaemonSet lands on are not known at admission time, and a
// percentage that rounds to zero on a smaller cluster is the same wedge. Round
// up, the way the DaemonSet controller does for maxUnavailable, so "1%" is
// admitted and behaves as one node.
func validateUpdateStrategy(fldPath *field.Path, strategy OVNChassisUpdateStrategy) field.ErrorList {
	if strategy.MaxUnavailable == nil {
		return nil
	}
	maxUnavailablePath := fldPath.Child("maxUnavailable")

	if strategy.Type == updateStrategyOnDelete {
		return field.ErrorList{field.Invalid(
			maxUnavailablePath, strategy.MaxUnavailable.String(),
			"maxUnavailable applies to RollingUpdate only",
		)}
	}

	resolved, err := intstr.GetScaledValueFromIntOrPercent(strategy.MaxUnavailable, 100, true)
	if err != nil {
		return field.ErrorList{field.Invalid(
			maxUnavailablePath, strategy.MaxUnavailable.String(),
			"maxUnavailable must be an integer or a percentage such as \"25%\"",
		)}
	}
	if resolved < 1 {
		return field.ErrorList{field.Invalid(
			maxUnavailablePath, strategy.MaxUnavailable.String(),
			"maxUnavailable must resolve to at least 1 for RollingUpdate",
		)}
	}
	return nil
}
