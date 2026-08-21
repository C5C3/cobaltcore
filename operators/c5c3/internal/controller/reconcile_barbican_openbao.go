// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The dedicated secret store (services.barbican.secretStore.dedicated) gives one
// Barbican an OpenBao instance of its own, in the Barbican service namespace. The
// ensemble below is the operator-side equivalent of the proving instance
// deploy/kind/infrastructure/openbao-instance.yaml declares by hand, together with
// the choreography hack/deploy-infra.sh runs around it: seed the static-seal key
// before the CR reconciles, attach the instance's ownerReference to that Secret,
// and grant the TokenReview the instance's Kubernetes auth method needs.
//
// Two pieces of that hand choreography disappear here rather than being
// reproduced. The proving instance starts paused because an ExternalSecret has to
// materialise a key custodied elsewhere before the operator can blind-create its
// own; the operator generates the key itself and writes the Secret before the CR
// exists, so nothing has to be paused and un-paused. And the ownerReference the
// script patches on afterwards is stamped in the same reconcile pass the instance
// is created in.
//
// Everything the ControlPlane places carries its ownership labels, so the
// finalizer-driven teardown can find it: a cross-namespace child takes no owner
// reference (Kubernetes forbids one), and the cluster-scoped ClusterRoleBinding is
// collected by no namespace deletion at all.
//
// The whole ensemble is written to the cluster the Barbican service was placed
// on, because everything that acts on it runs there: the openbao-operator that
// reconciles the instance, the cert-manager that issues its transport
// certificates, the TokenReview its Kubernetes-auth logins are validated with, and
// the Barbican pods that read their tenant key material from it. The one owner
// reference in it is target-local and therefore stays: the unseal Secret takes the
// instance's, and both live in the same namespace on the same cluster.

// defaultOpenBaoVersion is the OpenBao image tag the projected instance runs. The
// image resolves to docker.io/openbao/openbao:<version>; the static seal needs
// >= 2.4.0. Renovate tracks this pin, so the assignment stays a single-line string
// literal.
const defaultOpenBaoVersion = "2.6.1"

const (
	// barbicanOpenBaoNameSuffix is appended to barbicanName(cp) to derive the
	// name of the dedicated OpenBao instance, so a namespace can host the
	// instances of several ControlPlanes without collision.
	barbicanOpenBaoNameSuffix = "-bao"
	// barbicanOpenBaoTenantSuffix names the OpenBaoTenant that admits the service
	// namespace to the openbao-operator.
	barbicanOpenBaoTenantSuffix = "-tenant"
	// barbicanOpenBaoUnsealSecretSuffix names the Secret holding the instance's
	// static-seal key. The name is the operator's fixed contract
	// (<instance>-unseal-key, data key `key`), not a choice.
	barbicanOpenBaoUnsealSecretSuffix = "-unseal-key" //nolint:gosec // G101 false positive: Secret name suffix, not a credential.
	// barbicanOpenBaoUnsealSecretKey is the data key the openbao-operator mounts
	// the static-seal key from.
	barbicanOpenBaoUnsealSecretKey = "key"
	// barbicanOpenBaoServerCertSuffix / barbicanOpenBaoCACertSuffix name the two
	// fixed-name cert-manager Secrets the instance's External TLS mode consumes.
	barbicanOpenBaoServerCertSuffix = "-tls-server"
	barbicanOpenBaoCACertSuffix     = "-tls-ca"
	// barbicanOpenBaoProvisionerSuffix names the ServiceAccount the Kubernetes-auth
	// role "provisioner" binds to — the identity the barbican-operator logs in with.
	barbicanOpenBaoProvisionerSuffix = "-provisioner"
	// barbicanOpenBaoPodSASuffix is the ServiceAccount the openbao-operator itself
	// creates for the instance pods. It is not projected here; it is the subject of
	// the auth-delegator binding, because the instance validates Kubernetes-auth
	// logins with a TokenReview issued under that account.
	barbicanOpenBaoPodSASuffix = "-serviceaccount"
	// barbicanOpenBaoAuthDelegatorSuffix names the cluster-scoped binding granting
	// that TokenReview.
	barbicanOpenBaoAuthDelegatorSuffix = "-auth-delegator"
	// barbicanOpenBaoTokenGrantSuffix names the Role and RoleBinding that let the
	// barbican-operator mint a bound token for the provisioner account.
	barbicanOpenBaoTokenGrantSuffix = "-provisioner-token" //nolint:gosec // G101 false positive: RBAC object name suffix, not a credential.
	// barbicanOpenBaoStorageSize is the raft volume the instance is created with.
	// It is honoured on fresh create only: the openbao-operator rejects a change to
	// spec.storage on an existing CR.
	barbicanOpenBaoStorageSize = "1Gi"
	// barbicanOpenBaoCertDuration / barbicanOpenBaoCertRenewBefore mirror the
	// 1-year lifetime and 30-day renewBefore every OpenBao transport certificate in
	// this stack carries.
	barbicanOpenBaoCertDuration    = "8760h"
	barbicanOpenBaoCertRenewBefore = "720h"
)

