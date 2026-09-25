// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The metadata shared secret is the value the Neutron metadata agent signs a
// proxied instance request with, and the value the compute service's metadata
// API checks that signature against. Both sides have to resolve the same Secret:
// a value only one of them knows leaves every call to 169.254.169.254 rejected.
//
// The ControlPlane sees both sides, so it generates the value rather than asking
// for one. The generation runs in ESO, through a Password generator and an
// ExternalSecret that draws from it: this leg writes two references and never
// reads the value it references. A ControlPlane whose agents are seeded from
// outside its reach names a Secret of its own instead
// (services.nova.metadataSharedSecretRef), and then this leg generates nothing.
//
// A metadata agent on a target cluster cannot read the Secret in the Nova
// namespace. For every agent that names "<cp>-nova-metadata-agent-secret", the
// ControlPlane reads the value out of the compute contract and copies it, under
// novaMetadataSecretKey, into the agent's namespace on the agent's cluster
// (reconcileNovaMetadataAgentSecrets). That is the one path on which the
// operator reads the value.

// novaMetadataSecretKey is the key the generated Secret carries the shared value
// under. It is the key both consumers default their reference to: the Nova CRD's
// spec.metadata.sharedSecretRef and the NeutronMetadataAgent's
// spec.novaMetadata.sharedSecretRef. The ControlPlane creates no metadata agent,
// so an agent written by hand that names only the Secret reaches the value
// without having to know how it was generated.
//
// The Password generator of the ESO 0.x line the deploy stack pins writes its
// value under "password" and has no option to rename it, so the ExternalSecret
// rewrites that one key (see novaMetadataExternalSecret).
const novaMetadataSecretKey = "shared_secret" //nolint:gosec // G101 false positive: Secret data key, not a credential.

// novaMetadataGeneratorKey is the key the Password generator writes its value
// under, the one the ExternalSecret rewrites to novaMetadataSecretKey.
const novaMetadataGeneratorKey = "password" //nolint:gosec // G101 false positive: Secret data key, not a credential.

// novaMetadataSecretName returns the deterministic name of the generated
// metadata shared-secret Secret, of the ExternalSecret that materialises it, and
// of the Password generator behind it ("<cp>-nova-metadata-secret"). All three
// share the name, the way the DB-credential trio does.
func novaMetadataSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return novaName(cp) + "-metadata-secret"
}

// novaMetadataPasswordGenerator builds the ESO generator that mints the shared
// value. PURE builder: the reconciler claims ownership and applies it.
//
// Thirty-two characters with no symbols. The value is rendered into a nova.conf
// option and into the metadata agent's configuration on every compute node, both
// INI files, so a symbol-free alphabet keeps it from depending on either
// renderer's quoting. Repeats are allowed because refusing them would have the
// generator draw 32 DISTINCT characters out of the alphabet the symbols were
// taken from, and a 32-character random string loses nothing measurable by
// repeating one.
func novaMetadataPasswordGenerator(cp *c5c3v1alpha1.ControlPlane) *esgenv1alpha1.Password {
	return &esgenv1alpha1.Password{
		ObjectMeta: metav1.ObjectMeta{Name: novaMetadataSecretName(cp), Namespace: cp.NovaNamespace()},
		Spec: esgenv1alpha1.PasswordSpec{
			Length:      32,
			Symbols:     ptr.To(0),
			AllowRepeat: true,
		},
	}
}

