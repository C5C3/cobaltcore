// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneCatalogEntry reconciler: the catalog consent, the
// collision probe, the projected rows and their waits, the sweep of undeclared
// endpoints, the freeze, the teardown and the ControlPlane watch mapping.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

const kceTestName = "workflow-dns"

// keystoneCatalogEntryCR returns the order "workflow-dns" in kuTestNamespace:
// type dns named designate with a public and an internal endpoint, already
// carrying the teardown finalizer.
func keystoneCatalogEntryCR() *c5c3v1alpha1.KeystoneCatalogEntry {
	return &c5c3v1alpha1.KeystoneCatalogEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:       kceTestName,
			Namespace:  kuTestNamespace,
			Generation: 1,
			UID:        types.UID("kce-uid"),
			Finalizers: []string{keystoneCatalogEntryFinalizerName},
		},
		Spec: c5c3v1alpha1.KeystoneCatalogEntrySpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "default"},
			ServiceType:     "dns",
			ServiceName:     "designate",
			Endpoints: []c5c3v1alpha1.KeystoneServiceEndpointSpec{
				{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://dns.example.test/v2"},
				{Interface: c5c3v1alpha1.ExternalEndpointTypeInternal, URL: "http://designate-api.tenant-a.svc:9001/v2"},
			},
		},
	}
}

// kceControlPlane assigns kuTestNamespace on the management cluster, with
// allowCatalogEntries as given.
func kceControlPlane(allow bool) *c5c3v1alpha1.ControlPlane {
	entry := assignOn(kuTestNamespace, c5c3v1alpha1.ManagementCluster)
	entry.AllowCatalogEntries = allow
	return keystoneUserControlPlane(entry)
}

func newKCEHarness(t *testing.T, mgmtFuncs *interceptor.Funcs, objs ...client.Object) *orderHarness {
	t.Helper()
	return newOrderHarness(t, c5c3v1alpha1.ManagementCluster,
		types.NamespacedName{Namespace: kuTestNamespace, Name: kceTestName}, mgmtFuncs, nil, objs,
		func(mgmt client.Client, resolver commonmulticluster.ClusterResolver) orderReconciler {
			return &KeystoneCatalogEntryReconciler{Client: mgmt, Scheme: mgmt.Scheme(), Resolver: resolver}
		})
}

func kceGet(t *testing.T, h *orderHarness) *c5c3v1alpha1.KeystoneCatalogEntry {
	t.Helper()
	return reloadOrder[c5c3v1alpha1.KeystoneCatalogEntry](t, h)
}

func kceCondition(order *c5c3v1alpha1.KeystoneCatalogEntry) *metav1.Condition {
	return conditions.GetCondition(order.Status.Conditions, conditionTypeKeystoneCatalogEntryCatalogReady)
}

// kceLabelled counts the Services, Endpoints and Regions in the ControlPlane's
// namespace that carry the order's name label.
func kceLabelled(t *testing.T, c client.Client) int {
	t.Helper()
	key := keystoneCatalogEntryLabelKeys.Name
	return labelledIn(t, c, &orcv1alpha1.ServiceList{}, key, kceTestName) +
		labelledIn(t, c, &orcv1alpha1.EndpointList{}, key, kceTestName) +
		labelledIn(t, c, &orcv1alpha1.RegionList{}, key, kceTestName)
}

