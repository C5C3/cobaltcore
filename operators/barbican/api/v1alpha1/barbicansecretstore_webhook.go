// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/validation"
	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// DefaultKVMountpoint is the KV v2 mount barbican's secret material lives under.
// It is the single mount the self-init contract provisions for a managed store,
// which is why a managed store is frozen to it.
const DefaultKVMountpoint = "barbican"

// Name-length bound enforced on metadata.name, driven by the AppRole credentials
// Secret the operator derives from it.
const (
	// ApproleSecretNameSuffix is appended to metadata.name to name the AppRole
	// credentials Secret of a managed store. It is exported because the
	// controller package builds that object and must derive the same name the
	// bound below is computed from; the api package cannot import the controller,
	// so this is the shared definition rather than a second copy.
	ApproleSecretNameSuffix = "-approle"
	// MaxBarbicanSecretStoreNameLength is what the 63-character DNS label budget
	// of that Secret's name leaves for metadata.name.
	MaxBarbicanSecretStoreNameLength = 63 - len(ApproleSecretNameSuffix)
)

// BarbicanSecretStoreWebhook implements defaulting and validation webhooks for
// the BarbicanSecretStore CRD. Client is injected at startup for the two sibling
// checks (a namespace-scoped List of the stores attached to the same Barbican).
// Production wiring injects mgr.GetAPIReader() — a direct, uncached reader — so
// admission never misses a just-created sibling from a stale informer cache and
// no lazy informer start happens inside the webhook timeout.
// +kubebuilder:object:generate=false
type BarbicanSecretStoreWebhook struct {
	commonwebhook.NoopDeleteValidator[*BarbicanSecretStore]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*BarbicanSecretStore] = &BarbicanSecretStoreWebhook{}
	_ admission.Validator[*BarbicanSecretStore] = &BarbicanSecretStoreWebhook{}
)

