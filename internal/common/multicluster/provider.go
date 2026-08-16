// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

// This file is derived from the kubeconfig provider of
// sigs.k8s.io/multicluster-runtime (providers/kubeconfig), which is licensed
// Apache-2.0 like this repository. The engagement machinery is kept close to
// that original — same Secret label, same data key, same controller name, same
// hash-based re-engagement — so an upstream change stays easy to follow here.
// The namespaces data key is the one thing this copy adds, and it is the reason
// the copy exists: the upstream provider builds every cluster with a
// cluster-wide cache, which requires cluster-wide read on every target.

const (
	// KubeconfigSecretLabel marks a Secret in the watched namespace as a
	// target-cluster registration; only the value "true" registers one. Both the
	// key and the value are the upstream provider's, so a Secret written for it
	// is picked up here unchanged.
	KubeconfigSecretLabel = "sigs.k8s.io/multicluster-runtime-kubeconfig" //nolint:gosec // G101 false positive: label key, not a credential.

	// KubeconfigDataKey is the registration Secret's data key holding the
	// kubeconfig the cluster is built from.
	KubeconfigDataKey = "kubeconfig"

	// NamespacesDataKey is the registration Secret's optional data key holding
	// the comma-separated namespaces the engaged cluster's cache covers. Absent,
	// the cluster is cached cluster-wide, which is what the upstream provider
	// always does and what every registration written before this key existed
	// keeps doing.
	//
	// Present, it is the credential's own scope written down: a target-cluster
	// ServiceAccount that is only bound in two namespaces cannot answer a
	// cluster-wide LIST, and an informer that issues one never syncs, so the
	// operator would hang on a cache it is not allowed to fill. Naming the
	// namespaces here turns those LIST/WATCH calls into namespaced ones, which
	// namespace-scoped RBAC on the target does cover. Cluster-scoped kinds are
	// unaffected — they have no namespace to scope to — so a registration that
	// names namespaces still needs cluster-scoped read for whatever the operator
	// reads outside a namespace.
	NamespacesDataKey = "namespaces"
)

// cacheSyncTimeout bounds how long one engagement waits for a target cluster's
// cache. A target whose credentials cannot answer the LIST an informer issues
// never syncs — client-go retries that forbidden LIST forever — and the wait
// runs on the registration controller's single worker. Unbounded, one such
// cluster stops every other registration in the namespace from ever being
// processed, deletes included, for the lifetime of the process. Bounded, the
// engagement fails, the worker is freed, and controller-runtime retries the
// registration with backoff, so a cluster that comes back is picked up.
//
// Two minutes is controller-runtime's own cache-sync timeout, and a target that
// answers at all is engaged in a fraction of it.
const cacheSyncTimeout = 2 * time.Minute

// namespacesHashSeparator separates the kubeconfig from the raw namespaces
// value in the engagement hash. Without it a kubeconfig ending in the bytes
// another registration's namespaces value starts with would hash the same, and
// a NUL byte is one no kubeconfig and no namespace name contains.
var namespacesHashSeparator = []byte{0}

var _ mcruntime.Provider = &KubeconfigProvider{}

// KubeconfigProviderOptions configures NewKubeconfigProvider.
type KubeconfigProviderOptions struct {
	// Namespace is the namespace on the management cluster the registration
	// Secrets are read from.
	Namespace string

	// ClusterOptions are applied to every cluster the provider builds. A
	// registration that names namespaces gets one more option appended, which
	// restricts that cluster's cache to them.
	ClusterOptions []cluster.Option
}

// KubeconfigProvider engages one target cluster per registration Secret in the
// configured namespace, and disengages it when that Secret is deleted or stops
// carrying a usable kubeconfig.
type KubeconfigProvider struct {
	opts     KubeconfigProviderOptions
	log      logr.Logger
	lock     sync.RWMutex // protects clusters and indexers
	clusters map[mcruntime.ClusterName]activeCluster
	indexers []clusterIndex
	mgr      mcmanager.Manager
}

