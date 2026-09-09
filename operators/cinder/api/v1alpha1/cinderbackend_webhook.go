// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/validation"
	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// Name-length bounds on a CinderBackend, both derived from an identifier the
// detach builds out of metadata.name.
const (
	// MaxBackendNamePlusCinderRef bounds metadata.name plus spec.cinderRef.name
	// together. Detaching a backend runs a "<cinder>-<name>-service-remove" Job,
	// and that Job's name is copied into a label value, which Kubernetes caps at
	// 63 characters: the separator and the 15-character suffix leave the two
	// names 47 between them. The budget is shared rather than per-name because
	// either name may be the long one.
	MaxBackendNamePlusCinderRef = 47

	// MaxBackendNameLength bounds metadata.name on its own. The same detach
	// records the Job's terminal state under the dedupe annotation key
	// "cobaltcore.c5c3.io/last-service-remove-<name>-job-uid" on the Cinder CR,
	// and Kubernetes caps an annotation key's name part at 63 characters, of
	// which the literals around the name spend 28. The two are separate bounds
	// because they measure different things: a short Cinder name buys room under
	// the shared budget but none here.
	//
	// The literals are duplicated from internal/common/job (the key format) and
	// the controller package (the component), which the api package cannot
	// reference from a webhook. Without the bound a backend named past it still
	// detaches, but the annotation patch is rejected as an invalid key, so its
	// detach metric is never emitted and the deferral records a Warning event
	// claiming a transient failure that never clears.
	MaxBackendNameLength = 63 - len("last-service-remove-") - len("-job-uid")
)

// nfsExtraOptionsDenylist enumerates the [<name>] backend-section option names
// spec.extraOptions must never carry, mapped to the spec field or component that
// owns each. The operator renders all of them from the typed fields, so a
// duplicate would silently shadow — or be shadowed by — the typed value depending
// on render order.
var nfsExtraOptionsDenylist = map[string]string{
	"volume_driver":                  "spec.type",
	"volume_backend_name":            "the operator (backend section wiring)",
	"backend_host":                   "the operator (the host identity this backend's volumes are keyed by)",
	"nfs_shares_config":              "spec.nfs.server and spec.nfs.path",
	"nfs_mount_point_base":           "the operator (the mount path the cinder-volume pod carries)",
	"nfs_mount_options":              "spec.nfs.mountOptions",
	"nas_secure_file_operations":     "the operator (it follows the pod's security context)",
	"nas_secure_file_permissions":    "the operator (it follows the pod's security context)",
	"nfs_snapshot_support":           "the operator (NFS driver wiring)",
	"nfs_sparsed_volumes":            "the operator (NFS driver wiring)",
	"nfs_qcow2_volumes":              "the operator (NFS driver wiring)",
	"image_volume_cache_enabled":     "spec.imageVolumeCache.enabled",
	"image_volume_cache_max_size_gb": "spec.imageVolumeCache.maxSizeGB",
	"image_volume_cache_max_count":   "spec.imageVolumeCache.maxCount",
}

// reservedBackendNames is the set of cinder.conf section names across every
// embedded option catalog, lower-cased. A CinderBackend's metadata.name becomes
// its [<name>] section, so a name equal to any of them would clobber — or be
// clobbered by — a section cinder or the operator already writes. It is computed
// once on first use rather than at package initialization so a package that never
// admits a backend never pays for it.
var reservedBackendNames = sync.OnceValue(func() map[string]struct{} {
	names := make(map[string]struct{})
	for _, catalog := range optionCatalogs {
		for section := range catalog.Sections {
			names[strings.ToLower(section)] = struct{}{}
		}
	}
	return names
})

// CinderBackendWebhook implements defaulting and validation webhooks for the
// CinderBackend CRD. Client is injected at startup to resolve spec.cinderRef for
// the image-volume-cache warning. Production wiring injects mgr.GetAPIReader() —
// a direct, uncached reader — so admission never misses a just-created Cinder
// from a stale informer cache and no lazy informer start happens inside the
// webhook timeout.
// +kubebuilder:object:generate=false
type CinderBackendWebhook struct {
	commonwebhook.NoopDeleteValidator[*CinderBackend]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*CinderBackend] = &CinderBackendWebhook{}
	_ admission.Validator[*CinderBackend] = &CinderBackendWebhook{}
)

