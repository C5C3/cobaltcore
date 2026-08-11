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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
)

// Aggregated SecretStoresReady vocabulary. The barbican-side sub-reconciler owns
// this condition on the Barbican CR; the per-store CredentialsReady /
// ProvisioningReady / ConfigProjected conditions stay owned by the dedicated
// BarbicanSecretStore controller.
const (
	// conditionTypeSecretStoresReady is the aggregated Barbican condition this
	// sub-reconciler drives.
	conditionTypeSecretStoresReady = "SecretStoresReady"
	// conditionReasonNoDefaultSecretStore is set when the exactly-one-default
	// invariant is violated (zero or more than one attached, credential-ready
	// store marked isDefault). The projection is invalid and last-good is
	// retained.
	conditionReasonNoDefaultSecretStore = "NoDefaultSecretStore"
	// conditionReasonMultipleOpenBaoStores is set when more than one attached,
	// credential-ready store is of type OpenBao. The [vault_plugin] section is
	// process-global, so a second OpenBao store would silently replace the first
	// one's server, credentials and mount.
	conditionReasonMultipleOpenBaoStores = "MultipleOpenBaoStores"
	// conditionReasonWaitingForSecretStores is set when the default store passed
	// its own credential gate but its credentials Secret cannot be read this pass.
	conditionReasonWaitingForSecretStores = "WaitingForSecretStores"
	// conditionReasonAllStoresProjected is set when every attached store is
	// credential-ready and projected, with a valid default.
	conditionReasonAllStoresProjected = "AllStoresProjected"
)

// secretStoreCAFilePath is the in-pod path the vault plugin's ssl_ca_crt_file
// points at, and the single source of truth for both the rendered option and the
// mount layout: the deployment step derives the mount directory back from it
// with path.Dir, so the file the plugin opens and the directory the bundle lands
// in cannot drift apart. The file name is the contract data key, so a bundle
// stored under it needs no key remapping.
//
// It sits outside barbicanConfigDir rather than under it, for the same reason
// dbTLSMountPath does: barbicanConfigDir is itself the read-only config-Secret
// mount, and the runtime cannot create a nested mountpoint inside a read-only
// Secret tmpfs.
const secretStoreCAFilePath = "/etc/barbican-secret-store-ca/" + barbicanv1alpha1.OpenBaoCAKey

// secretStoreProjection is the result the secret-store sub-reconciler hands
// downstream (the config, deployment and networkpolicy steps). A zero value
// (valid=false) means the aggregation found nothing renderable, in which case
// the config step keeps the last-good artefact the live Deployment mounts.
type secretStoreProjection struct {
	// valid reports whether exactly one attached, credential-ready store is
	// marked isDefault and its configuration could be rendered. Only a valid
	// projection is rendered; otherwise last-good is retained downstream.
	valid bool
	// sections are the rendered barbican.conf secret-store sections: the
	// process-global [secretstore] registry, one [secretstore:<name>] per ready
	// store, and the process-global [vault_plugin].
	sections map[string]map[string]string
	// credentialsSecretName names the Secret carrying the default store's AppRole
	// credentials. The deployment step sources OS_VAULT_PLUGIN__APPROLE_SECRET_ID
	// from it.
	credentialsSecretName string
	// secretIDDigest is the SHA-256 of the AppRole secret ID. The deployment step
	// stamps it into a pod-template annotation so a re-minted secret ID rolls the
	// pods — the secret ID is env-var-consumed, so it only takes effect on a Pod
	// restart.
	secretIDDigest string
	// caSecretName names the Secret holding the PEM bundle that authenticates the
	// OpenBao server; it is empty when the server presents a certificate the pods
	// already trust. The data key is not carried alongside it: SecretNameRefSpec
	// has no key field, so both modes read the bundle under the fixed contract key
	// barbicanv1alpha1.OpenBaoCAKey. The deployment step projects it at
	// secretStoreCAFilePath.
	caSecretName string
	// hosts is the server URL set for the networkpolicy egress step: every
	// attached, credential-ready store on a valid projection, widened by
	// retainedEgressHosts on an invalid one so the pods keep reaching the server
	// the last-good config still points them at.
	hosts []string
	// defaultStore is the single credential-ready default's name, rendered as the
	// [secretstore:<name>] section carrying global_default.
	defaultStore string
}

