// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/apply"
	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// ServiceAccountsReady reasons. Like the other condition-reason blocks these are
// the single source of truth for the status contract; call sites MUST reference
// the constants so a rename is caught by the compiler.
const (
	// reasonNoServiceAccountsDeclared is the True reason when the spec declares no
	// service accounts. It is still True (not simply omitted) so the condition
	// schema is identical whether or not accounts are declared.
	reasonNoServiceAccountsDeclared = "NoServiceAccountsDeclared"
	// reasonServiceAccountsProvisioned is the True reason when every declared
	// account is fully provisioned.
	reasonServiceAccountsProvisioned = "ServiceAccountsProvisioned"
	// reasonWaitingForServiceAccountAdmin defers projection until the admin
	// credential is minted (K-ORC cannot talk to Keystone before then).
	reasonWaitingForServiceAccountAdmin = "WaitingForAdminCredential"
	// reasonServiceAccountStoreNotReady reports the OpenBao-backed
	// ClusterSecretStore is unavailable, so no password can round-trip.
	reasonServiceAccountStoreNotReady = "SecretStoreNotReady"
	// reasonProbingForCollision reports that a fail-loudly collision probe has not
	// yet resolved either way.
	reasonProbingForCollision = "ProbingForCollision"
	// reasonServiceAccountCollision reports the fail-loudly default: a declared
	// user or managed project already exists in Keystone and adopt was not set.
	reasonServiceAccountCollision = "ServiceAccountCollision"
	// reasonWaitingForServiceAccounts is the bounded wait while the K-ORC User /
	// Project / password round-trip converges.
	reasonWaitingForServiceAccounts = "WaitingForServiceAccounts"
	// reasonServiceAccountsFailed reports a terminal K-ORC failure on a
	// service-account child CR.
	reasonServiceAccountsFailed = "ServiceAccountsFailed"
	// reasonServiceAccountError reports a Kubernetes-level failure reconciling a
	// service-account child (not a K-ORC/OpenStack failure).
	reasonServiceAccountError = "ServiceAccountError"
)

// serviceAccountPasswordGenerationAnnotation stamps the current password
// generation N onto the managed K-ORC User CR. reconcileServiceAccounts derives
// N from the User's passwordRef suffix and the annotation is the rotation nudge
// marker: the CredentialRotation reconciler CLEARS it to "" to request a rotation
// (mirroring adminPasswordHashAnnotation), and an empty value drives a generation
// bump on the next pass.
const serviceAccountPasswordGenerationAnnotation = "forge.c5c3.io/password-generation" //nolint:gosec // G101 false positive: annotation key, not a credential.

// serviceAccountPasswordKey is the Secret data key the generated password is
// stored under. K-ORC's passwordRef reads exactly this key; it is also the key
// the materialized consumer Secret carries.
const serviceAccountPasswordKey = "password"

// serviceAccountPushContentHashAnnotation stamps the assembled clouds.yaml
// content hash onto a service-account PushSecret, forcing an immediate re-push on
// a rotation (ESO's PushSecret controller does not watch the source Secret).
const serviceAccountPushContentHashAnnotation = "c5c3.io/service-account-push-hash" //nolint:gosec // G101 false positive: annotation key, not a credential.

// --- naming helpers ---

// serviceAccountChildPrefix scopes the prune sweep: only CRs carrying this prefix
// AND controlled by this ControlPlane are candidates for removal, so the admin
// imports (and any foreign CR) can never be caught by it.
func serviceAccountChildPrefix(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-service-account-"
}

// serviceAccountUserName resolves the OpenStack user name for an entry (defaults
// to the account name). It mirrors the webhook's effectiveServiceAccountUserName.
func serviceAccountUserName(sa c5c3v1alpha1.ServiceAccountSpec) string {
	return cmp.Or(sa.UserName, sa.Name)
}

// serviceAccountDomainName resolves the OpenStack domain for an entry, defaulting
// to the effective admin domain. It mirrors the webhook's
// effectiveServiceAccountDomain.
func serviceAccountDomainName(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec) string {
	return cmp.Or(sa.DomainName, adminDomainName(cp))
}

// The child CR / Secret names embed the account NAME (a stable handle), never the
// mutable OpenStack identity, and each discriminator sits in a fixed position so
// one account's name can never alias another's CR.
func serviceAccountUserRef(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec) string {
	return serviceAccountChildPrefix(cp) + "user-" + sa.Name
}

func serviceAccountUserProbeRef(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec) string {
	return serviceAccountChildPrefix(cp) + "user-probe-" + sa.Name
}

func serviceAccountProjectRef(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec) string {
	return serviceAccountChildPrefix(cp) + "project-" + sa.Name
}

func serviceAccountProjectProbeRef(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec) string {
	return serviceAccountChildPrefix(cp) + "project-probe-" + sa.Name
}

// serviceAccountDomainRef returns the Domain import CR the account's user/project
// resolve their domain against. When the effective domain equals the admin domain
// it REUSES the admin Domain import (reconcileKORC already created it before this
// sub-reconciler runs); otherwise a per-account unmanaged Domain import is used.
func serviceAccountDomainRef(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec) string {
	if serviceAccountDomainName(cp, sa) == adminDomainName(cp) {
		return adminDomainRef(cp)
	}
	return serviceAccountChildPrefix(cp) + "domain-" + sa.Name
}

