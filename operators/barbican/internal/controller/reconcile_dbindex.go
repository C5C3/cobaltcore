// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"crypto/sha256"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/c5c3/cobaltcore/internal/common/database"
)

// The db-index Job builds one index per list query on (project_id, deleted,
// created_at). Barbican deletes a secret, order or container by flagging its
// row, and the list queries of the three (SecretRepo.get_secret_list and the
// two get_by_create_date) filter on project and deleted = false and sort by
// created_at. The upstream schema indexes project_id alone, so the count and
// the page of every list request read every row the project has ever had.
//
// The indexes live outside Alembic's history: barbican-manage db upgrade
// neither knows nor touches them. A downstream Alembic revision would fork the
// upstream revision chain, and the default image tag does not re-run db-sync
// when the image is rebuilt.
const (
	// dbIndexJobSuffix names the Job {name}-db-index.
	dbIndexJobSuffix = "db-index"
	// dbIndexBackoffLimit is the backoff limit db-sync uses.
	dbIndexBackoffLimit int32 = 4

	conditionReasonDBIndexInProgress = "DBIndexInProgress"
	conditionReasonDBIndexFailed     = "DBIndexFailed"
)

// dbIndexTables are the tables the Job indexes. They reach the script as
// arguments from this list only, never from a CR field, so no user input ends
// up in a statement.
var dbIndexTables = []string{"secrets", "orders", "containers"}

// dbIndexScript creates the index on each table named in its arguments unless
// an index on the same column list exists, whatever its name. A second run
// therefore creates nothing, and neither does a run against a Barbican release
// that adds such an index under its own name. The c5c3_ prefix keeps the name
// clear of Alembic's ix_ names. Each CREATE INDEX commits on its own, so a run
// that fails at one table keeps the indexes it built and the retry skips them.
//
// The script needs only SQLAlchemy and PyMySQL, which every Barbican image
// carries, and hands OS_DATABASE__CONNECTION to create_engine as oslo.db does,
// so the ssl_* parameters of the DSN apply.
var dbIndexScript = `import os
import sys

import sqlalchemy as sa

COLUMNS = ["project_id", "deleted", "created_at"]

engine = sa.create_engine(os.environ["OS_DATABASE__CONNECTION"])
inspector = sa.inspect(engine)
for name in sys.argv[1:]:
    if any(index["column_names"] == COLUMNS for index in inspector.get_indexes(name)):
        print("%s: an index on (%s) exists" % (name, ", ".join(COLUMNS)), flush=True)
        continue
    table = sa.Table(name, sa.MetaData(), autoload_with=engine)
    index = sa.Index("c5c3_%s_project_id_deleted_created_at" % name, *[table.c[c] for c in COLUMNS])
    index.create(engine)
    print("%s: created index %s" % (name, index.name), flush=True)
`

// dbIndexCommand returns the command of the db-index Job: the script, then the
// tables it indexes as its arguments.
func dbIndexCommand() []string {
	return append([]string{"python3", "-c", dbIndexScript}, dbIndexTables...)
}

// dbIndexJobName returns the name of the db-index Job. The API server copies a
// Job's name into a label of its pod, so the name is held to 63 characters.
// Every admissible name keeps the plain {name}-db-index form; a longer one, from
// a Barbican admitted before the name bound, collapses onto a hash as
// dbCleanCronJobName does. Such a name of 55 characters still fits
// {name}-db-sync, so its Barbican reconciled before the db-index Job existed.
func dbIndexJobName(barbicanName string) string {
	suffix := "-" + dbIndexJobSuffix
	if len(barbicanName)+len(suffix) <= validation.LabelValueMaxLength {
		return barbicanName + suffix
	}
	sum := sha256.Sum256([]byte(barbicanName))
	kept := strings.TrimRight(
		barbicanName[:validation.LabelValueMaxLength-len(suffix)-dbCleanNameHashLength], "-.")
	return fmt.Sprintf("%s-%x%s", kept, sum[:4], suffix)
}

// dbIndexJob builds the db-index Job from the Job set the db-sync Job is built
// from, so both carry the same image, config mount, connection override, db-tls
// volume and pod settings.
func dbIndexJob(p database.JobSetParams) *batchv1.Job {
	j := database.BuildJob(p, p.Image, dbIndexJobSuffix, dbIndexCommand(), dbIndexBackoffLimit)
	j.Name = dbIndexJobName(p.InstanceName)
	return j
}
