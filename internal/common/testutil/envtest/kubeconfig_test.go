// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"testing"

	. "github.com/onsi/gomega"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func TestKubeconfigBytes_roundTripsRESTConfig(t *testing.T) {
	g := NewGomegaWithT(t)

	in := &rest.Config{
		Host: "https://127.0.0.1:6443",
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   []byte("ca-data"),
			CertData: []byte("cert-data"),
			KeyData:  []byte("key-data"),
		},
	}

	raw, err := KubeconfigBytes(in, "target-b")
	g.Expect(err).NotTo(HaveOccurred())

	loaded, err := clientcmd.Load(raw)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(loaded.CurrentContext).To(Equal("target-b"))
	g.Expect(loaded.Contexts).To(HaveKey("target-b"))

	out, err := clientcmd.NewDefaultClientConfig(*loaded, &clientcmd.ConfigOverrides{}).ClientConfig()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(out.Host).To(Equal(in.Host))
	g.Expect(out.CAData).To(Equal(in.CAData))
	g.Expect(out.CertData).To(Equal(in.CertData))
	g.Expect(out.KeyData).To(Equal(in.KeyData))
}

// A REST config without a CA is what a kubeconfig for an insecure or
// system-trust endpoint looks like. It must still produce a file the provider
// can parse, rather than one that fails to load.
func TestKubeconfigBytes_withoutCADataStaysLoadable(t *testing.T) {
	g := NewGomegaWithT(t)

	in := &rest.Config{
		Host:        "https://127.0.0.1:6443",
		BearerToken: "token",
	}

	raw, err := KubeconfigBytes(in, "target-b")
	g.Expect(err).NotTo(HaveOccurred())

	loaded, err := clientcmd.Load(raw)
	g.Expect(err).NotTo(HaveOccurred())

	out, err := clientcmd.NewDefaultClientConfig(*loaded, &clientcmd.ConfigOverrides{}).ClientConfig()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(out.Host).To(Equal(in.Host))
	g.Expect(out.BearerToken).To(Equal("token"))
	g.Expect(out.CAData).To(BeEmpty())
}
