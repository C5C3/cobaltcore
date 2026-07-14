// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/forge/internal/common/policy"
	"github.com/c5c3/forge/internal/common/release"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/validation"
)

// ControlPlane defaulting constants. These are the single source of
// truth shared by the defaulting webhook and (where relevant) the validation
// error messages, so the defaults cannot drift across call sites. The matching
// +kubebuilder:default markers on the spec fields remain as defense-in-depth
// for callers that bypass this webhook (e.g. envtest without the defaulter
// wired up) — kubebuilder markers require literals and cannot reference these
// Go constants.
const (
	// DefaultRegion is materialized when spec.region is empty (plan decision #4).
	DefaultRegion = "RegionOne"
	// DefaultCloudCredentialsSecretName is materialized when
	// spec.korc.adminCredential.cloudCredentialsRef.secretName is empty.
	DefaultCloudCredentialsSecretName = "k-orc-clouds-yaml" //nolint:gosec // G101 false positive: Secret name, not a credential
	// well-known defaults for the database, cache, and admin-credential
	// fields so a minimal managed-mode ControlPlane can omit spec.infrastructure
	// and the spec.korc.adminCredential body. The shared commonv1 leaves
	// (DatabaseSpec, CacheSpec, SecretRefSpec) are defaulted webhook-only — never
	// via a +kubebuilder:default marker — because the keystone operator reuses
	// those types and a c5c3-specific default would leak.
	//
	// DefaultDatabaseName is materialized when spec.infrastructure.database.database is empty.
	DefaultDatabaseName = "keystone"
	// DefaultDatabaseSecretName is materialized when spec.infrastructure.database.secretRef.name is empty.
	DefaultDatabaseSecretName = "keystone-db" //nolint:gosec // G101 false positive: Secret name, not a credential
	// DefaultDatabaseClusterRefName is the managed MariaDB CR name materialized when
	// spec.infrastructure.database is in managed mode (host unset).
	DefaultDatabaseClusterRefName = "openstack-db"
	// DefaultCacheBackend is materialized when spec.infrastructure.cache.backend
	// is empty. It aliases commonv1.DefaultCacheBackend so the keystone and c5c3
	// operators share one source of truth for the cache backend default.
	DefaultCacheBackend = commonv1.DefaultCacheBackend
	// DefaultCacheClusterRefName is the managed Memcached CR name materialized when
	// spec.infrastructure.cache is in managed mode (servers unset).
	DefaultCacheClusterRefName = "openstack-memcached"
	// The dedicated-backing-service clusterRef names are derived from the
	// ControlPlane's own name so a per-service instance never collides with the
	// shared one (openstack-db / openstack-memcached) nor with another
	// ControlPlane's instance in a shared namespace. They are materialized when a
	// dedicated block declares a managed instance without naming it.
	//
	// DedicatedKeystoneDatabaseClusterRefSuffix names the MariaDB CR of a
	// dedicated Keystone database.
	DedicatedKeystoneDatabaseClusterRefSuffix = "-keystone-db" //nolint:gosec // G101 false positive: CR name suffix, not a credential
	// DedicatedKeystoneCacheClusterRefSuffix names the Memcached CR of a dedicated
	// Keystone cache.
	DedicatedKeystoneCacheClusterRefSuffix = "-keystone-cache"
	// DedicatedHorizonCacheClusterRefSuffix names the Memcached CR of a dedicated
	// Horizon cache.
	DedicatedHorizonCacheClusterRefSuffix = "-horizon-cache"
	// DefaultDatabaseStorageSize is the effective per-replica MariaDB volume size
	// when spec.infrastructure.database.storageSize is empty. It aliases
	// commonv1.DatabaseStorageSizeDefault (also the CRD +kubebuilder:default and
	// the c5c3 fresh-create fallback) so validateImmutable normalizes an empty
	// stored value to the exact size the live MariaDB already uses. StorageSize is
	// defaulted by the CRD marker rather than Default() below, so this constant is
	// only consulted by the immutability check, not materialized onto the object.
	DefaultDatabaseStorageSize = commonv1.DatabaseStorageSizeDefault
	// DefaultAdminPasswordSecretName is materialized when
	// spec.korc.adminCredential.passwordSecretRef.name is empty.
	DefaultAdminPasswordSecretName = "keystone-admin" //nolint:gosec // G101 false positive: Secret name, not a credential
	// DefaultAdminPasswordSecretKey is materialized when
	// spec.korc.adminCredential.passwordSecretRef.key is empty. Unlike the Secret
	// *name* constants above (which carry a //nolint:gosec G101 false-positive
	// annotation), "password" is the Secret data KEY — the field name within the
	// Secret (SecretRefSpec.Key), not credential material — so it correctly needs
	// no G101 nolint.
	DefaultAdminPasswordSecretKey = "password"
	// DefaultCloudName is materialized when
	// spec.korc.adminCredential.cloudCredentialsRef.cloudName is empty.
	DefaultCloudName = "admin"
	// DefaultExternalEndpointType is materialized when
	// spec.services.keystone.external.endpointType is empty. It mirrors the
	// +kubebuilder:default=public marker on ExternalKeystoneSpec.EndpointType.
	DefaultExternalEndpointType = ExternalEndpointTypePublic
	// DefaultCABundleSecretKey is materialized when
	// spec.services.keystone.external.caBundleSecretRef.key is empty. It is
	// webhook-only because the shared SecretRefSpec carries no c5c3-specific
	// marker (the same discipline as passwordSecretRef.Key). "ca.crt" matches the
	// PEM key K-ORC reads inline from the credentials Secret.
	DefaultCABundleSecretKey = "ca.crt"
	// DefaultAdminUserName is materialized when
	// spec.korc.adminCredential.userName is empty. Webhook-only: the field carries
	// a +kubebuilder:default=admin marker for the normal admission path.
	DefaultAdminUserName = "admin"
	// DefaultAdminProjectName is materialized when
	// spec.korc.adminCredential.projectName is empty (mirrors the CRD default).
	DefaultAdminProjectName = "admin"
	// DefaultAdminDomainName is materialized when
	// spec.korc.adminCredential.domainName is empty (mirrors the CRD default). The
	// single domain feeds both user_domain_name and project_domain_name in the
	// generated clouds.yaml.
	DefaultAdminDomainName = "Default"
)

// controlPlaneReleaseRegexp mirrors the +kubebuilder:validation:Pattern marker
// on ControlPlaneSpec.OpenStackRelease. The [12] minor class matches the
// two-releases-per-year OpenStack cadence that release.ParseRelease also
// enforces, so validate() rejects a non-cadence minor (e.g. 2025.9) instead of
// letting validateReleaseNotDowngraded silently skip the downgrade check for a
// value ParseRelease cannot parse. The validating webhook re-checks the pattern
// as defense-in-depth for callers that bypass CRD schema admission.
var controlPlaneReleaseRegexp = regexp.MustCompile(`^\d{4}\.[12]$`)

// maxExternalAuthURLBytes mirrors the +kubebuilder:validation:MaxLength=2048 marker
// on ExternalKeystoneSpec.AuthURL. The cap exists because the reconciler
// interpolates authURL into status.conditions[].message, whose 32768-byte apiserver
// limit is a whole-object constraint — see the marker's doc comment. It is applied
// at the authURL call site rather than inside validateHTTPURL: the cap belongs to
// this one field, and the helper's other callers carry MaxLength markers of their
// own (512 on services.horizon.publicEndpoint).
const maxExternalAuthURLBytes = 2048

// maxCatalogEndpointURLBytes mirrors the MaxLength marker on
// ExternalCatalogEndpointSpec.URL, which in turn mirrors K-ORC's own
// EndpointResourceSpec.URL cap. A URL admitted here can therefore never be
// rejected downstream by the K-ORC CRD.
const maxCatalogEndpointURLBytes = 1024

// validateHTTPURL enforces that raw is a well-formed absolute HTTP(S) URL with a
// host, going beyond the coarse ^https?:// CRD Pattern markers: the unusable
// shapes (missing host, non-http(s) scheme, opaque or relative references,
// control characters) are rejected at admission rather than wedging the
// reconciler that consumes them. This is a shape gate, not an SSRF control —
// admission cannot resolve where the host points, so a dialing reconciler must
// still enforce network egress restrictions. It returns the parsed URL so
// callers can apply further per-field rules (byte caps, path/query checks)
// without re-parsing.
func validateHTTPURL(path *field.Path, raw string) (*url.URL, *field.Error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, field.Invalid(path, raw, "must be a valid http(s) URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, field.Invalid(path, raw, "must be an http(s) URL (scheme http or https)")
	case u.Host == "":
		return nil, field.Invalid(path, raw, "must include a host")
	}
	return u, nil
}

// validateHorizonPublicEndpoint enforces the rules on
// services.horizon.publicEndpoint that the CRD markers cannot express. The
// reconciler derives the dashboard's WebSSO origin from the value
// (publicEndpoint + "/auth/websso/") and projects it onto the Keystone child's
// [federation] trusted_dashboard.
//
//   - Shape, as defense-in-depth alongside the ^https?:// Pattern marker:
//     Keystone compares the origin verbatim, so a value that parses to no host
//     could never match any dashboard.
//   - A bare origin, with no path, query or fragment. The Pattern marker anchors
//     only the prefix, so "https://horizon.example.com?utm=1" is schema-legal,
//     survives the URL parse, agrees with gateway.hostname, and yields the
//     nonsense trusted origin "https://horizon.example.com?utm=1/auth/websso/" —
//     which Keystone's own validation accepts and then never matches, failing
//     every federated login AFTER the user authenticated at the IdP, with no
//     status, log, or admission error naming the cause. A path fails the same
//     way: Django derives the origin it sends from the request Host header and
//     mounts the dashboard at the root unless FORCE_SCRIPT_NAME is configured,
//     which this operator does not manage.
//   - With a gateway configured the listener terminates TLS, so the
//     browser-observed scheme is https — the same rule the Keystone CR applies
//     to its bootstrap.publicEndpoint. An http origin is also a token leak:
//     after the IdP authenticates the user, Keystone POSTs the unscoped WebSSO
//     token to this origin, and that bearer token grants the user's full API
//     privileges until it expires.
//   - With a gateway configured the host must equal gateway.hostname. Django
//     derives the origin it sends to Keystone from the request's Host header —
//     i.e. from gateway.hostname, never from this field — so a divergent host
//     produces an origin Keystone rejects, and it rejects it only AFTER the
//     user has already entered their corporate credentials at the IdP. The port
//     may still differ: Gateway API hostnames carry none, so a dashboard
//     published off 443 has to spell the port out here.
func validateHorizonPublicEndpoint(specPath *field.Path, hz *ServiceHorizonSpec) field.ErrorList {
	if hz == nil || hz.PublicEndpoint == "" {
		return nil
	}
	pePath := specPath.Child("services", "horizon", "publicEndpoint")
	u, err := validateHTTPURL(pePath, hz.PublicEndpoint)
	if err != nil {
		return field.ErrorList{err}
	}

	var errs field.ErrorList
	// A single trailing slash is the one path the reconciler tolerates:
	// horizonPublicEndpoint trims it before appending "/auth/websso/".
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, field.Invalid(pePath, hz.PublicEndpoint,
			"must be a bare origin (scheme://host[:port]) with no path, query, or fragment: the WebSSO origin is "+
				`derived as publicEndpoint+"/auth/websso/" and Keystone compares it verbatim`))
	}

	g := hz.Gateway
	if g == nil || g.Hostname == "" {
		return errs
	}

	if u.Scheme != "https" {
		errs = append(errs, field.Invalid(pePath, hz.PublicEndpoint,
			"scheme must be https when services.horizon.gateway is configured (the Gateway listener terminates TLS): "+
				"Keystone POSTs the unscoped WebSSO token to this origin, so http would deliver a bearer token in cleartext"))
	}
	if u.Hostname() != g.Hostname {
		errs = append(errs, field.Invalid(pePath, hz.PublicEndpoint,
			fmt.Sprintf("host %q must equal services.horizon.gateway.hostname %q: the dashboard derives the WebSSO "+
				"origin it sends from the request Host header, and Keystone compares it verbatim",
				u.Hostname(), g.Hostname)))
	}
	return errs
}

