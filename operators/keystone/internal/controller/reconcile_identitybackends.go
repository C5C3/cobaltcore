// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/secrets"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Shared identity-backend vocabulary. The constants live here (the keystone-
// side projection) and are shared with the dedicated
// KeystoneIdentityBackendReconciler so the two controllers agree on the
// volume/mount/file-name contract by construction.
const (
	// domainsVolumeName is the pod volume projecting the per-domain keystone
	// config Secret into the workloads.
	domainsVolumeName = "domains"
	// domainsMountPath is where the per-domain config files are mounted; the
	// rendered keystone.conf points [identity]domain_config_dir here.
	domainsMountPath = "/etc/keystone/domains"

	// conditionTypeIdentityBackendsReady is the aggregated Keystone condition
	// this sub-reconciler drives.
	conditionTypeIdentityBackendsReady = "IdentityBackendsReady"
	// conditionReasonIdentityBackendsNotRequired is set when no backend CR
	// references this Keystone (zero-backend clusters stay Ready).
	conditionReasonIdentityBackendsNotRequired = "IdentityBackendsNotRequired"
	// conditionReasonAllBackendsProjected is set when every attached,
	// DomainReady backend is rendered into the domains Secret.
	conditionReasonAllBackendsProjected = "AllBackendsProjected"
	// conditionReasonWaitingForBackends is set while at least one attached
	// backend is pending (domain not ready, bind Secret missing, or a
	// defensive duplicate-domain skip).
	conditionReasonWaitingForBackends = "WaitingForBackends"

	// conditionTypeDomainReady and conditionTypeConfigProjected are the
	// per-backend conditions owned by the dedicated
	// KeystoneIdentityBackendReconciler (single status writer). The
	// sub-reconciler only READS DomainReady — it gates projection so keystone
	// never loads config for a domain that does not exist yet.
	conditionTypeDomainReady     = "DomainReady"
	conditionTypeConfigProjected = "ConfigProjected"

	// identityBackendFinalizerName blocks KeystoneIdentityBackend deletion
	// until this sub-reconciler has de-projected the backend's config file
	// and the dedicated controller has applied the domain deletion policy.
	identityBackendFinalizerName = "keystone.openstack.c5c3.io/identitybackend"
)

// errControlCharInValue is returned by renderDomainConf when an assembled
// [ldap] option name or value carries a newline or carriage-return. RenderINI
// writes both verbatim as `key = value`, so such a character injects arbitrary
// INI lines. The webhook rejects CR-set keys and values up front, but the
// renderer revalidates as the last line of defense: it is the only gate that
// sees the Secret-sourced bind username/password (which the webhook never
// reads) and the only gate that still runs when a CR bypassed admission
// (direct etcd write / disabled webhook). A poisoned option is a per-backend
// fault, so the caller skips and warns like a missing bind Secret rather than
// failing the whole pipeline.
var errControlCharInValue = errors.New("[ldap] option name or value contains a newline or carriage-return character")

// domainConfFileName returns the per-domain config file name inside the
// domains Secret. Keystone's domain-specific-drivers scanner derives the
// domain from the keystone.<domain>.conf filename, so this MUST follow that
// exact shape.
func domainConfFileName(domain string) string {
	return "keystone." + domain + ".conf"
}

// domainCAFileName returns the sibling CA-bundle file name for a domain. The
// name deliberately does NOT match keystone's keystone.*.conf scan pattern so
// the scanner never tries to parse a PEM as INI.
func domainCAFileName(domain string) string {
	return domain + "-ca.pem"
}

// domainsSecretBaseName returns the content-hashed domains Secret's base name
// for a Keystone CR.
func domainsSecretBaseName(keystone *keystonev1alpha1.Keystone) string {
	return keystone.Name + "-domains"
}

// domainsVolumeAndMount builds the Volume + VolumeMount pair projecting the
// domains Secret into a Keystone workload pod. Callers must only invoke it
// when a domains Secret name is non-empty. DefaultMode 0o400 mirrors the
// fernet-keys / credential-keys volumes: the per-domain files carry LDAP bind
// passwords.
func domainsVolumeAndMount(domainsSecretName string) (corev1.Volume, corev1.VolumeMount) {
	volume := corev1.Volume{
		Name: domainsVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  domainsSecretName,
				DefaultMode: ptr.To(int32(0o400)),
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      domainsVolumeName,
		MountPath: domainsMountPath,
		ReadOnly:  true,
	}
	return volume, mount
}

