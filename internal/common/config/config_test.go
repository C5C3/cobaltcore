package config

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestRenderINI(t *testing.T) {
	tests := []struct {
		name     string
		config   map[string]map[string]string
		expected string
	}{
		{
			name: "single section single key",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			expected: "[DEFAULT]\ndebug = true",
		},
		{
			name: "multiple sections sorted alphabetically",
			config: map[string]map[string]string{
				"zeta":    {"key": "z"},
				"alpha":   {"key": "a"},
				"middle":  {"key": "m"},
			},
			expected: "[alpha]\nkey = a\n\n[middle]\nkey = m\n\n[zeta]\nkey = z",
		},
		{
			name: "multiple keys sorted alphabetically",
			config: map[string]map[string]string{
				"section": {
					"zebra": "z",
					"apple": "a",
					"mango": "m",
				},
			},
			expected: "[section]\napple = a\nmango = m\nzebra = z",
		},
		{
			name:     "empty map returns empty string",
			config:   map[string]map[string]string{},
			expected: "",
		},
		{
			name:     "nil map returns empty string",
			config:   nil,
			expected: "",
		},
		{
			name: "idempotent output",
			config: map[string]map[string]string{
				"b": {"y": "2", "x": "1"},
				"a": {"z": "3"},
			},
			expected: "[a]\nz = 3\n\n[b]\nx = 1\ny = 2",
		},
		{
			name: "special characters in values preserved",
			config: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://user:p%40ss@host:3306/db"},
			},
			expected: "[database]\nconnection = mysql+pymysql://user:p%40ss@host:3306/db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result := RenderINI(tt.config)
			g.Expect(result).To(Equal(tt.expected))
		})
	}

	// Verify idempotency by calling twice with the same input.
	t.Run("idempotent across calls", func(t *testing.T) {
		g := NewGomegaWithT(t)
		config := map[string]map[string]string{
			"b": {"y": "2", "x": "1"},
			"a": {"z": "3"},
		}
		g.Expect(RenderINI(config)).To(Equal(RenderINI(config)))
	})
}

func TestMergeDefaults(t *testing.T) {
	tests := []struct {
		name       string
		userConfig map[string]map[string]string
		defaults   map[string]map[string]string
		expected   map[string]map[string]string
	}{
		{
			name: "user overrides default same key",
			userConfig: map[string]map[string]string{
				"DEFAULT": {"debug": "false"},
			},
			defaults: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			expected: map[string]map[string]string{
				"DEFAULT": {"debug": "false"},
			},
		},
		{
			name: "default fills missing key",
			userConfig: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			defaults: map[string]map[string]string{
				"DEFAULT": {"log_level": "INFO"},
			},
			expected: map[string]map[string]string{
				"DEFAULT": {"debug": "true", "log_level": "INFO"},
			},
		},
		{
			name: "user-only section preserved",
			userConfig: map[string]map[string]string{
				"custom": {"key": "val"},
			},
			defaults: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			expected: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
				"custom":  {"key": "val"},
			},
		},
		{
			name:       "default-only section added",
			userConfig: map[string]map[string]string{},
			defaults: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			expected: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
		},
		{
			name:       "both empty returns empty map",
			userConfig: map[string]map[string]string{},
			defaults:   map[string]map[string]string{},
			expected:   map[string]map[string]string{},
		},
		{
			name:       "nil inputs return empty map",
			userConfig: nil,
			defaults:   nil,
			expected:   map[string]map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result := MergeDefaults(tt.userConfig, tt.defaults)
			g.Expect(result).To(Equal(tt.expected))
		})
	}

	// Verify inputs are not mutated.
	t.Run("does not mutate original inputs", func(t *testing.T) {
		g := NewGomegaWithT(t)

		userConfig := map[string]map[string]string{
			"DEFAULT": {"debug": "false"},
		}
		defaults := map[string]map[string]string{
			"DEFAULT": {"log_level": "INFO"},
		}

		result := MergeDefaults(userConfig, defaults)

		// Result has merged data.
		g.Expect(result["DEFAULT"]).To(HaveLen(2))

		// Originals are unchanged.
		g.Expect(userConfig["DEFAULT"]).To(HaveLen(1))
		g.Expect(userConfig["DEFAULT"]["debug"]).To(Equal("false"))
		g.Expect(defaults["DEFAULT"]).To(HaveLen(1))
		g.Expect(defaults["DEFAULT"]["log_level"]).To(Equal("INFO"))
	})
}