// warnInsecureHorizonPublicEndpoint surfaces a cleartext WebSSO hand-off that
// validateHorizonPublicEndpoint cannot reject: without a gateway the dashboard
// is published by some other means, and a plain-http origin is a legal, if
// unwise, development setup that the ^https?:// CRD Pattern deliberately allows.
// The downgrade must never be silent, though — the token Keystone POSTs to this
// origin is readable by any on-path observer and grants the user's full API
// privileges, not just dashboard access.
func warnInsecureHorizonPublicEndpoint(cp *ControlPlane) admission.Warnings {
	hz := cp.Spec.Services.Horizon
	if hz == nil || hz.PublicEndpoint == "" {
		return nil
	}
	if u, err := url.Parse(hz.PublicEndpoint); err != nil || u.Scheme != "http" {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.services.horizon.publicEndpoint %q uses http://: the WebSSO origin derived from it is projected onto "+
			"the Keystone child's trusted_dashboard, and Keystone POSTs the unscoped WebSSO token to that origin, so "+
			"every federated login would deliver a bearer token in cleartext. Use https://.",
		hz.PublicEndpoint,
	)}
}

// maxGatewayHostnameLen is the maximum length of a DNS name (RFC 1035). The
// commonv1.GatewaySpec.Hostname marker is MinLength=1 only, so admission would
// otherwise accept a hostname long enough to overrun the children's own
// MaxLength markers on the origins derived from it — see validateGatewayHostname.
const maxGatewayHostnameLen = 253

// validateGatewayHostname enforces that a services.<svc>.gateway.hostname is a
// concrete, port-free DNS name of usable length. The CRD marker on
// commonv1.GatewaySpec.Hostname is MinLength=1 only, but the reconciler derives
// BROWSER-facing origins from the value ("https://"+hostname) and projects them
// onto the children: the Keystone child's [federation] trusted_dashboard, which
// Keystone compares against the dashboard's origin byte-for-byte, and the
// Horizon child's websso.keystoneURL. Four shapes the reconciler cannot use
// pass every other gate:
//
//   - A control character (a pasted newline) survives the children's
//     ^https?:// Pattern markers — RE2 anchors ^ at start-of-text, not
//     start-of-line — and is caught only by the child's own webhook, so a typo
//     in a horizon field would take the healthy Keystone projection down with
//     an error naming neither the field nor the ControlPlane.
//   - A Gateway API wildcard ("*.example.com") is a legal HTTPRoute hostname
//     but yields a trusted origin that matches no dashboard, silently breaking
//     WebSSO forever.
//   - An embedded port is forbidden by Gateway API for the same field, and
//     would be carried into the origin verbatim.
//   - An over-long hostname overruns the children's MaxLength markers on the
//     derived origins (512 on both trustedDashboards[] and websso.keystoneURL),
//     so the API server rejects a child the operator never wrote.
//
// Rejecting them here surfaces the error on the ControlPlane the operator
// actually edits.
func validateGatewayHostname(path *field.Path, hostname string) *field.Error {
	u, err := url.Parse("https://" + hostname)
	switch {
	case err != nil || u.Host != hostname:
		return field.Invalid(path, hostname, "must be a bare DNS hostname")
	case strings.Contains(hostname, "*"):
		return field.Invalid(path, hostname,
			"must not be a wildcard hostname: the derived WebSSO origin is compared verbatim and would match no dashboard")
	case u.Port() != "":
		return field.Invalid(path, hostname,
			"must not include a port: set services.horizon.publicEndpoint to publish the dashboard on a non-default port")
	case len(hostname) > maxGatewayHostnameLen:
		return field.Invalid(path, hostname,
			fmt.Sprintf("must be at most %d characters (the maximum DNS name length)", maxGatewayHostnameLen))
	}
	return nil
}

// catalogEntryTypePattern mirrors the Pattern marker on
// ExternalCatalogEntrySpec.Type. The type is embedded verbatim in the names of
// the child K-ORC CRs, so it must be a DNS-1123 label.
var catalogEntryTypePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const (
	// maxManagedCatalogEntries mirrors the MaxItems marker on
	// ExternalCatalogSpec.ManagedEntries. It bounds how many K-ORC CRs — and
	// therefore how many writes against a third-party production Keystone — one
	// ControlPlane admission can amplify into.
	maxManagedCatalogEntries = 32

	// maxObjectNameBytes is the apiserver's cap on metadata.name. The entry type is
	// embedded in the names of the child K-ORC CRs, and nothing bounds the
	// ControlPlane's own name below 253, so the composed child name can overflow a
	// CR that admission already accepted.
	maxObjectNameBytes = 253

	// catalogEntryChildNameOverhead is the longest fixed part of a child catalog
	// Endpoint name, "{cp}-catalog-{type}-{interface}", i.e. everything except the
	// ControlPlane name and the entry type. "internal" is the longest interface.
	catalogEntryChildNameOverhead = len("-catalog-") + len("-internal")

	// identityImportChildNameOverhead is the longest fixed part of an identity
	// Endpoint import name, "{cp}-identity-endpoint-{interface}". It exceeds
	// catalogEntryChildNameOverhead, and External mode creates those imports
	// unconditionally — with or without a catalog block — so the guard hangs off the
	// mode rather than off the managedEntries opt-in.
	identityImportChildNameOverhead = len("-identity-endpoint") + len("-internal")
)

// validateExternalCatalog mirrors the declarative constraints on
// ExternalCatalogSpec as defense-in-depth for callers that bypass CRD schema
// admission: the CEL rule forbidding an `identity` managed entry, the Pattern /
// MinLength markers on identityServiceName and on the entry type and name, the
// MaxItems cap on the entry list, the listType=map uniqueness of entry types and
// endpoint interfaces, and the enum and URL shape of every endpoint.
//
// It also enforces the one rule the CRD schema cannot express: that the child
// K-ORC CR name composed from cpName and the entry type stays inside the
// apiserver's 253-byte metadata.name cap. Without it a CR admission accepts wedges
// the reconcile in an exponential backoff on an Invalid the operator never asked
// for.
//
// Creating catalog entries against a pre-existing installation is the one
// destructive thing External mode can do, so every rule guarding the opt-in is
// enforced twice.
func validateExternalCatalog(path *field.Path, cpName string, catalog *ExternalCatalogSpec) field.ErrorList {
	var allErrs field.ErrorList

	// K-ORC casts identityServiceName to its OpenStackName on the Service import
	// filter, whose Pattern rejects a comma — exactly as it does the entry name below.
	if strings.Contains(catalog.IdentityServiceName, ",") {
		allErrs = append(allErrs, field.Invalid(path.Child("identityServiceName"), catalog.IdentityServiceName,
			"must not contain a comma (mirrors K-ORC's OpenStackName pattern ^[^,]+$)"))
	}

	entriesPath := path.Child("managedEntries")
	if len(catalog.ManagedEntries) > maxManagedCatalogEntries {
		allErrs = append(allErrs, field.TooMany(entriesPath, len(catalog.ManagedEntries), maxManagedCatalogEntries))
	}

	seenTypes := make(map[string]struct{}, len(catalog.ManagedEntries))
	for i, entry := range catalog.ManagedEntries {
		entryPath := entriesPath.Index(i)
		typePath := entryPath.Child("type")

		switch {
		case entry.Type == "":
			allErrs = append(allErrs, field.Required(typePath, "must be set"))
		case entry.Type == IdentityCatalogServiceType:
			allErrs = append(allErrs, field.Forbidden(typePath,
				"the identity catalog entry is owned by the External-mode imports and must not be declared as a managed entry"))
		case !catalogEntryTypePattern.MatchString(entry.Type):
			allErrs = append(allErrs, field.Invalid(typePath, entry.Type,
				"must be a lowercase alphanumeric DNS-1123 label (it names the child K-ORC CRs)"))
		}
		if _, dup := seenTypes[entry.Type]; dup {
			allErrs = append(allErrs, field.Duplicate(typePath, entry.Type))
		}
		seenTypes[entry.Type] = struct{}{}

		if entry.Type != "" {
			if n := len(cpName) + catalogEntryChildNameOverhead + len(entry.Type); n > maxObjectNameBytes {
				allErrs = append(allErrs, field.Invalid(typePath, entry.Type, fmt.Sprintf(
					"the child K-ORC CR name would be %d bytes; shorten the ControlPlane name or the entry type "+
						"so the total stays within the %d-byte Kubernetes object-name limit", n, maxObjectNameBytes,
				)))
			}
		}

		// K-ORC casts the name to its OpenStackName, whose Pattern rejects a comma.
		if strings.Contains(entry.Name, ",") {
			allErrs = append(allErrs, field.Invalid(entryPath.Child("name"), entry.Name,
				"must not contain a comma (mirrors K-ORC's OpenStackName pattern ^[^,]+$)"))
		}

		seenInterfaces := make(map[ExternalEndpointType]struct{}, len(entry.Endpoints))
		for j, ep := range entry.Endpoints {
			epPath := entryPath.Child("endpoints").Index(j)
			switch ep.Interface {
			case ExternalEndpointTypePublic, ExternalEndpointTypeInternal, ExternalEndpointTypeAdmin:
			case "":
				allErrs = append(allErrs, field.Required(epPath.Child("interface"), "must be set"))
			default:
				// The interface reaches a child CR name, which must stay a DNS-1123
				// subdomain: an off-enum value the CRD would have rejected wedges the
				// reconcile rather than failing at admission.
				allErrs = append(allErrs, field.NotSupported(epPath.Child("interface"), ep.Interface,
					[]ExternalEndpointType{
						ExternalEndpointTypePublic, ExternalEndpointTypeInternal, ExternalEndpointTypeAdmin,
					}))
			}
			if _, dup := seenInterfaces[ep.Interface]; dup {
				allErrs = append(allErrs, field.Duplicate(epPath.Child("interface"), ep.Interface))
			}
			seenInterfaces[ep.Interface] = struct{}{}

			if _, err := validateHTTPURL(epPath.Child("url"), ep.URL); err != nil {
				allErrs = append(allErrs, err)
			} else if len(ep.URL) > maxCatalogEndpointURLBytes {
				allErrs = append(allErrs, field.Invalid(epPath.Child("url"), ep.URL,
					fmt.Sprintf("must be at most %d bytes", maxCatalogEndpointURLBytes)))
			}
		}
	}

	return allErrs
}

// serviceAccountNamePattern mirrors the Pattern marker on ServiceAccountSpec.Name.
// The account name keys the list and is embedded verbatim in the names of the
// child K-ORC CRs and Secrets, so it must be a DNS-1123 label.
var serviceAccountNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const (
	// maxServiceAccounts mirrors the MaxItems marker on KORCSpec.ServiceAccounts.
	maxServiceAccounts = 32

	// serviceAccountChildNameOverhead is the longest fixed part of a child name
	// the account name is embedded in — everything except the ControlPlane name
	// and the account name. The password Secret
	// "{cp}-service-account-{name}-password-vN" carries the longest fixed affix;
	// the "vN" generation suffix is bounded generously at 10 digits so a name that
	// admission accepts can never overflow a child CR / Secret the reconciler then
	// wedges on.
	serviceAccountChildNameOverhead = len("-service-account-") + len("-password-v") + 10
)

// effectiveServiceAccountUserName resolves the OpenStack user name for an entry:
// the explicit userName, or the account name (the defaulting webhook materializes
// this, but the resolver falls back for webhook-bypassed callers). It mirrors the
// reconciler's serviceAccountUserName so admission and reconcile agree on identity.
func effectiveServiceAccountUserName(sa ServiceAccountSpec) string {
	return cmp.Or(sa.UserName, sa.Name)
}