// reconcileSecretStores aggregates the attached, credential-ready
// BarbicanSecretStores into the secret-store sections of barbican.conf and sets
// the aggregated SecretStoresReady condition on the Barbican CR. It returns the
// projection (zero-valued except for hosts when the aggregation found nothing
// renderable) for the downstream config, deployment and networkpolicy steps.
//
// It gates each store on its CredentialsReady==True condition rather than the
// aggregate Ready: Ready also requires ConfigProjected, which only turns True
// AFTER this step projects the store, so gating on Ready would deadlock.
//
// CONTRACT: this step never returns a requeue and never returns an error for
// waiting states (no default, several defaults, several OpenBao stores, a
// credentials Secret that cannot be read, control-char values) — the
// BarbicanSecretStore watch wakes the parent when a store's status flips. Only
// genuine infrastructure failures (List errors, non-NotFound read failures)
// surface as errors.
func (r *BarbicanReconciler) reconcileSecretStores(ctx context.Context, children client.Client, barbican *barbicanv1alpha1.Barbican) (ctrl.Result, secretStoreProjection, error) {
	// The attached stores are sibling configuration CRs on the management
	// cluster, so this list goes through the embedded client and its field
	// index; only the credentials they name are read on children.
	var stores barbicanv1alpha1.BarbicanSecretStoreList
	if err := r.List(
		ctx, &stores,
		client.InNamespace(barbican.Namespace),
		client.MatchingFields{BarbicanSecretStoreBarbicanRefIndexKey: barbican.Name},
	); err != nil {
		return ctrl.Result{}, secretStoreProjection{}, fmt.Errorf("listing BarbicanSecretStores for %s: %w", barbican.Name, err)
	}

	// Sort by name so the rendered sections — and therefore the config Secret's
	// content hash and the stores_lookup_suffix order — are deterministic across
	// passes.
	sort.Slice(stores.Items, func(i, j int) bool {
		return stores.Items[i].Name < stores.Items[j].Name
	})

	// First pass: collect the credential-ready candidates and their egress hosts.
	//
	// The hosts come from the credential-ready stores ONLY. It is the operator —
	// not the API pods — that authenticates a store against its server, and the
	// operator's egress is governed by its own policy, so gating on readiness
	// deadlocks nothing: the pods first talk to a server once its store is ready
	// and its section is projected, which happens in this same pass. Collecting
	// from every attached store instead would let anyone who can create a
	// BarbicanSecretStore — spec.openBao.server.url is user-supplied and only
	// scheme-checked — add a destination-unrestricted egress port to the API pods
	// of a Barbican they cannot edit.
	//
	// That reasoning covers the initial attach, not the steady state: an invalid
	// projection leaves the pods running against the last-good config, which
	// still points at a store this pass may no longer see as credential-ready.
	// Every invalid-projection return below therefore widens the set through
	// retainedEgressHosts.
	var (
		hosts             []string
		ready             []*barbicanv1alpha1.BarbicanSecretStore
		defaultCandidates []*barbicanv1alpha1.BarbicanSecretStore
	)
	for i := range stores.Items {
		store := &stores.Items[i]
		// A deleting store is de-projected immediately: its section is dropped and
		// its host removed from the egress set on this pass.
		if store.DeletionTimestamp != nil {
			continue
		}
		if !storeCredentialsReady(store) {
			continue
		}
		if host := storeServerURL(store, barbican.Namespace); host != "" {
			hosts = append(hosts, host)
		}
		ready = append(ready, store)
		if store.Spec.IsDefault {
			defaultCandidates = append(defaultCandidates, store)
		}
	}

	// Exactly-one-default rule. Zero or more than one credential-ready default is
	// an invalid projection: SecretStoresReady=False / NoDefaultSecretStore,
	// nothing is re-rendered, and last-good is retained by the downstream config
	// step. hosts is still surfaced so the networkpolicy step tracks every
	// attached store.
	if len(defaultCandidates) != 1 {
		r.markSecretStoresWaiting(barbican, conditionReasonNoDefaultSecretStore, noDefaultSecretStoreMessage(defaultCandidates))
		return ctrl.Result{}, secretStoreProjection{hosts: retainedEgressHosts(barbican, hosts)}, nil
	}
	defaultStore := defaultCandidates[0]

	// The [vault_plugin] section is process-global: every OpenBao store on the
	// same Barbican shares one server URL, one AppRole and one mount. A second
	// ready OpenBao store would therefore not be configured alongside the first,
	// it would silently replace it. The validating webhook rejects the second
	// store at admission; this is the fail-closed backstop for a CR that bypassed
	// it.
	if openBao := countOpenBaoStores(ready); openBao > 1 {
		r.markSecretStoresWaiting(barbican, conditionReasonMultipleOpenBaoStores,
			fmt.Sprintf("%d attached, credential-ready stores are of type %s; the [vault_plugin] section is process-global, so exactly one is supported",
				openBao, barbicanv1alpha1.BarbicanSecretStoreTypeOpenBao))
		return ctrl.Result{}, secretStoreProjection{hosts: retainedEgressHosts(barbican, hosts)}, nil
	}

	projection, err := r.renderStoreSections(ctx, children, barbican.Namespace, ready, defaultStore)
	if err != nil {
		// A missing or unreadable credentials Secret and a value carrying a control
		// character are store-level faults the parent tolerates: the store passed
		// its own credential gate, so the gap is either transient (the store
		// controller re-mints) or the residue of a bypassed admission. Keep
		// last-good and wait for the store watch rather than failing the reconcile.
		// A transient (non-NotFound) client failure is infrastructure and surfaces
		// as an error.
		if secrets.IsMissingSecretOrKey(err) || errors.Is(err, errControlCharInValue) {
			msg := fmt.Sprintf("Skipping secret store %s: %v", defaultStore.Name, err)
			log.FromContext(ctx).Info(msg)
			r.Recorder.Event(barbican, corev1.EventTypeWarning, "BarbicanSecretStoreSkipped", msg)
			r.markSecretStoresWaiting(barbican, conditionReasonWaitingForSecretStores, msg)
			return ctrl.Result{}, secretStoreProjection{hosts: retainedEgressHosts(barbican, hosts)}, nil
		}
		return ctrl.Result{}, secretStoreProjection{}, fmt.Errorf("rendering secret-store sections for %s: %w", defaultStore.Name, err)
	}

	projection.valid = true
	projection.hosts = hosts
	projection.defaultStore = defaultStore.Name

	r.recordProjectedStores(barbican, ready, hosts)
	conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
		Type:               conditionTypeSecretStoresReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: barbican.Generation,
		Reason:             conditionReasonAllStoresProjected,
		Message:            "All attached secret stores are projected",
	})
	return ctrl.Result{}, projection, nil
}

