# Pattern: Standard doc.go package comment format

**Component**: internal/common/*
**Category**: naming
**Applies-When**: Creating a new package under internal/common/

## Description

Every package under internal/common/ has a doc.go file with a single-line or multi-line package comment following the format: '// Package <name> provides <description>. (<feature-ids>)'. Feature IDs (e.g., CC-0004, CC-0005) are appended in parentheses. No other decorative content is included except in job/doc.go which has a reviewer-requested poem.

## Examples

### `internal/common/deployment/doc.go:1`

```go
// Package deployment provides helper functions for managing Kubernetes
// Deployments and Services in OpenStack operator reconcilers. (CC-0005)
package deployment
```

### `internal/common/secrets/doc.go:1`

```go
// Package secrets provides Kubernetes Secret and ExternalSecret/PushSecret
// management helpers for OpenStack service operators. (CC-0005)
package secrets
```

