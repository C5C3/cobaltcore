package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// RenderINI produces a deterministic INI-format string from a nested config map.
// Sections and keys are sorted alphabetically to ensure stable ConfigMap content hashes. (CC-0004, REQ-005)
func RenderINI(config map[string]map[string]string) string {
	if len(config) == 0 {
		return ""
	}

	sections := make([]string, 0, len(config))
	for section := range config {
		sections = append(sections, section)
	}
	sort.Strings(sections)

	var b strings.Builder
	for i, section := range sections {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%s]\n", section)

		keys := make([]string, 0, len(config[section]))
		for key := range config[section] {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		for _, key := range keys {
			fmt.Fprintf(&b, "%s = %s\n", key, config[section][key])
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// MergeDefaults merges defaults into userConfig with user-wins precedence.
// User values always override defaults. Sections and keys from defaults fill gaps. (CC-0004, REQ-006)
func MergeDefaults(userConfig, defaults map[string]map[string]string) map[string]map[string]string {
	result := make(map[string]map[string]string)

	// Copy all defaults first.
	for section, kvs := range defaults {
		result[section] = make(map[string]string, len(kvs))
		for k, v := range kvs {
			result[section][k] = v
		}
	}

	// Overlay user config (user wins).
	for section, kvs := range userConfig {
		if _, ok := result[section]; !ok {
			result[section] = make(map[string]string, len(kvs))
		}
		for k, v := range kvs {
			result[section][k] = v
		}
	}

	return result
}

// InjectSecrets assembles connection strings from resolved secret values and injects
// them into the config. The secrets map keys follow the pattern "section/key" and values
// are the resolved secret values. (CC-0004, REQ-007)
//
// Supported connection patterns:
//   - database/connection: mysql+pymysql://{user}:{password}@{host}:{port}/{database}
//
// Special characters in password are URL-encoded.
func InjectSecrets(config map[string]map[string]string, secrets map[string]string) map[string]map[string]string {
	result := copyConfig(config)

	if len(secrets) == 0 {
		return result
	}

	// Check if a pre-assembled database/connection string is provided.
	if connStr, ok := secrets["database/connection"]; ok {
		if result["database"] == nil {
			result["database"] = make(map[string]string)
		}
		result["database"]["connection"] = connStr
	} else if hasDatabaseParts(secrets) {
		// Assemble from individual parts.
		if result["database"] == nil {
			result["database"] = make(map[string]string)
		}
		result["database"]["connection"] = assembleDatabaseConnection(secrets)
	}

	// Inject all other "section/key" entries.
	for secretKey, secretVal := range secrets {
		if isDatabasePart(secretKey) {
			continue
		}
		parts := strings.SplitN(secretKey, "/", 2)
		if len(parts) != 2 {
			continue
		}
		section, key := parts[0], parts[1]
		if result[section] == nil {
			result[section] = make(map[string]string)
		}
		result[section][key] = secretVal
	}

	return result
}

// InjectOsloPolicyConfig merges the policy_file key into the [oslo_policy] section,
// preserving any existing keys (e.g. enforce_scope, enforce_new_defaults).
// Does nothing if policyFilePath is empty. (CC-0004, REQ-008)
func InjectOsloPolicyConfig(config map[string]map[string]string, policyFilePath string) map[string]map[string]string {
	result := copyConfig(config)

	if policyFilePath == "" {
		return result
	}

	if result["oslo_policy"] == nil {
		result["oslo_policy"] = make(map[string]string)
	}
	result["oslo_policy"]["policy_file"] = policyFilePath

	return result
}

// copyConfig returns a deep copy of the config map.
func copyConfig(config map[string]map[string]string) map[string]map[string]string {
	result := make(map[string]map[string]string)
	for section, kvs := range config {
		result[section] = make(map[string]string, len(kvs))
		for k, v := range kvs {
			result[section][k] = v
		}
	}
	return result
}

// defaultMySQLPort is the default port for MySQL/MariaDB connections.
const defaultMySQLPort = "3306"

var databasePartKeys = map[string]bool{
	"database/user":     true,
	"database/password": true,
	"database/host":     true,
	"database/port":     true,
	"database/name":     true,
}

func isDatabasePart(key string) bool {
	return databasePartKeys[key] || key == "database/connection"
}

func hasDatabaseParts(secrets map[string]string) bool {
	_, hasUser := secrets["database/user"]
	_, hasPassword := secrets["database/password"]
	_, hasHost := secrets["database/host"]
	return hasUser && hasPassword && hasHost
}

func assembleDatabaseConnection(secrets map[string]string) string {
	user := secrets["database/user"]
	password := escapePassword(secrets["database/password"])
	host := secrets["database/host"]
	port := secrets["database/port"]
	name := secrets["database/name"]

	if port == "" {
		port = defaultMySQLPort
	}
	if name == "" {
		name = user
	}

	return fmt.Sprintf("mysql+pymysql://%s:%s@%s:%s/%s", user, password, host, port, name)
}

// escapePassword percent-encodes special characters in a password for safe
// embedding in a connection URI userinfo component per RFC 3986.
// Uses url.UserPassword to correctly handle all reserved characters
// including @, :, /, +, $, ;, and =.
func escapePassword(password string) string {
	// url.UserPassword("x", password).String() produces "x:encoded_password".
	// We extract just the encoded password portion after the first colon.
	userinfo := url.UserPassword("x", password).String()
	_, encoded, _ := strings.Cut(userinfo, ":")
	return encoded
}

// CreateImmutableConfigMap creates an immutable ConfigMap with content-hash naming.
// The ConfigMap name is formed as "{name}-{hash8}" where hash8 is the first 8 hex
// characters of the SHA-256 hash of the data (keys sorted for determinism).
// If a ConfigMap with that name already exists, it is returned as-is (idempotent).
// Owner references are set on the ConfigMap using the provided owner and scheme. (CC-0005, REQ-001, REQ-009, REQ-010)
func CreateImmutableConfigMap(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, name, namespace string, data map[string]string) (string, error) {
	hash := contentHash(data)
	cmName := fmt.Sprintf("%s-%s", name, hash)

	immutable := true
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: namespace,
		},
		Immutable: &immutable,
		Data:      data,
	}

	if err := controllerutil.SetControllerReference(owner, cm, scheme); err != nil {
		return "", fmt.Errorf("setting controller reference: %w", err)
	}

	// Create-then-check-AlreadyExists avoids a TOCTOU race between concurrent
	// reconcilers that could both observe NotFound and then race on Create.
	err := c.Create(ctx, cm)
	if apierrors.IsAlreadyExists(err) {
		return cmName, nil
	}
	if err != nil {
		return "", fmt.Errorf("creating ConfigMap %s/%s: %w", namespace, cmName, err)
	}

	return cmName, nil
}

// contentHash computes a deterministic SHA-256 hash of key-value data.
// Keys are sorted alphabetically before hashing. Returns the first 8 hex characters.
func contentHash(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, data[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}
