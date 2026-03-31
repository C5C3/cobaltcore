// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// configTestScheme returns a runtime.Scheme with core and Keystone types
// registered for reconcileConfig tests.
func configTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return s
}

// configTestKeystone returns a minimal Keystone CR for reconcileConfig tests
// using brownfield database and cache.
func configTestKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			Generation: 1,
			UID:        "test-uid-123",
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db-credentials"},
			},
			Cache: commonv1.CacheSpec{
				Backend: "dogpile.cache.pymemcache",
				Servers: []string{"mc-0:11211", "mc-1:11211"},
			},
			Fernet: keystonev1alpha1.FernetSpec{
				RotationSchedule: "0 0 * * 0",
				MaxActiveKeys:    3,
			},
			CredentialKeys: keystonev1alpha1.CredentialKeysSpec{
				RotationSchedule: "0 0 * * 0",
				MaxActiveKeys:    3,
			},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
		},
	}
}

// dbCredentialsSecret returns a Secret with username and password keys.
func dbCredentialsSecret(namespace, name, username, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"username": []byte(username),
			"password": []byte(password),
		},
	}
}

// newConfigTestReconciler creates a KeystoneReconciler for config tests.
func newConfigTestReconciler(s *runtime.Scheme, objs ...client.Object) *KeystoneReconciler {
	cb := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&keystonev1alpha1.Keystone{})
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

// getCreatedConfigMap retrieves the ConfigMap created by reconcileConfig.
func getCreatedConfigMap(ctx context.Context, c client.Client, namespace, name string) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		return nil, err
	}
	return cm, nil
}

func TestReconcileConfig_BasicManagedDatabaseAndCache(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	ks.Spec.Database = commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb-cluster"},
		Database:   "keystone",
		SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db-credentials"},
	}
	ks.Spec.Cache = commonv1.CacheSpec{
		Backend:    "dogpile.cache.pymemcache",
		ClusterRef: &corev1.LocalObjectReference{Name: "memcached-cluster"},
	}

	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "secret123")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(configMapName).To(HavePrefix("test-keystone-config-"))

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	keystoneConf := cm.Data["keystone.conf"]
	g.Expect(keystoneConf).To(ContainSubstring("[DEFAULT]"))
	g.Expect(keystoneConf).To(ContainSubstring("[token]"))
	g.Expect(keystoneConf).To(ContainSubstring("[fernet_tokens]"))
	g.Expect(keystoneConf).To(ContainSubstring("[cache]"))
	g.Expect(keystoneConf).To(ContainSubstring("[database]"))
	g.Expect(keystoneConf).To(ContainSubstring("[identity]"))
	g.Expect(keystoneConf).To(ContainSubstring("[oslo_middleware]"))
	g.Expect(keystoneConf).To(ContainSubstring("[memcache]"))
	g.Expect(keystoneConf).To(ContainSubstring("[credential]"))

	// Managed database: connection uses CR name as MySQL username and service DNS.
	g.Expect(keystoneConf).To(ContainSubstring("mysql+pymysql://test-keystone:secret123@mariadb-cluster.default.svc:3306/keystone?charset=utf8"))

	// Managed cache: uses Service DNS name.
	g.Expect(keystoneConf).To(ContainSubstring("memcached-cluster:11211"))

	// keystone.conf must contain [paste_deploy] with config_file (CC-0018).
	g.Expect(keystoneConf).To(ContainSubstring("[paste_deploy]"))
	g.Expect(keystoneConf).To(ContainSubstring("config_file = /etc/keystone/keystone.conf.d/api-paste.ini"))

	// api-paste.ini must contain complete PasteDeploy configuration (CC-0018).
	apiPaste := cm.Data["api-paste.ini"]
	g.Expect(apiPaste).To(ContainSubstring("[pipeline:public_api]"))
	g.Expect(apiPaste).To(ContainSubstring("[composite:main]"))
	g.Expect(apiPaste).To(ContainSubstring("[app:admin_service]"))
	g.Expect(apiPaste).To(ContainSubstring("use = egg:keystone#service_v3"))
	g.Expect(apiPaste).To(ContainSubstring("[filter:cors]"))
	g.Expect(apiPaste).To(ContainSubstring("[filter:sizelimit]"))
	g.Expect(apiPaste).To(ContainSubstring("[filter:request_id]"))
	g.Expect(apiPaste).To(ContainSubstring("[filter:url_normalize]"))
	g.Expect(apiPaste).To(ContainSubstring("[filter:http_proxy_to_wsgi]"))
}

