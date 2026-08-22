// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// This file is the ONE projection layer both registration mechanisms run on:
// the ControlPlane's own catalog rows and credential delivery
// (reconcile_catalog.go, reconcile_admincredential.go, builtin_registrations.go)
// and the KeystoneService controller's per-CR registrations
// (keystoneservice_account.go, keystoneservice_catalog.go). Only the hardened
// flows live here — the collision probe gate, the generation-scoped password
// engine, the OpenBao publish leg, the ESO nudges, and the K-ORC child spec
// builders. Each mechanism keeps its own orchestration skeleton, its own
// reporting model, and its own condition message texts, and parameterizes the
// differences that are deliberate (the two push-hash annotation keys, the two
// OpenBao path layouts, the two ownership strategies).
//
// It lives in the controller package rather than in internal/common because it
// reads c5c3 API types and package-local helpers (childNamespace,
// effectiveControlPlaneStoreRef, korcAuthURL, buildServiceAccountCloudsYAML),
// and both reconcilers already live here, so nothing needs exporting.

// --- shared vocabulary ---

// serviceAccountPasswordGenerationAnnotation stamps the current password
// generation N onto the managed K-ORC User CR. The KeystoneService controller
// derives N from the User's passwordRef suffix and the annotation is the
// rotation nudge marker: the CredentialRotation reconciler CLEARS it to "" to
// request a rotation (mirroring adminPasswordHashAnnotation), and an empty value
// drives a generation bump on the next pass.
const serviceAccountPasswordGenerationAnnotation = "cobaltcore.c5c3.io/password-generation" //nolint:gosec // G101 false positive: annotation key, not a credential.

// serviceAccountPasswordKey is the Secret data key the generated password is
// stored under. K-ORC's passwordRef reads exactly this key; it is also the key
// the materialized consumer Secret carries.
const serviceAccountPasswordKey = "password"

// serviceAccountRoleSlugNonAlnum matches every maximal run of characters outside
// [a-z0-9], which serviceAccountRoleSlug collapses to a single "-".
var serviceAccountRoleSlugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// serviceAccountRoleSlug derives a deterministic, name-safe discriminator for a
// role to embed in the Role import / RoleAssignment child CR names. It lowercases
// the role, collapses every run of characters outside [a-z0-9] to a single "-",
// trims leading/trailing "-", truncates the readable base to 16 chars, and appends
// "-" plus the first 8 hex chars of sha256(role). The hash is taken over the
// ORIGINAL role string (before normalization): Keystone role names are
// case-sensitive and up to 255 chars, so hashing the raw value keeps two roles
// that normalize alike ("Member" vs "member", or two long names sharing a 16-char
// prefix) alias-free. The result is at most 25 bytes.
func serviceAccountRoleSlug(role string) string {
	sum := sha256.Sum256([]byte(role))
	suffix := hex.EncodeToString(sum[:])[:8]
	base := strings.Trim(serviceAccountRoleSlugNonAlnum.ReplaceAllString(strings.ToLower(role), "-"), "-")
	if len(base) > 16 {
		base = base[:16]
	}
	return base + "-" + suffix
}

// parseServiceAccountGeneration extracts the generation N from a password Secret
// name of the form "…-password-vN". ok is false when the name carries no such
// suffix or N is not a positive integer.
func parseServiceAccountGeneration(passwordRefName string) (int64, bool) {
	i := strings.LastIndex(passwordRefName, "-password-v")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(passwordRefName[i+len("-password-v"):], 10, 64)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// probeVerdict is the interpretation of a collision probe import.
type probeVerdict int

const (
	// probePending — the probe has not resolved either way yet.
	probePending probeVerdict = iota
	// probeResolved — the probe matched an existing OpenStack resource (collision).
	probeResolved
	// probeAbsent — the probe reports the resource does not exist (safe to create).
	probeAbsent
)

func interpretProbe(obj orcv1alpha1.ObjectWithConditions) probeVerdict {
	switch {
	case korcAvailableUpToDate(obj):
		return probeResolved
	case korcImportPendingExternal(obj):
		return probeAbsent
	default:
		return probePending
	}
}

// pendingServiceAccountObjs returns the not-yet-resolved objects among the given
// ones, for the External-mode classifier.
func pendingServiceAccountObjs(objs ...orcv1alpha1.ObjectWithConditions) []orcv1alpha1.ObjectWithConditions {
	var pending []orcv1alpha1.ObjectWithConditions
	for _, obj := range objs {
		if obj != nil && !korcAvailableUpToDate(obj) {
			pending = append(pending, obj)
		}
	}
	return pending
}

// registrationEnsure applies a projected child, and registrationClaim marks one
// the caller builds itself. They are the seam ownership enters through: a
// KeystoneService projects into the ControlPlane's namespace from another one,
// where an owner reference is illegal and ownership is carried by labels
// instead.
type (
	registrationEnsure func(ctx context.Context, obj client.Object) error
	registrationClaim  func(obj client.Object) error
)

// deleteRegistrationChild issues an idempotent Delete on one named child in
// namespace, tolerating NotFound. It drops resolved collision probes and
// superseded password Secrets — neither of which either mechanism's prune sweep
// may touch while the block that owns them is declared.
func deleteRegistrationChild(ctx context.Context, c client.Client, obj client.Object, name, namespace string) error {
	obj.SetName(name)
	obj.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, obj)); err != nil {
		return fmt.Errorf("deleting registration child %q: %w", name, err)
	}
	return nil
}

