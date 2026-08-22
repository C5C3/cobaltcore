// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// korcOpenStackNameCommaMessage is the rejection every field cast to K-ORC's
// OpenStackName shares. A comma admitted here would only move the rejection to
// the K-ORC CRD, where it wedges the child reconcile in an exponential backoff
// that no KeystoneService field error explains.
const korcOpenStackNameCommaMessage = "must not contain a comma (mirrors K-ORC's OpenStackName pattern ^[^,]+$)"

const (
	// keystoneServiceChildPrefixOverhead is the fixed part every child name
	// carries between the CR's own name and its per-kind discriminator: the
	// separator, the eight hash characters, and the "-registration-"
	// discriminator that keystoneServiceChildPrefix composes (see
	// keystoneServiceChildPrefix in operators/c5c3/internal/controller/
	// keystoneservice_controller.go — a change to the shape there must be
	// reflected in these three constants).
	keystoneServiceChildPrefixOverhead = len("-") + 8 + len("-registration-")

	// keystoneServiceChildNameOverhead is the longest child name EVERY
	// KeystoneService can mint, i.e. everything except metadata.name. That is
	// the generation-scoped password Secret "<prefix>password-v<N>", whose
	// generation suffix is bounded generously at 10 digits.
	//
	// Every CR is charged it, a catalog-only one included: an account block can
	// be added to a live CR later and metadata.name can never be shortened, so
	// admitting a name here that the added block could not mint children for
	// would wedge that edit permanently.
	keystoneServiceChildNameOverhead = keystoneServiceChildPrefixOverhead + len("password-v") + 10

	// keystoneServiceRoleChildNameOverhead is the same bound for a CR that
	// declares roles: it additionally mints the managed RoleAssignment
	// "<prefix>assign-<slug>", where slug is bounded at 25 bytes by
	// serviceAccountRoleSlug (16 bytes of readable base, a separator, and 8 hash
	// characters). That is the longest child name any KeystoneService reaches.
	//
	// Only a CR WITH roles is charged it. A roles-less CR mints no assignment,
	// so charging it the wider budget would reject a name the reconciler handles
	// fine — and, because this check also runs on update, would wedge every
	// later edit to a CR an earlier operator level already admitted.
	keystoneServiceRoleChildNameOverhead = keystoneServiceChildPrefixOverhead + len("assign-") + 25
)

// KeystoneServiceWebhook implements defaulting and validation webhooks for the
// KeystoneService CRD.
//
// Unlike ControlPlaneWebhook it holds no Client: every rule it enforces is
// decidable from the object under admission alone. The two cross-object rules
// an admission-time check could carry (no two CRs resolving to one Keystone
// identity, and no CR taking over the admin identity) are NOT enforced here.
// Both would need reads that admission cannot rely on: a sibling List across
// namespaces the allowlist has yet to define, and a ControlPlane the CRD
// contract explicitly allows to be absent at admission time (GitOps ordering).
// The reconciler is the guard instead, and it fails loudly rather than silently:
// a sibling identity is caught by the collision probes, and the admin identity by
// the unconditional refusal at the head of ensureAccount — unconditional because
// account.adopt=true short-circuits the probes and must not double as the switch
// that hands over the cloud admin account.
// +kubebuilder:object:generate=false
type KeystoneServiceWebhook struct {
	commonwebhook.NoopDeleteValidator[*KeystoneService]
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*KeystoneService] = &KeystoneServiceWebhook{}
	_ admission.Validator[*KeystoneService] = &KeystoneServiceWebhook{}
)

