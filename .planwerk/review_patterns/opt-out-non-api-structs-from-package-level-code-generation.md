# Review Pattern: Opt out non-API structs from package-level code generation

**Review-Area**: architecture
**Detection-Hint**: When reviewing generated deepcopy files (`zz_generated.deepcopy.go`), scan for structs that are not Kubernetes API types (no `metav1.ObjectMeta`, no `runtime.Object` interface). If a non-API utility struct appears in the generated file, it needs a `+kubebuilder:object:generate=false` marker.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Package-level `+kubebuilder:object:generate=true` markers cause controller-gen to emit DeepCopy methods for every struct in the package. Utility structs (e.g., webhook handlers, helpers) that are not Kubernetes runtime objects should explicitly opt out.

## Why it matters

Unnecessary generated code adds noise to diffs, increases binary size marginally, and signals to future developers that the struct is a Kubernetes API type when it is not, causing confusion about the package's design.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: The `+kubebuilder:object:generate=true` package-level marker in `groupversion_info.go` causes controller-gen to emit `DeepCopyInto`/`DeepCopy` for every struct in the package, including `KeystoneWebhook{}`. `KeystoneWebhook` is a zero-field utility struct, not a Kubernetes runtime object, so generating deepcopy for it is unnecessary noise.
- **What was missed**: Package-level `+kubebuilder:object:generate=true` markers cause controller-gen to emit DeepCopy methods for every struct in the package. Utility structs (e.g., webhook handlers, helpers) that are not Kubernetes runtime objects should explicitly opt out.
- **Fix**: Added `// +kubebuilder:object:generate=false` above the `KeystoneWebhook` struct and re-ran `make generate` to remove the unnecessary DeepCopy methods.
