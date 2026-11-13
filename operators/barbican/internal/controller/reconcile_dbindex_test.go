// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/c5c3/cobaltcore/internal/common/database"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

// TestDBIndexJob_Shape pins what the index Job runs and reads: the CR's image,
// the script with the three tables as its arguments, the DB URL through the
// env override, the config Secret at the path db-sync mounts it, and the
// backoff limit db-sync uses.
func TestDBIndexJob_Shape(t *testing.T) {
	g := NewWithT(t)
	barbican := testBarbican()

	j := dbIndexJob(barbicanJobSetParams(barbican, dbConfigSecretName))

	g.Expect(j.Name).To(Equal(testBarbicanName + "-db-index"))
	g.Expect(j.Namespace).To(Equal(testNamespace))
	g.Expect(j.Spec.BackoffLimit).To(HaveValue(Equal(int32(4))))
	g.Expect(j.Spec.Template.Spec.Containers).To(HaveLen(1))
	c := j.Spec.Template.Spec.Containers[0]
	g.Expect(c.Name).To(Equal("db-index"))
	g.Expect(c.Image).To(Equal(barbican.Spec.Image.Reference()))
	g.Expect(c.Command).To(Equal([]string{"python3", "-c", dbIndexScript, "secrets", "orders", "containers"}))
	g.Expect(c.Command[3:]).To(Equal(dbIndexTables))

	var connEnv *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == "OS_DATABASE__CONNECTION" {
			connEnv = &c.Env[i]
		}
	}
	g.Expect(connEnv).NotTo(BeNil(), "the script reads the DB URL from the env override")
	g.Expect(connEnv.ValueFrom.SecretKeyRef.Name).To(Equal(database.ConnectionSecretName(barbican.Name)))
	// The per-read cap would cut a long index build: only the API container
	// carries the socket timeouts.
	g.Expect(c.Env).NotTo(ContainElement(HaveField("Name", HaveSuffix("__CONNECTION_PARAMETERS"))))

	g.Expect(j.Spec.Template.Spec.Volumes).To(HaveLen(1))
	g.Expect(j.Spec.Template.Spec.Volumes[0].Secret.SecretName).To(Equal(dbConfigSecretName))
	g.Expect(c.VolumeMounts).To(HaveLen(1))
	g.Expect(c.VolumeMounts[0].MountPath).To(Equal("/etc/barbican"))
}

// TestDBIndexJob_DBTLSVolumeOnlyWhenEnabled pins the db-tls keypair against
// the DSN: with database TLS the derived URL carries ssl_* paths under
// dbTLSMountPath, so the Job mounts them; without it there is nothing to mount.
func TestDBIndexJob_DBTLSVolumeOnlyWhenEnabled(t *testing.T) {
	g := NewWithT(t)

	plain := dbIndexJob(barbicanJobSetParams(testBarbican(), dbConfigSecretName))
	for _, v := range plain.Spec.Template.Spec.Volumes {
		g.Expect(v.Name).NotTo(Equal(dbTLSVolumeName))
	}
	for _, m := range plain.Spec.Template.Spec.Containers[0].VolumeMounts {
		g.Expect(m.Name).NotTo(Equal(dbTLSVolumeName))
	}

	barbican := testBarbican()
	barbican.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "barbican-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "barbican-db-client"},
	}
	tls := dbIndexJob(barbicanJobSetParams(barbican, dbConfigSecretName))
	tlsVol, tlsMount := barbicanDBTLSVolumeAndMount(barbican)
	g.Expect(tls.Spec.Template.Spec.Volumes).To(ContainElement(tlsVol))
	g.Expect(tls.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(tlsMount))
}

// TestDBIndexScript_MatchesByColumns pins the properties the Job's re-runs and
// upgrades rely on: the script reads the DSN from the env override and the
// tables from its arguments, matches an existing index by its column list, and
// names its own under the c5c3_ prefix.
func TestDBIndexScript_MatchesByColumns(t *testing.T) {
	g := NewWithT(t)
	g.Expect(dbIndexScript).To(ContainSubstring(`COLUMNS = ["project_id", "deleted", "created_at"]`))
	g.Expect(dbIndexScript).To(ContainSubstring(`index["column_names"] == COLUMNS`))
	g.Expect(dbIndexScript).To(ContainSubstring(`"c5c3_%s_project_id_deleted_created_at"`))
	g.Expect(dbIndexScript).To(ContainSubstring(`os.environ["OS_DATABASE__CONNECTION"]`))
	g.Expect(dbIndexScript).To(ContainSubstring(`sys.argv[1:]`))
	for _, table := range dbIndexTables {
		g.Expect(dbIndexScript).NotTo(ContainSubstring(table),
			"no table name is baked into the script; they arrive as arguments")
	}
}

