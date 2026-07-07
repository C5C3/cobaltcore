// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// KeystoneIdentityBackendWebhook implements defaulting and validation
// webhooks for the KeystoneIdentityBackend CRD. Client is injected at startup
// for the domain-name uniqueness check (a namespace-scoped List of sibling
// backends). Production wiring injects mgr.GetAPIReader() — a direct,
// uncached reader — so admission never misses a just-created sibling from a
// stale informer cache and no lazy informer start happens inside the webhook
// timeout.
// +kubebuilder:object:generate=false
type KeystoneIdentityBackendWebhook struct {
	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*KeystoneIdentityBackend] = &KeystoneIdentityBackendWebhook{}
	_ admission.Validator[*KeystoneIdentityBackend] = &KeystoneIdentityBackendWebhook{}
)

// +kubebuilder:webhook:path=/mutate-keystone-openstack-c5c3-io-v1alpha1-keystoneidentitybackend,mutating=true,failurePolicy=fail,sideEffects=None,groups=keystone.openstack.c5c3.io,resources=keystoneidentitybackends,verbs=create;update,versions=v1alpha1,name=mkeystoneidentitybackend.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-keystone-openstack-c5c3-io-v1alpha1-keystoneidentitybackend,mutating=false,failurePolicy=fail,sideEffects=None,groups=keystone.openstack.c5c3.io,resources=keystoneidentitybackends,verbs=create;update,versions=v1alpha1,name=vkeystoneidentitybackend.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks
// with the manager.
func (w *KeystoneIdentityBackendWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*KeystoneIdentityBackend](mgr, &KeystoneIdentityBackend{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*KeystoneIdentityBackend]. It
// materializes the documented defaults when the fields carry zero values, so
// the production admission path has a single source of truth; the
// +kubebuilder:default markers remain as defense-in-depth for callers that
// bypass the webhook (e.g. envtest without the defaulter wired up).
func (w *KeystoneIdentityBackendWebhook) Default(_ context.Context, obj *KeystoneIdentityBackend) error {
	if obj.Spec.Domain.Mode == "" {
		obj.Spec.Domain.Mode = DomainModeManage
	}
	if obj.Spec.Domain.DeletionPolicy == "" {
		obj.Spec.Domain.DeletionPolicy = DomainDeletionPolicyRetain
	}
	if obj.Spec.LDAP != nil && obj.Spec.LDAP.ReadOnly == nil {
		obj.Spec.LDAP.ReadOnly = ptr.To(true)
	}
	return nil
}

// ValidateCreate implements admission.Validator[*KeystoneIdentityBackend].
func (w *KeystoneIdentityBackendWebhook) ValidateCreate(ctx context.Context, obj *KeystoneIdentityBackend) (admission.Warnings, error) {
	return nil, w.validate(ctx, obj)
}

// ValidateUpdate implements admission.Validator[*KeystoneIdentityBackend].
// Field immutability (keystoneRef, type, domain.name, domain.mode) is
// enforced by the CRD CEL transition rules, so the webhook re-runs only the
// value-level rules against the new object.
func (w *KeystoneIdentityBackendWebhook) ValidateUpdate(ctx context.Context, _, newObj *KeystoneIdentityBackend) (admission.Warnings, error) {
	return nil, w.validate(ctx, newObj)
}

// ValidateDelete implements admission.Validator[*KeystoneIdentityBackend].
// The method is required by the Validator interface but is never invoked: the
// validating webhook registers only create/update (no delete verb), so with
// failurePolicy=Fail a down operator can never block backend CR — and thereby
// namespace — deletion.
func (w *KeystoneIdentityBackendWebhook) ValidateDelete(_ context.Context, _ *KeystoneIdentityBackend) (admission.Warnings, error) {
	return nil, nil
}

// ldapExtraOptionsDenylist enumerates the [ldap] option names spec.extraOptions
// must never carry. Three groups:
//
//   - options rendered from typed spec fields — a duplicate would silently
//     shadow (or be shadowed by) the typed value depending on render order;
//   - the driver / domain-config wiring options the operator owns;
//   - (appended conditionally in validate) the write-enabling user_allow_* /
//     group_allow_* options when readOnly is true, because the projection
//     forces them to false and an extraOptions override would contradict the
//     read-only contract.
var ldapExtraOptionsDenylist = map[string]string{
	// Rendered from typed fields.
	"url":                    "spec.ldap.url",
	"suffix":                 "spec.ldap.suffix",
	"user":                   "spec.ldap.bindCredentialsSecretRef",
	"password":               "spec.ldap.bindCredentialsSecretRef",
	"tls_cacertfile":         "spec.ldap.tls",
	"use_pool":               "spec.ldap.pool",
	"pool_size":              "spec.ldap.pool.size",
	"user_tree_dn":           "spec.ldap.users.treeDN",
	"user_filter":            "spec.ldap.users.filter",
	"user_objectclass":       "spec.ldap.users.objectClass",
	"user_id_attribute":      "spec.ldap.users.idAttribute",
	"user_name_attribute":    "spec.ldap.users.nameAttribute",
	"user_mail_attribute":    "spec.ldap.users.mailAttribute",
	"group_tree_dn":          "spec.ldap.groups.treeDN",
	"group_filter":           "spec.ldap.groups.filter",
	"group_objectclass":      "spec.ldap.groups.objectClass",
	"group_id_attribute":     "spec.ldap.groups.idAttribute",
	"group_name_attribute":   "spec.ldap.groups.nameAttribute",
	"group_member_attribute": "spec.ldap.groups.memberAttribute",
	// Operator-owned wiring.
	"driver":            "the operator (identity driver wiring)",
	"domain_config_dir": "the operator (domain config wiring)",
}

// ReadOnlyForcedOptions are the write-enabling options the projection forces
// to false when spec.ldap.readOnly is true (the default). It is exported so
// the identitybackends projection shares this single list instead of keeping a
// second copy that must be kept in sync by hand.
var ReadOnlyForcedOptions = []string{
	"user_allow_create", "user_allow_update", "user_allow_delete",
	"group_allow_create", "group_allow_update", "group_allow_delete",
}

// validate runs all validation rules against the KeystoneIdentityBackend
// spec, accumulating every violation into a single field.ErrorList so users
// see all problems at once. ctx is required for the sibling-backend List
// behind the domain-name uniqueness rule.
func (w *KeystoneIdentityBackendWebhook) validate(ctx context.Context, b *KeystoneIdentityBackend) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Defense-in-depth keystoneRef presence check alongside the
	// +kubebuilder:validation:MinLength=1 marker. The referenced Keystone CR
	// itself is deliberately NOT looked up: GitOps ordering may apply the
	// backend before the Keystone CR, so a dangling reference is reported via
	// the DomainReady=False/KeystoneNotFound condition instead.
	if b.Spec.KeystoneRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("keystoneRef", "name"),
			"keystoneRef.name must be set",
		))
	}

	// Defense-in-depth union check alongside the spec-level CEL rule:
	// exactly one backend block, matching spec.type.
	if (b.Spec.Type == IdentityBackendTypeLDAP) != (b.Spec.LDAP != nil) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("ldap"),
			b.Spec.Type,
			"exactly one backend block matching spec.type must be set (type LDAP requires spec.ldap)",
		))
	}

	// Defense-in-depth Default-domain check alongside the CEL rule on
	// DomainSpec.Name. Case-insensitive: keystone domain-name lookups behave
	// case-insensitively on MySQL's default collation, so "Default" would
	// still collide with the bootstrap admin domain.
	if strings.EqualFold(b.Spec.Domain.Name, "default") {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("domain", "name"),
			b.Spec.Domain.Name,
			"the Default domain hosts the SQL-backed service users and the bootstrap admin and must never be backed by an external identity backend",
		))
	} else if b.Spec.Domain.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("domain", "name"),
			"domain.name must be set",
		))
	}

	if b.Spec.LDAP != nil {
		allErrs = append(allErrs, w.validateLDAP(specPath.Child("ldap"), b.Spec.LDAP)...)
	}

	allErrs = append(allErrs, w.validateExtraOptions(specPath.Child("extraOptions"), b)...)
	allErrs = append(allErrs, w.validateDomainUniqueness(ctx, specPath.Child("domain", "name"), b)...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "KeystoneIdentityBackend"},
			b.Name,
			allErrs,
		)
	}
	return nil
}

