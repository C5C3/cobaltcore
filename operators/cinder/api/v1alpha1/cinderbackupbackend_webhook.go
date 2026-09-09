// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"slices"

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

// backupCompressionAlgorithms are the values spec.compression accepts,
// mirroring the Enum marker on CinderBackupBackendSpec.Compression. They are
// cinder's own choices for [DEFAULT] backup_compression_algorithm; a value
// outside them fails every backup at runtime rather than at admission.
var backupCompressionAlgorithms = []string{"none", "zlib", "bz2", "zstd"}

// nfsBackupExtraOptionsDenylist enumerates the backup-section option names
// spec.extraOptions must never carry, mapped to the spec field or component that
// owns each. The operator renders all of them from the typed fields, so a
// duplicate would silently shadow — or be shadowed by — the typed value depending
// on render order.
var nfsBackupExtraOptionsDenylist = map[string]string{
	"backup_driver":                "spec.type",
	"backup_share":                 "spec.nfs.server and spec.nfs.path",
	"backup_mount_point_base":      "the operator (the mount path the cinder-backup pod carries)",
	"backup_mount_options":         "spec.nfs.mountOptions",
	"backup_file_size":             "spec.fileSize",
	"backup_compression_algorithm": "spec.compression",
	"backup_use_same_host":         "the operator (backup service wiring)",
	"host":                         "the operator (the host identity this deployment's backups are recorded against)",
}

// CinderBackupBackendWebhook implements defaulting and validation webhooks for
// the CinderBackupBackend CRD. Client is injected at startup for the
// single-attachment check (a namespace-scoped List of sibling backup backends).
// Production wiring injects mgr.GetAPIReader() — a direct, uncached reader — so
// admission never misses a just-created sibling from a stale informer cache and
// no lazy informer start happens inside the webhook timeout.
// +kubebuilder:object:generate=false
type CinderBackupBackendWebhook struct {
	commonwebhook.NoopDeleteValidator[*CinderBackupBackend]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*CinderBackupBackend] = &CinderBackupBackendWebhook{}
	_ admission.Validator[*CinderBackupBackend] = &CinderBackupBackendWebhook{}
)