// NewKubeconfigProvider returns a provider watching opts.Namespace for
// registration Secrets. It engages nothing until SetupWithManager registers its
// Secret controller on a manager.
func NewKubeconfigProvider(opts KubeconfigProviderOptions) *KubeconfigProvider {
	return &KubeconfigProvider{
		opts:     opts,
		log:      ctrl.Log.WithName("kubeconfig-provider"),
		clusters: map[mcruntime.ClusterName]activeCluster{},
	}
}

// activeCluster is one engaged cluster: the cluster itself, the context whose
// cancellation stops it, and the hash of the registration it was built from.
type activeCluster struct {
	Cluster cluster.Cluster
	Cancel  context.CancelFunc
	Hash    string
}

// clusterIndex is one field index, kept so a cluster engaged later gets the
// indexes that were registered before it existed.
type clusterIndex struct {
	object       client.Object
	field        string
	extractValue client.IndexerFunc
}

// Get returns the cluster registered under clusterName, or
// mcruntime.ErrClusterNotFound when none is. A registration the provider
// refused — an unparseable kubeconfig, an unusable namespaces value — is
// indistinguishable from an absent one here, which is what makes a CR naming it
// report TargetClusterUnavailable.
func (p *KubeconfigProvider) Get(ctx context.Context, clusterName mcruntime.ClusterName) (cluster.Cluster, error) {
	ac, exists := p.getCluster(clusterName)
	if !exists {
		return nil, mcruntime.ErrClusterNotFound
	}
	return ac.Cluster, nil
}

// SetupWithManager registers the registration-Secret controller on mgr's local
// manager. It has to run before the operator's own controllers, so cluster
// engagement precedes the first reconcile that resolves a target-cluster ref.
func (p *KubeconfigProvider) SetupWithManager(ctx context.Context, mgr mcmanager.Manager) error {
	p.log.Info("starting the kubeconfig provider", "namespace", p.opts.Namespace)

	if mgr == nil {
		return errors.New("multicluster manager is nil")
	}
	p.mgr = mgr

	localMgr := mgr.GetLocalManager()
	if localMgr == nil {
		return errors.New("local manager is nil")
	}

	err := ctrl.NewControllerManagedBy(localMgr).
		For(&corev1.Secret{}, builder.WithPredicates(predicate.NewPredicateFuncs(
			func(obj client.Object) bool {
				return obj.GetNamespace() == p.opts.Namespace &&
					obj.GetLabels()[KubeconfigSecretLabel] == "true"
			},
		))).
		Named("kubeconfig-provider").
		Complete(p)
	if err != nil {
		return fmt.Errorf("creating the registration Secret controller: %w", err)
	}

	return nil
}