func TestInjectSecrets(t *testing.T) {
	tests := []struct {
		name     string
		config   map[string]map[string]string
		secrets  map[string]string
		expected map[string]map[string]string
	}{
		{
			name: "pre-assembled connection string injected as-is",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			secrets: map[string]string{
				"database/connection": "mysql+pymysql://admin:secret@db.host:3306/mydb",
			},
			expected: map[string]map[string]string{
				"DEFAULT":  {"debug": "true"},
				"database": {"connection": "mysql+pymysql://admin:secret@db.host:3306/mydb"},
			},
		},
		{
			name: "individual parts assembled into connection string",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			secrets: map[string]string{
				"database/user":     "admin",
				"database/password": "secret",
				"database/host":     "db.host",
				"database/port":     "3306",
				"database/name":     "mydb",
			},
			expected: map[string]map[string]string{
				"DEFAULT":  {"debug": "true"},
				"database": {"connection": "mysql+pymysql://admin:secret@db.host:3306/mydb"},
			},
		},
		{
			name:   "special characters in password are URL-encoded per RFC 3986 userinfo",
			config: map[string]map[string]string{},
			secrets: map[string]string{
				"database/user":     "admin",
				"database/password": "p@ss:w/rd",
				"database/host":     "db.host",
				"database/port":     "3306",
				"database/name":     "mydb",
			},
			expected: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://admin:p%40ss%3Aw%2Frd@db.host:3306/mydb"},
			},
		},
		{
			name: "empty secrets returns config unchanged",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			secrets: map[string]string{},
			expected: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
		},
		{
			name:     "nil config works without panic",
			config:   nil,
			secrets:  map[string]string{"database/connection": "mysql+pymysql://u:p@h:3306/d"},
			expected: map[string]map[string]string{"database": {"connection": "mysql+pymysql://u:p@h:3306/d"}},
		},
		{
			name: "generic section/key secrets injected into config",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			secrets: map[string]string{
				"keystone_authtoken/password": "auth-secret",
				"cache/backend":              "dogpile.cache.memcached",
			},
			expected: map[string]map[string]string{
				"DEFAULT":             {"debug": "true"},
				"keystone_authtoken":  {"password": "auth-secret"},
				"cache":              {"backend": "dogpile.cache.memcached"},
			},
		},
		{
			name: "generic section/key merges into existing section",
			config: map[string]map[string]string{
				"cache": {"enabled": "true"},
			},
			secrets: map[string]string{
				"cache/backend": "dogpile.cache.memcached",
			},
			expected: map[string]map[string]string{
				"cache": {"enabled": "true", "backend": "dogpile.cache.memcached"},
			},
		},
		{
			name: "malformed secret key without slash is skipped",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			secrets: map[string]string{
				"noSlashKey": "ignored",
			},
			expected: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
		},
		{
			name: "generic secrets alongside database parts",
			config: map[string]map[string]string{},
			secrets: map[string]string{
				"database/user":               "admin",
				"database/password":           "secret",
				"database/host":               "db.host",
				"database/port":               "3306",
				"database/name":               "mydb",
				"keystone_authtoken/password":  "auth-secret",
			},
			expected: map[string]map[string]string{
				"database":            {"connection": "mysql+pymysql://admin:secret@db.host:3306/mydb"},
				"keystone_authtoken":  {"password": "auth-secret"},
			},
		},
		{
			name: "default port 3306 when database/port omitted",
			config: map[string]map[string]string{},
			secrets: map[string]string{
				"database/user":     "admin",
				"database/password": "secret",
				"database/host":     "db.host",
				"database/name":     "mydb",
			},
			expected: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://admin:secret@db.host:3306/mydb"},
			},
		},
		{
			name:   "default name equals user when database/name omitted",
			config: map[string]map[string]string{},
			secrets: map[string]string{
				"database/user":     "admin",
				"database/password": "secret",
				"database/host":     "db.host",
				"database/port":     "3306",
			},
			expected: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://admin:secret@db.host:3306/admin"},
			},
		},
		{
			name: "incomplete database parts do not produce connection string",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			secrets: map[string]string{
				"database/user": "admin",
				"database/host": "db.host",
				// Missing database/password — hasDatabaseParts requires user, password, and host.
			},
			expected: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
		},
		{
			name: "pre-assembled connection takes precedence over individual parts",
			config: map[string]map[string]string{},
			secrets: map[string]string{
				"database/connection": "mysql+pymysql://override:conn@other:3306/db",
				"database/user":       "admin",
				"database/password":   "secret",
				"database/host":       "db.host",
			},
			expected: map[string]map[string]string{
				"database": {"connection": "mysql+pymysql://override:conn@other:3306/db"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result := InjectSecrets(tt.config, tt.secrets)
			g.Expect(result).To(Equal(tt.expected))
		})
	}

	// Verify inputs are not mutated.
	t.Run("does not mutate original config", func(t *testing.T) {
		g := NewGomegaWithT(t)

		config := map[string]map[string]string{
			"DEFAULT": {"debug": "true"},
		}
		secrets := map[string]string{
			"database/connection": "mysql+pymysql://u:p@h:3306/d",
		}

		result := InjectSecrets(config, secrets)
		g.Expect(result).To(HaveKey("database"))
		g.Expect(config).NotTo(HaveKey("database"))
	})
}

