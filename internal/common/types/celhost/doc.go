// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package celhost defines CELHost, a kind that exists only to install the CEL
// rules of the shared types into an envtest API server. The shared types carry
// no CRD of their own, and this module cannot import an operator, so CELHost
// embeds every shared type that declares a rule and its generated CRD under
// crd/ is what the CRD-only envtest of the shared types installs. The kind
// ships in no chart or deploy overlay. make generate-common writes its
// deepcopy file and make manifests-common writes its CRD.
//
// +kubebuilder:object:generate=true
// +groupName=test.c5c3.io
// +versionName=v1alpha1
package celhost