// hasControlChars reports whether s contains a newline or carriage-return.
// RenderINI writes every value verbatim as `key = value`, so either character
// lets a value inject additional INI lines — and thereby arbitrary
// [ldap]/[identity] options — defeating the readOnly forcing and the
// extraOptions denylist. Every value-side input into the projection is rejected
// for these characters.
func hasControlChars(s string) bool {
	return strings.ContainsAny(s, "\n\r")
}

// validateLDAP checks the LDAP block: URL scheme (defense-in-depth alongside
// the Pattern marker), control characters in every rendered value (INI
// injection guard), and the fixed-data-key contract on the bind Secret
// reference.
func (w *KeystoneIdentityBackendWebhook) validateLDAP(ldapPath *field.Path, l *LDAPBackendSpec) field.ErrorList {
	var errs field.ErrorList

	if !strings.HasPrefix(l.URL, "ldap://") && !strings.HasPrefix(l.URL, "ldaps://") {
		errs = append(errs, field.Invalid(
			ldapPath.Child("url"),
			l.URL,
			"url must use the ldap:// or ldaps:// scheme",
		))
	}

	// INI-injection guard: none of the rendered values may carry a newline or
	// carriage-return. The CRD markers do not anchor these fields (url is
	// start-anchored only, suffix/treeDN are MinLength, the attribute/filter
	// fields have no pattern), so the webhook is the primary gate; the renderer
	// revalidates as the last line of defense.
	checkNoCtrl := func(path *field.Path, value string) {
		if hasControlChars(value) {
			errs = append(errs, field.Invalid(path, value,
				"value must not contain newline or carriage-return characters"))
		}
	}
	checkNoCtrl(ldapPath.Child("url"), l.URL)
	checkNoCtrl(ldapPath.Child("suffix"), l.Suffix)
	checkNoCtrl(ldapPath.Child("users", "treeDN"), l.Users.TreeDN)
	checkNoCtrl(ldapPath.Child("users", "filter"), l.Users.Filter)
	checkNoCtrl(ldapPath.Child("users", "objectClass"), l.Users.ObjectClass)
	checkNoCtrl(ldapPath.Child("users", "idAttribute"), l.Users.IDAttribute)
	checkNoCtrl(ldapPath.Child("users", "nameAttribute"), l.Users.NameAttribute)
	checkNoCtrl(ldapPath.Child("users", "mailAttribute"), l.Users.MailAttribute)
	if g := l.Groups; g != nil {
		checkNoCtrl(ldapPath.Child("groups", "treeDN"), g.TreeDN)
		checkNoCtrl(ldapPath.Child("groups", "filter"), g.Filter)
		checkNoCtrl(ldapPath.Child("groups", "objectClass"), g.ObjectClass)
		checkNoCtrl(ldapPath.Child("groups", "idAttribute"), g.IDAttribute)
		checkNoCtrl(ldapPath.Child("groups", "nameAttribute"), g.NameAttribute)
		checkNoCtrl(ldapPath.Child("groups", "memberAttribute"), g.MemberAttribute)
	}

	// The bind Secret's data keys are fixed by contract ("username" = bind
	// DN, "password"), so a key override is meaningless and almost certainly
	// a misunderstanding of the contract — reject it loudly instead of
	// silently ignoring it.
	if l.BindCredentialsSecretRef.Key != "" {
		errs = append(errs, field.Invalid(
			ldapPath.Child("bindCredentialsSecretRef", "key"),
			l.BindCredentialsSecretRef.Key,
			`key must be empty: the bind Secret's data keys are fixed ("username" and "password")`,
		))
	}

	// Same fixed-key contract for the CA bundle Secret ("ca.crt").
	if l.TLS != nil && l.TLS.CABundleSecretRef.Key != "" {
		errs = append(errs, field.Invalid(
			ldapPath.Child("tls", "caBundleSecretRef", "key"),
			l.TLS.CABundleSecretRef.Key,
			`key must be empty: the CA bundle Secret's data key is fixed ("ca.crt")`,
		))
	}

	return errs
}