// recordProjectedStores stamps the stores this pass projects — and the server
// URLs they resolved to — onto the Barbican status, and announces the ones that
// dropped out of the recorded set.
//
// The host record is what retainedEgressHosts widens an invalid projection from,
// so it is written here and nowhere else: this function runs on the valid path
// alone, which is the only path that establishes what the running pods are
// configured for.
//
// Detaching a store is not a no-op for the data behind it. Barbican resolves each
// stored secret through the secret_stores row naming the store it was written to,
// so once a [secretstore:<name>] section is gone every secret written under it
// stops resolving. Nothing else says so: the store CR carries no finalizer, its
// deletion is not validated, and the parent re-renders the moment the remaining
// stores form a valid projection — which is exactly when the operator believes
// the change succeeded. The warning therefore fires at the pass that de-projects
// the store, naming the consequence.
func (r *BarbicanReconciler) recordProjectedStores(barbican *barbicanv1alpha1.Barbican, ready []*barbicanv1alpha1.BarbicanSecretStore, hosts []string) {
	projected := make([]string, 0, len(ready))
	for _, store := range ready {
		projected = append(projected, store.Name)
	}
	stillProjected := make(map[string]bool, len(projected))
	for _, name := range projected {
		stillProjected[name] = true
	}
	for _, name := range barbican.Status.ProjectedSecretStores {
		if stillProjected[name] {
			continue
		}
		r.Recorder.Eventf(barbican, corev1.EventTypeWarning, "SecretStoreDetached",
			"Secret store %q is no longer projected: its [%s%s] section is dropped from barbican.conf, so every secret "+
				"barbican wrote through that store stops resolving", name, storeSectionPrefix, name)
	}
	barbican.Status.ProjectedSecretStores = projected
	barbican.Status.ProjectedSecretStoreHosts = hosts
}