// kceConvergedChildren returns the order's managed Service, Region import and
// one Endpoint per declared interface as K-ORC reports them once the rows
// exist, with ids s-1, RegionOne and e-pub / e-int.
func kceConvergedChildren(order *c5c3v1alpha1.KeystoneCatalogEntry) []client.Object {
	labels := keystoneCatalogEntryRef(order, "").childLabels()
	service := managedCatalogServiceChild(keystoneCatalogEntryServiceRef(order, ""), "default",
		order.Spec.ServiceType, keystoneCatalogEntryName(order), orcv1alpha1.CloudCredentialsReference{})
	service.Labels = labels
	service.Status = orcv1alpha1.ServiceStatus{Conditions: availableImportConditions(), ID: ptr.To("s-1")}
	region := unmanagedRegionImport(keystoneCatalogEntryRegionRef(order, ""), "default", "RegionOne",
		orcv1alpha1.CloudCredentialsReference{})
	region.Labels = labels
	region.Status = orcv1alpha1.RegionStatus{Conditions: availableImportConditions(), ID: ptr.To("RegionOne")}
	objs := []client.Object{service, region}
	ids := map[c5c3v1alpha1.ExternalEndpointType]string{
		c5c3v1alpha1.ExternalEndpointTypePublic: "e-pub", c5c3v1alpha1.ExternalEndpointTypeInternal: "e-int",
	}
	for _, ep := range order.Spec.Endpoints {
		endpoint := managedCatalogEndpointChild(keystoneCatalogEntryEndpointRef(order, "", ep.Interface), "default",
			string(ep.Interface), ep.URL, service.Name, region.Name, orcv1alpha1.CloudCredentialsReference{})
		endpoint.Labels = labels
		endpoint.Status = orcv1alpha1.EndpointStatus{Conditions: availableImportConditions(), ID: ptr.To(ids[ep.Interface])}
		objs = append(objs, endpoint)
	}
	return objs
}

func TestKeystoneCatalogEntry_WithoutTheFlagIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneCatalogEntryCR()
	order.Finalizers = nil
	h := newKCEHarness(t, nil, order, kceControlPlane(false))

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(time.Minute))
	got := kceGet(t, h)
	cond := kceCondition(got)
	g.Expect(cond.Reason).To(Equal(reasonKeystoneCatalogEntryNotAllowed))
	g.Expect(cond.Message).To(ContainSubstring("allowCatalogEntries"))
	g.Expect(cond.Message).To(ContainSubstring("spec.namespaceAssignments"))
	g.Expect(got.Finalizers).To(BeEmpty(), "an order its entry does not admit gets no finalizer")
	g.Expect(kceLabelled(t, h.mgmt)).To(BeZero())
}

func TestKeystoneCatalogEntry_ProbesThenProjectsTheRows(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneCatalogEntryCR()
	h := newKCEHarness(t, nil, order, kceControlPlane(true))

	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
	g.Expect(kceCondition(kceGet(t, h)).Reason).To(Equal(reasonProbingForCollision))

	prefix := keystoneCatalogEntryRef(order, "").childPrefix()
	g.Expect(prefix).To(MatchRegexp(`^workflow-dns-[0-9a-f]{8}-catalogentry-$`))
	labels := keystoneCatalogEntryRef(order, "").childLabels()
	probe := &orcv1alpha1.Service{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "service-probe"}, probe)).To(Succeed())
	g.Expect(*probe.Spec.Import.Filter.Type).To(Equal("dns"))
	g.Expect(string(*probe.Spec.Import.Filter.Name)).To(Equal("designate"))
	g.Expect(probe.Labels).To(Equal(labels))

	probe.Status.Conditions = pendingImportConditions(time.Minute)
	g.Expect(h.mgmt.Update(ctx, probe)).To(Succeed())
	result, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
	g.Expect(kceCondition(kceGet(t, h)).Reason).To(Equal(conditionReasonWaitingForCatalog))

	service := &orcv1alpha1.Service{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "service"}, service)).To(Succeed())
	g.Expect(service.Spec.Resource.Type).To(Equal("dns"))
	g.Expect(string(*service.Spec.Resource.Name)).To(Equal("designate"))
	g.Expect(service.Labels).To(Equal(labels))
	g.Expect(service.OwnerReferences).To(BeEmpty())
	region := &orcv1alpha1.Region{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "region"}, region)).To(Succeed())
	g.Expect(string(*region.Spec.Import.Filter.Name)).To(Equal(korcRegion(kceControlPlane(true))))
	g.Expect(region.Labels).To(Equal(labels))
	for _, iface := range []string{"public", "internal"} {
		endpoint := &orcv1alpha1.Endpoint{}
		g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "endpoint-" + iface}, endpoint)).
			To(Succeed(), iface)
		g.Expect(endpoint.Spec.Resource.Interface).To(Equal(iface))
		g.Expect(string(endpoint.Spec.Resource.ServiceRef)).To(Equal(prefix + "service"))
		g.Expect(string(*endpoint.Spec.Resource.RegionRef)).To(Equal(prefix + "region"))
		g.Expect(endpoint.Labels).To(Equal(labels))
	}
	err = h.mgmt.Get(ctx, client.ObjectKeyFromObject(probe), &orcv1alpha1.Service{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "an absent probe is dropped")
}

