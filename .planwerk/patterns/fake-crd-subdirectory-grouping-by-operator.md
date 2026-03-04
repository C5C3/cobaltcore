# Pattern: Fake CRD subdirectory grouping by operator

**Component**: internal/common/testutil/fake_crds/*/
**Category**: testing
**Applies-When**: Adding new external operator CRDs for envtest integration tests

## Description

Fake CRD YAML files are organized into subdirectories named after the external operator that owns them (e.g., mariadb-operator/, cert-manager/, external-secrets/). Each subdirectory contains only the CRDs belonging to that operator. The fakeCRDsDirs() function in setup.go dynamically discovers these subdirectories at runtime. Adding a new operator requires only creating a new subdirectory with its CRD YAMLs — no code changes needed.

## Examples

### `internal/common/testutil/envtest/setup.go:107`

```go
func fakeCRDsDirs() []string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("envtest: runtime.Caller failed to determine source file path")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "fake_crds")

	entries, err := os.ReadDir(root)
	if err != nil {
		panic(fmt.Sprintf("envtest: failed to read fake_crds directory %s: %v", root, err))
	}

	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	if len(dirs) == 0 {
		panic(fmt.Sprintf("envtest: no subdirectories found in fake_crds directory %s", root))
	}
	return dirs
}
```

### `internal/common/testutil/fake_crds/doc.go:10`

```go
// CRDs are grouped by controller in subdirectories:
//
//	cert-manager/        — cert-manager.io CRDs (Certificate, ClusterIssuer)
//	external-secrets/    — external-secrets.io CRDs (ExternalSecret, PushSecret, ClusterSecretStore)
//	mariadb-operator/    — k8s.mariadb.com CRDs (MariaDB, Database, Grant, User)
//	memcached-operator/  — cache.c5c3.io CRDs (Memcached)
//	rabbitmq-operator/   — rabbitmq.com CRDs (RabbitmqCluster)
```