// retainedEgressHosts widens the egress set of an INVALID projection to cover
// what the running pods are actually configured for, and returns hosts
// unchanged when there is nothing to add.
//
// The two consumers of an invalid projection disagree on what it means, and the
// pipeline runs both: the config step retains the last-good barbican.conf, so
// the pods keep their [vault_plugin] section and its server URL, while the
// deployment step returns a zero result and lets the parallel group reach the
// networkpolicy step. Egress must therefore track the config the pods run, not
// the projection this pass could build — a dropped host means no OpenBao egress
// rule at all, and PolicyTypes includes Egress, so no rule is a deny.
//
// The gap it closes is a CredentialsReady blip on the default store: the store
// contributes no host to this pass, yet the pods still talk to its server, and
// a rejection the reactive re-mint cooldown declines to re-mint holds that state
// for up to ten minutes. status.projectedSecretStoreHosts is the record of the
// last valid projection, so it holds exactly the URLs that config still points
// the pods at.
//
// It is that RECORD — not the live store specs — that the set is widened from,
// and only recordProjectedStores writes it, on the valid path. Re-deriving the
// URL from the store object as it exists right now would reinstate the
// escalation the credential-ready gate blocks: spec.openBao.server.url is
// mutable and only scheme-checked, so anyone able to write a store could point
// it at a server of their choosing, let the projection go invalid, and have the
// port land in a destination-unrestricted egress rule on API pods they cannot
// edit.
func retainedEgressHosts(barbican *barbicanv1alpha1.Barbican, hosts []string) []string {
	seen := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		seen[host] = true
	}
	for _, host := range barbican.Status.ProjectedSecretStoreHosts {
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	return hosts
}

