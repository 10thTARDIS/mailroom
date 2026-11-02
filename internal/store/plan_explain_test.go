// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

func TestIsExplainableGuard(t *testing.T) {
	ok := []string{
		"SELECT 1",
		"  select a from t where x = $1",
		"WITH c AS (SELECT 1) SELECT * FROM c",
		"SELECT 'DELETE me' FROM t",
		"SELECT\n  a\nFROM t",
	}
	for _, s := range ok {
		if !IsExplainable(s) {
			t.Errorf("refused a plain read: %q", s)
		}
	}
	refuse := []string{
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"INSERT INTO t VALUES (1)",
		"SET plan_cache_mode = 'x'",
		"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d",
		"WITH d AS (\nUPDATE t SET a = 1 RETURNING *) SELECT * FROM d",
		"WITH d AS (INSERT INTO t VALUES (1) RETURNING *) SELECT * FROM d",
		"WITH d AS (MERGE INTO t USING s ON true WHEN MATCHED THEN DELETE RETURNING *) SELECT * FROM d",
		"",
	}
	for _, s := range refuse {
		if IsExplainable(s) {
			t.Errorf("accepted a statement that is not a plain read: %q", s)
		}
	}
}
