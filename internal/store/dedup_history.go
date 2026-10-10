package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DuplicateMergePreservesHistoryContext checks content that deduplication does
// not transfer. Archive-only copies must remain visible if that content is unique.
func (s *Store) DuplicateMergePreservesHistoryContext(ctx context.Context, survivorID int64, duplicateIDs []int64) (bool, error) {
	return duplicateMergePreservesHistory(ctx, s.db, survivorID, duplicateIDs)
}

func duplicateMergePreservesHistory(ctx context.Context, q contextStatementQuerier, survivorID int64, duplicateIDs []int64) (bool, error) {
	for _, id := range duplicateIDs {
		var archiveOnly bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM source_merge_archive_only_messages WHERE message_id = ?
		)`, id).Scan(&archiveOnly); err != nil {
			return false, fmt.Errorf("check duplicate %d archive status: %w", id, err)
		}
		if !archiveOnly {
			continue
		}
		var historicalText, historicalHTML, survivorText, survivorHTML sql.NullString
		err := q.QueryRowContext(ctx, `SELECT body_text, body_html FROM message_bodies WHERE message_id = ?`, id).
			Scan(&historicalText, &historicalHTML)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("read historical duplicate %d body: %w", id, err)
		}
		err = q.QueryRowContext(ctx, `SELECT body_text, body_html FROM message_bodies WHERE message_id = ?`, survivorID).
			Scan(&survivorText, &survivorHTML)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("read survivor %d body: %w", survivorID, err)
		}
		if historicalText.String != "" && historicalText.String != survivorText.String ||
			historicalHTML.String != "" && historicalHTML.String != survivorHTML.String {
			return false, nil
		}
		var missingAttachment bool
		err = q.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM attachments historical
			WHERE historical.message_id = ? AND (
				COALESCE(historical.content_hash, '') = '' OR NOT EXISTS (
					SELECT 1 FROM attachments survivor
					WHERE survivor.message_id = ?
					  AND survivor.content_hash = historical.content_hash
					  AND COALESCE(survivor.filename, '') = COALESCE(historical.filename, '')
					  AND COALESCE(survivor.mime_type, '') = COALESCE(historical.mime_type, '')
					  AND survivor.attachment_role = historical.attachment_role
					  AND COALESCE(survivor.content_id, '') = COALESCE(historical.content_id, '')
					  AND (historical.storage_path = '' OR survivor.storage_path <> '')
				)
			)
		)`, id, survivorID).Scan(&missingAttachment)
		if err != nil {
			return false, fmt.Errorf("compare historical duplicate %d attachments: %w", id, err)
		}
		if missingAttachment {
			return false, nil
		}
	}
	return true, nil
}
