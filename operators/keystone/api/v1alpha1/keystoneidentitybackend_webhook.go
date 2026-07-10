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
	if o := obj.Spec.OIDC; o != nil {
		if o.ProtocolID == "" {
			o.ProtocolID = DefaultOIDCProtocolID
		}
		if o.IdentityProviderName == "" {
			o.IdentityProviderName = obj.Name
		}
		if o.RemoteIDAttribute == "" {
			o.RemoteIDAttribute = DefaultOIDCRemoteIDAttribute
		}
		if len(o.Scopes) == 0 {
			o.Scopes = append([]string(nil), DefaultOIDCScopes...)
		}
		if o.ResponseType == "" {
			o.ResponseType = DefaultOIDCResponseType
		}
		if o.SessionType == "" {
			o.SessionType = OIDCSessionTypeClientCookie
		}
		if o.StateInputHeaders == "" {
			o.StateInputHeaders = OIDCStateInputHeadersNone
		}
		// When neither discovery shape is set, derive the metadata URL from
		// the issuer (the OIDC discovery convention). The trailing slash is
		// trimmed so "https://idp/realms/x/" and "https://idp/realms/x"
		// derive the same document URL.
		if o.ProviderMetadataURL == "" && o.Endpoints == nil && o.Issuer != "" {
			o.ProviderMetadataURL = strings.TrimRight(o.Issuer, "/") + "/.well-known/openid-configuration"
		}
	}
	if s := obj.Spec.SAML; s != nil {
		if s.ProtocolID == "" {
			s.ProtocolID = DefaultSAMLProtocolID
		}
		if s.IdentityProviderName == "" {
			s.IdentityProviderName = obj.Name
		}
		if s.RemoteIDAttribute == "" {
			s.RemoteIDAttribute = DefaultSAMLRemoteIDAttribute
		}
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
//
//nolint:gosec // G101 false positive: [ldap] option names and spec field paths, not credentials.
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

	// Defense-in-depth union checks alongside the spec-level CEL rules:
	// exactly one backend block, matching spec.type.
	if (b.Spec.Type == IdentityBackendTypeLDAP) != (b.Spec.LDAP != nil) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("ldap"),
			b.Spec.Type,
			"exactly one backend block matching spec.type must be set (type LDAP requires spec.ldap)",
		))
	}
	if (b.Spec.Type == IdentityBackendTypeOIDC) != (b.Spec.OIDC != nil) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("oidc"),
			b.Spec.Type,
			"exactly one backend block matching spec.type must be set (type OIDC requires spec.oidc)",
		))
	}
	if (b.Spec.Type == IdentityBackendTypeSAML) != (b.Spec.SAML != nil) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("saml"),
			b.Spec.Type,
			"exactly one backend block matching spec.type must be set (type SAML requires spec.saml)",
		))
	}

	// mappings/groups are federation vocabulary (OIDC or SAML); extraOptions is
	// documented [ldap] vocabulary. Both are type-gated (defense-in-depth
	// beside the spec-level CEL rules).
	if len(b.Spec.Mappings) > 0 && b.Spec.Type == IdentityBackendTypeLDAP {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("mappings"), b.Spec.Type,
			"mappings are only supported on federation backends (type OIDC or SAML)",
		))
	}
	if len(b.Spec.Groups) > 0 && b.Spec.Type == IdentityBackendTypeLDAP {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("groups"), b.Spec.Type,
			"groups are only supported on federation backends (type OIDC or SAML)",
		))
	}
	if len(b.Spec.ExtraOptions) > 0 && b.Spec.Type != IdentityBackendTypeLDAP {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("extraOptions"), b.Spec.Type,
			"extraOptions carries [ldap] section options and is only supported on type LDAP",
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
	if b.Spec.OIDC != nil {
		allErrs = append(allErrs, w.validateOIDC(specPath.Child("oidc"), b.Spec.OIDC)...)
	}
	if b.Spec.SAML != nil {
		allErrs = append(allErrs, w.validateSAML(specPath.Child("saml"), b.Spec.SAML)...)
	}

	allErrs = append(allErrs, w.validateMappings(specPath.Child("mappings"), b.Spec.Mappings)...)
	allErrs = append(allErrs, w.validateGroups(specPath.Child("groups"), b.Spec.Groups)...)
	allErrs = append(allErrs, w.validateExtraOptions(specPath.Child("extraOptions"), b)...)
	allErrs = append(allErrs, w.validateSiblingBackends(ctx, specPath, b)...)

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

// validateOIDC checks the OIDC block: URL schemes (defense-in-depth alongside
// the Pattern markers), discovery-shape exclusivity, control characters in
// every value rendered into the Apache proxy config or the metadata JSON
// (config-injection guard, mirroring the LDAP INI guard), and the fixed
// data-key contract on the client Secret reference.
func (w *KeystoneIdentityBackendWebhook) validateOIDC(oidcPath *field.Path, o *OIDCBackendSpec) field.ErrorList {
	var errs field.ErrorList

	checkScheme := func(path *field.Path, value string) {
		if value != "" && !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
			errs = append(errs, field.Invalid(path, value,
				"url must use the http:// or https:// scheme"))
		}
	}
	checkScheme(oidcPath.Child("issuer"), o.Issuer)
	if o.Issuer == "" {
		errs = append(errs, field.Required(oidcPath.Child("issuer"), "issuer must be set"))
	}
	checkScheme(oidcPath.Child("providerMetadataURL"), o.ProviderMetadataURL)

	// Defense-in-depth discovery-shape exclusivity beside the CEL rule.
	if o.ProviderMetadataURL != "" && o.Endpoints != nil {
		errs = append(errs, field.Invalid(
			oidcPath.Child("endpoints"), "",
			"providerMetadataURL and endpoints are mutually exclusive",
		))
	}

	// Config-injection guard: every value rendered into the Apache proxy conf
	// or the metadata JSON must not carry newline/carriage-return characters.
	// The Pattern markers are start-anchored only, so the webhook is the
	// primary gate; the renderer revalidates as the last line of defense.
	checkNoCtrl := func(path *field.Path, value string) {
		if hasControlChars(value) || strings.ContainsAny(value, `"`) {
			errs = append(errs, field.Invalid(path, value,
				"value must not contain newline, carriage-return, or double-quote characters"))
		}
	}
	checkNoCtrl(oidcPath.Child("issuer"), o.Issuer)
	checkNoCtrl(oidcPath.Child("providerMetadataURL"), o.ProviderMetadataURL)
	checkNoCtrl(oidcPath.Child("clientID"), o.ClientID)
	checkNoCtrl(oidcPath.Child("protocolID"), o.ProtocolID)
	checkNoCtrl(oidcPath.Child("identityProviderName"), o.IdentityProviderName)
	checkNoCtrl(oidcPath.Child("remoteIDAttribute"), o.RemoteIDAttribute)
	checkNoCtrl(oidcPath.Child("responseType"), o.ResponseType)
	for i, scope := range o.Scopes {
		checkNoCtrl(oidcPath.Child("scopes").Index(i), scope)
	}
	if e := o.Endpoints; e != nil {
		endpointsPath := oidcPath.Child("endpoints")
		for _, ep := range []struct {
			name  string
			value string
		}{
			{"authorizationEndpoint", e.AuthorizationEndpoint},
			{"tokenEndpoint", e.TokenEndpoint},
			{"jwksURI", e.JWKSURI},
			{"userinfoEndpoint", e.UserinfoEndpoint},
			{"endSessionEndpoint", e.EndSessionEndpoint},
			{"introspectionEndpoint", e.IntrospectionEndpoint},
		} {
			checkScheme(endpointsPath.Child(ep.name), ep.value)
			checkNoCtrl(endpointsPath.Child(ep.name), ep.value)
		}
		// Bearer introspection needs the endpoint when discovery is explicit;
		// with metadata-driven discovery it comes from the document.
		if o.OAuth2Introspection != nil && o.OAuth2Introspection.Enabled {
			switch {
			case e.IntrospectionEndpoint == "":
				errs = append(errs, field.Required(
					endpointsPath.Child("introspectionEndpoint"),
					"introspectionEndpoint must be set when oauth2Introspection is enabled with explicit endpoints",
				))
			case !strings.HasPrefix(e.IntrospectionEndpoint, "https://"):
				// mod_auth_openidc's OIDCOAuthIntrospectionEndpoint is
				// https-only at Apache config-parse time — an http endpoint
				// would crash-loop the sidecar, so reject it at admission
				// when knowable (the render re-checks for metadata-derived
				// endpoints).
				errs = append(errs, field.Invalid(
					endpointsPath.Child("introspectionEndpoint"), e.IntrospectionEndpoint,
					"introspectionEndpoint must use https:// (mod_auth_openidc rejects http introspection endpoints)",
				))
			}
		}
	}

	// The client Secret's data key is fixed by contract ("clientSecret"),
	// mirroring the LDAP bind Secret contract.
	if o.ClientSecretRef.Key != "" {
		errs = append(errs, field.Invalid(
			oidcPath.Child("clientSecretRef", "key"),
			o.ClientSecretRef.Key,
			`key must be empty: the client Secret's data key is fixed ("clientSecret")`,
		))
	}

	return errs
}