const (
	// defaultBarbicanOperatorNamespace / defaultBarbicanOperatorServiceAccount are
	// the barbican-operator's chart defaults. They are the fallback for a
	// reconciler constructed without the BARBICAN_OPERATOR_* environment, which is
	// every unit test and envtest run.
	defaultBarbicanOperatorNamespace      = "barbican-system"
	defaultBarbicanOperatorServiceAccount = "barbican-operator"
)

// barbicanUnsealKeyRandRead is the randomness source for the static-seal key,
// indirected through a package-level variable so a test can drive the failure
// path. Nothing but a test replaces it.
var barbicanUnsealKeyRandRead = rand.Read

// barbicanOperatorNamespace returns the namespace the barbican-operator runs in,
// falling back to the chart default when the reconciler carries no value.
func (r *ControlPlaneReconciler) barbicanOperatorNamespace() string {
	if r.BarbicanOperatorNamespace != "" {
		return r.BarbicanOperatorNamespace
	}
	return defaultBarbicanOperatorNamespace
}

// barbicanOperatorServiceAccount returns the ServiceAccount the barbican-operator
// runs as, falling back to the chart default when the reconciler carries no value.
func (r *ControlPlaneReconciler) barbicanOperatorServiceAccount() string {
	if r.BarbicanOperatorServiceAccount != "" {
		return r.BarbicanOperatorServiceAccount
	}
	return defaultBarbicanOperatorServiceAccount
}

// barbicanOpenBaoName returns the name of the dedicated OpenBao instance
// projected for this ControlPlane's Barbican service. Every other object in the
// ensemble is named after it, because the openbao-operator derives several of its
// own object names from the instance name.
func barbicanOpenBaoName(cp *c5c3v1alpha1.ControlPlane) string {
	return barbicanName(cp) + barbicanOpenBaoNameSuffix
}

// ensureBarbicanOpenBao projects the dedicated OpenBao instance and everything it
// needs to serve a managed Barbican secret store, and reports whether the instance
// is Available. It sets no ControlPlane condition: the caller owns status.
//
// c is the client the Barbican namespace's children are written with, so the whole
// ensemble — the cluster-scoped auth-delegator binding included — lands on the
// cluster the service was placed on, and the Available gate is answered from
// there. The caller resolves it before anything is written.
//
// The order is a dependency order. The unseal key exists before the CR, or the
// operator blind-creates an immutable Secret of its own and the generated key is
// lost. The TLS Secrets and the provisioner account exist before the CR, because
// External TLS blocks until both certificates land and the self-init'd
// Kubernetes-auth role names the account. The auth-delegator binding and the
// TokenRequest grant are what turn a login that would fail 403 into one that
// succeeds. The instance's ownerReference is stamped onto the unseal Secret last,
// once there is an instance to reference.
func (r *ControlPlaneReconciler) ensureBarbicanOpenBao(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) (bool, error) {
	namespace := cp.BarbicanNamespace()
	name := barbicanOpenBaoName(cp)

	if err := r.ensureBarbicanOpenBaoTenant(ctx, c, cp); err != nil {
		return false, err
	}
	if err := r.ensureBarbicanUnsealKey(ctx, c, cp, nil); err != nil {
		return false, err
	}
	if err := r.ensureBarbicanOpenBaoCertificates(ctx, c, cp); err != nil {
		return false, err
	}
	if err := r.ensureUnownedOrOwned(ctx, c, cp, barbicanOpenBaoProvisionerServiceAccount(name, namespace)); err != nil {
		return false, fmt.Errorf("ensuring Barbican OpenBao provisioner ServiceAccount: %w", err)
	}
	if err := r.ensureUnownedOrOwned(ctx, c, cp, barbicanOpenBaoAuthDelegatorBinding(name, namespace)); err != nil {
		return false, fmt.Errorf("ensuring Barbican OpenBao auth-delegator ClusterRoleBinding: %w", err)
	}
	if err := r.ensureUnownedOrOwned(ctx, c, cp, barbicanOpenBaoTokenRole(name, namespace)); err != nil {
		return false, fmt.Errorf("ensuring Barbican OpenBao TokenRequest Role: %w", err)
	}
	binding := barbicanOpenBaoTokenRoleBinding(name, namespace,
		r.barbicanOperatorServiceAccount(), r.barbicanOperatorNamespace())
	if err := r.ensureUnownedOrOwned(ctx, c, cp, binding); err != nil {
		return false, fmt.Errorf("ensuring Barbican OpenBao TokenRequest RoleBinding: %w", err)
	}

	instance, err := r.ensureBarbicanOpenBaoCluster(ctx, c, cp)
	if err != nil {
		return false, err
	}
	if err := r.ensureBarbicanUnsealKey(ctx, c, cp, instance); err != nil {
		return false, err
	}
	return openBaoClusterAvailable(instance), nil
}

// openBaoClusterAvailable reports whether the instance serves requests, which the
// openbao-operator signals with Available=True.
func openBaoClusterAvailable(instance *openbaov1alpha1.OpenBaoCluster) bool {
	if instance == nil {
		return false
	}
	return meta.IsStatusConditionTrue(instance.Status.Conditions, string(openbaov1alpha1.ConditionAvailable))
}

