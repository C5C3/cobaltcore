# Pattern: Immutability verification test for map-returning pure functions

**Component**: internal/common/*/
**Category**: testing
**Applies-When**: Writing a pure function that accepts map inputs and returns a new map (i.e., must not mutate inputs)

## Description

Every pure function that takes map parameters and returns a new map has a dedicated _doesNotMutateInput(s) test. The test calls the function, then mutates the returned map, and asserts the original input maps are unchanged. This ensures the function properly deep-copies map values rather than sharing references. Applied consistently across 4 functions in 3 packages: MergeDefaults, InjectSecrets, RenderPluginConfig, MergePolicies.

## Examples

### `internal/common/config/config_test.go:185`

```go
func TestMergeDefaults_doesNotMutateInputs(t *testing.T) {
	g := NewGomegaWithT(t)

	userConfig := map[string]map[string]string{
		"DEFAULT": {"debug": "true"},
	}
	defaults := map[string]map[string]string{
		"DEFAULT": {"debug": "false", "log_file": "/var/log/app.log"},
	}

	result := MergeDefaults(userConfig, defaults)

	// Mutating the result should not affect the inputs.
	result["DEFAULT"]["debug"] = "mutated"
	result["DEFAULT"]["new_key"] = "new_value"

	g.Expect(userConfig["DEFAULT"]["debug"]).To(Equal("true"))
	g.Expect(defaults["DEFAULT"]["debug"]).To(Equal("false"))
	g.Expect(userConfig["DEFAULT"]).NotTo(HaveKey("new_key"))
	g.Expect(defaults["DEFAULT"]).NotTo(HaveKey("new_key"))
}
```

### `internal/common/policy/policy_test.go:246`

```go
func TestMergePolicies_doesNotMutateInputs(t *testing.T) {
	g := NewGomegaWithT(t)

	base := types.PolicySpec{
		Rules: map[string]string{
			"compute:create": "role:admin",
		},
		ConfigMapRef: &corev1.LocalObjectReference{Name: "base-policy"},
	}
	override := types.PolicySpec{
		Rules: map[string]string{
			"compute:create": "role:member",
		},
	}

	result := MergePolicies(base, override)

	// Mutating the result should not affect the inputs.
	result.Rules["compute:create"] = "mutated"
	result.Rules["new_rule"] = "new_value"

	g.Expect(base.Rules["compute:create"]).To(Equal("role:admin"))
	g.Expect(override.Rules["compute:create"]).To(Equal("role:member"))
	g.Expect(base.Rules).NotTo(HaveKey("new_rule"))
	g.Expect(override.Rules).NotTo(HaveKey("new_rule"))
}
```

