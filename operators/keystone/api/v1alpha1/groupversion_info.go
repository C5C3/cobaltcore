// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// +kubebuilder:object:generate=true
// +groupName=keystone.openstack.c5c3.io

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
	GroupVersion = schema.GroupVersion{Group: "keystone.openstack.c5c3.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