// effectiveServiceAccountDomain resolves the OpenStack domain for an entry: the
// explicit domainName, else the effective admin domain. It mirrors the
// reconciler's serviceAccountDomainName.
func effectiveServiceAccountDomain(cp *ControlPlane, sa ServiceAccountSpec) string {
	return cmp.Or(sa.DomainName, effectiveAdminDomain(cp))
}

// effectiveAdminUserName / effectiveAdminDomain resolve the admin identity with
// the same fallbacks the reconciler's adminUserName / adminDomainName apply, so
// the collision check compares against the identity the AC actually mints as.
func effectiveAdminUserName(cp *ControlPlane) string {
	return cmp.Or(cp.Spec.KORC.AdminCredential.UserName, DefaultAdminUserName)
}

func effectiveAdminDomain(cp *ControlPlane) string {
	return cmp.Or(cp.Spec.KORC.AdminCredential.DomainName, DefaultAdminDomainName)
}

// validateServiceAccounts mirrors the declarative constraints on
// KORCSpec.ServiceAccounts as defense-in-depth for callers that bypass CRD schema
// admission (the account-name / userName / domainName / project.name shapes, the
// MaxItems cap, the child-CR name-length bound the CRD cannot express) AND adds
// the cross-item rules CEL cannot carry:
//
//   - two entries must not resolve to the same (userName, domainName): they would
//     project two managed Users onto one Keystone user and race its password;
//   - no entry's effective identity may equal the effective admin identity: a
//     managed User would take over the admin user and rotate ITS password;
//   - two create:true entries must not name the same project in the same domain:
//     each managed Project would adopt the other's Keystone row.
func validateServiceAccounts(cp *ControlPlane) field.ErrorList {
	sas := cp.Spec.KORC.ServiceAccounts
	if len(sas) == 0 {
		return nil
	}
	var allErrs field.ErrorList
	basePath := field.NewPath("spec", "korc", "serviceAccounts")

	if len(sas) > maxServiceAccounts {
		allErrs = append(allErrs, field.TooMany(basePath, len(sas), maxServiceAccounts))
	}

	adminUser := effectiveAdminUserName(cp)
	adminDomain := effectiveAdminDomain(cp)

	seenNames := make(map[string]struct{}, len(sas))
	seenIdentities := make(map[string]struct{}, len(sas))
	seenManagedProjects := make(map[string]struct{}, len(sas))
	for i := range sas {
		sa := sas[i]
		entryPath := basePath.Index(i)

		switch {
		case sa.Name == "":
			allErrs = append(allErrs, field.Required(entryPath.Child("name"), "must be set"))
		case !serviceAccountNamePattern.MatchString(sa.Name):
			allErrs = append(allErrs, field.Invalid(entryPath.Child("name"), sa.Name,
				"must be a lowercase alphanumeric DNS-1123 label (it names the child K-ORC CRs and Secrets)"))
		}
		if _, dup := seenNames[sa.Name]; dup {
			allErrs = append(allErrs, field.Duplicate(entryPath.Child("name"), sa.Name))
		}
		seenNames[sa.Name] = struct{}{}

		if sa.Name != "" {
			if n := len(cp.Name) + serviceAccountChildNameOverhead + len(sa.Name); n > maxObjectNameBytes {
				allErrs = append(allErrs, field.Invalid(entryPath.Child("name"), sa.Name, fmt.Sprintf(
					"the child K-ORC CR name would be %d bytes; shorten the ControlPlane name or the account "+
						"name so the total stays within the %d-byte Kubernetes object-name limit", n, maxObjectNameBytes,
				)))
			}
		}

		// K-ORC casts userName/domainName to OpenStackName, whose Pattern rejects a comma.
		if strings.Contains(sa.UserName, ",") {
			allErrs = append(allErrs, field.Invalid(entryPath.Child("userName"), sa.UserName,
				"must not contain a comma (mirrors K-ORC's OpenStackName pattern ^[^,]+$)"))
		}
		if strings.Contains(sa.DomainName, ",") {
			allErrs = append(allErrs, field.Invalid(entryPath.Child("domainName"), sa.DomainName,
				"must not contain a comma (mirrors K-ORC's OpenStackName pattern ^[^,]+$)"))
		}

		projPath := entryPath.Child("project")
		switch {
		case sa.Project.Name == "":
			allErrs = append(allErrs, field.Required(projPath.Child("name"), "must be set"))
		case strings.Contains(sa.Project.Name, ","):
			allErrs = append(allErrs, field.Invalid(projPath.Child("name"), sa.Project.Name,
				"must not contain a comma (mirrors K-ORC's KeystoneName pattern ^[^,]+$)"))
		}

		for j, role := range sa.Roles {
			if strings.Contains(role, ",") {
				allErrs = append(allErrs, field.Invalid(entryPath.Child("roles").Index(j), role,
					"must not contain a comma (mirrors K-ORC's OpenStackName pattern ^[^,]+$)"))
			}
		}

		user := effectiveServiceAccountUserName(sa)
		domain := effectiveServiceAccountDomain(cp, sa)
		identity := user + "\x00" + domain
		if _, dup := seenIdentities[identity]; dup {
			allErrs = append(allErrs, field.Duplicate(entryPath.Child("userName"), user))
		}
		seenIdentities[identity] = struct{}{}

		if user == adminUser && domain == adminDomain {
			allErrs = append(allErrs, field.Invalid(entryPath.Child("userName"), user, fmt.Sprintf(
				"the effective service-account identity (user %q in domain %q) equals the admin identity "+
					"(spec.korc.adminCredential.userName / domainName); a managed User would take over the admin "+
					"user and rotate its password", user, domain,
			)))
		}

		if sa.Project.Create && sa.Project.Name != "" {
			key := sa.Project.Name + "\x00" + domain
			if _, dup := seenManagedProjects[key]; dup {
				allErrs = append(allErrs, field.Duplicate(projPath.Child("name"), sa.Project.Name))
			}
			seenManagedProjects[key] = struct{}{}
		}
	}
	return allErrs
}

// externalAuthURLIsPlaintext reports whether raw is an http:// (non-TLS) endpoint.
// A parse failure reads as false: validateHTTPURL already rejects those on the same
// field, and a second error on it would only add noise.
func externalAuthURLIsPlaintext(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http"
}

// ControlPlaneWebhook implements defaulting and validation webhooks for the
// ControlPlane CRD. Client is injected at startup and used by
// ValidateCreate to enforce one ControlPlane per namespace.
// Production wiring injects mgr.GetAPIReader() — a direct, uncached reader —
// so concurrent or cache-sync-window CREATEs cannot both pass the check
// against an empty informer cache.
// +kubebuilder:object:generate=false
type ControlPlaneWebhook struct {
	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*ControlPlane] = &ControlPlaneWebhook{}
	_ admission.Validator[*ControlPlane] = &ControlPlaneWebhook{}
)

// +kubebuilder:webhook:path=/mutate-c5c3-io-v1alpha1-controlplane,mutating=true,failurePolicy=fail,sideEffects=None,groups=c5c3.io,resources=controlplanes,verbs=create;update,versions=v1alpha1,name=mcontrolplane.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-c5c3-io-v1alpha1-controlplane,mutating=false,failurePolicy=fail,sideEffects=None,groups=c5c3.io,resources=controlplanes,verbs=create;update,versions=v1alpha1,name=vcontrolplane.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *ControlPlaneWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*ControlPlane](mgr, &ControlPlane{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// defaultDatabaseLeaves materializes the well-known leaves of a DatabaseSpec —
// the logical database name, the credential Secret name, and, in MANAGED mode
// only, the clusterRef naming the MariaDB CR. clusterRefName is the managed CR
// name to invent when the block names none: the shared, well-known
// DefaultDatabaseClusterRefName for spec.infrastructure, and a ControlPlane-
// derived name for a per-service dedicated instance.
//
// The managed clusterRef is invented only when the brownfield discriminator
// (host) is unset, so an explicit brownfield endpoint is never coerced into
// managed mode and the database XOR check still passes. Idempotent: only zero
// values are filled.
func defaultDatabaseLeaves(db *commonv1.DatabaseSpec, clusterRefName string) {
	if db.Database == "" {
		db.Database = DefaultDatabaseName
	}
	if db.SecretRef.Name == "" {
		db.SecretRef.Name = DefaultDatabaseSecretName
	}
	if db.Host == "" {
		if db.ClusterRef == nil {
			db.ClusterRef = &corev1.LocalObjectReference{Name: clusterRefName}
		} else if db.ClusterRef.Name == "" {
			db.ClusterRef.Name = clusterRefName
		}
	}
}

// defaultCacheLeaves materializes the well-known leaves of a CacheSpec — the
// oslo.cache backend and, in MANAGED mode only, the clusterRef naming the
// Memcached CR — with the same brownfield-preserving discipline as
// defaultDatabaseLeaves (see there).
func defaultCacheLeaves(cache *commonv1.CacheSpec, clusterRefName string) {
	if cache.Backend == "" {
		cache.Backend = DefaultCacheBackend
	}
	if len(cache.Servers) == 0 {
		if cache.ClusterRef == nil {
			cache.ClusterRef = &corev1.LocalObjectReference{Name: clusterRefName}
		} else if cache.ClusterRef.Name == "" {
			cache.ClusterRef.Name = clusterRefName
		}
	}
}