func serviceAccountPasswordSecretName(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec, gen int64) string {
	return fmt.Sprintf("%s%s-password-v%d", serviceAccountChildPrefix(cp), sa.Name, gen)
}

func serviceAccountSourceSecretName(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec) string {
	return serviceAccountChildPrefix(cp) + sa.Name + "-source"
}

func serviceAccountPushSecretName(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec) string {
	return serviceAccountChildPrefix(cp) + sa.Name + "-backup"
}

func serviceAccountCredentialsSecretName(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec) string {
	return serviceAccountChildPrefix(cp) + sa.Name + "-credentials"
}

// serviceAccountRemoteKeyFor returns the per-CR, namespace-scoped OpenBao path a
// service account's credentials are mirrored to, following the established
// per-CR + namespace-scoped convention (cf. adminAppCredentialRemoteKeyFor).
func serviceAccountRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane, name string) string {
	return "openstack/keystone/" + cp.Namespace + "/" + cp.Name + "/service-accounts/" + name
}

// ownsServiceAccountChild reports whether obj is a CR this ControlPlane created
// for a declared service account. BOTH the controller reference and the
// "-service-account-" name prefix must match, so the admin imports (and any
// foreign CR sharing the namespace) can never be caught by the prune or teardown
// sweep. Shared by pruneServiceAccounts and the deletion sweep.
func (r *ControlPlaneReconciler) ownsServiceAccountChild(cp *c5c3v1alpha1.ControlPlane, obj client.Object) bool {
	return metav1.IsControlledBy(obj, cp) && strings.HasPrefix(obj.GetName(), serviceAccountChildPrefix(cp))
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

// serviceAccountState carries one account's ensure outcome: the projected status,
// whether it is fully ready, and — if not — the single blocking condition, in
// precedence order (collision, terminal, probing, bounded wait). pendingObjs are
// the not-yet-resolved K-ORC objects the External-mode classifier inspects.
type serviceAccountState struct {
	status      c5c3v1alpha1.ServiceAccountStatus
	created     bool // the managed User was created this pass (one-shot deferral events)
	roles       bool // roles were declared (deferred)
	scheduled   bool // rotation.mode Scheduled (deferred)
	ready       bool
	collision   string
	terminalMsg string
	probingMsg  string
	waitMsg     string
	pendingObjs []orcv1alpha1.ObjectWithConditions
}

// reconcileServiceAccounts projects spec.korc.serviceAccounts onto managed K-ORC
// User / Project CRs with operator-generated, OpenBao-backed, rotatable passwords,
// driving the ServiceAccountsReady condition. It is mode-independent: the same
// declaration works against a managed in-cluster Keystone and an external one.
//
// It is GATED on AdminCredentialReady (the admin credential must be minted before
// K-ORC can talk to Keystone) and on the OpenBao-backed ClusterSecretStore. The
// fail-loudly collision posture is operator-implemented: K-ORC's managed create
// SILENTLY adopts a same-name resource, so a short-lived unmanaged probe import
// decides exists/absent before any managed User/Project is created; a hit fails
// loud unless the entry opts into adoption.
func (r *ControlPlaneReconciler) reconcileServiceAccounts(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fail := conditionFailer(cp, conditionTypeServiceAccountsReady)
	sas := cp.Spec.KORC.ServiceAccounts

	// Gate on AdminCredentialReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeAdminCredentialReady) {
		logger.Info("AdminCredential not ready, deferring service-account projection")
		fail(reasonWaitingForServiceAccountAdmin, "AdminCredentialReady is not True; service-account projection deferred")
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Gate on the store the ControlPlane selected via spec.secretStoreRef
	// (default: the operator-provisioned per-tenant store) so an ESO/OpenBao
	// outage surfaces promptly rather than at the next hourly refresh (#476). A
	// namespaced store is resolved in the child namespace where the
	// service-account Secrets are materialised.
	storeRef := effectiveControlPlaneStoreRef(cp)
	storeReady, err := secrets.IsStoreRefReady(ctx, r.Client, storeRef, childNamespace(cp))
	if err != nil {
		return ctrl.Result{}, err
	}
	if !storeReady {
		logger.Info("secret store not ready, deferring service-account round-trip")
		fail(reasonServiceAccountStoreNotReady, fmt.Sprintf(
			"%s %q is not ready; upstream secret backend unreachable", storeRef.Kind, storeRef.Name,
		))
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	secretName := cmp.Or(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName, korcCloudsYamlSecretName)
	credRef := orcv1alpha1.CloudCredentialsReference{
		SecretName: secretName,
		CloudName:  cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName,
	}
	// Managed User/Project authenticate via the admin PASSWORD cloud, not the
	// spec's app-credential clouds.yaml, so a teardown Delete survives the AC
	// revoke (the same rationale as the opt-in catalog entries).
	managedCredRef := entryCredentialsRef(cp, credRef)

	// Preserve the previously-recorded per-account status so LastPasswordRotation
	// survives a steady-state pass (it is only refreshed on an actual rotation).
	prior := make(map[string]c5c3v1alpha1.ServiceAccountStatus, len(cp.Status.ServiceAccounts))
	for _, s := range cp.Status.ServiceAccounts {
		prior[s.Name] = s
	}

	states := make([]serviceAccountState, 0, len(sas))
	for i := range sas {
		st, err := r.ensureServiceAccount(ctx, cp, sas[i], credRef, managedCredRef, prior[sas[i].Name])
		if err != nil {
			fail(reasonServiceAccountError, fmt.Sprintf("reconciling service account %q: %v", sas[i].Name, err))
			return ctrl.Result{}, err
		}
		states = append(states, st)
	}

	// Prune undeclared children (always — even with zero declared accounts a
	// previously-declared account must be swept).
	pruning, err := r.pruneServiceAccounts(ctx, cp, sas)
	if err != nil {
		fail(reasonServiceAccountError, fmt.Sprintf("pruning undeclared service accounts: %v", err))
		return ctrl.Result{}, err
	}

	// Project status before any early return.
	statuses := make([]c5c3v1alpha1.ServiceAccountStatus, 0, len(states))
	for _, st := range states {
		statuses = append(statuses, st.status)
	}
	cp.Status.ServiceAccounts = statuses

	// One-shot deferral events, on the account-creation pass only.
	for i := range states {
		if !states[i].created {
			continue
		}
		if states[i].roles {
			r.Recorder.Event(cp, "Normal", "RoleAssignmentsDeferred", fmt.Sprintf(
				"roles declared on service account %q are accepted but not yet projected: K-ORC ships no "+
					"RoleAssignment kind; role assignments follow as a fast-follow", sas[i].Name,
			))
		}
		if states[i].scheduled {
			r.Recorder.Event(cp, "Normal", "ScheduledRotationDeferred", fmt.Sprintf(
				"rotation.mode Scheduled on service account %q is accepted but not yet implemented; rotate on "+
					"demand via a CredentialRotation CR", sas[i].Name,
			))
		}
	}

	// Failure precedence, most specific cause first.
	// 1. A classifiable External-mode K-ORC message on any pending object.
	if cp.IsExternalKeystone() {
		var pending []orcv1alpha1.ObjectWithConditions
		for _, st := range states {
			pending = append(pending, st.pendingObjs...)
		}
		if reason, raw := classifyExternalKORCFailure(pending...); reason != "" {
			message := fmt.Sprintf("external Keystone at %s: %s", externalKeystoneAuthURL(cp), raw)
			if reason == conditionReasonCatalogEndpointMismatch {
				message += "; " + catalogEndpointMismatchHint(cp)
			}
			fail(reason, message)
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
	}
	// 2. A fail-loudly collision.
	for _, st := range states {
		if st.collision != "" {
			fail(reasonServiceAccountCollision, st.collision)
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
	}
	// 3. A terminal K-ORC error.
	for _, st := range states {
		if st.terminalMsg != "" {
			fail(reasonServiceAccountsFailed, st.terminalMsg)
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
	}
	// 4. A collision probe still resolving.
	for _, st := range states {
		if st.probingMsg != "" {
			fail(reasonProbingForCollision, st.probingMsg)
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
	}
	// 5. Bounded waits.
	for _, st := range states {
		if !st.ready {
			fail(reasonWaitingForServiceAccounts, st.waitMsg)
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
	}
	// 6. Undeclared children still being removed.
	if len(pruning) > 0 {
		fail(reasonWaitingForServiceAccounts, fmt.Sprintf(
			"%d undeclared service-account child CR(s) are still being removed", len(pruning),
		))
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	if len(sas) == 0 {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeServiceAccountsReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             reasonNoServiceAccountsDeclared,
			Message:            "no service accounts are declared",
		})
		return ctrl.Result{}, nil
	}
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeServiceAccountsReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             reasonServiceAccountsProvisioned,
		Message:            fmt.Sprintf("%d service account(s) provisioned and materialized", len(sas)),
	})
	return ctrl.Result{}, nil
}

// ensureServiceAccount projects one declared account. It gates the managed
// User/Project creation behind the fail-loudly collision probe, generates and
// round-trips the password, and returns the account's readiness plus the single
// blocking condition (if any).
func (r *ControlPlaneReconciler) ensureServiceAccount(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec,
	credRef, managedCredRef orcv1alpha1.CloudCredentialsReference, prior c5c3v1alpha1.ServiceAccountStatus,
) (serviceAccountState, error) {
	userName := serviceAccountUserName(sa)
	domain := serviceAccountDomainName(cp, sa)
	st := serviceAccountState{
		status: c5c3v1alpha1.ServiceAccountStatus{
			Name:                 sa.Name,
			SecretName:           serviceAccountCredentialsSecretName(cp, sa),
			LastPasswordRotation: prior.LastPasswordRotation,
		},
		roles:     len(sa.Roles) > 0,
		scheduled: sa.Rotation != nil && sa.Rotation.Mode == c5c3v1alpha1.ServiceAccountRotationModeScheduled,
	}

	// (a) Domain handle.
	domainRef, err := r.ensureServiceAccountDomain(ctx, cp, sa, credRef)
	if err != nil {
		return st, fmt.Errorf("ensuring domain import: %w", err)
	}

	// (b) Project.
	projObj, projReady, err := r.ensureServiceAccountProject(ctx, cp, sa, credRef, managedCredRef, domainRef, &st)
	if err != nil {
		return st, err
	}
	if st.collision != "" || st.probingMsg != "" {
		return st, nil
	}
	if projObj != nil && projObj.Status.ID != nil {
		st.status.ProjectID = *projObj.Status.ID
	}

	// (c) User collision gate.
	proceed, err := r.serviceAccountUserCollisionGate(ctx, cp, sa, credRef, userName, domainRef, &st)
	if err != nil {
		return st, err
	}
	if !proceed {
		return st, nil
	}

	// (d)/(e) Managed User with generation-scoped password (and rotation flip).
	user, gen, created, rotatedAt, err := r.ensureServiceAccountUser(ctx, cp, sa, managedCredRef, domainRef,
		serviceAccountProjectRef(cp, sa))
	if err != nil {
		return st, err
	}
	st.created = created
	st.status.PasswordGeneration = gen
	if user.Status.ID != nil {
		st.status.UserID = *user.Status.ID
	}
	if rotatedAt != nil {
		st.status.LastPasswordRotation = rotatedAt
	}

	// A terminal K-ORC error on the user or project fails loud.
	for _, obj := range []orcv1alpha1.ObjectWithConditions{projObj, user} {
		if termErr := orcv1alpha1.GetTerminalError(obj); termErr != nil {
			st.terminalMsg = fmt.Sprintf("K-ORC reported a terminal error on service account %q: %v", sa.Name, termErr)
			return st, nil
		}
	}

	passwordRefName := serviceAccountPasswordSecretName(cp, sa, gen)
	appliedCurrent := user.Status.Resource != nil && user.Status.Resource.AppliedPasswordRef == passwordRefName
	if !projReady || !orcv1alpha1.IsAvailable(user) || !appliedCurrent {
		st.waitMsg = serviceAccountWaitMessage(sa.Name, projObj, user)
		st.pendingObjs = pendingServiceAccountObjs(projObj, user)
		return st, nil
	}

	// (f) Publish: assemble the source Secret, push to OpenBao, materialize the
	// consumer Secret, and gate readiness on the materialized password matching.
	published, err := r.publishServiceAccount(ctx, cp, sa, userName, sa.Project.Name, domain, gen)
	if err != nil {
		return st, err
	}
	if !published {
		st.waitMsg = fmt.Sprintf("service account %q is provisioned in Keystone; awaiting the OpenBao round-trip and materialized Secret", sa.Name)
		return st, nil
	}
	st.ready = true
	return st, nil
}

// ensureServiceAccountDomain ensures the Domain handle the account's user/project
// resolve against. When the effective domain matches the admin domain it reuses
// the admin Domain import (created by reconcileKORC) and creates nothing.
func (r *ControlPlaneReconciler) ensureServiceAccountDomain(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec,
	credRef orcv1alpha1.CloudCredentialsReference,
) (string, error) {
	name := serviceAccountDomainRef(cp, sa)
	if name == adminDomainRef(cp) {
		return name, nil
	}
	domain := &orcv1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: childNamespace(cp)},
		Spec: orcv1alpha1.DomainSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			Import: &orcv1alpha1.DomainImport{
				Filter: &orcv1alpha1.DomainFilter{Name: ptr.To(orcv1alpha1.KeystoneName(serviceAccountDomainName(cp, sa)))},
			},
		},
	}
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, domain, apply.FieldManager); err != nil {
		return "", fmt.Errorf("service-account Domain import %q: %w", name, err)
	}
	return name, nil
}

