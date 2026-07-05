// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"fmt"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

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