// --- ESO nudges ---

// repushPushSecret nudges ESO to re-push the named PushSecret's source Secret to
// OpenBao by stamping the given annotation on it. ESO's PushSecret controller
// re-pushes only when the PushSecret object's own metadata hash changes — it does
// not watch the referenced Secret — so without this stamp a source-Secret update
// (a rotated service-account password, a newly projected CA bundle, the
// bootstrap-to-minted credential handoff) would not reach OpenBao until the
// hourly refreshInterval. Each caller keys its own annotation by the content it
// owns, so an unchanged value writes nothing and a steady-state pass is a no-op.
//
// The read-modify-write runs under RetryOnConflict: ESO mutates the PushSecret's
// status on every push, so a 409 between the Get and the Update is expected
// concurrency, not a fault. A missing PushSecret is a nil no-op — the freshness
// gate is the caller's byte-compare, not this nudge.
func repushPushSecret(ctx context.Context, c client.Client, namespace, name, annotation, hash string) error {
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ps := &esov1alpha1.PushSecret{}
		if err := c.Get(ctx, key, ps); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if ps.Annotations[annotation] == hash {
			return nil
		}
		if ps.Annotations == nil {
			ps.Annotations = map[string]string{}
		}
		ps.Annotations[annotation] = hash
		return c.Update(ctx, ps)
	}); err != nil {
		return fmt.Errorf("forcing PushSecret %q re-push: %w", name, err)
	}
	return nil
}

// resyncExternalSecret nudges ESO to re-materialize the consumer Secret now
// rather than at the next hourly refresh, by stamping the
// external-secrets.io/force-sync annotation with the given trigger (typically a
// content hash combined with the source PushSecret's syncedResourceVersion — see
// the admin-credential call site in reconcileAdminCredential for why the
// completed-push marker must be part of it). ESO folds the ExternalSecret's
// annotations into its sync-decision hash, so a changed trigger forces a re-sync
// and an unchanged one writes nothing.
//
// Like repushPushSecret it retries on conflict — ESO mutates this object's status
// and its own annotations on every refresh — and treats a missing ExternalSecret
// as a nil no-op: the sub-reconciler that owns the ExternalSecret's creation, and
// the byte-compare gate at the call site, are what guarantee the materialized
// value is fresh before the owning condition flips True.
func resyncExternalSecret(ctx context.Context, c client.Client, namespace, name, trigger string) error {
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		es := &esov1.ExternalSecret{}
		if err := c.Get(ctx, key, es); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if es.Annotations[esov1.AnnotationForceSync] == trigger {
			return nil
		}
		if es.Annotations == nil {
			es.Annotations = map[string]string{}
		}
		es.Annotations[esov1.AnnotationForceSync] = trigger
		return c.Update(ctx, es)
	}); err != nil {
		return fmt.Errorf("forcing ExternalSecret %q re-sync: %w", name, err)
	}
	return nil
}

// --- K-ORC child spec builders ---
//
// Every builder is a pure projection: it takes the child's name, its namespace,
// the credential the child authenticates with, and the identity it stands for,
// and returns the fully populated typed object. Both mechanisms' naming helpers
// stay where they are, so the caller decides WHICH child is built and the builder
// decides WHAT it looks like — the shape that must not drift between them.
//
// The unmanaged* builders are imports: they REFERENCE an OpenStack resource and
// never create or delete it, which is also what makes them safe as collision
// probes. The managed* builders declare a Resource K-ORC creates and, at
// teardown, deletes.

