// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

// The addresses the endpoint step publishes for the two databases. northd and
// the relay both refuse to project anything before they are set, so a fixture
// that leaves them empty exercises only the wait.
const (
	testNorthboundAddress = "ssl:10.96.0.11:6641"
	testSouthboundAddress = "ssl:10.96.0.21:6642"
)

// publishEndpoints stamps the two published addresses on cr and returns it, so
// a fixture reaches the steps that consume them in one expression.
func publishEndpoints(cr *ovnv1alpha1.OVNCentral) *ovnv1alpha1.OVNCentral {
	cr.Status.Northbound.InternalDbAddress = testNorthboundAddress
	cr.Status.Southbound.InternalDbAddress = testSouthboundAddress
	return cr
}

// publishOnNodePorts turns the node-port publication on for both databases and
// returns cr, so a test that exercises the address outside the cluster reads as
// one that asked for it. The CRD default is off.
func publishOnNodePorts(cr *ovnv1alpha1.OVNCentral) *ovnv1alpha1.OVNCentral {
	cr.Spec.Northbound.ExternallyReachable = true
	cr.Spec.Southbound.ExternallyReachable = true
	return cr
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
// scheme and the status subresources the reconciler reads: the CR's own, the
// StatefulSet's, whose ready-member count the database step judges a Raft
// cluster by, the Deployment's, which northd and the relay are judged by, and
// the Certificate's, which carries the Ready condition the TLS step waits on.
func ovnCentralFakeClientBuilder(t *testing.T, objs ...client.Object) *fake.ClientBuilder {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&ovnv1alpha1.OVNCentral{}, &appsv1.StatefulSet{},
			&appsv1.Deployment{}, &certmanagerv1.Certificate{})
}

// newTestOVNCentralReconciler builds an OVNCentralReconciler over a fake client
// pre-loaded with objs. cert-manager counts as installed, which is the only
// configuration an OVNCentral runs in: spec.tls is required, so a cluster
// without it serves no OVNCentral at all. The tests of the unavailable path
// build their own reconciler.
func newTestOVNCentralReconciler(t *testing.T, objs ...client.Object) *OVNCentralReconciler {
	t.Helper()

	return &OVNCentralReconciler{
		Client:               ovnCentralFakeClientBuilder(t, objs...).Build(),
		Scheme:               newTestScheme(t),
		Recorder:             record.NewFakeRecorder(50),
		certManagerAvailable: true,
	}
}

// ovnCentralCondition returns one of the OVNCentral's conditions, or nil.
func ovnCentralCondition(cr *ovnv1alpha1.OVNCentral, conditionType string) *metav1.Condition {
	return conditions.GetCondition(cr.Status.Conditions, conditionType)
}

// ovnCentralRequest is the reconcile request for the shared OVNCentral fixture.
var ovnCentralRequest = reconcile.Request{
	NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testOVNCentralName},
}

// getOVNCentral re-reads the OVNCentral CR from the given client, so an
// assertion reads what a pass persisted rather than the in-memory copy it
// mutated.
func getOVNCentral(t *testing.T, c client.Client, name string) *ovnv1alpha1.OVNCentral {
	t.Helper()

	var cr ovnv1alpha1.OVNCentral
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &cr); err != nil {
		t.Fatalf("re-reading OVNCentral %s: %v", name, err)
	}
	return &cr
}

// collectEvents drains the FakeRecorder channel non-blocking.
func collectEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// The shared OVNChassis fixture coordinates. The chassis fixture lives in the
// same namespace as the OVNCentral one and names it, because spec.centralRef is
// namespace-local: the chassis mount the client Secret that OVNCentral
// publishes.
const (
	testOVNChassisName = "chassis"

	// testChassisNodeLabel is the label a node carries to be selected by the
	// chassis fixture.
	testChassisNodeLabel = "openstack.c5c3.io/chassis"

	// testGatewayNodeLabel narrows the selected nodes down to the gateways in
	// the tests that set spec.gateway.
	testGatewayNodeLabel = "openstack.c5c3.io/gateway"
)

// testOVNChassis returns the shared OVNChassis fixture, carrying the values the
// CRD schema would have defaulted. A CR the operator reads has been through
// admission, so a fixture that left the defaults off would exercise a spec no
// reconcile ever sees.
func testOVNChassis() *ovnv1alpha1.OVNChassis {
	return &ovnv1alpha1.OVNChassis{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testOVNChassisName,
			Namespace: testNamespace,
		},
		Spec: ovnv1alpha1.OVNChassisSpec{
			CentralRef:            ovnv1alpha1.OVNCentralRef{Name: testOVNCentralName},
			NodeSelector:          map[string]string{testChassisNodeLabel: "true"},
			EncapType:             "geneve",
			UpdateStrategy:        ovnv1alpha1.OVNChassisUpdateStrategy{Type: "RollingUpdate"},
			RemoteProbeIntervalMs: 60000,
		},
	}
}

// ovnChassisFakeClientBuilder returns a fake client builder with the package
// scheme and the status subresources the chassis reconciler reads or writes: the
// CR's own, which every sub-reconciler stamps its conditions and its node list
// on, and the DaemonSet's, whose node counters the two DaemonSet steps judge a
// rollout by.
func ovnChassisFakeClientBuilder(t *testing.T, objs ...client.Object) *fake.ClientBuilder {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&ovnv1alpha1.OVNChassis{}, &appsv1.DaemonSet{})
}

// newTestOVNChassisReconciler builds an OVNChassisReconciler over a fake client
// pre-loaded with objs. The Resolver stays nil, which is the always-local mode
// every single-cluster test runs in: the children land on the same fake client
// the CRs live on.
func newTestOVNChassisReconciler(t *testing.T, objs ...client.Object) *OVNChassisReconciler {
	t.Helper()

	return &OVNChassisReconciler{
		Client:   ovnChassisFakeClientBuilder(t, objs...).Build(),
		Scheme:   newTestScheme(t),
		Recorder: record.NewFakeRecorder(50),
	}
}

// ovnChassisCondition returns one of the OVNChassis's conditions, or nil.
func ovnChassisCondition(cr *ovnv1alpha1.OVNChassis, conditionType string) *metav1.Condition {
	return conditions.GetCondition(cr.Status.Conditions, conditionType)
}

// chassisKey builds the key of a child of the shared OVNChassis fixture.
func chassisKey(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: testNamespace, Name: name}
}

// ovnChassisRequest is the reconcile request for the shared OVNChassis fixture.
var ovnChassisRequest = reconcile.Request{
	NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testOVNChassisName},
}

// getOVNChassis re-reads the OVNChassis CR from the given client, so an
// assertion reads what a pass persisted rather than the in-memory copy it
// mutated.
func getOVNChassis(t *testing.T, c client.Client, name string) *ovnv1alpha1.OVNChassis {
	t.Helper()

	var cr ovnv1alpha1.OVNChassis
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &cr); err != nil {
		t.Fatalf("re-reading OVNChassis %s: %v", name, err)
	}
	return &cr
}
