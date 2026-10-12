// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// All envtest coverage of the kubeconfig provider lives in the one test
// function below, on purpose. The provider registers its registration-Secret
// watch under the fixed controller name "kubeconfig-provider", and
// controller-runtime validates controller names against a process-global set.
// The function runs two providers, each on its own manager, because the
// assigned-namespace scope differs by provider option and not by registration:
// the second manager skips controller-name validation, which is the only way a
// second provider registers in this binary. The scenarios are ordered subtests
// over the shared setup, and the later ones build on clusters the earlier ones
// engaged.
//
// The logger is captured process-wide for the same reason. The provider takes
// its logger from ctrl.Log at construction time and exposes no seam to inject
// one, so the subtests that assert on a refused registration read what the
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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
)

// TestIntegration_KubeconfigProvider_ScopedCaches runs the provider against a
// management environment holding the registration Secrets and a second
// environment standing in for the target cluster, and walks what the namespaces
// data key does to an engaged cluster: a scoped registration that can only read
// the namespaces it declares, a registration without the key that stays
// cluster-wide, two values the provider refuses to engage on, and a scope
// narrowed in place on a running cluster. It then walks the assignedNamespaces
// key: the kinds a provider lists reach the assigned namespace and no other
// kind does, a provider that lists none keeps every kind off it and keeps its
// cluster when the value changes, four registrations the provider refuses, an
// assigned scope moved in place, and two refused edits that disengage a
// running cluster.
func TestIntegration_KubeconfigProvider_ScopedCaches(t *testing.T) {
	commonenvtest.SkipIfEnvTestUnavailable(t)

	const (
		// clustersNamespace mirrors the --clusters-namespace default the
		// operator binaries pass to the provider. plainClustersNamespace is
		// the second provider's, so each provider reads its own registrations.
		clustersNamespace      = "c5c3-clusters"
		plainClustersNamespace = "c5c3-clusters-plain"

		// One cluster name per registration: the provider keys its clusters by
		// the Secret's name, so a distinct name is a distinct cluster.
		scopedCluster  = "scoped-target"
		wideCluster    = "wide-target"
		emptyCluster   = "empty-scope-target"
		invalidCluster = "invalid-scope-target"

		// The assignedNamespaces registrations. assignedCluster and
		// plainAssignedCluster carry the same keys, one per provider.
		assignedCluster        = "assigned-target"
		plainAssignedCluster   = "plain-assigned-target"
		unplacedCluster        = "unplaced-assigned-target"
		emptyAssignedCluster   = "empty-assigned-target"
		invalidAssignedCluster = "invalid-assigned-target"
		overlapCluster         = "overlap-assigned-target"

		// Three namespaces on the target, of which the scoped registration
		// declares the first two. The third is what makes the scope observable.
		declaredA  = "tenant-a"
		declaredB  = "tenant-b"
		undeclared = "tenant-c"

		// seedConfigMap exists in all three namespaces, so a failed read can
		// only be the cache refusing the namespace and never a missing object.
		// A Secret of the same name sits beside it, the kind the first
		// provider lists for its assigned namespaces.
		seedConfigMap = "seed"

		// badNamespace and badAssignedNamespace are rejected by DNS-1123 label
		// validation on both the underscore and the capitals.
		badNamespace         = "Bad_NS"
		badAssignedNamespace = "Tenant_B"

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
		g.Expect(targetClient.Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: seedConfigMap, Namespace: ns},
		})).To(gomega.Succeed(), "seed the Secret in namespace %s on the target environment", ns)
	}

	kubeconfig, err := commonenvtest.KubeconfigBytes(targetCfg, "target")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "build a kubeconfig for the target environment")

	// --- Environment A: the management cluster, hosting the manager.
	mgmtClient, mgmtCfg := commonenvtest.StartEnvTestWithConfig(t, scheme, nil)

	// Without the scheme option the provider builds every target cluster's
	// client on client-go's global scheme. Core kinds are all this test reads,
	// but passing the scheme is what an operator does and keeps the option list
	// non-empty, so the appended scoping option is proven to be additive.
	clusterOptions := []cluster.Option{func(o *cluster.Options) { o.Scheme = scheme }}

	// provider lists Secret as its assigned-namespace kind, the way the
	// c5c3-operator lists the order kinds and Secret; plainProvider lists none,
	// the way every service operator does.
	provider := NewKubeconfigProvider(KubeconfigProviderOptions{
		Namespace:              clustersNamespace,
		ClusterOptions:         clusterOptions,
		AssignedNamespaceKinds: []client.Object{&corev1.Secret{}},
	})
	plainProvider := NewKubeconfigProvider(KubeconfigProviderOptions{
		Namespace:      plainClustersNamespace,
		ClusterOptions: clusterOptions,
	})

	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		running.Wait()
	})

	// startManager runs p on a manager of its own. The second one skips
	// controller-name validation, because both providers name their
	// registration-Secret controller "kubeconfig-provider".
	startManager := func(p *KubeconfigProvider, skipNameValidation bool) {
		mcMgr, err := mcmanager.New(mgmtCfg, p, ctrl.Options{
			Scheme:                 scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Controller:             config.Controller{SkipNameValidation: ptr.To(skipNameValidation)},
		})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "build the multicluster manager")
		g.Expect(p.SetupWithManager(context.Background(), mcMgr)).To(gomega.Succeed(),
			"register the provider's registration-Secret controller")

		running.Go(func() {
			if err := mcMgr.Start(ctx); err != nil {
				t.Errorf("the multicluster manager exited with an error: %v", err)
			}
		})
	}
	startManager(provider, false)
	startManager(plainProvider, true)

	// The providers watch these namespaces for registration Secrets.
	for _, ns := range []string{clustersNamespace, plainClustersNamespace} {
		g.Expect(mgmtClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(gomega.Succeed(), "create the clusters namespace %s on the management environment", ns)
	}

	// registerIn creates a registration Secret in clustersNs. A nil namespaces
	// or assigned value leaves that key out entirely; without either it is the
	// cluster-wide registration.
	registerIn := func(clustersNs, name string, namespaces, assigned []byte) {
		t.Helper()

		data := map[string][]byte{KubeconfigDataKey: kubeconfig}
		if namespaces != nil {
			data[NamespacesDataKey] = namespaces
		}
		if assigned != nil {
			data[AssignedNamespacesDataKey] = assigned
		}
		gomega.NewWithT(t).Expect(mgmtClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: clustersNs,
				Labels:    map[string]string{KubeconfigSecretLabel: "true"},
			},
			Data: data,
		})).To(gomega.Succeed(), "create the registration Secret for cluster %s", name)
	}

	// register creates a registration Secret for the first provider without
	// the assignedNamespaces key.
	register := func(name string, namespaces []byte) {
		t.Helper()
		registerIn(clustersNamespace, name, namespaces, nil)
	}

	// engagedClientOf resolves the cluster p engaged and returns its cached
	// client. It resolves on every call because a re-engaged cluster is a new
	// one. engagedClient does the same for the first provider.
	engagedClientOf := func(gi gomega.Gomega, p *KubeconfigProvider, name string) client.Client {
		cl, err := p.Get(ctx, mcruntime.ClusterName(name))
		gi.Expect(err).NotTo(gomega.HaveOccurred(), "cluster %s should be engaged", name)
		return cl.GetClient()
	}
	engagedClient := func(gi gomega.Gomega, name string) client.Client {
		return engagedClientOf(gi, provider, name)
	}

	// expectEngaged waits until p has engaged the cluster.
	expectEngaged := func(g gomega.Gomega, p *KubeconfigProvider, name string) {
		g.Eventually(func() error {
			_, err := p.Get(ctx, mcruntime.ClusterName(name))
			return err
		}, engageTimeout, pollInterval).Should(gomega.Succeed(),
			"the provider should engage cluster %s from its registration Secret", name)
	}

	// expectRefused waits until the provider has logged reason for the
	// cluster's registration, then watches the cluster stay unresolvable.
	expectRefused := func(g gomega.Gomega, name, reason string) {
		g.Eventually(logs.contains, engageTimeout, pollInterval).
			WithArguments(reason).Should(gomega.BeTrue(),
			"the provider should log why it refused the registration of %s", name)

		g.Consistently(func() error {
			_, err := provider.Get(ctx, mcruntime.ClusterName(name))
			return err
		}, refusalTimeout, pollInterval).Should(gomega.MatchError(mcruntime.ErrClusterNotFound),
			"a refused registration must stay unresolvable, so a CR naming it reports "+
				"TargetClusterUnavailable")
	}

	// expectDisengaged waits until p has dropped the cluster it engaged.
	expectDisengaged := func(g gomega.Gomega, p *KubeconfigProvider, name, why string) {
		g.Eventually(func() error {
			_, err := p.Get(ctx, mcruntime.ClusterName(name))
			return err
		}, engageTimeout, pollInterval).Should(gomega.MatchError(mcruntime.ErrClusterNotFound), why)
	}

	// editRegistration applies edit to the data of the registration Secret name
	// in clustersNs and writes it back.
	editRegistration := func(g gomega.Gomega, clustersNs, name string, edit func(data map[string][]byte)) {
		secret := &corev1.Secret{}
		g.Expect(mgmtClient.Get(ctx, client.ObjectKey{Namespace: clustersNs, Name: name}, secret)).
			To(gomega.Succeed(), "read the registration Secret of cluster %s", name)
		edit(secret.Data)
		g.Expect(mgmtClient.Update(ctx, secret)).To(gomega.Succeed(),
			"write the registration Secret of cluster %s", name)
	}

	// expectOutOfScope asserts that err is the cache refusing a namespace.
	expectOutOfScope := func(g gomega.Gomega, err error, what string) {
		g.Expect(err).To(gomega.HaveOccurred(), "reading %s should fail", what)
		g.Expect(err.Error()).To(gomega.ContainSubstring("unknown namespace for the cache"), what)
	}

	seedKey := func(namespace string) client.ObjectKey {
		return client.ObjectKey{Namespace: namespace, Name: seedConfigMap}
	}

	t.Run("a scoped registration engages a cluster that reads only its namespaces", func(t *testing.T) {
		g := gomega.NewWithT(t)

		register(scopedCluster, []byte(declaredA+","+declaredB))
		expectEngaged(g, provider, scopedCluster)

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
		expectOutOfScope(g, cl.Get(ctx, seedKey(undeclared), &corev1.ConfigMap{}),
			"the undeclared namespace "+undeclared)
		g.Expect(targetClient.Get(ctx, seedKey(undeclared), &corev1.ConfigMap{})).To(gomega.Succeed(),
			"the ConfigMap the scoped cluster refused to read does exist on the target environment")
	})

	t.Run("a registration without the namespaces key stays cluster-wide", func(t *testing.T) {
		g := gomega.NewWithT(t)

		// The compatibility path: same target environment, second cluster name,
		// no namespaces key. What the scoped cluster refused, this one reads.
		register(wideCluster, nil)
		expectEngaged(g, provider, wideCluster)

		g.Expect(engagedClient(g, wideCluster).Get(ctx, seedKey(undeclared), &corev1.ConfigMap{})).
			To(gomega.Succeed(), "a cluster-wide cache should read every namespace")
	})

	t.Run("an empty namespaces value never engages", func(t *testing.T) {
		g := gomega.NewWithT(t)

		register(emptyCluster, []byte("  "))
		expectRefused(g, emptyCluster, "namespaces key present but empty")
	})

	t.Run("a namespaces entry that is no DNS-1123 label never engages", func(t *testing.T) {
		g := gomega.NewWithT(t)

		// One unusable entry refuses the whole registration, and the log names it.
		register(invalidCluster, []byte(declaredA+","+badNamespace))
		expectRefused(g, invalidCluster, badNamespace)
	})

	// Stripping the key is how a registration is revoked in place — the Secret
	// stays, so a `kubectl patch --type=json remove` or an ESO source that lost
	// the key both land here. Leaving the last good cluster engaged would go on
	// running every informer and writing every child under credentials the
	// operator has withdrawn, and the run before this fix logged "skipping" while
	// doing exactly that.
	t.Run("a registration that loses its kubeconfig disengages the cluster", func(t *testing.T) {
		g := gomega.NewWithT(t)

		editRegistration(g, clustersNamespace, wideCluster, func(data map[string][]byte) {
			delete(data, KubeconfigDataKey)
		})
		expectDisengaged(g, provider, wideCluster,
			"a registration that carries no kubeconfig must disengage the cluster it engaged")
	})

	t.Run("narrowing the namespaces value re-engages the cluster", func(t *testing.T) {
		g := gomega.NewWithT(t)

		// Only the namespaces key is written; the kubeconfig is byte-identical
		// to the one the cluster was engaged with. A hash over the kubeconfig
		// alone would skip this Secret as unchanged and the cluster would keep
		// the scope it no longer has.
		editRegistration(g, clustersNamespace, scopedCluster, func(data map[string][]byte) {
			data[NamespacesDataKey] = []byte(declaredA)
		})

		g.Eventually(func(gi gomega.Gomega) {
			cl := engagedClient(gi, scopedCluster)

			expectOutOfScope(gi, cl.List(ctx, &corev1.ConfigMapList{}, client.InNamespace(declaredB)),
				"the namespace dropped from the registration")

			gi.Expect(cl.Get(ctx, seedKey(declaredA), &corev1.ConfigMap{})).To(gomega.Succeed(),
				"the namespace that stayed declared should still be readable")
		}, engageTimeout, pollInterval).Should(gomega.Succeed())
	})

	// The c5c3-operator's shape: the kinds the provider lists read the owner's
	// namespace, and every other kind is refused there, so a service operator's
	// informers on the same target never LIST where the assigned Role grants
	// nothing.
	t.Run("a registration with both keys engages a cluster whose listed kinds reach the assigned namespace",
		func(t *testing.T) {
			g := gomega.NewWithT(t)

			registerIn(clustersNamespace, assignedCluster, []byte(declaredA), []byte(declaredB))
			expectEngaged(g, provider, assignedCluster)

			cl := engagedClient(g, assignedCluster)

			g.Expect(cl.Get(ctx, seedKey(declaredB), &corev1.Secret{})).To(gomega.Succeed(),
				"the listed kind should read the assigned namespace %s", declaredB)
			expectOutOfScope(g, cl.Get(ctx, seedKey(declaredB), &corev1.ConfigMap{}),
				"a kind the provider does not list in the assigned namespace")
			g.Expect(cl.Get(ctx, seedKey(declaredA), &corev1.Secret{})).To(gomega.Succeed(),
				"the listed kind should read the placed namespace %s", declaredA)
			g.Expect(cl.Get(ctx, seedKey(declaredA), &corev1.ConfigMap{})).To(gomega.Succeed(),
				"every kind should read the placed namespace %s", declaredA)
			expectOutOfScope(g, cl.Get(ctx, seedKey(undeclared), &corev1.Secret{}),
				"the listed kind in a namespace neither key names")
		})

	// Every service operator's shape: the same registration engages, and the
	// assigned namespace is off its cache for every kind.
	t.Run("the same registration on a provider without the option keeps every kind off the assigned namespace",
		func(t *testing.T) {
			g := gomega.NewWithT(t)

			registerIn(plainClustersNamespace, plainAssignedCluster, []byte(declaredA), []byte(declaredB))
			expectEngaged(g, plainProvider, plainAssignedCluster)

			cl := engagedClientOf(g, plainProvider, plainAssignedCluster)

			expectOutOfScope(g, cl.Get(ctx, seedKey(declaredB), &corev1.Secret{}),
				"a Secret in the assigned namespace without the option")
			g.Expect(cl.Get(ctx, seedKey(declaredA), &corev1.Secret{})).To(gomega.Succeed(),
				"the placed namespace %s should stay readable", declaredA)
		})

	// A service operator's cache is the same whatever the assigned value says,
	// so assigning an owner one more namespace must not cost it the cluster it
	// works with: a rebuild leaves the cluster unavailable until the new cache
	// syncs.
	t.Run("changing assignedNamespaces keeps a provider without the option on the cluster it engaged",
		func(t *testing.T) {
			g := gomega.NewWithT(t)

			engaged, err := plainProvider.Get(ctx, mcruntime.ClusterName(plainAssignedCluster))
			g.Expect(err).NotTo(gomega.HaveOccurred(), "cluster %s should be engaged", plainAssignedCluster)

			skipped := "cluster is already engaged from this registration, skipping"
			before := logs.count(skipped)

			editRegistration(g, plainClustersNamespace, plainAssignedCluster, func(data map[string][]byte) {
				data[AssignedNamespacesDataKey] = []byte(declaredB + "," + undeclared)
			})

			g.Eventually(logs.count, engageTimeout, pollInterval).WithArguments(skipped).
				Should(gomega.BeNumerically(">", before), "the provider should keep the cluster it engaged")
			g.Consistently(func() (cluster.Cluster, error) {
				return plainProvider.Get(ctx, mcruntime.ClusterName(plainAssignedCluster))
			}, refusalTimeout, pollInterval).Should(gomega.BeIdenticalTo(engaged),
				"the cluster engaged before the edit should stay the engaged one")
		})

	t.Run("assignedNamespaces without namespaces never engages", func(t *testing.T) {
		g := gomega.NewWithT(t)

		registerIn(clustersNamespace, unplacedCluster, nil, []byte(declaredB))
		expectRefused(g, unplacedCluster, "assignedNamespaces without namespaces")
	})

	t.Run("an empty assignedNamespaces value never engages", func(t *testing.T) {
		g := gomega.NewWithT(t)

		registerIn(clustersNamespace, emptyAssignedCluster, []byte(declaredA), []byte("  "))
		expectRefused(g, emptyAssignedCluster, "assignedNamespaces key present but empty")
	})

	t.Run("an assignedNamespaces entry that is no DNS-1123 label never engages", func(t *testing.T) {
		g := gomega.NewWithT(t)

		registerIn(clustersNamespace, invalidAssignedCluster, []byte(declaredA), []byte(badAssignedNamespace))
		expectRefused(g, invalidAssignedCluster, badAssignedNamespace)
	})

	// The second key added and the first one left as it was: the overlap keeps
	// the namespace in every operator's cache for every kind.
	t.Run("a namespace under both keys never engages", func(t *testing.T) {
		g := gomega.NewWithT(t)

		registerIn(clustersNamespace, overlapCluster, []byte(declaredA+","+declaredB), []byte(declaredB))
		expectRefused(g, overlapCluster, "under both namespaces and assignedNamespaces")
	})

	t.Run("changing the assignedNamespaces value re-engages the cluster", func(t *testing.T) {
		g := gomega.NewWithT(t)

		// The narrowing subtest above logged the same line for another
		// cluster, so the count has to grow.
		reengaged := "registration changed, re-engaging the cluster"
		before := logs.count(reengaged)

		editRegistration(g, clustersNamespace, assignedCluster, func(data map[string][]byte) {
			data[AssignedNamespacesDataKey] = []byte(undeclared)
		})

		g.Eventually(logs.count, engageTimeout, pollInterval).WithArguments(reengaged).
			Should(gomega.BeNumerically(">", before), "the provider should log the re-engagement")

		g.Eventually(func(gi gomega.Gomega) {
			cl := engagedClient(gi, assignedCluster)

			gi.Expect(cl.Get(ctx, seedKey(undeclared), &corev1.Secret{})).To(gomega.Succeed(),
				"the newly assigned namespace should be readable for the listed kind")
			expectOutOfScope(gi, cl.Get(ctx, seedKey(declaredB), &corev1.Secret{}),
				"the namespace dropped from the assigned scope")
		}, engageTimeout, pollInterval).Should(gomega.Succeed())
	})

	// A scope the provider refuses on a running cluster is a refusal like the
	// lost kubeconfig above: the cluster built with the old scope has to go.
	t.Run("a registration that drops namespaces but keeps assignedNamespaces disengages the cluster",
		func(t *testing.T) {
			g := gomega.NewWithT(t)

			editRegistration(g, clustersNamespace, assignedCluster, func(data map[string][]byte) {
				delete(data, NamespacesDataKey)
			})
			expectDisengaged(g, provider, assignedCluster,
				"a registration the provider refuses must disengage the cluster it engaged")
		})

	t.Run("an unusable assignedNamespaces value disengages the cluster", func(t *testing.T) {
		g := gomega.NewWithT(t)

		// The provider without the option leaves the value out of its hash, and
		// still refuses a value that is no namespace.
		editRegistration(g, plainClustersNamespace, plainAssignedCluster, func(data map[string][]byte) {
			data[AssignedNamespacesDataKey] = []byte(badAssignedNamespace)
		})
		expectDisengaged(g, plainProvider, plainAssignedCluster,
			"a registration the provider refuses must disengage the cluster it engaged")
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
	return r.count(substring) > 0
}

// count reports how many recorded lines contain substring.
func (r *recordingLogs) count(substring string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0
	for _, line := range r.lines {
		if strings.Contains(line, substring) {
			n++
		}
	}
	return n
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