// ensureServiceAccountProject ensures the project handle. For create:false it is
// an unmanaged import (referenced, never created or deleted by the operator). For
// create:true it is a probe-gated managed Project. It writes any collision /
// probing verdict onto st and returns the project object plus whether it is ready.
func (r *ControlPlaneReconciler) ensureServiceAccountProject(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec,
	credRef, managedCredRef orcv1alpha1.CloudCredentialsReference, domainRef string, st *serviceAccountState,
) (*orcv1alpha1.Project, bool, error) {
	ns := childNamespace(cp)
	name := serviceAccountProjectRef(cp, sa)

	if !sa.Project.Create {
		project := &orcv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: orcv1alpha1.ProjectSpec{
				ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
				CloudCredentialsRef: credRef,
				Import: &orcv1alpha1.ProjectImport{
					Filter: &orcv1alpha1.ProjectFilter{
						Name:      ptr.To(orcv1alpha1.KeystoneName(sa.Project.Name)),
						DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(domainRef)),
					},
				},
			},
		}
		if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, project, apply.FieldManager); err != nil {
			return nil, false, fmt.Errorf("service-account referenced Project import %q: %w", name, err)
		}
		return project, korcAvailableUpToDate(project), nil
	}

	// create:true — probe for a pre-existing project before creating a managed one
	// (K-ORC would silently adopt it). Skip the probe once the managed Project we
	// own already exists.
	managed := &orcv1alpha1.Project{}
	switch err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, managed); {
	case err == nil:
		// already ours — drop any leftover probe and converge.
	case apierrors.IsNotFound(err):
		verdict, probe, err := r.serviceAccountProjectProbe(ctx, cp, sa, credRef, domainRef)
		if err != nil {
			return nil, false, err
		}
		switch verdict {
		case probeResolved:
			st.collision = fmt.Sprintf(
				"service account %q wants to CREATE project %q in domain %q, but a project of that name already "+
					"exists in Keystone; the operator will not silently adopt it — set project.create=false to "+
					"reference the existing project instead",
				sa.Name, sa.Project.Name, serviceAccountDomainName(cp, sa),
			)
			return probe, false, nil
		case probePending:
			st.probingMsg = fmt.Sprintf("probing whether project %q already exists in Keystone before creating it for service account %q",
				sa.Project.Name, sa.Name)
			st.pendingObjs = pendingServiceAccountObjs(probe)
			return probe, false, nil
		case probeAbsent:
			// No pre-existing project; fall through to create the managed Project.
		}
		// probeAbsent — remove the probe and fall through to create the managed Project.
		if err := r.deleteServiceAccountChild(ctx, &orcv1alpha1.Project{}, serviceAccountProjectProbeRef(cp, sa), ns); err != nil {
			return nil, false, err
		}
	default:
		return nil, false, fmt.Errorf("reading managed Project %q: %w", name, err)
	}

	project := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: orcv1alpha1.ProjectSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: managedCredRef,
			Resource: &orcv1alpha1.ProjectResourceSpec{
				Name:      ptr.To(orcv1alpha1.KeystoneName(sa.Project.Name)),
				DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(domainRef)),
			},
		},
	}
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, project, apply.FieldManager); err != nil {
		return nil, false, fmt.Errorf("service-account managed Project %q: %w", name, err)
	}
	return project, korcAvailableUpToDate(project), nil
}