// ensureBarbicanOpenBaoTenant admits the Barbican service namespace to the
// openbao-operator, but only when nobody else has already admitted it.
//
// The check is a CLUSTER-WIDE list on spec.targetNamespace rather than a get on
// our own name: a namespace admitted twice is admitted by two tenants, and each
// holds a finalizer over it. In the kind stack the proving instance's tenant
// already targets the openstack namespace, so a second, ControlPlane-owned tenant
// would leave teardown waiting behind a finalizer this operator does not own. A
// pre-existing tenant is left exactly as it is. The list runs on the cluster the
// tenant would be created on, which is the only cluster whose openbao-operator the
// admission would apply to.
func (r *ControlPlaneReconciler) ensureBarbicanOpenBaoTenant(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) error {
	namespace := cp.BarbicanNamespace()

	tenants := &openbaov1alpha1.OpenBaoTenantList{}
	if err := c.List(ctx, tenants); err != nil {
		return fmt.Errorf("listing OpenBaoTenants before admitting namespace %q: %w", namespace, err)
	}
	for i := range tenants.Items {
		if tenants.Items[i].Spec.TargetNamespace == namespace {
			log.FromContext(ctx).V(1).Info("namespace already admitted by an OpenBaoTenant, skipping creation",
				"namespace", namespace, "tenant", client.ObjectKeyFromObject(&tenants.Items[i]))
			return nil
		}
	}

	tenant := &openbaov1alpha1.OpenBaoTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      barbicanOpenBaoName(cp) + barbicanOpenBaoTenantSuffix,
			Namespace: namespace,
		},
		Spec: openbaov1alpha1.OpenBaoTenantSpec{TargetNamespace: namespace},
	}
	if err := claimChildOwnership(c, cp, tenant, r.Scheme); err != nil {
		return fmt.Errorf("claiming ownership of OpenBaoTenant %q: %w", tenant.Name, err)
	}
	if err := c.Create(ctx, tenant); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating OpenBaoTenant %q: %w", tenant.Name, err)
	}
	return nil
}

// ensureBarbicanUnsealKey ensures the Secret carrying the instance's static-seal
// key, generating the key once and never again: a static seal whose key material
// changes seals the instance permanently, so the value is read before it is
// written and an existing one is left alone.
//
// owner is the instance the key belongs to, and is nil on the call that runs
// before the CR exists. Once it is non-nil the Secret takes the instance's
// controller ownerReference — legal because both live in the service namespace on
// the same cluster, wherever that is — so deleting the instance reaps its seal key
// with it, and the openbao-operator reads that reference as the ownership proof it
// adopts a pre-existing unseal Secret against.
//
// That reference is why the ControlPlane's own claim stays label-only here rather
// than routing through claimChildOwnership: an object carries one controller
// reference, and this one belongs to the instance. On a target cluster the claim
// still has to add the owner triple the shared teardown selects on, which
// ClaimWithLabels' remote path does without setting a reference of its own.
func (r *ControlPlaneReconciler) ensureBarbicanUnsealKey(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, owner *openbaov1alpha1.OpenBaoCluster,
) error {
	name := barbicanOpenBaoName(cp) + barbicanOpenBaoUnsealSecretSuffix
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cp.BarbicanNamespace()},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		if aerr := refuseForeignAdoption(c, cp, secret, r.Scheme); aerr != nil {
			return aerr
		}
		if commonmulticluster.IsRemote(c) {
			if cerr := claimChildOwnership(c, cp, secret, r.Scheme); cerr != nil {
				return cerr
			}
		} else {
			stampControlPlaneChildLabels(secret, cp)
		}
		if len(secret.Data[barbicanOpenBaoUnsealSecretKey]) == 0 {
			key, gerr := generateBarbicanUnsealKey()
			if gerr != nil {
				return gerr
			}
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data[barbicanOpenBaoUnsealSecretKey] = []byte(key)
		}
		if owner == nil {
			return nil
		}
		return controllerutil.SetControllerReference(owner, secret, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring Barbican OpenBao unseal-key Secret %q: %w", name, err)
	}
	return nil
}