// unmanagedDomainImport builds the Domain import an account's user and project
// resolve their domain against, filtered on the domain name alone.
func unmanagedDomainImport(name, namespace, domainName string, credRef orcv1alpha1.CloudCredentialsReference) *orcv1alpha1.Domain {
	return &orcv1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: orcv1alpha1.DomainSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			Import: &orcv1alpha1.DomainImport{
				Filter: &orcv1alpha1.DomainFilter{Name: ptr.To(orcv1alpha1.KeystoneName(domainName))},
			},
		},
	}
}

// unmanagedProjectImport builds the Project import filtered on the project name
// within a domain. It serves both the referenced project (project.create=false)
// and the collision probe that gates creating a managed one.
func unmanagedProjectImport(name, namespace, projectName, domainRef string, credRef orcv1alpha1.CloudCredentialsReference) *orcv1alpha1.Project {
	return &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: orcv1alpha1.ProjectSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			Import: &orcv1alpha1.ProjectImport{
				Filter: &orcv1alpha1.ProjectFilter{
					Name:      ptr.To(orcv1alpha1.KeystoneName(projectName)),
					DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(domainRef)),
				},
			},
		},
	}
}

// managedProjectChild builds the managed Project the operator creates once the
// collision probe reports no project of that name exists in the domain.
func managedProjectChild(name, namespace, projectName, domainRef string, credRef orcv1alpha1.CloudCredentialsReference) *orcv1alpha1.Project {
	return &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: orcv1alpha1.ProjectSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: credRef,
			Resource: &orcv1alpha1.ProjectResourceSpec{
				Name:      ptr.To(orcv1alpha1.KeystoneName(projectName)),
				DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(domainRef)),
			},
		},
	}
}

// unmanagedUserImport builds the short-lived User import that decides
// exists/absent before a managed User is created. The filter carries the domain,
// so it answers the question K-ORC's own adoption asks.
func unmanagedUserImport(name, namespace, userName, domainRef string, credRef orcv1alpha1.CloudCredentialsReference) *orcv1alpha1.User {
	return &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			Import: &orcv1alpha1.UserImport{
				Filter: &orcv1alpha1.UserFilter{
					Name:      ptr.To(orcv1alpha1.OpenStackName(userName)),
					DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(domainRef)),
				},
			},
		},
	}
}

// unmanagedRoleImport builds the Role import for one declared role. It carries NO
// DomainRef: Keystone roles are global by default, and reading a role never
// mutates it, so the import rides the spec's own clouds.yaml.
func unmanagedRoleImport(name, namespace, role string, credRef orcv1alpha1.CloudCredentialsReference) *orcv1alpha1.Role {
	return &orcv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: orcv1alpha1.RoleSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			Import: &orcv1alpha1.RoleImport{
				Filter: &orcv1alpha1.RoleFilter{Name: ptr.To(orcv1alpha1.KeystoneName(role))},
			},
		},
	}
}

// managedRoleAssignmentChild builds the managed RoleAssignment binding one role to
// an account's user on its project. It authenticates via the admin PASSWORD cloud,
// not the spec's application credential, so a teardown Delete survives the
// credential's own revoke.
func managedRoleAssignmentChild(
	name, namespace, roleRef, userRef, projectRef string, credRef orcv1alpha1.CloudCredentialsReference,
) *orcv1alpha1.RoleAssignment {
	return &orcv1alpha1.RoleAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: orcv1alpha1.RoleAssignmentSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: credRef,
			Resource: &orcv1alpha1.RoleAssignmentResourceSpec{
				RoleRef:    orcv1alpha1.KubernetesNameRef(roleRef),
				UserRef:    ptr.To(orcv1alpha1.KubernetesNameRef(userRef)),
				ProjectRef: ptr.To(orcv1alpha1.KubernetesNameRef(projectRef)),
			},
		},
	}
}

// unmanagedServiceImport builds the catalog collision probe. The filter carries
// the type AND the effective name, the exact pair K-ORC's service actuator
// matches an existing row on.
func unmanagedServiceImport(name, namespace, serviceType, serviceName string, credRef orcv1alpha1.CloudCredentialsReference) *orcv1alpha1.Service {
	return &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: orcv1alpha1.ServiceSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			Import: &orcv1alpha1.ServiceImport{
				Filter: &orcv1alpha1.ServiceFilter{
					Type: ptr.To(serviceType),
					Name: ptr.To(orcv1alpha1.OpenStackName(serviceName)),
				},
			},
		},
	}
}

