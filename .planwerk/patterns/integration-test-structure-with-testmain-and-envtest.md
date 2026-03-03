# Pattern: Integration test structure with TestMain and envtest

**Component**: internal/common/ test files
**Category**: testing
**Applies-When**: Writing integration tests for any package that interacts with the Kubernetes API

## Description

Each package's integration test file follows: //go:build integration tag, package_test package, var k8sClient client.Client, const testNamespace, TestMain that calls testenvtest.SetupEnvTest(), creates a dedicated namespace, runs tests, and calls teardown. Individual tests use unique resource names and t.Cleanup for resource deletion. Unit tests for pure functions use //go:build !integration tag.

## Examples

### `internal/common/config/configmap_integration_test.go:1-46`

```go
//go:build integration

package config_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"github.com/c5c3/forge/internal/common/config"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var k8sClient client.Client
const testNamespace = "test-config"

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create test namespace: %v\n", err)
		teardown()
		os.Exit(1)
	}
	code := m.Run()
	teardown()
	os.Exit(code)
}
```

### `internal/common/deployment/deployment_test.go:1-13`

```go
//go:build !integration

package deployment_test

import (
	"testing"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"github.com/c5c3/forge/internal/common/deployment"
)
```