// reconcileIdentityBackends aggregates every attached, DomainReady
// KeystoneIdentityBackend into one immutable content-hashed domains Secret
// and sets the aggregated IdentityBackendsReady condition on the Keystone CR.
// It returns the Secret's name ("" when nothing is projected) for the
// downstream config/deployment/job builders.
//
// CONTRACT: this step never returns a requeue and never returns an error for
// waiting states (pending domains, missing bind Secrets) — RunPipeline
// short-circuits on non-zero results, and blocking here would deadlock
// first-install: a backend cannot become DomainReady until the Keystone API
// is up, which requires the Deployment this pipeline has not created yet.
// Wake-ups are watch-driven (backend status flips re-enqueue the Keystone).
// Only genuine infrastructure failures (List/render/create errors) surface as
// errors.
func (r *KeystoneReconciler) reconcileIdentityBackends(ctx context.Context, keystone *keystonev1alpha1.Keystone) (string, error) {
	logger := log.FromContext(ctx)

	var backends keystonev1alpha1.KeystoneIdentityBackendList
	if err := r.List(
		ctx, &backends,
		client.InNamespace(keystone.Namespace),
		client.MatchingFields{IdentityBackendKeystoneRefIndexKey: keystone.Name},
	); err != nil {
		return "", fmt.Errorf("listing KeystoneIdentityBackends for %s: %w", keystone.Name, err)
	}

	if len(backends.Items) == 0 {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeIdentityBackendsReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonIdentityBackendsNotRequired,
			Message:            "No KeystoneIdentityBackend references this Keystone",
		})
		return "", nil
	}

	// Sort by name so the rendered Secret content — and therefore its
	// content-hashed name — is deterministic across passes.
	sort.Slice(backends.Items, func(i, j int) bool {
		return backends.Items[i].Name < backends.Items[j].Name
	})

	// Defensive duplicate-domain detection (the webhook normally prevents
	// this; direct etcd writes or a disabled webhook can not). On collision
	// NONE of the colliding set is projected — projecting an arbitrary one
	// would silently pick a winner.
	domainOwners := make(map[string][]string, len(backends.Items))
	for i := range backends.Items {
		b := &backends.Items[i]
		key := strings.ToLower(b.Spec.Domain.Name)
		domainOwners[key] = append(domainOwners[key], b.Name)
	}

	data := map[string][]byte{}
	var pending []string
	for i := range backends.Items {
		backend := &backends.Items[i]

		// A deleting backend is de-projected immediately: the dedicated
		// controller's finalizer waits for exactly this de-projection before
		// it disables/deletes the domain, so config never points at a domain
		// mid-teardown.
		if backend.DeletionTimestamp != nil {
			continue
		}

		if owners := domainOwners[strings.ToLower(backend.Spec.Domain.Name)]; len(owners) > 1 {
			pending = append(pending, fmt.Sprintf("%s (duplicate domain %q also claimed by %s)",
				backend.Name, backend.Spec.Domain.Name, strings.Join(owners, ", ")))
			continue
		}

		// The D-gate: never project config for a domain that does not exist
		// yet — keystone would 500 on domain-scoped requests.
		domainReady := conditions.GetCondition(backend.Status.Conditions, conditionTypeDomainReady)
		if domainReady == nil || domainReady.Status != metav1.ConditionTrue {
			pending = append(pending, fmt.Sprintf("%s (domain %q not ready)", backend.Name, backend.Spec.Domain.Name))
			continue
		}

		conf, caPEM, err := r.renderDomainConf(ctx, keystone.Namespace, backend)
		if err != nil {
			// A missing/incomplete bind Secret or a value carrying a control
			// character (INI-injection guard) is a per-backend
			// misconfiguration, not a pipeline failure: skip the backend,
			// warn loudly, and keep projecting the healthy siblings.
			if secrets.IsMissingSecretOrKey(err) || errors.Is(err, errControlCharInValue) {
				msg := fmt.Sprintf("Skipping identity backend %s: %v", backend.Name, err)
				logger.Info(msg)
				r.Recorder.Event(keystone, corev1.EventTypeWarning, "IdentityBackendSkipped", msg)
				pending = append(pending, fmt.Sprintf("%s (%v)", backend.Name, err))
				continue
			}
			return "", fmt.Errorf("rendering domain config for backend %s: %w", backend.Name, err)
		}
		data[domainConfFileName(backend.Spec.Domain.Name)] = conf
		if caPEM != nil {
			data[domainCAFileName(backend.Spec.Domain.Name)] = caPEM
		}
	}

	var domainsSecretName string
	if len(data) > 0 {
		name, err := config.CreateImmutableSecret(ctx, r.Client, r.Scheme, keystone,
			domainsSecretBaseName(keystone), keystone.Namespace, data)
		if err != nil {
			return "", fmt.Errorf("creating domains Secret: %w", err)
		}
		domainsSecretName = name
	}

	if len(pending) > 0 {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeIdentityBackendsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonWaitingForBackends,
			Message:            "Waiting for identity backends: " + strings.Join(pending, "; "),
		})
		return domainsSecretName, nil
	}

	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               conditionTypeIdentityBackendsReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonAllBackendsProjected,
		Message:            "All attached identity backends are projected",
	})
	return domainsSecretName, nil
}

