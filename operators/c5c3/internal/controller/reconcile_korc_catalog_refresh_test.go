// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the K-ORC catalog refresh: the epoch korcCatalogEpoch computes over
// the registrations of a ControlPlane, the annotation key it is recorded under,
// and reconcileKORCCatalogRefresh, which restarts K-ORC by writing that epoch
// onto the pod template of its Deployment.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// catalogEpochControlPlane returns a ControlPlane that admits registrations from
// team-a, the namespace the standalone registrations below come from.
func catalogEpochControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := dbCredManagedControlPlane()
	cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{AllowedNamespaces: []string{"team-a"}}
	return cp
}

// catalogRefreshControlPlane returns a catalogEpochControlPlane whose
// ServiceAccountsReady gate is open.
func catalogRefreshControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := catalogEpochControlPlane()
	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeServiceAccountsReady))
	return cp
}

// settledRegistration returns a registration of cp in namespace that declares a
// catalog entry with one endpoint per interface=URL pair and reports
// CatalogReady=True at its generation.
func settledRegistration(namespace, name string, cp *c5c3v1alpha1.ControlPlane, endpoints ...string) *c5c3v1alpha1.KeystoneService {
	ks := registrationIn(namespace, name, cp)
	ks.Generation = 1
	ks.Spec.Catalog = &c5c3v1alpha1.KeystoneServiceCatalogSpec{ServiceType: name}
	for _, endpoint := range endpoints {
		iface, url, _ := strings.Cut(endpoint, "=")
		ks.Spec.Catalog.Endpoints = append(ks.Spec.Catalog.Endpoints, c5c3v1alpha1.KeystoneServiceEndpointSpec{
			Interface: c5c3v1alpha1.ExternalEndpointType(iface), URL: url,
		})
	}
	ks.Status.Conditions = []metav1.Condition{{
		Type:               conditionTypeKeystoneServiceCatalogReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ks.Generation,
		Reason:             "CatalogRegistered",
		LastTransitionTime: metav1.Now(),
	}}
	return ks
}

// accountOnlyRegistration returns a registration of cp that declares no catalog
// entry and is therefore never part of the epoch.
func accountOnlyRegistration(namespace, name string, cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	ks := registrationIn(namespace, name, cp)
	ks.Spec.Account = &c5c3v1alpha1.KeystoneServiceAccountSpec{}
	return ks
}

// korcDeployment returns the K-ORC Deployment with the given pod-template
// annotations; nil leaves the map unset.
func korcDeployment(annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: korcDeploymentName, Namespace: korcDeploymentNamespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}},
		},
	}
}

// registrationItems dereferences registrations into the slice korcCatalogEpoch takes.
func registrationItems(registrations ...*c5c3v1alpha1.KeystoneService) []c5c3v1alpha1.KeystoneService {
	out := make([]c5c3v1alpha1.KeystoneService, 0, len(registrations))
	for _, ks := range registrations {
		out = append(out, *ks)
	}
	return out
}

// mustEpoch returns the epoch of a settled registration set.
func mustEpoch(t *testing.T, cp *c5c3v1alpha1.ControlPlane, registrations ...*c5c3v1alpha1.KeystoneService) string {
	t.Helper()
	epoch, settled := korcCatalogEpoch(cp, registrationItems(registrations...))
	if !settled || epoch == "" {
		t.Fatalf("expected a settled, non-empty epoch, got (%q, %v)", epoch, settled)
	}
	return epoch
}

// catalogRefreshHarness is a reconciler over a fake client that counts the
// Deployment Gets and Patches and keeps the options of the last Patch. funcs
// fail a call by returning an error before the counted delegate runs.
type catalogRefreshHarness struct {
	r          *ControlPlaneReconciler
	c          client.Client
	rec        *record.FakeRecorder
	gets       int
	patches    int
	patchOwner string
}

