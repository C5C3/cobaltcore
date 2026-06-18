// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains the API types for the c5c3 ControlPlane operator
// It defines the ControlPlane aggregate CRD that projects an
// OpenStack control plane (Keystone today; more services later) plus the
// CredentialRotation and SecretAggregate helper CRDs.
//
// DECISION (plan decision #1): the API group is "c5c3.io" (NOT
// keystone.openstack.c5c3.io). The ControlPlane is a cross-service aggregate,
// so it lives in the vendor-neutral c5c3.io group rather than under a
// per-service openstack subgroup.
// +kubebuilder:object:generate=true
// +groupName=c5c3.io

//go:generate controller-gen object:headerFile=../../../../hack/boilerplate.go.txt paths=.
//go:generate controller-gen crd paths=. output:crd:artifacts:config=../../config/crd/bases
//go:generate controller-gen webhook paths=. output:webhook:artifacts:config=../../config/webhook

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	// DECISION (plan decision #1): Group is "c5c3.io", version "v1alpha1".
	GroupVersion = schema.GroupVersion{Group: "c5c3.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	//nolint:staticcheck // SA1019: kubebuilder-scaffolded helper; no drop-in replacement that keeps the api package controller-runtime-free.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