// extraOptionKeyPattern is the allowlist every spec.extraOptions key must
// match. keystone/oslo.config [ldap] option names are snake_case, so letters,
// digits, and underscores are the full legitimate charset. See
// validateExtraOptions for the two key-side attacks this closes that the
// value-only guards miss.
var extraOptionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// validateExtraOptions rejects extraOptions keys the projection owns: options
// rendered from typed fields, the driver/domain-config wiring, empty keys,
// keys whose shape could inject INI lines or evade the denylist, and — when
// readOnly is true (nil counts as true, the documented default) — the
// write-enabling user_allow_*/group_allow_* options.
func (w *KeystoneIdentityBackendWebhook) validateExtraOptions(optsPath *field.Path, b *KeystoneIdentityBackend) field.ErrorList {
	var errs field.ErrorList
	if len(b.Spec.ExtraOptions) == 0 {
		return nil
	}

	readOnly := b.Spec.LDAP == nil || b.Spec.LDAP.ReadOnly == nil || *b.Spec.LDAP.ReadOnly

	// Sort keys so the aggregated error list is deterministic.
	keys := make([]string, 0, len(b.Spec.ExtraOptions))
	for k := range b.Spec.ExtraOptions {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if k == "" {
			errs = append(errs, field.Invalid(optsPath, k, "option name must not be empty"))
			continue
		}
		// Enforce the key allowlist before the denylist and forced-option
		// checks, both of which match exact strings and so are blind to two
		// key-side attacks:
		//   - a newline in the key injects an arbitrary [ldap] line through
		//     RenderINI's verbatim `key = value` write — the exact INI
		//     injection the readOnly forcing and the denylist exist to prevent,
		//     just moved from the value to the key;
		//   - a denylist-evading variant such as a trailing space
		//     ("user_allow_create ") is not string-equal to the denylisted or
		//     forced option, so it slips past both exact-match checks, yet
		//     oslo.config strips it to a duplicate that overrides the forced
		//     false value.
		if !extraOptionKeyPattern.MatchString(k) {
			errs = append(errs, field.Invalid(
				optsPath.Key(k), k,
				"option name must match ^[A-Za-z0-9_]+$ (letters, digits, and underscore)",
			))
			continue
		}
		if owner, denied := ldapExtraOptionsDenylist[k]; denied {
			errs = append(errs, field.Invalid(
				optsPath.Key(k),
				b.Spec.ExtraOptions[k],
				fmt.Sprintf("option %q is owned by %s and must not be set via extraOptions", k, owner),
			))
			continue
		}
		if readOnly {
			for _, forced := range ReadOnlyForcedOptions {
				if k == forced {
					errs = append(errs, field.Invalid(
						optsPath.Key(k),
						b.Spec.ExtraOptions[k],
						fmt.Sprintf("option %q conflicts with readOnly: true (the default), which forces it to false", k),
					))
					break
				}
			}
		}
		// INI-injection guard: a value with an embedded newline injects
		// arbitrary [ldap] lines (e.g. re-enabling the write options readOnly
		// forces off) regardless of how innocuous the key is.
		if hasControlChars(b.Spec.ExtraOptions[k]) {
			errs = append(errs, field.Invalid(
				optsPath.Key(k),
				b.Spec.ExtraOptions[k],
				"value must not contain newline or carriage-return characters",
			))
		}
	}
	return errs
}