// samlReservedSections enumerates the keystone.conf section names the config
// renderer (reconcile_config.go) owns. A SAML backend's protocolID becomes a
// [<protocolID>] section (carrying the per-protocol remote_id_attribute), so it
// must not collide with any of these or the render would clobber an
// operator-owned section. Keep this in sync with reconcileConfig's defaults map.
var samlReservedSections = map[string]struct{}{
	"DEFAULT": {}, "database": {}, "cache": {}, "memcache": {}, "identity": {},
	"token": {}, "fernet_tokens": {}, "credential": {}, "auth": {},
	"federation": {}, "openid": {}, "paste_deploy": {},
	"oslo_middleware": {}, "oslo_policy": {},
}

// samlRemoteIDAttributePattern requires the HTTP_ prefix: the mellon env var
// crosses the sidecar → uWSGI HTTP hop as a request header, so the WSGI environ
// key keystone reads must be header-conveyable.
var samlRemoteIDAttributePattern = regexp.MustCompile(`^HTTP_[A-Za-z0-9_]+$`)

// samlForwardAttributePattern is the allowlist for forwarded assertion
// attribute names: they render into Apache RequestHeader directives, so the
// charset guard closes the same injection vector the mapping remote-type guard
// closes.
var samlForwardAttributePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// validateSAML checks the SAML block: idpEntityID presence, control characters
// in every value rendered into the Apache proxy config or keystone.conf, the
// header-conveyable remoteIDAttribute contract, the protocolID section-collision
// guard, forwardAttributes charset/uniqueness, the exactly-one metadata source
// (defense-in-depth beside the CEL rule), URL scheme, the fixed data-key
// contracts on the metadata and SP-certificate Secret references, and — when
// the metadata is inline — that its entityID matches spec.saml.idpEntityID.
func (w *KeystoneIdentityBackendWebhook) validateSAML(samlPath *field.Path, s *SAMLBackendSpec) field.ErrorList {
	var errs field.ErrorList

	if s.IdPEntityID == "" {
		errs = append(errs, field.Required(samlPath.Child("idpEntityID"), "idpEntityID must be set"))
	}

	// Config-injection guard: every value rendered into the Apache proxy conf
	// or keystone.conf must not carry newline/carriage-return/double-quote. The
	// renderer revalidates as the last line of defense.
	checkNoCtrl := func(path *field.Path, value string) {
		if hasControlChars(value) || strings.ContainsAny(value, `"`) {
			errs = append(errs, field.Invalid(path, value,
				"value must not contain newline, carriage-return, or double-quote characters"))
		}
	}
	checkNoCtrl(samlPath.Child("idpEntityID"), s.IdPEntityID)
	checkNoCtrl(samlPath.Child("protocolID"), s.ProtocolID)
	checkNoCtrl(samlPath.Child("identityProviderName"), s.IdentityProviderName)
	checkNoCtrl(samlPath.Child("remoteIDAttribute"), s.RemoteIDAttribute)

	if s.RemoteIDAttribute != "" && !samlRemoteIDAttributePattern.MatchString(s.RemoteIDAttribute) {
		errs = append(errs, field.Invalid(samlPath.Child("remoteIDAttribute"), s.RemoteIDAttribute,
			"remoteIDAttribute must match ^HTTP_[A-Za-z0-9_]+$ (it is conveyed to keystone as a request header across the sidecar hop)"))
	}

	if _, reserved := samlReservedSections[s.ProtocolID]; reserved {
		errs = append(errs, field.Invalid(samlPath.Child("protocolID"), s.ProtocolID,
			fmt.Sprintf("protocolID %q collides with the operator-owned keystone.conf section of the same name", s.ProtocolID)))
	}

	seenAttr := map[string]struct{}{}
	for i, attr := range s.ForwardAttributes {
		attrPath := samlPath.Child("forwardAttributes").Index(i)
		if !samlForwardAttributePattern.MatchString(attr) {
			errs = append(errs, field.Invalid(attrPath, attr,
				"forwardAttributes entry must match ^[A-Za-z0-9_]+$ (it renders into Apache header directives)"))
			continue
		}
		if _, dup := seenAttr[attr]; dup {
			errs = append(errs, field.Duplicate(attrPath, attr))
			continue
		}
		seenAttr[attr] = struct{}{}
	}

	// Exactly-one metadata source (defense-in-depth beside the CEL rule).
	m := s.IdPMetadata
	sources := 0
	if m.Inline != "" {
		sources++
	}
	if m.SecretRef != nil {
		sources++
	}
	if m.URL != "" {
		sources++
	}
	if sources != 1 {
		errs = append(errs, field.Invalid(samlPath.Child("idpMetadata"), sources,
			"exactly one of inline, secretRef, or url must be set"))
	}
	if m.URL != "" && !strings.HasPrefix(m.URL, "https://") {
		errs = append(errs, field.Invalid(samlPath.Child("idpMetadata", "url"), m.URL,
			"url must use https:// (the IdP metadata carries the assertion-signing certificate; a plaintext fetch lets an on-path attacker substitute the trust anchor)"))
	}
	if m.SecretRef != nil && m.SecretRef.Key != "" {
		errs = append(errs, field.Invalid(samlPath.Child("idpMetadata", "secretRef", "key"), m.SecretRef.Key,
			`key must be empty: the metadata Secret's data key is fixed ("idp-metadata.xml")`))
	}
	if m.Inline != "" {
		if entityID, err := SAMLEntityIDFromMetadata([]byte(m.Inline)); err != nil {
			errs = append(errs, field.Invalid(samlPath.Child("idpMetadata", "inline"), "<xml>",
				fmt.Sprintf("inline IdP metadata is not a single SAML EntityDescriptor: %v", err)))
		} else if s.IdPEntityID != "" && entityID != s.IdPEntityID {
			errs = append(errs, field.Invalid(samlPath.Child("idpMetadata", "inline"), entityID,
				fmt.Sprintf("inline IdP metadata entityID %q does not match spec.saml.idpEntityID %q", entityID, s.IdPEntityID)))
		}
	}

	// The SP certificate Secret's data keys are fixed by contract
	// ("tls.crt"/"tls.key"), mirroring the LDAP/OIDC Secret contracts.
	if s.SP != nil && s.SP.CertificateSecretRef != nil && s.SP.CertificateSecretRef.Key != "" {
		errs = append(errs, field.Invalid(samlPath.Child("sp", "certificateSecretRef", "key"), s.SP.CertificateSecretRef.Key,
			`key must be empty: the SP certificate Secret's data keys are fixed ("tls.crt" and "tls.key")`))
	}

	return errs
}

