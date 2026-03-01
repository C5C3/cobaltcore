---
title: Shared Types
quadrant: backend
---

# Shared Types

**Package:** `internal/common/types`
**Feature:** CC-0004

Shared Go struct types for embedding in operator CRD specs. These types carry no kubebuilder markers (Decision #4) — markers belong on CRD embedding sites. The only external dependency is `k8s.io/api/core/v1` for `LocalObjectReference`.

## ImageSpec

Container image reference.

| Field        | Type     | JSON Tag     | Required | Description                          |
|--------------|----------|--------------|----------|--------------------------------------|
| `Repository` | `string` | `repository` | Yes      | Container image repository URL       |
| `Tag`        | `string` | `tag`        | Yes      | Container image tag or digest        |

## SecretRefSpec

References a Kubernetes Secret by name and key. Embedded as a value type in `DatabaseSpec`, `MessagingSpec`, and other specs that need secret references.

| Field  | Type     | JSON Tag | Required | Description                     |
|--------|----------|----------|----------|---------------------------------|
| `Name` | `string` | `name`   | Yes      | Name of the Kubernetes Secret   |
| `Key`  | `string` | `key`    | Yes      | Key within the Secret's data    |

## DatabaseSpec

Database connection parameters. Supports two mutually exclusive modes:

- **Managed mode:** Set `ClusterRef` to reference a managed MariaDB cluster.
- **Brownfield mode:** Set `Host` and `Port` to connect to an external database.

| Field        | Type                              | JSON Tag              | Required | Description                                          |
|--------------|-----------------------------------|-----------------------|----------|------------------------------------------------------|
| `ClusterRef` | `*corev1.LocalObjectReference`    | `clusterRef,omitempty`| No       | Reference to a managed MariaDB cluster               |
| `Host`       | `string`                          | `host,omitempty`      | No       | Hostname for brownfield database                     |
| `Port`       | `int32`                           | `port,omitempty`      | No       | Port for brownfield database                         |
| `Database`   | `string`                          | `database`            | Yes      | Database name                                        |
| `SecretRef`  | `SecretRefSpec`                   | `secretRef`           | Yes      | Secret containing database credentials               |

## MessagingSpec

RabbitMQ messaging parameters.

| Field       | Type            | JSON Tag          | Required | Description                           |
|-------------|-----------------|-------------------|----------|---------------------------------------|
| `Host`      | `string`        | `host`            | Yes      | RabbitMQ hostname                     |
| `Port`      | `int32`         | `port,omitempty`  | No       | RabbitMQ port (defaults to 5672)      |
| `SecretRef` | `SecretRefSpec` | `secretRef`       | Yes      | Secret containing messaging credentials|
| `Vhost`     | `string`        | `vhost,omitempty` | No       | RabbitMQ virtual host                 |

## CacheSpec

Memcached cache parameters. Supports two mutually exclusive modes:

- **Managed mode:** Set `ClusterRef` to reference a managed Memcached cluster.
- **Brownfield mode:** Set `Servers` to provide explicit server addresses.

| Field        | Type                           | JSON Tag               | Required | Description                               |
|--------------|--------------------------------|------------------------|----------|-------------------------------------------|
| `ClusterRef` | `*corev1.LocalObjectReference` | `clusterRef,omitempty` | No       | Reference to a managed Memcached cluster  |
| `Servers`    | `[]string`                     | `servers,omitempty`    | No       | Explicit server addresses (host:port)     |

## PolicySpec

Policy override sources for oslo.policy.

| Field          | Type                           | JSON Tag                  | Required | Description                              |
|----------------|--------------------------------|---------------------------|----------|------------------------------------------|
| `ConfigMapRef` | `*corev1.LocalObjectReference` | `configMapRef,omitempty`  | No       | ConfigMap containing external policy rules|
| `Inline`       | `map[string]string`            | `inline,omitempty`        | No       | Inline policy rule overrides             |

## PluginSpec

Plugin configuration section for merging into the main INI config.

| Field     | Type                | JSON Tag          | Required | Description                                  |
|-----------|---------------------|-------------------|----------|----------------------------------------------|
| `Name`    | `string`            | `name`            | Yes      | Plugin name                                  |
| `Section` | `string`            | `section`         | Yes      | INI section name for this plugin's config    |
| `Config`  | `map[string]string` | `config,omitempty`| No       | Key-value pairs for the plugin section       |

## PipelinePosition

Specifies where middleware is inserted in the PasteDeploy pipeline. At most one of `Before` or `After` should be set. If both are empty, the middleware is appended to the end.

| Field    | Type     | JSON Tag          | Required | Description                                    |
|----------|----------|-------------------|----------|------------------------------------------------|
| `Before` | `string` | `before,omitempty`| No       | Insert before this pipeline token              |
| `After`  | `string` | `after,omitempty` | No       | Insert after this pipeline token               |

## MiddlewareSpec

PasteDeploy middleware/filter to insert into the pipeline.

| Field           | Type                | JSON Tag          | Required | Description                                |
|-----------------|---------------------|-------------------|----------|--------------------------------------------|
| `Name`          | `string`            | `name`            | Yes      | Middleware name (used in pipeline and filter section) |
| `FilterFactory` | `string`            | `filterFactory`   | Yes      | PasteDeploy filter factory entry point     |
| `Config`        | `map[string]string` | `config,omitempty`| No       | Additional filter configuration keys       |
| `Position`      | `PipelinePosition`  | `position`        | Yes      | Where to insert in the pipeline            |

## FilterSpec

PasteDeploy filter entry for the base pipeline.

| Field     | Type                | JSON Tag          | Required | Description                                |
|-----------|---------------------|-------------------|----------|--------------------------------------------|
| `Name`    | `string`            | `name`            | Yes      | Filter name (used in `[filter:name]` section)|
| `Factory` | `string`            | `factory`         | Yes      | PasteDeploy filter factory entry point     |
| `Config`  | `map[string]string` | `config,omitempty`| No       | Additional filter configuration keys       |

## PipelineSpec

Complete PasteDeploy pipeline configuration. Aggregates a base pipeline string with filters and middleware.

| Field          | Type               | JSON Tag               | Required | Description                                 |
|----------------|--------------------|------------------------|----------|---------------------------------------------|
| `Name`         | `string`           | `name`                 | Yes      | Pipeline name (used in `[pipeline:name]`)   |
| `BasePipeline` | `string`           | `basePipeline`         | Yes      | Space-separated base pipeline token list    |
| `Filters`      | `[]FilterSpec`     | `filters,omitempty`    | No       | Base pipeline filter definitions            |
| `Middleware`    | `[]MiddlewareSpec` | `middleware,omitempty` | No       | Middleware to insert into the pipeline      |

## Dependencies

- **CC-0004** — implements all 11 types
- **CC-0011** — will add kubebuilder markers when embedding in CRD specs
