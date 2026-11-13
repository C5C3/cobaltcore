// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"fmt"
	"strings"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// Route names the address a database connection goes to.
type Route string

const (
	// RouteExplicit is a host and port stated outright rather than resolved
	// from a MariaDB: a brownfield DatabaseSpec's host, or an address a caller
	// supplies, such as a ControlPlane's publishedDatabaseEndpoint.
	RouteExplicit Route = "Explicit"
	// RouteService is the MariaDB's own Service, <name>.<namespace>.svc. It is
	// the route of a single instance and of a MariaDB the reader cannot read.
	RouteService Route = "Service"
	// RoutePrimary is the <name>-primary Service mariadb-operator publishes for
	// a Galera or replication MariaDB. It selects the current primary only.
	RoutePrimary Route = "Primary"
	// RouteMaxScale is the Service of a MaxScale that sends every statement to
	// the current primary, <maxscale>.<namespace>.svc.
	RouteMaxScale Route = "MaxScale"
)

// The reasons ResolveEndpoint reports in Endpoint.BypassReason when the
// MariaDB names a MaxScale in spec.maxScaleRef that the connection does not
// go through.
const (
	// BypassMaxScaleOtherNamespace: maxScaleRef names another namespace, which a
	// namespace-scoped operator cannot read.
	BypassMaxScaleOtherNamespace = "MaxScaleOtherNamespace"
	// BypassMaxScaleNotFound: the MaxScale does not exist, or the cluster does
	// not serve the kind.
	BypassMaxScaleNotFound = "MaxScaleNotFound"
	// BypassMaxScaleServesOtherMariaDB: the MaxScale's spec.mariaDbRef is unset
	// or names another MariaDB, by name or by a namespace other than the
	// MaxScale's.
	BypassMaxScaleServesOtherMariaDB = "MaxScaleServesOtherMariaDB"
	// BypassMaxScaleNotSingleWriter: no service of the MaxScale sends every
	// statement to the primary on the database port.
	BypassMaxScaleNotSingleWriter = "MaxScaleNotSingleWriter"
)

// Endpoint is the address a service connects to for a DatabaseSpec.
type Endpoint struct {
	// Host is the DNS name or address, without a port.
	Host string
	// Port is Port(db) on every route ResolveEndpoint returns. A caller that
	// builds an Endpoint from another address sets the port that address
	// names, and 0 when it has none.
	Port int32
	// Route names how Host was chosen.
	Route Route
	// BypassReason is set when the MariaDB names a MaxScale in
	// spec.maxScaleRef that the connection does not go through.
	BypassReason string
	// MaxScale is the name of the MaxScale the MariaDB names. It is set with
	// BypassReason and with RouteMaxScale.
	MaxScale string
}

// HostPort returns "<Host>:<Port>".
func (e Endpoint) HostPort() string {
	return fmt.Sprintf("%s:%d", e.Host, e.Port)
}

// BypassMessage names the MaxScale that MariaDB namespace/mariadb names and
// the connection does not go through, with BypassReason.
func (e Endpoint) BypassMessage(namespace, mariadb string) string {
	return fmt.Sprintf("MariaDB %s/%s names MaxScale %s, which the operator does not route through (%s)",
		namespace, mariadb, e.MaxScale, e.BypassReason)
}

// ResolveEndpoint returns the address a service connects to for db in
// namespace. A spec without ClusterRef returns its Host with RouteExplicit and
// issues no read. A managed spec reads the MariaDB that ClusterRef names and
// picks, in this order: a MaxScale that spec.maxScaleRef names and that sends
// every statement to the primary on Port(db) (RouteMaxScale), the
// <name>-primary Service of a Galera or replication MariaDB (RoutePrimary),
// and the MariaDB's own Service (RouteService). A MaxScale that does not
// qualify sets BypassReason and the route falls through to the next one.
//
// The decision reads spec only, never status, so a MariaDB or MaxScale that
// flaps between ready and not ready does not change the host and roll the
// pods. A MariaDB that does not exist and a cluster that does not serve the
// kind return RouteService without an error, so the caller never waits for
// the read; the cluster-Ready gate of the database step holds back the
// workloads until the MariaDB exists. Any other read error is returned.
func ResolveEndpoint(ctx context.Context, c client.Reader, db *commonv1.DatabaseSpec, namespace string) (Endpoint, error) {
	port := Port(db)
	if db.ClusterRef == nil {
		return Endpoint{Host: db.Host, Port: port, Route: RouteExplicit}, nil
	}
	name := db.ClusterRef.Name
	service := Endpoint{Host: serviceHost(name, namespace), Port: port, Route: RouteService}

	key := client.ObjectKey{Namespace: namespace, Name: name}
	mariadb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(ctx, key, mariadb); err != nil {
		reason := ""
		switch {
		case apierrors.IsNotFound(err):
			reason = "NotFound"
		case meta.IsNoMatchError(err):
			reason = "KindNotServed"
		default:
			return Endpoint{}, fmt.Errorf("getting MariaDB %s: %w", key, err)
		}
		log.FromContext(ctx).V(1).Info("MariaDB not readable; rendering the Service name",
			"mariadb", key.String(), "reason", reason)
		return service, nil
	}

	ep := service
	if mariadb.IsHAEnabled() {
		ep = Endpoint{Host: serviceHost(mariadb.PrimaryServiceKey().Name, namespace), Port: port, Route: RoutePrimary}
	}

	ref := mariadb.Spec.MaxScaleRef
	if ref == nil {
		return ep, nil
	}
	reason, err := maxScaleBypass(ctx, c, ref, name, namespace, port)
	if err != nil {
		return Endpoint{}, err
	}
	if reason == "" {
		return Endpoint{Host: serviceHost(ref.Name, namespace), Port: port, Route: RouteMaxScale, MaxScale: ref.Name}, nil
	}
	ep.BypassReason = reason
	ep.MaxScale = ref.Name
	return ep, nil
}