func TestKeystoneCatalogEntry_WithoutEndpointsRegistersTheServiceAlone(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneCatalogEntryCR()
	order.Spec.Endpoints = nil
	h := newKCEHarness(t, nil, append([]client.Object{order, kceControlPlane(true)}, kceConvergedChildren(order)...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	cond := kceCondition(kceGet(t, h))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogRegistered))
	g.Expect(cond.Message).To(ContainSubstring("0 endpoint(s)"))
	g.Expect(labelledIn(t, h.mgmt, &orcv1alpha1.EndpointList{}, keystoneCatalogEntryLabelKeys.Name, kceTestName)).
		To(BeZero())
}

func TestKeystoneCatalogEntry_CollisionProbe(t *testing.T) {
	cases := []struct {
		name       string
		conds      []metav1.Condition
		wantReason string
		wantSub    string
	}{
		{
			name: "the probe resolves an existing row", conds: availableImportConditions(),
			wantReason: reasonKeystoneServiceCatalogCollision, wantSub: "never takes over",
		},
		{name: "the probe is pending", wantReason: reasonProbingForCollision, wantSub: `type "dns" named "designate"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneCatalogEntryCR()
			probe := unmanagedServiceImport(keystoneCatalogEntryServiceProbeRef(order, ""), "default", "dns", "designate",
				orcv1alpha1.CloudCredentialsReference{})
			probe.Labels = keystoneCatalogEntryRef(order, "").childLabels()
			probe.Status.Conditions = tc.conds
			h := newKCEHarness(t, nil, order, probe, kceControlPlane(true))

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
			cond := kceCondition(kceGet(t, h))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantSub))
			err = h.mgmt.Get(context.Background(),
				types.NamespacedName{Namespace: "default", Name: keystoneCatalogEntryServiceRef(order, "")}, &orcv1alpha1.Service{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no managed Service is created")
		})
	}
}

func TestKeystoneCatalogEntry_AvailableRowsAreRegistered(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneCatalogEntryCR()
	h := newKCEHarness(t, nil, append([]client.Object{order, kceControlPlane(true)}, kceConvergedChildren(order)...)...)

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	got := kceGet(t, h)
	cond := kceCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogRegistered))
	g.Expect(cond.Message).To(Equal(`catalog entry "designate" of type "dns" is registered with 2 endpoint(s) in region "RegionOne"`))
	g.Expect(got.Status.ServiceID).To(Equal("s-1"))
	g.Expect(got.Status.ServiceName).To(Equal("designate"))
	g.Expect(got.Status.Endpoints).To(Equal([]c5c3v1alpha1.KeystoneServiceEndpointStatus{
		{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, ID: "e-pub"},
		{Interface: c5c3v1alpha1.ExternalEndpointTypeInternal, ID: "e-int"},
	}))
}

func TestKeystoneCatalogEntry_KORCOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(objs []client.Object)
		wantReason string
		wantSub    string
	}{
		{
			name: "a terminal error on the Service",
			mutate: func(objs []client.Object) {
				objs[0].(*orcv1alpha1.Service).Status.Conditions = terminalImportConditions("bad type")
			},
			wantReason: conditionReasonCatalogFailed, wantSub: "catalog Service",
		},
		{
			name: "a Region not resolved",
			mutate: func(objs []client.Object) {
				objs[1].(*orcv1alpha1.Region).Status.Conditions = pendingImportConditions(0)
			},
			wantReason: conditionReasonWaitingForCatalog, wantSub: `Keystone region "RegionOne"`,
		},
		{
			name: "an Endpoint not Available",
			mutate: func(objs []client.Object) {
				objs[3].(*orcv1alpha1.Endpoint).Status.Conditions = nil
			},
			wantReason: conditionReasonWaitingForCatalog, wantSub: "endpoint-internal",
		},
		{
			name: "a terminal error on the Region",
			mutate: func(objs []client.Object) {
				objs[1].(*orcv1alpha1.Region).Status.Conditions = terminalImportConditions("no such region")
			},
			wantReason: conditionReasonCatalogFailed, wantSub: "importing the catalog Region",
		},
		{
			name: "a terminal error on an Endpoint",
			mutate: func(objs []client.Object) {
				objs[2].(*orcv1alpha1.Endpoint).Status.Conditions = terminalImportConditions("bad url")
			},
			wantReason: conditionReasonCatalogFailed, wantSub: "registering the catalog Endpoint",
		},
		{
			name: "terminal errors on the Region and an Endpoint report the Region",
			mutate: func(objs []client.Object) {
				objs[1].(*orcv1alpha1.Region).Status.Conditions = terminalImportConditions("no such region")
				objs[2].(*orcv1alpha1.Endpoint).Status.Conditions = terminalImportConditions("bad url")
			},
			wantReason: conditionReasonCatalogFailed, wantSub: "importing the catalog Region",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneCatalogEntryCR()
			children := kceConvergedChildren(order)
			tc.mutate(children)
			h := newKCEHarness(t, nil, append([]client.Object{order, kceControlPlane(true)}, children...)...)

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
			cond := kceCondition(kceGet(t, h))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantSub))
		})
	}
}

func TestKeystoneCatalogEntry_UndeclaredEndpointIsRemoved(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneCatalogEntryCR()
	children := kceConvergedChildren(order)
	order.Spec.Endpoints = order.Spec.Endpoints[:1]
	internal := keystoneCatalogEntryEndpointRef(order, "", c5c3v1alpha1.ExternalEndpointTypeInternal)
	h := newKCEHarness(t, nil, append([]client.Object{order, kceControlPlane(true)}, children...)...)

	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
	cond := kceCondition(kceGet(t, h))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring(internal + " the spec no longer declares are being removed"))
	err = h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: internal}, &orcv1alpha1.Endpoint{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	got := kceGet(t, h)
	g.Expect(kceCondition(got).Reason).To(Equal(reasonKeystoneServiceCatalogRegistered))
	g.Expect(kceCondition(got).Message).To(ContainSubstring("1 endpoint(s)"))
	g.Expect(got.Status.Endpoints).To(HaveLen(1))
}

// TestKeystoneCatalogEntry_RegistrationErrorsAreReported covers the
// Kubernetes-level failures of the registration: a child of the order's name it
// did not create, a failed probe read and a failed Endpoint list. Each writes
// CatalogError and returns the error, so the pass backs off.
func TestKeystoneCatalogEntry_RegistrationErrorsAreReported(t *testing.T) {
	boom := errors.New("boom")
	// stranger returns obj without the order's labels, so ensureOrderChild
	// refuses to adopt it.
	stranger := func(obj client.Object) client.Object {
		obj.SetLabels(nil)
		return obj
	}
	cases := []struct {
		name    string
		funcs   func(order *c5c3v1alpha1.KeystoneCatalogEntry) *interceptor.Funcs
		seed    func(children []client.Object) []client.Object
		wantErr error
		wantSub string
	}{
		{
			name: "a Service the order did not create",
			seed: func(children []client.Object) []client.Object {
				return []client.Object{stranger(children[0])}
			},
			wantSub: "applying the catalog Service",
		},
		{
			name: "a Region import the order did not create",
			seed: func(children []client.Object) []client.Object {
				return []client.Object{children[0], stranger(children[1])}
			},
			wantSub: "applying the catalog Region import",
		},
		{
			name: "an Endpoint the order did not create",
			seed: func(children []client.Object) []client.Object {
				return []client.Object{children[0], children[1], stranger(children[2])}
			},
			wantSub: `applying the "public" catalog Endpoint`,
		},
		{
			name: "a failed read of the managed Service",
			funcs: func(order *c5c3v1alpha1.KeystoneCatalogEntry) *interceptor.Funcs {
				return &interceptor.Funcs{
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*orcv1alpha1.Service); ok && key.Name == keystoneCatalogEntryServiceRef(order, "") {
							return boom
						}
						return cl.Get(ctx, key, obj, opts...)
					},
				}
			},
			seed:    func(children []client.Object) []client.Object { return children },
			wantErr: boom, wantSub: "probing for a pre-existing catalog entry",
		},
		{
			name: "a failed list of the order's Endpoints",
			funcs: func(*c5c3v1alpha1.KeystoneCatalogEntry) *interceptor.Funcs {
				return &interceptor.Funcs{
					List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if _, ok := list.(*orcv1alpha1.EndpointList); ok {
							return boom
						}
						return cl.List(ctx, list, opts...)
					},
				}
			},
			seed:    func(children []client.Object) []client.Object { return children },
			wantErr: boom, wantSub: "removing undeclared catalog Endpoints",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneCatalogEntryCR()
			var funcs *interceptor.Funcs
			if tc.funcs != nil {
				funcs = tc.funcs(order)
			}
			objs := append([]client.Object{order, kceControlPlane(true)}, tc.seed(kceConvergedChildren(order))...)
			h := newKCEHarness(t, funcs, objs...)

			_, err := h.reconcile(context.Background())

			if tc.wantErr != nil {
				g.Expect(err).To(MatchError(tc.wantErr))
			} else {
				g.Expect(err).To(MatchError(ContainSubstring("refusing to adopt")))
			}
			cond := kceCondition(kceGet(t, h))
			g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogError))
			g.Expect(cond.Message).To(HavePrefix(tc.wantSub))
		})
	}
}

// TestKeystoneCatalogEntry_UndeclaredEndpointSweep pins the sweep of the rows
// the spec stopped declaring: it runs whatever the declared rows report, issues
// one delete per row while K-ORC holds it, and leaves an Endpoint outside the
// order's prefix alone.
func TestKeystoneCatalogEntry_UndeclaredEndpointSweep(t *testing.T) {
	// recordDeletes returns interceptor funcs that append every deleted name to
	// deleted.
	recordDeletes := func(deleted *[]string) *interceptor.Funcs {
		return &interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				*deleted = append(*deleted, obj.GetName())
				return cl.Delete(ctx, obj, opts...)
			},
		}
	}

	t.Run("a dropped row is removed while a declared one is not Available", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		order := keystoneCatalogEntryCR()
		children := kceConvergedChildren(order)
		children[2].(*orcv1alpha1.Endpoint).Status.Conditions = nil
		order.Spec.Endpoints = order.Spec.Endpoints[:1]
		internal := keystoneCatalogEntryEndpointRef(order, "", c5c3v1alpha1.ExternalEndpointTypeInternal)
		h := newKCEHarness(t, nil, append([]client.Object{order, kceControlPlane(true)}, children...)...)

		_, err := h.reconcile(ctx)

		g.Expect(err).NotTo(HaveOccurred())
		err = h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: internal}, &orcv1alpha1.Endpoint{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the dropped row does not wait on the declared one")
		cond := kceCondition(kceGet(t, h))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
		g.Expect(cond.Message).To(ContainSubstring("endpoint-public"), "the declared row's wait is still reported")
	})

	t.Run("a dropped row K-ORC still holds is deleted once and listed until released", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		order := keystoneCatalogEntryCR()
		children := kceConvergedChildren(order)
		children[3].SetFinalizers([]string{"openstack.k-orc.cloud/endpoint"})
		order.Spec.Endpoints = order.Spec.Endpoints[:1]
		internal := keystoneCatalogEntryEndpointRef(order, "", c5c3v1alpha1.ExternalEndpointTypeInternal)
		var deleted []string
		h := newKCEHarness(t, recordDeletes(&deleted),
			append([]client.Object{order, kceControlPlane(true)}, children...)...)

		for range 2 {
			result, err := h.reconcile(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
			cond := kceCondition(kceGet(t, h))
			g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
			g.Expect(cond.Message).To(ContainSubstring(internal))
		}
		g.Expect(deleted).To(Equal([]string{internal}), "a row K-ORC still holds is not deleted again")

		endpoint := &orcv1alpha1.Endpoint{}
		g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: internal}, endpoint)).To(Succeed())
		endpoint.Finalizers = nil
		g.Expect(h.mgmt.Update(ctx, endpoint)).To(Succeed())

		_, err := h.reconcile(ctx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(kceCondition(kceGet(t, h)).Reason).To(Equal(reasonKeystoneServiceCatalogRegistered))
	})

	t.Run("an Endpoint outside the order's prefix is never deleted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		order := keystoneCatalogEntryCR()
		foreign := &orcv1alpha1.Endpoint{ObjectMeta: metav1.ObjectMeta{
			Name: "foreign-endpoint", Namespace: "default", Labels: keystoneCatalogEntryRef(order, "").childLabels(),
		}}
		var deleted []string
		h := newKCEHarness(t, recordDeletes(&deleted),
			append([]client.Object{order, foreign, kceControlPlane(true)}, kceConvergedChildren(order)...)...)

		_, err := h.reconcile(ctx)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(deleted).To(BeEmpty())
		g.Expect(h.mgmt.Get(ctx, client.ObjectKeyFromObject(foreign), &orcv1alpha1.Endpoint{})).To(Succeed())
		g.Expect(kceCondition(kceGet(t, h)).Reason).To(Equal(reasonKeystoneServiceCatalogRegistered))
	})
}

// TestKeystoneCatalogEntry_ClearedFlagFreezes pins D2's freeze: clearing the
// flag after convergence refuses the order and leaves every row in place.
func TestKeystoneCatalogEntry_ClearedFlagFreezes(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneCatalogEntryCR()
	cp := kceControlPlane(true)
	h := newKCEHarness(t, nil, append([]client.Object{order, cp}, kceConvergedChildren(order)...)...)
	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kceCondition(kceGet(t, h)).Status).To(Equal(metav1.ConditionTrue))

	live := &c5c3v1alpha1.ControlPlane{}
	g.Expect(h.mgmt.Get(ctx, client.ObjectKeyFromObject(cp), live)).To(Succeed())
	live.Spec.NamespaceAssignments[0].AllowCatalogEntries = false
	g.Expect(h.mgmt.Update(ctx, live)).To(Succeed())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	got := kceGet(t, h)
	g.Expect(kceCondition(got).Reason).To(Equal(reasonKeystoneCatalogEntryNotAllowed))
	g.Expect(got.Finalizers).To(ContainElement(keystoneCatalogEntryFinalizerName))
	g.Expect(kceLabelled(t, h.mgmt)).To(Equal(4), "a frozen order revokes nothing")
}

func TestKeystoneCatalogEntry_DeleteRemovesTheRowsInOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneCatalogEntryCR()
	order.DeletionTimestamp = ptr.To(metav1.Now())
	children := kceConvergedChildren(order)
	children[1].SetFinalizers([]string{"openstack.k-orc.cloud/region"})
	probe := unmanagedServiceImport(keystoneCatalogEntryServiceProbeRef(order, ""), "default", "dns", "designate",
		orcv1alpha1.CloudCredentialsReference{})
	probe.Labels = keystoneCatalogEntryRef(order, "").childLabels()
	var deleted []string
	h := newKCEHarness(t, &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
	}, append([]client.Object{order, probe, kceControlPlane(true)}, children...)...)

	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	prefix := keystoneCatalogEntryRef(order, "").childPrefix()
	g.Expect(deleted).To(Equal([]string{
		prefix + "endpoint-internal", prefix + "endpoint-public", prefix + "service", prefix + "service-probe", prefix + "region",
	}))
	g.Expect(kceGet(t, h).Finalizers).To(ContainElement(keystoneCatalogEntryFinalizerName),
		"the finalizer is held while K-ORC still holds the Region import")

	region := &orcv1alpha1.Region{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "region"}, region)).To(Succeed())
	region.Finalizers = nil
	g.Expect(h.mgmt.Update(ctx, region)).To(Succeed())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kceGet(t, h)).To(BeNil())
	g.Expect(kceLabelled(t, h.mgmt)).To(BeZero())
}

func TestKeystoneCatalogEntry_DeleteFailsOpenWithoutTheControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneCatalogEntryCR()
	order.DeletionTimestamp = ptr.To(metav1.Now())
	children := kceConvergedChildren(order)
	children[0].SetFinalizers([]string{"openstack.k-orc.cloud/service"})
	h := newKCEHarness(t, nil, append([]client.Object{order}, children...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kceGet(t, h)).To(BeNil(), "without a plane the finalizer is released at once")
	g.Expect(kceLabelled(t, h.mgmt)).To(Equal(1), "only the Service K-ORC still holds is left")
}

// TestKeystoneCatalogEntry_ReconcileEntry covers the branches Reconcile answers
// itself: a cluster that does not resolve is skipped without a requeue, and a
// failed read of the order is returned.
func TestKeystoneCatalogEntry_ReconcileEntry(t *testing.T) {
	t.Run("an unresolvable cluster is skipped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := keystoneCatalogEntryCR()
		entry := assignOn(kuTestNamespace, kuTestCluster)
		entry.AllowCatalogEntries = true
		h := newOrderHarness(t, kuTestCluster, types.NamespacedName{Namespace: kuTestNamespace, Name: kceTestName},
			nil, nil, []client.Object{order, keystoneUserControlPlane(entry)},
			func(mgmt client.Client, _ commonmulticluster.ClusterResolver) orderReconciler {
				return &KeystoneCatalogEntryReconciler{
					Client: mgmt, Scheme: mgmt.Scheme(), Resolver: &childrenResolver{err: mcruntime.ErrClusterNotFound},
				}
			})

		result, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
		g.Expect(kceLabelled(t, h.mgmt)).To(BeZero(), "nothing is written for an unresolvable cluster")
		g.Expect(kceGet(t, h).Status.Conditions).To(BeEmpty())
	})

	t.Run("a failed read of the order is returned", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		h := newKCEHarness(t, &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneCatalogEntry); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}, keystoneCatalogEntryCR(), kceControlPlane(true))

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(ContainSubstring("fetching KeystoneCatalogEntry"))
	})
}

func TestControlPlaneToKeystoneCatalogEntriesMapper(t *testing.T) {
	g := NewGomegaWithT(t)

	explicit := keystoneCatalogEntryCR()
	defaulted := keystoneCatalogEntryCR()
	defaulted.Name, defaulted.Namespace = "same-namespace", "default"
	defaulted.Spec.ControlPlaneRef.Namespace = ""
	elsewhere := keystoneCatalogEntryCR()
	elsewhere.Name = "other-plane"
	elsewhere.Spec.ControlPlaneRef.Namespace = "other"
	c := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithObjects(explicit, defaulted, elsewhere).
		WithIndex(&c5c3v1alpha1.KeystoneCatalogEntry{}, KeystoneCatalogEntryControlPlaneRefIndexKey,
			keystoneCatalogEntryControlPlaneRefExtractor).
		Build()
	mapper := controlPlaneToKeystoneCatalogEntriesMapper(c)

	g.Expect(mapper(context.Background(), ksControlPlane())).To(ConsistOf(
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(explicit)},
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(defaulted)},
	))
	unrelated := ksControlPlane()
	unrelated.Name = "another"
	g.Expect(mapper(context.Background(), unrelated)).To(BeEmpty())

	failing := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	}).Build()
	g.Expect(controlPlaneToKeystoneCatalogEntriesMapper(failing)(context.Background(), ksControlPlane())).To(BeNil())
}

func TestKeystoneCatalogEntryControlPlaneRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneCatalogEntryCR()
	g.Expect(keystoneCatalogEntryControlPlaneRefExtractor(order)).To(Equal([]string{"default/cp"}))
	order.Spec.ControlPlaneRef.Namespace = ""
	g.Expect(keystoneCatalogEntryControlPlaneRefExtractor(order)).To(Equal([]string{"tenant-a/cp"}),
		"an empty namespace defaults to the order's own")
	order.Spec.ControlPlaneRef.Name = ""
	g.Expect(keystoneCatalogEntryControlPlaneRefExtractor(order)).To(BeNil())
	g.Expect(keystoneCatalogEntryControlPlaneRefExtractor(ksControlPlane())).To(BeNil())
}