func newCatalogRefreshHarness(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *catalogRefreshHarness {
	t.Helper()
	h := &catalogRefreshHarness{rec: record.NewFakeRecorder(10)}
	s := korcTestScheme(t)
	h.c = fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithIndex(&c5c3v1alpha1.KeystoneService{}, KeystoneServiceControlPlaneRefIndexKey,
			keystoneServiceControlPlaneRefExtractor).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if funcs.List != nil {
					return funcs.List(ctx, cl, list, opts...)
				}
				return cl.List(ctx, list, opts...)
			},
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption,
			) error {
				if _, isDeployment := obj.(*appsv1.Deployment); isDeployment {
					h.gets++
				}
				if funcs.Get != nil {
					return funcs.Get(ctx, cl, key, obj, opts...)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption,
			) error {
				h.patches++
				po := &client.PatchOptions{}
				po.ApplyOptions(opts)
				h.patchOwner = po.FieldManager
				if funcs.Patch != nil {
					return funcs.Patch(ctx, cl, obj, patch, opts...)
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	h.r = &ControlPlaneReconciler{Client: h.c, Scheme: s, Recorder: h.rec}
	return h
}

// run drives one pass and fails the test on a non-zero result.
func (h *catalogRefreshHarness) run(t *testing.T, cp *c5c3v1alpha1.ControlPlane) error {
	t.Helper()
	res, err := h.r.reconcileKORCCatalogRefresh(context.Background(), cp)
	if res != (ctrl.Result{}) {
		t.Fatalf("reconcileKORCCatalogRefresh must never requeue, got %+v", res)
	}
	return err
}

// templateAnnotations reads the pod-template annotations of the stored Deployment.
func (h *catalogRefreshHarness) templateAnnotations(t *testing.T) map[string]string {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := h.c.Get(context.Background(), client.ObjectKey{
		Namespace: korcDeploymentNamespace, Name: korcDeploymentName,
	}, dep); err != nil {
		t.Fatalf("reading the K-ORC Deployment: %v", err)
	}
	return dep.Spec.Template.Annotations
}

var (
	deploymentsResource = schema.GroupResource{Group: "apps", Resource: "deployments"}
	errRBACDenied       = errors.New("RBAC: access denied")
)

// --- korcCatalogEpoch ---

func TestKORCCatalogEpoch_NothingToRecord(t *testing.T) {
	cp := catalogEpochControlPlane()
	for _, tc := range []struct {
		name          string
		registrations []c5c3v1alpha1.KeystoneService
	}{
		{name: "nil list"},
		{name: "empty list", registrations: []c5c3v1alpha1.KeystoneService{}},
		{name: "account-only registrations", registrations: registrationItems(
			accountOnlyRegistration("openstack", "neutron-nova", cp),
			accountOnlyRegistration("openstack", "nova-hypervisor-operator", cp),
		)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			epoch, settled := korcCatalogEpoch(cp, tc.registrations)
			g.Expect(epoch).To(BeEmpty())
			g.Expect(settled).To(BeTrue(), "nothing to wait for is settled")
		})
	}
}

func TestKORCCatalogEpoch_Unsettled(t *testing.T) {
	cp := catalogEpochControlPlane()
	for _, tc := range []struct {
		name     string
		unsettle func(*c5c3v1alpha1.KeystoneService)
	}{
		{name: "no CatalogReady condition", unsettle: func(ks *c5c3v1alpha1.KeystoneService) {
			ks.Status.Conditions = nil
		}},
		{name: "CatalogReady=False", unsettle: func(ks *c5c3v1alpha1.KeystoneService) {
			ks.Status.Conditions[0].Status = metav1.ConditionFalse
		}},
		{name: "CatalogReady=True at an older generation", unsettle: func(ks *c5c3v1alpha1.KeystoneService) {
			ks.Generation = 2
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			peer := settledRegistration("openstack", "glance", cp, "public=https://glance.example")
			pending := settledRegistration("openstack", "placement", cp, "public=https://placement.example")
			tc.unsettle(pending)

			epoch, settled := korcCatalogEpoch(cp, registrationItems(peer, pending))

			g.Expect(epoch).To(BeEmpty())
			g.Expect(settled).To(BeFalse(), "one unsettled registration holds the whole epoch")
		})
	}
}

// TestKORCCatalogEpoch_HashesTheDocumentedByteStream pins the epoch recipe a
// later operator version must reproduce: a changed recipe restarts K-ORC on the
// upgrade.
func TestKORCCatalogEpoch_HashesTheDocumentedByteStream(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := catalogEpochControlPlane()
	glance := settledRegistration("openstack", "glance", cp,
		"public=https://glance.example", "internal=http://glance.openstack.svc")
	glance.Spec.Catalog.ServiceType = "image"
	glance.Spec.Catalog.ServiceName = "glance-images"
	// No service name: the CR name stands in for it.
	custom := settledRegistration("team-a", "custom", cp, "public=https://custom.example")
	custom.Spec.Catalog.ServiceType = "custom-type"

	epoch, settled := korcCatalogEpoch(cp, registrationItems(custom, glance))

	sum := sha256.Sum256([]byte("cp-uid" +
		"\nopenstack/glance\x00image\x00glance-images\x00internal=http://glance.openstack.svc\x00public=https://glance.example" +
		"\nteam-a/custom\x00custom-type\x00custom\x00public=https://custom.example"))
	g.Expect(settled).To(BeTrue())
	g.Expect(epoch).To(MatchRegexp(`^[0-9a-f]{16}$`))
	g.Expect(epoch).To(Equal(hex.EncodeToString(sum[:])[:16]))
}

func TestKORCCatalogEpoch_OrderIndependent(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := catalogEpochControlPlane()
	glance := settledRegistration("openstack", "glance", cp,
		"admin=http://a", "internal=http://i", "public=https://p")
	nova := settledRegistration("openstack", "nova", cp, "public=https://nova")
	custom := settledRegistration("team-a", "custom", cp, "public=https://custom")
	baseline := mustEpoch(t, cp, glance, nova, custom)

	reversedEndpoints := glance.DeepCopy()
	slices.Reverse(reversedEndpoints.Spec.Catalog.Endpoints)

	g.Expect(mustEpoch(t, cp, custom, nova, glance)).To(Equal(baseline), "registration order must not matter")
	g.Expect(mustEpoch(t, cp, reversedEndpoints, nova, custom)).To(Equal(baseline), "endpoint order must not matter")
}

func TestKORCCatalogEpoch_ChangesWith(t *testing.T) {
	cp := catalogEpochControlPlane()
	glance := func() *c5c3v1alpha1.KeystoneService {
		return settledRegistration("openstack", "glance", cp, "public=https://glance.example")
	}
	nova := func() *c5c3v1alpha1.KeystoneService {
		return settledRegistration("openstack", "nova", cp, "public=https://nova.example")
	}
	baseline := mustEpoch(t, cp, glance(), nova())

	for _, tc := range []struct {
		name          string
		cp            *c5c3v1alpha1.ControlPlane
		registrations func() []*c5c3v1alpha1.KeystoneService
	}{
		{name: "an endpoint URL change", registrations: func() []*c5c3v1alpha1.KeystoneService {
			changed := glance()
			changed.Spec.Catalog.Endpoints[0].URL = "https://images.example"
			return []*c5c3v1alpha1.KeystoneService{changed, nova()}
		}},
		{name: "an added endpoint", registrations: func() []*c5c3v1alpha1.KeystoneService {
			added := glance()
			added.Spec.Catalog.Endpoints = append(added.Spec.Catalog.Endpoints, c5c3v1alpha1.KeystoneServiceEndpointSpec{
				Interface: c5c3v1alpha1.ExternalEndpointTypeInternal, URL: "http://glance.openstack.svc",
			})
			return []*c5c3v1alpha1.KeystoneService{added, nova()}
		}},
		{name: "an added registration", registrations: func() []*c5c3v1alpha1.KeystoneService {
			return []*c5c3v1alpha1.KeystoneService{
				glance(), nova(), settledRegistration("team-a", "custom", cp, "public=https://custom"),
			}
		}},
		{name: "a removed registration", registrations: func() []*c5c3v1alpha1.KeystoneService {
			return []*c5c3v1alpha1.KeystoneService{glance()}
		}},
		{name: "a different ControlPlane UID", cp: func() *c5c3v1alpha1.ControlPlane {
			recreated := catalogEpochControlPlane()
			recreated.UID = types.UID("cp-uid-recreated")
			return recreated
		}(), registrations: func() []*c5c3v1alpha1.KeystoneService {
			return []*c5c3v1alpha1.KeystoneService{glance(), nova()}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			plane := cp
			if tc.cp != nil {
				plane = tc.cp
			}
			g.Expect(mustEpoch(t, plane, tc.registrations()...)).NotTo(Equal(baseline))
		})
	}
}

func TestKORCCatalogEpoch_SkipsTerminating(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := catalogEpochControlPlane()
	glance := settledRegistration("openstack", "glance", cp, "public=https://glance.example")
	baseline := mustEpoch(t, cp, glance)

	terminating := settledRegistration("team-a", "custom", cp, "public=https://custom")
	terminating.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	g.Expect(mustEpoch(t, cp, glance, terminating)).To(Equal(baseline),
		"a Terminating registration is not part of the epoch")

	terminating.Status.Conditions[0].Status = metav1.ConditionFalse
	epoch, settled := korcCatalogEpoch(cp, registrationItems(glance, terminating))
	g.Expect(settled).To(BeTrue(), "an unsettled Terminating registration must not hold the epoch")
	g.Expect(epoch).To(Equal(baseline))
}

// TestKORCCatalogEpoch_SkipsNamespacesThePlaneDoesNotAdmit guards the
// allowlist: a registration from a namespace the plane does not admit projects
// nothing and stays CatalogReady=False/NamespaceNotAllowed for good, so it must
// neither enter the epoch nor hold it.
func TestKORCCatalogEpoch_SkipsNamespacesThePlaneDoesNotAdmit(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := catalogEpochControlPlane()
	glance := settledRegistration("openstack", "glance", cp, "public=https://glance.example")
	baseline := mustEpoch(t, cp, glance)

	notAdmitted := settledRegistration("team-b", "custom", cp, "public=https://custom")
	g.Expect(mustEpoch(t, cp, glance, notAdmitted)).To(Equal(baseline),
		"a registration from a namespace the plane does not admit is not part of the epoch")

	notAdmitted.Status.Conditions[0].Status = metav1.ConditionFalse
	notAdmitted.Status.Conditions[0].Reason = reasonKeystoneServiceNamespaceNotAllowed
	epoch, settled := korcCatalogEpoch(cp, registrationItems(glance, notAdmitted))
	g.Expect(settled).To(BeTrue(), "a NamespaceNotAllowed registration must not hold the epoch")
	g.Expect(epoch).To(Equal(baseline))
}

func TestKORCCatalogEpochAnnotationKey(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := dbCredManagedControlPlane()
	key := korcCatalogEpochAnnotationKey(cp)

	sum := sha256.Sum256([]byte("openstack/controlplane"))
	g.Expect(key).To(Equal(korcCatalogEpochAnnotationPrefix + hex.EncodeToString(sum[:])[:10]))
	g.Expect(validation.IsQualifiedName(key)).To(BeEmpty(), "the key must be a valid annotation key")

	otherNamespace := cp.DeepCopy()
	otherNamespace.Namespace = "tenant"
	otherName := cp.DeepCopy()
	otherName.Name = "second"
	g.Expect(korcCatalogEpochAnnotationKey(otherNamespace)).NotTo(Equal(key))
	g.Expect(korcCatalogEpochAnnotationKey(otherName)).NotTo(Equal(key))
}

// --- reconcileKORCCatalogRefresh ---

func TestReconcileKORCCatalogRefresh_GatedOnServiceAccountsReady(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate func(*c5c3v1alpha1.ControlPlane)
	}{
		{name: "ServiceAccountsReady absent", gate: func(cp *c5c3v1alpha1.ControlPlane) {
			cp.Status.Conditions = nil
		}},
		{name: "ServiceAccountsReady=False", gate: func(cp *c5c3v1alpha1.ControlPlane) {
			cp.Status.Conditions[0].Status = metav1.ConditionFalse
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := catalogRefreshControlPlane()
			tc.gate(cp)
			h := newCatalogRefreshHarness(t, interceptor.Funcs{},
				settledRegistration("openstack", "glance", cp, "public=https://glance.example"), korcDeployment(nil))

			g.Expect(h.run(t, cp)).To(Succeed())

			g.Expect(h.gets).To(BeZero(), "the gate must hold before the Deployment is read")
			g.Expect(h.patches).To(BeZero())
			g.Expect(h.templateAnnotations(t)).To(BeEmpty())
			g.Expect(h.rec.Events).NotTo(Receive())
		})
	}
}