// validateMappings checks the mapping rules: every rule needs at least one
// local and one remote entry, every remote matcher needs a non-empty type,
// and remote types must be header-safe — they render into the proxy's
// RequestHeader-unset directives, so the control-char guard closes the same
// injection vector the LDAP INI guard closes.
func (w *KeystoneIdentityBackendWebhook) validateMappings(mappingsPath *field.Path, rules []MappingRuleSpec) field.ErrorList {
	var errs field.ErrorList
	for i := range rules {
		rulePath := mappingsPath.Index(i)
		if len(rules[i].Local) == 0 {
			errs = append(errs, field.Required(rulePath.Child("local"),
				"every mapping rule needs at least one local entry"))
		}
		if len(rules[i].Remote) == 0 {
			errs = append(errs, field.Required(rulePath.Child("remote"),
				"every mapping rule needs at least one remote entry"))
		}
		for j := range rules[i].Remote {
			remote := &rules[i].Remote[j]
			typePath := rulePath.Child("remote").Index(j).Child("type")
			if remote.Type == "" {
				errs = append(errs, field.Required(typePath, "remote.type must be set"))
				continue
			}
			if !remoteTypePattern.MatchString(remote.Type) {
				errs = append(errs, field.Invalid(typePath, remote.Type,
					"remote.type must match ^[A-Za-z0-9_-]+$ (it renders into Apache header directives)"))
			}
		}
	}
	return errs
}

