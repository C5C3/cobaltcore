// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"cmp"
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/validation"
	commonwebhook "github.com/c5c3/cobaltcore/internal/common/webhook"
)

// Backup defaults for the recurring OVN database backup.
//
// They are consumed by the reconcile-time resolver and deliberately NOT applied
// by the defaulting webhook: a nil or partial spec.backup block keeps tracking
// these operator defaults across upgrades instead of freezing today's values
// into the stored CR. The validating webhook reads the retention to tell a
// reduction from an edit that leaves the window alone.
const (
	// DefaultBackupSchedule runs the backup daily at 02:00, resolved when
	// spec.backup.schedule is empty. The OVN databases are small enough that the
	// snapshot finishes in seconds, so the slot only has to avoid the busiest
	// hours of the control plane.
	DefaultBackupSchedule = "0 2 * * *"
	// DefaultBackupRetentionDays is how long a snapshot is kept, resolved when
	// spec.backup.retentionDays is unset. Two weeks spans a full change-freeze
	// window, which is the interval over which a logical-model regression is
	// still traceable to the change that caused it.
	DefaultBackupRetentionDays int32 = 14
)

// NodePort defaults for the two database ranges. Each Raft member is published
// on its own node port, assigned in ordinal order from the base, because a Raft
// client addresses the members individually rather than through one
// load-balanced name.
const (
	// DefaultNorthboundNodePortBase is the first Northbound node port, resolved
	// when spec.northbound.nodePortBase is nil. It carries the OVN Northbound
	// port 6641 in its last two digits so the mapping is readable off the port
	// number.
	DefaultNorthboundNodePortBase int32 = 30641
	// DefaultSouthboundNodePortBase is the first Southbound node port, resolved
	// when spec.southbound.nodePortBase is nil, following the same convention
	// against port 6642. It sits ten ports above the Northbound base, which
	// leaves both ranges room to grow to the five-replica ceiling without
	// colliding.
	DefaultSouthboundNodePortBase int32 = 30651
	// maxNodePort is the top of the Kubernetes default node-port range, the
	// ceiling a base plus its replicas has to stay under.
	maxNodePort int32 = 32767
	// defaultDatabaseReplicas mirrors the +kubebuilder:default on
	// OVNDatabaseSpec.Replicas. The webhook resolves it so a call that bypasses
	// API-server defaulting (a unit test, or the ControlPlane operator validating
	// a projected spec) computes the same port ranges as admission does.
	defaultDatabaseReplicas int32 = 3
)

// Name-length bounds enforced on metadata.name, driven by the backup CronJob,
// the child object with the tightest name budget.
const (
	// MaxCronJobNameLength is the API server's own cap on a CronJob name:
	// DNS1035LabelMaxLength (63) minus the 11-character "-<timestamp>" suffix its
	// controller appends to every Job it spawns.
	MaxCronJobNameLength = 52
	// backupNameSuffix is appended to metadata.name to name the backup CronJob.
	// It is duplicated from the controller package (which builds the object)
	// because the api package cannot import the controller.
	backupNameSuffix = "-backup"
	// MaxOVNCentralNameLength is what those two leave for metadata.name.
	MaxOVNCentralNameLength = MaxCronJobNameLength - len(backupNameSuffix)
)

// OVNCentralWebhook implements defaulting and validation webhooks for the
// OVNCentral CRD. Client is injected at startup for cluster-scoped resource
// lookups. Production wiring injects mgr.GetAPIReader(), a direct uncached
// reader, so admission never rejects a just-created object from a stale informer
// cache and no lazy informer start happens inside the webhook timeout.
// +kubebuilder:object:generate=false
type OVNCentralWebhook struct {
	commonwebhook.NoopDeleteValidator[*OVNCentral]

	Client client.Reader
}

// Compile-time interface checks.
var (
	_ admission.Defaulter[*OVNCentral] = &OVNCentralWebhook{}
	_ admission.Validator[*OVNCentral] = &OVNCentralWebhook{}
)

// +kubebuilder:webhook:path=/mutate-ovn-openstack-c5c3-io-v1alpha1-ovncentral,mutating=true,failurePolicy=fail,sideEffects=None,groups=ovn.openstack.c5c3.io,resources=ovncentrals,verbs=create;update,versions=v1alpha1,name=movncentral.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-ovn-openstack-c5c3-io-v1alpha1-ovncentral,mutating=false,failurePolicy=fail,sideEffects=None,groups=ovn.openstack.c5c3.io,resources=ovncentrals,verbs=create;update,versions=v1alpha1,name=vovncentral.kb.io,admissionReviewVersions=v1

