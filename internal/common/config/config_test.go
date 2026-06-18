// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"reflect"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRenderINI(t *testing.T) {
	tests := []struct {
		name     string
		sections map[string]map[string]string
		want     string
	}{
		{
			name:     "nil sections",
			sections: nil,
			want:     "",
		},
		{
			name:     "empty sections map",
			sections: map[string]map[string]string{},
			want:     "",
		},
		{
			name: "single section with single key",
			sections: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			want: "[DEFAULT]\ndebug = true\n",
		},
		{
			name: "single section with multiple keys sorted alphabetically",
			sections: map[string]map[string]string{
				"DEFAULT": {"debug": "true", "admin_token": "secret", "log_file": "/var/log/app.log"},
			},
			want: "[DEFAULT]\nadmin_token = secret\ndebug = true\nlog_file = /var/log/app.log\n",
		},
		{
			name: "multiple sections sorted alphabetically",
			sections: map[string]map[string]string{
				"database": {"connection": "mysql://localhost/db"},
				"DEFAULT":  {"debug": "true"},
			},
			want: "[DEFAULT]\ndebug = true\n\n[database]\nconnection = mysql://localhost/db\n",
		},
		{
			name: "section with empty key-value map",
			sections: map[string]map[string]string{
				"DEFAULT": {},
			},
			want: "[DEFAULT]\n",
		},
		{
			name: "multiple sections with multiple keys",
			sections: map[string]map[string]string{
				"DEFAULT":  {"debug": "true", "log_file": "/var/log/app.log"},
				"database": {"connection": "mysql://localhost/db", "max_retries": "-1"},
				"cache":    {"backend": "dogpile.cache.memcached", "enabled": "true"},
			},
			want: "[DEFAULT]\ndebug = true\nlog_file = /var/log/app.log\n\n" +
				"[cache]\nbackend = dogpile.cache.memcached\nenabled = true\n\n" +
				"[database]\nconnection = mysql://localhost/db\nmax_retries = -1\n",
		},
		{
			name: "values with special characters",
			sections: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://user:p@ss=word@host:3306/db?charset=utf8"},
			},
			want: "[database]\nconnection = mysql+pymysql://user:p@ss=word@host:3306/db?charset=utf8\n",
		},
		{
			name: "percent in value is preserved for oslo.config compatibility",
			sections: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://user:p%25ssword@host:3306/db"},
			},
			want: "[database]\nconnection = mysql+pymysql://user:p%25ssword@host:3306/db\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(RenderINI(tt.sections)).To(Equal(tt.want))
		})
	}
}