func TestReconcileKORCCatalogRefresh_NothingSettledNothingWritten(t *testing.T) {
	for _, tc := range []struct {
		name         string
		registration func(*c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService
	}{
		{name: "an unsettled registration", registration: func(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
			ks := settledRegistration("openstack", "glance", cp, "public=https://glance.example")
			ks.Status.Conditions = nil
			return ks
		}},
		{name: "no registration declares a catalog entry", registration: func(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
			return accountOnlyRegistration("openstack", "neutron-nova", cp)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := catalogRefreshControlPlane()
			h := newCatalogRefreshHarness(t, interceptor.Funcs{}, tc.registration(cp), korcDeployment(nil))

			g.Expect(h.run(t, cp)).To(Succeed())

			g.Expect(h.gets).To(BeZero())
			g.Expect(h.patches).To(BeZero())
			g.Expect(h.templateAnnotations(t)).To(BeEmpty())
			g.Expect(h.rec.Events).NotTo(Receive())
		})
	}
}

func TestReconcileKORCCatalogRefresh_RecordsEpochAndRestarts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		annotations map[string]string
	}{
		{name: "pod template with annotations", annotations: map[string]string{
			"kubectl.kubernetes.io/default-container": "manager",
		}},
		{name: "pod template without an annotations map"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := catalogRefreshControlPlane()
			glance := settledRegistration("openstack", "glance", cp, "public=https://glance.example")
			h := newCatalogRefreshHarness(t, interceptor.Funcs{}, glance, korcDeployment(tc.annotations))

			g.Expect(h.run(t, cp)).To(Succeed())

			g.Expect(h.templateAnnotations(t)).To(HaveKeyWithValue(
				MatchRegexp(`^c5c3\.io/korc-catalog-epoch-[0-9a-f]{10}$`),
				"openstack/controlplane="+mustEpoch(t, cp, glance)))
			g.Expect(h.rec.Events).To(Receive(Equal("Normal KORCRestarted restarted Deployment " +
				"orc-system/orc-controller-manager: the service catalog registered through ControlPlane " +
				"openstack/controlplane changed")))
		})
	}
}