// TestDBIndexJobName pins the Job name against the 63 characters the API
// server allows it: the plain form for every name the webhook admits and for
// the longest name that fits, a stable hash for a longer inherited name.
func TestDBIndexJobName(t *testing.T) {
	t.Run("a name the webhook admits keeps the plain form", func(t *testing.T) {
		g := NewWithT(t)
		name := strings.Repeat("b", barbicanv1alpha1.MaxBarbicanNameLength)
		g.Expect(dbIndexJobName(name)).To(Equal(name + "-db-index"))
		g.Expect(dbIndexJobName(testBarbicanName)).To(Equal(testBarbicanName + "-db-index"))
	})

	t.Run("the longest name that fits keeps the plain form", func(t *testing.T) {
		g := NewWithT(t)
		name := strings.Repeat("b", 54)
		g.Expect(dbIndexJobName(name)).To(Equal(name + "-db-index"))
		g.Expect(dbIndexJobName(name)).To(HaveLen(63))
	})

	// A Barbican admitted before the name bound can be 55 characters long;
	// '<name>-db-sync' still fits 63 then.
	t.Run("a longer name collapses onto a hash within 63 characters", func(t *testing.T) {
		g := NewWithT(t)
		for _, name := range []string{strings.Repeat("b", 55), strings.Repeat("b", 63)} {
			got := dbIndexJobName(name)
			g.Expect(len(got)).To(BeNumerically("<=", 63), got)
			g.Expect(got).To(MatchRegexp(`^b+-[0-9a-f]{8}-db-index$`))
			g.Expect(dbIndexJobName(name)).To(Equal(got), "the name is stable across passes")
		}
		g.Expect(dbIndexJobName(strings.Repeat("b", 55))).
			NotTo(Equal(dbIndexJobName(strings.Repeat("b", 56))), "two names keep two Jobs")
	})

	t.Run("the truncation does not end on a separator", func(t *testing.T) {
		g := NewWithT(t)
		got := dbIndexJobName(strings.Repeat("b", 43) + "-." + strings.Repeat("b", 10))
		g.Expect(got).To(MatchRegexp(`^b{43}-[0-9a-f]{8}-db-index$`))
	})

	t.Run("the Job carries the name", func(t *testing.T) {
		g := NewWithT(t)
		barbican := testBarbican()
		barbican.Name = strings.Repeat("b", 55)
		j := dbIndexJob(barbicanJobSetParams(barbican, dbConfigSecretName))
		g.Expect(j.Name).To(Equal(dbIndexJobName(barbican.Name)))
		g.Expect(len(j.Name)).To(BeNumerically("<=", 63))
		g.Expect(j.Spec.Template.Spec.Containers[0].Name).To(Equal("db-index"))
	})
}

// sqliteTablesScript creates the three tables the db-index script indexes in
// the SQLite file argv[1], each with the upstream ix_<table>_project_id index,
// and then runs the statements that follow.
const sqliteTablesScript = `import sqlite3
import sys

conn = sqlite3.connect(sys.argv[1])
for table in ("secrets", "orders", "containers"):
    conn.execute("CREATE TABLE %s (id VARCHAR(36) PRIMARY KEY, project_id VARCHAR(36), deleted BOOLEAN, created_at DATETIME)" % table)
    conn.execute("CREATE INDEX ix_%s_project_id ON %s (project_id)" % (table, table))
for statement in sys.argv[2:]:
    conn.execute(statement)
conn.commit()
`

// sqliteIndexesScript prints the indexes of the SQLite file argv[1] as JSON: per
// table, one "name(column,...)" entry per index.
const sqliteIndexesScript = `import json
import sqlite3
import sys

conn = sqlite3.connect(sys.argv[1])
out = {}
for name, table in conn.execute("SELECT name, tbl_name FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL"):
    columns = [row[2] for row in conn.execute("SELECT * FROM pragma_index_info(?)", (name,))]
    out.setdefault(table, []).append("%s(%s)" % (name, ",".join(columns)))
print(json.dumps(out))
`

// requireSQLAlchemy skips the test unless python3 can import SQLAlchemy, the
// one library the db-index script needs. Every Barbican image carries it; a
// development machine may not.
func requireSQLAlchemy(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	if err := exec.CommandContext(t.Context(), "python3", "-c", "import sqlalchemy").Run(); err != nil {
		t.Skip("python3 cannot import sqlalchemy")
	}
}

