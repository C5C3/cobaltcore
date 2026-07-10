// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
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

// identityCatalogServiceType is the OpenStack service type of the Keystone
// catalog entry. In External mode that entry is owned by the unmanaged imports,
// so it may never be declared as a managed entry.
const identityCatalogServiceType = "identity"

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
		case entry.Type == identityCatalogServiceType:
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
		db := &obj.Spec.Infrastructure.Database
		if db.Database == "" {
			db.Database = DefaultDatabaseName
		}
		if db.SecretRef.Name == "" {
			db.SecretRef.Name = DefaultDatabaseSecretName
		}
		if db.Host == "" {
			if db.ClusterRef == nil {
				db.ClusterRef = &corev1.LocalObjectReference{Name: DefaultDatabaseClusterRefName}
			} else if db.ClusterRef.Name == "" {
				db.ClusterRef.Name = DefaultDatabaseClusterRefName
			}
		}

		cache := &obj.Spec.Infrastructure.Cache
		if cache.Backend == "" {
			cache.Backend = DefaultCacheBackend
		}
		if len(cache.Servers) == 0 {
			if cache.ClusterRef == nil {
				cache.ClusterRef = &corev1.LocalObjectReference{Name: DefaultCacheClusterRefName}
			} else if cache.ClusterRef.Name == "" {
				cache.ClusterRef.Name = DefaultCacheClusterRefName
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
	return warnings, w.validateUniqueInNamespace(ctx, obj)
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

		// database.replicas must be 1 (standalone) or >=3 (a quorum-safe Galera
		// cluster). Exactly 2 is rejected because the managed-mode MariaDB projection
		// (ensureMariaDB) turns any replicas>1 into a Galera cluster, and a two-node
		// Galera cluster cannot hold a majority — a single pod disruption (restart,
		// OOM-kill, rolling update, network partition) then loses quorum and takes the
		// whole database offline. The CRD marker only enforces Minimum=1, so this
		// webhook is the enforcement point; the shared commonv1.DatabaseSpec must not
		// carry a c5c3-specific CEL rule the keystone operator (which ignores replicas)
		// would also inherit. A zero value (CRD/webhook default bypassed) is left to
		// the reconciler's floor, so only an explicit 2 is rejected here.
		if db.Replicas == 2 {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("infrastructure", "database", "replicas"),
				db.Replicas,
				"database replicas must be 1 (standalone) or >=3 (Galera needs quorum); 2 cannot hold a majority",
			))
		}

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
			switch {
			case ks.External.AuthURL == "":
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
//   - Database/cache MODE (managed clusterRef vs brownfield host/servers):
//     flipping it leaves the previously-projected MariaDB/Memcached child (and,
//     in managed mode, its per-ControlPlane credential ExternalSecret) running
//     and owned until the ControlPlane is deleted.
//   - A managed clusterRef.NAME change re-points the projection at a new child
//     and orphans the old one the same way.
//   - A cloudCredentialsRef.secretName change re-points the K-ORC clouds.yaml
//     projection and leaks the previously-named ExternalSecret.
//   - The database NAME (spec.infrastructure.database.database) and the region
//     (spec.region) are projected verbatim into the Keystone child's now-immutable
//     spec.database.database / spec.bootstrap.region (#466). Renaming either here
//     would make the next reconcile attempt an update the Keystone CEL rule
//     rejects, wedging the loop; rejecting the change at the ControlPlane layer
//     surfaces a clean error instead.
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
		dbPath := field.NewPath("spec", "infrastructure", "database")
		oldDB := oldInfra.Database
		newDB := newInfra.Database
		switch {
		case (oldDB.ClusterRef != nil) != (newDB.ClusterRef != nil):
			allErrs = append(allErrs, field.Invalid(dbPath, newDB,
				"database mode (managed clusterRef vs brownfield host) is immutable"))
		case oldDB.ClusterRef != nil && newDB.ClusterRef != nil && oldDB.ClusterRef.Name != newDB.ClusterRef.Name:
			allErrs = append(allErrs, field.Invalid(dbPath.Child("clusterRef", "name"),
				newDB.ClusterRef.Name, "managed database clusterRef.name is immutable"))
		}
		if oldDB.Database != newDB.Database {
			allErrs = append(allErrs, field.Invalid(dbPath.Child("database"),
				newDB.Database, "database name is immutable"))
		}
		// database.replicas is create-only. It is projected into the managed MariaDB
		// child's replica count and the derived Galera topology (ensureMariaDB), so
		// editing it on a live ControlPlane would drive a destructive Update on the
		// owned cluster — toggling Galera off (3->1) or scaling a running Galera
		// cluster down. Freezing it here keeps the CR the single source of truth for a
		// topology that can only be changed safely by recreating the control plane,
		// mirroring the database name / region / clusterRef.name immutability above.
		if oldDB.Replicas != newDB.Replicas {
			allErrs = append(allErrs, field.Invalid(dbPath.Child("replicas"),
				newDB.Replicas, "database replicas is immutable after creation "+
					"(toggling Galera or scaling down a live cluster is destructive)"))
		}
		// database.storageSize is create-only for the same reason: it is projected
		// into the owned MariaDB's spec.storage.size, which the mariadb-operator
		// rejects changing on a live CR (its webhook forbids resizing/shrinking the
		// PVC). Freezing it at the ControlPlane layer surfaces the constraint at
		// admission with a clear message instead of letting the edit reach — and be
		// rejected by — the child MariaDB. Mirrors the replicas immutability above.
		//
		// The comparison is against the DEFAULTED value on both sides: a ControlPlane
		// created before storageSize existed has "" persisted (and any read/apply
		// path that bypasses CRD defaulting, e.g. a fake-client test, can surface ""),
		// yet its live MariaDB was provisioned at the default size. Normalizing ""
		// to DefaultDatabaseStorageSize lets such a CR migrate once — an update that
		// pins the field to the default it already runs at is admitted — while any
		// OTHER value is still rejected as a resize the mariadb-operator would refuse.
		if effectiveStorageSize(oldDB.StorageSize) != effectiveStorageSize(newDB.StorageSize) {
			allErrs = append(allErrs, field.Invalid(dbPath.Child("storageSize"),
				newDB.StorageSize, "database storageSize is immutable after creation "+
					"(the mariadb-operator rejects resizing a live volume)"))
		}

		cachePath := field.NewPath("spec", "infrastructure", "cache")
		oldCache := oldInfra.Cache
		newCache := newInfra.Cache
		switch {
		case (oldCache.ClusterRef != nil) != (newCache.ClusterRef != nil):
			allErrs = append(allErrs, field.Invalid(cachePath, newCache,
				"cache mode (managed clusterRef vs brownfield servers) is immutable"))
		case oldCache.ClusterRef != nil && newCache.ClusterRef != nil && oldCache.ClusterRef.Name != newCache.ClusterRef.Name:
			allErrs = append(allErrs, field.Invalid(cachePath.Child("clusterRef", "name"),
				newCache.ClusterRef.Name, "managed cache clusterRef.name is immutable"))
		}
	}

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
