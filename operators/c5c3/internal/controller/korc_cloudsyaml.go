// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// appCredCloudsYAMLKey is the Secret data key the assembled app-credential
// clouds.yaml is stored under; the PushSecret mirrors it to OpenBao and the
// k-orc-clouds-yaml ExternalSecret reads it back as the "clouds.yaml" property.
const appCredCloudsYAMLKey = "clouds.yaml"

// buildAppCredCloudsYAML assembles the application-credential clouds.yaml the
// control plane authenticates K-ORC with after minting: the credential id comes
// from the minted AC, the secret from the generated "value", and the auth_url from
// the projected Keystone Service (keystoneEndpointURL).
//
// CRITICAL (endpoint_type: internal): gophercloud only uses the auth_url to obtain
// a token; for every subsequent API call it resolves the endpoint from the returned
// service catalog, picking the interface set here. K-ORC runs IN-CLUSTER, so it
// must use the "internal" (cluster-DNS) identity endpoint. Once the ControlPlane
// exposes Keystone via the shared Gateway the catalog's "public" identity endpoint
// becomes the external host (e.g. https://keystone.<host>.nip.io:8443/v3), which
// from inside a pod is unreachable — so "public" makes every list/get fail. Worse,
// K-ORC swallows that failure (osclients ListDomains does `_ = pager.EachPage(...)`)
// and reports it as an EMPTY import, so the admin Domain/User imports hang forever
// on "Waiting for OpenStack resource to be created externally".
//
// The key MUST be "endpoint_type", NOT "interface": K-ORC's scope builder copies
// only clientconfig.Cloud.EndpointType (the `endpoint_type` key) into the client
// options and drops Cloud.Interface (the `interface` key) — see vendored
// internal/scope/provider.go NewProviderClient. An "interface:" value is therefore
// ignored and the endpoint defaults to "public". The auth_url already points at the
// in-cluster Service for the same reason (keystoneEndpointURL, never the external
// endpoint).
func buildAppCredCloudsYAML(cp *c5c3v1alpha1.ControlPlane, acID, secret string) string {
	region := cp.Spec.Region
	if region == "" {
		region = "RegionOne"
	}
	return fmt.Sprintf(`clouds:
  admin:
    auth:
      auth_url: %q
      application_credential_id: %q
      application_credential_secret: %q
    auth_type: v3applicationcredential
    region_name: %q
    endpoint_type: internal
    identity_api_version: 3
`, keystoneEndpointURL(cp), acID, secret, region)
}

// buildPasswordCloudsYAML assembles the password-based clouds.yaml the admin
// ApplicationCredential authenticates with to mint (and, on re-mint, revoke) the
// Keystone credential. It mirrors the bootstrap seed
// (deploy/openbao/bootstrap/write-bootstrap-secrets.sh) so the in-cluster and
// operator-owned credentials are byte-compatible: the cloud key matches the
// CloudCredentialsRef.CloudName, auth_url is the in-cluster Keystone Service
// (keystoneEndpointURL — never the external endpoint), and endpoint_type is
// "internal" (the key MUST be "endpoint_type", not "interface"; see
// buildAppCredCloudsYAML for the full rationale).
func buildPasswordCloudsYAML(cp *c5c3v1alpha1.ControlPlane, password string) string {
	region := cp.Spec.Region
	if region == "" {
		region = "RegionOne"
	}
	cloudName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName
	if cloudName == "" {
		cloudName = korcAdminUsername
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
    endpoint_type: internal
    identity_api_version: 3
`, cloudName, keystoneEndpointURL(cp), korcAdminUsername, password,
		korcAdminUsername, korcAdminDomainName, korcAdminDomainName, region)
}
