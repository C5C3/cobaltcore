# Pattern: Typed-generic webhook struct with object:generate=false opt-out

**Component**: operators/*/api/v1alpha1/
**Category**: validation
**Applies-When**: Adding defaulting and validating webhooks for a new CobaltCore operator CRD (e.g., GlanceWebhook, NovaWebhook)

## Description

Webhooks use a dedicated zero-field struct (e.g., KeystoneWebhook) implementing admission.Defaulter[*T] and admission.Validator[*T] typed generics from controller-runtime v0.23+. The struct has a +kubebuilder:object:generate=false marker to prevent unnecessary DeepCopy generation. SetupWebhookWithManager registers both webhooks via builder.WebhookManagedBy[*T]. Compile-time interface checks are declared as package-level var _ assertions. Default() returns error (not void), and Validate{Create,Update} accept context + typed object parameters. ValidateDelete returns nil unconditionally. The internal validate() method accumulates errors in field.ErrorList and returns apierrors.NewInvalid with the correct GroupKind. This replaces the older kubebuilder pattern where methods are defined directly on the CRD type (as shown in the architecture doc).

## Examples

### `operators/keystone/api/v1alpha1/keystone_webhook.go:20-36`

```go
// KeystoneWebhook implements defaulting and validation webhooks for the Keystone CRD (CC-0011).
// +kubebuilder:object:generate=false
type KeystoneWebhook struct{}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*Keystone] = &KeystoneWebhook{}
	_ admission.Validator[*Keystone] = &KeystoneWebhook{}
)

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *KeystoneWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*Keystone](mgr, &Keystone{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}
```

### `operators/keystone/api/v1alpha1/keystone_webhook.go:42-58`

```go
func (w *KeystoneWebhook) Default(_ context.Context, obj *Keystone) error {
	if obj.Spec.Replicas == 0 {
		obj.Spec.Replicas = 3
	}
	if obj.Spec.Fernet.MaxActiveKeys == 0 {
		obj.Spec.Fernet.MaxActiveKeys = 3
	}
	if obj.Spec.Cache.Backend == "" {
		obj.Spec.Cache.Backend = "dogpile.cache.pymemcache"
	}
	if obj.Spec.Bootstrap.AdminUser == "" {
		obj.Spec.Bootstrap.AdminUser = "admin"
	}
	if obj.Spec.Bootstrap.Region == "" {
		obj.Spec.Bootstrap.Region = "RegionOne"
	}
	return nil
}
```

