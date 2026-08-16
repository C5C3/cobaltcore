// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// All envtest coverage of the kubeconfig provider lives in the one test
// function below, on purpose. The provider registers its registration-Secret
// watch under the fixed controller name "kubeconfig-provider", and
// controller-runtime validates controller names against a process-global set. A
// second provider anywhere in this test binary would therefore fail to
// register. One manager, one provider, one function: the scenarios are ordered
// subtests over the shared setup, and the last one builds on the cluster the
// first one engaged.
//
// The logger is captured process-wide for the same reason. The provider takes
// its logger from ctrl.Log at construction time and exposes no seam to inject
// one, so the two subtests that assert on a refused registration read what the
// provider logged through a recording sink installed here.

package multicluster

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	commonenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// TestIntegration_KubeconfigProvider_ScopedCaches runs the provider against a
// management environment holding the registration Secrets and a second
// environment standing in for the target cluster, and walks what the namespaces
// data key does to an engaged cluster: a scoped registration that can only read
// the namespaces it declares, a registration without the key that stays
// cluster-wide, two values the provider refuses to engage on, and a scope
// narrowed in place on a running cluster.
func TestIntegration_KubeconfigProvider_ScopedCaches(t *testing.T) {
	commonenvtest.SkipIfEnvTestUnavailable(t)

	const (
		// clustersNamespace mirrors the --clusters-namespace default the
		// operator binaries pass to the provider.
		clustersNamespace = "c5c3-clusters"

		// One cluster name per registration: the provider keys its clusters by
		// the Secret's name, so a distinct name is a distinct cluster.
		scopedCluster  = "scoped-target"
		wideCluster    = "wide-target"
		emptyCluster   = "empty-scope-target"
		invalidCluster = "invalid-scope-target"

		// Three namespaces on the target, of which the scoped registration
		// declares the first two. The third is what makes the scope observable.
		declaredA  = "tenant-a"
		declaredB  = "tenant-b"
		undeclared = "tenant-c"

		// seedConfigMap exists in all three namespaces, so a failed read can
		// only be the cache refusing the namespace and never a missing object.
		seedConfigMap = "seed"

		// badNamespace is rejected by DNS-1123 label validation on both the
		// underscore and the capitals.
		badNamespace = "Bad_NS"

		// engageTimeout bounds cluster engagement: the provider has to parse the
		// kubeconfig, build a cluster, and sync its cache before Get answers.
		engageTimeout = 60 * time.Second
		pollInterval  = 250 * time.Millisecond

		// refusalTimeout is how long a registration the provider must never
		// engage is watched. It only has to outlast one reconcile.
		refusalTimeout = 5 * time.Second
	)

	g := gomega.NewWithT(t)

	logs := newRecordingLogs()
	ctrl.SetLogger(logr.New(logs.sink()))

	scheme := commonenvtest.BuildScheme()

	// --- Environment B: the target cluster. It holds no registration Secret and
	// runs no manager; everything asserted on it is read through the cluster the
	// provider built.
	targetClient, targetCfg := commonenvtest.StartEnvTestWithConfig(t, scheme, nil)

	for _, ns := range []string{declaredA, declaredB, undeclared} {
		g.Expect(targetClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(gomega.Succeed(), "create namespace %s on the target environment", ns)
		g.Expect(targetClient.Create(context.Background(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: seedConfigMap, Namespace: ns},
		})).To(gomega.Succeed(), "seed the ConfigMap in namespace %s on the target environment", ns)
	}

	kubeconfig, err := commonenvtest.KubeconfigBytes(targetCfg, "target")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "build a kubeconfig for the target environment")

	// --- Environment A: the management cluster, hosting the manager.
	mgmtClient, mgmtCfg := commonenvtest.StartEnvTestWithConfig(t, scheme, nil)

	provider := NewKubeconfigProvider(KubeconfigProviderOptions{
		Namespace: clustersNamespace,
		// Without this the provider builds every target cluster's client on
		// client-go's global scheme. Core kinds are all this test reads, but
		// passing the scheme is what an operator does and keeps the option list
		// non-empty, so the appended scoping option is proven to be additive.
		ClusterOptions: []cluster.Option{func(o *cluster.Options) { o.Scheme = scheme }},
	})

	mcMgr, err := mcmanager.New(mgmtCfg, provider, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "build the multicluster manager")
	g.Expect(provider.SetupWithManager(context.Background(), mcMgr)).To(gomega.Succeed(),
		"register the provider's registration-Secret controller")

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if err := mcMgr.Start(ctx); err != nil {
			t.Errorf("the multicluster manager exited with an error: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

	// The provider watches this namespace for registration Secrets.
	g.Expect(mgmtClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: clustersNamespace},
	})).To(gomega.Succeed(), "create the clusters namespace on the management environment")

	// register creates a registration Secret. A nil namespaces value leaves the
	// key out entirely, which is the cluster-wide registration.
	register := func(name string, namespaces []byte) {
		t.Helper()

		data := map[string][]byte{KubeconfigDataKey: kubeconfig}
		if namespaces != nil {
			data[NamespacesDataKey] = namespaces
		}
		gomega.NewWithT(t).Expect(mgmtClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: clustersNamespace,
				Labels:    map[string]string{KubeconfigSecretLabel: "true"},
			},
			Data: data,
		})).To(gomega.Succeed(), "create the registration Secret for cluster %s", name)
	}

	// engagedClient resolves the cluster and returns its cached client. It
	// resolves on every call because a re-engaged cluster is a new one.
	engagedClient := func(gi gomega.Gomega, name string) client.Client {
		cl, err := provider.Get(ctx, mcruntime.ClusterName(name))
		gi.Expect(err).NotTo(gomega.HaveOccurred(), "cluster %s should be engaged", name)
		return cl.GetClient()
	}

	seedKey := func(namespace string) client.ObjectKey {
		return client.ObjectKey{Namespace: namespace, Name: seedConfigMap}
	}

	t.Run("a scoped registration engages a cluster that reads only its namespaces", func(t *testing.T) {
		g := gomega.NewWithT(t)

		register(scopedCluster, []byte(declaredA+","+declaredB))

		g.Eventually(func() error {
			_, err := provider.Get(ctx, mcruntime.ClusterName(scopedCluster))
			return err
		}, engageTimeout, pollInterval).Should(gomega.Succeed(),
			"the provider should engage the target cluster from its registration Secret")

		cl := engagedClient(g, scopedCluster)

		// Both declared namespaces answer, so the scoped cache is not simply
		// broken: it holds the informers the registration asked for.
		for _, ns := range []string{declaredA, declaredB} {
			list := &corev1.ConfigMapList{}
			g.Expect(cl.List(ctx, list, client.InNamespace(ns))).To(gomega.Succeed(),
				"listing ConfigMaps in the declared namespace %s should succeed", ns)
			g.Expect(configMapNames(list)).To(gomega.ContainElement(seedConfigMap),
				"the seeded ConfigMap in %s should be in the listing", ns)
		}

		// The undeclared one does not, and the object is there, so this is the
		// cache refusing a namespace and not a missing ConfigMap.
		err := cl.Get(ctx, seedKey(undeclared), &corev1.ConfigMap{})
		g.Expect(err).To(gomega.HaveOccurred(),
			"reading the undeclared namespace %s should fail", undeclared)
		g.Expect(err.Error()).To(gomega.ContainSubstring("unknown namespace for the cache"))
		g.Expect(targetClient.Get(ctx, seedKey(undeclared), &corev1.ConfigMap{})).To(gomega.Succeed(),
			"the ConfigMap the scoped cluster refused to read does exist on the target environment")
	})

	t.Run("a registration without the namespaces key stays cluster-wide", func(t *testing.T) {
		g := gomega.NewWithT(t)

		// The compatibility path: same target environment, second cluster name,
		// no namespaces key. What the scoped cluster refused, this one reads.
		register(wideCluster, nil)

		g.Eventually(func() error {
			_, err := provider.Get(ctx, mcruntime.ClusterName(wideCluster))
			return err
		}, engageTimeout, pollInterval).Should(gomega.Succeed(),
			"a registration without the namespaces key should engage as before")

		g.Expect(engagedClient(g, wideCluster).Get(ctx, seedKey(undeclared), &corev1.ConfigMap{})).
			To(gomega.Succeed(), "a cluster-wide cache should read every namespace")
	})

	t.Run("an empty namespaces value never engages", func(t *testing.T) {
		g := gomega.NewWithT(t)

		register(emptyCluster, []byte("  "))

		g.Eventually(logs.contains, engageTimeout, pollInterval).
			WithArguments("namespaces key present but empty").Should(gomega.BeTrue(),
			"the provider should log why it refused the registration")

		g.Consistently(func() error {
			_, err := provider.Get(ctx, mcruntime.ClusterName(emptyCluster))
			return err
		}, refusalTimeout, pollInterval).Should(gomega.MatchError(mcruntime.ErrClusterNotFound),
			"a cluster whose scope cannot be read must stay unresolvable, so a CR naming it "+
				"reports TargetClusterUnavailable")
	})

	t.Run("a namespaces entry that is no DNS-1123 label never engages", func(t *testing.T) {
		g := gomega.NewWithT(t)

		register(invalidCluster, []byte(declaredA+","+badNamespace))

		g.Eventually(logs.contains, engageTimeout, pollInterval).
			WithArguments(badNamespace).Should(gomega.BeTrue(),
			"the log should name the entry that cannot be a namespace")

		g.Consistently(func() error {
			_, err := provider.Get(ctx, mcruntime.ClusterName(invalidCluster))
			return err
		}, refusalTimeout, pollInterval).Should(gomega.MatchError(mcruntime.ErrClusterNotFound),
			"one unusable entry refuses the whole registration")
	})

	// Stripping the key is how a registration is revoked in place — the Secret
	// stays, so a `kubectl patch --type=json remove` or an ESO source that lost
	// the key both land here. Leaving the last good cluster engaged would go on
	// running every informer and writing every child under credentials the
	// operator has withdrawn, and the run before this fix logged "skipping" while
	// doing exactly that.
	t.Run("a registration that loses its kubeconfig disengages the cluster", func(t *testing.T) {
		g := gomega.NewWithT(t)

		secret := &corev1.Secret{}
		key := client.ObjectKey{Namespace: clustersNamespace, Name: wideCluster}
		g.Expect(mgmtClient.Get(ctx, key, secret)).To(gomega.Succeed())
		delete(secret.Data, KubeconfigDataKey)
		g.Expect(mgmtClient.Update(ctx, secret)).To(gomega.Succeed(),
			"strip the kubeconfig off the registration of the running cluster")

		g.Eventually(func() error {
			_, err := provider.Get(ctx, mcruntime.ClusterName(wideCluster))
			return err
		}, engageTimeout, pollInterval).Should(gomega.MatchError(mcruntime.ErrClusterNotFound),
			"a registration that carries no kubeconfig must disengage the cluster it engaged")
	})

	t.Run("narrowing the namespaces value re-engages the cluster", func(t *testing.T) {
		g := gomega.NewWithT(t)

		// Only the namespaces key is written; the kubeconfig is byte-identical
		// to the one the cluster was engaged with. A hash over the kubeconfig
		// alone would skip this Secret as unchanged and the cluster would keep
		// the scope it no longer has.
		secret := &corev1.Secret{}
		key := client.ObjectKey{Namespace: clustersNamespace, Name: scopedCluster}
		g.Expect(mgmtClient.Get(ctx, key, secret)).To(gomega.Succeed())
		secret.Data[NamespacesDataKey] = []byte(declaredA)
		g.Expect(mgmtClient.Update(ctx, secret)).To(gomega.Succeed(),
			"narrow the declared namespaces of the running cluster")

		g.Eventually(func(gi gomega.Gomega) {
			cl := engagedClient(gi, scopedCluster)

			err := cl.List(ctx, &corev1.ConfigMapList{}, client.InNamespace(declaredB))
			gi.Expect(err).To(gomega.HaveOccurred(),
				"the namespace dropped from the registration should have left the cache")
			gi.Expect(err.Error()).To(gomega.ContainSubstring("unknown namespace for the cache"))

			gi.Expect(cl.Get(ctx, seedKey(declaredA), &corev1.ConfigMap{})).To(gomega.Succeed(),
				"the namespace that stayed declared should still be readable")
		}, engageTimeout, pollInterval).Should(gomega.Succeed())
	})
}