// +kubebuilder:webhook:path=/mutate-c5c3-io-v1alpha1-keystoneservice,mutating=true,failurePolicy=fail,sideEffects=None,groups=c5c3.io,resources=keystoneservices,verbs=create;update,versions=v1alpha1,name=mkeystoneservice.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-c5c3-io-v1alpha1-keystoneservice,mutating=false,failurePolicy=fail,sideEffects=None,groups=c5c3.io,resources=keystoneservices,verbs=create;update,versions=v1alpha1,name=vkeystoneservice.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with
// the manager.
func (w *KeystoneServiceWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*KeystoneService](mgr, &KeystoneService{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*KeystoneService]. It materializes the
// account's user name from the CR's own name, the default the field documents,
// so the stored object carries the identity the reconciler acts on rather than
// leaving the two to agree by convention.
//
// Nothing else is defaulted. rotation.mode carries a +kubebuilder:default
// marker, and the account's domain and the catalog's service name resolve
// against values admission cannot see (the referenced ControlPlane's admin
// domain, and a name the reconciler derives), so both stay reconciler-side
// exactly as their field docs promise.
//
// It fills only a zero-valued field, so it is idempotent, and it returns a nil
// error unconditionally: the webhook holds no client and reads nothing.
func (w *KeystoneServiceWebhook) Default(_ context.Context, obj *KeystoneService) error {
	if obj.Spec.Account != nil && obj.Spec.Account.UserName == "" {
		obj.Spec.Account.UserName = obj.Name
	}
	return nil
}

// ValidateCreate implements admission.Validator[*KeystoneService].
func (w *KeystoneServiceWebhook) ValidateCreate(_ context.Context, obj *KeystoneService) (admission.Warnings, error) {
	return nil, newInvalidKeystoneServiceIfErrs(obj, w.validate(obj))
}

// ValidateUpdate implements admission.Validator[*KeystoneService]. It re-runs
// the value rules against the new object and adds the identity freezes, folding
// both into one Invalid response so a reviewer sees every problem at once.
func (w *KeystoneServiceWebhook) ValidateUpdate(_ context.Context, oldObj, newObj *KeystoneService) (admission.Warnings, error) {
	allErrs := w.validate(newObj)
	allErrs = append(allErrs, validateKeystoneServiceImmutable(oldObj, newObj)...)
	return nil, newInvalidKeystoneServiceIfErrs(newObj, allErrs)
}

// newInvalidKeystoneServiceIfErrs wraps accumulated field errors into a single
// Invalid response, or returns nil when there are none.
func newInvalidKeystoneServiceIfErrs(ks *KeystoneService, allErrs field.ErrorList) error {
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "KeystoneService"},
		ks.Name,
		allErrs,
	)
}

// validate accumulates every value-level violation on ks. The CRD markers and
// CEL rules are the primary enforcement point; these checks are the
// defense-in-depth twin for callers that bypass CRD schema admission, plus the
// one rule the schema cannot express at all — the child-CR name-length bound.
func (w *KeystoneServiceWebhook) validate(ks *KeystoneService) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Mirrors the spec-level CEL rule: a CR declaring neither block is noise no
	// reconciler can act on.
	if ks.Spec.Catalog == nil && ks.Spec.Account == nil {
		allErrs = append(allErrs, field.Invalid(specPath, "",
			"at least one of spec.catalog or spec.account must be set"))
	}

	allErrs = append(allErrs, validateKeystoneServiceControlPlaneRef(specPath.Child("controlPlaneRef"), ks.Spec.ControlPlaneRef)...)
	allErrs = append(allErrs, validateKeystoneServiceCatalog(specPath.Child("catalog"), ks.Spec.Catalog)...)
	allErrs = append(allErrs, validateKeystoneServiceAccount(specPath.Child("account"), ks.Spec.Account)...)
	allErrs = append(allErrs, validateKeystoneServiceChildName(ks)...)

	return allErrs
}

