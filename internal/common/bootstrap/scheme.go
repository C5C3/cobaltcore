// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// NewScheme returns a runtime.Scheme with the client-go baseline types
// registered first, followed by each extra registration function in argument
// order. Every operator needs the built-in Kubernetes types (Deployment,
// Secret, Service, ...), so callers pass only their operator-specific
// AddToScheme functions.
//
// A failing registration panics through utilruntime.Must, exactly as the
// per-operator init() blocks did before this helper existed. A type that
// cannot be registered is a programming error, not a recoverable condition: a
// scheme missing a type surfaces much later as a "no kind is registered for
// the type ..." failure mid-reconcile, so the failure must kill startup while
// it is still attributable.
func NewScheme(extras ...func(*runtime.Scheme) error) *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	for _, add := range extras {
		utilruntime.Must(add(scheme))
	}
	return scheme
}