// managedCatalogServiceChild builds the managed Service CR for one catalog row.
func managedCatalogServiceChild(name, namespace, serviceType, serviceName string, credRef orcv1alpha1.CloudCredentialsReference) *orcv1alpha1.Service {
	return &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: orcv1alpha1.ServiceSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: credRef,
			Resource: &orcv1alpha1.ServiceResourceSpec{
				Type:    serviceType,
				Name:    ptr.To(orcv1alpha1.OpenStackName(serviceName)),
				Enabled: ptr.To(true),
			},
		},
	}
}

// managedCatalogEndpointChild builds the managed Endpoint CR for one interface of
// a catalog row, pointing at that row's Service CR.
func managedCatalogEndpointChild(
	name, namespace, iface, url, serviceRef string, credRef orcv1alpha1.CloudCredentialsReference,
) *orcv1alpha1.Endpoint {
	return &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: orcv1alpha1.EndpointSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: credRef,
			Resource: &orcv1alpha1.EndpointResourceSpec{
				Interface:  iface,
				URL:        url,
				ServiceRef: orcv1alpha1.KubernetesNameRef(serviceRef),
				Enabled:    ptr.To(true),
			},
		},
	}
}

// generatedPasswordMutator fills the K-ORC-facing password key on a
// generation-scoped Secret, once. A new generation is a NEW Secret name, never an
// in-place edit — that name flip is the only thing K-ORC's user actuator reacts
// to — so a value already present is preserved.
func generatedPasswordMutator(secret *corev1.Secret) error {
	if len(secret.Data[serviceAccountPasswordKey]) > 0 {
		return nil
	}
	v, err := generateAppCredSecretValue()
	if err != nil {
		return err
	}
	secret.Data[serviceAccountPasswordKey] = []byte(v)
	return nil
}

// accountPushSecretSpec builds the PushSecret mirroring an account's assembled
// source Secret to its per-CR OpenBao path. DeletionPolicy Delete: the credential
// dies with the account that owns it.
func accountPushSecretSpec(name, namespace, sourceName, remoteKey string, storeRef commonv1.SecretStoreRefSpec) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: esov1alpha1.PushSecretSpec{
			DeletionPolicy:  esov1alpha1.PushSecretDeletionPolicyDelete,
			SecretStoreRefs: secrets.PushSecretStoreRefs(storeRef),
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{Name: sourceName},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{RemoteKey: remoteKey},
				},
			}},
		},
	}
}

// accountExternalSecretSpec builds the ExternalSecret that materializes an
// account's consumer Secret from its per-CR OpenBao path. It reads back the
// "password" and "clouds.yaml" properties — the documented consumption contract —
// and refreshes hourly. CreationPolicy Owner ties the materialized Secret's life
// to the ExternalSecret, so the delete sweeps never have to name it.
func accountExternalSecretSpec(name, namespace, remoteKey string, storeRef commonv1.SecretStoreRefSpec) *esov1.ExternalSecret {
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: esov1.ExternalSecretSpec{
			RefreshInterval: &metav1.Duration{Duration: time.Hour},
			SecretStoreRef:  secrets.ESOSecretStoreRef(storeRef),
			Target:          esov1.ExternalSecretTarget{Name: name, CreationPolicy: esov1.CreatePolicyOwner},
			Data: []esov1.ExternalSecretData{
				{
					SecretKey: serviceAccountPasswordKey,
					RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: serviceAccountPasswordKey},
				},
				{
					SecretKey: appCredCloudsYAMLKey,
					RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: appCredCloudsYAMLKey},
				},
			},
		},
	}
}

// --- collision probe gate ---

// probeChild is a K-ORC typed object usable both as a Kubernetes object and as a
// K-ORC status surface. Every K-ORC kind satisfies it, which is what lets one gate
// serve the User, Project and Service probes.
type probeChild interface {
	client.Object
	orcv1alpha1.ObjectWithConditions
}

// managedChildProbeInput describes one collision gate. Both mechanisms build the
// managed child's identity themselves — the gate never derives a name — so this
// carries only what the gate reads.
type managedChildProbeInput struct {
	// kind is the K-ORC kind the two error texts name ("User", "Project",
	// "Service"). It is passed rather than derived from the scheme so a failure
	// here can never be a lookup error with no sensible fallback.
	kind string
	// managed is an EMPTY typed object of that kind; the live Get reads into it.
	managed     client.Object
	managedName string
	namespace   string
	// adopt short-circuits the probe: the caller consented to taking over a
	// pre-existing OpenStack resource. Only the user and catalog gates set it;
	// project takeover is expressed as project.create=false, never as adopt.
	adopt bool
	// probe is the fully built unmanaged import, applied and then interpreted.
	probe probeChild
	// dropProbeOnOwned deletes a leftover probe on the already-owned path. It is
	// true for the user and catalog gates and false for the project gates,
	// matching what each kind does today.
	dropProbeOnOwned bool
	// ensure applies the probe.
	ensure registrationEnsure
}

