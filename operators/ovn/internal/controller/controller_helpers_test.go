// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The shared fixture coordinates. The name is short on purpose: it prefixes
// every child object name, and the backup CronJob the webhook bounds the name
// by is the tightest of them.
const (
	testNamespace      = "openstack"
	testOVNCentralName = "ovn"
	testIssuerName     = "openstack-ovn-ca"
)

// testOVNCentral returns the shared OVNCentral fixture, carrying the values the
// CRD schema would have defaulted. A CR the operator reads has been through
// admission, so a fixture that left the defaults off would exercise a spec no
// reconcile ever sees.
func testOVNCentral() *ovnv1alpha1.OVNCentral {
	return &ovnv1alpha1.OVNCentral{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testOVNCentralName,
			Namespace: testNamespace,
		},
		Spec: ovnv1alpha1.OVNCentralSpec{
			Northbound: defaultedDatabaseSpec(),
			Southbound: defaultedDatabaseSpec(),
			Northd:     ovnv1alpha1.OVNNorthdSpec{Threads: 1},
			TLS: ovnv1alpha1.OVNTLSSpec{
				IssuerRef: ovnv1alpha1.OVNIssuerRef{Name: testIssuerName, Kind: "ClusterIssuer"},
			},
		},
	}
}

// defaultedDatabaseSpec is one database block as the CRD defaults it.
func defaultedDatabaseSpec() ovnv1alpha1.OVNDatabaseSpec {
	return ovnv1alpha1.OVNDatabaseSpec{
		Replicas:          3,
		Storage:           ovnv1alpha1.OVNStorageSpec{Size: "1Gi"},
		ElectionTimerMs:   1000,
		InactivityProbeMs: 60000,
	}
}

// newTestScheme returns the scheme the fake clients are built with: the
// client-go kinds the children are, the OVN kinds the CRs are, and the
// cert-manager kinds the TLS step projects.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the client-go kinds to the test scheme: %v", err)
	}
	if err := ovnv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the OVN kinds to the test scheme: %v", err)
	}
	if err := certmanagerv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the cert-manager kinds to the test scheme: %v", err)
	}
	return scheme
}

// ovnCentralFakeClientBuilder returns a fake client builder with the package
// scheme and the status subresources the reconciler reads: the CR's own, and the
// StatefulSet's, whose ready-member count the database step judges a Raft
// cluster by.
func ovnCentralFakeClientBuilder(t *testing.T, objs ...client.Object) *fake.ClientBuilder {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&ovnv1alpha1.OVNCentral{}, &appsv1.StatefulSet{})
}

// newTestOVNCentralReconciler builds an OVNCentralReconciler over a fake client
// pre-loaded with objs.
func newTestOVNCentralReconciler(t *testing.T, objs ...client.Object) *OVNCentralReconciler {
	t.Helper()

	return &OVNCentralReconciler{
		Client:   ovnCentralFakeClientBuilder(t, objs...).Build(),
		Scheme:   newTestScheme(t),
		Recorder: record.NewFakeRecorder(50),
	}
}

// ovnCentralCondition returns one of the OVNCentral's conditions, or nil.
func ovnCentralCondition(cr *ovnv1alpha1.OVNCentral, conditionType string) *metav1.Condition {
	return conditions.GetCondition(cr.Status.Conditions, conditionType)
}
