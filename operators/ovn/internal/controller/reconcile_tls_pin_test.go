// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the three Certificates the TLS step requests. The
// goldens below are FULL-OBJECT YAML captured from the builders as they stand
// today, so any refactor of the certificate projection has to reproduce every
// rendered byte.
//
// The subject alternative names are what the pin is really for: a Raft member
// verifies its peer against the name it dialed, and a missing or reordered
// dnsName takes the cluster apart on the next rollout without any object
// failing to apply.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// pinSingleMemberIssuerOVNCentral is the fixture behind the third server-
// certificate golden: a one-member Northbound cluster requested from a
// namespaced Issuer, so a builder that ignored the replica count or hard-coded
// the issuer scope cannot hide behind a value that happens to match the
// default.
func pinSingleMemberIssuerOVNCentral() *ovnv1alpha1.OVNCentral {
	cr := testOVNCentral()
	cr.Spec.Northbound.Replicas = 1
	cr.Spec.TLS.IssuerRef.Kind = "Issuer"
	return cr
}

const pinNorthboundServerCertificateGolden = `metadata:
  labels:
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-nb-server
  namespace: openstack
spec:
  commonName: ovn-nb
  dnsNames:
  - ovn-nb-0.ovn-nb.openstack.svc.cluster.local
  - ovn-nb-0.ovn-nb.openstack.svc
  - ovn-nb-1.ovn-nb.openstack.svc.cluster.local
  - ovn-nb-1.ovn-nb.openstack.svc
  - ovn-nb-2.ovn-nb.openstack.svc.cluster.local
  - ovn-nb-2.ovn-nb.openstack.svc
  - ovn-nb.openstack.svc.cluster.local
  duration: 8760h0m0s
  issuerRef:
    group: cert-manager.io
    kind: ClusterIssuer
    name: openstack-ovn-ca
  privateKey:
    algorithm: ECDSA
    size: 256
  renewBefore: 720h0m0s
  secretName: ovn-nb-server
  usages:
  - server auth
  - client auth
  - digital signature
  - key encipherment
status: {}
`

const pinSouthboundServerCertificateGolden = `metadata:
  labels:
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-sb-server
  namespace: openstack
spec:
  commonName: ovn-sb
  dnsNames:
  - ovn-sb-0.ovn-sb.openstack.svc.cluster.local
  - ovn-sb-0.ovn-sb.openstack.svc
  - ovn-sb-1.ovn-sb.openstack.svc.cluster.local
  - ovn-sb-1.ovn-sb.openstack.svc
  - ovn-sb-2.ovn-sb.openstack.svc.cluster.local
  - ovn-sb-2.ovn-sb.openstack.svc
  - ovn-sb.openstack.svc.cluster.local
  duration: 8760h0m0s
  issuerRef:
    group: cert-manager.io
    kind: ClusterIssuer
    name: openstack-ovn-ca
  privateKey:
    algorithm: ECDSA
    size: 256
  renewBefore: 720h0m0s
  secretName: ovn-sb-server
  usages:
  - server auth
  - client auth
  - digital signature
  - key encipherment
status: {}
`

const pinSingleMemberIssuerCertificateGolden = `metadata:
  labels:
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-nb-server
  namespace: openstack
spec:
  commonName: ovn-nb
  dnsNames:
  - ovn-nb-0.ovn-nb.openstack.svc.cluster.local
  - ovn-nb-0.ovn-nb.openstack.svc
  - ovn-nb.openstack.svc.cluster.local
  duration: 8760h0m0s
  issuerRef:
    group: cert-manager.io
    kind: Issuer
    name: openstack-ovn-ca
  privateKey:
    algorithm: ECDSA
    size: 256
  renewBefore: 720h0m0s
  secretName: ovn-nb-server
  usages:
  - server auth
  - client auth
  - digital signature
  - key encipherment
status: {}
`

const pinClientCertificateGolden = `metadata:
  labels:
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-client
  namespace: openstack
spec:
  commonName: ovn-client
  duration: 8760h0m0s
  issuerRef:
    group: cert-manager.io
    kind: ClusterIssuer
    name: openstack-ovn-ca
  privateKey:
    algorithm: ECDSA
    size: 256
  renewBefore: 720h0m0s
  secretName: ovn-client
  usages:
  - client auth
  - digital signature
  - key encipherment
status: {}
`

// TestPinServerCertificate pins the keypair a database's Raft members listen
// with, across the two databases and across a one-member cluster on a
// namespaced Issuer.
func TestPinServerCertificate(t *testing.T) {
	cases := []struct {
		name   string
		cr     func() *ovnv1alpha1.OVNCentral
		db     func(*ovnv1alpha1.OVNCentral) raftDB
		golden string
	}{
		{name: "northbound", cr: testOVNCentral, db: northboundDB, golden: pinNorthboundServerCertificateGolden},
		{name: "southbound", cr: testOVNCentral, db: southboundDB, golden: pinSouthboundServerCertificateGolden},
		{
			name:   "single-member-issuer",
			cr:     pinSingleMemberIssuerOVNCentral,
			db:     northboundDB,
			golden: pinSingleMemberIssuerCertificateGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cr := tc.cr()

			got, err := yaml.Marshal(serverCertificate(cr, tc.db(cr)))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered server Certificate must stay byte-identical")
		})
	}
}

// TestPinClientCertificate pins the keypair every OVN client authenticates
// with. Its common name is what OVN RBAC is keyed on, so it is the one field a
// rename would silently revoke every client's access with.
func TestPinClientCertificate(t *testing.T) {
	g := NewWithT(t)

	got, err := yaml.Marshal(clientCertificate(testOVNCentral()))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinClientCertificateGolden),
		"the rendered client Certificate must stay byte-identical")
}