// validateDomainUniqueness rejects a backend whose domain name collides
// (case-insensitively) with another live backend attached to the same
// Keystone CR. Two backends projecting the same keystone.<domain>.conf file
// would fight over one domain; the webhook is the primary guard and the
// sub-reconciler keeps a defensive skip for CRs that bypassed it.
func (w *KeystoneIdentityBackendWebhook) validateDomainUniqueness(ctx context.Context, namePath *field.Path, b *KeystoneIdentityBackend) field.ErrorList {
	// Skip when no lookup client is injected (e.g. direct unit invocation
	// without a reader) — mirrors the PriorityClass validator's behavior.
	if w.Client == nil {
		return nil
	}

	var siblings KeystoneIdentityBackendList
	if err := w.Client.List(ctx, &siblings, client.InNamespace(b.Namespace)); err != nil {
		return field.ErrorList{field.InternalError(namePath,
			fmt.Errorf("listing KeystoneIdentityBackends for the domain-name uniqueness check: %w", err))}
	}

	for i := range siblings.Items {
		other := &siblings.Items[i]
		if other.Name == b.Name {
			// Self on UPDATE.
			continue
		}
		if other.DeletionTimestamp != nil {
			// A Terminating sibling is on its way out; blocking a
			// replacement on it would deadlock recreate-during-teardown.
			continue
		}
		if other.Spec.KeystoneRef.Name != b.Spec.KeystoneRef.Name {
			continue
		}
		if strings.EqualFold(other.Spec.Domain.Name, b.Spec.Domain.Name) {
			return field.ErrorList{field.Invalid(
				namePath,
				b.Spec.Domain.Name,
				fmt.Sprintf("domain name collides with KeystoneIdentityBackend %q attached to the same Keystone %q (comparison is case-insensitive)",
					other.Name, b.Spec.KeystoneRef.Name),
			)}
		}
	}
	return nil
}
