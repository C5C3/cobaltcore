// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"errors"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"

	"github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// AddStoreWatches adds the input watch legs for the stores a CR can select
// through spec.secretStoreRef: the cluster-scoped ClusterSecretStore unless
// namespaceScoped, then the namespaced SecretStore.
//
// A namespace-scoped operator runs under a Role, and a Role cannot grant a
// cluster-scoped kind. A ClusterSecretStore leg would start an informer whose
// list is forbidden, every other source would block waiting for it to sync,
// and the manager would exit at its cache-sync timeout. Such an operator
// refuses a cluster store through GateStoreReady instead of watching one.
//
// mapper builds the map function for the given store kind from c; each leg's
// events go through the function built for its own kind. An error from
// multicluster.AddInputWatch is returned unchanged with a nil builder.
func AddStoreWatches(
	b *mcbuilder.Builder,
	scheme *runtime.Scheme,
	targets multicluster.TargetClusterFunc,
	namespaceScoped bool,
	c client.Reader,
	mapper func(client.Reader, commonv1.SecretStoreRefKind) handler.MapFunc,
) (*mcbuilder.Builder, error) {
	if mapper == nil {
		return nil, errors.New("secrets: store watch mapper must not be nil")
	}
	if !namespaceScoped {
		var err error
		b, err = multicluster.AddInputWatch(b, scheme, targets, &esov1.ClusterSecretStore{},
			mapper(c, commonv1.SecretStoreKindCluster))
		if err != nil {
			return nil, err
		}
	}
	return multicluster.AddInputWatch(b, scheme, targets, &esov1.SecretStore{},
		mapper(c, commonv1.SecretStoreKindNamespaced))
}
