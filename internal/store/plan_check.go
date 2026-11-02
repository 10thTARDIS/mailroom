// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"
)

// PlanCacheMode is a value of PostgreSQL's plan_cache_mode setting that the
// plan-check diagnostic is allowed to apply. The set is closed on purpose: the
// helper below interpolates nothing, but a closed set also keeps a future
// caller from reaching for a mode (or a setting) this diagnostic was not
// written for.
type PlanCacheMode string

const (
	// PlanCacheAuto is PostgreSQL's default: custom plans for the first few
	// executions of a prepared statement, then the generic plan if it looks no
	// worse. It is the mode every statement in this package normally runs in.
	PlanCacheAuto PlanCacheMode = "auto"
	// PlanCacheForceGeneric reuses the parameter-blind generic plan on every
	// execution.
	PlanCacheForceGeneric PlanCacheMode = "force_generic_plan"
	// PlanCacheForceCustom plans with the actual parameter values on every
	// execution.
	PlanCacheForceCustom PlanCacheMode = "force_custom_plan"
)

// SetPlanCacheMode sets plan_cache_mode for the REST OF THIS TRANSACTION ONLY
// (set_config with is_local = true, the parameterised form of SET LOCAL), so a
// pooled connection can never carry the mode into another request. Only the
// three PlanCacheMode values are accepted. It applies to statements pgx has
// prepared (its default statement cache): the mode is read when a prepared
// statement executes, so it takes effect on a statement already prepared on
// the connection too.
func (tx *UserTx) SetPlanCacheMode(ctx context.Context, mode PlanCacheMode) error {
	switch mode {
	case PlanCacheAuto, PlanCacheForceGeneric, PlanCacheForceCustom:
	default:
		return fmt.Errorf("store: plan_cache_mode %q is not one of auto, force_generic_plan, force_custom_plan", string(mode))
	}
	if _, err := tx.tx.Exec(ctx, `SELECT set_config('plan_cache_mode', $1, true)`, string(mode)); err != nil {
		return fmt.Errorf("store: set plan_cache_mode: %w", err)
	}
	return nil
}

// PlanCacheModeSetting reads plan_cache_mode as this transaction sees it. The
// diagnostic's tests use it to prove the mode does not outlive the
// transaction.
func (tx *UserTx) PlanCacheModeSetting(ctx context.Context) (string, error) {
	var v string
	if err := tx.tx.QueryRow(ctx, `SELECT current_setting('plan_cache_mode')`).Scan(&v); err != nil {
		return "", fmt.Errorf("store: read plan_cache_mode: %w", err)
	}
	return v, nil
}

// SetStatementTimeout bounds every later statement in this transaction
// (transaction-local, like SetPlanCacheMode). A diagnostic that deliberately
// forces the worse plan uses it as a safety net.
func (tx *UserTx) SetStatementTimeout(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("store: statement timeout must be positive, got %s", d)
	}
	if _, err := tx.tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`,
		fmt.Sprintf("%dms", d.Milliseconds())); err != nil {
		return fmt.Errorf("store: set statement_timeout: %w", err)
	}
	return nil
}