func TestReconcileKORCCatalogRefresh_SecondPassIsNoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := catalogRefreshControlPlane()
	h := newCatalogRefreshHarness(t, interceptor.Funcs{},
		settledRegistration("openstack", "glance", cp, "public=https://glance.example"), korcDeployment(nil))

	g.Expect(h.run(t, cp)).To(Succeed())
	g.Expect(h.rec.Events).To(Receive())
	g.Expect(h.run(t, cp)).To(Succeed())

	g.Expect(h.patches).To(Equal(1), "an unchanged epoch must not patch again")
	g.Expect(h.rec.Events).NotTo(Receive(), "an unchanged epoch must not announce a restart")
}

func TestReconcileKORCCatalogRefresh_ChangedEpochPatchesAgain(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := catalogRefreshControlPlane()
	before := settledRegistration("openstack", "glance", cp, "public=https://glance.example")
	key := korcCatalogEpochAnnotationKey(cp)
	stale := "openstack/controlplane=" + mustEpoch(t, cp, before)
	after := settledRegistration("openstack", "glance", cp, "public=https://images.example")
	after.Generation = 2
	after.Status.Conditions[0].ObservedGeneration = 2
	h := newCatalogRefreshHarness(t, interceptor.Funcs{}, after, korcDeployment(map[string]string{key: stale}))

	g.Expect(h.run(t, cp)).To(Succeed())

	g.Expect(h.patches).To(Equal(1))
	g.Expect(h.templateAnnotations(t)).To(HaveKeyWithValue(key, "openstack/controlplane="+mustEpoch(t, cp, after)))
	g.Expect(h.templateAnnotations(t)[key]).NotTo(Equal(stale))
	g.Expect(h.rec.Events).To(Receive(ContainSubstring("KORCRestarted")))
}