// generateBarbicanUnsealKey returns 32 bytes of randomness, base64-encoded, which
// is the form the openbao-operator mounts a static seal key in.
func generateBarbicanUnsealKey() (string, error) {
	b := make([]byte, 32)
	if _, err := barbicanUnsealKeyRandRead(b); err != nil {
		return "", fmt.Errorf("generating barbican unseal key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// ensureBarbicanOpenBaoCertificates projects the two cert-manager Certificates the
// instance's External TLS mode consumes: the server keypair for its API listener,
// and a leaf whose Secret exists only to carry the trust-domain CA under ca.crt
// (cert-manager has no primitive that materialises a CA-only Secret). Both are
// issued by the same CA-type ClusterIssuer, because the operator's External
// contract requires the server certificate to chain DIRECTLY to the CA it reads.
//
// They stay read-modify-write: no Go module ships the cert-manager types, and
// apply.EnsureObject converts typed structs rather than unstructured input.
func (r *ControlPlaneReconciler) ensureBarbicanOpenBaoCertificates(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) error {
	name, namespace := barbicanOpenBaoName(cp), cp.BarbicanNamespace()

	if err := r.ensureBarbicanOpenBaoCertificate(ctx, c, cp, name+barbicanOpenBaoServerCertSuffix,
		func(u *unstructured.Unstructured) { applyOpenBaoServerCertificateSpec(u, name, namespace) },
	); err != nil {
		return err
	}
	return r.ensureBarbicanOpenBaoCertificate(ctx, c, cp, name+barbicanOpenBaoCACertSuffix,
		func(u *unstructured.Unstructured) { applyOpenBaoCACertificateSpec(u, name+barbicanOpenBaoCACertSuffix) },
	)
}

// ensureBarbicanOpenBaoCertificate projects one of the ensemble's Certificates
// under certName, with apply writing the desired spec onto the live object.
func (r *ControlPlaneReconciler) ensureBarbicanOpenBaoCertificate(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
	certName string, apply func(*unstructured.Unstructured),
) error {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(certificateGVK)
	live.SetName(certName)
	live.SetNamespace(cp.BarbicanNamespace())
	if _, err := controllerutil.CreateOrUpdate(ctx, c, live, func() error {
		if aerr := refuseForeignAdoption(c, cp, live, r.Scheme); aerr != nil {
			return aerr
		}
		apply(live)
		return claimChildOwnership(c, cp, live, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring Barbican OpenBao Certificate %q: %w", certName, err)
	}
	return nil
}

// applyOpenBaoServerCertificateSpec sets the desired spec on the instance's server
// Certificate. The usages are pinned so the extended key usage is serverAuth
// alone: openBaoCAIssuerName also signs the client certificates the management
// OpenBao verifies behind tls_require_and_verify_client_cert, and a certificate
// with no EKU passes that check, so an unpinned server keypair would be a
// ready-made client identity for it.
//
// The SAN list is what the operator's External-mode validation and the pod probes
// verify against. No loopback IP SAN is issued: it would authenticate whatever
// listens on localhost rather than this instance.
func applyOpenBaoServerCertificateSpec(u *unstructured.Unstructured, name, namespace string) {
	// SetNested* only errors on a type conflict at an existing path; on a
	// freshly-built or Certificate-typed object the writes cannot fail, so the
	// errors are intentionally ignored here.
	_ = unstructured.SetNestedField(u.Object, name+barbicanOpenBaoServerCertSuffix, "spec", "secretName")
	_ = unstructured.SetNestedField(u.Object, barbicanOpenBaoCertDuration, "spec", "duration")
	_ = unstructured.SetNestedField(u.Object, barbicanOpenBaoCertRenewBefore, "spec", "renewBefore")
	_ = unstructured.SetNestedMap(u.Object, map[string]interface{}{
		"name": openBaoCAIssuerName,
		"kind": "ClusterIssuer",
	}, "spec", "issuerRef")
	_ = unstructured.SetNestedStringSlice(u.Object,
		[]string{"digital signature", "key encipherment", "server auth"}, "spec", "usages")
	_ = unstructured.SetNestedStringSlice(u.Object, []string{
		// The operator's internal TLS server name, which its External-mode
		// validation rejects a server certificate without.
		"openbao-cluster-" + name + ".local",
		name + "." + namespace + ".svc",
		// Pod DNS through the headless Service.
		"*." + name + "." + namespace + ".svc",
		name + "-public." + namespace + ".svc",
	}, "spec", "dnsNames")
}

// applyOpenBaoCACertificateSpec sets the desired spec on the Certificate whose
// Secret carries the trust-domain CA. Its leaf keypair goes unused, so the one
// thing that matters about it is the same EKU pin the server certificate carries:
// it must not be replayable as a client identity.
func applyOpenBaoCACertificateSpec(u *unstructured.Unstructured, name string) {
	_ = unstructured.SetNestedField(u.Object, name, "spec", "secretName")
	_ = unstructured.SetNestedField(u.Object, name, "spec", "commonName")
	_ = unstructured.SetNestedField(u.Object, barbicanOpenBaoCertDuration, "spec", "duration")
	_ = unstructured.SetNestedField(u.Object, barbicanOpenBaoCertRenewBefore, "spec", "renewBefore")
	_ = unstructured.SetNestedMap(u.Object, map[string]interface{}{
		"name": openBaoCAIssuerName,
		"kind": "ClusterIssuer",
	}, "spec", "issuerRef")
	_ = unstructured.SetNestedStringSlice(u.Object,
		[]string{"digital signature", "key encipherment", "server auth"}, "spec", "usages")
}

// barbicanOpenBaoProvisionerServiceAccount builds the account the instance's
// Kubernetes-auth role "provisioner" binds to. Every consumer mints a token for it
// explicitly and for the instance's audience, so no pod ever needs the
// auto-mounted default-audience token — which is why the projection turns it off.
// PURE builder: the caller claims ownership.
func barbicanOpenBaoProvisionerServiceAccount(name, namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + barbicanOpenBaoProvisionerSuffix,
			Namespace: namespace,
		},
		AutomountServiceAccountToken: ptr.To(false),
	}
}

// barbicanOpenBaoAuthDelegatorName returns the name of the cluster-scoped
// auth-delegator binding for the instance called name in namespace.
//
// The namespace is part of the NAME, not just of the subject, because the object
// is cluster-scoped while every other name in the ensemble is only namespace-
// unique. Admission allows one ControlPlane per namespace, not one ControlPlane
// name per cluster, so two ControlPlanes called "openstack" in different
// namespaces derive the same instance name — and would collide here. The second
// one would meet a binding it does not own, ensureUnownedOrOwned would refuse it
// on every pass, and BarbicanReady would park on BarbicanOpenBaoError with no
// recovery short of recreating the control plane (metadata.name is immutable).
//
// It enters as a HASH of "namespace/name", not as a "namespace-name" prefix,
// because "-" is legal inside both operands and concatenating them is therefore
// ambiguous: ControlPlane "prod" in namespace "cloud-eu" and ControlPlane
// "eu-prod" in namespace "cloud" would derive the very same binding name and hit
// the collision this function exists to prevent. The separator inside the hashed
// string is "/", which no DNS-1123 name can contain, so the two inputs cannot be
// re-parsed into each other.
//
// It is truncated to EIGHT bytes rather than the four an accidental collision
// would need, because the collision this guards against need not be accidental:
// namespace names are free-form DNS-1123 labels, so in a cluster where tenants
// self-serve their namespaces both operands are attacker-chosen and a colliding
// one is an offline search away — seconds against 32 bits, infeasible against 64.
// Whoever registers the binding first owns it, and the ControlPlane that arrives
// second parks on BarbicanOpenBaoError with no recovery, exactly as above. Sixteen
// hex characters still leave the binding name readable next to the instance it
// belongs to.
func barbicanOpenBaoAuthDelegatorName(name, namespace string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return name + "-" + hex.EncodeToString(sum[:8]) + barbicanOpenBaoAuthDelegatorSuffix
}

// barbicanOpenBaoAuthDelegatorBinding builds the cluster-scoped binding that lets
// the instance run a TokenReview. OpenBao validates every Kubernetes-auth login
// with one, issued under the instance pods' own projected ServiceAccount token;
// the openbao-operator creates that account but grants it nothing, so without this
// binding every login fails with 403 permission denied.
//
// Cluster-scoped objects are collected by no namespace deletion, so this one lives
// and dies by the ownership labels the projection stamps on it.
func barbicanOpenBaoAuthDelegatorBinding(name, namespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanOpenBaoAuthDelegatorName(name, namespace)},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "system:auth-delegator",
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      name + barbicanOpenBaoPodSASuffix,
			Namespace: namespace,
		}},
	}
}

