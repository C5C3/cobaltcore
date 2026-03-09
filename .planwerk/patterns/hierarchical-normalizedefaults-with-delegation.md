# Pattern: Hierarchical normalize.*Defaults with delegation

**Component**: internal/common/normalize
**Category**: service-structure
**Applies-When**: Adding a new resource-type normalization function to the normalize/ package (e.g., StatefulSetSpecDefaults)

## Description

Each resource-level normalize function fills in its own API-server defaults, then delegates to the next lower level. CronJobSpecDefaults calls JobSpecDefaults and PodSpecDefaults. DeploymentSpecDefaults calls PodSpecDefaults. This avoids duplication and ensures pod-level defaults are always applied regardless of which entry point is used. New resource normalizers must follow this delegation chain.

## Examples

### `internal/common/normalize/cronjob.go:15`

```go
func CronJobSpecDefaults(spec *batchv1.CronJobSpec) {
	if spec.ConcurrencyPolicy == "" {
		spec.ConcurrencyPolicy = batchv1.AllowConcurrent
	}
	// ... cronjob-level defaults ...
	JobSpecDefaults(&spec.JobTemplate.Spec)
	PodSpecDefaults(&spec.JobTemplate.Spec.Template.Spec)
}
```

### `internal/common/normalize/deployment.go:16`

```go
func DeploymentSpecDefaults(spec *appsv1.DeploymentSpec) {
	if spec.RevisionHistoryLimit == nil {
		v := int32(10)
		spec.RevisionHistoryLimit = &v
	}
	// ... deployment-level defaults ...
	PodSpecDefaults(&spec.Template.Spec)
}
```