// managedChildProbeGate reports whether a managed child may be created without
// silently adopting a pre-existing OpenStack resource.
//
// K-ORC's managed create matches an existing resource and takes it over rather
// than failing, so the operator decides first: it creates nothing while it owns
// no managed child, and instead applies a short-lived UNMANAGED import that
// answers exists/absent. proceed is true when the operator already owns the
// managed child, adopt was requested, or the probe reports the resource absent;
// the probe is deleted on adopt, on absent, and — only when dropProbeOnOwned — on
// the owned path.
//
// verdict carries the interpretation for the two proceed=false cases, so each
// caller renders its own collision and probing messages. A Kubernetes read error
// on the managed child, or a failed probe apply, is returned as an error and
// never collapsed into a verdict: a gate that cannot see is not a gate that says
// "absent".
func managedChildProbeGate(
	ctx context.Context, c client.Client, in managedChildProbeInput,
) (bool, probeVerdict, error) {
	dropProbe := func() error {
		return deleteRegistrationChild(ctx, c, in.probe, in.probe.GetName(), in.namespace)
	}

	switch err := c.Get(ctx, types.NamespacedName{Name: in.managedName, Namespace: in.namespace}, in.managed); {
	case err == nil:
		// Already ours: the verdict is settled, whatever is in Keystone.
		if in.dropProbeOnOwned {
			return true, probeAbsent, dropProbe()
		}
		return true, probeAbsent, nil
	case apierrors.IsNotFound(err):
	default:
		return false, probePending, fmt.Errorf("reading managed %s %q: %w", in.kind, in.managedName, err)
	}

	if in.adopt {
		return true, probeAbsent, dropProbe()
	}

	if err := in.ensure(ctx, in.probe); err != nil {
		return false, probePending, fmt.Errorf("registration %s probe %q: %w", in.kind, in.probe.GetName(), err)
	}
	switch verdict := interpretProbe(in.probe); verdict {
	case probeResolved, probePending:
		return false, verdict, nil
	case probeAbsent:
	}
	return true, probeAbsent, dropProbe()
}

// --- managed user, generation-scoped password and rotation ---

// managedAccountUserInput describes the managed User one account projects. The
// caller supplies every name (its own naming helpers produce them) and the two
// strategies that differ: how a generation-scoped password Secret is ensured, and
// what counts as a child this owner may delete.
type managedAccountUserInput struct {
	name, namespace       string // the managed User child CR
	userName              string // OpenStack user name
	domainRef, projectRef string // sibling child CR names
	managedCredRef        orcv1alpha1.CloudCredentialsReference
	passwordSecretNameFor func(gen int64) string
	ensurePasswordSecret  func(ctx context.Context, gen int64) error
	// passwordSecretPrefix guards the superseded-generation prune: only Secrets
	// named "<this account's prefix>password-v<N>" are candidates.
	passwordSecretPrefix string
	ownsChild            func(obj client.Object) bool
	// claim marks the managed User. The User is read-modify-written rather than
	// applied (the mutation reads live state), so the seam is a claim on the
	// object rather than a full apply.
	claim registrationClaim
}

