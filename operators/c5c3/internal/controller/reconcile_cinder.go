// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The projected Cinder CR is named "{controlplane.Name}-cinder", the same
// deterministic, collision-free naming convention as the Keystone, Horizon,
// Glance, Placement, Barbican, and Neutron children (see keystoneNameSuffix), and
// lives in cp.CinderNamespace(): the ControlPlane's own namespace by default, or
// the one services.cinder.namespace assigns.

// cinderNameSuffix is appended to the ControlPlane name to derive the name of the
// projected Cinder CR (and, through it, its credential and registration objects).
const cinderNameSuffix = "-cinder"

// cinderAPIPort is the port the cinder operator's API Service listens on.
const cinderAPIPort int32 = 8776

// cinderName returns the name of the Cinder CR the reconciler projects for the
// given ControlPlane (see cinderNameSuffix).
func cinderName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + cinderNameSuffix
}

// cinderEndpointURL renders the in-cluster URL of the projected Cinder API
// Service by naming convention, the cross-service endpoint contract the catalog
// registers against. It carries no path: the catalog rows append the "/v3" the
// block-storage API is served under (see cinderCatalogURL).
func cinderEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return managedServiceURL(cinderName(cp), cp.CinderNamespace(), cinderAPIPort, "")
}