// Default implements admission.Defaulter[*ControlPlane].
// It fills only zero-valued fields with their documented defaults, leaving any
// explicit value untouched. It is idempotent: applying it twice produces the
// same result.
func (w *ControlPlaneWebhook) Default(_ context.Context, obj *ControlPlane) error {
	// Plan decision #4: region defaults to RegionOne.
	if obj.Spec.Region == "" {
		obj.Spec.Region = DefaultRegion
	}

	// Default the keystone mode to Managed when the service block is present with
	// an empty mode, so IsExternalKeystone() reads a definite discriminator below.
	// Mirrors the +kubebuilder:default=Managed marker on ServiceKeystoneSpec.Mode.
	if ks := obj.Spec.Services.Keystone; ks != nil && ks.Mode == "" {
		ks.Mode = KeystoneModeManaged
	}

	// A per-service namespace assignment defaults to the Managed lifecycle — the
	// operator creates, owns, and deletes the namespace. Defaulted only on a
	// DECLARED block: an absent assignment means "stay in the ControlPlane's
	// namespace", and the webhook must never invent a placement. Mirrors the
	// +kubebuilder:default=Managed marker on ServiceNamespaceSpec.Lifecycle.
	for _, ns := range []*ServiceNamespaceSpec{keystoneNamespaceBlock(obj), horizonNamespaceBlock(obj)} {
		if ns != nil && ns.Lifecycle == "" {
			ns.Lifecycle = ServiceNamespaceLifecycleManaged
		}
	}

	if obj.IsExternalKeystone() {
		// External mode: the ControlPlane manages identity against a pre-existing
		// Keystone and provisions NO backing services, so the infrastructure
		// defaulting below is deliberately skipped — the webhook never invents a
		// managed database/cache clusterRef (spec.infrastructure stays nil and the
		// validating webhook forbids it in External mode). Only the external block's
		// own defaults are materialized here.
		if ext := obj.Spec.Services.Keystone.External; ext != nil {
			if ext.EndpointType == "" {
				ext.EndpointType = DefaultExternalEndpointType
			}
			if ext.CABundleSecretRef != nil && ext.CABundleSecretRef.Key == "" {
				ext.CABundleSecretRef.Key = DefaultCABundleSecretKey
			}
		}
	} else {
		// Managed mode (or unset keystone): well-known infrastructure defaults so a
		// minimal managed-mode CR can omit spec.infrastructure entirely. The
		// mode-neutral leaves (database name, secretRef.name, cache backend) are
		// defaulted in BOTH managed and brownfield mode; the managed clusterRef is
		// only invented when the brownfield discriminator (database.host /
		// cache.servers) is unset, so the validating webhook's database/cache XOR
		// check still passes for a brownfield CR — the webhook never coerces an
		// explicit brownfield endpoint into managed mode. Materialize an empty block
		// when nil so the leaf defaulting preserves today's omit-infrastructure
		// contract.
		if obj.Spec.Infrastructure == nil {
			obj.Spec.Infrastructure = &InfrastructureSpec{}
		}
		defaultDatabaseLeaves(&obj.Spec.Infrastructure.Database, DefaultDatabaseClusterRefName)
		defaultCacheLeaves(&obj.Spec.Infrastructure.Cache, DefaultCacheClusterRefName)

		// Per-service DEDICATED backing services take the same leaf defaults as the
		// shared block — the same helpers, so a dedicated instance can never drift
		// from the shared one on what an omitted leaf means — but derive their
		// managed clusterRef name from the ControlPlane so they never collide with
		// the shared instance. A dedicated block is only defaulted when the operator
		// declared it: absent means "share the ControlPlane-wide instances", and the
		// webhook must never invent an opt-in.
		if ks := keystoneDedicatedBlock(obj); ks != nil {
			if db := ks.Database; db != nil {
				defaultDatabaseLeaves(db, obj.Name+DedicatedKeystoneDatabaseClusterRefSuffix)
				// A dedicated MANAGED database is Static-only: the OpenBao
				// database-engine connection is bootstrapped once per namespace against
				// the SHARED cluster, so no engine role can issue credentials for a
				// dedicated instance. Materialize the mode so the stored spec states the
				// contract the reconciler applies; validate() rejects an explicit Dynamic.
				if db.ClusterRef != nil && db.CredentialsMode == "" {
					db.CredentialsMode = commonv1.CredentialsModeStatic
				}
			}
			if cache := ks.Cache; cache != nil {
				defaultCacheLeaves(cache, obj.Name+DedicatedKeystoneCacheClusterRefSuffix)
			}
		}
		if hz := horizonDedicatedBlock(obj); hz != nil {
			if cache := hz.Cache; cache != nil {
				defaultCacheLeaves(cache, obj.Name+DedicatedHorizonCacheClusterRefSuffix)
			}
		}
	}

	// K-ORC admin-credential defaults. cloudCredentialsRef.secretName defaults to
	// the documented shared Secret name.
	korc := &obj.Spec.KORC.AdminCredential
	if korc.CloudCredentialsRef.SecretName == "" {
		korc.CloudCredentialsRef.SecretName = DefaultCloudCredentialsSecretName
	}
	// cloudCredentialsRef.cloudName defaults to the conventional admin
	// cloud entry; passwordSecretRef.name/.key default to the conventional admin
	// Secret and its data key. Defaulting .key makes the stored spec explicit and
	// consistent with the reconciler's existing readAdminPassword "password"
	// fallback. These are webhook-only (no marker on the shared commonv1 types).
	if korc.CloudCredentialsRef.CloudName == "" {
		korc.CloudCredentialsRef.CloudName = DefaultCloudName
	}
	if korc.PasswordSecretRef.Name == "" {
		korc.PasswordSecretRef.Name = DefaultAdminPasswordSecretName
	}
	if korc.PasswordSecretRef.Key == "" {
		korc.PasswordSecretRef.Key = DefaultAdminPasswordSecretKey
	}
	// admin identity: userName/projectName default to "admin", domainName to
	// "Default" — the three identities a stock Keystone bootstrap creates. Valid
	// in both keystone modes; consumed by the K-ORC clouds.yaml builders and the
	// admin import filters. Webhook-only mirror of the CRD markers.
	if korc.UserName == "" {
		korc.UserName = DefaultAdminUserName
	}
	if korc.ProjectName == "" {
		korc.ProjectName = DefaultAdminProjectName
	}
	if korc.DomainName == "" {
		korc.DomainName = DefaultAdminDomainName
	}

	// applicationCredential.restricted defaults to true (least-privilege). The
	// pointer lets us distinguish "unset" (nil → default true) from an explicit
	// false, which we must preserve.
	appCred := &korc.ApplicationCredential
	if appCred.Restricted == nil {
		restricted := true
		appCred.Restricted = &restricted
	}

	// applicationCredential.rotation.mode defaults to PasswordDriven.
	if appCred.Rotation.Mode == "" {
		appCred.Rotation.Mode = RotationModePasswordDriven
	}

	// serviceAccounts[i].userName defaults to serviceAccounts[i].name: the K-ORC
	// User CR is named after the account, but the OpenStack user name it manages
	// defaults to the same value unless the operator overrides it. Mirrors the
	// cloudName/secretName defaulting discipline. (The domain default is resolved
	// in the reconciler, not here, so it can follow spec.korc.adminCredential.
	// domainName.)
	for i := range obj.Spec.KORC.ServiceAccounts {
		if obj.Spec.KORC.ServiceAccounts[i].UserName == "" {
			obj.Spec.KORC.ServiceAccounts[i].UserName = obj.Spec.KORC.ServiceAccounts[i].Name
		}
	}

	return nil
}

// ValidateCreate implements admission.Validator[*ControlPlane].
// DECISION boundary 6 = option (a): in addition to the spec
// checks in validate(), a CREATE is rejected when another ControlPlane already
// exists in the same namespace. The per-CR OpenBao credential paths (admin AC,
// bootstrap admin password, fernet/credential keys) are scoped by namespace+name
// and the CredentialRotation reconciler resolves its target by listing
// ControlPlanes in the namespace and expecting exactly one; enforcing
// one-ControlPlane-per-namespace at admission keeps that resolution unambiguous.
// The check runs only on CREATE (not UPDATE) so an existing CR stays mutable.
// Reviewer: please verify boundary 6 = option (a).
func (w *ControlPlaneWebhook) ValidateCreate(ctx context.Context, obj *ControlPlane) (admission.Warnings, error) {
	warnings := warnInsecureHorizonPublicEndpoint(obj)
	if err := newInvalidIfErrs(obj, w.validate(obj)); err != nil {
		return warnings, err
	}
	if err := w.validateUniqueInNamespace(ctx, obj); err != nil {
		return warnings, err
	}
	return warnings, w.validateNamespaceClaims(ctx, obj)
}

// ValidateUpdate implements admission.Validator[*ControlPlane].
// In addition to the spec checks in validate(), it enforces the create-only
// immutable fields between oldObj and newObj (validateImmutable): flipping the
// database/cache mode or renaming a managed clusterRef would orphan the
// previously-projected MariaDB/Memcached child (and its credentials), renaming
// cloudCredentialsRef.secretName would leak the previously-projected K-ORC
// clouds.yaml ExternalSecret (#476), and renaming the database or changing the
// region would re-point the projection at the now-immutable Keystone child
// fields and wedge the reconcile loop (#466). It additionally rejects an
// openStackRelease downgrade (validateReleaseNotDowngraded), since Keystone DB
// migrations are forward-only. Spec errors, immutability errors, and the
// downgrade error are accumulated into a single Invalid response so a reviewer
// sees all problems at once.
func (w *ControlPlaneWebhook) ValidateUpdate(_ context.Context, oldObj, newObj *ControlPlane) (admission.Warnings, error) {
	allErrs := w.validate(newObj)
	allErrs = append(allErrs, validateImmutable(oldObj, newObj)...)
	allErrs = append(allErrs, validateReleaseNotDowngraded(oldObj, newObj)...)
	return warnInsecureHorizonPublicEndpoint(newObj), newInvalidIfErrs(newObj, allErrs)
}

// ValidateDelete implements admission.Validator[*ControlPlane]. The method is
// required by the Validator interface but is never invoked: the validating
// webhook registers only create/update (no delete verb), so with
// failurePolicy=Fail a down operator can never block ControlPlane CR — and
// thereby namespace — deletion.
func (w *ControlPlaneWebhook) ValidateDelete(_ context.Context, _ *ControlPlane) (admission.Warnings, error) {
	return nil, nil
}

