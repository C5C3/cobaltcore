// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package keystoneauth renders the [keystone_authtoken] section of an
// OpenStack service's oslo.config INI file, together with the password the
// service-account authenticates with. Every OpenStack service other than
// Keystone validates incoming API tokens through this section; the operator
// renders the non-secret options into the service's shared ConfigMap and
// injects the password separately through oslo.config's OS_<GROUP>__<OPTION>
// env override so the secret never lands in the ConfigMap.
package keystoneauth

import (
	corev1 "k8s.io/api/core/v1"
)

// PasswordEnvVarName is the oslo.config env override key for
// [keystone_authtoken].password. The OS_<GROUP>__<OPTION> form wins over the
// ConfigMap value at runtime, so service containers read the service-account
// password from a Secret via PasswordEnvVar instead of from the rendered
// ConfigMap, keeping the secret out of the config.
const PasswordEnvVarName = "OS_KEYSTONE_AUTHTOKEN__PASSWORD"

// SectionParams carries the non-secret [keystone_authtoken] options a service
// operator renders for its API token middleware. The password is deliberately
// absent: it is delivered exclusively through PasswordEnvVar so it never lands
// in the rendered ConfigMap.
type SectionParams struct {
	// AuthURL is the Keystone identity endpoint the middleware validates tokens
	// against (the auth_url option).
	AuthURL string
	// WWWAuthenticateURI is the public identity endpoint advertised to clients
	// in 401 responses (the www_authenticate_uri option); it may differ from
	// AuthURL when the internal and public endpoints diverge.
	WWWAuthenticateURI string
	// Username is the service account the middleware authenticates as.
	Username string
	// ProjectName is the project the service account authenticates within.
	ProjectName string
	// UserDomainName is the domain owning the service-account user.
	UserDomainName string
	// ProjectDomainName is the domain owning the service project.
	ProjectDomainName string
	// RegionName is the Keystone region the middleware targets. It is optional:
	// when empty the region_name option is omitted and oslo.config keeps its
	// compiled-in default.
	RegionName string
	// MemcachedServers is the comma-joined memcached server list the middleware
	// caches validated tokens in, already joined by the caller. It is optional:
	// when empty the memcached_servers option is omitted.
	MemcachedServers string
}

// Section returns the key/value map for the [keystone_authtoken] INI section.
// auth_type is fixed to "password"; the remaining always-present keys are taken
// from p. The optional region_name and memcached_servers keys are emitted only
// when their SectionParams fields are non-empty, so an unset field falls back to
// oslo.config's compiled-in default rather than an empty override.
//
// The map never contains a password key: the password arrives exclusively
// through the PasswordEnvVar env override, keeping the secret out of the
// rendered ConfigMap.
func Section(p SectionParams) map[string]string {
	section := map[string]string{
		"auth_type":            "password",
		"auth_url":             p.AuthURL,
		"www_authenticate_uri": p.WWWAuthenticateURI,
		"username":             p.Username,
		"project_name":         p.ProjectName,
		"user_domain_name":     p.UserDomainName,
		"project_domain_name":  p.ProjectDomainName,
	}
	if p.RegionName != "" {
		section["region_name"] = p.RegionName
	}
	if p.MemcachedServers != "" {
		section["memcached_servers"] = p.MemcachedServers
	}
	return section
}

// PasswordEnvVar returns the EnvVar that overrides [keystone_authtoken].password
// by sourcing the value from key within the named Secret. Every pod-spec builder
// that renders a [keystone_authtoken] section uses this helper so the override
// key and the Secret wiring stay in one place and the password is never written
// to the ConfigMap.
func PasswordEnvVar(secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: PasswordEnvVarName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: secretName,
				},
				Key: key,
			},
		},
	}
}