// renderDomainConf renders the keystone.<domain>.conf content for one
// backend and, when TLS is configured, the CA-bundle PEM projected beside it.
// Only user-set optional fields are rendered so upstream keystone defaults
// apply otherwise.
func (r *KeystoneReconciler) renderDomainConf(ctx context.Context, namespace string, backend *keystonev1alpha1.KeystoneIdentityBackend) (conf, caPEM []byte, err error) {
	l := backend.Spec.LDAP
	if l == nil {
		// The webhook + CEL union rule prevent this; fail loudly rather than
		// rendering an empty driver block if admission was bypassed.
		return nil, nil, fmt.Errorf("backend %s has type %s but no ldap block", backend.Name, backend.Spec.Type)
	}

	bindKey := client.ObjectKey{Namespace: namespace, Name: l.BindCredentialsSecretRef.Name}
	bindUser, err := secrets.GetSecretValue(ctx, r.Client, bindKey, "username")
	if err != nil {
		return nil, nil, err
	}
	bindPassword, err := secrets.GetSecretValue(ctx, r.Client, bindKey, "password")
	if err != nil {
		return nil, nil, err
	}

	ldap := map[string]string{
		"url":          l.URL,
		"suffix":       l.Suffix,
		"user":         bindUser,
		"password":     bindPassword,
		"user_tree_dn": l.Users.TreeDN,
	}
	setIfNonEmpty := func(key, value string) {
		if value != "" {
			ldap[key] = value
		}
	}
	setIfNonEmpty("user_filter", l.Users.Filter)
	setIfNonEmpty("user_objectclass", l.Users.ObjectClass)
	setIfNonEmpty("user_id_attribute", l.Users.IDAttribute)
	setIfNonEmpty("user_name_attribute", l.Users.NameAttribute)
	setIfNonEmpty("user_mail_attribute", l.Users.MailAttribute)

	if g := l.Groups; g != nil {
		ldap["group_tree_dn"] = g.TreeDN
		setIfNonEmpty("group_filter", g.Filter)
		setIfNonEmpty("group_objectclass", g.ObjectClass)
		setIfNonEmpty("group_id_attribute", g.IDAttribute)
		setIfNonEmpty("group_name_attribute", g.NameAttribute)
		setIfNonEmpty("group_member_attribute", g.MemberAttribute)
	}

	if l.TLS != nil {
		caKey := client.ObjectKey{Namespace: namespace, Name: l.TLS.CABundleSecretRef.Name}
		ca, err := secrets.GetSecretValue(ctx, r.Client, caKey, "ca.crt")
		if err != nil {
			return nil, nil, err
		}
		caPEM = []byte(ca)
		ldap["tls_cacertfile"] = domainsMountPath + "/" + domainCAFileName(backend.Spec.Domain.Name)
	}

	if p := l.Pool; p != nil {
		ldap["use_pool"] = fmt.Sprintf("%t", p.Enabled)
		if p.Size != nil {
			ldap["pool_size"] = fmt.Sprintf("%d", *p.Size)
		}
	}

	// ReadOnly (nil = true, the documented default) forces the write-enabling
	// options to false so keystone can never write into the directory. The list
	// is shared with the webhook, which rejects these keys in extraOptions.
	if l.ReadOnly == nil || *l.ReadOnly {
		for _, opt := range keystonev1alpha1.ReadOnlyForcedOptions {
			ldap[opt] = "false"
		}
	}

	// extraOptions last; the webhook denylist guarantees these keys are
	// disjoint from everything set above.
	for k, v := range backend.Spec.ExtraOptions {
		ldap[k] = v
	}

	// Last line of defense against INI injection: RenderINI writes every option
	// verbatim as `key = value`, so a newline in any key OR value injects
	// arbitrary [ldap] options (e.g. re-enabling the write options readOnly
	// forces off). The webhook rejects CR-set keys and values, but the bind
	// username/password come from a Secret it never reads, and a CRD-bypass CR
	// reaches here unvalidated. Fail the render (the caller skips and warns)
	// rather than emitting a corrupted config. Keys are sorted so the error is
	// stable.
	ldapKeys := make([]string, 0, len(ldap))
	for k := range ldap {
		ldapKeys = append(ldapKeys, k)
	}
	sort.Strings(ldapKeys)
	for _, k := range ldapKeys {
		if strings.ContainsAny(k, "\n\r") || strings.ContainsAny(ldap[k], "\n\r") {
			return nil, nil, fmt.Errorf("[ldap] option %q: %w", k, errControlCharInValue)
		}
	}

	sections := map[string]map[string]string{
		"identity": {"driver": "ldap"},
		"ldap":     ldap,
	}
	return []byte(config.RenderINI(sections)), caPEM, nil
}

// pruneStaleDomainsSecrets removes historical immutable domains Secrets past
// the retain count (matching the config-ConfigMap retention). When nothing is
// projected anymore (empty currentName), every historical Secret is removed —
// the last backend detached, so no bind password may linger.
func (r *KeystoneReconciler) pruneStaleDomainsSecrets(ctx context.Context, keystone *keystonev1alpha1.Keystone, domainsSecretName string) error {
	retain := defaultConfigMapRetainCount
	if domainsSecretName == "" {
		retain = 0
	}
	return config.PruneImmutableSecrets(ctx, r.Client, keystone, config.PruneOptions{
		BaseName:    domainsSecretBaseName(keystone),
		Namespace:   keystone.Namespace,
		CurrentName: domainsSecretName,
		Retain:      retain,
	})
}