func TestMergeDefaults(t *testing.T) {
	tests := []struct {
		name       string
		userConfig map[string]map[string]string
		defaults   map[string]map[string]string
		want       map[string]map[string]string
	}{
		{
			name:       "nil user config and nil defaults",
			userConfig: nil,
			defaults:   nil,
			want:       map[string]map[string]string{},
		},
		{
			name:       "nil user config with defaults",
			userConfig: nil,
			defaults: map[string]map[string]string{
				"DEFAULT": {"debug": "false"},
			},
			want: map[string]map[string]string{
				"DEFAULT": {"debug": "false"},
			},
		},
		{
			name: "user config with nil defaults",
			userConfig: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			defaults: nil,
			want: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
		},
		{
			name: "user value overrides default",
			userConfig: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			defaults: map[string]map[string]string{
				"DEFAULT": {"debug": "false"},
			},
			want: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
		},
		{
			name: "user adds new key to existing default section",
			userConfig: map[string]map[string]string{
				"DEFAULT": {"custom_key": "custom_value"},
			},
			defaults: map[string]map[string]string{
				"DEFAULT": {"debug": "false"},
			},
			want: map[string]map[string]string{
				"DEFAULT": {"debug": "false", "custom_key": "custom_value"},
			},
		},
		{
			name: "user adds entirely new section",
			userConfig: map[string]map[string]string{
				"custom_section": {"key": "value"},
			},
			defaults: map[string]map[string]string{
				"DEFAULT": {"debug": "false"},
			},
			want: map[string]map[string]string{
				"DEFAULT":        {"debug": "false"},
				"custom_section": {"key": "value"},
			},
		},
		{
			name: "complex merge with overlapping and non-overlapping sections",
			userConfig: map[string]map[string]string{
				"DEFAULT":  {"debug": "true"},
				"database": {"max_retries": "5"},
			},
			defaults: map[string]map[string]string{
				"DEFAULT":  {"debug": "false", "log_file": "/var/log/app.log"},
				"database": {"connection": "sqlite:///local.db", "max_retries": "-1"},
				"cache":    {"enabled": "true"},
			},
			want: map[string]map[string]string{
				"DEFAULT":  {"debug": "true", "log_file": "/var/log/app.log"},
				"database": {"connection": "sqlite:///local.db", "max_retries": "5"},
				"cache":    {"enabled": "true"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(MergeDefaults(tt.userConfig, tt.defaults)).To(Equal(tt.want))
		})
	}
}

func TestMergeDefaults_doesNotMutateInputs(t *testing.T) {
	g := NewGomegaWithT(t)

	userConfig := map[string]map[string]string{
		"DEFAULT": {"debug": "true"},
	}
	defaults := map[string]map[string]string{
		"DEFAULT": {"debug": "false", "log_file": "/var/log/app.log"},
	}

	result := MergeDefaults(userConfig, defaults)

	// Mutating the result should not affect the inputs.
	result["DEFAULT"]["debug"] = "mutated"
	result["DEFAULT"]["new_key"] = "new_value"

	g.Expect(userConfig["DEFAULT"]["debug"]).To(Equal("true"))
	g.Expect(defaults["DEFAULT"]["debug"]).To(Equal("false"))
	g.Expect(userConfig["DEFAULT"]).NotTo(HaveKey("new_key"))
	g.Expect(defaults["DEFAULT"]).NotTo(HaveKey("new_key"))
}

func TestInjectSecrets(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]map[string]string
		secrets map[string]string
		want    map[string]map[string]string
	}{
		{
			name:    "nil config and nil secrets",
			config:  nil,
			secrets: nil,
			want:    map[string]map[string]string{},
		},
		{
			name: "no placeholders in config",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			secrets: map[string]string{"DB_PASSWORD": "secret"},
			want: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
		},
		{
			name: "single placeholder replaced",
			config: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://nova:{{DB_PASSWORD}}@host:3306/nova"},
			},
			secrets: map[string]string{"DB_PASSWORD": "s3cret"},
			want: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://nova:s3cret@host:3306/nova"},
			},
		},
		{
			name: "multiple placeholders in single value",
			config: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://{{DB_USER}}:{{DB_PASSWORD}}@host:3306/db"},
			},
			secrets: map[string]string{"DB_USER": "nova", "DB_PASSWORD": "s3cret"},
			want: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://nova:s3cret@host:3306/db"},
			},
		},
		{
			name: "unresolved placeholder left as-is",
			config: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://{{DB_USER}}:{{DB_PASSWORD}}@host:3306/db"},
			},
			secrets: map[string]string{"DB_USER": "nova"},
			want: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://nova:{{DB_PASSWORD}}@host:3306/db"},
			},
		},
		{
			name: "empty secrets map leaves placeholders unchanged",
			config: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://{{DB_USER}}:{{DB_PASSWORD}}@host:3306/db"},
			},
			secrets: map[string]string{},
			want: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://{{DB_USER}}:{{DB_PASSWORD}}@host:3306/db"},
			},
		},
		{
			name: "placeholders across multiple sections",
			config: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://nova:{{DB_PASSWORD}}@host:3306/nova"},
				"DEFAULT":  {"transport_url": "rabbit://nova:{{RABBIT_PASSWORD}}@rabbit:5672/nova"},
			},
			secrets: map[string]string{"DB_PASSWORD": "dbpass", "RABBIT_PASSWORD": "rmqpass"},
			want: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://nova:dbpass@host:3306/nova"},
				"DEFAULT":  {"transport_url": "rabbit://nova:rmqpass@rabbit:5672/nova"},
			},
		},
		{
			name: "value that is only a placeholder",
			config: map[string]map[string]string{
				"DEFAULT": {"secret_key": "{{API_KEY}}"},
			},
			secrets: map[string]string{"API_KEY": "abc123"},
			want: map[string]map[string]string{
				"DEFAULT": {"secret_key": "abc123"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(InjectSecrets(tt.config, tt.secrets)).To(Equal(tt.want))
		})
	}
}