// +kubebuilder:webhook:path=/mutate-barbican-openstack-c5c3-io-v1alpha1-barbicansecretstore,mutating=true,failurePolicy=fail,sideEffects=None,groups=barbican.openstack.c5c3.io,resources=barbicansecretstores,verbs=create;update,versions=v1alpha1,name=mbarbicansecretstore.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-barbican-openstack-c5c3-io-v1alpha1-barbicansecretstore,mutating=false,failurePolicy=fail,sideEffects=None,groups=barbican.openstack.c5c3.io,resources=barbicansecretstores,verbs=create;update,versions=v1alpha1,name=vbarbicansecretstore.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with
// the manager.
func (w *BarbicanSecretStoreWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*BarbicanSecretStore](mgr, &BarbicanSecretStore{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*BarbicanSecretStore]. It materializes
// the documented defaults when the fields carry zero values, so the production
// admission path has a single source of truth; the +kubebuilder:default markers
// remain as defense-in-depth for callers that bypass the webhook (e.g. envtest
// without the defaulter wired up).
func (w *BarbicanSecretStoreWebhook) Default(_ context.Context, obj *BarbicanSecretStore) error {
	// Mirror the +kubebuilder:default=barbican marker on
	// OpenBaoStoreSpec.KVMountpoint for webhook-bypassing callers: fill the
	// provisioned mount when the openBao block exists and the field is empty.
	if obj.Spec.OpenBao != nil && obj.Spec.OpenBao.KVMountpoint == "" {
		obj.Spec.OpenBao.KVMountpoint = DefaultKVMountpoint
	}
	return nil
}

// ValidateCreate implements admission.Validator[*BarbicanSecretStore].
//
// The metadata.name bound is enforced here rather than in validate(), which
// update shares: the name is immutable, so on update the rule could only ever
// fire against an object a pre-upgrade operator already admitted — and the
// validating webhook also sees the finalizer-removal update reconcileDelete
// issues, so rejecting it would wedge that CR in Terminating with no field left
// to edit to repair it.
func (w *BarbicanSecretStoreWebhook) ValidateCreate(ctx context.Context, obj *BarbicanSecretStore) (admission.Warnings, error) {
	return nil, w.validate(ctx, obj, validateStoreNameLength(obj.Name))
}

// ValidateUpdate implements admission.Validator[*BarbicanSecretStore].
// Field immutability (barbicanRef, type) is enforced by the CRD CEL transition
// rules, so the webhook re-runs only the value-level rules against the new
// object.
func (w *BarbicanSecretStoreWebhook) ValidateUpdate(ctx context.Context, _, newObj *BarbicanSecretStore) (admission.Warnings, error) {
	return nil, w.validate(ctx, newObj, nil)
}

// barbicanReservedStoreNames enumerates the barbican.conf section names that a
// store name must not collide with. A BarbicanSecretStore's metadata.name becomes
// the suffix of its [secretstore:<name>] section, and barbican resolves that
// suffix against the flat section namespace of one config file, so a name equal
// to any of these would clobber (or be clobbered by) a section the operator or an
// oslo library already owns. Kubernetes object names are lowercase (RFC 1123), so
// the set is written in lowercase; the upstream "DEFAULT" section is matched
// case-insensitively via its lowercase form (see validateStoreName).
var barbicanReservedStoreNames = map[string]struct{}{
	"default":            {},
	"database":           {},
	"keystone_authtoken": {},
	"secretstore":        {},
	"vault_plugin":       {},
	"queue":              {},
	"oslo_policy":        {},
	"oslo_middleware":    {},
	"healthcheck":        {},
}

// openBaoExtraOptionsDenylist enumerates the option names spec.extraOptions must
// never carry, mapped to the spec field that owns each. The first group is the
// [vault_plugin] options the operator derives from the typed store spec (a
// duplicate would silently shadow — or be shadowed by — the derived value
// depending on render order); the second is the three structural
// [secretstore:<name>] options that make the section a secret store at all.
//
//nolint:gosec // G101 false positive: vault plugin option names and spec field paths, not credentials.
var openBaoExtraOptionsDenylist = map[string]string{
	"vault_url":         "the operator (derived from spec.openBao.instanceRef, or spec.openBao.server.url)",
	"use_ssl":           "the operator (derived from the resolved server URL's scheme)",
	"ssl_ca_crt_file":   "the operator (spec.openBao.server.caBundleSecretRef, or the CA of the referenced OpenBaoCluster)",
	"approle_role_id":   "the operator (the AppRole credentials it mints for a managed store or reads from spec.openBao.server.credentialsSecretRef)",
	"approle_secret_id": "the operator (env-injected from the credentials Secret, never written to the config file)",
	"root_token_id":     "the operator (the plugin authenticates through a mount-scoped AppRole, so a root token is never rendered)",
	"kv_mountpoint":     "spec.openBao.kvMountpoint",
	"namespace":         "spec.openBao.namespace",

	"secret_store_plugin": "the operator (secret-store section wiring)",
	"crypto_plugin":       "the operator (secret-store section wiring)",
	"global_default":      "spec.isDefault",
}

// DeniedOpenBaoExtraOption reports whether key names a [vault_plugin] option the
// operator owns and spec.extraOptions must never carry. It is exported for the
// render-time backstop: the renderer skips an operator key by checking whether it
// already wrote that key this pass, which is blind to the denied options it does
// NOT write (root_token_id, approle_secret_id, crypto_plugin, and ssl_ca_crt_file
// on a store with no CA bundle). Those are exactly the ones worth stopping — a
// root token makes the plugin bypass the mount-scoped AppRole entirely — so the
// renderer consults this predicate instead of inferring the set from its own
// output.
func DeniedOpenBaoExtraOption(key string) bool {
	_, denied := openBaoExtraOptionsDenylist[key]
	return denied
}

// openBaoExtraOptionKeyPattern is the allowlist every spec.extraOptions key must
// match. Vault plugin option names are snake_case, so letters, digits,
// and underscores are the full legitimate charset. Anything else (an embedded
// newline, a denylist-evading trailing space) could inject an INI line or slip
// past the exact-match denylist, so it is rejected before either exact-match
// check runs.
var openBaoExtraOptionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// validate runs all validation rules against the BarbicanSecretStore spec,
// accumulating every violation into a single field.ErrorList so users see all
// problems at once. ctx is required for the sibling List behind the single-default
// and OpenBao-uniqueness rules. extra carries the errors accumulated by the caller
// (on create the metadata.name bound) so they aggregate into the same Invalid
// error.
func (w *BarbicanSecretStoreWebhook) validate(ctx context.Context, b *BarbicanSecretStore, extra field.ErrorList) error {
	allErrs := extra
	specPath := field.NewPath("spec")

	allErrs = append(allErrs, validateStoreUnion(specPath, b)...)
	allErrs = append(allErrs, validateStoreName(b)...)
	allErrs = append(allErrs, validateStoreExtraOptions(specPath.Child("extraOptions"), b)...)
	allErrs = append(allErrs, w.validateSiblingDefault(ctx, specPath, b)...)
	allErrs = append(allErrs, w.validateSiblingOpenBaoUniqueness(ctx, specPath, b)...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "BarbicanSecretStore"},
			b.Name,
			allErrs,
		)
	}
	return nil
}