// maxScaleBypass returns the reason the MaxScale ref names is not a route to
// MariaDB mariadbName in namespace, or "" when it qualifies. The checks run in
// order and stop at the first failure.
func maxScaleBypass(ctx context.Context, c client.Reader, ref *mariadbv1alpha1.ObjectReference,
	mariadbName, namespace string, port int32,
) (string, error) {
	if ref.Namespace != "" && ref.Namespace != namespace {
		return BypassMaxScaleOtherNamespace, nil
	}
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	mxs := &mariadbv1alpha1.MaxScale{}
	if err := c.Get(ctx, key, mxs); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return BypassMaxScaleNotFound, nil
		}
		return "", fmt.Errorf("getting MaxScale %s: %w", key, err)
	}
	served := mxs.Spec.MariaDBRef
	if served == nil || served.Name != mariadbName || (served.Namespace != "" && served.Namespace != namespace) {
		return BypassMaxScaleServesOtherMariaDB, nil
	}
	for i := range mxs.Spec.Services {
		if singleWriterService(&mxs.Spec.Services[i], port) {
			return "", nil
		}
	}
	return BypassMaxScaleNotSingleWriter, nil
}

// singleWriterService reports whether svc sends every statement to the
// current primary on port. readwritesplit sends autocommit reads to replicas
// unless max_slave_connections is 0; a readconnroute session stays on the
// server it first connected to and never follows a new primary.
func singleWriterService(svc *mariadbv1alpha1.MaxScaleService, port int32) bool {
	return svc.Router == mariadbv1alpha1.ServiceRouterReadWriteSplit &&
		strings.TrimSpace(svc.Params["max_slave_connections"]) == "0" &&
		svc.Listener.Port == port &&
		!svc.Suspend &&
		!svc.Listener.Suspend
}

// serviceHost returns the in-cluster DNS name of Service name in namespace.
func serviceHost(name, namespace string) string {
	return fmt.Sprintf("%s.%s.svc", name, namespace)
}

// ResolveHost returns the database host:port for the shared DatabaseSpec.
// In managed mode (ClusterRef set), it constructs the MariaDB Service DNS
// name in the given namespace. In brownfield mode (Host set), it uses the
// explicit host:port.
func ResolveHost(db *commonv1.DatabaseSpec, namespace string) string {
	if db.ClusterRef != nil {
		return fmt.Sprintf("%s.%s.svc:%d", db.ClusterRef.Name, namespace, Port(db))
	}
	return fmt.Sprintf("%s:%d", db.Host, Port(db))
}

// Port returns the database port, defaulting to 3306 if not set.
func Port(db *commonv1.DatabaseSpec) int32 {
	if db.Port > 0 {
		return db.Port
	}
	return 3306
}

// ResolveUsername returns the MySQL username for the shared DatabaseSpec. In
// Static managed mode the MariaDB User CR name (= the CR instance name) is
// the MySQL username, so it is not read from the Secret. Brownfield mode and
// Dynamic managed mode both take "username" from the upstream Secret data —
// the dynamic engine issues an ephemeral username (e.g. v-kube-...) alongside
// the password, so the username is not derivable from the CR name. ok is
// false when the Secret data lacks the required "username" key.
func ResolveUsername(db *commonv1.DatabaseSpec, instanceName string, secretData map[string][]byte) (username string, ok bool) {
	if db.ClusterRef != nil && db.CredentialsMode != commonv1.CredentialsModeDynamic {
		return instanceName, true
	}
	u, ok := secretData["username"]
	if !ok {
		return "", false
	}
	return string(u), true
}