// serviceAccountUserCollisionGate implements the fail-loudly default for
// pre-existing users. It returns proceed=true when the managed User may be
// created (the operator already owns it, adoption was requested, or a probe
// confirmed the user is absent) and writes a collision / probing verdict onto st
// otherwise.
func (r *ControlPlaneReconciler) serviceAccountUserCollisionGate(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec,
	credRef orcv1alpha1.CloudCredentialsReference, userName, domainRef string, st *serviceAccountState,
) (bool, error) {
	ns := childNamespace(cp)

	existing := &orcv1alpha1.User{}
	switch err := r.Get(ctx, types.NamespacedName{Name: serviceAccountUserRef(cp, sa), Namespace: ns}, existing); {
	case err == nil:
		// The managed User already exists (we own it); no probe needed.
		return true, r.deleteServiceAccountChild(ctx, &orcv1alpha1.User{}, serviceAccountUserProbeRef(cp, sa), ns)
	case apierrors.IsNotFound(err):
	default:
		return false, fmt.Errorf("reading managed User %q: %w", serviceAccountUserRef(cp, sa), err)
	}

	// adopt: skip the probe and take over the pre-existing user's password.
	if sa.Adopt {
		return true, r.deleteServiceAccountChild(ctx, &orcv1alpha1.User{}, serviceAccountUserProbeRef(cp, sa), ns)
	}

	verdict, probe, err := r.serviceAccountUserProbe(ctx, cp, sa, credRef, userName, domainRef)
	if err != nil {
		return false, err
	}
	switch verdict {
	case probeResolved:
		st.collision = fmt.Sprintf(
			"service account %q resolves to Keystone user %q in domain %q, which already exists; the operator "+
				"fails loudly rather than take over an account it did not create — set adopt=true to take over "+
				"the account (and rotate its password), or remove the entry",
			sa.Name, userName, serviceAccountDomainName(cp, sa),
		)
		return false, nil
	case probePending:
		st.probingMsg = fmt.Sprintf("probing whether Keystone user %q already exists before creating service account %q",
			userName, sa.Name)
		st.pendingObjs = pendingServiceAccountObjs(probe)
		return false, nil
	case probeAbsent:
		// The user does not exist; drop the probe and create the managed User below.
	}
	return true, r.deleteServiceAccountChild(ctx, &orcv1alpha1.User{}, serviceAccountUserProbeRef(cp, sa), ns)
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

// serviceAccountUserProbe ensures the short-lived unmanaged User import used to
// decide exists/absent before creating the managed User, and returns its verdict.
func (r *ControlPlaneReconciler) serviceAccountUserProbe(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec,
	credRef orcv1alpha1.CloudCredentialsReference, userName, domainRef string,
) (probeVerdict, *orcv1alpha1.User, error) {
	probe := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountUserProbeRef(cp, sa), Namespace: childNamespace(cp)},
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
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, probe, apply.FieldManager); err != nil {
		return probePending, nil, fmt.Errorf("service-account User probe %q: %w", probe.Name, err)
	}
	return interpretProbe(probe), probe, nil
}

