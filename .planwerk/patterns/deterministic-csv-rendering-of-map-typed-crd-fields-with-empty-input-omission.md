# Pattern: Deterministic CSV rendering of map-typed CRD fields with empty-input omission

**Component**: operators/keystone/internal/controller
**Category**: data-access
**Applies-When**: Rendering a Go map[string]string from a CRD spec into a string-valued config key where Go's randomized map iteration order would otherwise break the immutable ConfigMap content-hash invariant

## Description

Helper functions that flatten a map into a CSV/INI value sort the keys with sort.Strings before formatting `key=value` pairs, then strings.Join with a fixed separator. Empty-input maps return an empty string so the caller can omit the config key entirely (rather than emitting an empty CSV that would override compiled-in defaults of the consumer). Pinned by an explicit two-spec test that builds the same logical input from differently-ordered map literals and asserts identical ConfigMap names — the strongest available proxy for randomized iteration order under fake-client tests.

## Examples

### `operators/keystone/internal/controller/reconcile_config.go:305-325`

```go
// renderDefaultLogLevels formats PerLoggerLevels as oslo.log's
// default_log_levels CSV ("name=LEVEL,..."), with keys sorted alphabetically
// so the rendered keystone.conf — and therefore the immutable ConfigMap
// content hash — is independent of Go's randomized map iteration order
// (CC-0098, REQ-005). Returns "" for empty input so the caller can omit the
// key entirely rather than overriding oslo.log defaults with an empty list.
func renderDefaultLogLevels(perLogger map[string]string) string {
    if len(perLogger) == 0 {
        return ""
    }
    keys := make([]string, 0, len(perLogger))
    for k := range perLogger {
        keys = append(keys, k)
    }
    sort.Strings(keys)
    pairs := make([]string, 0, len(keys))
    for _, k := range keys {
        pairs = append(pairs, fmt.Sprintf("%s=%s", k, perLogger[k]))
    }
    return strings.Join(pairs, ",")
}
```

### `operators/keystone/internal/controller/reconcile_config_test.go:1075-1114`

```go
func TestReconcileConfig_LoggingPerLoggerLevelsDeterministicOrder(t *testing.T) {
    // ... two specs with the same logical PerLoggerLevels but different
    // map-literal orderings ...
    cmName, err := r.reconcileConfig(context.Background(), ks)
    g.Expect(err).NotTo(HaveOccurred())
    g.Expect(cm.Data["keystone.conf"]).To(ContainSubstring(
        "default_log_levels = amqp=ERROR,keystone.middleware=DEBUG,sqlalchemy.engine=WARNING"))

    // Second reconcile with reordered map literal must produce the same name.
    cm2Name, err := r.reconcileConfig(context.Background(), ks2)
    g.Expect(err).NotTo(HaveOccurred())
    g.Expect(cm2Name).To(Equal(cmName),
        "identical PerLoggerLevels with different map iteration order must yield the same ConfigMap name")
}
```