// validate accumulates all spec validation errors for cp.
// The kubebuilder markers / CEL rules on the CRD are the primary enforcement
// point at admission time; the checks below are defense-in-depth (mirroring the
// KeystoneWebhook discipline) so callers that bypass CRD schema admission still
// get field-specific errors. It returns the accumulated field errors; callers
// wrap them via newInvalidIfErrs so ValidateUpdate can fold in the immutability
// errors before constructing a single Invalid response.
func (w *ControlPlaneWebhook) validate(cp *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// openStackRelease must match the date-based release pattern.
	// Mirrors the +kubebuilder:validation:Pattern marker on
	// ControlPlaneSpec.OpenStackRelease.
	if !controlPlaneReleaseRegexp.MatchString(cp.Spec.OpenStackRelease) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("openStackRelease"),
			cp.Spec.OpenStackRelease,
			"must match the OpenStack release pattern ^\\d{4}\\.[12]$ (e.g. 2025.2)",
		))
	}

	// secretStoreRef is optional (nil defaults to the shared cluster store);
	// when set it must carry a name and a kind in the enum. Defense-in-depth
	// twin of the CRD markers on commonv1.SecretStoreRefSpec.
	allErrs = append(allErrs, validation.SecretStoreRef(specPath.Child("secretStoreRef"), cp.Spec.SecretStoreRef)...)

	// spec.infrastructure is optional at the Go/CRD layer now (External keystone
	// mode omits it), so the database/cache checks only run when the block is
	// present. The mode-conditional required/forbidden rules for the block itself
	// are added with the External-mode validation matrix; here a nil block simply
	// has no database/cache to validate.
	if infra := cp.Spec.Infrastructure; infra != nil {
		// database must use exactly one of clusterRef or host, and CredentialsMode
		// Dynamic (engine-issued credentials) requires managed mode (ClusterRef
		// set) — the shared validators mirroring the CEL rules on the shared
		// commonv1.DatabaseSpec.
		db := infra.Database
		allErrs = append(allErrs, validation.DatabaseXOR(specPath.Child("infrastructure", "database"), &db)...)
		allErrs = append(allErrs, validation.DynamicCredentialsRequireClusterRef(specPath.Child("infrastructure", "database"), &db)...)

		allErrs = append(allErrs, validateDatabaseReplicas(specPath.Child("infrastructure", "database"), &db)...)

		// cache must use exactly one of clusterRef or servers — the shared
		// validator mirroring the CEL rule on the shared commonv1.CacheSpec.
		cache := infra.Cache
		allErrs = append(allErrs, validation.CacheXOR(specPath.Child("infrastructure", "cache"), &cache)...)
	}

	// the K-ORC admin-credential password Secret reference is required —
	// without it the reconciler cannot (re-)mint the admin application credential.
	if cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("korc", "adminCredential", "passwordSecretRef", "name"),
			"passwordSecretRef.name must be set",
		))
	}

	// reject a Keystone rotationInterval the reconciler's intervalToCron
	// cannot represent (only a positive whole number of days — 168h weekly or any
	// positive multiple of 24h daily) so a bad interval is a clean admission error
	// rather than a steady-state KeystoneReady=False with no requeue. Mirrors
	// intervalToCron in internal/controller/helpers.go and is kept in sync as
	// defense-in-depth, exactly like the openStackRelease pattern check above.
	// services.keystone is optional; all per-service checks below only apply when
	// the block is present.
	if ks := cp.Spec.Services.Keystone; ks != nil {
		if ri := ks.RotationInterval; ri != nil {
			if d := ri.Duration; d <= 0 || d%(24*time.Hour) != 0 {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("services", "keystone", "rotationInterval"),
					d.String(),
					"must be a positive whole number of days (e.g. 24h, 168h); only daily and weekly Fernet rotation schedules are supported",
				))
			}
		}

		// When a gateway is configured, its hostname must be set and usable as
		// the host of the derived public endpoint. Mirrors the
		// +kubebuilder:validation:MinLength=1 marker on commonv1.GatewaySpec.Hostname;
		// without it the reconciler derives an empty "https:///v3" public endpoint.
		if g := ks.Gateway; g != nil {
			hostnamePath := specPath.Child("services", "keystone", "gateway", "hostname")
			if g.Hostname == "" {
				allErrs = append(allErrs, field.Required(hostnamePath,
					"must be set when a gateway is configured"))
			} else if err := validateGatewayHostname(hostnamePath, g.Hostname); err != nil {
				allErrs = append(allErrs, err)
			}
		}

		// When the Keystone image is overridden, mirror the ImageSpec tag/digest
		// XOR (the +kubebuilder:validation:XValidation rule on commonv1.ImageSpec)
		// with a defense-in-depth check: exactly one of tag or digest must be set.
		if img := ks.Image; img != nil && (img.Tag != "") == (img.Digest != "") {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("services", "keystone", "image"),
				img,
				"exactly one of image.tag or image.digest must be set",
			))
		}
	}

	// services.horizon is optional; all per-service checks below only apply
	// when the block is present. Policy overrides are N/A for horizon (the
	// dashboard enforces no oslo.policy of its own), so unlike keystone there
	// is no per-service policy block to validate.
	if hz := cp.Spec.Services.Horizon; hz != nil {
		// When a gateway is configured, its hostname must be set and usable as
		// the host of the WebSSO origin derived from it. Mirrors the
		// +kubebuilder:validation:MinLength=1 marker on commonv1.GatewaySpec.Hostname.
		if g := hz.Gateway; g != nil {
			hostnamePath := specPath.Child("services", "horizon", "gateway", "hostname")
			if g.Hostname == "" {
				allErrs = append(allErrs, field.Required(hostnamePath,
					"must be set when a gateway is configured"))
			} else if err := validateGatewayHostname(hostnamePath, g.Hostname); err != nil {
				allErrs = append(allErrs, err)
			}
		}

		allErrs = append(allErrs, validateHorizonPublicEndpoint(specPath, hz)...)

		// When the Horizon image is overridden, mirror the ImageSpec tag/digest
		// XOR (the +kubebuilder:validation:XValidation rule on commonv1.ImageSpec)
		// with a defense-in-depth check: exactly one of tag or digest must be set.
		if img := hz.Image; img != nil && (img.Tag != "") == (img.Digest != "") {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("services", "horizon", "image"),
				img,
				"exactly one of image.tag or image.digest must be set",
			))
		}

		// When the SECRET_KEY Secret is overridden, its name must be non-empty.
		// Mirrors the MinLength marker on commonv1.SecretRefSpec.Name.
		if ref := hz.SecretKeyRef; ref != nil && ref.Name == "" {
			allErrs = append(allErrs, field.Required(
				specPath.Child("services", "horizon", "secretKeyRef", "name"),
				"must be set when secretKeyRef is configured",
			))
		}
	}

	// Reject empty policy rule names and values on both the global policy and the
	// per-service Keystone override. The c5c3 webhook previously validated policy
	// rules not at all; this mirrors the keystone webhook and the CEL rule on
	// commonv1.PolicySpec, closing the empty-value gap the audit reported
	// (issue #479).
	if g := cp.Spec.GlobalPolicyOverrides; g != nil {
		allErrs = append(allErrs, policy.ValidatePolicyRules(
			g.Rules, specPath.Child("globalPolicyOverrides", "rules"),
		)...)
	}
	if ks := cp.Spec.Services.Keystone; ks != nil && ks.PolicyOverrides != nil {
		allErrs = append(allErrs, policy.ValidatePolicyRules(
			ks.PolicyOverrides.Rules, specPath.Child("services", "keystone", "policyOverrides", "rules"),
		)...)
	}

	allErrs = append(allErrs, validateKeystoneMode(cp)...)
	allErrs = append(allErrs, validateServiceAccounts(cp)...)
	allErrs = append(allErrs, validateDedicatedBackingServices(cp)...)
	allErrs = append(allErrs, validateServiceNamespaces(cp)...)

	return allErrs
}

// namespaceNamePattern mirrors the Pattern marker on ServiceNamespaceSpec.Name:
// a namespace name must be an RFC-1123 label.
var namespaceNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// serviceNamespaceAssignment pairs one service's declared namespace block with
// the field path it lives at, so the validators below walk every service
// uniformly and a new service extends the walk rather than reshaping it.
type serviceNamespaceAssignment struct {
	path *field.Path
	ns   *ServiceNamespaceSpec
}

// declaredServiceNamespaces returns the namespace assignments cp actually
// declares, in a stable order. A service that stays in the ControlPlane's
// namespace (the default) contributes nothing.
func declaredServiceNamespaces(cp *ControlPlane) []serviceNamespaceAssignment {
	svcPath := field.NewPath("spec", "services")
	var out []serviceNamespaceAssignment
	if ns := keystoneNamespaceBlock(cp); ns != nil {
		out = append(out, serviceNamespaceAssignment{path: svcPath.Child("keystone", "namespace"), ns: ns})
	}
	if ns := horizonNamespaceBlock(cp); ns != nil {
		out = append(out, serviceNamespaceAssignment{path: svcPath.Child("horizon", "namespace"), ns: ns})
	}
	return out
}

// validateServiceNamespaces enforces the rules on the per-service namespace
// assignments. It mirrors the declarative constraints as defense-in-depth for
// callers that bypass CRD schema admission (the RFC-1123 name shape, the
// lifecycle enum) and adds the two the CRD schema cannot express:
//
//   - An assignment must not name the ControlPlane's OWN namespace. The block is
//     the opt-in to a SEPARATE namespace; naming the current one is a no-op the
//     reconciler would have to special-case at every cross-namespace site, and
//     under the Managed lifecycle it would make the operator claim ownership of —
//     and, at teardown, delete — the namespace the ControlPlane itself lives in.
//   - Two services placed in the SAME namespace must agree on its lifecycle. They
//     share that namespace's backing services and its tenant store, so they cannot
//     disagree on whether the operator owns it: one declaration would have the
//     teardown delete the namespace the other declared untouchable.
//
// The cross-field rule that a namespace assignment is forbidden in External mode
// lives in validateKeystoneMode with the rest of the External-mode matrix.
func validateServiceNamespaces(cp *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList

	lifecycles := map[string]ServiceNamespaceLifecycle{}
	for _, a := range declaredServiceNamespaces(cp) {
		namePath := a.path.Child("name")
		switch {
		case a.ns.Name == "":
			allErrs = append(allErrs, field.Required(namePath, "must be set"))
		case a.ns.Name == cp.Namespace:
			allErrs = append(allErrs, field.Invalid(namePath, a.ns.Name,
				"must differ from the ControlPlane's own namespace; omit the block entirely to place the service "+
					"in the ControlPlane's namespace"))
		case !namespaceNamePattern.MatchString(a.ns.Name):
			allErrs = append(allErrs, field.Invalid(namePath, a.ns.Name,
				"must be a lowercase alphanumeric RFC-1123 label (it names a Kubernetes namespace)"))
		}

		switch a.ns.Lifecycle {
		case ServiceNamespaceLifecycleManaged, ServiceNamespaceLifecycleExternal, "":
		default:
			allErrs = append(allErrs, field.NotSupported(a.path.Child("lifecycle"), a.ns.Lifecycle,
				[]ServiceNamespaceLifecycle{ServiceNamespaceLifecycleManaged, ServiceNamespaceLifecycleExternal}))
		}

		if a.ns.Name == "" {
			continue
		}
		if seen, dup := lifecycles[a.ns.Name]; dup && seen != a.ns.Lifecycle {
			allErrs = append(allErrs, field.Invalid(a.path.Child("lifecycle"), a.ns.Lifecycle, fmt.Sprintf(
				"services co-located in namespace %q must declare the same lifecycle; they share that namespace's "+
					"backing services and secret store, so they cannot disagree on whether the operator owns it",
				a.ns.Name,
			)))
		}
		lifecycles[a.ns.Name] = a.ns.Lifecycle
	}

	return allErrs
}

// validateNamespaceClaims enforces that a namespace belongs to at most ONE
// ControlPlane. The namespace is the tenant key the whole secret stack is scoped
// by — the OpenBao KV paths (bootstrap/{ns}/…), the database-engine role
// (keystone-{ns}), and the templated eso-tenant policy that confines a store's
// token to its own namespace — so two ControlPlanes sharing one namespace would
// share that scope: exactly the isolation the per-tenant store exists to enforce.
// The one-ControlPlane-per-namespace rule (validateUniqueInNamespace) already
// says so for the ControlPlane's own namespace; a service namespace is the same
// tenant key one level out, so it takes the same rule.
//
// Both directions are rejected: a namespace this ControlPlane claims must not be
// another's (own or service) namespace, and this ControlPlane's own namespace
// must not be another's service namespace. The List is cluster-wide through the
// injected uncached API reader — a service namespace can be claimed from any
// namespace, so a namespace-scoped List would miss the claim it must find.
//
// It runs on CREATE only: the assignments are immutable afterwards
// (validateServiceNamespacesImmutable), so no UPDATE can introduce a new claim.
// A nil w.Client skips the check, mirroring validateUniqueInNamespace.
func (w *ControlPlaneWebhook) validateNamespaceClaims(ctx context.Context, obj *ControlPlane) error {
	if w.Client == nil {
		return nil
	}
	claims := declaredServiceNamespaces(obj)
	var existing ControlPlaneList
	if err := w.Client.List(ctx, &existing); err != nil {
		return apierrors.NewInternalError(
			fmt.Errorf("listing ControlPlanes to enforce namespace-claim uniqueness: %w", err),
		)
	}

	var allErrs field.ErrorList
	for i := range existing.Items {
		other := &existing.Items[i]
		if other.UID == obj.UID && other.Namespace == obj.Namespace && other.Name == obj.Name {
			continue
		}
		// Everything the other ControlPlane occupies: its own namespace (whose
		// tenant scope validateUniqueInNamespace already guards) and every service
		// namespace it claims.
		occupied := map[string]struct{}{other.Namespace: {}}
		for _, ns := range other.DedicatedServiceNamespaces() {
			occupied[ns.Name] = struct{}{}
		}

		for _, a := range claims {
			if _, taken := occupied[a.ns.Name]; taken && a.ns.Name != "" {
				allErrs = append(allErrs, field.Invalid(a.path.Child("name"), a.ns.Name, fmt.Sprintf(
					"namespace %q is already occupied by ControlPlane %q in namespace %q; a namespace is the tenant "+
						"key the OpenBao paths and the per-tenant secret store are scoped by, so it belongs to at "+
						"most one ControlPlane",
					a.ns.Name, other.Name, other.Namespace,
				)))
			}
		}

		for _, ns := range other.DedicatedServiceNamespaces() {
			if ns.Name == obj.Namespace {
				allErrs = append(allErrs, field.Forbidden(field.NewPath("metadata", "namespace"), fmt.Sprintf(
					"namespace %q is already claimed as a service namespace by ControlPlane %q in namespace %q; a "+
						"namespace is the tenant key the OpenBao paths and the per-tenant secret store are scoped "+
						"by, so it belongs to at most one ControlPlane",
					obj.Namespace, other.Name, other.Namespace,
				)))
			}
		}
	}

	return newInvalidIfErrs(obj, allErrs)
}

