// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/validation"
	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// Nova metadata defaults the agent block is filled with when it is present.
const (
	// DefaultNovaMetadataPort is nova-api-metadata's own listen port, resolved
	// when spec.novaMetadata is set without one.
	DefaultNovaMetadataPort int32 = 8775
	// defaultSharedSecretKey is the Secret key spec.novaMetadata.sharedSecretRef
	// is defaulted to.
	defaultSharedSecretKey = "shared_secret"
)

// NeutronMetadataAgentWebhook implements defaulting and validation webhooks for
// the NeutronMetadataAgent CRD. Client is injected at startup for cluster-scoped
// resource lookups. Production wiring injects mgr.GetAPIReader(), a direct
// uncached reader, so admission never rejects a just-created object from a stale
// informer cache and no lazy informer start happens inside the webhook timeout.
// +kubebuilder:object:generate=false
type NeutronMetadataAgentWebhook struct {
	commonwebhook.NoopDeleteValidator[*NeutronMetadataAgent]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*NeutronMetadataAgent] = &NeutronMetadataAgentWebhook{}
	_ admission.Validator[*NeutronMetadataAgent] = &NeutronMetadataAgentWebhook{}
)

// +kubebuilder:webhook:path=/mutate-neutron-openstack-c5c3-io-v1alpha1-neutronmetadataagent,mutating=true,failurePolicy=fail,sideEffects=None,groups=neutron.openstack.c5c3.io,resources=neutronmetadataagents,verbs=create;update,versions=v1alpha1,name=mneutronmetadataagent.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-neutron-openstack-c5c3-io-v1alpha1-neutronmetadataagent,mutating=false,failurePolicy=fail,sideEffects=None,groups=neutron.openstack.c5c3.io,resources=neutronmetadataagents,verbs=create;update,versions=v1alpha1,name=vneutronmetadataagent.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *NeutronMetadataAgentWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*NeutronMetadataAgent](mgr, &NeutronMetadataAgent{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*NeutronMetadataAgent]. It materializes
// spec.logging so downstream reconciler code never sees a nil pointer, and fills
// the two Nova metadata leaves only when the block that carries them is present:
// a nil spec.novaMetadata stays nil, because the agent then renders neither key
// and the oslo defaults apply.
func (w *NeutronMetadataAgentWebhook) Default(_ context.Context, obj *NeutronMetadataAgent) error {
	if obj.Spec.Logging == nil {
		obj.Spec.Logging = &LoggingSpec{}
	}
	obj.Spec.Logging.Default()

	if obj.Spec.NovaMetadata != nil {
		if obj.Spec.NovaMetadata.Port == 0 {
			obj.Spec.NovaMetadata.Port = DefaultNovaMetadataPort
		}
		if obj.Spec.NovaMetadata.SharedSecretRef != nil && obj.Spec.NovaMetadata.SharedSecretRef.Key == "" {
			obj.Spec.NovaMetadata.SharedSecretRef.Key = defaultSharedSecretKey
		}
	}
	return nil
}

// ValidateCreate implements admission.Validator[*NeutronMetadataAgent].
//
// The metadata.name bound is enforced here rather than in validate(), which
// update shares: the name is immutable, so on update the rule could only ever
// fire against an object a pre-upgrade operator already admitted — and the
// validating webhook also sees the finalizer-removal update reconcileDelete
// issues, so rejecting it would wedge that CR in Terminating with no field left
// to edit to repair it.
func (w *NeutronMetadataAgentWebhook) ValidateCreate(_ context.Context, obj *NeutronMetadataAgent) (admission.Warnings, error) {
	warnings, createErrs := validateExtraConfigOptions(
		field.NewPath("spec"), obj.Spec.OpenStackRelease, obj.Spec.ExtraConfig, MetadataAgentOwnedConfigKeys)
	createErrs = append(createErrs, validateMetadataAgentNameLength(obj.Name)...)
	return warnings, w.validate(obj, createErrs)
}

// validateMetadataAgentNameLength bounds metadata.name by the label value it
// becomes on every child.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateMetadataAgentNameLength(name string) field.ErrorList {
	if len(name) <= MaxNeutronMetadataAgentNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"), name,
		fmt.Sprintf("name must be at most %d characters: it is the app.kubernetes.io/instance label value on every child and Kubernetes caps label values at %d characters",
			MaxNeutronMetadataAgentNameLength, MaxNeutronMetadataAgentNameLength),
	)}
}