func TestReconcileConfig_BrownfieldDatabase(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "ks_user", "ks_pass")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	keystoneConf := cm.Data["keystone.conf"]
	g.Expect(keystoneConf).To(ContainSubstring("mysql+pymysql://ks_user:ks_pass@db.example.com:3306/keystone?charset=utf8"))
}

func TestReconcileConfig_SpecialCharactersInCredentialsAreURLEncoded(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	// Credentials with special characters that could break a PyMySQL URL.
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "user@domain", "p@ss:w/rd")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	keystoneConf := cm.Data["keystone.conf"]
	// Special characters must be percent-encoded per RFC 3986 userinfo rules (CC-0013).
	// url.UserPassword encodes '@', ':', and '/' in the password component.
	g.Expect(keystoneConf).To(ContainSubstring("mysql+pymysql://user%40domain:p%40ss%3Aw%2Frd@db.example.com:3306/keystone?charset=utf8"),
		"special characters (@, :, /) in credentials must be percent-encoded for PyMySQL")
}

func TestReconcileConfig_BrownfieldCache(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	keystoneConf := cm.Data["keystone.conf"]
	g.Expect(keystoneConf).To(ContainSubstring("memcache_servers = mc-0:11211,mc-1:11211"))
}

func TestReconcileConfig_ManagedCacheCustomReplicas(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	ks.Spec.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Port:      3306,
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "keystone-db-credentials"},
	}
	ks.Spec.Cache = commonv1.CacheSpec{
		Backend:    "dogpile.cache.pymemcache",
		ClusterRef: &corev1.LocalObjectReference{Name: "mc"},
		Replicas:   5,
	}

	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	keystoneConf := cm.Data["keystone.conf"]
	// Managed cache: uses Service DNS name (replicas field unused for endpoint generation).
	g.Expect(keystoneConf).To(ContainSubstring("mc:11211"))
}

func TestReconcileConfig_PluginConfig(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	ks.Spec.Plugins = []commonv1.PluginSpec{
		{
			Name:          "keycloak-backend",
			ConfigSection: "keycloak",
			Config: map[string]string{
				"server_url": "https://keycloak.example.com",
				"realm":      "openstack",
			},
		},
	}
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	keystoneConf := cm.Data["keystone.conf"]
	g.Expect(keystoneConf).To(ContainSubstring("[keycloak]"))
	g.Expect(keystoneConf).To(ContainSubstring("server_url = https://keycloak.example.com"))
	g.Expect(keystoneConf).To(ContainSubstring("realm = openstack"))
}

func TestReconcileConfig_ExtraConfigOverridesDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	ks.Spec.ExtraConfig = map[string]map[string]string{
		"token": {
			"provider":   "jws",
			"expiration": "3600",
		},
		"custom_section": {
			"key1": "value1",
		},
	}
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	keystoneConf := cm.Data["keystone.conf"]
	// ExtraConfig overrides default token provider.
	g.Expect(keystoneConf).To(ContainSubstring("provider = jws"))
	g.Expect(keystoneConf).NotTo(ContainSubstring("provider = fernet"))
	// ExtraConfig adds new sections.
	g.Expect(keystoneConf).To(ContainSubstring("[custom_section]"))
	g.Expect(keystoneConf).To(ContainSubstring("key1 = value1"))
	// ExtraConfig adds new keys to existing sections.
	g.Expect(keystoneConf).To(ContainSubstring("expiration = 3600"))
}

func TestReconcileConfig_PolicyOverridesInlineRules(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	ks.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{
			"identity:create_user": "role:admin",
			"identity:list_users":  "role:admin or role:reader",
		},
	}
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	// policy.yaml should be present.
	policyYAML := cm.Data["policy.yaml"]
	g.Expect(policyYAML).NotTo(BeEmpty())
	g.Expect(policyYAML).To(ContainSubstring("identity:create_user"))
	g.Expect(policyYAML).To(ContainSubstring("role:admin"))

	// oslo_policy section should be in keystone.conf.
	keystoneConf := cm.Data["keystone.conf"]
	g.Expect(keystoneConf).To(ContainSubstring("[oslo_policy]"))
	g.Expect(keystoneConf).To(ContainSubstring("policy_file = /etc/keystone/policy.yaml"))
}