func TestReconcileKORCCatalogRefresh_PreservesOtherAnnotations(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := catalogRefreshControlPlane()
	second := cp.DeepCopy()
	second.Name = "second"
	secondKey := korcCatalogEpochAnnotationKey(second)
	h := newCatalogRefreshHarness(t, interceptor.Funcs{},
		settledRegistration("openstack", "glance", cp, "public=https://glance.example"),
		korcDeployment(map[string]string{
			"kubectl.kubernetes.io/default-container": "manager",
			secondKey: "openstack/second=0123456789abcdef",
		}))

	g.Expect(h.run(t, cp)).To(Succeed())

	annotations := h.templateAnnotations(t)
	g.Expect(annotations).To(HaveLen(3))
	g.Expect(annotations).To(HaveKeyWithValue("kubectl.kubernetes.io/default-container", "manager"))
	g.Expect(annotations).To(HaveKeyWithValue(secondKey, "openstack/second=0123456789abcdef"),
		"another ControlPlane's epoch must survive the patch")
	g.Expect(annotations).To(HaveKey(korcCatalogEpochAnnotationKey(cp)))
}

func TestReconcileKORCCatalogRefresh_CountsOnlyThisPlanesRegistrations(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := catalogRefreshControlPlane()
	glance := settledRegistration("openstack", "glance", cp, "public=https://glance.example")
	standalone := settledRegistration("team-a", "custom", cp, "public=https://custom.example")
	// A same-named ControlPlane in another namespace: its pending registration
	// would hold the epoch if it were counted here.
	elsewhere := cp.DeepCopy()
	elsewhere.Namespace = "elsewhere"
	foreign := settledRegistration("team-a", "foreign", elsewhere, "public=https://foreign.example")
	foreign.Status.Conditions = nil
	// A registration of this plane from a namespace it does not admit: it stays
	// CatalogReady=False for good and would hold every restart if it were counted.
	notAdmitted := settledRegistration("team-b", "custom", cp, "public=https://custom.example")
	notAdmitted.Status.Conditions[0].Status = metav1.ConditionFalse
	notAdmitted.Status.Conditions[0].Reason = reasonKeystoneServiceNamespaceNotAllowed
	h := newCatalogRefreshHarness(t, interceptor.Funcs{}, glance, standalone, foreign, notAdmitted, korcDeployment(nil))

	g.Expect(h.run(t, cp)).To(Succeed())

	g.Expect(h.templateAnnotations(t)).To(HaveKeyWithValue(korcCatalogEpochAnnotationKey(cp),
		"openstack/controlplane="+mustEpoch(t, cp, glance, standalone)))
}

