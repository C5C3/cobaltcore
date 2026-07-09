// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"fmt"

	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// appCredCloudsYAMLKey is the Secret data key the assembled app-credential
// clouds.yaml is stored under; the PushSecret mirrors it to OpenBao and the
// k-orc-clouds-yaml ExternalSecret reads it back as the "clouds.yaml" property.
const appCredCloudsYAMLKey = "clouds.yaml"

// managedEndpointType is the clouds.yaml endpoint_type a MANAGED Keystone is
// always addressed with. See buildAppCredCloudsYAML for why it can never be
// "public".
const managedEndpointType = "internal"

// defaultRegion is the region_name both builders render when spec.region is
// unset. It mirrors the OpenStack default a fresh Keystone bootstraps with.
const defaultRegion = "RegionOne"

// korcAuthURL resolves the auth_url both clouds.yaml builders render, switching
// on the Keystone mode:
//
//   - MANAGED: the in-cluster Keystone Service DNS (keystoneEndpointURL). K-ORC
//     runs in-cluster, so it must never dial the externally routable endpoint.
//   - EXTERNAL: spec.services.keystone.external.authURL verbatim — the
//     pre-existing installation is, by definition, not in this cluster.
//
// It returns "" for an External-mode CR with no external block; admission forbids
// that shape, and rendering an empty auth_url fails loud at K-ORC rather than
// silently pointing somewhere else.
func korcAuthURL(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.IsExternalKeystone() {
		return externalKeystoneAuthURL(cp)
	}
	return keystoneEndpointURL(cp)
}

// korcEndpointType resolves the clouds.yaml endpoint_type both builders render.
// Managed mode is always "internal" (see buildAppCredCloudsYAML); External mode
// takes spec.services.keystone.external.endpointType, falling back to
// DefaultExternalEndpointType ("public") for a webhook-bypassed CR that reached
// the reconciler with the field unset.
func korcEndpointType(cp *c5c3v1alpha1.ControlPlane) string {
	if !cp.IsExternalKeystone() {
		return managedEndpointType
	}
	if ks := cp.Spec.Services.Keystone; ks != nil && ks.External != nil && ks.External.EndpointType != "" {
		return string(ks.External.EndpointType)
	}
	return string(c5c3v1alpha1.DefaultExternalEndpointType)
}

// korcRegion resolves the clouds.yaml region_name from spec.region, defaulting to
// RegionOne.
//
// PREREQUISITE (External mode): the resolved region must exist in the external
// Keystone's service catalog. gophercloud fails LOUD on a mismatch ("No suitable
// endpoint could be found in the service catalog"), which reconcileKORC classifies
// onto KORCReady=False/CatalogEndpointMismatch — the operator cannot repair an
// external catalog, so it names spec.region and endpointType in the message.
func korcRegion(cp *c5c3v1alpha1.ControlPlane) string {
	return cmp.Or(cp.Spec.Region, defaultRegion)
}

// buildAppCredCloudsYAML assembles the application-credential clouds.yaml the
// control plane authenticates K-ORC with after minting: the credential id comes
// from the minted AC, the secret from the generated "value", and the auth_url and
// endpoint_type from the mode-aware resolvers (korcAuthURL, korcEndpointType).
//
// CRITICAL (endpoint_type, managed mode): gophercloud only uses the auth_url to
// obtain a token; for every subsequent API call it resolves the endpoint from the
// returned service catalog, picking the interface set here. A MANAGED K-ORC runs
// IN-CLUSTER, so it must use the "internal" (cluster-DNS) identity endpoint. Once
// the ControlPlane exposes Keystone via the shared Gateway the catalog's "public"
// identity endpoint becomes the external host (e.g.
// https://keystone.<host>.nip.io:8443/v3), which from inside a pod is unreachable —
// so "public" makes every list/get fail. Worse, K-ORC swallows that failure
// (osclients ListDomains does `_ = pager.EachPage(...)`) and reports it as an
// EMPTY import, so the admin Domain/User imports hang forever on "Waiting for
// OpenStack resource to be created externally".
//
// EXTERNAL mode inherits the same hazard from the other side: the endpoint_type is
// whatever spec.services.keystone.external.endpointType selects (default "public",
// because the external installation is reached over its routable interface), and a
// value that resolves to an interface the external catalog does not publish — or
// publishes on a network the cluster cannot reach — produces the SAME silent-empty
// import. reconcileKORC therefore escalates a stalled External-mode import to
// KORCReady=False/ImportStalled instead of waiting forever.
//
// The key MUST be "endpoint_type", NOT "interface": K-ORC's scope builder copies
// only clientconfig.Cloud.EndpointType (the `endpoint_type` key) into the client
// options and drops Cloud.Interface (the `interface` key) — see vendored
// internal/scope/provider.go NewProviderClient. An "interface:" value is therefore
// ignored and the endpoint defaults to "public".
func buildAppCredCloudsYAML(cp *c5c3v1alpha1.ControlPlane, acID, secret string) string {
	return fmt.Sprintf(`clouds:
  admin:
    auth:
      auth_url: %q
      application_credential_id: %q
      application_credential_secret: %q
    auth_type: v3applicationcredential
    region_name: %q
    endpoint_type: %s
    identity_api_version: 3
`, korcAuthURL(cp), acID, secret, korcRegion(cp), korcEndpointType(cp))
}

// buildPasswordCloudsYAML assembles the password-based clouds.yaml the admin
// ApplicationCredential authenticates with to mint (and, on re-mint, revoke) the
// Keystone credential. The cloud key matches the CloudCredentialsRef.CloudName;
// auth_url, endpoint_type and region_name come from the mode-aware resolvers.
//
// In MANAGED mode it mirrors the bootstrap seed
// (deploy/openbao/bootstrap/write-bootstrap-secrets.sh) so the in-cluster and
// operator-owned credentials are byte-compatible. External mode has no shell seed
// at all — the operator-owned document is the only one.
func buildPasswordCloudsYAML(cp *c5c3v1alpha1.ControlPlane, password string) string {
	cloudName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName
	if cloudName == "" {
		cloudName = c5c3v1alpha1.DefaultCloudName
	}
	return fmt.Sprintf(`clouds:
  %s:
    auth:
      auth_url: %q
      username: %q
      password: %q
      project_name: %q
      user_domain_name: %q
      project_domain_name: %q
    region_name: %q
    endpoint_type: %s
    identity_api_version: 3
`, cloudName, korcAuthURL(cp), korcAdminUsername, password,
		korcAdminUsername, korcAdminDomainName, korcAdminDomainName,
		korcRegion(cp), korcEndpointType(cp))
}