func TestReconcileConfig_PolicyOverridesConfigMapRef(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	ks.Spec.PolicyOverrides = &commonv1.PolicySpec{
		ConfigMapRef: &corev1.LocalObjectReference{Name: "external-policy"},
	}

	policyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "external-policy",
			Namespace: "default",
		},
		Data: map[string]string{
			"policy.yaml": "identity:get_user: \"role:reader\"\nidentity:delete_user: \"role:admin\"\n",
		},
	}
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret, policyCM)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	policyYAML := cm.Data["policy.yaml"]
	g.Expect(policyYAML).To(ContainSubstring("identity:get_user"))
	g.Expect(policyYAML).To(ContainSubstring("identity:delete_user"))

	keystoneConf := cm.Data["keystone.conf"]
	g.Expect(keystoneConf).To(ContainSubstring("[oslo_policy]"))
	g.Expect(keystoneConf).To(ContainSubstring("policy_file = /etc/keystone/policy.yaml"))
}

func TestReconcileConfig_Middleware(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	ks.Spec.Middleware = []commonv1.MiddlewareSpec{
		{
			Name:          "audit",
			FilterFactory: "keystonemiddleware.audit:filter_factory",
			Position:      commonv1.PipelinePositionAfter,
			Config: map[string]string{
				"audit_map_file": "/etc/keystone/api_audit_map.conf",
			},
		},
	}
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	apiPaste := cm.Data["api-paste.ini"]
	g.Expect(apiPaste).To(ContainSubstring("[pipeline:public_api]"))
	g.Expect(apiPaste).To(ContainSubstring("audit"))
	g.Expect(apiPaste).To(ContainSubstring("[filter:audit]"))
	g.Expect(apiPaste).To(ContainSubstring("paste.filter_factory = keystonemiddleware.audit:filter_factory"))
}

func TestReconcileConfig_ConfigMapNameContainsContentHash(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(configMapName).To(HavePrefix("test-keystone-config-"))
	// Name should be base name + "-" + 8-char hex hash.
	parts := strings.SplitN(configMapName, "test-keystone-config-", 2)
	g.Expect(parts).To(HaveLen(2))
	g.Expect(parts[1]).To(HaveLen(8))
}

func TestReconcileConfig_ImmutableConfigMapWithOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	// ConfigMap should be immutable.
	g.Expect(cm.Immutable).NotTo(BeNil())
	g.Expect(*cm.Immutable).To(BeTrue())

	// ConfigMap should have an owner reference pointing to the Keystone CR.
	g.Expect(cm.OwnerReferences).To(HaveLen(1))
	g.Expect(cm.OwnerReferences[0].Name).To(Equal("test-keystone"))
	g.Expect(cm.OwnerReferences[0].UID).To(Equal(ks.UID))
}

func TestReconcileConfig_CredentialKeysSectionPresent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	keystoneConf := cm.Data["keystone.conf"]
	// REQ-011 (CC-0036): [credential] section must be present with correct values.
	g.Expect(keystoneConf).To(ContainSubstring("[credential]"))
	g.Expect(keystoneConf).To(ContainSubstring("key_repository = /etc/keystone/credential-keys"))
	g.Expect(keystoneConf).To(ContainSubstring("max_active_keys = 3"))
}

func TestReconcileConfig_CredentialKeysCustomMaxActiveKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	ks.Spec.CredentialKeys.MaxActiveKeys = 7
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret)

	configMapName, err := r.reconcileConfig(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cm, err := getCreatedConfigMap(context.Background(), r.Client, "default", configMapName)
	g.Expect(err).NotTo(HaveOccurred())

	keystoneConf := cm.Data["keystone.conf"]
	g.Expect(keystoneConf).To(ContainSubstring("[credential]"))
	g.Expect(keystoneConf).To(ContainSubstring("max_active_keys = 7"))
}

