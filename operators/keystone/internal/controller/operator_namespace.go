// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"strings"
)

// serviceAccountNamespacePath is the in-cluster file the kubelet projects into
// every Pod that mounts a ServiceAccount token. It holds the Pod's own
// Namespace. Declared as a package-level var so tests can repoint it at a
// temporary file.
var serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// DetectOperatorNamespace resolves the Namespace the operator Pod runs in. It
// prefers the POD_NAMESPACE environment variable (injected via the downward API
// when configured) and falls back to the ServiceAccount Namespace file that the
// kubelet mounts into every in-cluster Pod. It returns "" when neither source is
// available — for example outside a cluster (unit tests) — which callers treat
// as "operator namespace unknown" and skip the operator-namespace NetworkPolicy
// ingress peer accordingly.
func DetectOperatorNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	data, err := os.ReadFile(serviceAccountNamespacePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