// validateDatabaseReplicas enforces that a managed database's replica count is 1
// (standalone) or >=3 (a quorum-safe Galera cluster). Exactly 2 is rejected
// because the managed-mode MariaDB projection (ensureMariaDB) turns any
// replicas>1 into a Galera cluster, and a two-node Galera cluster cannot hold a
// majority — a single pod disruption (restart, OOM-kill, rolling update, network
// partition) then loses quorum and takes the whole database offline. The CRD
// marker only enforces Minimum=1, so this webhook is the enforcement point; the
// shared commonv1.DatabaseSpec must not carry a c5c3-specific CEL rule the
// keystone operator (which ignores replicas) would also inherit. A zero value
// (CRD/webhook default bypassed) is left to the reconciler's floor, so only an
// explicit 2 is rejected. It applies to the shared database and to every
// dedicated one alike: the projection that makes 2 unsafe is the same.
func validateDatabaseReplicas(fldPath *field.Path, db *commonv1.DatabaseSpec) field.ErrorList {
	if db.Replicas != 2 {
		return nil
	}
	return field.ErrorList{field.Invalid(
		fldPath.Child("replicas"),
		db.Replicas,
		"database replicas must be 1 (standalone) or >=3 (Galera needs quorum); 2 cannot hold a majority",
	)}
}

// dedicatedBackingServices pairs one service's declared dedicated block with the
// field path it lives at, so the validators below walk every service uniformly
// and a new backing-service class or a new service extends the walk rather than
// reshaping it.
type dedicatedBackingServices struct {
	path  *field.Path
	db    *commonv1.DatabaseSpec
	cache *commonv1.CacheSpec
}

// declaredDedicatedBackingServices returns the dedicated blocks cp actually
// declares, in a stable order. A service that shares the ControlPlane-wide
// instances (the default) contributes nothing.
func declaredDedicatedBackingServices(cp *ControlPlane) []dedicatedBackingServices {
	svcPath := field.NewPath("spec", "services")
	var out []dedicatedBackingServices
	if ks := keystoneDedicatedBlock(cp); ks != nil {
		out = append(out, dedicatedBackingServices{
			path:  svcPath.Child("keystone", "dedicatedBackingServices"),
			db:    ks.Database,
			cache: ks.Cache,
		})
	}
	if hz := horizonDedicatedBlock(cp); hz != nil {
		out = append(out, dedicatedBackingServices{
			path:  svcPath.Child("horizon", "dedicatedBackingServices"),
			cache: hz.Cache,
		})
	}
	return out
}

// validateDedicatedBackingServices enforces the rules on the per-service
// dedicated backing-service blocks. It mirrors the declarative constraints as
// defense-in-depth for callers that bypass CRD schema admission (the
// at-least-one-class CEL rule, and the database/cache XORs carried by the shared
// commonv1 types) and adds the rules the CRD schema cannot express:
//
//   - A dedicated MANAGED database may not use credentialsMode Dynamic. The
//     OpenBao database engine has exactly one connection and role per NAMESPACE
//     (deploy/openbao/bootstrap/setup-database-tenant.sh), bootstrapped against
//     the SHARED cluster, so no engine role exists that could issue credentials
//     for a dedicated instance — an admitted Dynamic dedicated database would
//     wedge on an ExternalSecret that can never sync. Static is the supported
//     mode (and the one the defaulting webhook materializes).
//   - Managed clusterRef NAMES must be unique per backing-service class across
//     the shared block and every dedicated instance. Two instances sharing a name
//     resolve to one child CR, which the projections would then fight over —
//     silently voiding the very isolation the opt-in exists for.
//   - The Galera-quorum replicas rule applies to a dedicated database exactly as
//     it does to the shared one.
//
// The cross-field rule that a dedicated block is forbidden in External mode
// lives in validateKeystoneMode with the rest of the External-mode matrix.
func validateDedicatedBackingServices(cp *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList

	// Seed the per-class name sets with the SHARED instances so a dedicated
	// instance colliding with them is caught, not just a dedicated-vs-dedicated
	// collision.
	dbNames := map[string]struct{}{}
	cacheNames := map[string]struct{}{}
	if infra := cp.Spec.Infrastructure; infra != nil {
		if ref := infra.Database.ClusterRef; ref != nil {
			dbNames[ref.Name] = struct{}{}
		}
		if ref := infra.Cache.ClusterRef; ref != nil {
			cacheNames[ref.Name] = struct{}{}
		}
	}

	for _, d := range declaredDedicatedBackingServices(cp) {
		if d.db == nil && d.cache == nil {
			allErrs = append(allErrs, field.Required(d.path,
				"at least one backing-service class must be declared; omit the block entirely to share the "+
					"ControlPlane-wide instances"))
			continue
		}

		if db := d.db; db != nil {
			dbPath := d.path.Child("database")
			allErrs = append(allErrs, validation.DatabaseXOR(dbPath, db)...)
			allErrs = append(allErrs, validateDatabaseReplicas(dbPath, db)...)
			// Strictly stronger than the shared block's Dynamic-requires-clusterRef
			// rule (which the commonv1 CEL rule still carries): Dynamic is rejected on
			// a dedicated database in EITHER mode.
			if db.CredentialsMode == commonv1.CredentialsModeDynamic {
				allErrs = append(allErrs, field.Forbidden(dbPath.Child("credentialsMode"),
					"credentialsMode Dynamic is not supported on a dedicated database: the OpenBao database-engine "+
						"connection is bootstrapped once per namespace against the shared cluster, so no engine role "+
						"issues credentials for a dedicated instance; use Static"))
			}
			if ref := db.ClusterRef; ref != nil {
				if _, dup := dbNames[ref.Name]; dup {
					allErrs = append(allErrs, field.Duplicate(dbPath.Child("clusterRef", "name"), ref.Name))
				}
				dbNames[ref.Name] = struct{}{}
			}
		}

		if cache := d.cache; cache != nil {
			cachePath := d.path.Child("cache")
			allErrs = append(allErrs, validation.CacheXOR(cachePath, cache)...)
			if ref := cache.ClusterRef; ref != nil {
				if _, dup := cacheNames[ref.Name]; dup {
					allErrs = append(allErrs, field.Duplicate(cachePath.Child("clusterRef", "name"), ref.Name))
				}
				cacheNames[ref.Name] = struct{}{}
			}
		}
	}

	return allErrs
}

// validateKeystoneMode enforces the External-mode validation matrix. It mirrors
// the type-level CEL rules on ServiceKeystoneSpec as defense-in-depth for callers
// that bypass CRD schema admission (the same discipline as the release/database
// mirrors above) AND adds the cross-field rules CEL cannot express — the ones
// spanning spec.infrastructure and spec.services.{keystone,horizon}, which live
// in the webhook per the established CEL-vs-webhook split.
//
//   - External mode: metadata.name must leave room for the identity Endpoint
//     import CR names the mode composes from it; services.keystone.external is
//     required (with an http(s) authURL and a non-empty caBundleSecretRef.name when
//     the ref is set); the managed-only Keystone fields (replicas, image,
//     policyOverrides, rotationInterval, gateway, publicEndpoint) are forbidden;
//     spec.infrastructure is forbidden (phase 2 will relax this to optional) and so
//     is services.horizon (P2 — Horizon needs its own External-mode design).
//   - Not External (Managed, unset mode, or unset keystone): services.keystone.external
//     is forbidden and spec.infrastructure is required — preserving today's
//     contract now that the Go field is an optional pointer.
func validateKeystoneMode(cp *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	if cp.IsExternalKeystone() {
		ks := cp.Spec.Services.Keystone
		ksPath := specPath.Child("services", "keystone")

		// External mode imports one identity Endpoint per interface, unconditionally.
		// Nothing bounds the ControlPlane's own name below 253, so the composed child
		// name can overflow a CR admission already accepted, and ensureExternalCatalogImports
		// then wedges in CatalogReady=False/ImportError backoff on an apiserver Invalid
		// the operator never asked for. Same rule as the managedEntries child names
		// (validateExternalCatalog), one level up — and it binds first, because the
		// import name is the longer of the two.
		if n := len(cp.Name) + identityImportChildNameOverhead; n > maxObjectNameBytes {
			allErrs = append(allErrs, field.Invalid(field.NewPath("metadata", "name"), cp.Name, fmt.Sprintf(
				"the identity Endpoint import CR name would be %d bytes; shorten the ControlPlane name "+
					"so it stays within the %d-byte Kubernetes object-name limit", n, maxObjectNameBytes,
			)))
		}

		if ks.External == nil {
			allErrs = append(allErrs, field.Required(ksPath.Child("external"),
				"external is required when services.keystone.mode is External"))
		} else {
			switch ks.External.AuthURL {
			case "":
				allErrs = append(allErrs, field.Required(ksPath.Child("external", "authURL"),
					"authURL is required when services.keystone.mode is External"))
			default:
				authURLPath := ksPath.Child("external", "authURL")
				if _, err := validateHTTPURL(authURLPath, ks.External.AuthURL); err != nil {
					allErrs = append(allErrs, err)
				} else if len(ks.External.AuthURL) > maxExternalAuthURLBytes {
					allErrs = append(allErrs, field.Invalid(authURLPath, ks.External.AuthURL,
						fmt.Sprintf("must be at most %d bytes", maxExternalAuthURLBytes)))
				}
			}
			if ref := ks.External.CABundleSecretRef; ref != nil {
				if ref.Name == "" {
					allErrs = append(allErrs, field.Required(ksPath.Child("external", "caBundleSecretRef", "name"),
						"must be set when caBundleSecretRef is configured"))
				}
				// A CA bundle is only ever consulted during a TLS handshake, and a
				// plaintext endpoint never performs one. Accepting the pair would hand
				// the operator full positive confirmation that trust is enforced —
				// readExternalCABundle blocks the mint on WaitingForCABundle until the
				// Secret exists, setCACertKey projects `cacert` into both credentials
				// Secrets — while buildPasswordCloudsYAML renders the admin password
				// next to an http:// auth_url and K-ORC POSTs it in the clear on every
				// mint and re-mint. Reject the combination rather than silently voiding
				// the bundle. Plain http:// WITHOUT a caBundleSecretRef stays admissible:
				// it claims no transport security, so it misleads nobody.
				if externalAuthURLIsPlaintext(ks.External.AuthURL) {
					allErrs = append(allErrs, field.Invalid(ksPath.Child("external", "authURL"), ks.External.AuthURL,
						"must use scheme https when caBundleSecretRef is set: a plaintext endpoint never "+
							"performs the TLS handshake the CA bundle would verify"))
				}
			}
			if ks.External.Catalog != nil {
				allErrs = append(allErrs,
					validateExternalCatalog(ksPath.Child("external", "catalog"), cp.Name, ks.External.Catalog)...)
			}
		}

		// Managed-only Keystone fields are forbidden in External mode: no Keystone
		// workload is deployed, and per P2 catalog advertisement (publicEndpoint) is
		// owned by the W5 catalog imports.
		if ks.Replicas != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("replicas"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed)"))
		}
		if ks.Image != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("image"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed)"))
		}
		if ks.PolicyOverrides != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("policyOverrides"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed)"))
		}
		if ks.RotationInterval != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("rotationInterval"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed)"))
		}
		if ks.Gateway != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("gateway"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed)"))
		}
		if ks.PublicEndpoint != "" {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("publicEndpoint"),
				"forbidden when services.keystone.mode is External (catalog advertisement is owned by the External Keystone)"))
		}
		if ks.FederationProxyImage != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("federationProxyImage"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed, so there is no sidecar to image)"))
		}
		if ks.DedicatedBackingServices != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("dedicatedBackingServices"),
				"forbidden when services.keystone.mode is External (no backing services are provisioned at all)"))
		}
		if ks.Namespace != nil {
			allErrs = append(allErrs, field.Forbidden(ksPath.Child("namespace"),
				"forbidden when services.keystone.mode is External (no Keystone workload is deployed, so there is nothing to place)"))
		}

		// Cross-field rules CEL cannot express.
		if cp.Spec.Infrastructure != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("infrastructure"),
				"forbidden when services.keystone.mode is External (phase 2 will relax this to optional)"))
		}
		if cp.Spec.Services.Horizon != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("services", "horizon"),
				"forbidden when services.keystone.mode is External (Horizon needs its own External-mode design)"))
		}

		return allErrs
	}

	// Not External (Managed, unset mode, or unset keystone service).
	if ks := cp.Spec.Services.Keystone; ks != nil && ks.External != nil {
		allErrs = append(allErrs, field.Forbidden(
			specPath.Child("services", "keystone", "external"),
			"may only be set when services.keystone.mode is External",
		))
	}
	if cp.Spec.Infrastructure == nil {
		allErrs = append(allErrs, field.Required(specPath.Child("infrastructure"),
			"is required unless services.keystone.mode is External"))
	}

	// Defense-in-depth federationProxyImage checks alongside the
	// commonv1.ImageSpec markers. The value is projected verbatim onto the
	// Keystone child's spec.federation.proxyImage, whose own webhook enforces
	// the same rules — rejecting here surfaces the error on the ControlPlane
	// the operator actually edits, rather than as an opaque
	// KeystoneProjectionRejected condition.
	if ks := cp.Spec.Services.Keystone; ks != nil && ks.FederationProxyImage != nil {
		imgPath := specPath.Child("services", "keystone", "federationProxyImage")
		if ks.FederationProxyImage.Repository == "" {
			allErrs = append(allErrs, field.Required(imgPath.Child("repository"),
				"federationProxyImage.repository must be set"))
		}
		if (ks.FederationProxyImage.Tag != "") == (ks.FederationProxyImage.Digest != "") {
			allErrs = append(allErrs, field.Invalid(imgPath, ks.FederationProxyImage,
				"exactly one of federationProxyImage.tag or federationProxyImage.digest must be set"))
		}
	}

	return allErrs
}