// ensureManagedAccountUser create-or-updates the managed K-ORC User with the
// current-generation password, and flips the passwordRef to a fresh generation
// when the CredentialRotation reconciler has cleared the generation annotation.
// K-ORC's user actuator re-applies the password only when the passwordRef NAME
// changes, so a rotation is a Secret-name flip, never a content edit.
//
// It returns the applied User, the desired generation, whether the User was
// created this pass (the one-shot deferral event both callers emit), and the
// rotation timestamp — non-nil only when a nudge was consumed this pass.
//
// This projection stays read-modify-write (not Server-Side Apply): the mutate
// closure reads the LIVE User's CreationTimestamp and existing
// serviceAccountPasswordGenerationAnnotation to decide whether to (re-)stamp the
// generation, which cannot be expressed as a pure projection of the spec.
func ensureManagedAccountUser(
	ctx context.Context, c client.Client, in managedAccountUserInput,
) (*orcv1alpha1.User, int64, bool, *metav1.Time, error) {
	existing := &orcv1alpha1.User{}
	getErr := c.Get(ctx, types.NamespacedName{Name: in.name, Namespace: in.namespace}, existing)
	exists := getErr == nil
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return nil, 0, false, nil, fmt.Errorf("reading managed User %q: %w", in.name, getErr)
	}

	var currentGen int64
	if exists && existing.Spec.Resource != nil && existing.Spec.Resource.PasswordRef != nil {
		if g, ok := parseServiceAccountGeneration(string(*existing.Spec.Resource.PasswordRef)); ok {
			currentGen = g
		}
	}
	if currentGen < 1 {
		currentGen = 1
	}
	// The empty generation annotation is the CredentialRotation reconciler's
	// rotation nudge.
	rotating := false
	if exists {
		if v, present := existing.Annotations[serviceAccountPasswordGenerationAnnotation]; present && v == "" {
			rotating = true
		}
	}
	desiredGen := currentGen
	if !exists {
		desiredGen = 1
	} else if rotating {
		desiredGen = currentGen + 1
	}

	if err := in.ensurePasswordSecret(ctx, desiredGen); err != nil {
		return nil, 0, false, nil, err
	}
	passwordRefName := in.passwordSecretNameFor(desiredGen)

	user := &orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: in.name, Namespace: in.namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, c, user, func() error {
		user.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
		user.Spec.Import = nil
		user.Spec.CloudCredentialsRef = in.managedCredRef
		if user.Spec.Resource == nil {
			user.Spec.Resource = &orcv1alpha1.UserResourceSpec{}
		}
		user.Spec.Resource.Name = ptr.To(orcv1alpha1.OpenStackName(in.userName))
		user.Spec.Resource.DomainRef = ptr.To(orcv1alpha1.KubernetesNameRef(in.domainRef))
		user.Spec.Resource.DefaultProjectRef = ptr.To(orcv1alpha1.KubernetesNameRef(in.projectRef))
		user.Spec.Resource.PasswordRef = ptr.To(orcv1alpha1.KubernetesNameRef(passwordRefName))
		if user.Annotations == nil {
			user.Annotations = map[string]string{}
		}
		// Stamp the generation on a fresh create or a rotation. In the steady state
		// stamp only when the key is ABSENT (re-derive) — a present-but-empty value
		// is a rotation nudge this pass did not observe (the lost-rotation race);
		// leave it so the NEXT pass rotates. Mirrors shouldStampPasswordHash.
		if _, present := user.Annotations[serviceAccountPasswordGenerationAnnotation]; user.CreationTimestamp.IsZero() || rotating || !present {
			user.Annotations[serviceAccountPasswordGenerationAnnotation] = strconv.FormatInt(desiredGen, 10)
		}
		return in.claim(user)
	})
	if err != nil {
		return nil, 0, false, nil, fmt.Errorf("registration managed User %q: %w", in.name, err)
	}

	// Record the rotation timestamp and prune superseded password Secrets once
	// K-ORC confirms the current generation is applied.
	var rotatedAt *metav1.Time
	if rotating {
		now := metav1.Now()
		rotatedAt = &now
	}
	if user.Status.Resource != nil && user.Status.Resource.AppliedPasswordRef == passwordRefName {
		// List the account's password Secrets that ACTUALLY exist and delete only
		// the superseded generations, rather than blind-Deleting v1..v(desiredGen-1)
		// every steady-state pass. This runs on every reconcile once applied stays
		// true, so a blind loop would issue a growing, unbounded stream of NotFound
		// DELETE round-trips for long-gone generations that never converges.
		var pwSecrets corev1.SecretList
		if err := c.List(ctx, &pwSecrets, client.InNamespace(in.namespace)); err != nil {
			return nil, 0, false, nil, fmt.Errorf("listing superseded password Secrets: %w", err)
		}
		for i := range pwSecrets.Items {
			secretName := pwSecrets.Items[i].Name
			if g, ok := parseServiceAccountGeneration(secretName); ok &&
				strings.HasPrefix(secretName, in.passwordSecretPrefix) && g < desiredGen &&
				in.ownsChild(&pwSecrets.Items[i]) {
				if err := deleteRegistrationChild(ctx, c, &corev1.Secret{}, secretName, in.namespace); err != nil {
					return nil, 0, false, nil, err
				}
			}
		}
	}
	return user, desiredGen, op == controllerutil.OperationResultCreated, rotatedAt, nil
}