// barbicanOpenBaoTokenRole builds the TokenRequest grant the barbican-operator
// needs: its BarbicanSecretStore controller mints a bound token for the
// provisioner account and presents it to the instance's Kubernetes auth method,
// which takes create on the serviceaccounts/token subresource. Without it the
// store controller reports ProvisioningDenied.
//
// resourceNames is what keeps the grant from being a namespace-wide impersonation
// primitive: the object name of a subresource create is in the request path, so
// RBAC enforces it, and the verb reaches exactly the one account the instance
// trusts.
func barbicanOpenBaoTokenRole(name, namespace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name + barbicanOpenBaoTokenGrantSuffix, Namespace: namespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"serviceaccounts/token"},
			ResourceNames: []string{name + barbicanOpenBaoProvisionerSuffix},
			Verbs:         []string{"create"},
		}},
	}
}

// barbicanOpenBaoTokenRoleBinding binds the TokenRequest Role to the
// barbican-operator's own ServiceAccount, which runs in the operator's namespace
// rather than the service one.
func barbicanOpenBaoTokenRoleBinding(name, namespace, operatorSA, operatorNamespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name + barbicanOpenBaoTokenGrantSuffix, Namespace: namespace},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     name + barbicanOpenBaoTokenGrantSuffix,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      operatorSA,
			Namespace: operatorNamespace,
		}},
	}
}