func TestInjectSecrets_doesNotMutateInput(t *testing.T) {
	g := NewGomegaWithT(t)

	config := map[string]map[string]string{
		"database": {"connection": "mysql+pymysql://nova:{{DB_PASSWORD}}@host:3306/nova"},
	}
	secrets := map[string]string{"DB_PASSWORD": "s3cret"}

	result := InjectSecrets(config, secrets)

	// Original config should still have the placeholder.
	g.Expect(config["database"]["connection"]).To(Equal("mysql+pymysql://nova:{{DB_PASSWORD}}@host:3306/nova"))
	// Result should have the replaced value.
	g.Expect(result["database"]["connection"]).To(Equal("mysql+pymysql://nova:s3cret@host:3306/nova"))
}

func TestInjectOsloPolicyConfig(t *testing.T) {
	tests := []struct {
		name           string
		config         map[string]map[string]string
		policyFilePath string
		want           map[string]map[string]string
	}{
		{
			name:           "nil config with non-empty path creates oslo_policy section",
			config:         nil,
			policyFilePath: "/etc/keystone/policy.yaml",
			want: map[string]map[string]string{
				"oslo_policy": {"policy_file": "/etc/keystone/policy.yaml"},
			},
		},
		{
			name:           "empty path is a no-op on empty config",
			config:         map[string]map[string]string{},
			policyFilePath: "",
			want:           map[string]map[string]string{},
		},
		{
			name: "empty path is a no-op and preserves existing config",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			policyFilePath: "",
			want: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
		},
		{
			name:           "adds oslo_policy section to empty config",
			config:         map[string]map[string]string{},
			policyFilePath: "/etc/keystone/policy.yaml",
			want: map[string]map[string]string{
				"oslo_policy": {"policy_file": "/etc/keystone/policy.yaml"},
			},
		},
		{
			name: "adds oslo_policy section alongside existing sections",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			policyFilePath: "/etc/nova/policy.yaml",
			want: map[string]map[string]string{
				"DEFAULT":     {"debug": "true"},
				"oslo_policy": {"policy_file": "/etc/nova/policy.yaml"},
			},
		},
		{
			name: "preserves existing keys in oslo_policy section",
			config: map[string]map[string]string{
				"oslo_policy": {"policy_dirs": "/etc/nova/policy.d"},
			},
			policyFilePath: "/etc/nova/policy.yaml",
			want: map[string]map[string]string{
				"oslo_policy": {
					"policy_dirs": "/etc/nova/policy.d",
					"policy_file": "/etc/nova/policy.yaml",
				},
			},
		},
		{
			name: "overwrites existing policy_file value",
			config: map[string]map[string]string{
				"oslo_policy": {"policy_file": "/old/path/policy.yaml"},
			},
			policyFilePath: "/etc/nova/policy.yaml",
			want: map[string]map[string]string{
				"oslo_policy": {"policy_file": "/etc/nova/policy.yaml"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result := InjectOsloPolicyConfig(tt.config, tt.policyFilePath)
			g.Expect(result).To(Equal(tt.want))
		})
	}
}

func TestInjectOsloPolicyConfig_doesNotMutateInput(t *testing.T) {
	g := NewGomegaWithT(t)

	config := map[string]map[string]string{
		"DEFAULT": {"debug": "true"},
	}

	result := InjectOsloPolicyConfig(config, "/etc/nova/policy.yaml")

	// Original config should not have oslo_policy section.
	g.Expect(config).NotTo(HaveKey("oslo_policy"))
	// Result should have it.
	g.Expect(result["oslo_policy"]["policy_file"]).To(Equal("/etc/nova/policy.yaml"))
}

func TestInjectOsloPolicyConfig_emptyPathReturnsOriginalReference(t *testing.T) {
	g := NewGomegaWithT(t)

	config := map[string]map[string]string{
		"DEFAULT": {"debug": "true"},
	}

	result := InjectOsloPolicyConfig(config, "")

	// Empty path must return the exact same map reference (documented no-copy contract).
	// Note: Go maps are not comparable with ==, so BeIdenticalTo cannot be used.
	// Instead we compare the underlying map header pointers via reflect.
	g.Expect(reflect.ValueOf(result).Pointer()).To(Equal(reflect.ValueOf(config).Pointer()))

	// Mutating result must therefore also mutate config (caller beware).
	result["mutated"] = map[string]string{"key": "val"}
	g.Expect(config).To(HaveKey("mutated"))
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       "test-uid",
		},
	}
}