// serviceAccountProjectProbe ensures the short-lived unmanaged Project import used
// to decide exists/absent before creating a managed Project.
func (r *ControlPlaneReconciler) serviceAccountProjectProbe(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec,
	credRef orcv1alpha1.CloudCredentialsReference, domainRef string,
) (probeVerdict, *orcv1alpha1.Project, error) {
	probe := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountProjectProbeRef(cp, sa), Namespace: childNamespace(cp)},
		Spec: orcv1alpha1.ProjectSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			Import: &orcv1alpha1.ProjectImport{
				Filter: &orcv1alpha1.ProjectFilter{
					Name:      ptr.To(orcv1alpha1.KeystoneName(sa.Project.Name)),
					DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(domainRef)),
				},
			},
		},
	}
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, probe, apply.FieldManager); err != nil {
		return probePending, nil, fmt.Errorf("service-account Project probe %q: %w", probe.Name, err)
	}
	return interpretProbe(probe), probe, nil
}

// ensureServiceAccountUser create-or-updates the managed K-ORC User with the
// current-generation password, and flips the passwordRef to a fresh generation
// when the CredentialRotation reconciler has cleared the generation annotation.
// K-ORC's user actuator re-applies the password only when the passwordRef NAME
// changes, so a rotation is a Secret-name flip, not a content edit.
func (r *ControlPlaneReconciler) ensureServiceAccountUser(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec,
	managedCredRef orcv1alpha1.CloudCredentialsReference, domainRef, projectRef string,
) (*orcv1alpha1.User, int64, bool, *metav1.Time, error) {
	ns := childNamespace(cp)
	userName := serviceAccountUserName(sa)
	name := serviceAccountUserRef(cp, sa)

	existing := &orcv1alpha1.User{}
	getErr := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing)
	exists := getErr == nil
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return nil, 0, false, nil, fmt.Errorf("reading managed User %q: %w", name, getErr)
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

	if err := r.ensureServiceAccountPasswordSecret(ctx, cp, sa, desiredGen); err != nil {
		return nil, 0, false, nil, err
	}
	passwordRefName := serviceAccountPasswordSecretName(cp, sa, desiredGen)

	// This projection stays read-modify-write (not Server-Side Apply): the mutate
	// closure reads the LIVE User's CreationTimestamp and existing
	// serviceAccountPasswordGenerationAnnotation to decide whether to (re-)stamp the
	// generation, which cannot be expressed as a pure projection of cp.Spec.
	user := &orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, user, func() error {
		user.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
		user.Spec.Import = nil
		user.Spec.CloudCredentialsRef = managedCredRef
		if user.Spec.Resource == nil {
			user.Spec.Resource = &orcv1alpha1.UserResourceSpec{}
		}
		user.Spec.Resource.Name = ptr.To(orcv1alpha1.OpenStackName(userName))
		user.Spec.Resource.DomainRef = ptr.To(orcv1alpha1.KubernetesNameRef(domainRef))
		user.Spec.Resource.DefaultProjectRef = ptr.To(orcv1alpha1.KubernetesNameRef(projectRef))
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
		return controllerutil.SetControllerReference(cp, user, r.Scheme)
	})
	if err != nil {
		return nil, 0, false, nil, fmt.Errorf("service-account managed User %q: %w", name, err)
	}

	// Record the rotation timestamp and prune superseded password Secrets once
	// K-ORC confirms the current generation is applied.
	var rotatedAt *metav1.Time
	if rotating {
		now := metav1.Now()
		rotatedAt = &now
	}
	applied := user.Status.Resource != nil && user.Status.Resource.AppliedPasswordRef == passwordRefName
	if applied {
		// List the account's password Secrets that ACTUALLY exist and delete only
		// the superseded generations, rather than blind-Deleting v1..v(desiredGen-1)
		// every steady-state pass. This runs on every reconcile once applied stays
		// true, so a blind loop would issue a growing, unbounded stream of NotFound
		// DELETE round-trips for long-gone generations that never converges.
		var pwSecrets corev1.SecretList
		if err := r.List(ctx, &pwSecrets, client.InNamespace(ns)); err != nil {
			return nil, 0, false, nil, fmt.Errorf("listing superseded password Secrets: %w", err)
		}
		prefix := serviceAccountChildPrefix(cp) + sa.Name + "-password-v"
		for i := range pwSecrets.Items {
			name := pwSecrets.Items[i].Name
			if g, ok := parseServiceAccountGeneration(name); ok &&
				strings.HasPrefix(name, prefix) && g < desiredGen &&
				r.ownsServiceAccountChild(cp, &pwSecrets.Items[i]) {
				if err := r.deleteServiceAccountChild(ctx, &corev1.Secret{}, name, ns); err != nil {
					return nil, 0, false, nil, err
				}
			}
		}
	}
	return user, desiredGen, op == controllerutil.OperationResultCreated, rotatedAt, nil
}

