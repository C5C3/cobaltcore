# Pattern: Per-CR OpenBao RemoteKey builder embedding namespace+name path segments

**Component**: operators/c5c3/internal/controller, operators/keystone/internal/controller
**Category**: data-access
**Applies-When**: Building an ESO PushSecret RemoteRef.RemoteKey (or its read consumer) that mirrors a per-CR credential to the cluster-global OpenBao backend — the path must embed the owning CR's {namespace}/{name} so two CRs never clobber a shared leaf

## Description

Each credential family computes its OpenBao RemoteKey from the owning CR's Namespace and Name as explicit path segments (openstack/keystone/{ns}/{name}/... or bootstrap/{ns}/{name}/admin) instead of a package-level constant or single CR field. The matching OpenBao ACL grants the shape with one '+' single-segment glob per CR axis (kv-v2/{data,metadata}/.../+/+/<leaf>) so the grant still terminates at the literal leaf and never widens to a trailing '*'. Distinctness is locked by tests that assert the new key AND assert NotTo(Equal(legacy/superseded key)). This PR (CC-0112) extends the CC-0093 per-name layout by adding the leading {namespace} segment across all four families.

## Examples

### `operators/c5c3/internal/controller/reconcile_korc.go:61`

```go
func adminAppCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("openstack/keystone/%s/%s/admin/app-credential", cp.Namespace, cp.Name)
}
```

### `operators/keystone/internal/controller/reconcile_passwordrotation.go:604`

```go
RemoteKey: fmt.Sprintf("bootstrap/%s/%s/admin", keystone.Namespace, keystone.Name),
```

### `operators/keystone/internal/controller/reconcile_fernet.go:460`

```go
// DECISION: boundary 4 (CC-0112, REQ-004) — chose option (a), a keystone.Namespace
// path segment, so two Keystone CRs with the same Name in different namespaces
// resolve to distinct OpenBao leaves. Reviewer: please verify.
RemoteKey: "openstack/keystone/" + keystone.Namespace + "/" + keystone.Name + "/fernet-keys",
```