func TestCreateImmutableConfigMap_creates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
	ctx := context.Background()

	data := map[string]string{"key": "value"}
	name, err := CreateImmutableConfigMap(ctx, c, s, owner, "my-config", "default", data)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).To(HavePrefix("my-config-"))
	g.Expect(name).NotTo(Equal("my-config-"))

	// Verify the ConfigMap was actually created.
	var cm corev1.ConfigMap
	g.Expect(c.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, &cm)).To(Succeed())
	g.Expect(cm.Data).To(Equal(data))
	g.Expect(*cm.Immutable).To(BeTrue())
	g.Expect(cm.Labels).To(HaveKeyWithValue(ConfigBaseLabelKey, "my-config"))
	g.Expect(cm.OwnerReferences).To(HaveLen(1))
	g.Expect(cm.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestCreateImmutableConfigMap_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
	ctx := context.Background()

	data := map[string]string{"key": "value"}
	name1, err := CreateImmutableConfigMap(ctx, c, s, owner, "my-config", "default", data)
	g.Expect(err).NotTo(HaveOccurred())

	name2, err := CreateImmutableConfigMap(ctx, c, s, owner, "my-config", "default", data)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name2).To(Equal(name1))
}

func TestCreateImmutableConfigMap_deterministic(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
	ctx := context.Background()

	data := map[string]string{"a": "1", "b": "2"}
	name1, err := CreateImmutableConfigMap(ctx, c, s, owner, "cfg", "default", data)
	g.Expect(err).NotTo(HaveOccurred())

	// Second call with same data must produce the same name.
	name2, err := CreateImmutableConfigMap(ctx, c, s, owner, "cfg", "default", data)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name2).To(Equal(name1))
}

func TestCreateImmutableConfigMap_differentData(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
	ctx := context.Background()

	name1, err := CreateImmutableConfigMap(ctx, c, s, owner, "cfg", "default", map[string]string{"a": "1"})
	g.Expect(err).NotTo(HaveOccurred())

	name2, err := CreateImmutableConfigMap(ctx, c, s, owner, "cfg", "default", map[string]string{"a": "2"})
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(name1).NotTo(Equal(name2))
}

func TestCreateImmutableConfigMap_newlineInValueIsUnambiguous(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	// A single key whose value contains an embedded newline that could look
	// like a second key=value entry under a naive encoding.
	c1 := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
	name1, err := CreateImmutableConfigMap(ctx, c1, s, owner, "cfg", "default",
		map[string]string{"key1": "x\nb=y"})
	g.Expect(err).NotTo(HaveOccurred())

	// Two separate keys whose naive encoding would collide with the above.
	c2 := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
	name2, err := CreateImmutableConfigMap(ctx, c2, s, owner, "cfg", "default",
		map[string]string{"key1": "x", "b": "y"})
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(name1).NotTo(Equal(name2), "length-prefixed encoding must distinguish values with embedded newlines")
}

func TestCreateImmutableConfigMap_rejectsUnownedExisting(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	data := map[string]string{"key": "value"}

	// Create once to learn the content-hashed name.
	c1 := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
	name, err := CreateImmutableConfigMap(ctx, c1, s, owner, "my-config", "default", data)
	g.Expect(err).NotTo(HaveOccurred())

	// Pre-create a ConfigMap with the same name but a different controller owner.
	isController := true
	conflicting := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       "other-owner",
				UID:        "other-uid",
				Controller: &isController,
			}},
		},
	}

	c2 := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, conflicting).Build()
	_, err = CreateImmutableConfigMap(ctx, c2, s, owner, "my-config", "default", data)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("not owned by"))
}

