// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"testing"

	"github.com/onsi/gomega"
)

func provisionParams() ProvisionParams {
	return ProvisionParams{
		Name:               "keystone",
		Namespace:          "openstack",
		ClusterRef:         "mariadb",
		DatabaseName:       "keystone",
		PasswordSecretName: "keystone-db",
	}
}

func TestBuildDatabase(t *testing.T) {
	g := gomega.NewWithT(t)
	db := BuildDatabase(provisionParams())
	g.Expect(db.Name).To(gomega.Equal("keystone"))
	g.Expect(db.Namespace).To(gomega.Equal("openstack"))
	g.Expect(db.Spec.MariaDBRef.Name).To(gomega.Equal("mariadb"))
	g.Expect(db.Spec.Name).To(gomega.Equal("keystone"))
	// utf8 / utf8_general_ci are applied to every built database.
	g.Expect(db.Spec.CharacterSet).To(gomega.Equal("utf8"))
	g.Expect(db.Spec.Collate).To(gomega.Equal("utf8_general_ci"))
	g.Expect(db.Labels).To(gomega.BeNil())
}

func TestBuildDatabase_Labels(t *testing.T) {
	g := gomega.NewWithT(t)
	p := provisionParams()
	p.Labels = map[string]string{"app": "nova"}
	db := BuildDatabase(p)
	g.Expect(db.Labels).To(gomega.HaveKeyWithValue("app", "nova"))
}

func TestBuildUser(t *testing.T) {
	g := gomega.NewWithT(t)
	user := BuildUser(provisionParams())
	g.Expect(user.Name).To(gomega.Equal("keystone"))
	g.Expect(user.Spec.MariaDBRef.Name).To(gomega.Equal("mariadb"))
	g.Expect(user.Spec.PasswordSecretKeyRef.Name).To(gomega.Equal("keystone-db"))
	// The password Secret key is always "password".
	g.Expect(user.Spec.PasswordSecretKeyRef.Key).To(gomega.Equal("password"))
}

func TestBuildGrant(t *testing.T) {
	g := gomega.NewWithT(t)
	grant := BuildGrant(provisionParams())
	g.Expect(grant.Name).To(gomega.Equal("keystone"))
	g.Expect(grant.Spec.MariaDBRef.Name).To(gomega.Equal("mariadb"))
	g.Expect(grant.Spec.Privileges).To(gomega.Equal([]string{"ALL PRIVILEGES"}))
	g.Expect(grant.Spec.Database).To(gomega.Equal("keystone"))
	g.Expect(grant.Spec.Table).To(gomega.Equal("*"))
	g.Expect(grant.Spec.Username).To(gomega.Equal("keystone"))
}
