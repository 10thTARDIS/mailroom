// SPDX-License-Identifier: AGPL-3.0-only

package store

import "github.com/jackc/pgx/v5"

// planForValues is passed as the first argument after the SQL to make PostgreSQL plan
// a statement with its actual parameter values on every execution, instead of
// reusing the generic plan it caches for a prepared statement.
//
// WHY IT EXISTS. Every hot table here leads its index with user_id, and on a
// single-user instance user_id has one distinct value. A generic plan cannot
// see the value, so it estimates "this user's rows" as nearly the whole table
// and chooses a sequential scan. The JMAP /changes statement then read the
// whole change log to return a row or two, where a plan made with its values
// walks a few pages of change_log_pkey; it is called constantly, so it became
// the dominant cost. A one-row contact lookup scanned every address instead of
// using its unique index the same way. Planning each time costs far less than
// either scan.
//
// Use it for statements that run very often and whose good plan depends on a
// parameter's value (a seq > $n range, a user_id on a single-tenant table).
// Not a blanket setting: most statements plan fine either way, and the pool
// keeps its statement cache for them.
var planForValues = pgx.QueryExecModeExec
