# Pattern: Cross-reference access policies against all consumers before finalizing

**Component**: deploy/openbao/policies/, deploy/eso/
**Category**: validation
**Applies-When**: Adding or modifying an ESO HCL policy or ExternalSecret manifest — verify every secret path referenced by ExternalSecrets using the bound role is covered by the corresponding policy

## Description

Each ESO HCL policy (eso-management, eso-control-plane, etc.) must grant read access to every kv-v2/data/* path referenced by ExternalSecrets using the corresponding ClusterSecretStore auth role. For example, if keystone-db ExternalSecret reads from openstack/keystone/db via the management ClusterSecretStore (role eso-management), then eso-management.hcl must include path kv-v2/data/openstack/*. A missing path causes silent 403 errors at runtime. Cross-reference by grepping ExternalSecret manifests for the store name, then mapping each remoteRef.key to its kv-v2/data/ path.

## Examples

### `deploy/openbao/policies/eso-management.hcl:19-21`

```
path "kv-v2/data/openstack/*" {
  capabilities = ["read"]
}
```

### `deploy/eso/externalsecrets/keystone-db.yaml:27-28`

```
      remoteRef:
        key: openstack/keystone/db
```

