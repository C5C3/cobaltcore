// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package keystoneauth

import (
	"reflect"
	"testing"
)

func TestSection(t *testing.T) {
	tests := []struct {
		name   string
		params SectionParams
		want   map[string]string
		absent []string
	}{
		{
			name: "full params render every key",
			params: SectionParams{
				AuthURL:            "http://keystone.svc:5000/v3",
				WWWAuthenticateURI: "https://keystone.example.com/v3",
				Username:           "glance",
				ProjectName:        "service",
				UserDomainName:     "Default",
				ProjectDomainName:  "Default",
				RegionName:         "RegionOne",
				MemcachedServers:   "mc-0:11211,mc-1:11211",
			},
			want: map[string]string{
				"auth_type":            "password",
				"auth_url":             "http://keystone.svc:5000/v3",
				"www_authenticate_uri": "https://keystone.example.com/v3",
				"username":             "glance",
				"project_name":         "service",
				"user_domain_name":     "Default",
				"project_domain_name":  "Default",
				"region_name":          "RegionOne",
				"memcached_servers":    "mc-0:11211,mc-1:11211",
			},
		},
		{
			name: "minimal params omit optional keys",
			params: SectionParams{
				AuthURL:            "http://keystone.svc:5000/v3",
				WWWAuthenticateURI: "https://keystone.example.com/v3",
				Username:           "glance",
				ProjectName:        "service",
				UserDomainName:     "Default",
				ProjectDomainName:  "Default",
			},
			want: map[string]string{
				"auth_type":            "password",
				"auth_url":             "http://keystone.svc:5000/v3",
				"www_authenticate_uri": "https://keystone.example.com/v3",
				"username":             "glance",
				"project_name":         "service",
				"user_domain_name":     "Default",
				"project_domain_name":  "Default",
			},
			absent: []string{"region_name", "memcached_servers"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Section(tc.params)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Section() = %#v, want %#v", got, tc.want)
			}
			for _, k := range tc.absent {
				if _, ok := got[k]; ok {
					t.Errorf("Section() unexpectedly contains key %q", k)
				}
			}
		})
	}
}

// TestSectionNeverRendersPassword asserts the no-password invariant: the
// password is delivered exclusively through PasswordEnvVar, so it must never
// leak into the rendered section, which feeds a plaintext ConfigMap.
func TestSectionNeverRendersPassword(t *testing.T) {
	cases := map[string]SectionParams{
		"zero value": {},
		"full params": {
			AuthURL:            "http://keystone.svc:5000/v3",
			WWWAuthenticateURI: "https://keystone.example.com/v3",
			Username:           "glance",
			ProjectName:        "service",
			UserDomainName:     "Default",
			ProjectDomainName:  "Default",
			RegionName:         "RegionOne",
			MemcachedServers:   "mc:11211",
		},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := Section(params)["password"]; ok {
				t.Errorf("Section() rendered a password key, want none")
			}
		})
	}
}

func TestPasswordEnvVar(t *testing.T) {
	env := PasswordEnvVar("glance-keystone", "password")

	if env.Name != PasswordEnvVarName {
		t.Errorf("EnvVar name = %q, want %q", env.Name, PasswordEnvVarName)
	}
	if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("EnvVar is not sourced from a Secret key: %#v", env)
	}
	sel := env.ValueFrom.SecretKeyRef
	if sel.Name != "glance-keystone" {
		t.Errorf("SecretKeySelector name = %q, want %q", sel.Name, "glance-keystone")
	}
	if sel.Key != "password" {
		t.Errorf("SecretKeySelector key = %q, want %q", sel.Key, "password")
	}
}

func TestServiceUserSection(t *testing.T) {
	tests := []struct {
		name   string
		params SectionParams
		want   map[string]string
		absent []string
	}{
		{
			name: "full params render every key",
			params: SectionParams{
				AuthURL:           "http://keystone.svc:5000/v3",
				Username:          "cinder",
				ProjectName:       "service",
				UserDomainName:    "Default",
				ProjectDomainName: "Default",
				RegionName:        "RegionOne",
			},
			want: map[string]string{
				"send_service_user_token": "true",
				"auth_type":               "password",
				"auth_url":                "http://keystone.svc:5000/v3",
				"username":                "cinder",
				"project_name":            "service",
				"user_domain_name":        "Default",
				"project_domain_name":     "Default",
				"region_name":             "RegionOne",
			},
		},
		{
			name: "empty region omits region_name",
			params: SectionParams{
				AuthURL:           "http://keystone.svc:5000/v3",
				Username:          "cinder",
				ProjectName:       "service",
				UserDomainName:    "Default",
				ProjectDomainName: "Default",
			},
			want: map[string]string{
				"send_service_user_token": "true",
				"auth_type":               "password",
				"auth_url":                "http://keystone.svc:5000/v3",
				"username":                "cinder",
				"project_name":            "service",
				"user_domain_name":        "Default",
				"project_domain_name":     "Default",
			},
			absent: []string{"region_name"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ServiceUserSection(tc.params)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ServiceUserSection() = %#v, want %#v", got, tc.want)
			}
			for _, k := range tc.absent {
				if _, ok := got[k]; ok {
					t.Errorf("ServiceUserSection() unexpectedly contains key %q", k)
				}
			}
		})
	}
}

// TestServiceUserSectionNeverRendersPassword asserts the no-password invariant
// for [service_user] and pins the two token-middleware options the section must
// not inherit from Section: even a SectionParams with every field set renders
// neither www_authenticate_uri nor memcached_servers.
func TestServiceUserSectionNeverRendersPassword(t *testing.T) {
	got := ServiceUserSection(SectionParams{
		AuthURL:            "http://keystone.svc:5000/v3",
		WWWAuthenticateURI: "https://keystone.example.com/v3",
		Username:           "cinder",
		ProjectName:        "service",
		UserDomainName:     "Default",
		ProjectDomainName:  "Default",
		RegionName:         "RegionOne",
		MemcachedServers:   "mc:11211",
	})

	for _, k := range []string{"password", "www_authenticate_uri", "memcached_servers"} {
		if _, ok := got[k]; ok {
			t.Errorf("ServiceUserSection() unexpectedly contains key %q", k)
		}
	}
}

func TestServiceUserPasswordEnvVar(t *testing.T) {
	tests := []struct {
		name       string
		secretName string
		key        string
	}{
		{name: "named secret and key", secretName: "cinder-keystone", key: "password"},
		{name: "empty secret and key pass through verbatim", secretName: "", key: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := ServiceUserPasswordEnvVar(tc.secretName, tc.key)

			if env.Name != ServiceUserPasswordEnvVarName {
				t.Errorf("EnvVar name = %q, want %q", env.Name, ServiceUserPasswordEnvVarName)
			}
			if env.Name != "OS_SERVICE_USER__PASSWORD" {
				t.Errorf("EnvVar name = %q, want %q", env.Name, "OS_SERVICE_USER__PASSWORD")
			}
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				t.Fatalf("EnvVar is not sourced from a Secret key: %#v", env)
			}
			sel := env.ValueFrom.SecretKeyRef
			if sel.Name != tc.secretName {
				t.Errorf("SecretKeySelector name = %q, want %q", sel.Name, tc.secretName)
			}
			if sel.Key != tc.key {
				t.Errorf("SecretKeySelector key = %q, want %q", sel.Key, tc.key)
			}
		})
	}
}
