// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

const (
	resolveNS       = "openstack"
	resolveMariaDB  = "mariadb"
	resolveMaxScale = "mxs"
)

// getCounter counts the Gets a fake client serves, in total and per kind, and
// fails the Gets of the kinds named in fail with the given error.
type getCounter struct {
	all      int
	mariadb  int
	maxscale int
	fail     map[string]error
}

func (g *getCounter) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			g.all++
			kind := ""
			switch obj.(type) {
			case *mariadbv1alpha1.MariaDB:
				g.mariadb++
				kind = "MariaDB"
			case *mariadbv1alpha1.MaxScale:
				g.maxscale++
				kind = "MaxScale"
			}
			if err, ok := g.fail[kind]; ok {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

func resolveScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	return s
}

func managedSpec() *commonv1.DatabaseSpec {
	return &commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: resolveMariaDB}}
}

// mariaDB returns a single-instance MariaDB; the mutators turn it into the
// topology a case needs.
func mariaDB(mutate ...func(*mariadbv1alpha1.MariaDB)) *mariadbv1alpha1.MariaDB {
	m := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: resolveMariaDB, Namespace: resolveNS}}
	for _, f := range mutate {
		f(m)
	}
	return m
}

func withGalera(enabled bool) func(*mariadbv1alpha1.MariaDB) {
	return func(m *mariadbv1alpha1.MariaDB) { m.Spec.Galera = &mariadbv1alpha1.Galera{Enabled: enabled} }
}

func withReplication(m *mariadbv1alpha1.MariaDB) {
	m.Spec.Replication = &mariadbv1alpha1.Replication{Enabled: true}
}

func withMaxScaleRef(namespace string) func(*mariadbv1alpha1.MariaDB) {
	return func(m *mariadbv1alpha1.MariaDB) {
		m.Spec.MaxScaleRef = &mariadbv1alpha1.ObjectReference{Name: resolveMaxScale, Namespace: namespace}
	}
}

// singleWriter returns the MaxScale service the resolver routes through.
func singleWriter() mariadbv1alpha1.MaxScaleService {
	return mariadbv1alpha1.MaxScaleService{
		Name:     "rw-router",
		Router:   mariadbv1alpha1.ServiceRouterReadWriteSplit,
		Params:   map[string]string{"max_slave_connections": "0"},
		Listener: mariadbv1alpha1.MaxScaleListener{Port: 3306},
	}
}

// maxScale returns a MaxScale for resolveMariaDB with the given services.
func maxScale(services ...mariadbv1alpha1.MaxScaleService) *mariadbv1alpha1.MaxScale {
	return &mariadbv1alpha1.MaxScale{
		ObjectMeta: metav1.ObjectMeta{Name: resolveMaxScale, Namespace: resolveNS},
		Spec: mariadbv1alpha1.MaxScaleSpec{
			MariaDBRef: &mariadbv1alpha1.MariaDBRef{ObjectReference: mariadbv1alpha1.ObjectReference{Name: resolveMariaDB}},
			Services:   services,
		},
	}
}

func service(mutate func(*mariadbv1alpha1.MaxScaleService)) mariadbv1alpha1.MaxScaleService {
	s := singleWriter()
	mutate(&s)
	return s
}