func TestInjectOsloPolicyConfig(t *testing.T) {
	tests := []struct {
		name           string
		config         map[string]map[string]string
		policyFilePath string
		expected       map[string]map[string]string
	}{
		{
			name: "non-empty path adds oslo_policy section",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			policyFilePath: "/etc/nova/policy.yaml",
			expected: map[string]map[string]string{
				"DEFAULT":      {"debug": "true"},
				"oslo_policy":  {"policy_file": "/etc/nova/policy.yaml"},
			},
		},
		{
			name: "empty path returns config unchanged",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
			policyFilePath: "",
			expected: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
			},
		},
		{
			name:           "nil config works without panic",
			config:         nil,
			policyFilePath: "/etc/nova/policy.yaml",
			expected: map[string]map[string]string{
				"oslo_policy": {"policy_file": "/etc/nova/policy.yaml"},
			},
		},
		{
			name: "merges into existing oslo_policy section preserving caller keys",
			config: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
				"oslo_policy": {
					"enforce_scope":        "true",
					"enforce_new_defaults": "true",
				},
			},
			policyFilePath: "/etc/nova/policy.yaml",
			expected: map[string]map[string]string{
				"DEFAULT": {"debug": "true"},
				"oslo_policy": {
					"policy_file":          "/etc/nova/policy.yaml",
					"enforce_scope":        "true",
					"enforce_new_defaults": "true",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result := InjectOsloPolicyConfig(tt.config, tt.policyFilePath)
			g.Expect(result).To(Equal(tt.expected))
		})
	}

	// Verify inputs are not mutated.
	t.Run("does not mutate original config", func(t *testing.T) {
		g := NewGomegaWithT(t)

		config := map[string]map[string]string{
			"DEFAULT": {"debug": "true"},
		}

		result := InjectOsloPolicyConfig(config, "/etc/nova/policy.yaml")
		g.Expect(result).To(HaveKey("oslo_policy"))
		g.Expect(config).NotTo(HaveKey("oslo_policy"))
	})
}