// ensureBarbicanOpenBaoCluster creates the instance or re-projects the spec of one
// this ControlPlane owns, and returns it so the caller can read its status.
//
// It stays read-modify-write for the reason ensureMariaDB does: the write is gated
// on the LIVE object's ownership, which no pure projection can express. A
// same-named instance the ControlPlane did not create in a namespace it does not
// own is refused rather than reshaped (refuseForeignAdoption). spec.storage is
// never re-projected — the openbao-operator rejects a change to it on an existing
// CR — so it is carried over from the live object.
func (r *ControlPlaneReconciler) ensureBarbicanOpenBaoCluster(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) (*openbaov1alpha1.OpenBaoCluster, error) {
	key := types.NamespacedName{Name: barbicanOpenBaoName(cp), Namespace: cp.BarbicanNamespace()}

	// Resolved before the read, so a cluster whose API-server endpoints cannot be
	// determined writes no instance at all. See resolveAPIServerEndpointIPs for why
	// this fails closed rather than projecting without the egress allowance.
	apiServerIPs, err := r.resolveAPIServerEndpointIPs(ctx, cp)
	if err != nil {
		return nil, err
	}

	instance := &openbaov1alpha1.OpenBaoCluster{}
	err = c.Get(ctx, key, instance)
	switch {
	case apierrors.IsNotFound(err):
		instance.Name = key.Name
		instance.Namespace = key.Namespace
		instance.Spec = r.barbicanOpenBaoClusterSpec(cp, apiServerIPs)
		instance.Spec.Storage = openbaov1alpha1.StorageConfig{Size: barbicanOpenBaoStorageSize}
		if oerr := claimChildOwnership(c, cp, instance, r.Scheme); oerr != nil {
			return nil, fmt.Errorf("claiming ownership of OpenBaoCluster %q: %w", key.Name, oerr)
		}
		if cerr := c.Create(ctx, instance); cerr != nil {
			return nil, fmt.Errorf("creating OpenBaoCluster %q: %w", key.Name, cerr)
		}
	case err != nil:
		return nil, fmt.Errorf("getting OpenBaoCluster %q: %w", key.Name, err)
	default:
		if aerr := refuseForeignAdoption(c, cp, instance, r.Scheme); aerr != nil {
			return nil, aerr
		}
		desired := r.barbicanOpenBaoClusterSpec(cp, apiServerIPs)
		desired.Storage = instance.Spec.Storage
		if !equality.Semantic.DeepEqual(instance.Spec, desired) {
			instance.Spec = desired
			if uerr := c.Update(ctx, instance); uerr != nil {
				return nil, fmt.Errorf("updating owned OpenBaoCluster %q: %w", key.Name, uerr)
			}
		}
	}
	return instance, nil
}

// barbicanOpenBaoClusterSpec derives the instance's spec from the ControlPlane. It
// omits spec.storage, which the caller sets on fresh create alone.
//
// The posture is deliberately the Development profile: a single replica and a
// static seal whose key lives in a Secret beside the instance. The Hardened
// profile forces an external-KMS unseal and three replicas, which is the control
// that defends a production secret store and the thing a dedicated store in a kind
// or proving cluster cannot supply.
//
// deletionPolicy DeletePVCs is not optional here. The instance name is derived
// from the ControlPlane name, so a re-created ControlPlane of the same name meets
// the previous instance's data-<instance>-0 PVC — raft storage initialised under a
// seal key that no longer exists — and never unseals.
func (r *ControlPlaneReconciler) barbicanOpenBaoClusterSpec(
	cp *c5c3v1alpha1.ControlPlane, apiServerIPs []string,
) openbaov1alpha1.OpenBaoClusterSpec {
	name, namespace := barbicanOpenBaoName(cp), cp.BarbicanNamespace()
	return openbaov1alpha1.OpenBaoClusterSpec{
		Profile:  openbaov1alpha1.ProfileDevelopment,
		Version:  defaultOpenBaoVersion,
		Replicas: 1,
		TLS: openbaov1alpha1.TLSConfig{
			Enabled: true,
			// External consumes the two fixed-name cert-manager Secrets. The
			// operator mounts them and waits for both, and never rotates them.
			Mode: openbaov1alpha1.TLSModeExternal,
		},
		// The key material contract is the fixed-name Secret
		// <instance>-unseal-key, data key `key`. unseal.credentialsSecretRef
		// addresses KMS/transit provider credentials and is ignored for a static
		// seal, so it stays unset.
		Unseal:         &openbaov1alpha1.UnsealConfig{Type: "static"},
		DeletionPolicy: openbaov1alpha1.DeletionPolicyDeletePVCs,
		Network: &openbaov1alpha1.NetworkConfig{
			TrustedIngressPeers: barbicanOpenBaoIngressPeers(namespace, r.barbicanOperatorNamespace()),
			// The API server, addressed by the endpoints it actually answers on.
			// The openbao-operator's own detection allows the in-cluster service
			// VIP on port 443, which a CNI enforcing egress against the post-DNAT
			// destination never matches.
			APIServerEndpointIPs: apiServerIPs,
		},
		SelfInit: &openbaov1alpha1.SelfInitConfig{
			Enabled:  true,
			Requests: barbicanOpenBaoSelfInitRequests(name, namespace),
		},
	}
}

// The Kubernetes API server publishes its own endpoints in this EndpointSlice.
// kube-apiserver maintains it directly rather than through the controller manager,
// so it is present on every conformant cluster.
const (
	apiServerEndpointSliceName      = "kubernetes"
	apiServerEndpointSliceNamespace = "default"
)