// validateStoreUnion is the defense-in-depth twin of the three CEL rules that
// shape the store union: exactly one store block matching spec.type, exactly one
// of the two OpenBao modes, and the mount layout a managed store is frozen to.
// Each violation names the path that has to change.
func validateStoreUnion(specPath *field.Path, b *BarbicanSecretStore) field.ErrorList {
	var errs field.ErrorList
	openBaoPath := specPath.Child("openBao")

	if (b.Spec.Type == BarbicanSecretStoreTypeOpenBao) != (b.Spec.OpenBao != nil) {
		errs = append(errs, field.Invalid(
			openBaoPath,
			b.Spec.Type,
			"exactly one store block matching spec.type must be set (type OpenBao requires spec.openBao)",
		))
	}
	if b.Spec.OpenBao == nil {
		return errs
	}

	o := b.Spec.OpenBao
	if (o.InstanceRef != nil) == (o.Server != nil) {
		errs = append(errs, field.Invalid(
			openBaoPath,
			*o,
			"exactly one of instanceRef or server must be set",
		))
	}
	// A managed store is frozen to the mount layout the self-init contract
	// provisions: the barbican/ mount at the root namespace. Anything else renders
	// a configuration pointing at a path nothing provisions and no AppRole policy
	// covers.
	// Defense-in-depth twin of the ^https:// pattern on OpenBaoServerSpec.URL. A
	// plaintext server URL puts the AppRole credentials and every secret barbican
	// stores on the wire in the clear, and makes a supplied caBundleSecretRef a
	// no-op, so a CR that looks TLS-configured would not be.
	if o.Server != nil && !strings.HasPrefix(o.Server.URL, "https://") {
		errs = append(errs, field.Invalid(
			openBaoPath.Child("server", "url"),
			o.Server.URL,
			"url must use the https:// scheme: the AppRole credentials and every stored secret travel it",
		))
	}
	if o.InstanceRef != nil {
		if o.KVMountpoint != DefaultKVMountpoint {
			errs = append(errs, field.Invalid(
				openBaoPath.Child("kvMountpoint"),
				o.KVMountpoint,
				fmt.Sprintf("a managed store (instanceRef) must keep kvMountpoint %q: the self-init contract provisions only that mount",
					DefaultKVMountpoint),
			))
		}
		if o.Namespace != "" {
			errs = append(errs, field.Invalid(
				openBaoPath.Child("namespace"),
				o.Namespace,
				"a managed store (instanceRef) must leave namespace unset: the self-init contract provisions at the root namespace",
			))
		}
	}
	return errs
}

// validateStoreName rejects a metadata.name that collides with a reserved
// barbican.conf section. The comparison is done on the lowercased name
// (Kubernetes names are already lowercase), which also makes the "DEFAULT" match
// case-insensitive.
func validateStoreName(b *BarbicanSecretStore) field.ErrorList {
	if _, reserved := barbicanReservedStoreNames[strings.ToLower(b.Name)]; !reserved {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"),
		b.Name,
		fmt.Sprintf("name %q collides with the reserved or operator-managed barbican.conf section of the same name", b.Name),
	)}
}

// validateStoreNameLength bounds metadata.name by the child object with the
// tightest name budget, the "{name}-approle" credentials Secret.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateStoreNameLength(name string) field.ErrorList {
	if len(name) <= MaxBarbicanSecretStoreNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"), name,
		fmt.Sprintf("name must be at most %d characters: the operator appends %q to name the AppRole credentials Secret",
			MaxBarbicanSecretStoreNameLength, ApproleSecretNameSuffix),
	)}
}