// --- role and domain legs ---

// ensureAccountDomainImport applies the unmanaged Domain import an account's user
// and project resolve their domain against. It is only reached when the effective
// domain is NOT the admin domain: that case reuses the admin import, and each
// caller keeps that early return because each resolves the admin handle its own
// way. The apply error is returned unwrapped, so the caller keeps naming the
// mechanism in its own words.
func ensureAccountDomainImport(
	ctx context.Context,
	name, namespace, domainName string, credRef orcv1alpha1.CloudCredentialsReference,
	ensure registrationEnsure,
) error {
	return ensure(ctx, unmanagedDomainImport(name, namespace, domainName, credRef))
}

// accountRoleOutcome is what one (account, role) pair resolved to. The caller
// renders every message from it, so the two mechanisms keep their own wording and
// their own reporting model.
type accountRoleOutcome struct {
	roleObj    *orcv1alpha1.Role
	assignment *orcv1alpha1.RoleAssignment
	// terminalErr is the first terminal K-ORC error, role import before
	// assignment: K-ORC has stopped retrying, so a bounded wait would never
	// resolve.
	terminalErr error
	// blocked is the first object that is not Available, in the same order, and
	// nil once both are ready. Naming the ROOT dependency keeps a wait from
	// pointing at an assignment that is merely queued behind its import.
	blocked orcv1alpha1.ObjectWithConditions
	// stalled reports that the role import has been unresolved past
	// externalImportStallGrace, which is when "the role may not exist in Keystone"
	// stops being noise and becomes the likely cause.
	stalled bool
}

// applyAccountRole applies one declared role in dependency order: an UNMANAGED
// Role import filtered by name (Keystone roles are global, and reading one never
// mutates it, so it rides the spec's own clouds.yaml) and a MANAGED RoleAssignment
// binding it to the account's user on its project. The assignment authenticates
// via the admin PASSWORD cloud, so a teardown Delete survives the application
// credential's revoke.
//
// Both names are deterministic per (account, role), so both applies are pure
// projections. Only the object construction, the apply and the inspection live
// here; the per-role loop, the precedence and every message stay in the caller.
func applyAccountRole(
	ctx context.Context,
	importName, assignmentName, namespace, role string,
	credRef, managedCredRef orcv1alpha1.CloudCredentialsReference,
	userRef, projectRef string, ensure registrationEnsure,
) (accountRoleOutcome, error) {
	var out accountRoleOutcome

	out.roleObj = unmanagedRoleImport(importName, namespace, role, credRef)
	if err := ensure(ctx, out.roleObj); err != nil {
		return out, fmt.Errorf("registration Role import %q: %w", importName, err)
	}

	out.assignment = managedRoleAssignmentChild(assignmentName, namespace, importName, userRef, projectRef, managedCredRef)
	if err := ensure(ctx, out.assignment); err != nil {
		return out, fmt.Errorf("registration RoleAssignment %q: %w", assignmentName, err)
	}

	for _, obj := range []orcv1alpha1.ObjectWithConditions{out.roleObj, out.assignment} {
		if termErr := orcv1alpha1.GetTerminalError(obj); termErr != nil {
			out.terminalErr = termErr
			break
		}
	}

	out.stalled = korcImportStalled(out.roleObj, externalImportStallGrace)
	switch {
	case !korcAvailableUpToDate(out.roleObj):
		out.blocked = out.roleObj
	case !korcAvailableUpToDate(out.assignment):
		out.blocked = out.assignment
	}
	return out, nil
}

// --- publish leg ---

// accountPublication describes one account's delivery: where its
// generation-scoped password is read from, what the assembled document says, and
// how each delivery object is written. The three ensure closures are the seam the
// two ownership strategies enter through — a controller reference at home versus
// the ownership labels a cross-namespace or cross-cluster child needs.
type accountPublication struct {
	management client.Client // reads the password Secret, always on the management cluster
	delivery   client.Client // writes the delivery objects, on the consumer's cluster
	// childNamespace is where the K-ORC-facing password Secret lives. It never
	// moves: K-ORC resolves the managed User's passwordRef in the User's own
	// namespace.
	childNamespace     string
	deliveryNamespace  string
	passwordSecretName string

	userName, projectName, domain string
	// deliveryRef resolves the auth URL for the cluster the document is READ on;
	// nil means the in-cluster one.
	deliveryRef *commonv1.TargetClusterRefSpec

	sourceSecretName, credentialsSecretName string
	pushSecret                              *esov1alpha1.PushSecret
	pushHashAnnotation                      string

	ensureSourceSecret   func(ctx context.Context, mutate func(*corev1.Secret) error) error
	ensurePushSecret     func(ctx context.Context, ps *esov1alpha1.PushSecret) error
	ensureExternalSecret func(ctx context.Context) error
}