func TestReconcileKORCCatalogRefresh_PatchesAsCobaltcoreOperator(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := catalogRefreshControlPlane()
	h := newCatalogRefreshHarness(t, interceptor.Funcs{},
		settledRegistration("openstack", "glance", cp, "public=https://glance.example"), korcDeployment(nil))

	g.Expect(h.run(t, cp)).To(Succeed())

	g.Expect(h.patches).To(Equal(1))
	g.Expect(h.patchOwner).To(Equal("cobaltcore-operator"),
		"the k-orc Flux Kustomization reverts kubectl-owned fields, not this manager's")
}

// TestReconcileKORCCatalogRefresh_ReadsThroughTheUncachedReader pins where the
// Deployment is read from. The cached client fails every Deployment read, so
// the restart only happens through the uncached reader.
func TestReconcileKORCCatalogRefresh_ReadsThroughTheUncachedReader(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := catalogRefreshControlPlane()
	h := newCatalogRefreshHarness(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			if _, isDeployment := obj.(*appsv1.Deployment); isDeployment {
				return errors.New("no informer for Deployment has synced")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, settledRegistration("openstack", "glance", cp, "public=https://glance.example"), korcDeployment(nil))
	uncachedGets := 0
	h.r.APIReader = interceptor.NewClient(fake.NewClientBuilder().WithScheme(h.r.Scheme).
		WithObjects(korcDeployment(nil)).Build(), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			uncachedGets++
			return cl.Get(ctx, key, obj, opts...)
		},
	})

	g.Expect(h.run(t, cp)).To(Succeed())

	g.Expect(uncachedGets).To(Equal(1))
	g.Expect(h.gets).To(BeZero(), "the Deployment must never be read through the cached client")
	g.Expect(h.patches).To(Equal(1))
}