// runPython runs python3 with args, env added to the test's environment, and
// returns its combined output and exit error.
func runPython(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	//nolint:gosec // G204: the scripts are this package's constants; the arguments are table names and a temp path the test chose.
	cmd := exec.CommandContext(t.Context(), "python3", args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// newDBIndexDatabase creates a SQLite file with the tables of
// sqliteTablesScript, runs statements against it, and returns its path.
func newDBIndexDatabase(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "barbican.sqlite")
	if out, err := runPython(t, nil, append([]string{"-c", sqliteTablesScript, path}, statements...)...); err != nil {
		t.Fatalf("creating the tables: %v\n%s", err, out)
	}
	return path
}

// runDBIndexScript runs the db-index script against the SQLite file at path
// with tables as its arguments, and returns its output and exit error.
func runDBIndexScript(t *testing.T, path string, tables ...string) (string, error) {
	t.Helper()
	return runPython(t, []string{"OS_DATABASE__CONNECTION=sqlite:///" + path},
		append([]string{"-c", dbIndexScript}, tables...)...)
}

// sqliteIndexes returns the indexes of the SQLite file at path, per table.
func sqliteIndexes(t *testing.T, path string) map[string][]string {
	t.Helper()
	out, err := runPython(t, nil, "-c", sqliteIndexesScript, path)
	if err != nil {
		t.Fatalf("listing the indexes: %v\n%s", err, out)
	}
	var indexes map[string][]string
	if err := json.Unmarshal([]byte(out), &indexes); err != nil {
		t.Fatalf("decoding the index list %q: %v", out, err)
	}
	return indexes
}

// TestDBIndexScript_RunsAgainstADatabase runs the script against a SQLite file
// that carries the three tables and their upstream project_id index. It pins
// what the Job's re-runs and upgrades rely on: a second run creates nothing,
// an index on the same columns under another name counts as the index, and a
// run that fails at one table keeps the indexes it built for the retry to skip.
func TestDBIndexScript_RunsAgainstADatabase(t *testing.T) {
	requireSQLAlchemy(t)
	const columns = "(project_id,deleted,created_at)"
	exists := func(table string) string {
		return table + ": an index on (project_id, deleted, created_at) exists"
	}
	created := func(table string) string {
		return table + ": created index c5c3_" + table + "_project_id_deleted_created_at"
	}

	t.Run("a second run creates nothing", func(t *testing.T) {
		g := NewWithT(t)
		path := newDBIndexDatabase(t)

		out, err := runDBIndexScript(t, path, dbIndexTables...)
		g.Expect(err).NotTo(HaveOccurred(), out)
		first := sqliteIndexes(t, path)
		for _, table := range dbIndexTables {
			g.Expect(out).To(ContainSubstring(created(table)))
			g.Expect(first[table]).To(ConsistOf(
				"ix_"+table+"_project_id(project_id)",
				"c5c3_"+table+"_project_id_deleted_created_at"+columns))
		}

		out, err = runDBIndexScript(t, path, dbIndexTables...)
		g.Expect(err).NotTo(HaveOccurred(), out)
		for _, table := range dbIndexTables {
			g.Expect(out).To(ContainSubstring(exists(table)))
		}
		g.Expect(out).NotTo(ContainSubstring("created index"))
		g.Expect(sqliteIndexes(t, path)).To(Equal(first))
	})

	t.Run("an index on the same columns under another name counts", func(t *testing.T) {
		g := NewWithT(t)
		path := newDBIndexDatabase(t, "CREATE INDEX ix_upstream ON secrets (project_id, deleted, created_at)")

		out, err := runDBIndexScript(t, path, dbIndexTables...)
		g.Expect(err).NotTo(HaveOccurred(), out)
		g.Expect(out).To(ContainSubstring(exists("secrets")))
		indexes := sqliteIndexes(t, path)
		g.Expect(indexes["secrets"]).To(ConsistOf("ix_secrets_project_id(project_id)", "ix_upstream"+columns))
		g.Expect(indexes["orders"]).To(ContainElement("c5c3_orders_project_id_deleted_created_at" + columns))
		g.Expect(indexes["containers"]).To(ContainElement("c5c3_containers_project_id_deleted_created_at" + columns))
	})

	t.Run("a failed run keeps what it built and the retry skips it", func(t *testing.T) {
		g := NewWithT(t)
		path := newDBIndexDatabase(t)

		out, err := runDBIndexScript(t, path, "secrets", "missing", "containers")
		g.Expect(err).To(HaveOccurred(), "a missing table fails the run")
		g.Expect(out).To(ContainSubstring("NoSuchTableError"))
		indexes := sqliteIndexes(t, path)
		g.Expect(indexes["secrets"]).To(ContainElement("c5c3_secrets_project_id_deleted_created_at" + columns))
		g.Expect(indexes["containers"]).To(ConsistOf("ix_containers_project_id(project_id)"),
			"the run stops at the failing table")

		out, err = runDBIndexScript(t, path, dbIndexTables...)
		g.Expect(err).NotTo(HaveOccurred(), out)
		g.Expect(out).To(ContainSubstring(exists("secrets")))
		g.Expect(out).To(ContainSubstring(created("orders")))
		g.Expect(out).To(ContainSubstring(created("containers")))
	})
}
