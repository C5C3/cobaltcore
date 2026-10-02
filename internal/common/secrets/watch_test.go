// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"fmt"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// storeWatchTargets is a target-cluster function that keeps every CR on the
// management cluster; AddStoreWatches only passes it on to the remote legs.
func storeWatchTargets(context.Context, types.NamespacedName) (string, error) {
	return "", nil
}

// recordingStoreMapper returns a mapper factory that appends every kind it is
// asked for to kinds and hands back a map function that enqueues nothing.
func recordingStoreMapper(kinds *[]commonv1.SecretStoreRefKind) func(client.Reader, commonv1.SecretStoreRefKind) handler.MapFunc {
	return func(_ client.Reader, kind commonv1.SecretStoreRefKind) handler.MapFunc {
		*kinds = append(*kinds, kind)
		return func(context.Context, client.Object) []reconcile.Request { return nil }
	}
}

// TestAddStoreWatches_watchedKinds resolves the legs against a scheme that
// registers only some store kinds. AddInputWatch looks each leg's kind up in
// the scheme, so a leg on a kind the scheme lacks fails setup: the outcome
// shows which kinds are watched, not only which mappers were built.
func TestAddStoreWatches_watchedKinds(t *testing.T) {
	clusterStore := []runtime.Object{&esov1.ClusterSecretStore{}, &esov1.ClusterSecretStoreList{}}
	namespacedStore := []runtime.Object{&esov1.SecretStore{}, &esov1.SecretStoreList{}}

	for _, tc := range []struct {
		name            string
		namespaceScoped bool
		registered      []runtime.Object
		wantKinds       []commonv1.SecretStoreRefKind
		wantErr         string
	}{
		{
			name:       "cluster-wide watches both kinds",
			registered: append(append([]runtime.Object{}, clusterStore...), namespacedStore...),
			wantKinds:  []commonv1.SecretStoreRefKind{commonv1.SecretStoreKindCluster, commonv1.SecretStoreKindNamespaced},
		},
		{
			name:       "cluster-wide watches ClusterSecretStore",
			registered: namespacedStore,
			wantErr:    "for the type v1.ClusterSecretStore in scheme",
		},
		{
			name:       "cluster-wide watches SecretStore",
			registered: clusterStore,
			wantErr:    "for the type v1.SecretStore in scheme",
		},
		{
			// A Role cannot grant the cluster-scoped kind, so a namespace-scoped
			// operator must not register a leg on it at all: its informer would
			// never sync. Without ClusterSecretStore in the scheme, such a leg
			// would fail setup here.
			name:            "namespace-scoped watches no ClusterSecretStore",
			namespaceScoped: true,
			registered:      namespacedStore,
			wantKinds:       []commonv1.SecretStoreRefKind{commonv1.SecretStoreKindNamespaced},
		},
		{
			name:            "namespace-scoped watches SecretStore",
			namespaceScoped: true,
			registered:      clusterStore,
			wantErr:         "for the type v1.SecretStore in scheme",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			scheme := runtime.NewScheme()
			scheme.AddKnownTypes(esov1.SchemeGroupVersion, tc.registered...)

			var kinds []commonv1.SecretStoreRefKind
			got, err := AddStoreWatches(mcbuilder.ControllerManagedBy(nil), scheme, storeWatchTargets,
				tc.namespaceScoped, nil, recordingStoreMapper(&kinds))

			if tc.wantErr != "" {
				g.Expect(got).To(gomega.BeNil())
				g.Expect(err).To(gomega.MatchError(gomega.And(
					gomega.ContainSubstring("resolving input kind for remote watch"),
					gomega.ContainSubstring(tc.wantErr))))
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(got).NotTo(gomega.BeNil())
			g.Expect(kinds).To(gomega.Equal(tc.wantKinds))
		})
	}
}

func TestAddStoreWatches_nilMapper(t *testing.T) {
	for _, namespaceScoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("namespaceScoped=%t", namespaceScoped), func(t *testing.T) {
			g := gomega.NewWithT(t)

			got, err := AddStoreWatches(mcbuilder.ControllerManagedBy(nil), gateTestScheme(t), storeWatchTargets,
				namespaceScoped, nil, nil)

			g.Expect(got).To(gomega.BeNil())
			g.Expect(err).To(gomega.MatchError("secrets: store watch mapper must not be nil"))
		})
	}
}

// A scheme without the ESO kinds is a wiring mistake that has to fail setup in
// both modes, with the diagnosis AddInputWatch gives it.
func TestAddStoreWatches_unknownKindFailsSetup(t *testing.T) {
	for _, namespaceScoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("namespaceScoped=%t", namespaceScoped), func(t *testing.T) {
			g := gomega.NewWithT(t)

			var kinds []commonv1.SecretStoreRefKind
			got, err := AddStoreWatches(mcbuilder.ControllerManagedBy(nil), runtime.NewScheme(), storeWatchTargets,
				namespaceScoped, nil, recordingStoreMapper(&kinds))

			g.Expect(got).To(gomega.BeNil())
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("resolving input kind for remote watch")))
		})
	}
}