// remoteTypePattern is the allowlist for mapping remote types: WSGI environ
// keys (HTTP_OIDC_*) are upper snake_case; the pattern additionally admits
// dashes for robustness. Anything else could inject Apache directives via the
// generated RequestHeader-unset lines.
var remoteTypePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validateGroups checks the declarative group targets: names must be set
// (schema-guarded, re-checked for bypass) and every role assignment must
// scope to exactly one of a project or the domain (defense-in-depth beside
// the CEL rule on FederationRoleAssignmentSpec).
func (w *KeystoneIdentityBackendWebhook) validateGroups(groupsPath *field.Path, groups []FederationGroupSpec) field.ErrorList {
	var errs field.ErrorList
	seen := map[string]struct{}{}
	for i := range groups {
		groupPath := groupsPath.Index(i)
		if groups[i].Name == "" {
			errs = append(errs, field.Required(groupPath.Child("name"), "group name must be set"))
		} else if _, dup := seen[groups[i].Name]; dup {
			errs = append(errs, field.Duplicate(groupPath.Child("name"), groups[i].Name))
		} else {
			seen[groups[i].Name] = struct{}{}
		}
		for j := range groups[i].RoleAssignments {
			ra := &groups[i].RoleAssignments[j]
			raPath := groupPath.Child("roleAssignments").Index(j)
			if ra.Role == "" {
				errs = append(errs, field.Required(raPath.Child("role"), "role must be set"))
			}
			if (ra.Project != nil) == ra.Domain {
				errs = append(errs, field.Invalid(raPath, ra.Role,
					"exactly one of project or domain must be set"))
			}
		}
	}
	return errs
}