func ownedConfigMap(name, namespace, baseName string, owner *corev1.ConfigMap, creationTime time.Time) *corev1.ConfigMap {
	isController := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(creationTime),
			Labels: map[string]string{
				ConfigBaseLabelKey: baseName,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       owner.Name,
				UID:        owner.UID,
				Controller: &isController,
			}},
		},
	}
}

func TestPruneImmutableConfigMaps_deletesStaleConfigMaps(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-current1"

	// 5 historical ConfigMaps + 1 current
	cm1 := ownedConfigMap("my-config-aaaaaaaa", "default", "my-config", owner, baseTime)
	cm2 := ownedConfigMap("my-config-bbbbbbbb", "default", "my-config", owner, baseTime.Add(1*time.Hour))
	cm3 := ownedConfigMap("my-config-cccccccc", "default", "my-config", owner, baseTime.Add(2*time.Hour))
	cm4 := ownedConfigMap("my-config-dddddddd", "default", "my-config", owner, baseTime.Add(3*time.Hour))
	cm5 := ownedConfigMap("my-config-eeeeeeee", "default", "my-config", owner, baseTime.Add(4*time.Hour))
	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime.Add(5*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, cm1, cm2, cm3, cm4, cm5, current).Build()

	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 3)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	// 3 newest historical + current = 4 remaining
	g.Expect(remaining).To(HaveLen(4))
	g.Expect(remaining).To(ContainElement(currentName))
	g.Expect(remaining).To(ContainElement("my-config-cccccccc"))
	g.Expect(remaining).To(ContainElement("my-config-dddddddd"))
	g.Expect(remaining).To(ContainElement("my-config-eeeeeeee"))
	// 2 oldest should be deleted
	g.Expect(remaining).NotTo(ContainElement("my-config-aaaaaaaa"))
	g.Expect(remaining).NotTo(ContainElement("my-config-bbbbbbbb"))
}

func TestPruneImmutableConfigMaps_retainsNewestByCreationTimestamp(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-current1"

	// Names sort alphabetically: aaa < bbb < zzz
	// But timestamps are: zzz (oldest), aaa (middle), bbb (newest)
	cmZzz := ownedConfigMap("my-config-zzzzzzzz", "default", "my-config", owner, baseTime)
	cmAaa := ownedConfigMap("my-config-aaaaaaaa", "default", "my-config", owner, baseTime.Add(2*time.Hour))
	cmBbb := ownedConfigMap("my-config-bbbbbbbb", "default", "my-config", owner, baseTime.Add(4*time.Hour))
	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime.Add(6*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, cmZzz, cmAaa, cmBbb, current).Build()

	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 1)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	// retain=1: newest historical (bbb) + current = 2
	g.Expect(remaining).To(HaveLen(2))
	g.Expect(remaining).To(ContainElement(currentName))
	g.Expect(remaining).To(ContainElement("my-config-bbbbbbbb"))
	// zzz and aaa (older by timestamp) should be deleted
	g.Expect(remaining).NotTo(ContainElement("my-config-zzzzzzzz"))
	g.Expect(remaining).NotTo(ContainElement("my-config-aaaaaaaa"))
}

func TestPruneImmutableConfigMaps_retainZeroDeletesAllHistorical(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-current1"

	cm1 := ownedConfigMap("my-config-aaaaaaaa", "default", "my-config", owner, baseTime)
	cm2 := ownedConfigMap("my-config-bbbbbbbb", "default", "my-config", owner, baseTime.Add(1*time.Hour))
	cm3 := ownedConfigMap("my-config-cccccccc", "default", "my-config", owner, baseTime.Add(2*time.Hour))
	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime.Add(3*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, cm1, cm2, cm3, current).Build()

	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 0)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	// Only current should remain
	g.Expect(remaining).To(HaveLen(1))
	g.Expect(remaining).To(ContainElement(currentName))
}

