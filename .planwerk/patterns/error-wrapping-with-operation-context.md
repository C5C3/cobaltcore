# Pattern: Error wrapping with operation context

**Component**: internal/common/ all packages
**Category**: error-handling
**Applies-When**: Returning errors from K8s API calls or field manipulation

## Description

All errors are wrapped with fmt.Errorf using %w and include: the operation being performed, the resource namespace/name, and the original error. Pattern: fmt.Errorf("<verb>ing <Resource> %s/%s: %w", namespace, name, err). For unstructured field operations: fmt.Errorf("setting <Resource> <field.path>: %w", err).

## Examples

### `internal/common/secrets/externalsecret.go:26-28`

```go
return false, fmt.Errorf("getting ExternalSecret %s/%s: %w", namespace, name, err)
```

### `internal/common/database/database.go:60-62`

```go
if err := unstructured.SetNestedField(obj.Object, mariaDbRefMap, "spec", "mariaDbRef"); err != nil {
	return fmt.Errorf("setting Database spec.mariaDbRef: %w", err)
}
```