// SetupWebhookWithManager registers the defaulting and validating webhooks with the manager.
func (w *OVNCentralWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy[*OVNCentral](mgr, &OVNCentral{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// Default implements admission.Defaulter[*OVNCentral]. It leaves the object
// untouched: every OVNCentral default is either a +kubebuilder:default the API
// server applies from the CRD schema, or a value the operator resolves at
// reconcile time (the image, the two nodePort bases, the backup schedule and
// retention) so an unset field keeps tracking the operator default across
// upgrades instead of freezing today's value into the stored CR.
//
// The mutating webhook is registered nonetheless, so a default that has to be
// materialized later can be added without changing the deployed webhook
// configuration.
func (w *OVNCentralWebhook) Default(_ context.Context, _ *OVNCentral) error {
	return nil
}

// ValidateCreate implements admission.Validator[*OVNCentral].
//
// The metadata.name bound is enforced here rather than in validate(), which
// update shares: the name is immutable, so on update the rule could only ever
// fire against an object a pre-upgrade operator already admitted, and the
// validating webhook also sees the finalizer-removal update reconcileDelete
// issues, so rejecting it would wedge that CR in Terminating with no field left
// to edit to repair it.
func (w *OVNCentralWebhook) ValidateCreate(_ context.Context, obj *OVNCentral) (admission.Warnings, error) {
	return nil, w.validate(obj, validateOVNCentralNameLength(obj.Name))
}

// validateOVNCentralNameLength bounds metadata.name by the child object with the
// tightest name budget, the "{name}-backup" CronJob: the API server rejects a
// CronJob name longer than MaxCronJobNameLength.
//
// It is called from ValidateCreate only, for the reason documented there.
func validateOVNCentralNameLength(name string) field.ErrorList {
	if len(name) <= MaxOVNCentralNameLength {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"), name,
		fmt.Sprintf("name must be at most %d characters: the backup CronJob appends %q and Kubernetes caps CronJob names at %d characters",
			MaxOVNCentralNameLength, backupNameSuffix, MaxCronJobNameLength),
	)}
}

// ValidateUpdate implements admission.Validator[*OVNCentral].
//
// spec.targetClusterRef is compared across both revisions here, the webhook-layer
// twin of the two transition CEL rules on OVNCentralSpec.
func (w *OVNCentralWebhook) ValidateUpdate(_ context.Context, oldObj, newObj *OVNCentral) (admission.Warnings, error) {
	updateErrs := validation.TargetClusterRefImmutable(
		field.NewPath("spec", "targetClusterRef"),
		oldObj.Spec.TargetClusterRef,
		newObj.Spec.TargetClusterRef,
	)
	return warnBackupRetention(oldObj.Spec.Backup, newObj.Spec.Backup), w.validate(newObj, updateErrs)
}

// validate runs all validation rules against the OVNCentral spec, accumulating
// every violation so users see the full list in one admission response. extra
// carries the errors accumulated by the caller (on create the metadata.name
// bound, on update the targetClusterRef immutability check) so they aggregate
// into the single Invalid error alongside the rest.
func (w *OVNCentralWebhook) validate(c *OVNCentral, extra field.ErrorList) error {
	specPath := field.NewPath("spec")

	allErrs := validation.TargetClusterRef(specPath.Child("targetClusterRef"), c.Spec.TargetClusterRef)
	allErrs = append(allErrs, validateImage(specPath.Child("image"), c.Spec.Image)...)
	allErrs = append(allErrs, validateNodePortRanges(specPath, &c.Spec)...)
	allErrs = append(allErrs, validateBackup(specPath.Child("backup"), c.Spec.Backup)...)

	// TLS is not optional: the OVN databases carry the whole logical network
	// model, so a listener without a certificate lets any pod that reaches the
	// port rewrite the network. It is also the only control there is, since the
	// connection rows carry no role= column.
	if c.Spec.TLS.IssuerRef.Name == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("tls", "issuerRef", "name"), "issuerRef.name must be set",
		))
	}

	allErrs = append(allErrs, extra...)

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "OVNCentral"},
			c.Name,
			allErrs,
		)
	}
	return nil
}