func TestPruneImmutableConfigMaps_idempotentOnSecondCall(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-current1"

	cm1 := ownedConfigMap("my-config-aaaaaaaa", "default", "my-config", owner, baseTime)
	cm2 := ownedConfigMap("my-config-bbbbbbbb", "default", "my-config", owner, baseTime.Add(1*time.Hour))
	cm3 := ownedConfigMap("my-config-cccccccc", "default", "my-config", owner, baseTime.Add(2*time.Hour))
	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime.Add(3*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, cm1, cm2, cm3, current).Build()

	// First call: should delete cm1 (oldest), retain cm2, cm3, current
	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 2)
	g.Expect(err).NotTo(HaveOccurred())

	// Second call: nothing more to delete
	err = PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 2)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	g.Expect(remaining).To(HaveLen(3))
	g.Expect(remaining).To(ContainElement(currentName))
	g.Expect(remaining).To(ContainElement("my-config-bbbbbbbb"))
	g.Expect(remaining).To(ContainElement("my-config-cccccccc"))
}

func TestPruneImmutableConfigMaps_ignoresNotFoundOnDelete(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-current1"

	cm1 := ownedConfigMap("my-config-aaaaaaaa", "default", "my-config", owner, baseTime)
	cm2 := ownedConfigMap("my-config-bbbbbbbb", "default", "my-config", owner, baseTime.Add(1*time.Hour))
	cm3 := ownedConfigMap("my-config-cccccccc", "default", "my-config", owner, baseTime.Add(2*time.Hour))
	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime.Add(3*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, cm1, cm2, cm3, current).Build()

	// Pre-delete cm1 to simulate it being removed between list and delete
	g.Expect(c.Delete(ctx, cm1)).To(Succeed())

	// Prune with retain=1: should try to delete cm1 (already gone) and succeed
	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 1)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestPruneImmutableConfigMaps_skipsConfigMapsOwnedByDifferentController(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-current1"

	// ConfigMap owned by a different controller
	otherOwner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-owner",
			Namespace: "default",
			UID:       "other-uid",
		},
	}
	cmOther := ownedConfigMap("my-config-aaaaaaaa", "default", "my-config", otherOwner, baseTime)
	cmOwned := ownedConfigMap("my-config-bbbbbbbb", "default", "my-config", owner, baseTime.Add(1*time.Hour))
	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime.Add(2*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, otherOwner, cmOther, cmOwned, current).Build()

	// retain=0 should only delete owner's historical ConfigMaps
	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 0)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" && cm.Name != "other-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	// cmOther (different owner) + current = 2
	g.Expect(remaining).To(HaveLen(2))
	g.Expect(remaining).To(ContainElement("my-config-aaaaaaaa"))
	g.Expect(remaining).To(ContainElement(currentName))
}

func TestPruneImmutableConfigMaps_skipsConfigMapsWithoutOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-current1"

	// Unowned ConfigMap with matching label but no owner reference.
	unowned := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-config-unowned1",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(baseTime),
			Labels: map[string]string{
				ConfigBaseLabelKey: "my-config",
			},
		},
	}
	cmOwned := ownedConfigMap("my-config-bbbbbbbb", "default", "my-config", owner, baseTime.Add(1*time.Hour))
	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime.Add(2*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, unowned, cmOwned, current).Build()

	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 0)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	// unowned + current = 2
	g.Expect(remaining).To(HaveLen(2))
	g.Expect(remaining).To(ContainElement("my-config-unowned1"))
	g.Expect(remaining).To(ContainElement(currentName))
}

func TestPruneImmutableConfigMaps_neverDeletesCurrentConfigMap(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-cccccccc"

	cm1 := ownedConfigMap("my-config-aaaaaaaa", "default", "my-config", owner, baseTime)
	cm2 := ownedConfigMap("my-config-bbbbbbbb", "default", "my-config", owner, baseTime.Add(1*time.Hour))
	// current is in the candidate set but must never be deleted
	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime.Add(2*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, cm1, cm2, current).Build()

	// retain=0: delete all historical, but never current
	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 0)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	g.Expect(remaining).To(HaveLen(1))
	g.Expect(remaining).To(ContainElement(currentName))
}