func TestReconcileKORCCatalogRefresh_MissingForbiddenOrDeniedIsSkipped(t *testing.T) {
	forbidden := apierrors.NewForbidden(deploymentsResource, korcDeploymentName, errRBACDenied)
	notFound := apierrors.NewNotFound(deploymentsResource, korcDeploymentName)
	// A webhook denial without a status code reaches the client as 400 BadRequest,
	// a ValidatingAdmissionPolicy denial as 422 Invalid.
	webhookDenied := apierrors.NewBadRequest(`admission webhook "validate.kyverno.svc-fail" denied the request`)
	policyDenied := apierrors.NewInvalid(schema.GroupKind{Group: "apps", Kind: "Deployment"}, korcDeploymentName,
		field.ErrorList{field.Forbidden(field.NewPath("spec", "template"), "denied by ValidatingAdmissionPolicy")})
	failPatch := func(err error) interceptor.Funcs {
		return interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch,
			...client.PatchOption,
		) error {
			return err
		}}
	}
	for _, tc := range []struct {
		name       string
		funcs      interceptor.Funcs
		deployment bool
		cause      string
	}{
		{name: "Get NotFound", cause: "not found"},
		{name: "Get Forbidden", deployment: true, cause: errRBACDenied.Error(), funcs: interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption,
			) error {
				if _, isDeployment := obj.(*appsv1.Deployment); isDeployment {
					return forbidden
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}},
		{name: "Patch Forbidden", deployment: true, cause: errRBACDenied.Error(), funcs: failPatch(forbidden)},
		{name: "Patch NotFound", deployment: true, cause: notFound.Error(), funcs: failPatch(notFound)},
		{name: "Patch denied by a webhook", deployment: true, cause: webhookDenied.Error(), funcs: failPatch(webhookDenied)},
		{name: "Patch denied by a policy", deployment: true, cause: policyDenied.Error(), funcs: failPatch(policyDenied)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := catalogRefreshControlPlane()
			objs := []client.Object{settledRegistration("openstack", "glance", cp, "public=https://glance.example")}
			if tc.deployment {
				objs = append(objs, korcDeployment(nil))
			}
			h := newCatalogRefreshHarness(t, tc.funcs, objs...)

			g.Expect(h.run(t, cp)).To(Succeed(), "a K-ORC this reconciler cannot restart is no reconcile error")

			g.Expect(h.rec.Events).To(Receive(And(
				HavePrefix("Warning KORCRestartSkipped Deployment orc-system/orc-controller-manager cannot be restarted ("),
				ContainSubstring(tc.cause),
				HaveSuffix("); K-ORC keeps its cached service catalog for up to half a token lifetime"),
			)))
			g.Expect(h.rec.Events).NotTo(Receive(ContainSubstring("KORCRestarted")))
		})
	}
}

func TestReconcileKORCCatalogRefresh_ErrorsAreWrapped(t *testing.T) {
	cause := apierrors.NewInternalError(errors.New("etcd is unavailable"))
	for _, tc := range []struct {
		name   string
		funcs  interceptor.Funcs
		prefix string
	}{
		{name: "List", prefix: "listing KeystoneServices for the K-ORC catalog refresh: ", funcs: interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return cause
			},
		}},
		{
			name: "Get", prefix: "reading Deployment orc-system/orc-controller-manager for the K-ORC catalog refresh: ",
			funcs: interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
					opts ...client.GetOption,
				) error {
					if _, isDeployment := obj.(*appsv1.Deployment); isDeployment {
						return cause
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			},
		},
		{
			name: "Patch", prefix: "restarting Deployment orc-system/orc-controller-manager for the K-ORC catalog refresh: ",
			funcs: interceptor.Funcs{
				Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					return cause
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := catalogRefreshControlPlane()
			h := newCatalogRefreshHarness(t, tc.funcs,
				settledRegistration("openstack", "glance", cp, "public=https://glance.example"), korcDeployment(nil))

			err := h.run(t, cp)

			g.Expect(err).To(MatchError(cause))
			g.Expect(err.Error()).To(HavePrefix(tc.prefix))
			g.Expect(h.rec.Events).NotTo(Receive(), "an error is retried, not announced")
		})
	}
}

func TestReconcileKORCCatalogRefresh_NilRecorder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deployment bool
	}{
		{name: "restart", deployment: true},
		{name: "skip on a missing Deployment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := catalogRefreshControlPlane()
			objs := []client.Object{settledRegistration("openstack", "glance", cp, "public=https://glance.example")}
			if tc.deployment {
				objs = append(objs, korcDeployment(nil))
			}
			h := newCatalogRefreshHarness(t, interceptor.Funcs{}, objs...)
			h.r.Recorder = nil

			g.Expect(func() { g.Expect(h.run(t, cp)).To(Succeed()) }).NotTo(Panic())
		})
	}
}