// validateStoreExtraOptions rejects spec.extraOptions keys the projection owns:
// empty keys, keys whose shape could inject INI lines or evade the denylist, the
// options the operator renders from the typed spec, and values carrying control
// characters.
func validateStoreExtraOptions(optsPath *field.Path, b *BarbicanSecretStore) field.ErrorList {
	if len(b.Spec.ExtraOptions) == 0 {
		return nil
	}
	options := b.Spec.ExtraOptions

	// Sort keys so the aggregated error list is deterministic.
	keys := make([]string, 0, len(options))
	for k := range options {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var errs field.ErrorList
	for _, k := range keys {
		if k == "" {
			errs = append(errs, field.Invalid(optsPath, k, "option name must not be empty"))
			continue
		}
		// Enforce the key allowlist before the denylist, which matches exact strings
		// and so is blind to a newline in the key (an INI-injection vector through
		// the renderer's verbatim `key = value` write) or a denylist-evading variant
		// such as a trailing space.
		if !openBaoExtraOptionKeyPattern.MatchString(k) {
			errs = append(errs, field.Invalid(
				optsPath.Key(k), k,
				"option name must match ^[A-Za-z0-9_]+$ (letters, digits, and underscore)",
			))
			continue
		}
		if owner, denied := openBaoExtraOptionsDenylist[k]; denied {
			errs = append(errs, field.Invalid(
				optsPath.Key(k),
				options[k],
				fmt.Sprintf("option %q is owned by %s and must not be set via extraOptions", k, owner),
			))
			continue
		}
		// INI-injection guard: a value with an embedded newline injects arbitrary
		// plugin-section lines regardless of how innocuous the key is.
		if validation.HasControlChars(options[k]) {
			errs = append(errs, field.Invalid(
				optsPath.Key(k),
				options[k],
				"value must not contain newline or carriage-return characters",
			))
		}
	}
	return errs
}

// validateSiblingDefault enforces the single-default invariant: at most one
// BarbicanSecretStore attached to a given Barbican may be marked isDefault. When
// the CR under validation is a default, it lists its namespace siblings (uncached
// reader), filters to the same spec.barbicanRef.name, skips self and Terminating
// siblings, and rejects if another default already exists.
func (w *BarbicanSecretStoreWebhook) validateSiblingDefault(ctx context.Context, specPath *field.Path, b *BarbicanSecretStore) field.ErrorList {
	// Skip when no lookup client is injected (e.g. direct unit invocation without a
	// reader) — mirrors the PriorityClass validator's behavior.
	if w.Client == nil || !b.Spec.IsDefault {
		return nil
	}

	isDefaultPath := specPath.Child("isDefault")
	siblings, err := w.attachedSiblings(ctx, b)
	if err != nil {
		return field.ErrorList{field.InternalError(isDefaultPath,
			fmt.Errorf("listing BarbicanSecretStores for the single-default check: %w", err))}
	}

	var errs field.ErrorList
	for _, other := range siblings {
		if other.Spec.IsDefault {
			errs = append(errs, field.Invalid(
				isDefaultPath,
				b.Spec.IsDefault,
				fmt.Sprintf("BarbicanSecretStore %q attached to the same Barbican %q is already marked isDefault; exactly one default store is allowed",
					other.Name, b.Spec.BarbicanRef.Name),
			))
		}
	}
	return errs
}

// validateSiblingOpenBaoUniqueness enforces that at most one OpenBao-typed store
// is attached to a given Barbican, whether or not it is the default.
//
// The vault plugin reads its server URL, its credentials, and its mount from the
// process-global [vault_plugin] section, which every OpenBao store in the same
// barbican.conf shares. A second OpenBao store would therefore not get a second
// server: both [secretstore:<name>] sections would resolve to the same plugin
// configuration, and whichever store rendered last would decide where the other
// one's secrets are written.
func (w *BarbicanSecretStoreWebhook) validateSiblingOpenBaoUniqueness(ctx context.Context, specPath *field.Path, b *BarbicanSecretStore) field.ErrorList {
	if w.Client == nil || b.Spec.Type != BarbicanSecretStoreTypeOpenBao {
		return nil
	}

	typePath := specPath.Child("type")
	siblings, err := w.attachedSiblings(ctx, b)
	if err != nil {
		return field.ErrorList{field.InternalError(typePath,
			fmt.Errorf("listing BarbicanSecretStores for the OpenBao-uniqueness check: %w", err))}
	}

	var errs field.ErrorList
	for _, other := range siblings {
		if other.Spec.Type == BarbicanSecretStoreTypeOpenBao {
			errs = append(errs, field.Invalid(
				typePath,
				b.Spec.Type,
				fmt.Sprintf("BarbicanSecretStore %q attached to the same Barbican %q is already of type OpenBao; the vault plugin's [vault_plugin] options are process-global, so both stores would share one server, one credential, and one mount",
					other.Name, b.Spec.BarbicanRef.Name),
			))
		}
	}
	return errs
}

// attachedSiblings lists the live stores attached to the same Barbican as b. It
// is the List-and-filter skeleton both sibling rules run: each keeps its own
// entry predicate, path and message, and neither repeats the namespace-scoped
// List or the attachedSibling filter.
func (w *BarbicanSecretStoreWebhook) attachedSiblings(ctx context.Context, b *BarbicanSecretStore) ([]*BarbicanSecretStore, error) {
	var siblings BarbicanSecretStoreList
	if err := w.Client.List(ctx, &siblings, client.InNamespace(b.Namespace)); err != nil {
		return nil, err
	}
	var attached []*BarbicanSecretStore
	for i := range siblings.Items {
		if other := &siblings.Items[i]; attachedSibling(other, b) {
			attached = append(attached, other)
		}
	}
	return attached, nil
}

// attachedSibling reports whether other is a live store the sibling rules have to
// consider for b: attached to the same Barbican, not b itself (which appears in
// the List on UPDATE), and not Terminating — blocking a replacement on a store on
// its way out would deadlock recreate-during-teardown.
func attachedSibling(other, b *BarbicanSecretStore) bool {
	return other.Name != b.Name &&
		other.DeletionTimestamp == nil &&
		other.Spec.BarbicanRef.Name == b.Spec.BarbicanRef.Name
}
