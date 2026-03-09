# Pattern: Normalize function test pair: defaults + preservesExplicitValues

**Component**: internal/common/normalize/*_test.go
**Category**: testing
**Applies-When**: Adding tests for a new normalize.*Defaults function

## Description

Every normalize function has exactly two tests: TestXxxDefaults verifies that zero/nil fields are filled with correct API-server defaults (including delegation to lower-level normalizers), and TestXxxDefaults_preservesExplicitValues verifies that non-zero fields are left unchanged. Both use NewGomegaWithT(t) and gomega matchers.

## Examples

### `internal/common/normalize/deployment_test.go:19`

```go
func TestDeploymentSpecDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	spec := &appsv1.DeploymentSpec{...}
	DeploymentSpecDefaults(spec)
	g.Expect(spec.RevisionHistoryLimit).NotTo(BeNil())
	g.Expect(*spec.RevisionHistoryLimit).To(Equal(int32(10)))
	// ...
}
```

### `internal/common/normalize/deployment_test.go:52`

```go
func TestDeploymentSpecDefaults_preservesExplicitValues(t *testing.T) {
	g := NewGomegaWithT(t)
	revisionLimit := int32(5)
	spec := &appsv1.DeploymentSpec{RevisionHistoryLimit: &revisionLimit, ...}
	DeploymentSpecDefaults(spec)
	g.Expect(*spec.RevisionHistoryLimit).To(Equal(int32(5)))
	// ...
}
```