// ValidateUpdate implements admission.Validator[*NeutronMetadataAgent].
//
// The extraConfig option-catalog check is re-run only when one of its inputs
// changed (extraConfig or spec.openStackRelease), so a CR whose extraConfig went
// stale-invalid against a regenerated catalog is not rejected by an unrelated
// update.
//
// spec.targetClusterRef and spec.chassisRef are compared across both revisions
// here, the webhook-layer twin of the three transition CEL rules on
// NeutronMetadataAgentSpec.
func (w *NeutronMetadataAgentWebhook) ValidateUpdate(_ context.Context, oldObj, newObj *NeutronMetadataAgent) (admission.Warnings, error) {
	var warnings admission.Warnings
	var updateErrs field.ErrorList
	if extraConfigCatalogInputsChanged(
		oldObj.Spec.OpenStackRelease, newObj.Spec.OpenStackRelease,
		oldObj.Spec.ExtraConfig, newObj.Spec.ExtraConfig,
	) {
		warnings, updateErrs = validateExtraConfigOptions(
			field.NewPath("spec"), newObj.Spec.OpenStackRelease, newObj.Spec.ExtraConfig, MetadataAgentOwnedConfigKeys)
	}
	updateErrs = append(updateErrs, validation.TargetClusterRefImmutable(
		field.NewPath("spec", "targetClusterRef"),
		oldObj.Spec.TargetClusterRef,
		newObj.Spec.TargetClusterRef,
	)...)

	// An agent re-pointed at another chassis lands on another set of nodes, whose
	// local OVS databases carry none of the ports it was answering for.
	if oldObj.Spec.ChassisRef.Name != newObj.Spec.ChassisRef.Name {
		updateErrs = append(updateErrs, field.Invalid(
			field.NewPath("spec", "chassisRef", "name"),
			newObj.Spec.ChassisRef.Name,
			"chassisRef is immutable",
		))
	}

	return warnings, w.validate(newObj, updateErrs)
}

// validate runs all validation rules against the NeutronMetadataAgent spec,
// accumulating every violation so users see the full list in one admission
// response. extra carries the errors accumulated by the caller (the extraConfig
// option-catalog check, on create the metadata.name bound, and on update the
// targetClusterRef and chassisRef immutability checks) so they aggregate into the
// single Invalid error alongside the rest.
func (w *NeutronMetadataAgentWebhook) validate(a *NeutronMetadataAgent, extra field.ErrorList) error {
	specPath := field.NewPath("spec")

	allErrs := validation.TargetClusterRef(specPath.Child("targetClusterRef"), a.Spec.TargetClusterRef)
	allErrs = append(allErrs, validateImage(specPath.Child("image"), a.Spec.Image)...)

	// The chassis is what puts the agent on a node and gives it the local OVS
	// database to read, so an agent without one has nothing to attach to. This is
	// the webhook twin of the MinLength marker on OVNChassisRef.Name.
	if a.Spec.ChassisRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("chassisRef", "name"),
			"chassisRef.name must be set (the OVNChassis this agent runs alongside)",
		))
	}

	// The messaging block is optional here, so the XOR only applies once it is
	// present. It is the webhook twin of the CEL rule on commonv1.MessagingSpec.
	if a.Spec.Messaging != nil {
		allErrs = append(allErrs, validation.MessagingXOR(specPath.Child("messaging"), a.Spec.Messaging)...)
	}

	if a.Spec.NovaMetadata != nil {
		novaPath := specPath.Child("novaMetadata")
		// Defense-in-depth bound alongside the Minimum=1 / Maximum=65535 markers.
		// The defaulting webhook fills a zero port with DefaultNovaMetadataPort, so
		// this fires only for an object that bypassed it.
		if a.Spec.NovaMetadata.Port < 1 || a.Spec.NovaMetadata.Port > 65535 {
			allErrs = append(allErrs, field.Invalid(
				novaPath.Child("port"), a.Spec.NovaMetadata.Port,
				"port must be between 1 and 65535",
			))
		}
		if a.Spec.NovaMetadata.SharedSecretRef != nil && a.Spec.NovaMetadata.SharedSecretRef.Name == "" {
			allErrs = append(allErrs, field.Required(
				novaPath.Child("sharedSecretRef", "name"),
				"sharedSecretRef.name must be set when spec.novaMetadata.sharedSecretRef is configured",
			))
		}
	}

	// The two typed fields below reach the verbatim INI renderer: chassisRef.name
	// is resolved into the [ovs] and [ovn] connection strings, and
	// novaMetadata.host is rendered as a [DEFAULT] option. A newline in either
	// injects an additional config line, smuggling a whole key past the (section,
	// key)-keyed ownership and catalog gates.
	for _, f := range []struct {
		path  *field.Path
		value string
	}{
		{specPath.Child("chassisRef", "name"), a.Spec.ChassisRef.Name},
		{specPath.Child("novaMetadata", "host"), novaMetadataHost(a)},
	} {
		if validation.HasControlChars(f.value) {
			allErrs = append(allErrs, field.Invalid(f.path, f.value,
				"value must not contain a newline or carriage return: it is rendered verbatim into "+
					"neutron_ovn_metadata_agent.ini, so a newline injects arbitrary config lines"))
		}
	}

	allErrs = append(allErrs, validateLogging(
		specPath.Child("logging"), a.Spec.Logging, "neutron_ovn_metadata_agent.ini")...)
	allErrs = append(allErrs, validateExtraConfigShape(
		specPath, a.Spec.ExtraConfig, MetadataAgentOwnedConfigKeys)...)

	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "NeutronMetadataAgent"},
			a.Name,
			allErrs,
		)
	}
	return nil
}

// novaMetadataHost returns spec.novaMetadata.host, or "" when spec.novaMetadata
// is unset. It exists so the control-character guard can carry the host as a
// plain table entry: HasControlChars("") is false, so a CR without the block
// contributes no error.
func novaMetadataHost(a *NeutronMetadataAgent) string {
	if a.Spec.NovaMetadata == nil {
		return ""
	}
	return a.Spec.NovaMetadata.Host
}