// newInvalidIfErrs wraps a non-empty field.ErrorList in an apierrors.NewInvalid
// for the ControlPlane GroupKind, or returns nil when there are no errors. It is
// the single point where the validating webhook turns accumulated field errors
// into the admission response, so ValidateCreate and ValidateUpdate share an
// identical error shape.
func newInvalidIfErrs(cp *ControlPlane, allErrs field.ErrorList) error {
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "ControlPlane"},
		cp.Name,
		allErrs,
	)
}

// validateImmutable accumulates errors for every create-only-immutable field
// that changed between oldObj and newObj (#476). The validating webhook is the
// load-bearing mechanism here because the affected leaves live in the shared
// commonv1.DatabaseSpec/CacheSpec types, which the keystone operator reuses and
// which therefore must not carry c5c3-specific CEL immutability markers.
//
//   - The create-only leaves of every database/cache instance — the shared block
//     and each per-service dedicated one — via validateDatabaseImmutable /
//     validateCacheImmutable, and the shared<->dedicated presence freeze via
//     validateDedicatedBackingServicesImmutable.
//   - A cloudCredentialsRef.secretName change re-points the K-ORC clouds.yaml
//     projection and leaks the previously-named ExternalSecret.
//   - The region (spec.region) is projected verbatim into the Keystone child's
//     now-immutable spec.bootstrap.region (#466). Changing it here would make the
//     next reconcile attempt an update the Keystone CEL rule rejects, wedging the
//     loop; rejecting the change at the ControlPlane layer surfaces a clean error
//     instead.
//
// keystoneModeString returns cp's keystone service mode as a string for use in a
// transition-gating error message, or "unset" when the service block is absent.
func keystoneModeString(cp *ControlPlane) string {
	if ks := cp.Spec.Services.Keystone; ks != nil {
		return string(ks.Mode)
	}
	return "unset"
}

// validate() already enforces the database/cache XOR (exactly one of clusterRef
// or host/servers), so clusterRef nil-ness is an unambiguous mode discriminator
// here.
//
// It also gates the keystone MODE transition. This is webhook-only for the same
// reason as the leaves above but one level up: the rule is cross-field over the
// OLD and NEW objects (a comparison CEL x-kubernetes-validations cannot express),
// and — unlike the immutable leaves — External->Managed must become a *gated*
// takeover in phase 3, so both directions are rejected with distinct messages
// rather than marked immutable (an immutable marker could never be relaxed to a
// gated transition later).
func validateImmutable(oldObj, newObj *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList

	// Keystone mode transition gating. Managed->External is rejected outright
	// (adoption of an existing installation must be a fresh External-mode
	// ControlPlane, not an in-place flip of a live one). External->Managed (or
	// away from External by removing the service) is rejected with a distinct
	// message naming the reserved phase-3 takeover, so the direction stays a
	// deliberate future transition rather than a hard immutable field.
	oldExternal := oldObj.IsExternalKeystone()
	newExternal := newObj.IsExternalKeystone()
	modePath := field.NewPath("spec", "services", "keystone", "mode")
	switch {
	case !oldExternal && newExternal:
		allErrs = append(allErrs, field.Invalid(modePath, string(KeystoneModeExternal),
			"keystone mode cannot be changed to External on an existing ControlPlane; "+
				"create a new External-mode ControlPlane to adopt an existing installation"))
	case oldExternal && !newExternal:
		allErrs = append(allErrs, field.Invalid(modePath, keystoneModeString(newObj),
			"switching an External-mode ControlPlane to Managed is not yet supported; "+
				"the managed takeover is reserved as the gated phase-3 transition"))
	}

	// Infrastructure presence flip (defense-in-depth for webhook-bypassed states,
	// e.g. a direct etcd write). Adding or removing the block on UPDATE is an
	// infrastructure-vs-mode transition that the mode gating above already covers
	// for a mode change; freezing presence independently rejects a bare
	// add/remove that leaves the mode unchanged.
	if (oldObj.Spec.Infrastructure == nil) != (newObj.Spec.Infrastructure == nil) {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "infrastructure"), newObj.Spec.Infrastructure,
			"infrastructure presence is immutable (adding or removing the block after creation is not permitted)",
		))
	}

	// spec.infrastructure is an optional pointer now (External keystone mode omits
	// it). The database/cache immutability comparisons only apply when the block is
	// present on BOTH revisions — a presence flip (block added or removed) is an
	// infrastructure-vs-mode transition governed by the External-mode gating, not a
	// database/cache field mutation. When either side is nil there are no managed
	// clusterRef/name/replicas/storageSize leaves to freeze. The
	// cloudCredentialsRef.secretName and region immutability checks below are
	// mode-independent and always run.
	if oldInfra, newInfra := oldObj.Spec.Infrastructure, newObj.Spec.Infrastructure; oldInfra != nil && newInfra != nil {
		specPath := field.NewPath("spec", "infrastructure")
		allErrs = append(allErrs, validateDatabaseImmutable(specPath.Child("database"), &oldInfra.Database, &newInfra.Database)...)
		allErrs = append(allErrs, validateCacheImmutable(specPath.Child("cache"), &oldInfra.Cache, &newInfra.Cache)...)
	}

	allErrs = append(allErrs, validateDedicatedBackingServicesImmutable(oldObj, newObj)...)
	allErrs = append(allErrs, validateServiceNamespacesImmutable(oldObj, newObj)...)

	oldSecretName := oldObj.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	newSecretName := newObj.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	if oldSecretName != newSecretName {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "korc", "adminCredential", "cloudCredentialsRef", "secretName"),
			newSecretName, "cloudCredentialsRef.secretName is immutable",
		))
	}

	if oldObj.Spec.Region != newObj.Spec.Region {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "region"),
			newObj.Spec.Region, "region is immutable",
		))
	}

	allErrs = append(allErrs, validateServiceAccountsImmutable(oldObj, newObj)...)

	return allErrs
}

// validateDatabaseImmutable freezes the create-only leaves of ONE database
// instance — the shared spec.infrastructure.database and every per-service
// dedicated one alike, since each is projected onto the same MariaDB and
// Keystone-child fields:
//
//   - the MODE (managed clusterRef vs brownfield host): flipping it leaves the
//     previously-projected MariaDB child (and its credential ExternalSecret)
//     running and owned until the ControlPlane is deleted;
//   - a managed clusterRef.NAME change re-points the projection at a new child
//     and orphans the old one the same way;
//   - the database NAME, which is projected verbatim into the consuming service
//     child's now-immutable spec.database.database — renaming it here would make
//     the next reconcile attempt an update the child's CEL rule rejects, wedging
//     the loop behind a KeystoneProjectionRejected condition;
//   - replicas, which drives the owned MariaDB's replica count and the derived
//     Galera topology, so an in-place edit would toggle Galera off or scale a
//     running Galera cluster down — destructive on a live cluster;
//   - storageSize, which the mariadb-operator refuses to change on a live CR. The
//     comparison normalizes "" to the default the fresh-create projection
//     actually provisions (effectiveStorageSize), so a ControlPlane stored before
//     the field existed can migrate once to an explicit default while any OTHER
//     value is still rejected as a resize.
//
// The checks are webhook-only: the leaves live in the shared commonv1.DatabaseSpec,
// which the keystone operator reuses and which therefore must not carry
// c5c3-specific CEL immutability markers.
func validateDatabaseImmutable(fldPath *field.Path, oldDB, newDB *commonv1.DatabaseSpec) field.ErrorList {
	var allErrs field.ErrorList

	// validate() enforces the database XOR (exactly one of clusterRef or host), so
	// clusterRef nil-ness is an unambiguous mode discriminator here.
	switch {
	case (oldDB.ClusterRef != nil) != (newDB.ClusterRef != nil):
		allErrs = append(allErrs, field.Invalid(fldPath, *newDB,
			"database mode (managed clusterRef vs brownfield host) is immutable"))
	case oldDB.ClusterRef != nil && newDB.ClusterRef != nil && oldDB.ClusterRef.Name != newDB.ClusterRef.Name:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("clusterRef", "name"),
			newDB.ClusterRef.Name, "managed database clusterRef.name is immutable"))
	}
	if oldDB.Database != newDB.Database {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("database"),
			newDB.Database, "database name is immutable"))
	}
	if oldDB.Replicas != newDB.Replicas {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("replicas"),
			newDB.Replicas, "database replicas is immutable after creation "+
				"(toggling Galera or scaling down a live cluster is destructive)"))
	}
	if effectiveStorageSize(oldDB.StorageSize) != effectiveStorageSize(newDB.StorageSize) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("storageSize"),
			newDB.StorageSize, "database storageSize is immutable after creation "+
				"(the mariadb-operator rejects resizing a live volume)"))
	}
	return allErrs
}

// validateCacheImmutable freezes the create-only leaves of ONE cache instance —
// the shared spec.infrastructure.cache and every per-service dedicated one alike.
// Only the MODE and the managed clusterRef.name are frozen: replicas stays
// mutable, because ensureMemcached reconciles an owned Memcached's replica count
// in place (scaling a cache loses no data).
func validateCacheImmutable(fldPath *field.Path, oldCache, newCache *commonv1.CacheSpec) field.ErrorList {
	var allErrs field.ErrorList
	switch {
	case (oldCache.ClusterRef != nil) != (newCache.ClusterRef != nil):
		allErrs = append(allErrs, field.Invalid(fldPath, *newCache,
			"cache mode (managed clusterRef vs brownfield servers) is immutable"))
	case oldCache.ClusterRef != nil && newCache.ClusterRef != nil && oldCache.ClusterRef.Name != newCache.ClusterRef.Name:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("clusterRef", "name"),
			newCache.ClusterRef.Name, "managed cache clusterRef.name is immutable"))
	}
	return allErrs
}

// dedicatedTransitionMessage is the single message every shared<->dedicated
// presence freeze reports, so the four sites (the per-service block and each
// backing-service class within it) cannot drift apart.
const dedicatedTransitionMessage = "switching a service between shared and dedicated backing services on a live " +
	"ControlPlane is not yet supported; remove and recreate the ControlPlane to change it"