func TestResolveEndpoint(t *testing.T) {
	unavailable := apierrors.NewServiceUnavailable("etcd leader lost")
	noKind := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "k8s.mariadb.com", Kind: "MariaDB"}}
	notFound := func(kind string) error {
		return apierrors.NewNotFound(schema.GroupResource{Group: "k8s.mariadb.com", Resource: kind}, "x")
	}

	const (
		wantServiceHost  = "mariadb.openstack.svc"
		wantPrimaryHost  = "mariadb-primary.openstack.svc"
		wantMaxScaleHost = "mxs.openstack.svc"
	)

	tests := []struct {
		name    string
		db      *commonv1.DatabaseSpec
		objects []client.Object
		fail    map[string]error

		wantRoute    Route
		wantHost     string
		wantPort     int32
		wantBypass   string
		wantMaxScale string
		wantErr      []string
		wantDBGets   int
		wantMxsGets  int
	}{
		{
			name:      "brownfield spec returns its host without a read",
			db:        &commonv1.DatabaseSpec{Host: "db.example.com", Port: 3307},
			wantRoute: RouteExplicit, wantHost: "db.example.com", wantPort: 3307,
		},
		{
			name:      "missing MariaDB renders the Service without an error",
			db:        managedSpec(),
			wantRoute: RouteService, wantHost: wantServiceHost, wantDBGets: 1,
		},
		{
			name:      "unserved MariaDB kind renders the Service without an error",
			db:        managedSpec(),
			fail:      map[string]error{"MariaDB": noKind},
			wantRoute: RouteService, wantHost: wantServiceHost, wantDBGets: 1,
		},
		{
			name:       "other MariaDB read error is returned",
			db:         managedSpec(),
			fail:       map[string]error{"MariaDB": unavailable},
			wantErr:    []string{"getting MariaDB openstack/mariadb"},
			wantDBGets: 1,
		},
		{
			name:      "single instance without galera renders the Service",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB()},
			wantRoute: RouteService, wantHost: wantServiceHost, wantDBGets: 1,
		},
		{
			name:      "galera disabled and no replication renders the Service",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(false))},
			wantRoute: RouteService, wantHost: wantServiceHost, wantDBGets: 1,
		},
		{
			name:      "galera renders the primary Service",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true))},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantDBGets: 1,
		},
		{
			name:      "replication renders the primary Service",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withReplication)},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantDBGets: 1,
		},
		{
			name:      "the primary Service keeps a non-default port",
			db:        &commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: resolveMariaDB}, Port: 3307},
			objects:   []client.Object{mariaDB(withGalera(true))},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantPort: 3307, wantDBGets: 1,
		},
		{
			name:      "single-writer MaxScale is the route",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef("")), maxScale(singleWriter())},
			wantRoute: RouteMaxScale, wantHost: wantMaxScaleHost, wantMaxScale: resolveMaxScale, wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "a single-writer listener on the spec's non-default port is the route",
			db:        &commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: resolveMariaDB}, Port: 3307},
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef("")), maxScale(service(func(s *mariadbv1alpha1.MaxScaleService) { s.Listener.Port = 3307 }))},
			wantRoute: RouteMaxScale, wantHost: wantMaxScaleHost, wantPort: 3307, wantMaxScale: resolveMaxScale, wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name: "max_slave_connections with blanks around 0 qualifies",
			db:   managedSpec(),
			objects: []client.Object{
				mariaDB(withGalera(true), withMaxScaleRef(resolveNS)),
				maxScale(service(func(s *mariadbv1alpha1.MaxScaleService) { s.Params["max_slave_connections"] = " 0 " })),
			},
			wantRoute: RouteMaxScale, wantHost: wantMaxScaleHost, wantMaxScale: resolveMaxScale, wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name: "a qualifying service after a read-splitting one qualifies",
			db:   managedSpec(),
			objects: []client.Object{
				mariaDB(withGalera(true), withMaxScaleRef("")),
				maxScale(service(func(s *mariadbv1alpha1.MaxScaleService) {
					s.Name = "rw-split"
					s.Params = nil
					s.Listener.Port = 3307
				}), singleWriter()),
			},
			wantRoute: RouteMaxScale, wantHost: wantMaxScaleHost, wantMaxScale: resolveMaxScale, wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "missing MaxScale is bypassed for the primary Service",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef(""))},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleNotFound, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "unserved MaxScale kind is bypassed as not found",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef(""))},
			fail:      map[string]error{"MaxScale": noKind},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleNotFound, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "MaxScale NotFound from the API server is bypassed",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef(""))},
			fail:      map[string]error{"MaxScale": notFound("maxscales")},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleNotFound, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:       "other MaxScale read error is returned",
			db:         managedSpec(),
			objects:    []client.Object{mariaDB(withGalera(true), withMaxScaleRef(""))},
			fail:       map[string]error{"MaxScale": unavailable},
			wantErr:    []string{"getting MaxScale openstack/mxs"},
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "MaxScale in another namespace is bypassed without a read",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef("infra")), maxScale(singleWriter())},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleOtherNamespace, wantMaxScale: resolveMaxScale,
			wantDBGets: 1,
		},
		{
			name: "MaxScale without mariaDbRef is bypassed",
			db:   managedSpec(),
			objects: []client.Object{
				mariaDB(withGalera(true), withMaxScaleRef("")),
				func() client.Object { m := maxScale(singleWriter()); m.Spec.MariaDBRef = nil; return m }(),
			},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleServesOtherMariaDB, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name: "MaxScale serving another MariaDB is bypassed",
			db:   managedSpec(),
			objects: []client.Object{
				mariaDB(withGalera(true), withMaxScaleRef("")),
				func() client.Object { m := maxScale(singleWriter()); m.Spec.MariaDBRef.Name = "other-db"; return m }(),
			},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleServesOtherMariaDB, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name: "MaxScale serving a same-named MariaDB in another namespace is bypassed",
			db:   managedSpec(),
			objects: []client.Object{
				mariaDB(withGalera(true), withMaxScaleRef("")),
				func() client.Object { m := maxScale(singleWriter()); m.Spec.MariaDBRef.Namespace = "other"; return m }(),
			},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleServesOtherMariaDB, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name: "mariaDbRef naming the MariaDB's own namespace qualifies",
			db:   managedSpec(),
			objects: []client.Object{
				mariaDB(withGalera(true), withMaxScaleRef("")),
				func() client.Object { m := maxScale(singleWriter()); m.Spec.MariaDBRef.Namespace = resolveNS; return m }(),
			},
			wantRoute: RouteMaxScale, wantHost: wantMaxScaleHost, wantMaxScale: resolveMaxScale, wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "MaxScale without services is not a single writer",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef("")), maxScale()},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleNotSingleWriter, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "service with nil params is not a single writer",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef("")), maxScale(service(func(s *mariadbv1alpha1.MaxScaleService) { s.Params = nil }))},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleNotSingleWriter, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "max_slave_connections 1 is not a single writer",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef("")), maxScale(service(func(s *mariadbv1alpha1.MaxScaleService) { s.Params["max_slave_connections"] = "1" }))},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleNotSingleWriter, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "readconnroute is not a single writer",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef("")), maxScale(service(func(s *mariadbv1alpha1.MaxScaleService) { s.Router = mariadbv1alpha1.ServiceRouterReadConnRoute }))},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleNotSingleWriter, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "listener on another port is not a single writer",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef("")), maxScale(service(func(s *mariadbv1alpha1.MaxScaleService) { s.Listener.Port = 3307 }))},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleNotSingleWriter, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "suspended service is not a single writer",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef("")), maxScale(service(func(s *mariadbv1alpha1.MaxScaleService) { s.Suspend = true }))},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleNotSingleWriter, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "suspended listener is not a single writer",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withGalera(true), withMaxScaleRef("")), maxScale(service(func(s *mariadbv1alpha1.MaxScaleService) { s.Listener.Suspend = true }))},
			wantRoute: RoutePrimary, wantHost: wantPrimaryHost, wantBypass: BypassMaxScaleNotSingleWriter, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
		{
			name:      "bypassed MaxScale of a single instance falls back to the Service",
			db:        managedSpec(),
			objects:   []client.Object{mariaDB(withMaxScaleRef("")), maxScale(service(func(s *mariadbv1alpha1.MaxScaleService) { s.Params["max_slave_connections"] = "1" }))},
			wantRoute: RouteService, wantHost: wantServiceHost, wantBypass: BypassMaxScaleNotSingleWriter, wantMaxScale: resolveMaxScale,
			wantDBGets: 1, wantMxsGets: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			counter := &getCounter{fail: tc.fail}
			c := fake.NewClientBuilder().
				WithScheme(resolveScheme()).
				WithObjects(tc.objects...).
				WithInterceptorFuncs(counter.funcs()).
				Build()

			ep, err := ResolveEndpoint(context.Background(), c, tc.db, resolveNS)

			g.Expect(counter.all).To(Equal(tc.wantDBGets+tc.wantMxsGets), "the resolver reads nothing but the MariaDB and the MaxScale")
			g.Expect(counter.mariadb).To(Equal(tc.wantDBGets), "MariaDB Gets")
			g.Expect(counter.maxscale).To(Equal(tc.wantMxsGets), "MaxScale Gets")
			if len(tc.wantErr) > 0 {
				g.Expect(err).To(HaveOccurred())
				for _, want := range tc.wantErr {
					g.Expect(err.Error()).To(ContainSubstring(want))
				}
				g.Expect(errors.Is(err, unavailable)).To(BeTrue(), "the error must wrap its cause")
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			wantPort := tc.wantPort
			if wantPort == 0 {
				wantPort = 3306
			}
			g.Expect(ep).To(Equal(Endpoint{
				Host:         tc.wantHost,
				Port:         wantPort,
				Route:        tc.wantRoute,
				BypassReason: tc.wantBypass,
				MaxScale:     tc.wantMaxScale,
			}))
		})
	}
}

func TestEndpointHostPort(t *testing.T) {
	g := NewWithT(t)
	g.Expect(Endpoint{Host: "db.example.com", Port: 3307}.HostPort()).To(Equal("db.example.com:3307"))
	g.Expect(Endpoint{Host: "mariadb-primary.openstack.svc", Port: 3306}.HostPort()).
		To(Equal("mariadb-primary.openstack.svc:3306"))
}