// validateSiblingBackends runs every cross-CR rule over one namespace-scoped
// List of sibling backends:
//
//   - domain-name uniqueness (case-insensitive) per referenced Keystone — two
//     backends projecting the same keystone.<domain>.conf would fight over
//     one domain;
//   - identityProviderName uniqueness across every federation sibling (OIDC and
//     SAML) of one Keystone — the name is a path segment of the federation API
//     objects and the protected websso/auth Locations;
//   - at most one SAML sibling per referenced Keystone — mod_auth_mellon's SP
//     configuration projects onto a shared /v3 parent Location, so a second
//     SAML backend cannot coexist;
//   - remoteIDAttribute uniformity across the OIDC siblings of one Keystone —
//     it renders into the single [openid] section of keystone.conf;
//   - at most one OIDC sibling with oauth2Introspection enabled —
//     mod_auth_openidc's OIDCOAuth* resource-server directives are
//     server-scoped, so a second introspection backend would silently shadow
//     the first.
//
// The webhook is the primary guard; the sub-reconciler keeps defensive skips
// for CRs that bypassed it.
func (w *KeystoneIdentityBackendWebhook) validateSiblingBackends(ctx context.Context, specPath *field.Path, b *KeystoneIdentityBackend) field.ErrorList {
	// Skip when no lookup client is injected (e.g. direct unit invocation
	// without a reader) — mirrors the PriorityClass validator's behavior.
	if w.Client == nil {
		return nil
	}

	namePath := specPath.Child("domain", "name")
	var siblings KeystoneIdentityBackendList
	if err := w.Client.List(ctx, &siblings, client.InNamespace(b.Namespace)); err != nil {
		return field.ErrorList{field.InternalError(namePath,
			fmt.Errorf("listing KeystoneIdentityBackends for the cross-backend checks: %w", err))}
	}

	var errs field.ErrorList
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
			errs = append(errs, field.Invalid(
				namePath,
				b.Spec.Domain.Name,
				fmt.Sprintf("domain name collides with KeystoneIdentityBackend %q attached to the same Keystone %q (comparison is case-insensitive)",
					other.Name, b.Spec.KeystoneRef.Name),
			))
		}
		// The identity provider name is a keystone-global path segment shared by
		// OIDC and SAML federation objects, so a collision between ANY two
		// federation siblings of one Keystone is rejected (not just OIDC pairs).
		if b.IsFederationType() && other.IsFederationType() &&
			b.EffectiveIdentityProviderName() == other.EffectiveIdentityProviderName() {
			idpNamePath := specPath.Child("oidc", "identityProviderName")
			if b.Spec.SAML != nil {
				idpNamePath = specPath.Child("saml", "identityProviderName")
			}
			errs = append(errs, field.Invalid(
				idpNamePath,
				b.EffectiveIdentityProviderName(),
				fmt.Sprintf("identity provider name collides with KeystoneIdentityBackend %q attached to the same Keystone %q",
					other.Name, b.Spec.KeystoneRef.Name),
			))
		}

		// At most one SAML backend per referenced Keystone: mod_auth_mellon's SP
		// configuration projects onto the shared /v3 parent Location, so a second
		// SAML backend cannot coexist (server-scoped like OIDCOAuth*).
		if b.Spec.SAML != nil && other.Spec.SAML != nil {
			errs = append(errs, field.Invalid(
				specPath.Child("saml"),
				b.Spec.Type,
				fmt.Sprintf("at most one SAML backend per Keystone %q is supported (the mod_auth_mellon SP configuration projects onto a shared /v3 parent Location); KeystoneIdentityBackend %q already provides one",
					b.Spec.KeystoneRef.Name, other.Name),
			))
		}

		// The remaining rules are OIDC-pair-specific.
		if b.Spec.OIDC == nil || other.Spec.OIDC == nil {
			continue
		}
		if b.EffectiveRemoteIDAttribute() != other.EffectiveRemoteIDAttribute() {
			errs = append(errs, field.Invalid(
				specPath.Child("oidc", "remoteIDAttribute"),
				b.EffectiveRemoteIDAttribute(),
				fmt.Sprintf("remoteIDAttribute must be uniform across all OIDC backends of Keystone %q (it renders into the single [openid] section); KeystoneIdentityBackend %q uses %q",
					b.Spec.KeystoneRef.Name, other.Name, other.EffectiveRemoteIDAttribute()),
			))
		}
		if b.Spec.OIDC.OAuth2Introspection != nil && b.Spec.OIDC.OAuth2Introspection.Enabled &&
			other.Spec.OIDC.OAuth2Introspection != nil && other.Spec.OIDC.OAuth2Introspection.Enabled {
			errs = append(errs, field.Invalid(
				specPath.Child("oidc", "oauth2Introspection", "enabled"),
				true,
				fmt.Sprintf("at most one OIDC backend per Keystone may enable oauth2Introspection (mod_auth_openidc's OIDCOAuth* directives are server-scoped); KeystoneIdentityBackend %q already enables it",
					other.Name),
			))
		}
	}
	return errs
}