// markSecretStoresWaiting records a False SecretStoresReady with the given
// reason. Every waiting state of this step keeps the pipeline running, so a
// first install can proceed and the config step can retain last-good.
func (r *BarbicanReconciler) markSecretStoresWaiting(barbican *barbicanv1alpha1.Barbican, reason, message string) {
	conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
		Type:               conditionTypeSecretStoresReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: barbican.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// renderStoreSections renders the process-global [secretstore] registry, one
// [secretstore:<name>] section per credential-ready store, and the
// process-global [vault_plugin] section derived from the default store. It
// returns the projection carrying those sections plus the Secret references the
// deployment step resolves; the caller stamps validity, hosts and the default's
// name onto it.
func (r *BarbicanReconciler) renderStoreSections(
	ctx context.Context, children client.Client, namespace string,
	ready []*barbicanv1alpha1.BarbicanSecretStore, defaultStore *barbicanv1alpha1.BarbicanSecretStore,
) (secretStoreProjection, error) {
	names := make([]string, 0, len(ready))
	sections := map[string]map[string]string{}
	for _, store := range ready {
		names = append(names, store.Name)
		section := map[string]string{
			// Every supported store type is served by barbican's vault plugin: the
			// plugin talks the KV v2 API, which OpenBao and HashiCorp Vault both
			// serve.
			"secret_store_plugin": "vault_plugin",
		}
		if store.Name == defaultStore.Name {
			// Barbican parses the value with oslo.config's boolean parser, which
			// accepts "True". The option is written only on the default, since a
			// second global default is a configuration error barbican refuses to
			// start with.
			section["global_default"] = "True"
		}
		sections[storeSectionPrefix+store.Name] = section
	}
	sections["secretstore"] = map[string]string{
		// The per-store [secretstore:<name>] sections are only read while multiple
		// secret stores are enabled, so the flag is unconditional rather than
		// derived from the number of attached stores.
		"enable_multiple_secret_stores": "true",
		"stores_lookup_suffix":          strings.Join(names, ","),
	}

	// The plugin section is derived from the DEFAULT store alone. It is
	// process-global, and the multiple-OpenBao-stores gate guarantees the default
	// is the only store that can contribute to it.
	projection, section, err := r.renderVaultPlugin(ctx, children, namespace, defaultStore)
	if err != nil {
		return secretStoreProjection{}, err
	}
	sections["vault_plugin"] = section
	projection.sections = sections
	return projection, nil
}

// renderVaultPlugin renders the process-global [vault_plugin] section from one
// store and resolves the Secret references the deployment step needs. It reads
// the AppRole role ID from the store's credentials Secret (a managed store's
// operator-minted <store>-approle, a brownfield store's referenced Secret),
// digests the secret ID for the pod-roll annotation, then merges the store's
// extraOptions WITHOUT overriding an operator key (operator keys win on
// collision — the webhook denylist normally guarantees disjointness, this is the
// fail-closed backstop for a bypassed webhook).
//
// approle_secret_id is deliberately never rendered. The secret ID travels
// exclusively through the OS_VAULT_PLUGIN__APPROLE_SECRET_ID env override the
// deployment step sources from credentialsSecretName. The rendered file is a
// Secret, but it is mounted into every API pod and read back by the store
// controller, while the env override keeps the secret ID's only copy in the
// Secret the store itself owns.
func (r *BarbicanReconciler) renderVaultPlugin(
	ctx context.Context, children client.Client, namespace string, store *barbicanv1alpha1.BarbicanSecretStore,
) (secretStoreProjection, map[string]string, error) {
	spec := store.Spec.OpenBao
	if spec == nil {
		// The schema union rule guarantees spec.openBao for a type-OpenBao store; a
		// bypassed admission leaves nothing to render.
		return secretStoreProjection{}, nil, fmt.Errorf("store %s has type %s but no openBao block", store.Name, store.Spec.Type)
	}

	var projection secretStoreProjection
	if spec.InstanceRef != nil {
		projection.credentialsSecretName = store.Name + approleSecretNameSuffix
		projection.caSecretName = spec.InstanceRef.Name + instanceCASecretSuffix
	} else {
		projection.credentialsSecretName = spec.Server.CredentialsSecretRef.Name
		if spec.Server.CABundleSecretRef != nil {
			projection.caSecretName = spec.Server.CABundleSecretRef.Name
		}
	}

	credsKey := client.ObjectKey{Namespace: namespace, Name: projection.credentialsSecretName}
	roleID, err := secrets.GetSecretValue(ctx, children, credsKey, barbicanv1alpha1.OpenBaoRoleIDKey)
	if err != nil {
		return secretStoreProjection{}, nil, err
	}
	secretID, err := secrets.GetSecretValue(ctx, children, credsKey, barbicanv1alpha1.OpenBaoSecretIDKey)
	if err != nil {
		return secretStoreProjection{}, nil, err
	}
	// The digest is the pod-roll signal only; the secret ID itself never leaves
	// the Secret the deployment step sources the env override from.
	projection.secretIDDigest = secrets.AdminPasswordDigest(secretID)

	vaultURL := storeServerURL(store, namespace)
	section := map[string]string{
		"vault_url": vaultURL,
		// The plugin verifies the server certificate only while use_ssl is on, so
		// the flag follows the resolved URL's scheme rather than being configurable.
		"use_ssl":         fmt.Sprintf("%t", strings.HasPrefix(vaultURL, "https://")),
		"approle_role_id": roleID,
		"kv_mountpoint":   spec.KVMountpoint,
	}
	// Written only when a bundle exists: without one the pods verify the server
	// against their system trust store, and pointing the option at a file nothing
	// mounts would fail every request.
	if projection.caSecretName != "" {
		section["ssl_ca_crt_file"] = secretStoreCAFilePath
	}
	// The namespace header is brownfield-only; omitting it keeps every request at
	// the root namespace the self-init contract provisions.
	if spec.Namespace != "" {
		section["namespace"] = spec.Namespace
	}

	// extraOptions merged without clobbering an operator key: operator keys win on
	// collision. An empty map merges nothing.
	//
	// The denylist check is what makes this a backstop rather than a coincidence.
	// The exists check only covers the keys the operator actually wrote this pass,
	// which leaves the four denied options it never writes — root_token_id,
	// approle_secret_id, crypto_plugin, and ssl_ca_crt_file on a store with no CA
	// bundle — free to pass through on a CR that reached etcd without the
	// validating webhook. root_token_id is the one that matters: barbican's vault
	// plugin prefers a root token over AppRole auth, so it would defeat the
	// mount-scoped AppRole the managed-store design is built on.
	for k, v := range store.Spec.ExtraOptions {
		if _, exists := section[k]; exists {
			continue
		}
		if barbicanv1alpha1.DeniedOpenBaoExtraOption(k) {
			continue
		}
		section[k] = v
	}

	// Last line of defense against INI injection: config.RenderINI writes every
	// option verbatim, so a newline in any key OR value injects arbitrary
	// [vault_plugin] options. The webhook rejects CR-set keys and values, but the
	// role ID comes from a Secret it never reads, and a CRD-bypass CR reaches here
	// unvalidated.
	for k, v := range section {
		if strings.ContainsAny(k, "\n\r") || strings.ContainsAny(v, "\n\r") {
			return secretStoreProjection{}, nil, errControlCharInValue
		}
	}
	return projection, section, nil
}

// errControlCharInValue is returned by renderVaultPlugin when an assembled
// [vault_plugin] option name or value carries a newline or carriage-return.
// config.RenderINI writes both verbatim as `key = value`, so such a character
// injects arbitrary INI lines. A poisoned option keeps the projection invalid,
// so the caller retains last-good rather than rendering the injection.
var errControlCharInValue = errors.New("[vault_plugin] option name or value contains a newline or carriage-return character")

// storeCredentialsReady reports whether a store's CredentialsReady condition is
// present and True — the D-gate the projection uses. It deliberately does NOT
// consult the aggregate Ready (which also requires ConfigProjected, only True
// after this step projects the store), so gating here never deadlocks.
func storeCredentialsReady(store *barbicanv1alpha1.BarbicanSecretStore) bool {
	cond := conditions.GetCondition(store.Status.Conditions, conditionTypeCredentialsReady)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// storeServerURL returns the API base URL of the server a store addresses: the
// derived cluster-local URL of the referenced OpenBaoCluster in managed mode,
// the configured URL in brownfield mode. It is both the rendered vault_url and
// the egress host the networkpolicy step opens.
func storeServerURL(store *barbicanv1alpha1.BarbicanSecretStore, namespace string) string {
	spec := store.Spec.OpenBao
	switch {
	case spec == nil:
		return ""
	case spec.InstanceRef != nil:
		return instanceURL(spec.InstanceRef.Name, namespace)
	case spec.Server != nil:
		return spec.Server.URL
	default:
		return ""
	}
}

// countOpenBaoStores counts the stores of type OpenBao among the
// credential-ready ones.
func countOpenBaoStores(ready []*barbicanv1alpha1.BarbicanSecretStore) int {
	count := 0
	for _, store := range ready {
		if store.Spec.Type == barbicanv1alpha1.BarbicanSecretStoreTypeOpenBao {
			count++
		}
	}
	return count
}

// noDefaultSecretStoreMessage builds the SecretStoresReady=False message for the
// exactly-one-default violation, naming the colliding defaults when there is
// more than one.
func noDefaultSecretStoreMessage(defaults []*barbicanv1alpha1.BarbicanSecretStore) string {
	if len(defaults) == 0 {
		return "No attached, credential-ready secret store is marked isDefault; exactly one is required"
	}
	names := make([]string, 0, len(defaults))
	for _, store := range defaults {
		names = append(names, store.Name)
	}
	return fmt.Sprintf("%d attached, credential-ready secret stores are marked isDefault (%s); exactly one is required",
		len(names), strings.Join(names, ", "))
}
