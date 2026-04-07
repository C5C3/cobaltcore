# Pattern: Helm RBAC conditional guard with sync-required comment

**Component**: operators/keystone/helm/keystone-operator/templates/
**Category**: configuration
**Applies-When**: Adding a new Helm chart that supports both cluster-wide and namespace-scoped RBAC deployment modes, or adding new RBAC rules to an existing dual-mode chart

## Description

When a Helm chart supports both ClusterRole and namespace-scoped Role, the rules are duplicated across two template files (clusterrole.yaml and role.yaml) with mutually exclusive conditional guards: `{{- if not .Values.rbac.namespaceScoped }}` for ClusterRole and `{{- if .Values.rbac.namespaceScoped }}` for Role. The Role template includes a sync comment '# Rules MUST be kept in sync with clusterrole.yaml' to flag the coupling. The corresponding bindings (ClusterRoleBinding/RoleBinding) use matching guards. A values.yaml toggle `rbac.namespaceScoped: false` controls the mode. Deployment template conditionally injects `--namespace={{ .Release.Namespace }}` arg when namespaceScoped is true. Tests verify mutual exclusion (document count 0 when mode is off) and rule count parity.

## Examples

### `operators/keystone/helm/keystone-operator/templates/role.yaml:1-3`

```
{{- if .Values.rbac.namespaceScoped }}
# CC-0043: Namespace-scoped Role (mirrors ClusterRole rules).
# Rules MUST be kept in sync with clusterrole.yaml.
```

### `operators/keystone/helm/keystone-operator/templates/clusterrole.yaml:1`

```
{{- if not .Values.rbac.namespaceScoped }}
```