func TestPruneImmutableConfigMaps_noopWhenFewerThanRetain(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-current1"

	cm1 := ownedConfigMap("my-config-aaaaaaaa", "default", "my-config", owner, baseTime)
	cm2 := ownedConfigMap("my-config-bbbbbbbb", "default", "my-config", owner, baseTime.Add(1*time.Hour))
	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime.Add(2*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, cm1, cm2, current).Build()

	// retain=3 with only 2 historical: no deletions
	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 3)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	g.Expect(remaining).To(HaveLen(3))
}

func TestPruneImmutableConfigMaps_noopWhenNoHistoricalConfigMaps(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-current1"

	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime)

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, current).Build()

	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 3)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	g.Expect(remaining).To(HaveLen(1))
	g.Expect(remaining).To(ContainElement(currentName))
}

func TestPruneImmutableConfigMaps_skipsMismatchedPrefix(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-current1"

	// ConfigMap with different baseName prefix and label value.
	cmOther := ownedConfigMap("other-config-aaaaaaaa", "default", "other-config", owner, baseTime)
	cmOwned := ownedConfigMap("my-config-bbbbbbbb", "default", "my-config", owner, baseTime.Add(1*time.Hour))
	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime.Add(2*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, cmOther, cmOwned, current).Build()

	// retain=0 should only delete "my-config-" prefixed historical ones
	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, 0)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	// other-config-aaaaaaaa + current = 2
	g.Expect(remaining).To(HaveLen(2))
	g.Expect(remaining).To(ContainElement("other-config-aaaaaaaa"))
	g.Expect(remaining).To(ContainElement(currentName))
}

func TestPruneImmutableConfigMaps_handlesOverlappingPrefixCorrectly(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "test-config-current1"

	// "test-config-extra-def12345" has prefix "test-config-" but NOT "test-config-extra-"
	// When baseName is "test-config", it matches "test-config-abc12345" and "test-config-extra-def12345"
	// because both have prefix "test-config-"
	// But when baseName is "test-config-extra", it should NOT match "test-config-abc12345"
	cmMatch := ownedConfigMap("test-config-abc12345", "default", "test-config", owner, baseTime)
	cmOverlap := ownedConfigMap("test-config-extra-def12345", "default", "test-config-extra", owner, baseTime.Add(1*time.Hour))
	current := ownedConfigMap(currentName, "default", "test-config", owner, baseTime.Add(2*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, cmMatch, cmOverlap, current).Build()

	// Prune with baseName "test-config-extra": should only match "test-config-extra-def12345"
	err := PruneImmutableConfigMaps(ctx, c, owner, "test-config-extra", "default", currentName, 0)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	// test-config-abc12345 (not matching prefix "test-config-extra-") + current = 2
	g.Expect(remaining).To(HaveLen(2))
	g.Expect(remaining).To(ContainElement("test-config-abc12345"))
	g.Expect(remaining).To(ContainElement(currentName))
	g.Expect(remaining).NotTo(ContainElement("test-config-extra-def12345"))
}

// Verify negative retain is clamped to 0, deleting all historical ConfigMaps.
func TestPruneImmutableConfigMaps_negativeRetainClampedToZero(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	ctx := context.Background()

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentName := "my-config-current1"

	cm1 := ownedConfigMap("my-config-aaaaaaaa", "default", "my-config", owner, baseTime)
	cm2 := ownedConfigMap("my-config-bbbbbbbb", "default", "my-config", owner, baseTime.Add(1*time.Hour))
	current := ownedConfigMap(currentName, "default", "my-config", owner, baseTime.Add(2*time.Hour))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, cm1, cm2, current).Build()

	// Negative retain should behave like retain=0: delete all historical, keep current.
	err := PruneImmutableConfigMaps(ctx, c, owner, "my-config", "default", currentName, -5)
	g.Expect(err).NotTo(HaveOccurred())

	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace("default"))).To(Succeed())

	remaining := make([]string, 0)
	for _, cm := range cmList.Items {
		if cm.Name != "test-owner" {
			remaining = append(remaining, cm.Name)
		}
	}
	// Only current should remain — all historical deleted.
	g.Expect(remaining).To(HaveLen(1))
	g.Expect(remaining).To(ContainElement(currentName))
}
