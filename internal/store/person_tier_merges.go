package store

import (
	"context"
	"fmt"
)

// reconcilePersonTierOptOutEffectsTx applies the merged opt-outs to automatic
// grants and updates prior merge journals only when this reconciliation removes
// a grant. This lets an exact split restore grants removed by the merge without
// treating an earlier manual removal as part of the current merge.
func (s *Store) reconcilePersonTierOptOutEffectsTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
) error {
	grantPresenceBefore := make(map[string]bool, 2)
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		var present bool
		query := "SELECT EXISTS(SELECT 1 FROM " + table + " WHERE person_id=?)"
		if err := tx.QueryRowContext(ctx, query, personID).Scan(&present); err != nil {
			return fmt.Errorf("inspect merged %s before opt-out reconciliation: %w", table, err)
		}
		grantPresenceBefore[table] = present
	}

	if err := s.reconcilePersonTierOptOutStateTx(ctx, tx, personID); err != nil {
		return err
	}

	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		var present bool
		query := "SELECT EXISTS(SELECT 1 FROM " + table + " WHERE person_id=?)"
		if err := tx.QueryRowContext(ctx, query, personID).Scan(&present); err != nil {
			return fmt.Errorf("inspect merged %s after opt-out reconciliation: %w", table, err)
		}
		if grantPresenceBefore[table] && !present {
			const query = "UPDATE person_merge_rows SET action='deleted_snapshot'," +
				"current_row_id=NULL,current_row_key=NULL " +
				"WHERE table_name=? AND current_row_id=? AND split_id IS NULL"
			if _, err := tx.ExecContext(ctx, query, table, personID); err != nil {
				return fmt.Errorf("journal opt-out removal of merged %s: %w", table, err)
			}
		}
	}
	return nil
}