// ensureServiceAccountPasswordSecret ensures the generation-scoped, operator-owned
// Secret holding the K-ORC-facing password exists with a generated value. The
// value is generated once and preserved (a new generation is a new Secret name,
// never an in-place edit).
func (r *ControlPlaneReconciler) ensureServiceAccountPasswordSecret(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec, gen int64,
) error {
	name := serviceAccountPasswordSecretName(cp, sa, gen)
	if err := r.ensureOwnedSecret(ctx, cp, name, func(secret *corev1.Secret) error {
		if len(secret.Data[serviceAccountPasswordKey]) == 0 {
			v, gerr := generateAppCredSecretValue()
			if gerr != nil {
				return gerr
			}
			secret.Data[serviceAccountPasswordKey] = []byte(v)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ensuring service-account password Secret %q: %w", name, err)
	}
	return nil
}

// publishServiceAccount assembles the source Secret, mirrors it to the per-CR
// OpenBao path, materializes the consumer Secret, and reports whether the
// materialized "password" matches the current generation (a rotated-away password
// never reads ready). It is only called once K-ORC confirms the current password
// is applied to the user.
func (r *ControlPlaneReconciler) publishServiceAccount(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec,
	userName, projectName, domain string, gen int64,
) (bool, error) {
	ns := childNamespace(cp)

	pwSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: serviceAccountPasswordSecretName(cp, sa, gen), Namespace: ns}, pwSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading service-account password Secret: %w", err)
	}
	password := pwSecret.Data[serviceAccountPasswordKey]
	if len(password) == 0 {
		return false, nil
	}

	cloudsYAML := []byte(buildServiceAccountCloudsYAML(cp, userName, projectName, domain, string(password)))
	if err := r.ensureOwnedSecret(ctx, cp, serviceAccountSourceSecretName(cp, sa), func(secret *corev1.Secret) error {
		secret.Data[serviceAccountPasswordKey] = password
		secret.Data["username"] = []byte(userName)
		secret.Data["project_name"] = []byte(projectName)
		secret.Data["user_domain_name"] = []byte(domain)
		secret.Data["project_domain_name"] = []byte(domain)
		secret.Data["auth_url"] = []byte(korcAuthURL(cp))
		secret.Data["region_name"] = []byte(korcRegion(cp))
		secret.Data[appCredCloudsYAMLKey] = cloudsYAML
		return nil
	}); err != nil {
		return false, fmt.Errorf("assembling service-account source Secret: %w", err)
	}

	sum := sha256.Sum256(cloudsYAML)
	contentHash := hex.EncodeToString(sum[:])

	ps := serviceAccountPushSecret(cp, sa)
	if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, cp, ps); err != nil {
		return false, fmt.Errorf("ensuring service-account PushSecret: %w", err)
	}
	if err := r.forceRepushPushSecret(ctx, cp, ps.Name, serviceAccountPushContentHashAnnotation, contentHash); err != nil {
		return false, fmt.Errorf("forcing service-account PushSecret re-push: %w", err)
	}
	pushed := &esov1alpha1.PushSecret{}
	if err := r.Get(ctx, types.NamespacedName{Name: ps.Name, Namespace: ns}, pushed); err != nil {
		return false, fmt.Errorf("reading service-account PushSecret: %w", err)
	}
	if !pushSecretReady(pushed) {
		return false, nil
	}

	if err := r.ensureServiceAccountExternalSecret(ctx, cp, sa); err != nil {
		return false, fmt.Errorf("ensuring service-account ExternalSecret: %w", err)
	}
	syncTrigger := contentHash + "/" + pushed.Status.SyncedResourceVersion
	if err := r.forceSyncExternalSecret(ctx, cp, serviceAccountCredentialsSecretName(cp, sa), syncTrigger); err != nil {
		return false, fmt.Errorf("forcing service-account ExternalSecret re-sync: %w", err)
	}

	materialized := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: serviceAccountCredentialsSecretName(cp, sa), Namespace: ns}, materialized); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading materialized service-account Secret: %w", err)
	}
	return bytes.Equal(materialized.Data[serviceAccountPasswordKey], password), nil
}

