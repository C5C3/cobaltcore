# Pattern: Content-hash naming for immutable resources

**Component**: internal/common/config/
**Category**: configuration
**Applies-When**: Creating ConfigMaps whose content must trigger rolling restarts when changed

## Description

ConfigMaps are named {base}-{hash[:8]} where hash is SHA-256 of sorted key=value pairs. This ensures: (1) identical content produces identical names (idempotent), (2) different content produces different names (triggers rolling restart via pod template annotation), (3) old ConfigMaps are preserved until garbage-collected via owner references.

## Examples

### `internal/common/config/configmap.go:68-80`

```go
func contentHash(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, data[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", sum[:4])
}
```