// novaMetadataExternalSecret builds the ExternalSecret that materialises the
// generator's output into the Nova namespace. It carries no SecretStoreRef: the
// generatorRef is the sole source, the shape dbCredentialGeneratorExternalSecret
// already takes. The one rewrite rule renames the generator's "password" key to
// novaMetadataSecretKey, the key both consumers look for by default.
//
// RefreshInterval is ZERO, which turns the periodic sync off. A Password
// generator mints a NEW value on every read, so a refreshing ExternalSecret
// would rewrite the shared secret on its own schedule while every metadata agent
// on every compute node still carries the previous one, and every instance's
// call to 169.254.169.254 would be rejected until each agent had been
// reconfigured. The value is generated exactly once, and rotating it is a
// deliberate act (delete the Secret) rather than a timer.
func novaMetadataExternalSecret(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	name := novaMetadataSecretName(cp)
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cp.NovaNamespace()},
		Spec: esov1.ExternalSecretSpec{
			RefreshInterval: &metav1.Duration{Duration: 0},
			Target:          esov1.ExternalSecretTarget{Name: name, CreationPolicy: esov1.CreatePolicyOwner},
			DataFrom: []esov1.ExternalSecretDataFromRemoteRef{
				{
					SourceRef: &esov1.StoreGeneratorSourceRef{
						GeneratorRef: &esov1.GeneratorRef{
							APIVersion: "generators.external-secrets.io/v1alpha1",
							Kind:       "Password",
							Name:       name,
						},
					},
					Rewrite: []esov1.ExternalSecretRewrite{{
						Regexp: &esov1.ExternalSecretRewriteRegexp{
							Source: "^" + novaMetadataGeneratorKey + "$",
							Target: novaMetadataSecretKey,
						},
					}},
				},
			},
		},
	}
}

// usesGeneratedNovaMetadataSecret reports whether the reference the projected
// Nova child receives names the generated Secret: services.nova names no Secret
// of its own, or names the generated one (to adopt its value under a key of its
// choosing). Either way the generated pair is what the child reads, so it is
// kept, and the pair is only reaped once the ControlPlane names another Secret.
func usesGeneratedNovaMetadataSecret(cp *c5c3v1alpha1.ControlPlane) bool {
	ref := cp.Spec.Services.Nova.MetadataSharedSecretRef
	return ref == nil || ref.Name == novaMetadataSecretName(cp)
}

// effectiveNovaMetadataSharedSecretRef resolves the reference the projected Nova
// child receives: the Secret services.nova.metadataSharedSecretRef names when the
// ControlPlane supplies one, and otherwise the generated pair's own Secret. It is
// resolved at projection time rather than materialised into the spec, so removing
// a user-supplied reference reverts the child to the generated value instead of
// pinning the last one.
func effectiveNovaMetadataSharedSecretRef(cp *c5c3v1alpha1.ControlPlane) commonv1.SecretRefSpec {
	if ref := cp.Spec.Services.Nova.MetadataSharedSecretRef; ref != nil {
		return *ref
	}
	return commonv1.SecretRefSpec{Name: novaMetadataSecretName(cp), Key: novaMetadataSecretKey}
}

