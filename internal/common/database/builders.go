// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Values the provisioning builders apply to every built resource.
const (
	defaultCharacterSet = "utf8"
	defaultCollate      = "utf8_general_ci"
	defaultPasswordKey  = "password"
)

// ProvisionParams carries the inputs of the MariaDB provisioning builders. Every
// database-backed service operator provisions the same three resources — a
// Database, a User, and a Grant — against a MariaDB cluster; only the names, the
// cluster reference, and the credential Secret differ. The operator supplies
// those; the builders assemble the mariadb-operator CRs.
type ProvisionParams struct {
	// Name is the Database/User/Grant resource name and the SQL username; the
	// operator's naming convention chooses it.
	Name string
	// Namespace is the resource namespace.
	Namespace string
	// Labels are applied to every built resource; nil leaves the resources
	// unlabelled.
	Labels map[string]string
	// ClusterRef is the MariaDB cluster the resources attach to
	// (spec.database.clusterRef.name).
	ClusterRef string
	// DatabaseName is the SQL database to create and grant on.
	DatabaseName string
	// PasswordSecretName locates the user password Secret.
	PasswordSecretName string
}

func (p ProvisionParams) mariaDBRef() mariadbv1alpha1.MariaDBRef {
	return mariadbv1alpha1.MariaDBRef{
		ObjectReference: mariadbv1alpha1.ObjectReference{Name: p.ClusterRef},
	}
}

func (p ProvisionParams) objectMeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: p.Labels}
}

// BuildDatabase builds the mariadb-operator Database CR that creates the SQL
// database.
func BuildDatabase(p ProvisionParams) *mariadbv1alpha1.Database {
	return &mariadbv1alpha1.Database{
		ObjectMeta: p.objectMeta(),
		Spec: mariadbv1alpha1.DatabaseSpec{
			MariaDBRef:   p.mariaDBRef(),
			CharacterSet: defaultCharacterSet,
			Collate:      defaultCollate,
			Name:         p.DatabaseName,
		},
	}
}

// BuildUser builds the mariadb-operator User CR that creates the SQL user,
// reading its password from the referenced Secret.
func BuildUser(p ProvisionParams) *mariadbv1alpha1.User {
	return &mariadbv1alpha1.User{
		ObjectMeta: p.objectMeta(),
		Spec: mariadbv1alpha1.UserSpec{
			MariaDBRef: p.mariaDBRef(),
			PasswordSecretKeyRef: &mariadbv1alpha1.SecretKeySelector{
				LocalObjectReference: mariadbv1alpha1.LocalObjectReference{
					Name: p.PasswordSecretName,
				},
				Key: defaultPasswordKey,
			},
		},
	}
}

// BuildGrant builds the mariadb-operator Grant CR that grants the user ALL
// PRIVILEGES on the database.
func BuildGrant(p ProvisionParams) *mariadbv1alpha1.Grant {
	return &mariadbv1alpha1.Grant{
		ObjectMeta: p.objectMeta(),
		Spec: mariadbv1alpha1.GrantSpec{
			MariaDBRef: p.mariaDBRef(),
			Privileges: []string{"ALL PRIVILEGES"},
			Database:   p.DatabaseName,
			Table:      "*",
			Username:   p.Name,
		},
	}
}