// serviceAccountPushSecret builds the PushSecret mirroring the source Secret to
// the per-CR OpenBao path. DeletionPolicy Delete: the credential dies with the
// account that owns it (the same rationale as adminAppCredentialPushSecret).
func serviceAccountPushSecret(cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountPushSecretName(cp, sa), Namespace: childNamespace(cp)},
		Spec: esov1alpha1.PushSecretSpec{
			DeletionPolicy:  esov1alpha1.PushSecretDeletionPolicyDelete,
			SecretStoreRefs: secrets.PushSecretStoreRefs(effectiveControlPlaneStoreRef(cp)),
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{Name: serviceAccountSourceSecretName(cp, sa)},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{RemoteKey: serviceAccountRemoteKeyFor(cp, sa.Name)},
				},
			}},
		},
	}
}

// ensureServiceAccountExternalSecret create-or-updates the operator-owned
// ExternalSecret that materializes the consumer Secret from the per-CR OpenBao
// path. It reads back the "password" and "clouds.yaml" properties — the documented
// consumption contract.
func (r *ControlPlaneReconciler) ensureServiceAccountExternalSecret(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, sa c5c3v1alpha1.ServiceAccountSpec,
) error {
	name := serviceAccountCredentialsSecretName(cp, sa)
	remoteKey := serviceAccountRemoteKeyFor(cp, sa.Name)
	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: childNamespace(cp)},
		Spec: esov1.ExternalSecretSpec{
			RefreshInterval: &metav1.Duration{Duration: time.Hour},
			SecretStoreRef:  secrets.ESOSecretStoreRef(effectiveControlPlaneStoreRef(cp)),
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
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, es, apply.FieldManager); err != nil {
		return fmt.Errorf("ensuring service-account ExternalSecret %q: %w", name, err)
	}
	return nil
}

// deleteServiceAccountChild issues an idempotent Delete on a single named child in
// childNamespace(cp), tolerating NotFound. It is used to drop resolved probes and
// superseded password Secrets.
func (r *ControlPlaneReconciler) deleteServiceAccountChild(ctx context.Context, obj client.Object, name, ns string) error {
	obj.SetName(name)
	obj.SetNamespace(ns)
	if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
		return fmt.Errorf("deleting service-account child %q: %w", name, err)
	}
	return nil
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

// serviceAccountWaitMessage names the stuck dependency (Project before User, the
// dependency order) so a bounded wait points at the real blocker.
func serviceAccountWaitMessage(name string, project *orcv1alpha1.Project, user *orcv1alpha1.User) string {
	switch {
	case project != nil && !korcAvailableUpToDate(project):
		return fmt.Sprintf("service account %q: project is registered but not yet Available", name)
	case !orcv1alpha1.IsAvailable(user):
		return fmt.Sprintf("service account %q: user is registered but not yet Available", name)
	default:
		return fmt.Sprintf("service account %q: awaiting K-ORC to apply the current password to the user", name)
	}
}