// validateKeystoneServiceControlPlaneRef mirrors the declarative constraints on
// ControlPlaneRefSpec. The referenced ControlPlane itself is deliberately NOT
// looked up: GitOps ordering may apply the registration before the plane, so a
// dangling reference is reported through the CR's conditions rather than
// rejected at admission (the contract the field doc states).
func validateKeystoneServiceControlPlaneRef(refPath *field.Path, ref ControlPlaneRefSpec) field.ErrorList {
	var errs field.ErrorList
	if ref.Name == "" {
		errs = append(errs, field.Required(refPath.Child("name"), "must be set"))
	}
	// An empty namespace means the CR's own and is the documented default; a
	// declared one names a Kubernetes namespace and must have its shape.
	if ref.Namespace != "" && !namespaceNamePattern.MatchString(ref.Namespace) {
		errs = append(errs, field.Invalid(refPath.Child("namespace"), ref.Namespace,
			"must be a lowercase alphanumeric RFC-1123 label (it names a Kubernetes namespace)"))
	}
	return errs
}

// validateKeystoneServiceCatalog mirrors the declarative constraints on the
// catalog block, including the identity rejection, and rejects an endpoint row
// the K-ORC Endpoint CRD would reject downstream.
func validateKeystoneServiceCatalog(catalogPath *field.Path, catalog *KeystoneServiceCatalogSpec) field.ErrorList {
	if catalog == nil {
		return nil
	}
	var errs field.ErrorList

	typePath := catalogPath.Child("serviceType")
	switch {
	case catalog.ServiceType == "":
		errs = append(errs, field.Required(typePath, "must be set"))
	case catalog.ServiceType == IdentityCatalogServiceType:
		errs = append(errs, field.Invalid(typePath, catalog.ServiceType,
			"the identity catalog entry is ControlPlane-owned and cannot be registered through a KeystoneService"))
	case !catalogEntryTypePattern.MatchString(catalog.ServiceType):
		errs = append(errs, field.Invalid(typePath, catalog.ServiceType,
			"must be a lowercase alphanumeric DNS-1123 label (it names the child K-ORC CRs)"))
	}

	// K-ORC casts the service name to its OpenStackName, whose Pattern rejects a comma.
	if strings.Contains(catalog.ServiceName, ",") {
		errs = append(errs, field.Invalid(catalogPath.Child("serviceName"), catalog.ServiceName,
			korcOpenStackNameCommaMessage))
	}

	seenInterfaces := make(map[ExternalEndpointType]struct{}, len(catalog.Endpoints))
	for i, ep := range catalog.Endpoints {
		epPath := catalogPath.Child("endpoints").Index(i)
		switch ep.Interface {
		case ExternalEndpointTypePublic, ExternalEndpointTypeInternal, ExternalEndpointTypeAdmin:
		case "":
			errs = append(errs, field.Required(epPath.Child("interface"), "must be set"))
		default:
			// The interface reaches a child CR name, which must stay a DNS-1123
			// subdomain: an off-enum value the CRD would have rejected wedges the
			// reconcile rather than failing at admission.
			errs = append(errs, field.NotSupported(epPath.Child("interface"), ep.Interface,
				[]ExternalEndpointType{
					ExternalEndpointTypePublic, ExternalEndpointTypeInternal, ExternalEndpointTypeAdmin,
				}))
		}
		// The listType=map key: the apiserver rejects a duplicate for callers that
		// went through CRD schema admission, this arm for the ones that did not.
		if _, dup := seenInterfaces[ep.Interface]; dup {
			errs = append(errs, field.Duplicate(epPath.Child("interface"), ep.Interface))
		}
		seenInterfaces[ep.Interface] = struct{}{}

		if _, err := validateHTTPURL(epPath.Child("url"), ep.URL); err != nil {
			errs = append(errs, err)
		} else if len(ep.URL) > maxCatalogEndpointURLBytes {
			errs = append(errs, field.Invalid(epPath.Child("url"), ep.URL,
				fmt.Sprintf("must be at most %d bytes", maxCatalogEndpointURLBytes)))
		}
	}

	return errs
}

