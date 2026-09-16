// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package keystoneauth renders the [keystone_authtoken] and [service_user]
// sections of an OpenStack service's oslo.config INI file and the client
// sections it calls other services through, such as [placement], together with
// the passwords the service account authenticates with. Every OpenStack service
// other than Keystone validates incoming API tokens through
// [keystone_authtoken]; services that call other services on a user's behalf
// additionally send their own token from [service_user] and authenticate each
// call from the client section of the service called. The operator renders the
// non-secret options into the service's shared ConfigMap and injects the
// passwords separately through oslo.config's OS_<GROUP>__<OPTION> env override
// so the secrets never land in the ConfigMap.
package keystoneauth

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// PasswordEnvVarName is the oslo.config env override key for
// [keystone_authtoken].password. The OS_<GROUP>__<OPTION> form wins over the
// ConfigMap value at runtime, so service containers read the service-account
// password from a Secret via PasswordEnvVar instead of from the rendered
// ConfigMap, keeping the secret out of the config.
const PasswordEnvVarName = "OS_KEYSTONE_AUTHTOKEN__PASSWORD" //nolint:gosec // G101 false positive: env var name, not a credential.

// SectionParams carries the non-secret options a service operator renders into
// [keystone_authtoken], [service_user] and its client sections: the three share
// one service account. The password is deliberately absent: it is delivered
// exclusively through the env overrides (PasswordEnvVar,
// ServiceUserPasswordEnvVar, ClientPasswordEnvVar) so it never lands in the
// rendered ConfigMap.
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
	return ClientPasswordEnvVar("keystone_authtoken", secretName, key)
}

// ServiceUserPasswordEnvVarName is the oslo.config env override key for
// [service_user].password. Like PasswordEnvVarName it wins over the ConfigMap
// value at runtime, so the service reads its own service-account password from
// a Secret via ServiceUserPasswordEnvVar rather than from the rendered
// ConfigMap.
const ServiceUserPasswordEnvVarName = "OS_SERVICE_USER__PASSWORD" //nolint:gosec // G101 false positive: env var name, not a credential.

// ServiceUserSection returns the key/value map for the [service_user] INI
// section. The section makes the service send a token of its own alongside the
// token of the user whose request it is serving, which the receiving service
// needs to accept a long-running request whose user token has since expired
// (Cinder calling Glance or Barbican, for example). send_service_user_token is
// fixed to "true" and auth_type to "password"; the credentials are taken from
// p and are the same service account [keystone_authtoken] uses. region_name is
// emitted only when p.RegionName is non-empty, so an unset field falls back to
// oslo.config's compiled-in default rather than an empty override.
//
// The map never contains a password key: the password arrives exclusively
// through the ServiceUserPasswordEnvVar env override, keeping the secret out of
// the rendered ConfigMap. Unlike Section, the map carries neither
// www_authenticate_uri nor memcached_servers: both belong to the token
// middleware, not to the outgoing service token.
func ServiceUserSection(p SectionParams) map[string]string {
	section := ClientSection(p)
	section["send_service_user_token"] = "true"
	return section
}

// ServiceUserPasswordEnvVar returns the EnvVar that overrides
// [service_user].password by sourcing the value from key within the named
// Secret. Every pod-spec builder that renders a [service_user] section uses this
// helper so the override key and the Secret wiring stay in one place and the
// password is never written to the ConfigMap.
func ServiceUserPasswordEnvVar(secretName, key string) corev1.EnvVar {
	return ClientPasswordEnvVar("service_user", secretName, key)
}

// ClientSection returns the key/value map for the INI section a service renders
// to talk to another service as a client, such as [placement], [neutron] or
// [cinder]. auth_type is fixed to "password" and the remaining always-present
// keys are taken from p. region_name is emitted only when p.RegionName is
// non-empty, so an unset field falls back to oslo.config's compiled-in default
// rather than an empty override.
//
// The map never contains a password key: the password stays env-injected
// through ClientPasswordEnvVar, which oslo.config reads from the
// OS_<SECTION>__<OPTION> override, so the secret never lands in the rendered
// ConfigMap. Unlike Section and ServiceUserSection, the map carries none of
// www_authenticate_uri, memcached_servers and send_service_user_token: those
// options belong to the token middleware and to the outgoing service token, not
// to an outgoing client call.
func ClientSection(p SectionParams) map[string]string {
	section := map[string]string{
		"auth_type":           "password",
		"auth_url":            p.AuthURL,
		"username":            p.Username,
		"project_name":        p.ProjectName,
		"user_domain_name":    p.UserDomainName,
		"project_domain_name": p.ProjectDomainName,
	}
	if p.RegionName != "" {
		section["region_name"] = p.RegionName
	}
	return section
}

// ClientPasswordEnvVar returns the EnvVar that overrides the password option of
// the client section named by section, sourcing the value from key within the
// named Secret. The name follows oslo.config's OS_<SECTION>__<OPTION> override
// form, so section "placement" yields OS_PLACEMENT__PASSWORD. Every pod-spec
// builder that renders a client section uses this helper so the password stays
// env-injected and is never written to the ConfigMap, with one env var per
// client section it renders. PasswordEnvVar and ServiceUserPasswordEnvVar are
// this helper for the [keystone_authtoken] and [service_user] sections.
func ClientPasswordEnvVar(section, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: "OS_" + strings.ToUpper(section) + "__PASSWORD",
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