// pruneServiceAccounts deletes the service-account child CRs this ControlPlane
// owns which the spec no longer declares, scoped by controller ownership AND the
// "-service-account-" name prefix, so the admin imports and any foreign CR can
// never be caught by it. It returns the names of the still-present K-ORC child
// removals (Terminating behind a K-ORC finalizer) so the caller gates readiness
// on them; finalizer-less children (Secrets, ESO CRs) delete instantly and do not
// gate.
//
// Password Secrets are NOT swept for declared accounts: their generation lifecycle
// is owned by ensureServiceAccountUser (create v(N), delete superseded v(<N) once
// K-ORC applies the new one), and the sweep must not race it into deleting the
// current-generation Secret the managed User references.
func (r *ControlPlaneReconciler) pruneServiceAccounts(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, declared []c5c3v1alpha1.ServiceAccountSpec,
) ([]string, error) {
	logger := log.FromContext(ctx)
	ns := childNamespace(cp)
	prefix := serviceAccountChildPrefix(cp)

	keep := serviceAccountDeclaredChildNames(cp, declared)
	declaredPrefixes := make([]string, 0, len(declared))
	for i := range declared {
		declaredPrefixes = append(declaredPrefixes, prefix+declared[i].Name+"-")
	}
	declaredPasswordSecret := func(name string) bool {
		for _, p := range declaredPrefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}

	var pruning []string
	// sweep deletes obj when it is owned, prefixed, and undeclared; gate reports
	// whether a still-present removal should block readiness.
	sweep := func(obj client.Object, gate bool) error {
		name := obj.GetName()
		if !r.ownsServiceAccountChild(cp, obj) {
			return nil
		}
		if strings.Contains(name, "-password-v") {
			if declaredPasswordSecret(name) {
				return nil
			}
		} else if keep[name] {
			return nil
		}
		logger.Info("removing an undeclared service-account child", "name", name)
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			return fmt.Errorf("deleting service-account child %q: %w", name, err)
		}
		if gate {
			pruning = append(pruning, name)
		}
		return nil
	}

	var users orcv1alpha1.UserList
	if err := r.List(ctx, &users, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing service-account Users: %w", err)
	}
	for i := range users.Items {
		if err := sweep(&users.Items[i], true); err != nil {
			return nil, err
		}
	}
	var projects orcv1alpha1.ProjectList
	if err := r.List(ctx, &projects, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing service-account Projects: %w", err)
	}
	for i := range projects.Items {
		if err := sweep(&projects.Items[i], true); err != nil {
			return nil, err
		}
	}
	var domains orcv1alpha1.DomainList
	if err := r.List(ctx, &domains, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing service-account Domains: %w", err)
	}
	for i := range domains.Items {
		if err := sweep(&domains.Items[i], true); err != nil {
			return nil, err
		}
	}
	var pushSecrets esov1alpha1.PushSecretList
	if err := r.List(ctx, &pushSecrets, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing service-account PushSecrets: %w", err)
	}
	for i := range pushSecrets.Items {
		if err := sweep(&pushSecrets.Items[i], false); err != nil {
			return nil, err
		}
	}
	var externalSecrets esov1.ExternalSecretList
	if err := r.List(ctx, &externalSecrets, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing service-account ExternalSecrets: %w", err)
	}
	for i := range externalSecrets.Items {
		if err := sweep(&externalSecrets.Items[i], false); err != nil {
			return nil, err
		}
	}
	// Secrets: password (generation-scoped) + source. The materialized credentials
	// Secret is ESO-owned (CreationPolicy Owner) and garbage-collected with its
	// ExternalSecret, so it carries no ControlPlane controller reference and the
	// ownership guard correctly leaves it to the ExternalSecret deletion above.
	var kubeSecrets corev1.SecretList
	if err := r.List(ctx, &kubeSecrets, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing service-account Secrets: %w", err)
	}
	for i := range kubeSecrets.Items {
		if err := sweep(&kubeSecrets.Items[i], false); err != nil {
			return nil, err
		}
	}

	return pruning, nil
}

// serviceAccountDeclaredChildNames returns the set of NON-password child names the
// declared accounts legitimately own, so the prune sweep keeps them. Password
// Secrets are excluded here and handled by the prefix guard in pruneServiceAccounts
// (their generation lifecycle is owned by ensureServiceAccountUser).
func serviceAccountDeclaredChildNames(cp *c5c3v1alpha1.ControlPlane, declared []c5c3v1alpha1.ServiceAccountSpec) map[string]bool {
	keep := map[string]bool{}
	for i := range declared {
		sa := declared[i]
		keep[serviceAccountUserRef(cp, sa)] = true
		keep[serviceAccountUserProbeRef(cp, sa)] = true
		keep[serviceAccountProjectRef(cp, sa)] = true
		keep[serviceAccountProjectProbeRef(cp, sa)] = true
		keep[serviceAccountDomainRef(cp, sa)] = true
		keep[serviceAccountSourceSecretName(cp, sa)] = true
		keep[serviceAccountPushSecretName(cp, sa)] = true
		keep[serviceAccountCredentialsSecretName(cp, sa)] = true
	}
	return keep
}
