// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// KubeconfigBytes builds a kubeconfig for registering an envtest environment as
// a target cluster via the kubeconfig provider's Secret format. clusterName
// names the cluster, its credentials, and the current context inside the file;
// the provider takes the target-cluster name from the Secret's name, not from
// here, so this only has to be a stable identifier.
//
// The credentials are whatever the REST config carries: the client certificate
// and key envtest hands out, plus a bearer token when one is set.
func KubeconfigBytes(cfg *rest.Config, clusterName string) ([]byte, error) {
	apiCfg := clientcmdapi.NewConfig()
	apiCfg.Clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	apiCfg.AuthInfos[clusterName] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
	}
	apiCfg.Contexts[clusterName] = &clientcmdapi.Context{
		Cluster:  clusterName,
		AuthInfo: clusterName,
	}
	apiCfg.CurrentContext = clusterName

	out, err := clientcmd.Write(*apiCfg)
	if err != nil {
		return nil, fmt.Errorf("serializing kubeconfig for cluster %q: %w", clusterName, err)
	}
	return out, nil
}