// validateNodePortRanges checks the two node-port ranges the databases are
// published on. Each range runs from its base over as many consecutive ports as
// there are Raft members, so two bases that look far apart still collide once
// both databases are scaled up. The check runs against the effective bases and
// replica counts, not the literal fields, because both are resolved rather than
// stored.
func validateNodePortRanges(fldPath *field.Path, spec *OVNCentralSpec) field.ErrorList {
	nbPath := fldPath.Child("northbound", "nodePortBase")
	sbPath := fldPath.Child("southbound", "nodePortBase")

	nbBase := ptr.Deref(spec.Northbound.NodePortBase, DefaultNorthboundNodePortBase)
	sbBase := ptr.Deref(spec.Southbound.NodePortBase, DefaultSouthboundNodePortBase)
	nbReplicas := cmp.Or(spec.Northbound.Replicas, defaultDatabaseReplicas)
	sbReplicas := cmp.Or(spec.Southbound.Replicas, defaultDatabaseReplicas)

	errs := validateNodePortRoom(nbPath, nbBase, nbReplicas)
	errs = append(errs, validateNodePortRoom(sbPath, sbBase, sbReplicas)...)

	if nbBase <= sbBase+sbReplicas-1 && sbBase <= nbBase+nbReplicas-1 {
		errs = append(errs, field.Invalid(
			sbPath, sbBase, "northbound and southbound nodePort ranges overlap",
		))
	}
	return errs
}

// validateNodePortRoom rejects a base whose range runs past the top of the
// node-port range. The Service for the last member would be rejected by the API
// server, leaving that Raft member unreachable from outside the cluster with
// nothing in the OVNCentral spec to point at.
func validateNodePortRoom(fldPath *field.Path, base, replicas int32) field.ErrorList {
	if base+replicas-1 <= maxNodePort {
		return nil
	}
	return field.ErrorList{field.Invalid(
		fldPath, base,
		fmt.Sprintf("nodePortBase %d leaves no room for %d replicas below %d", base, replicas, maxNodePort),
	)}
}

// validateBackup checks the backup block. A nil block is valid: the operator
// schedules the backup with its own defaults, since an OVN control plane with no
// snapshot has no way back from an operator error applied to every Raft member.
func validateBackup(fldPath *field.Path, backup *OVNBackupSpec) field.ErrorList {
	if backup == nil {
		return nil
	}
	var errs field.ErrorList

	// Defense-in-depth retention check alongside the
	// +kubebuilder:validation:Minimum=1 marker. Zero would delete every snapshot
	// the run just took.
	if backup.RetentionDays != nil && *backup.RetentionDays < 1 {
		errs = append(errs, field.Invalid(
			fldPath.Child("retentionDays"), *backup.RetentionDays, "retentionDays must be at least 1",
		))
	}

	// An empty schedule resolves DefaultBackupSchedule, so only a stated one is
	// parsed. The CronJob controller would otherwise reject the projected object
	// with an error nothing surfaces back onto the OVNCentral.
	if backup.Schedule != "" {
		errs = append(errs, validation.CronSchedule(fldPath.Child("schedule"), backup.Schedule)...)
	}

	if backup.S3 != nil {
		s3Path := fldPath.Child("s3")
		if backup.S3.CredentialsSecretRef.Name == "" {
			errs = append(errs, field.Required(
				s3Path.Child("credentialsSecretRef", "name"), "credentialsSecretRef.name must be set",
			))
		}
		errs = append(errs, validateImage(s3Path.Child("image"), backup.S3.Image)...)
	}
	return errs
}

// warnBackupRetention surfaces a retention window that an update shortens.
// Unlike every other knob on the CR the effect is immediate and irreversible: at
// the next firing the backup deletes every snapshot that fell out of the window,
// and nothing brings those back. A typo (3 for 30) is indistinguishable from an
// intended change at admission time, so the reduction is echoed back to whoever
// made it. It stays a warning rather than a rejection because shortening the
// window is a legitimate operational choice.
func warnBackupRetention(oldBackup, newBackup *OVNBackupSpec) admission.Warnings {
	oldDays, newDays := effectiveRetentionDays(oldBackup), effectiveRetentionDays(newBackup)
	if newDays >= oldDays {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.backup.retentionDays reduced %d → %d: the next backup run deletes the snapshots taken between %d and %d days ago, which cannot be undone",
		oldDays, newDays, newDays, oldDays,
	)}
}

// effectiveRetentionDays resolves the retention a spec.backup block runs with,
// mirroring the reconcile-time resolver so a block that leaves the field unset
// compares as the operator default rather than as zero. Otherwise dropping
// spec.backup entirely would read as a reduction to nothing.
func effectiveRetentionDays(b *OVNBackupSpec) int32 {
	if b == nil || b.RetentionDays == nil {
		return DefaultBackupRetentionDays
	}
	return *b.RetentionDays
}
