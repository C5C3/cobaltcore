# Pattern: FluxCD HelmRelease common install/upgrade settings

**Component**: deploy/flux-system/releases/
**Category**: configuration
**Applies-When**: Adding a new HelmRelease CR for an infrastructure operator (e.g., OpenBao for CC-0009, Keystone Operator for CC-0017)

## Description

All HelmRelease CRs in deploy/flux-system/releases/ share identical install and upgrade settings: interval 30m, install.crds CreateReplace, install.createNamespace true, upgrade.crds CreateReplace, upgrade.remediation.retries 3. All operators except cert-manager (the base layer) must include dependsOn [{name: cert-manager, namespace: cert-manager}]. The sourceRef always points to a HelmRepository in the flux-system namespace. Each operator is deployed to its own namespace (<operator>-system or <operator-name>).

## Examples

### `deploy/flux-system/releases/mariadb-operator.yaml:4`

```
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: mariadb-operator
  namespace: mariadb-system
spec:
  interval: 30m
  dependsOn:
    - name: cert-manager
      namespace: cert-manager
  chart:
    spec:
      chart: mariadb-operator
      version: ">=0.30.0 <1.0.0"
      sourceRef:
        kind: HelmRepository
        name: mariadb-operator
        namespace: flux-system
  values:
    metrics:
      enabled: true
    webhook:
      enabled: true
  install:
    crds: CreateReplace
    createNamespace: true
  upgrade:
    crds: CreateReplace
    remediation:
      retries: 3
```

### `deploy/flux-system/releases/memcached-operator.yaml:4`

```
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: memcached-operator
  namespace: memcached-system
spec:
  interval: 30m
  dependsOn:
    - name: cert-manager
      namespace: cert-manager
  chart:
    spec:
      chart: memcached-operator
      version: ">=0.1.0 <1.0.0"
      sourceRef:
        kind: HelmRepository
        name: c5c3-charts
        namespace: flux-system
  values:
    metrics:
      enabled: true
    webhook:
      enabled: true
  install:
    crds: CreateReplace
    createNamespace: true
  upgrade:
    crds: CreateReplace
    remediation:
      retries: 3
```