// configMapNames returns the names in a ConfigMap listing.
func configMapNames(list *corev1.ConfigMapList) []string {
	names := make([]string, 0, len(list.Items))
	for _, cm := range list.Items {
		names = append(names, cm.Name)
	}
	return names
}

// recordingLogs keeps every line logged through the sink it hands out, so a
// subtest can assert on what the provider reported about a registration it
// refused. The provider logs those refusals rather than returning them: no
// retry turns an unusable Secret into a usable one, so the log is the only
// place the reason surfaces.
type recordingLogs struct {
	mu    sync.Mutex
	lines []string
}

func newRecordingLogs() *recordingLogs {
	return &recordingLogs{}
}

// sink returns the logr.LogSink writing into this recorder.
func (r *recordingLogs) sink() logr.LogSink {
	return recordingSink{logs: r}
}

// contains reports whether any recorded line contains substring.
func (r *recordingLogs) contains(substring string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, line := range r.lines {
		if strings.Contains(line, substring) {
			return true
		}
	}
	return false
}

// record appends one rendered line. Message, error text, and key/value pairs go
// into the same string, because a subtest cares that the reason was reported
// and not which field of the entry it landed in.
func (r *recordingLogs) record(msg string, err error, keysAndValues []any) {
	var line strings.Builder
	line.WriteString(msg)
	if err != nil {
		line.WriteString(": " + err.Error())
	}
	for _, kv := range keysAndValues {
		fmt.Fprintf(&line, " %v", kv)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line.String())
}

// recordingSink is the logr.LogSink half of recordingLogs. It keeps no state of
// its own, so the WithName and WithValues copies every controller-runtime
// component makes all write into the same recorder.
type recordingSink struct {
	logs *recordingLogs
}

func (s recordingSink) Init(logr.RuntimeInfo) {}

func (s recordingSink) Enabled(int) bool { return true }

func (s recordingSink) Info(_ int, msg string, keysAndValues ...any) {
	s.logs.record(msg, nil, keysAndValues)
}

func (s recordingSink) Error(err error, msg string, keysAndValues ...any) {
	s.logs.record(msg, err, keysAndValues)
}

func (s recordingSink) WithValues(...any) logr.LogSink { return s }

func (s recordingSink) WithName(string) logr.LogSink { return s }
