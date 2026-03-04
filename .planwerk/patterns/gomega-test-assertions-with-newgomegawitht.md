# Pattern: Gomega test assertions with NewGomegaWithT

**Component**: internal/common/testutil/*_test.go
**Category**: testing
**Applies-When**: Writing unit or integration tests in any testutil package or consumer of testutil

## Description

All test functions use gomega via dot import (`. "github.com/onsi/gomega"`) and create a local `g := NewGomegaWithT(t)` at the start of each test function. Assertions use `g.Expect(...)` with gomega matchers (To, NotTo, Succeed, HaveOccurred, BeTrue, Equal, HaveLen, HaveKeyWithValue, ContainSubstring, BeNil, BeEmpty, BeEquivalentTo, Panic). This replaces raw if/else + t.Errorf/t.Fatalf patterns.

## Examples

### `internal/common/testutil/builders/secret_builder_test.go:21`

```go
func TestNewSecretBuilder_basic(t *testing.T) {
	g := NewGomegaWithT(t)
	secret := NewSecretBuilder("my-secret", "default").Build()

	g.Expect(secret.Name).To(Equal("my-secret"))
	g.Expect(secret.Namespace).To(Equal("default"))
}
```

### `internal/common/testutil/simulators/simulators_test.go:44`

```go
func TestSimulateMariaDBReady(t *testing.T) {
	g := NewGomegaWithT(t)

	mariadb := newUnstructured("k8s.mariadb.com", "v1alpha1", "MariaDB", "test-mariadb", "default")
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mariadb).
		WithStatusSubresource(mariadb).
		Build()

	err := SimulateMariaDBReady(context.Background(), c, client.ObjectKeyFromObject(mariadb), 3)
	g.Expect(err).NotTo(HaveOccurred())
```