// +kubebuilder:webhook:path=/mutate-cinder-openstack-c5c3-io-v1alpha1-cinderbackupbackend,mutating=true,failurePolicy=fail,sideEffects=None,groups=cinder.openstack.c5c3.io,resources=cinderbackupbackends,verbs=create;update,versions=v1alpha1,name=mcinderbackupbackend.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-cinder-openstack-c5c3-io-v1alpha1-cinderbackupbackend,mutating=false,failurePolicy=fail,sideEffects=None,groups=cinder.openstack.c5c3.io,resources=cinderbackupbackends,verbs=create;update,versions=v1alpha1,name=vcinderbackupbackend.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with
// the manager.
func (w *CinderBackupBackendWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*CinderBackupBackend](mgr, &CinderBackupBackend{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*CinderBackupBackend]. It materializes
// the documented defaults when the fields carry zero values, so the production
// admission path has a single source of truth; the +kubebuilder:default markers
// remain as defense-in-depth for callers that bypass the webhook (e.g. envtest
// without the defaulter wired up).
func (w *CinderBackupBackendWebhook) Default(_ context.Context, obj *CinderBackupBackend) error {
	if obj.Spec.FileSize == nil {
		size := DefaultBackupFileSize
		obj.Spec.FileSize = &size
	}
	if obj.Spec.Compression == "" {
		obj.Spec.Compression = DefaultBackupCompression
	}
	if obj.Spec.NFS != nil && obj.Spec.NFS.MountOptions == "" {
		obj.Spec.NFS.MountOptions = DefaultNFSMountOptions
	}
	return nil
}

// ValidateCreate implements admission.Validator[*CinderBackupBackend].
func (w *CinderBackupBackendWebhook) ValidateCreate(ctx context.Context, obj *CinderBackupBackend) (admission.Warnings, error) {
	return nil, w.validate(ctx, obj)
}

// ValidateUpdate implements admission.Validator[*CinderBackupBackend].
// Field immutability (cinderRef, type) is enforced by the CRD CEL transition
// rules, so the webhook re-runs only the value-level rules against the new
// object. The single-attachment check runs on update too: it is a rule about the
// namespace rather than about this object, and the object under validation
// excludes itself from the sibling list.
func (w *CinderBackupBackendWebhook) ValidateUpdate(ctx context.Context, _, newObj *CinderBackupBackend) (admission.Warnings, error) {
	return nil, w.validate(ctx, newObj)
}

// validate runs all validation rules against the CinderBackupBackend spec,
// accumulating every violation into a single field.ErrorList so users see all
// problems at once. ctx is required for the sibling List behind the
// single-attachment rule.
func (w *CinderBackupBackendWebhook) validate(ctx context.Context, b *CinderBackupBackend) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Defense-in-depth union check alongside the spec-level CEL rule: exactly one
	// backup backend block, matching spec.type.
	if (b.Spec.Type == CinderBackupBackendTypeNFS) != (b.Spec.NFS != nil) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("nfs"),
			b.Spec.Type,
			"exactly one backup backend block matching spec.type must be set (type NFS requires spec.nfs)",
		))
	}

	// Defense-in-depth bounds on the chunk size alongside the Minimum and
	// MultipleOf markers on CinderBackupBackendSpec.FileSize. cinder hashes each
	// chunk in blocks of backup_sha_block_size_bytes and refuses a chunk size that
	// block size does not divide, so an unaligned value fails every backup at
	// runtime.
	if b.Spec.FileSize != nil {
		fileSizePath := specPath.Child("fileSize")
		if *b.Spec.FileSize < MinBackupFileSize {
			allErrs = append(allErrs, field.Invalid(
				fileSizePath, *b.Spec.FileSize,
				fmt.Sprintf("fileSize must be at least %d bytes", MinBackupFileSize),
			))
		} else if *b.Spec.FileSize%BackupFileSizeMultiple != 0 {
			allErrs = append(allErrs, field.Invalid(
				fileSizePath, *b.Spec.FileSize,
				fmt.Sprintf("fileSize must be a multiple of %d bytes (the block size cinder hashes backup chunks in)", BackupFileSizeMultiple),
			))
		}
	}

	// Defense-in-depth enum check alongside the Enum marker on
	// CinderBackupBackendSpec.Compression. An empty value is how the CR asks for
	// the operator default, so only a set one is measured.
	if b.Spec.Compression != "" && !slices.Contains(backupCompressionAlgorithms, b.Spec.Compression) {
		allErrs = append(allErrs, field.NotSupported(
			specPath.Child("compression"), b.Spec.Compression, backupCompressionAlgorithms,
		))
	}

	if b.Spec.NFS != nil {
		allErrs = append(allErrs, validateNFSExport(
			specPath.Child("nfs"), b.Spec.NFS.Path, b.Spec.NFS.MountOptions)...)
	}

	allErrs = append(allErrs, validation.ExtraOptions(
		specPath.Child("extraOptions"), b.Spec.ExtraOptions, validation.ExtraOptionsRules{
			Denylist: nfsBackupExtraOptionsDenylist,
		})...)
	allErrs = append(allErrs, w.validateSingleAttachment(ctx, specPath, b)...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "CinderBackupBackend"},
			b.Name,
			allErrs,
		)
	}
	return nil
}

// validateSingleAttachment enforces the one-backup-driver invariant: the backup
// driver is a property of the single cinder-backup Deployment, not one of several
// backends it serves, so a second CinderBackupBackend pointing at the same Cinder
// describes a Deployment that cannot exist. It lists the namespace siblings
// (uncached reader), filters to the same spec.cinderRef.name, skips self and
// Terminating siblings, and rejects if another attachment is already there.
//
// It is skipped when no lookup client is injected (a programmatically constructed
// webhook), mirroring the shared PriorityClass validator's behavior.
func (w *CinderBackupBackendWebhook) validateSingleAttachment(ctx context.Context, specPath *field.Path, b *CinderBackupBackend) field.ErrorList {
	if w.Client == nil {
		return nil
	}

	cinderRefPath := specPath.Child("cinderRef")
	siblings, err := validation.AttachedSiblings(ctx, w.Client, b, &CinderBackupBackendList{},
		func(other *CinderBackupBackend) bool {
			return other.Spec.CinderRef.Name == b.Spec.CinderRef.Name
		})
	if err != nil {
		return field.ErrorList{field.InternalError(cinderRefPath,
			fmt.Errorf("listing CinderBackupBackends for the single-attachment check: %w", err))}
	}

	var errs field.ErrorList
	for _, other := range siblings {
		errs = append(errs, field.Forbidden(
			cinderRefPath,
			fmt.Sprintf("Cinder %q already has CinderBackupBackend %q attached", b.Spec.CinderRef.Name, other.Name),
		))
	}
	return errs
}
