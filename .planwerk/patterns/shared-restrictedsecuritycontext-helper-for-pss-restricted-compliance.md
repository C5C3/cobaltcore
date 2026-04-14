# Pattern: Shared restrictedSecurityContext helper for PSS Restricted compliance

**Component**: operators/keystone/internal/controller
**Category**: configuration
**Applies-When**: Adding a new Job or CronJob builder function that creates pod specs for the Keystone operator (or future operators following the same pattern); Adding a new shell script that needs to run inside a CronJob or Job pod managed by an operator reconciler, where the script content should be independently lintable, testable, and versioned

## Description

All Job and CronJob container specs must include SecurityContext: restrictedSecurityContext() on every container (including init containers). The restrictedSecurityContext() helper in reconcile_deployment.go returns a *corev1.SecurityContext with AllowPrivilegeEscalation=ptr.To(false), RunAsNonRoot=ptr.To(true), ReadOnlyRootFilesystem=ptr.To(true), and SeccompProfile={Type: RuntimeDefault}. This ensures Pod Security Standards Restricted profile compliance. The helper is used by 4 builder functions across 6 containers.

Shell scripts are stored as standalone .sh files under a scripts/ subdirectory co-located with the reconciler that uses them. The script is embedded into the Go binary via '//go:embed scripts/<name>.sh' as a string variable. At reconciliation time, the script content is passed to config.CreateImmutableConfigMap() which creates an immutable ConfigMap with a content-hash suffix (e.g., '{name}-fernet-rotate-script-abc123'). The ConfigMap is mounted as a volume with DefaultMode 0555 (executable, read-only) at /scripts/ in the pod spec. The container Command references the mounted path (e.g., ['/scripts/fernet_rotate.sh']) instead of inline 'sh -c <script>'. A companion unit test (TestXxxScript_EmbeddedContent) verifies the embedded variable is non-empty, starts with the correct shebang, and contains key commands — guarding against broken go:embed directives.

## Examples

### `operators/keystone/internal/controller/reconcile_deployment.go:290`

```go
func restrictedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		RunAsNonRoot:             ptr.To(true),
		ReadOnlyRootFilesystem:   ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}
```

### `operators/keystone/internal/controller/reconcile_fernet.go:253`

```go
InitContainers: []corev1.Container{{
	Name:            "copy-keys",
	Image:           image,
	Command:         []string{"sh", "-c", "cp /fernet-keys-src/* /etc/keystone/fernet-keys/"},
	SecurityContext: restrictedSecurityContext(),
	VolumeMounts: []corev1.VolumeMount{...},
}}
```
### `operators/keystone/internal/controller/reconcile_fernet.go:38-39`

```go
//go:embed scripts/fernet_rotate.sh
var fernetRotateScript string
```

### `operators/keystone/internal/controller/reconcile_fernet.go:77-83`

```go
	// 3. Create the immutable ConfigMap containing the rotation script (CC-0073).
	scriptConfigMapName, err := config.CreateImmutableConfigMap(ctx, r.Client, r.Scheme, keystone,
		fmt.Sprintf("%s-fernet-rotate-script", keystone.Name), keystone.Namespace,
		map[string]string{"fernet_rotate.sh": fernetRotateScript})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating fernet rotate script ConfigMap: %w", err)
	}
```

### `operators/keystone/internal/controller/reconcile_fernet.go:309-318`

```go
								{
									Name: "scripts",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{Name: scriptConfigMapName},
											DefaultMode:          ptr.To(int32(0555)),
										},
									},
								},
```

### `operators/keystone/internal/controller/reconcile_credential.go:38-39`

```go
//go:embed scripts/credential_rotate.sh
var credentialRotateScript string
```