// validateKeystoneServiceAccount mirrors the declarative constraints on the
// account block: every value K-ORC casts to one of its name filters, plus the
// required project name.
func validateKeystoneServiceAccount(accountPath *field.Path, account *KeystoneServiceAccountSpec) field.ErrorList {
	if account == nil {
		return nil
	}
	var errs field.ErrorList

	if strings.Contains(account.UserName, ",") {
		errs = append(errs, field.Invalid(accountPath.Child("userName"), account.UserName,
			korcOpenStackNameCommaMessage))
	}
	if strings.Contains(account.DomainName, ",") {
		errs = append(errs, field.Invalid(accountPath.Child("domainName"), account.DomainName,
			korcOpenStackNameCommaMessage))
	}

	projectPath := accountPath.Child("project")
	switch {
	case account.Project.Name == "":
		errs = append(errs, field.Required(projectPath.Child("name"), "must be set"))
	case strings.Contains(account.Project.Name, ","):
		errs = append(errs, field.Invalid(projectPath.Child("name"), account.Project.Name,
			"must not contain a comma (mirrors K-ORC's KeystoneName pattern ^[^,]+$)"))
	}

	for i, role := range account.Roles {
		if strings.Contains(role, ",") {
			errs = append(errs, field.Invalid(accountPath.Child("roles").Index(i), role,
				korcOpenStackNameCommaMessage))
		}
	}

	return errs
}

// validateKeystoneServiceChildName bounds metadata.name so every child CR the
// reconciler composes from it stays within the apiserver's object-name limit.
// This is the rule the CRD schema cannot express: nothing caps metadata.name
// below 253, so a name admitted without this check wedges the reconcile
// projecting a child the apiserver rejects — on a CR admission already accepted.
func validateKeystoneServiceChildName(ks *KeystoneService) field.ErrorList {
	overhead := keystoneServiceChildNameOverhead
	if ks.Spec.Account != nil && len(ks.Spec.Account.Roles) > 0 {
		overhead = keystoneServiceRoleChildNameOverhead
	}
	n := len(ks.Name) + overhead
	if n <= maxObjectNameBytes {
		return nil
	}
	return field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), ks.Name, fmt.Sprintf(
		"the child K-ORC CR name would be %d bytes; shorten the KeystoneService name (or drop the "+
			"account's roles) so the total stays within the %d-byte Kubernetes object-name limit",
		n, maxObjectNameBytes,
	))}
}

