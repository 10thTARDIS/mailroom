// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CapturedStatement is one statement the store sent to PostgreSQL while a
// StatementRecorder was recording: its SQL text and its argument values.
type CapturedStatement struct {
	SQL  string
	Args []any
}

// StatementRecorder is a pgx QueryTracer that remembers the statements sent
// while it is recording. It exists for the plan-check --explain diagnostic
// only: OpenRecording is the sole way to attach one, so the normal server
// path has no tracer. It never logs or prints; the caller reads Statements.
type StatementRecorder struct {
	mu        sync.Mutex
	recording bool
	stmts     []CapturedStatement
}

// Start clears the buffer and begins recording.
func (r *StatementRecorder) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recording = true
	r.stmts = nil
}

// Stop ends recording and returns what was captured, in send order.
func (r *StatementRecorder) Stop() []CapturedStatement {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recording = false
	out := r.stmts
	r.stmts = nil
	return out
}

// TraceQueryStart implements pgx.QueryTracer.
func (r *StatementRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recording {
		r.stmts = append(r.stmts, CapturedStatement{SQL: d.SQL, Args: append([]any(nil), d.Args...)})
	}
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer.
func (r *StatementRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// HasQueryTracer reports whether the pool's connections carry a query tracer.
// A default Open must report false.
func (s *Store) HasQueryTracer() bool {
	return s.pool.Config().ConnConfig.Tracer != nil
}

// OpenRecording is Open with a StatementRecorder attached to every connection.
// It is used solely by the plan-check --explain diagnostic.
func OpenRecording(ctx context.Context, dsn string) (*Store, *StatementRecorder, error) {
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("store: parse database url: %w", err)
	}
	if !strings.Contains(dsn, "pool_max_conns") {
		pcfg.MaxConns = DefaultMaxConns
	}
	rec := &StatementRecorder{}
	pcfg.ConnConfig.Tracer = rec
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, nil, fmt.Errorf("store: open pool: %w", err)
	}
	return &Store{pool: pool, trashRetention: DefaultTrashRetention}, rec, nil
}

// IsExplainable reports whether sql may be handed to ExplainAnalyze: it must
// begin with SELECT or WITH (after trimming) and must not contain a
// data-modifying keyword (INSERT, UPDATE, DELETE, MERGE) outside string
// literals. Conservative on purpose: a false refusal is acceptable, a false
// acceptance is not.
func IsExplainable(sql string) bool {
	t := strings.ToUpper(strings.TrimSpace(sql))
	if !strings.HasPrefix(t, "SELECT") && !strings.HasPrefix(t, "WITH") {
		return false
	}
	// Blank out single-quoted literals ('' is an escaped quote and simply
	// toggles twice), then pad every non-word byte so keywords match as words.
	var b strings.Builder
	inLit := false
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c == '\'' {
			inLit = !inLit
			b.WriteByte(' ')
			continue
		}
		if inLit {
			continue
		}
		if c == '\n' || c == '\t' || c == '\r' || c == '(' || c == ')' || c == ',' || c == ';' {
			c = ' '
		}
		b.WriteByte(c)
	}
	padded := " " + b.String() + " "
	for _, kw := range []string{" INSERT ", " UPDATE ", " DELETE ", " MERGE "} {
		if strings.Contains(padded, kw) {
			return false
		}
	}
	return true
}

// ExplainAnalyze runs EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) over a captured
// statement with its captured arguments and returns the plan text. It is for
// the read-only plan-check --explain diagnostic: it refuses anything that is
// not a plain SELECT/WITH (IsExplainable), and it must be called inside a
// UserReadTx. ANALYZE executes the statement, which the read-only transaction
// makes harmless.
func (tx *UserTx) ExplainAnalyze(ctx context.Context, c CapturedStatement) (string, error) {
	if !IsExplainable(c.SQL) {
		return "", fmt.Errorf("store: refusing to explain a statement that is not a plain SELECT/WITH")
	}
	args := c.Args
	// Drop pgx's per-call execution-mode arguments; replay in the default mode.
	for len(args) > 0 {
		switch args[0].(type) {
		case pgx.QueryExecMode, pgx.QueryResultFormats, pgx.QueryResultFormatsByOID:
			args = args[1:]
			continue
		}
		break
	}
	rows, err := tx.tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "+c.SQL, args...)
	if err != nil {
		return "", fmt.Errorf("store: explain: %w", err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", fmt.Errorf("store: explain scan: %w", err)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("store: explain: %w", err)
	}
	return sb.String(), nil
}