// Reconcile brings the engaged clusters in line with one registration Secret.
// The cluster's name is the Secret's name.
//
// A registration that cannot be honored disengages the cluster rather than
// leaving the last good one running: the Secret is the operator's whole
// statement about that cluster, and half-honoring a statement that no longer
// parses would keep writing to a cluster under credentials or a scope the
// operator has withdrawn. It is logged and not returned as an error, because no
// number of retries turns an unusable Secret into a usable one; the next write
// to the Secret is what triggers the next attempt.
func (p *KubeconfigProvider) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	secret, err := p.getSecret(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if secret == nil {
		p.removeCluster(mcruntime.ClusterName(req.Name))
		return ctrl.Result{}, nil
	}

	clusterName := mcruntime.ClusterName(secret.Name)
	logger := p.log.WithValues("cluster", clusterName,
		"secret", fmt.Sprintf("%s/%s", secret.Namespace, secret.Name))

	// Only reached when a finalizer holds the Secret; an unfinalized delete
	// arrives as the not-found above.
	if secret.DeletionTimestamp != nil {
		p.removeCluster(clusterName)
		return ctrl.Result{}, nil
	}

	kubeconfigData, ok := secret.Data[KubeconfigDataKey]
	if !ok || len(kubeconfigData) == 0 {
		// Disengaged rather than left running, for the reason the doc comment
		// gives: stripping the key is how a registration is revoked in place, and
		// keeping the last good cluster would go on writing children under
		// credentials the operator has withdrawn.
		logger.Error(nil, "registration Secret carries no kubeconfig, disengaging", "key", KubeconfigDataKey)
		p.removeCluster(clusterName)
		return ctrl.Result{}, nil
	}

	rawNamespaces, scoped := secret.Data[NamespacesDataKey]
	var namespaces []string
	if scoped {
		if namespaces, err = parseNamespaces(rawNamespaces); err != nil {
			logger.Error(err, "registration Secret carries an unusable namespaces key, not engaging",
				"key", NamespacesDataKey)
			p.removeCluster(clusterName)
			return ctrl.Result{}, nil
		}
	}

	// The namespaces value is part of the hash, so narrowing or widening the
	// scope re-engages the cluster the same way rotating the kubeconfig does. A
	// cache's namespaces are fixed when it is built, so there is no other way to
	// apply the new scope than to build the cluster again.
	hash := registrationHash(kubeconfigData, rawNamespaces, scoped)

	if existing, engaged := p.getCluster(clusterName); engaged {
		if existing.Hash == hash {
			logger.V(1).Info("cluster is already engaged from this registration, skipping")
			return ctrl.Result{}, nil
		}
		logger.Info("registration changed, re-engaging the cluster")
		p.removeCluster(clusterName)
	}

	if err := p.createAndEngageCluster(ctx, clusterName, kubeconfigData, namespaces, hash, logger); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// IndexField indexes a field on every cluster, engaged and future.
//
// The engaged ones are indexed off a snapshot rather than under the lock:
// IndexField on a started cache waits for that kind's informer to sync, and one
// unreachable target holding the provider's lock for that wait would block
// setCluster, addIndexer, and above all the removeCluster that would cancel the
// offending cluster and end the wait. A cluster disengaged between the snapshot
// and the call has its context cancelled, so its own index attempt fails rather
// than running on.
func (p *KubeconfigProvider) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	p.addIndexer(clusterIndex{object: obj, field: field, extractValue: extractValue})

	p.lock.RLock()
	engaged := make(map[mcruntime.ClusterName]activeCluster, len(p.clusters))
	maps.Copy(engaged, p.clusters)
	p.lock.RUnlock()

	for name, ac := range engaged {
		if err := ac.Cluster.GetFieldIndexer().IndexField(ctx, obj, field, extractValue); err != nil {
			return fmt.Errorf("indexing field %q on cluster %q: %w", field, name, err)
		}
	}

	return nil
}

// parseNamespaces turns the raw namespaces value into the namespaces the
// cluster's cache is restricted to. Every entry is trimmed, and an entry that
// is not a DNS-1123 label cannot name a namespace, so the whole value is
// refused rather than silently dropping the entry: dropping one would engage
// the cluster with a scope narrower than the operator asked for, and the
// missing namespace would surface much later as an unrelated cache error.
func parseNamespaces(raw []byte) ([]string, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return nil, errors.New("namespaces key present but empty")
	}

	entries := strings.Split(string(raw), ",")
	namespaces := make([]string, 0, len(entries))
	for _, entry := range entries {
		namespace := strings.TrimSpace(entry)
		if msgs := validation.IsDNS1123Label(namespace); len(msgs) > 0 {
			return nil, fmt.Errorf("namespaces entry %q is not a valid namespace name: %s",
				namespace, strings.Join(msgs, "; "))
		}
		namespaces = append(namespaces, namespace)
	}
	return namespaces, nil
}