// resolveAPIServerEndpointIPs returns the addresses the Kubernetes API server
// answers on, deduplicated and sorted.
//
// The openbao-operator renders a deny-by-default NetworkPolicy over the instance
// pods and derives its API-server egress rule from the in-cluster service VIP on
// port 443. A CNI that enforces egress against the post-DNAT destination, which
// kindnet does from kind 0.32 onwards, never matches that rule: the packet it
// inspects is addressed to the API server's own endpoint on port 6443.
// spec.network.apiServerEndpointIPs is the upstream remedy, adding one
// least-privilege egress rule per address on that port. The operator does not
// auto-detect the addresses, because doing so needs cluster permissions outside
// its trust model, so they are resolved here instead.
//
// The addresses are the ones of the cluster the INSTANCE runs on, which for a
// placed Barbican is its target cluster and not the management one. The policy is
// enforced by the CNI there, over pods that reach their own API server, so the
// management cluster's addresses would allow the instance nothing it needs.
//
// The read goes through the uncached reader of that cluster. A cached Get would
// have controller-runtime start a cluster-wide EndpointSlice informer to track a
// single well-known object.
//
// All three failure paths — an unresolvable cluster, an unreadable slice, an empty
// one — return an error rather than a partial answer, and the caller then writes no
// instance at all. An instance that comes up without these rules does not merely
// stay unavailable: its raft auto-join times out, self-init never completes, and
// the partial raft state wedges every later initialisation attempt, recoverable
// only by deleting the instance together with its PVC.
func (r *ControlPlaneReconciler) resolveAPIServerEndpointIPs(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) ([]string, error) {
	key := types.NamespacedName{
		Name:      apiServerEndpointSliceName,
		Namespace: apiServerEndpointSliceNamespace,
	}

	reader, err := commonmulticluster.ResolveChildrenAPIReader(ctx, r.Resolver, r.apiReader(),
		targetClusterRefForNamespace(cp, cp.BarbicanNamespace()))
	if err != nil {
		return nil, err
	}

	slice := &discoveryv1.EndpointSlice{}
	if err := reader.Get(ctx, key, slice); err != nil {
		return nil, fmt.Errorf("getting EndpointSlice %s/%s: %w",
			apiServerEndpointSliceNamespace, apiServerEndpointSliceName, err)
	}

	seen := make(map[string]struct{})
	ips := make([]string, 0, len(slice.Endpoints))
	for i := range slice.Endpoints {
		for _, address := range slice.Endpoints[i].Addresses {
			if _, duplicate := seen[address]; duplicate {
				continue
			}
			seen[address] = struct{}{}
			ips = append(ips, address)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("EndpointSlice %s/%s carries no API server address",
			apiServerEndpointSliceNamespace, apiServerEndpointSliceName)
	}

	// Sorted because ensureBarbicanOpenBaoCluster compares the desired spec against
	// the live one, and the API server guarantees no endpoint order. Unsorted, a
	// reordered read would look like drift and rewrite the instance every pass.
	slices.Sort(ips)
	return ips, nil
}

// barbicanOpenBaoIngressPeers names the only two sources allowed to reach the
// instance's API port. The openbao-operator renders a default-deny NetworkPolicy
// over the instance pods on every reconcile and admits its own pods, the
// backup/restore Jobs, and these peers; everything else is dropped, so an unnamed
// client sees a timeout rather than a refusal.
//
// Neither peer is a wildcard. Both namespaceSelectors pin an exact namespace by
// its kubernetes.io/metadata.name label, so a pod carrying the right
// app.kubernetes.io/name label in some other namespace reaches nothing. Sharing
// the instance's namespace is not an exemption either: the policy isolates the
// instance pods from every source it does not name.
func barbicanOpenBaoIngressPeers(serviceNamespace, operatorNamespace string) []networkingv1.NetworkPolicyPeer {
	return []networkingv1.NetworkPolicyPeer{
		{
			// The barbican-operator's BarbicanSecretStore controller. It logs in
			// with the provisioner's bound ServiceAccount token, mounts the KV
			// path, and mints AppRole secret IDs.
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": operatorNamespace},
			},
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app.kubernetes.io/name": "barbican-operator"},
			},
		},
		{
			// The Barbican API pods. Castellan's vault plugin reads and writes
			// tenant key material over this same port.
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": serviceNamespace},
			},
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app.kubernetes.io/name": "barbican"},
			},
		},
	}
}

