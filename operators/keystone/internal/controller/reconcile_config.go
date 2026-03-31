// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/plugins"
	"github.com/c5c3/forge/internal/common/policy"
	"github.com/c5c3/forge/internal/common/secrets"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0013

// reconcileConfig builds the Keystone configuration and creates an immutable
// ConfigMap containing keystone.conf, api-paste.ini, and optionally policy.yaml.
// It returns the name of the created ConfigMap (with content-hash suffix).
func (r *KeystoneReconciler) reconcileConfig(ctx context.Context, keystone *keystonev1alpha1.Keystone) (string, error) {
	// Step 1: Build keystone.conf INI sections from CRD spec.
	defaults := map[string]map[string]string{
		"DEFAULT": {
			"keystone_user":  "",
			"keystone_group": "",
		},
		"token": {
			"provider": "fernet",
		},
		"fernet_tokens": {
			"key_repository":  "/etc/keystone/fernet-keys",
			"max_active_keys": fmt.Sprintf("%d", normalizedFernetMaxActiveKeys(keystone)),
		},
		"credential": {
			"key_repository":  "/etc/keystone/credential-keys",
			"max_active_keys": fmt.Sprintf("%d", normalizedCredentialMaxActiveKeys(keystone)),
		},
		"cache": {
			"enabled": "true",
			"backend": keystone.Spec.Cache.Backend,
		},
		"paste_deploy": {
			"config_file": "/etc/keystone/keystone.conf.d/api-paste.ini",
		},
		"oslo_middleware": {
			"enable_proxy_headers_parsing": "true",
		},
		"identity": {
			"default_domain_id": "default",
		},
		"database": {
			"max_retries":            "-1",
			"connection_recycle_time": "600",
			"connection":             "{{DB_CONNECTION}}",
		},
	}

	// Step 2: Resolve cache servers.
	serverList := resolveCacheServers(keystone)
	defaults["cache"]["memcache_servers"] = serverList
	defaults["memcache"] = map[string]string{
		"servers": serverList,
	}

	// Step 3: Resolve database connection string.
	// In managed mode the MariaDB User CR name (= keystone.Name) is the MySQL
	// username, so the connection string must use that same value.
	// In brownfield mode no User CR exists; the secret's username is used.
	var username string
	if keystone.Spec.Database.ClusterRef != nil {
		username = keystone.Name
	} else {
		u, err := secrets.GetSecretValue(ctx, r.Client, client.ObjectKey{
			Namespace: keystone.Namespace,
			Name:      keystone.Spec.Database.SecretRef.Name,
		}, "username")
		if err != nil {
			return "", fmt.Errorf("reading database username: %w", err)
		}
		username = u
	}

	password, err := secrets.GetSecretValue(ctx, r.Client, client.ObjectKey{
		Namespace: keystone.Namespace,
		Name:      keystone.Spec.Database.SecretRef.Name,
	}, "password")
	if err != nil {
		return "", fmt.Errorf("reading database password: %w", err)
	}

	// url.UserPassword is used instead of url.PathEscape because PathEscape does not escape '@' or ':',
	// which are delimiters in the userinfo component per RFC 3986. url.UserPassword handles all
	// reserved characters ('@', '/', '?', ':') correctly for database connection strings.
	connURL := &url.URL{
		Scheme:   "mysql+pymysql",
		User:     url.UserPassword(username, password),
		Host:     resolveDatabaseHost(keystone),
		Path:     keystone.Spec.Database.Database,
		RawQuery: "charset=utf8",
	}

	defaults = config.InjectSecrets(defaults, map[string]string{
		"DB_CONNECTION": connURL.String(),
	})

	merged := defaults

	// Step 4: Merge plugin config.
	if len(keystone.Spec.Plugins) > 0 {
		pluginConfig, err := plugins.RenderPluginConfig(keystone.Spec.Plugins)
		if err != nil {
			return "", fmt.Errorf("rendering plugin config: %w", err)
		}
		merged = config.MergeDefaults(pluginConfig, defaults)
	}

	// Step 5: Merge extraConfig (extraConfig overrides everything).
	if keystone.Spec.ExtraConfig != nil {
		merged = config.MergeDefaults(keystone.Spec.ExtraConfig, merged)
	}

	// Step 6: Handle PolicyOverrides.
	var policyYAML string
	if keystone.Spec.PolicyOverrides != nil {
		policyYAML, err = buildPolicyYAML(ctx, r.Client, keystone)
		if err != nil {
			return "", fmt.Errorf("building policy: %w", err)
		}
		if policyYAML != "" {
			merged = config.InjectOsloPolicyConfig(merged, "/etc/keystone/policy.yaml")
		}
	}

	// Step 7: Render api-paste.ini.
	apiPasteINI, err := plugins.RenderPastePipelineINI(plugins.PipelineSpec{
		PipelineName: "public_api",
		AppName:      "admin_service",
		AppFactory:   "egg:keystone#service_v3",
		BaseFilters:  []string{"cors", "sizelimit", "http_proxy_to_wsgi", "url_normalize", "request_id"},
		BaseFilterFactories: map[string]string{
			"cors":               "egg:oslo.middleware#cors",
			"sizelimit":          "egg:oslo.middleware#sizelimit",
			"http_proxy_to_wsgi": "egg:oslo.middleware#http_proxy_to_wsgi",
			"url_normalize":      "egg:keystone#url_normalize",
			"request_id":         "egg:oslo.middleware#request_id",
		},
		BaseFilterConfigs: map[string]map[string]string{
			"cors": {"oslo_config_project": "keystone"},
		},
		CompositeRoutes: map[string]string{"/v3": "public_api"},
		Middleware:       keystone.Spec.Middleware,
	})
	if err != nil {
		return "", fmt.Errorf("rendering api-paste.ini: %w", err)
	}

	// Step 8: Create immutable ConfigMap.
	data := map[string]string{
		"keystone.conf": config.RenderINI(merged),
		"api-paste.ini": apiPasteINI,
	}
	if policyYAML != "" {
		data["policy.yaml"] = policyYAML
	}

	configMapName, err := config.CreateImmutableConfigMap(ctx, r.Client, r.Scheme, keystone,
		fmt.Sprintf("%s-config", keystone.Name), keystone.Namespace, data)
	if err != nil {
		return "", fmt.Errorf("creating config ConfigMap: %w", err)
	}

	return configMapName, nil
}