// reconcileNovaMetadataSecret ensures the generated metadata shared secret while
// the projected child reads it (usesGeneratedNovaMetadataSecret), and writes
// nothing once the ControlPlane names a Secret of its own. It never deletes the
// pair: the live metadata Deployment keeps sourcing its env from the generated
// Secret until the child has converged on the new reference, so the pair is
// reaped behind that convergence (reapGeneratedNovaMetadataSecret).
//
// halt reports that the caller must stop its pass and return the result and
// error unchanged; the condition is already set. A false halt means the
// reference effectiveNovaMetadataSharedSecretRef resolves is in place, or on its
// way.
//
// It does NOT wait for the materialised Secret. The nova child gates on it
// itself (WaitingForMetadataSharedSecret), which is the same wait one requeue
// later and keeps the two legs from both parking the plane on one Secret.
//
// The objects ride the cluster of the Nova namespace, which is where the ESO
// that has to run the generator and the child that consumes its output live.
func (r *ControlPlaneReconciler) reconcileNovaMetadataSecret(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (ctrl.Result, bool, error) {
	if !usesGeneratedNovaMetadataSecret(cp) {
		return ctrl.Result{}, false, nil
	}

	children, err := r.childrenClientFor(ctx, cp, cp.NovaNamespace())
	if err != nil {
		conditionFailer(cp, conditionTypeNovaReady)(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	fail := func(err error) (ctrl.Result, bool, error) {
		conditionFailer(cp, conditionTypeNovaReady)("NovaMetadataSecretError", err.Error())
		return ctrl.Result{}, true, err
	}

	// The generator before the ExternalSecret that draws from it, so the source
	// exists when ESO resolves the reference.
	if err := r.ensureUnownedOrOwned(ctx, children, cp, novaMetadataPasswordGenerator(cp)); err != nil {
		return fail(fmt.Errorf("ensuring the Nova metadata Password generator: %w", err))
	}
	if err := r.ensureUnownedOrOwned(ctx, children, cp, novaMetadataExternalSecret(cp)); err != nil {
		return fail(fmt.Errorf("ensuring the Nova metadata ExternalSecret: %w", err))
	}
	return ctrl.Result{}, false, nil
}

// reapGeneratedNovaMetadataSecret takes the generated pair down once the
// ControlPlane names a Secret of its own. A live Password generator behind an
// ExternalSecret nothing references is a second shared value in the namespace,
// and the next operator to read the Secret set cannot tell which of the two the
// agents actually sign with.
//
// The caller runs it only after the projected child has converged on the
// supplied reference: the Secret goes with the ExternalSecret (CreationPolicy:
// Owner), and the live metadata Deployment sources its env from it through a
// non-optional secretKeyRef until the nova operator has re-rendered it, so a
// reap ahead of that leaves every metadata pod created in the window on
// CreateContainerConfigError.
//
// halt carries the same contract as reconcileNovaMetadataSecret's.
func (r *ControlPlaneReconciler) reapGeneratedNovaMetadataSecret(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (ctrl.Result, bool, error) {
	if usesGeneratedNovaMetadataSecret(cp) {
		return ctrl.Result{}, false, nil
	}
	children, err := r.childrenClientFor(ctx, cp, cp.NovaNamespace())
	if err != nil {
		conditionFailer(cp, conditionTypeNovaReady)(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}
	if err := r.deleteGeneratedNovaMetadataSecret(ctx, children, cp); err != nil {
		conditionFailer(cp, conditionTypeNovaReady)("NovaMetadataSecretError", err.Error())
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{}, false, nil
}

// deleteGeneratedNovaMetadataSecret removes the generated pair, consumer first,
// so the ExternalSecret never outlives the generator it reads. Each delete is
// gated on this ControlPlane still owning the object and tolerates an absent
// one, so a ControlPlane that never generated a value converges rather than
// failing on the first missing object, and a same-named object somebody else
// wrote into a shared service namespace is left alone.
//
// The Password is read through the uncached teardown reader. Nothing watches the
// kind, so a cached read would start an informer over every Password on c's
// cluster just for this one existence check, and on a target cluster whose
// credentials cannot list the kind that informer never syncs and the read blocks
// the reconcile worker for good.
//
// The materialised Secret is not deleted here. It carries CreationPolicy: Owner,
// so ESO's own owner reference on it takes it down with the ExternalSecret.
func (r *ControlPlaneReconciler) deleteGeneratedNovaMetadataSecret(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) error {
	key := client.ObjectKey{Name: novaMetadataSecretName(cp), Namespace: cp.NovaNamespace()}
	es := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	if err := commonreconcile.DeleteOrphanedChildFunc(ctx, c, es, func(live client.Object) bool {
		return isControlPlaneChild(live, cp)
	}); err != nil {
		return fmt.Errorf("deleting the generated Nova metadata shared secret: %w", err)
	}

	gen := &esgenv1alpha1.Password{}
	switch err := r.teardownReader(c).Get(ctx, key, gen); {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return nil
	case apierrors.IsForbidden(err):
		// The generator is only ever written through c, so credentials that cannot
		// read the kind never wrote one. That is a target cluster whose access
		// release predates the passwords grant, and a ControlPlane that names its
		// own Secret there has no generator to reap; failing here would hold
		// NovaReady False on every pass for an object that cannot exist.
		return nil
	case err != nil:
		return fmt.Errorf("reading the generated Nova metadata Password generator %s: %w", key, err)
	case !isControlPlaneChild(gen, cp):
		return nil
	}
	if err := client.IgnoreNotFound(c.Delete(ctx, gen)); err != nil {
		return fmt.Errorf("deleting the generated Nova metadata Password generator %s: %w", key, err)
	}
	return nil
}