func TestResolveDatabaseHost(t *testing.T) {
	tests := []struct {
		name     string
		keystone *keystonev1alpha1.Keystone
		expected string
	}{
		{
			name: "managed mode with default port",
			keystone: &keystonev1alpha1.Keystone{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: keystonev1alpha1.KeystoneSpec{
					Database: commonv1.DatabaseSpec{
						ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
						Port:       0,
					},
				},
			},
			expected: "mariadb.default.svc:3306",
		},
		{
			name: "managed mode with explicit port",
			keystone: &keystonev1alpha1.Keystone{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: keystonev1alpha1.KeystoneSpec{
					Database: commonv1.DatabaseSpec{
						ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
						Port:       3307,
					},
				},
			},
			expected: "mariadb.default.svc:3307",
		},
		{
			name: "brownfield with explicit port",
			keystone: &keystonev1alpha1.Keystone{
				Spec: keystonev1alpha1.KeystoneSpec{
					Database: commonv1.DatabaseSpec{
						Host: "db.example.com",
						Port: 3306,
					},
				},
			},
			expected: "db.example.com:3306",
		},
		{
			name: "brownfield with default port",
			keystone: &keystonev1alpha1.Keystone{
				Spec: keystonev1alpha1.KeystoneSpec{
					Database: commonv1.DatabaseSpec{
						Host: "db.example.com",
						Port: 0,
					},
				},
			},
			expected: "db.example.com:3306",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result := resolveDatabaseHost(tt.keystone)
			g.Expect(result).To(Equal(tt.expected))
		})
	}
}

func TestDBPort(t *testing.T) {
	tests := []struct {
		name     string
		keystone *keystonev1alpha1.Keystone
		expected int32
	}{
		{
			name: "explicit port",
			keystone: &keystonev1alpha1.Keystone{
				Spec: keystonev1alpha1.KeystoneSpec{
					Database: commonv1.DatabaseSpec{
						Port: 3307,
					},
				},
			},
			expected: 3307,
		},
		{
			name: "zero port defaults to 3306",
			keystone: &keystonev1alpha1.Keystone{
				Spec: keystonev1alpha1.KeystoneSpec{
					Database: commonv1.DatabaseSpec{
						Port: 0,
					},
				},
			},
			expected: 3306,
		},
		{
			name: "negative port defaults to 3306",
			keystone: &keystonev1alpha1.Keystone{
				Spec: keystonev1alpha1.KeystoneSpec{
					Database: commonv1.DatabaseSpec{
						Port: -1,
					},
				},
			},
			expected: 3306,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result := dbPort(tt.keystone)
			g.Expect(result).To(Equal(tt.expected))
		})
	}
}

func TestResolveCacheServers(t *testing.T) {
	tests := []struct {
		name     string
		keystone *keystonev1alpha1.Keystone
		expected string
	}{
		{
			name: "brownfield with multiple servers",
			keystone: &keystonev1alpha1.Keystone{
				Spec: keystonev1alpha1.KeystoneSpec{
					Cache: commonv1.CacheSpec{
						Servers: []string{"mc-0:11211", "mc-1:11211"},
					},
				},
			},
			expected: "mc-0:11211,mc-1:11211",
		},
		{
			name: "brownfield with single server",
			keystone: &keystonev1alpha1.Keystone{
				Spec: keystonev1alpha1.KeystoneSpec{
					Cache: commonv1.CacheSpec{
						Servers: []string{"mc:11211"},
					},
				},
			},
			expected: "mc:11211",
		},
		{
			name: "managed mode uses service DNS name",
			keystone: &keystonev1alpha1.Keystone{
				Spec: keystonev1alpha1.KeystoneSpec{
					Cache: commonv1.CacheSpec{
						ClusterRef: &corev1.LocalObjectReference{Name: "memcached"},
					},
				},
			},
			expected: "memcached:11211",
		},
		{
			name: "managed mode with different cluster name",
			keystone: &keystonev1alpha1.Keystone{
				Spec: keystonev1alpha1.KeystoneSpec{
					Cache: commonv1.CacheSpec{
						ClusterRef: &corev1.LocalObjectReference{Name: "mc"},
					},
				},
			},
			expected: "mc:11211",
		},
		{
			name: "neither ClusterRef nor Servers set",
			keystone: &keystonev1alpha1.Keystone{
				Spec: keystonev1alpha1.KeystoneSpec{
					Cache: commonv1.CacheSpec{},
				},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result := resolveCacheServers(tt.keystone)
			g.Expect(result).To(Equal(tt.expected))
		})
	}
}