// +kubebuilder:webhook:path=/mutate-cinder-openstack-c5c3-io-v1alpha1-cinderbackend,mutating=true,failurePolicy=fail,sideEffects=None,groups=cinder.openstack.c5c3.io,resources=cinderbackends,verbs=create;update,versions=v1alpha1,name=mcinderbackend.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-cinder-openstack-c5c3-io-v1alpha1-cinderbackend,mutating=false,failurePolicy=fail,sideEffects=None,groups=cinder.openstack.c5c3.io,resources=cinderbackends,verbs=create;update,versions=v1alpha1,name=vcinderbackend.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with
// the manager.
func (w *CinderBackendWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*CinderBackend](mgr, &CinderBackend{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*CinderBackend]. It materializes the
// documented defaults when the fields carry zero values, so the production
// admission path has a single source of truth; the +kubebuilder:default markers
// remain as defense-in-depth for callers that bypass the webhook (e.g. envtest
// without the defaulter wired up).
func (w *CinderBackendWebhook) Default(_ context.Context, obj *CinderBackend) error {
	// Mirror the +kubebuilder:default marker on NFSBackendSpec.MountOptions for
	// webhook-bypassing callers: fill the soft NFSv4.1 mount when the nfs block
	// exists and the field is empty.
	if obj.Spec.NFS != nil && obj.Spec.NFS.MountOptions == "" {
		obj.Spec.NFS.MountOptions = DefaultNFSMountOptions
	}
	return nil
}

// ValidateCreate implements admission.Validator[*CinderBackend].
//
// The combined name bound is enforced here rather than in validate(), which
// update shares: both names are immutable (metadata.name always, cinderRef by the
// CEL transition rule), so on update the rule could only ever fire against an
// object a pre-upgrade operator already admitted — and the validating webhook
// also sees the finalizer-removal update reconcileDelete issues, so rejecting it
// would wedge that CR in Terminating with no field left to edit to repair it.
func (w *CinderBackendWebhook) ValidateCreate(ctx context.Context, obj *CinderBackend) (admission.Warnings, error) {
	return w.warnImageVolumeCacheWithoutInternalTenant(ctx, obj), w.validate(obj, validateBackendNameBudget(obj))
}

// validateBackendNameBudget enforces the two bounds a detach puts on the name:
// the shared 47-character budget the service-remove Job's label value leaves
// metadata.name and spec.cinderRef.name, and the 35 characters that Job's
// dedupe annotation key leaves metadata.name alone.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateBackendNameBudget(b *CinderBackend) field.ErrorList {
	namePath := field.NewPath("metadata", "name")
	var errs field.ErrorList
	if len(b.Name)+len(b.Spec.CinderRef.Name) > MaxBackendNamePlusCinderRef {
		errs = append(errs, field.Invalid(
			namePath, b.Name,
			fmt.Sprintf("metadata.name plus spec.cinderRef.name must not exceed %d characters: "+
				"detaching this backend runs the %q Job, whose name is copied into a label value Kubernetes caps at 63 characters",
				MaxBackendNamePlusCinderRef, "<cinder>-<name>-service-remove"),
		))
	}
	if len(b.Name) > MaxBackendNameLength {
		errs = append(errs, field.Invalid(
			namePath, b.Name,
			fmt.Sprintf("metadata.name must not exceed %d characters: detaching this backend records its "+
				"terminal state under the annotation key %q on the Cinder, and Kubernetes caps an annotation key's name part at 63 characters",
				MaxBackendNameLength, "cobaltcore.c5c3.io/last-service-remove-<name>-job-uid"),
		))
	}
	return errs
}

// ValidateUpdate implements admission.Validator[*CinderBackend].
// Field immutability (cinderRef, type) is enforced by the CRD CEL transition
// rules, so the webhook re-runs only the value-level rules against the new
// object.
func (w *CinderBackendWebhook) ValidateUpdate(ctx context.Context, _, newObj *CinderBackend) (admission.Warnings, error) {
	return w.warnImageVolumeCacheWithoutInternalTenant(ctx, newObj), w.validate(newObj, nil)
}