// registrationHash fingerprints everything about a registration Secret that a
// cluster has to be rebuilt for. scoped tells a registration without the
// namespaces key from one whose value happens to be empty, so adding the key to
// a cluster-wide registration re-engages it too.
func registrationHash(kubeconfig, namespaces []byte, scoped bool) string {
	h := sha256.New()
	_, _ = h.Write(kubeconfig)
	if scoped {
		_, _ = h.Write(namespacesHashSeparator)
		_, _ = h.Write(namespaces)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// namespaceScopedOption restricts a cluster's cache to namespaces, so its
// informers issue namespaced LIST/WATCH calls.
func namespaceScopedOption(namespaces []string) cluster.Option {
	return func(o *cluster.Options) {
		scoped := make(map[string]cache.Config, len(namespaces))
		for _, namespace := range namespaces {
			scoped[namespace] = cache.Config{}
		}
		o.Cache.DefaultNamespaces = scoped
	}
}

// refuseUnsupportedCredentials rejects a registration kubeconfig that has this
// operator's POD supply the credential instead of the Secret: a plugin the pod
// forks, or a path the pod reads.
//
// A registration Secret is data, not a program to run. clientcmd honors
// users[].user.exec and users[].user.auth-provider, and client-go would run the
// named binary inside this operator's pod the first time the cluster's transport
// issues a request — so create on Secrets in the clusters namespace, a far weaker
// grant than the operator's own, would be code execution with the operator's
// ServiceAccount token and every engaged cluster's credentials in reach. A target
// cluster authenticates with a bearer token or a client certificate; neither needs
// a plugin.
//
// The file-backed fields are the same hole without the fork. users[].user.tokenFile,
// client-certificate and client-key, and clusters[].cluster.certificate-authority
// are resolved against THIS pod's filesystem, so a registration naming
// /var/run/secrets/kubernetes.io/serviceaccount/token and a server of the author's
// choosing hands the operator's own ServiceAccount token to that server on the
// first LIST — and that token reads every other registration Secret in the
// namespace, which is the whole fleet's kubeconfigs for one namespaced create. The
// credential a target cluster is reached with belongs in the Secret, inline.
//
// It runs on the DECODED kubeconfig, and that is the load-bearing part: building a
// rest.Config already ACTS on those paths — clientcmd os.ReadFile()s tokenFile and
// os.Open()s client-certificate and certificate-authority while it resolves the
// credential — so a guard reading the finished rest.Config is unreachable for
// exactly the inputs whose read never returns. tokenFile: /dev/zero grows
// os.ReadFile's buffer until the container hits its memory limit and is OOM-killed,
// and a named pipe with no writer blocks the registration controller's single
// worker for the lifetime of the process; both come back on the next reconcile of
// the same Secret, so neither is something a restart or a backoff recovers from.
// clientcmd.Load only unmarshals.
//
// Every users[] and clusters[] entry is checked rather than the current context's
// alone: which context is current is the Secret author's choice too, and this
// operator engages one cluster per Secret, so a registration carrying a second
// credential has nothing legitimate to use it for.
func refuseUnsupportedCredentials(clusterName mcruntime.ClusterName, kubeconfig []byte) error {
	raw, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return fmt.Errorf("parsing the kubeconfig of cluster %q: %w", clusterName, err)
	}

	fileBacked := func() error {
		return fmt.Errorf("refusing the kubeconfig of cluster %q: file-backed credentials "+
			"(tokenFile, client-certificate, client-key, certificate-authority) are not supported, "+
			"inline the token, certificate and CA data", clusterName)
	}

	for _, authInfo := range raw.AuthInfos {
		if authInfo.Exec != nil || authInfo.AuthProvider != nil {
			return fmt.Errorf("refusing the kubeconfig of cluster %q: exec and auth-provider credentials "+
				"are not supported, use a bearer token or a client certificate", clusterName)
		}
		if authInfo.TokenFile != "" || authInfo.ClientCertificate != "" || authInfo.ClientKey != "" {
			return fileBacked()
		}
	}
	for _, cl := range raw.Clusters {
		if cl.CertificateAuthority != "" {
			return fileBacked()
		}
	}

	return nil
}

// createAndEngageCluster builds the cluster from the kubeconfig, starts it,
// waits for its cache, and hands it to the manager. namespaces is empty for a
// registration without the namespaces key, and the cache stays cluster-wide.
func (p *KubeconfigProvider) createAndEngageCluster(
	ctx context.Context,
	clusterName mcruntime.ClusterName,
	kubeconfig []byte,
	namespaces []string,
	hash string,
	logger logr.Logger,
) error {
	if err := refuseUnsupportedCredentials(clusterName, kubeconfig); err != nil {
		return err
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("parsing the kubeconfig of cluster %q: %w", clusterName, err)
	}

	// Appended last, so the scope the registration asked for wins over a
	// DefaultNamespaces one of the static options happened to set.
	opts := p.opts.ClusterOptions
	if len(namespaces) > 0 {
		opts = append(append([]cluster.Option{}, opts...), namespaceScopedOption(namespaces))
	}

	logger.Info("building the cluster from its registration Secret", "namespaces", namespaces)
	cl, err := cluster.New(restConfig, opts...)
	if err != nil {
		return fmt.Errorf("building cluster %q: %w", clusterName, err)
	}

	if err := p.applyIndexers(ctx, cl); err != nil {
		return err
	}

	// Cancelling this context is what stops the cluster when it is removed.
	clusterCtx, cancel := context.WithCancel(ctx)

	go func() {
		if err := cl.Start(clusterCtx); err != nil {
			logger.Error(err, "cluster stopped with an error")
		}
	}()

	syncCtx, cancelSync := context.WithTimeout(clusterCtx, cacheSyncTimeout)
	defer cancelSync()
	if !cl.GetCache().WaitForCacheSync(syncCtx) {
		cancel()
		return fmt.Errorf("waiting for the cache of cluster %q to sync: the credentials likely do not cover "+
			"the namespaces the registration declares", clusterName)
	}

	p.setCluster(clusterName, activeCluster{Cluster: cl, Cancel: cancel, Hash: hash})

	if err := p.mgr.Engage(clusterCtx, clusterName, cl); err != nil {
		p.removeCluster(clusterName)
		return fmt.Errorf("engaging cluster %q: %w", clusterName, err)
	}

	logger.Info("cluster engaged")
	return nil
}

// applyIndexers replays the registered field indexes on a freshly built
// cluster.
func (p *KubeconfigProvider) applyIndexers(ctx context.Context, cl cluster.Cluster) error {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, idx := range p.indexers {
		if err := cl.GetFieldIndexer().IndexField(ctx, idx.object, idx.field, idx.extractValue); err != nil {
			return fmt.Errorf("indexing field %q: %w", idx.field, err)
		}
	}

	return nil
}

// getSecret reads the registration Secret, answering (nil, nil) for one that is
// gone.
func (p *KubeconfigProvider) getSecret(ctx context.Context, key client.ObjectKey) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := p.mgr.GetLocalManager().GetClient().Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading registration Secret %s: %w", key, err)
	}
	return secret, nil
}

// getCluster returns the engaged cluster under clusterName.
func (p *KubeconfigProvider) getCluster(clusterName mcruntime.ClusterName) (activeCluster, bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	ac, exists := p.clusters[clusterName]
	return ac, exists
}

// setCluster records an engaged cluster.
func (p *KubeconfigProvider) setCluster(clusterName mcruntime.ClusterName, ac activeCluster) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.clusters[clusterName] = ac
}

// addIndexer records a field index for the clusters engaged from here on.
func (p *KubeconfigProvider) addIndexer(idx clusterIndex) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.indexers = append(p.indexers, idx)
}

// removeCluster disengages the cluster under clusterName, if one is engaged.
// The context is cancelled outside the lock, because that is what stops the
// cluster and every controller engaged on it.
func (p *KubeconfigProvider) removeCluster(clusterName mcruntime.ClusterName) {
	p.lock.Lock()
	ac, exists := p.clusters[clusterName]
	if !exists {
		p.lock.Unlock()
		return
	}
	delete(p.clusters, clusterName)
	p.lock.Unlock()

	ac.Cancel()
	p.log.Info("cluster disengaged", "cluster", clusterName)
}