// validateKeystoneServiceImmutable freezes the fields that name a live Keystone
// identity. A block that only one side declares is an add or a remove the
// reconciler handles as a create or a teardown, not a mutation, so its fields
// are compared only when both sides carry it.
//
// adopt stays mutable on both blocks: flipping it to true is the documented
// collision remediation. So do the endpoint rows, the roles and the rotation
// policy, none of which re-point an existing identity.
//
// Two fields are compared by their EFFECTIVE value rather than the declared
// one, because the reconciler acts on the effective value and admission can
// resolve it:
//
//   - the user name, whose fallback is metadata.name. A CR stored before this
//     webhook existed carries it empty, and the defaulter materializes it on the
//     next edit; a declared-value comparison would read that as a rename and
//     wedge every later update of that CR.
//   - the catalog service name, same fallback. Here the effective comparison is
//     the stricter one: setting an explicit serviceName on a CR that relied on
//     the fallback IS a catalog rename, and a declared-value comparison would
//     miss it because the old value was absent.
//
// The domain name is compared as declared. Resolving its fallback needs the
// referenced ControlPlane's admin domain, which admission cannot read, so an
// explicit value replacing the fallback is rejected even where it names that
// same domain.
func validateKeystoneServiceImmutable(oldObj, newObj *KeystoneService) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	refPath := specPath.Child("controlPlaneRef")
	if oldObj.Spec.ControlPlaneRef.Name != newObj.Spec.ControlPlaneRef.Name {
		allErrs = append(allErrs, field.Invalid(refPath.Child("name"), newObj.Spec.ControlPlaneRef.Name,
			"controlPlaneRef.name is immutable; delete and re-create the KeystoneService to register "+
				"against another ControlPlane"))
	}
	// The effective namespace: an empty value means the CR's own, so the
	// transition between an empty value and an explicit one naming that same
	// namespace changes nothing and stays admitted. This freeze is webhook-only —
	// a CEL rule on a spec field cannot read metadata.namespace to resolve it.
	if keystoneServiceControlPlaneNamespace(oldObj) != keystoneServiceControlPlaneNamespace(newObj) {
		allErrs = append(allErrs, field.Invalid(refPath.Child("namespace"), newObj.Spec.ControlPlaneRef.Namespace,
			"controlPlaneRef.namespace is immutable; delete and re-create the KeystoneService to register "+
				"against a ControlPlane in another namespace"))
	}

	if oldCatalog, newCatalog := oldObj.Spec.Catalog, newObj.Spec.Catalog; oldCatalog != nil && newCatalog != nil {
		catalogPath := specPath.Child("catalog")
		if oldCatalog.ServiceType != newCatalog.ServiceType {
			allErrs = append(allErrs, field.Invalid(catalogPath.Child("serviceType"), newCatalog.ServiceType,
				"serviceType is immutable; delete and re-create the KeystoneService to register a different "+
					"service type"))
		}
		if keystoneServiceCatalogName(oldObj) != keystoneServiceCatalogName(newObj) {
			allErrs = append(allErrs, field.Invalid(catalogPath.Child("serviceName"), newCatalog.ServiceName,
				"serviceName is immutable; delete and re-create the KeystoneService to rename its catalog entry"))
		}
	}

	if oldAccount, newAccount := oldObj.Spec.Account, newObj.Spec.Account; oldAccount != nil && newAccount != nil {
		accountPath := specPath.Child("account")
		if keystoneServiceUserName(oldObj) != keystoneServiceUserName(newObj) {
			allErrs = append(allErrs, field.Invalid(accountPath.Child("userName"), newAccount.UserName,
				"userName is immutable; delete and re-create the KeystoneService to rename its user"))
		}
		if oldAccount.DomainName != newAccount.DomainName {
			allErrs = append(allErrs, field.Invalid(accountPath.Child("domainName"), newAccount.DomainName,
				"domainName is immutable; delete and re-create the KeystoneService to move it to another domain"))
		}
		if oldAccount.Project.Name != newAccount.Project.Name {
			allErrs = append(allErrs, field.Invalid(accountPath.Child("project", "name"), newAccount.Project.Name,
				"project.name is immutable; delete and re-create the KeystoneService to re-point its project"))
		}
		if oldAccount.Project.Create != newAccount.Project.Create {
			allErrs = append(allErrs, field.Invalid(accountPath.Child("project", "create"), newAccount.Project.Create,
				"project.create is immutable; a managed<->referenced flip would orphan or adopt the live project"))
		}
	}

	return allErrs
}

// keystoneServiceControlPlaneNamespace resolves the namespace the referenced
// ControlPlane is looked up in: the declared one, else the CR's own. It mirrors
// the reconciler's own resolution so admission and reconcile agree on which
// plane a registration belongs to.
func keystoneServiceControlPlaneNamespace(ks *KeystoneService) string {
	return cmp.Or(ks.Spec.ControlPlaneRef.Namespace, ks.Namespace)
}

// keystoneServiceUserName resolves the OpenStack user name: the declared one,
// else the CR's own name. It mirrors the reconciler's keystoneServiceUserName
// so admission and reconcile agree on the identity being frozen.
func keystoneServiceUserName(ks *KeystoneService) string {
	if ks.Spec.Account == nil {
		return ""
	}
	return cmp.Or(ks.Spec.Account.UserName, ks.Name)
}

// keystoneServiceCatalogName resolves the catalog service name: the declared
// one, else the CR's own name. It mirrors the reconciler's
// keystoneServiceCatalogName, which deliberately does not fall back to K-ORC's
// default (the child CR's name, which carries the uniqueness suffix).
func keystoneServiceCatalogName(ks *KeystoneService) string {
	if ks.Spec.Catalog == nil {
		return ""
	}
	return cmp.Or(ks.Spec.Catalog.ServiceName, ks.Name)
}