// validateDedicatedBackingServicesImmutable freezes the per-service dedicated
// backing-service declarations on UPDATE, in two layers:
//
//   - PRESENCE, both of the per-service block and of each backing-service class
//     within it. An in-place shared->dedicated flip (or back) would re-point the
//     consuming child's database/cache at a different instance — and those child
//     leaves (spec.database.database, spec.bootstrap.*) are themselves immutable,
//     so the projection would be rejected and wedge the reconcile behind a
//     KeystoneProjectionRejected condition — while the previously-provisioned
//     instance keeps running, owned, with no migration of the data on it.
//   - The create-only LEAVES of a dedicated instance that stays declared, via the
//     same validateDatabaseImmutable / validateCacheImmutable rules the shared
//     block gets.
//
// The presence freeze is deliberately webhook-only, with NO CEL transition rule:
// switching an existing service between shared and dedicated (with or without
// data migration) is a reserved future feature, and an immutable CEL marker could
// never be relaxed to a gated transition later. This mirrors the keystone-mode
// transition gating above.
func validateDedicatedBackingServicesImmutable(oldObj, newObj *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	svcPath := field.NewPath("spec", "services")

	ksPath := svcPath.Child("keystone", "dedicatedBackingServices")
	oldKS := keystoneDedicatedBlock(oldObj)
	newKS := keystoneDedicatedBlock(newObj)
	if (oldKS == nil) != (newKS == nil) {
		allErrs = append(allErrs, field.Invalid(ksPath, newKS, dedicatedTransitionMessage))
	} else if oldKS != nil && newKS != nil {
		allErrs = append(allErrs, validateDedicatedDatabase(ksPath.Child("database"), oldKS.Database, newKS.Database)...)
		allErrs = append(allErrs, validateDedicatedCache(ksPath.Child("cache"), oldKS.Cache, newKS.Cache)...)
	}

	hzPath := svcPath.Child("horizon", "dedicatedBackingServices")
	oldHZ := horizonDedicatedBlock(oldObj)
	newHZ := horizonDedicatedBlock(newObj)
	if (oldHZ == nil) != (newHZ == nil) {
		allErrs = append(allErrs, field.Invalid(hzPath, newHZ, dedicatedTransitionMessage))
	} else if oldHZ != nil && newHZ != nil {
		allErrs = append(allErrs, validateDedicatedCache(hzPath.Child("cache"), oldHZ.Cache, newHZ.Cache)...)
	}

	return allErrs
}

// validateDedicatedDatabase freezes one dedicated database class: its presence
// (adding or removing the class is the same shared<->dedicated transition as
// adding or removing the whole block) and, when it stays declared, its
// create-only leaves.
func validateDedicatedDatabase(fldPath *field.Path, oldDB, newDB *commonv1.DatabaseSpec) field.ErrorList {
	switch {
	case (oldDB == nil) != (newDB == nil):
		return field.ErrorList{field.Invalid(fldPath, newDB, dedicatedTransitionMessage)}
	case oldDB == nil:
		return nil
	}
	return validateDatabaseImmutable(fldPath, oldDB, newDB)
}

// validateDedicatedCache is the cache twin of validateDedicatedDatabase.
func validateDedicatedCache(fldPath *field.Path, oldCache, newCache *commonv1.CacheSpec) field.ErrorList {
	switch {
	case (oldCache == nil) != (newCache == nil):
		return field.ErrorList{field.Invalid(fldPath, newCache, dedicatedTransitionMessage)}
	case oldCache == nil:
		return nil
	}
	return validateCacheImmutable(fldPath, oldCache, newCache)
}

// serviceNamespaceTransitionMessage is the single message every per-service
// namespace freeze reports, so the sites (the block's presence, its name, its
// lifecycle, for each service) cannot drift apart.
const serviceNamespaceTransitionMessage = "the namespace a service is placed in is immutable; moving a live service " +
	"across namespaces would leave its backing services, its secret store, and the credential material scoped to " +
	"the old namespace behind with no migration path — remove and recreate the ControlPlane to change it"

// validateServiceNamespacesImmutable freezes the per-service namespace
// assignments on UPDATE: the PRESENCE of the block, the namespace NAME, and the
// LIFECYCLE.
//
//   - Presence and name: the namespace is where the service's MariaDB/Memcached,
//     its per-namespace tenant SecretStore, and every OpenBao path scoped by it
//     (bootstrap/{ns}/…, the keystone-{ns} database-engine role) live. Re-pointing
//     a live service at another namespace would strand all of it — and the child's
//     own database/bootstrap leaves are themselves immutable, so the re-projection
//     would be rejected and wedge the reconcile behind a ProjectionRejected
//     condition anyway.
//   - Lifecycle: flipping External->Managed would have the operator claim, and at
//     teardown DELETE, a namespace it was told it does not own; flipping
//     Managed->External would abandon a namespace it created.
//
// The freeze is deliberately webhook-only, with NO CEL transition rule: moving a
// service between namespaces (with the data migration that implies) is a reserved
// future feature, and an immutable CEL marker could never be relaxed to a gated
// transition later. This mirrors the dedicated-backing-services freeze above.
func validateServiceNamespacesImmutable(oldObj, newObj *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	svcPath := field.NewPath("spec", "services")

	freeze := func(path *field.Path, oldNS, newNS *ServiceNamespaceSpec) {
		switch {
		case (oldNS == nil) != (newNS == nil):
			allErrs = append(allErrs, field.Invalid(path, newNS, serviceNamespaceTransitionMessage))
		case oldNS == nil:
			return
		}
		if oldNS == nil || newNS == nil {
			return
		}
		if oldNS.Name != newNS.Name {
			allErrs = append(allErrs, field.Invalid(path.Child("name"), newNS.Name, serviceNamespaceTransitionMessage))
		}
		if oldNS.Lifecycle != newNS.Lifecycle {
			allErrs = append(allErrs, field.Invalid(path.Child("lifecycle"), newNS.Lifecycle,
				serviceNamespaceTransitionMessage))
		}
	}

	freeze(svcPath.Child("keystone", "namespace"), keystoneNamespaceBlock(oldObj), keystoneNamespaceBlock(newObj))
	freeze(svcPath.Child("horizon", "namespace"), horizonNamespaceBlock(oldObj), horizonNamespaceBlock(newObj))

	return allErrs
}

// validateServiceAccountsImmutable freezes the per-entry identity and project
// fields whose in-place edit would rename or re-own live Keystone resources. The
// entry key is name (the listMapKey), so entries are matched across old/new by it;
// an added or removed entry is a create/delete the reconciler handles, not a
// mutation. adopt stays mutable: flipping it to true is the documented collision
// remediation.
func validateServiceAccountsImmutable(oldObj, newObj *ControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	oldSAs := make(map[string]ServiceAccountSpec, len(oldObj.Spec.KORC.ServiceAccounts))
	for _, sa := range oldObj.Spec.KORC.ServiceAccounts {
		oldSAs[sa.Name] = sa
	}
	for i, sa := range newObj.Spec.KORC.ServiceAccounts {
		old, ok := oldSAs[sa.Name]
		if !ok {
			continue
		}
		saPath := field.NewPath("spec", "korc", "serviceAccounts").Index(i)
		if old.UserName != sa.UserName {
			allErrs = append(allErrs, field.Invalid(saPath.Child("userName"), sa.UserName,
				"userName is immutable; remove and re-add the service account under a new name to rename its user"))
		}
		if old.DomainName != sa.DomainName {
			allErrs = append(allErrs, field.Invalid(saPath.Child("domainName"), sa.DomainName,
				"domainName is immutable; remove and re-add the service account to move it to another domain"))
		}
		if old.Project.Name != sa.Project.Name {
			allErrs = append(allErrs, field.Invalid(saPath.Child("project", "name"), sa.Project.Name,
				"project.name is immutable; remove and re-add the service account to re-point its project"))
		}
		if old.Project.Create != sa.Project.Create {
			allErrs = append(allErrs, field.Invalid(saPath.Child("project", "create"), sa.Project.Create,
				"project.create is immutable; a managed<->referenced flip would orphan or adopt the live project"))
		}
	}
	return allErrs
}

// effectiveStorageSize resolves an empty database.storageSize to the default the
// c5c3 fresh-create projection actually provisions (DefaultDatabaseStorageSize),
// so validateImmutable compares the sizes the live MariaDB runs at rather than
// the raw spec strings. This is what lets a pre-existing ControlPlane (stored
// with "" before the field existed) migrate once to an explicit default.
func effectiveStorageSize(size string) string {
	if size == "" {
		return DefaultDatabaseStorageSize
	}
	return size
}

// validateReleaseNotDowngraded rejects an openStackRelease downgrade on UPDATE.
// OpenStack/Keystone DB migrations are forward-only (keystone-manage db_sync has
// no down-migration path), so re-pointing a live control plane at an older
// release would project an older image whose schema is behind the already-migrated
// database -- an unrecoverable state. Upgrades and same-release updates are
// allowed. The shared release parser compares the (year, minor) integer tuples
// rather than the raw strings, so ordering stays correct even for hypothetical
// multi-digit minors where lexicographic comparison would silently invert. A
// release release.ParseRelease cannot parse (malformed, or a minor outside the
// two-releases-per-year OpenStack cadence) is left to validate()'s pattern
// check rather than mis-parsed here, so a malformed value yields the pattern
// error alone instead of a confusing downgrade message.
func validateReleaseNotDowngraded(oldObj, newObj *ControlPlane) field.ErrorList {
	oldRel, errOld := release.ParseRelease(oldObj.Spec.OpenStackRelease)
	newRel, errNew := release.ParseRelease(newObj.Spec.OpenStackRelease)
	if errOld != nil || errNew != nil {
		return nil
	}
	if release.IsDowngrade(oldRel, newRel) {
		return field.ErrorList{field.Invalid(
			field.NewPath("spec", "openStackRelease"),
			newRel.Raw,
			fmt.Sprintf("openStackRelease downgrade from %q to %q is not permitted; Keystone DB migrations are not reversible", oldRel.Raw, newRel.Raw),
		)}
	}
	return nil
}

// validateUniqueInNamespace enforces the one-ControlPlane-per-namespace contract
// It lists existing ControlPlanes in the new object's
// namespace; any pre-existing CR (len >= 1, since the object under admission is
// not yet persisted) makes this CREATE a Forbidden error naming the incumbent.
// The List goes through the injected uncached API reader (mgr.GetAPIReader() in
// production), so the check cannot admit a second CR off a stale or still-empty
// informer cache. The reconciler's duplicate-ControlPlane guard
// (duplicateControlPlaneIncumbent in operators/c5c3/internal/controller) is the
// defense-in-depth for CREATEs that race within the API server itself or bypass
// the webhook entirely.
//
// DECISION: when w.Client is nil (spec-level unit tests that construct a bare
// &ControlPlaneWebhook{}, or any caller that did not inject a client) the check
// is skipped rather than panicking. Production and envtest wiring always inject
// mgr.GetAPIReader() (operators/c5c3/main.go, integration_test.go), so the
// guard never trips at runtime; it only keeps the spec-validation unit tests
// client-free.
func (w *ControlPlaneWebhook) validateUniqueInNamespace(ctx context.Context, obj *ControlPlane) error {
	if w.Client == nil {
		return nil
	}
	var existing ControlPlaneList
	if err := w.Client.List(ctx, &existing, client.InNamespace(obj.Namespace)); err != nil {
		return apierrors.NewInternalError(
			fmt.Errorf("listing ControlPlanes in namespace %q to enforce one-per-namespace: %w", obj.Namespace, err),
		)
	}
	if len(existing.Items) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "ControlPlane"},
		obj.Name,
		field.ErrorList{field.Forbidden(
			field.NewPath("metadata", "namespace"),
			fmt.Sprintf("only one ControlPlane is permitted per namespace; %q already exists in namespace %q",
				existing.Items[0].Name, obj.Namespace),
		)},
	)
}