// publishAccountCredentials assembles the source Secret, mirrors it to the per-CR
// OpenBao path, materializes the consumer Secret, and reports whether the
// materialized "password" matches the current generation — so a rotated-away
// password never reads ready. It runs only once K-ORC confirms the current
// password is applied to the user.
//
// Every input that is merely not there yet is a bounded wait, not an error: a
// missing password Secret, an empty password key, an unsynced PushSecret, and a
// not-yet-materialized consumer Secret all return (false, nil), because each
// resolves on a later pass without anything being fixed.
func publishAccountCredentials(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, pub accountPublication,
) (bool, error) {
	pwSecret := &corev1.Secret{}
	pwKey := types.NamespacedName{Name: pub.passwordSecretName, Namespace: pub.childNamespace}
	if err := pub.management.Get(ctx, pwKey, pwSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading registration password Secret: %w", err)
	}
	password := pwSecret.Data[serviceAccountPasswordKey]
	if len(password) == 0 {
		return false, nil
	}

	// The document is read on the cluster it is delivered to, so its auth_url is
	// resolved for that cluster: a consumer beside a placed service cannot reach
	// the management cluster's in-cluster Keystone Service DNS name.
	cloudsYAML := []byte(buildServiceAccountCloudsYAML(cp, pub.userName, pub.projectName, pub.domain, string(password), pub.deliveryRef))
	if err := pub.ensureSourceSecret(ctx, func(secret *corev1.Secret) error {
		secret.Data[serviceAccountPasswordKey] = password
		secret.Data["username"] = []byte(pub.userName)
		secret.Data["project_name"] = []byte(pub.projectName)
		secret.Data["user_domain_name"] = []byte(pub.domain)
		secret.Data["project_domain_name"] = []byte(pub.domain)
		secret.Data["auth_url"] = []byte(korcAuthURL(cp, pub.deliveryRef))
		secret.Data["region_name"] = []byte(korcRegion(cp))
		secret.Data[appCredCloudsYAMLKey] = cloudsYAML
		return nil
	}); err != nil {
		return false, fmt.Errorf("assembling registration source Secret %q in namespace %q: %w",
			pub.sourceSecretName, pub.deliveryNamespace, err)
	}

	sum := sha256.Sum256(cloudsYAML)
	contentHash := hex.EncodeToString(sum[:])

	if err := pub.ensurePushSecret(ctx, pub.pushSecret); err != nil {
		return false, fmt.Errorf("ensuring registration PushSecret: %w", err)
	}
	if err := repushPushSecret(ctx, pub.delivery, pub.deliveryNamespace, pub.pushSecret.Name,
		pub.pushHashAnnotation, contentHash); err != nil {
		return false, fmt.Errorf("forcing registration PushSecret re-push: %w", err)
	}
	pushed := &esov1alpha1.PushSecret{}
	pushedKey := types.NamespacedName{Name: pub.pushSecret.Name, Namespace: pub.deliveryNamespace}
	if err := pub.delivery.Get(ctx, pushedKey, pushed); err != nil {
		return false, fmt.Errorf("reading registration PushSecret: %w", err)
	}
	if !pushSecretReady(pushed) {
		return false, nil
	}

	if err := pub.ensureExternalSecret(ctx); err != nil {
		return false, fmt.Errorf("ensuring registration ExternalSecret: %w", err)
	}
	// The completed push is part of the trigger: without it a re-sync could
	// materialize the value that was in OpenBao BEFORE this generation's push.
	syncTrigger := contentHash + "/" + pushed.Status.SyncedResourceVersion
	if err := resyncExternalSecret(ctx, pub.delivery, pub.deliveryNamespace,
		pub.credentialsSecretName, syncTrigger); err != nil {
		return false, fmt.Errorf("forcing registration ExternalSecret re-sync: %w", err)
	}

	materialized := &corev1.Secret{}
	credKey := types.NamespacedName{Name: pub.credentialsSecretName, Namespace: pub.deliveryNamespace}
	if err := pub.delivery.Get(ctx, credKey, materialized); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading materialized registration Secret: %w", err)
	}
	return bytes.Equal(materialized.Data[serviceAccountPasswordKey], password), nil
}