// resolveCacheServers returns the memcache server list based on the cache spec.
// In brownfield mode (Servers set), it joins them with commas.
// In managed mode (ClusterRef set), it constructs endpoints from the cluster name.
func resolveCacheServers(keystone *keystonev1alpha1.Keystone) string {
	if len(keystone.Spec.Cache.Servers) > 0 {
		return strings.Join(keystone.Spec.Cache.Servers, ",")
	}
	if keystone.Spec.Cache.ClusterRef != nil {
		// The memcached operator provisions a Deployment + headless Service.
		// Use the Service DNS name which resolves to all pod IPs.
		return fmt.Sprintf("%s:11211", keystone.Spec.Cache.ClusterRef.Name)
	}
	return ""
}

// resolveDatabaseHost returns the database host:port based on the database spec.
// In managed mode (ClusterRef set), it constructs a service DNS name.
// In brownfield mode (Host set), it uses the explicit host:port.
func resolveDatabaseHost(keystone *keystonev1alpha1.Keystone) string {
	if keystone.Spec.Database.ClusterRef != nil {
		return fmt.Sprintf("%s.%s.svc:%d",
			keystone.Spec.Database.ClusterRef.Name,
			keystone.Namespace,
			dbPort(keystone))
	}
	return fmt.Sprintf("%s:%d", keystone.Spec.Database.Host, dbPort(keystone))
}

// dbPort returns the database port, defaulting to 3306 if not set.
func dbPort(keystone *keystonev1alpha1.Keystone) int32 {
	if keystone.Spec.Database.Port > 0 {
		return keystone.Spec.Database.Port
	}
	return 3306
}

// buildPolicyYAML builds the policy.yaml content from PolicyOverrides.
func buildPolicyYAML(ctx context.Context, c client.Client, keystone *keystonev1alpha1.Keystone) (string, error) {
	po := keystone.Spec.PolicyOverrides
	if po == nil {
		return "", nil
	}

	var rules map[string]string

	// Load external policy from ConfigMap if set.
	if po.ConfigMapRef != nil {
		cmRules, err := policy.LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{
			Namespace: keystone.Namespace,
			Name:      po.ConfigMapRef.Name,
		})
		if err != nil {
			return "", fmt.Errorf("loading policy from ConfigMap: %w", err)
		}
		rules = cmRules
	}

	// Merge inline rules over external rules (inline wins).
	if len(po.Rules) > 0 {
		if rules == nil {
			rules = make(map[string]string)
		}
		for k, v := range po.Rules {
			rules[k] = v
		}
	}

	return policy.RenderPolicyYAML(rules)
}