// barbicanOpenBaoSelfInitRequests is the complete, permanent configuration of the
// instance: the KV mount Barbican stores tenant key material in, the AppRole
// identity its vault plugin authenticates with, the Kubernetes auth method the
// barbican-operator logs in through, and the two policies scoping each.
//
// self-init is ONE-SHOT. The operator renders these into OpenBao's initialize
// stanzas, which run once against freshly initialised storage, after which OpenBao
// revokes the root token. Changing the list therefore requires recreating the
// instance AND its PVC. Anything a consumer needs at runtime — minting AppRole
// secret IDs, writing secrets — goes through the provisioner role rather than
// through this list.
//
// Order matters: a mount or auth method is enabled before anything writes under
// it. Request names use underscores to satisfy the operator's
// ^[A-Za-z_][A-Za-z0-9_-]*$ name pattern.
//
// The mount, policy, and role names (barbican/, barbican-secretstore, barbican)
// are deliberately identical to the ones provisioned on the shared management
// OpenBao, so a Barbican on a dedicated store and one on an external store differ
// only in which instance they point at.
func barbicanOpenBaoSelfInitRequests(name, namespace string) []openbaov1alpha1.SelfInitRequest {
	return []openbaov1alpha1.SelfInitRequest{
		{
			Name:      "barbican_kv",
			Operation: openbaov1alpha1.SelfInitOperationCreate,
			Path:      "sys/mounts/barbican",
			SecretEngine: &openbaov1alpha1.SelfInitSecretEngine{
				Type:    "kv",
				Options: map[string]string{"version": "2"},
			},
		},
		{
			Name:      "barbican_secretstore_policy",
			Operation: openbaov1alpha1.SelfInitOperationCreate,
			Path:      "sys/policies/acl/barbican-secretstore",
			// No delete on metadata/: in KV v2 that verb permanently destroys
			// every version of a secret and its metadata, which is the only
			// in-store recovery mechanism tenant key material has. Castellan's
			// Vault key manager deletes through the data path, where delete is a
			// recoverable soft delete.
			Policy: &openbaov1alpha1.SelfInitPolicy{Policy: barbicanSecretStorePolicy},
		},
		{
			Name:       "approle_auth",
			Operation:  openbaov1alpha1.SelfInitOperationCreate,
			Path:       "sys/auth/approle",
			AuthMethod: &openbaov1alpha1.SelfInitAuthMethod{Type: "approle"},
		},
		{
			// The AppRole identity Barbican's vault plugin authenticates with.
			// Minting the secret ID stays out of self-init: it is runtime work of
			// the barbican-operator.
			Name:      "barbican_approle_role",
			Operation: openbaov1alpha1.SelfInitOperationCreate,
			Path:      "auth/approle/role/barbican",
			Data: selfInitData(map[string]string{
				"token_policies": "barbican-secretstore",
				"token_ttl":      "1h",
				"token_max_ttl":  "4h",
				// 30 days, not a year: a secret ID is a bearer credential with no
				// use count and no CIDR bound, so its TTL is the only thing that
				// bounds a leaked one.
				"secret_id_ttl": "720h",
			}),
		},
		{
			Name:       "kubernetes_auth",
			Operation:  openbaov1alpha1.SelfInitOperationCreate,
			Path:       "sys/auth/kubernetes",
			AuthMethod: &openbaov1alpha1.SelfInitAuthMethod{Type: "kubernetes"},
		},
		{
			// kubernetes_host is the only required setting: the instance pod's
			// projected ServiceAccount token and the kube root CA are mounted at
			// the standard path and supply the reviewer credentials. The authority
			// to run a TokenReview comes from the auth-delegator binding.
			Name:      "kubernetes_auth_config",
			Operation: openbaov1alpha1.SelfInitOperationUpdate,
			Path:      "auth/kubernetes/config",
			Data:      selfInitData(map[string]string{"kubernetes_host": "https://kubernetes.default.svc"}),
		},
		{
			// The provisioner is the runtime writer: it hands out the barbican
			// AppRole's credentials and can read and write the barbican/ KV mount.
			// Its barbican/ grants are spelled out per KV v2 sub-path rather than
			// as barbican/*, which would also cover destroy/ and the metadata
			// delete that permanently removes every version of a secret.
			Name:      "provisioner_policy",
			Operation: openbaov1alpha1.SelfInitOperationCreate,
			Path:      "sys/policies/acl/provisioner",
			Policy:    &openbaov1alpha1.SelfInitPolicy{Policy: barbicanProvisionerPolicy},
		},
		{
			Name:      "provisioner_k8s_role",
			Operation: openbaov1alpha1.SelfInitOperationCreate,
			Path:      "auth/kubernetes/role/provisioner",
			Data: selfInitData(map[string]string{
				"bound_service_account_names":      name + barbicanOpenBaoProvisionerSuffix,
				"bound_service_account_namespaces": namespace,
				// Without an audience the role accepts any default-audience token
				// of that ServiceAccount, including one auto-mounted into an
				// unrelated pod. Bound to the instance name, only a token
				// deliberately minted for this instance logs in.
				"audience":       name,
				"token_policies": "provisioner",
				"token_ttl":      "1h",
				"token_max_ttl":  "4h",
			}),
		},
	}
}

// barbicanSecretStorePolicy is the OpenBao policy the Barbican AppRole carries.
// It is named after the OpenBao policy it renders (barbican-secretstore).
//
//nolint:gosec // G101 false positive: an OpenBao ACL policy document, not a credential.
const barbicanSecretStorePolicy = `path "barbican/data/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}

path "barbican/metadata/*" {
  capabilities = ["create", "read", "update", "list"]
}
`

// barbicanProvisionerPolicy is the OpenBao policy the barbican-operator's
// Kubernetes-auth login carries.
const barbicanProvisionerPolicy = `path "auth/approle/role/barbican/role-id" {
  capabilities = ["read"]
}

path "auth/approle/role/barbican/secret-id" {
  capabilities = ["create", "update"]
}

path "barbican/data/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}

path "barbican/metadata/*" {
  capabilities = ["create", "read", "update", "list"]
}

path "sys/mounts/barbican" {
  capabilities = ["read", "update"]
}
`

// selfInitData renders a self-init request's payload. json.Marshal cannot fail for
// a map[string]string, so its error is intentionally ignored.
func selfInitData(fields map[string]string) *apiextensionsv1.JSON {
	raw, _ := json.Marshal(fields)
	return &apiextensionsv1.JSON{Raw: raw}
}