// validate runs all value-level rules against the CinderBackend spec,
// accumulating every violation into a single field.ErrorList so users see all
// problems at once. extra carries the errors accumulated by the caller (on create
// the combined name bound).
func (w *CinderBackendWebhook) validate(b *CinderBackend, extra field.ErrorList) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Defense-in-depth union check alongside the spec-level CEL rule: exactly one
	// backend block, matching spec.type.
	if (b.Spec.Type == CinderBackendTypeNFS) != (b.Spec.NFS != nil) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("nfs"),
			b.Spec.Type,
			"exactly one backend block matching spec.type must be set (type NFS requires spec.nfs)",
		))
	}

	allErrs = append(allErrs, validateBackendName(b)...)
	if b.Spec.NFS != nil {
		allErrs = append(allErrs, validateNFSExport(
			specPath.Child("nfs"), b.Spec.NFS.Path, b.Spec.NFS.MountOptions)...)
	}
	allErrs = append(allErrs, validation.ExtraOptions(
		specPath.Child("extraOptions"), b.Spec.ExtraOptions, validation.ExtraOptionsRules{
			Denylist: nfsExtraOptionsDenylist,
		})...)
	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "CinderBackend"},
			b.Name,
			allErrs,
		)
	}
	return nil
}

// validateBackendName rejects a metadata.name the backend cannot carry into
// cinder.conf. The name becomes three things at once: the [<name>] config
// section, the volume_backend_name volume types schedule against, and the
// "<cinder>@<name>" host identity the backend's volumes are keyed by. The
// comparison is done on the lower-cased name (Kubernetes names are already
// lowercase), which also makes the DEFAULT match case-insensitive.
func validateBackendName(b *CinderBackend) field.ErrorList {
	namePath := field.NewPath("metadata", "name")
	lower := strings.ToLower(b.Name)

	switch {
	case lower == "default":
		return field.ErrorList{field.Invalid(namePath, b.Name,
			"name must not be \"default\": it names cinder.conf's [DEFAULT] section, so the backend's options would be read as service-wide ones")}
	case isReservedBackendName(lower):
		return field.ErrorList{field.Invalid(namePath, b.Name,
			fmt.Sprintf("name %q collides with the [%s] section cinder.conf already carries", b.Name, lower))}
	case strings.ContainsAny(b.Name, "@#"):
		return field.ErrorList{field.Invalid(namePath, b.Name,
			`name must not contain "@" or "#": cinder reads them as the separators of a service identity ("<cinder>@<backend>") and of a pool within it ("<host>#<pool>")`)}
	}
	return nil
}

// isReservedBackendName reports whether the lower-cased name is a section name
// one of the embedded option catalogs enumerates.
func isReservedBackendName(lower string) bool {
	_, reserved := reservedBackendNames()[lower]
	return reserved
}

// warnImageVolumeCacheWithoutInternalTenant surfaces a cache that cannot work
// yet: the cached image volumes are owned by the deployment rather than by the
// requesting tenant, so cinder needs the Cinder's spec.internalTenant to create
// them under. It is a warning rather than a rejection because the two CRs are
// applied independently — GitOps ordering may present the backend first, and the
// Cinder may gain the block a moment later.
//
// Every lookup failure is silent: a NotFound Cinder is the ordering case above, a
// nil reader is a programmatically constructed webhook, and any other error is a
// transient API problem that must not turn into a warning claiming a
// misconfiguration.
func (w *CinderBackendWebhook) warnImageVolumeCacheWithoutInternalTenant(ctx context.Context, b *CinderBackend) admission.Warnings {
	if w.Client == nil || b.Spec.ImageVolumeCache == nil || !b.Spec.ImageVolumeCache.Enabled {
		return nil
	}

	cinder := &Cinder{}
	key := types.NamespacedName{Name: b.Spec.CinderRef.Name, Namespace: b.Namespace}
	if err := w.Client.Get(ctx, key, cinder); err != nil {
		return nil
	}
	if cinder.Spec.InternalTenant != nil {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.imageVolumeCache is enabled but Cinder %q sets no spec.internalTenant: the cached image volumes have no project and user to be created under, so the cache stays inactive until that block is added",
		b.Spec.CinderRef.Name,
	)}
}
