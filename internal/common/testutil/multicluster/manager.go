// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
)

// UnprobeableManager is a multicluster manager whose local cluster cannot say
// whether it serves kind: its RESTMapper fails every lookup of kind with err,
// which must not be a no-match, and maps every other kind the scheme knows.
// That is the setup probe meeting an API server that does not answer, which a
// reconciler's SetupWithManager has to fail on rather than start without the
// watch. Nothing listens at the manager's host, and the manager is never
// started, so a setup run against it reaches no API server.
func UnprobeableManager(t *testing.T, scheme *runtime.Scheme, kind schema.GroupKind, err error) mcmanager.Manager {
	t.Helper()

	served := meta.NewDefaultRESTMapper(nil)
	for gvk := range scheme.AllKnownTypes() {
		served.Add(gvk, meta.RESTScopeNamespace)
	}
	mgr, mgrErr := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		MapperProvider: func(*rest.Config, *http.Client) (meta.RESTMapper, error) {
			return kindFailingMapper{RESTMapper: served, kind: kind, err: err}, nil
		},
	})
	if mgrErr != nil {
		t.Fatalf("building the manager: %v", mgrErr)
	}
	mcMgr, mgrErr := mcmanager.WithMultiCluster(mgr, nil)
	if mgrErr != nil {
		t.Fatalf("wrapping the manager: %v", mgrErr)
	}
	return mcMgr
}

// kindFailingMapper fails every RESTMapping of kind with err and hands every
// other lookup to the embedded mapper.
type kindFailingMapper struct {
	meta.RESTMapper
	kind schema.GroupKind
	err  error
}

func (m kindFailingMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if gk == m.kind {
		return nil, m.err
	}
	return m.RESTMapper.RESTMapping(gk, versions...)
}
