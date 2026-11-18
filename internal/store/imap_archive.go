package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"go.kenn.io/msgvault/internal/emailtags"
)

const imapArchiveSourceType = "imap"

// IMAPPendingMove is the durable evidence for one native MOVE. A zero
// DestinationUID means dispatch may have happened but no mapping was saved.
type IMAPPendingMove struct {
	SourceIdentifier       string
	Target                 EmailTagTarget
	Destination            string
	DestinationUIDValidity uint32
	DestinationUID         uint32
}

// CheckIMAPArchiveTargetContext validates a previously authorized target.
func (s *Store) CheckIMAPArchiveTargetContext(ctx context.Context, target EmailTagTarget) (EmailTagTarget, error) {
	if target.Provider != imapArchiveSourceType {
		return target, errors.New("source is not IMAP")
	}
	copies, err := s.imapCurrentCopies(ctx, target.SourceID, target.ID)
	if err != nil {
		return target, fmt.Errorf("read IMAP memberships: %w", err)
	}
	if err := imapCopyCountFailure(len(copies)); err != nil {
		return target, err
	}
	return target, imapDraftIdle(ctx, s.db, "", target.SourceID, target.ID)
}

// imapCopyCountFailure explains why a message with n current copies cannot be
// moved; it returns nil for exactly one.
func imapCopyCountFailure(n int) error {
	switch n {
	case 1:
		return nil
	case 0:
		return emailtags.Failure("stale_identity", "no current mailbox copy for this message; run msgvault sync-full for the account without folder filters first", nil, nil)
	}
	return emailtags.Failure("stale_identity", "exactly one current copy required; mailbox selection is unavailable", nil, nil)
}

func (s *Store) IMAPPendingMoveContext(ctx context.Context, sourceID, messageID int64) (*IMAPPendingMove, error) {
	moves, err := loadIMAPPendingMoves(ctx, s.db, sourceID, messageID)
	if err != nil {
		return nil, err
	}
	if len(moves) == 0 {
		return nil, nil //nolint:nilnil // No row means no pending move.
	}
	return &moves[0], nil
}

func (s *Store) checkIMAPMoveSource(ctx context.Context, tx *loggedTx, m IMAPPendingMove) error {
	var identifier string
	err := tx.QueryRowContext(ctx, `SELECT identifier FROM sources WHERE id=? AND source_type='imap' AND NOT EXISTS(SELECT 1 FROM source_settings ss WHERE ss.source_id=sources.id AND ss.merged_into_source_id IS NOT NULL)`, m.Target.SourceID).Scan(&identifier)
	if err != nil {
		return fmt.Errorf("read IMAP account identity: %w", err)
	}
	if identifier != m.SourceIdentifier {
		return errors.New("IMAP account identity changed")
	}
	return imapDraftIdle(ctx, tx, s.dialect.SelectForUpdate(), m.Target.SourceID, m.Target.ID)
}

// BeginIMAPMoveContext writes intent before dispatch, under the source lock.
func (s *Store) BeginIMAPMoveContext(ctx context.Context, m IMAPPendingMove) error {
	t := m.Target
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{t.SourceID}}, func(tx *loggedTx) error {
		if err := s.checkIMAPMoveSource(ctx, tx, m); err != nil {
			return err
		}
		var count, exact int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(CASE WHEN mm.mailbox=? AND mm.uidvalidity=? AND mm.uid=? THEN 1 END) FROM messages msg JOIN sources src ON src.id=msg.source_id JOIN imap_message_memberships mm ON mm.source_id=msg.source_id AND mm.message_id=msg.id JOIN imap_folder_state fs ON fs.source_id=mm.source_id AND fs.mailbox=mm.mailbox AND fs.uidvalidity=mm.uidvalidity WHERE msg.id=? AND msg.source_id=? AND msg.source_message_id=? AND msg.deleted_at IS NULL AND msg.deleted_from_source_at IS NULL AND NOT EXISTS(SELECT 1 FROM source_settings ss WHERE ss.source_id=src.id AND ss.merged_into_source_id IS NOT NULL)`, t.Mailbox, t.UIDValidity, t.UID, t.ID, t.SourceID, t.SourceMessageID).Scan(&count, &exact)
		if err != nil {
			return err
		}
		if count != 1 || exact != 1 {
			return errors.New("IMAP membership changed")
		}

		var epoch uint32
		err = tx.QueryRowContext(ctx, `SELECT uidvalidity FROM imap_folder_state WHERE source_id=? AND mailbox=?`, t.SourceID, m.Destination).Scan(&epoch)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && epoch != m.DestinationUIDValidity {
			return errors.New("destination IMAP epoch changed; sync before retrying")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO imap_pending_moves(source_id,message_id,source_message_id,source_identifier,old_mailbox,old_uidvalidity,old_uid,destination,destination_uidvalidity) VALUES(?,?,?,?,?,?,?,?,?)`, t.SourceID, t.ID, t.SourceMessageID, m.SourceIdentifier, t.Mailbox, t.UIDValidity, t.UID, m.Destination, m.DestinationUIDValidity)
		return err
	})
}

// imapDraftIdle reads the managed draft that owns the message, as its current
// copy or a pending edit's original, and refuses while an edit or delete is
// pending. A transaction passes the dialect's row lock; draft claims refuse a
// pending move, so either operation sees the other's evidence. The conflict
// carries no draft or mailbox detail.
func imapDraftIdle(ctx context.Context, q contextRowQuerier, lock string, sourceID, messageID int64) error {
	var pending sql.NullString
	err := q.QueryRowContext(ctx, `SELECT pending_operation FROM imap_drafts WHERE source_id=? AND (current_message_id=? OR pending_original_message_id=?)`+lock, sourceID, messageID, messageID).Scan(&pending)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read managed draft for IMAP move: %w", err)
	}
	if pending.Valid {
		return emailtags.Failure("stale_identity", "a managed draft operation is pending for this message; finish or recover the draft first", nil, ErrIMAPDraftPending)
	}
	return nil
}

// moveIMAPDraftReceiptTx follows a verified move with the managed draft's
// current receipt, so later edits and deletes address the relocated copy.
func (s *Store) moveIMAPDraftReceiptTx(ctx context.Context, tx *loggedTx, m IMAPPendingMove) error {
	t := m.Target
	_, err := tx.ExecContext(ctx, `UPDATE imap_drafts SET current_mailbox=?, current_uidvalidity=?, current_uid=?, updated_at=`+s.dialect.Now()+` WHERE source_id=? AND current_message_id=? AND current_mailbox=? AND current_uidvalidity=? AND current_uid=?`, m.Destination, m.DestinationUIDValidity, m.DestinationUID, t.SourceID, t.ID, t.Mailbox, t.UIDValidity, t.UID)
	return err
}

// SaveIMAPMoveMappingContext persists COPYUID immediately after the response.
func (s *Store) SaveIMAPMoveMappingContext(ctx context.Context, m IMAPPendingMove) error {
	if m.DestinationUID == 0 {
		return errors.New("invalid destination UID")
	}
	t := m.Target
	result, err := s.db.ExecContext(ctx, `UPDATE imap_pending_moves SET destination_uid=? WHERE source_id=? AND message_id=? AND source_message_id=? AND old_mailbox=? AND old_uidvalidity=? AND old_uid=? AND destination=? AND destination_uidvalidity=? AND destination_uid IS NULL`, m.DestinationUID, t.SourceID, t.ID, t.SourceMessageID, t.Mailbox, t.UIDValidity, t.UID, m.Destination, m.DestinationUIDValidity)
	return imapMoveRowResult(result, err, "pending IMAP move changed")
}

func imapMoveRowResult(result sql.Result, err error, message string) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New(message)
	}
	return nil
}

// InvalidateIMAPMoveMappingContext disowns a disputed receipt. A failed write
// or crash before commit leaves the first mapping trusted; a lone wrong receipt
// cannot be detected.
func (s *Store) InvalidateIMAPMoveMappingContext(ctx context.Context, m IMAPPendingMove) error {
	t := m.Target
	result, err := s.db.ExecContext(ctx, `UPDATE imap_pending_moves SET destination_uid=NULL WHERE source_id=? AND message_id=? AND source_message_id=? AND source_identifier=? AND old_mailbox=? AND old_uidvalidity=? AND old_uid=? AND destination=? AND destination_uidvalidity=? AND destination_uid=? AND EXISTS(SELECT 1 FROM sources WHERE id=imap_pending_moves.source_id AND source_type='imap' AND identifier=? AND NOT EXISTS(SELECT 1 FROM source_settings ss WHERE ss.source_id=sources.id AND ss.merged_into_source_id IS NOT NULL))`, t.SourceID, t.ID, t.SourceMessageID, m.SourceIdentifier, t.Mailbox, t.UIDValidity, t.UID, m.Destination, m.DestinationUIDValidity, m.DestinationUID, m.SourceIdentifier)
	return imapMoveRowResult(result, err, "pending IMAP move changed")
}

// ClearIMAPMoveContext removes an intent only when dispatch definitely did not happen.
func (s *Store) ClearIMAPMoveContext(ctx context.Context, m IMAPPendingMove) error {
	t := m.Target
	_, err := s.db.ExecContext(ctx, `DELETE FROM imap_pending_moves WHERE source_id=? AND message_id=? AND source_message_id=? AND old_mailbox=? AND old_uidvalidity=? AND old_uid=? AND destination=? AND destination_uidvalidity=? AND destination_uid IS NULL`, t.SourceID, t.ID, t.SourceMessageID, t.Mailbox, t.UIDValidity, t.UID, m.Destination, m.DestinationUIDValidity)
	return err
}

// ReconcileIMAPMoveContext installs a verified pair without changing content,
// other mailbox copies, or sync cursors. It consumes the exact saved mapping.
func (s *Store) ReconcileIMAPMoveContext(ctx context.Context, m IMAPPendingMove) error {
	if m.DestinationUID == 0 {
		return errors.New("missing IMAP move mapping")
	}
	t := m.Target
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{t.SourceID}}, func(tx *loggedTx) error {
		if err := s.checkIMAPMoveSource(ctx, tx, m); err != nil {
			return err
		}
		var count int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM imap_pending_moves p JOIN messages msg ON msg.id=p.message_id AND msg.source_id=p.source_id AND msg.source_message_id=p.source_message_id JOIN imap_message_memberships mm ON mm.source_id=p.source_id AND mm.message_id=p.message_id AND mm.mailbox=p.old_mailbox AND mm.uidvalidity=p.old_uidvalidity AND mm.uid=p.old_uid JOIN imap_folder_state fs ON fs.source_id=mm.source_id AND fs.mailbox=mm.mailbox AND fs.uidvalidity=mm.uidvalidity WHERE p.source_id=? AND p.message_id=? AND p.source_message_id=? AND p.old_mailbox=? AND p.old_uidvalidity=? AND p.old_uid=? AND p.destination=? AND p.destination_uidvalidity=? AND p.destination_uid=? AND msg.deleted_at IS NULL AND msg.deleted_from_source_at IS NULL`, t.SourceID, t.ID, t.SourceMessageID, t.Mailbox, t.UIDValidity, t.UID, m.Destination, m.DestinationUIDValidity, m.DestinationUID).Scan(&count)
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("pending IMAP move identity changed")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO imap_folder_state(source_id,mailbox,uidvalidity,uidnext) VALUES(?,?,?,0) ON CONFLICT(source_id,mailbox) DO NOTHING`, t.SourceID, m.Destination, m.DestinationUIDValidity)
		if err != nil {
			return err
		}
		var epoch uint32
		if err = tx.QueryRowContext(ctx, `SELECT uidvalidity FROM imap_folder_state WHERE source_id=? AND mailbox=?`, t.SourceID, m.Destination).Scan(&epoch); err != nil {
			return err
		}
		if epoch != m.DestinationUIDValidity {
			return errors.New("destination IMAP epoch changed")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO imap_message_memberships(source_id,mailbox,uidvalidity,uid,message_id,flags) SELECT source_id,?,?,?,message_id,flags FROM imap_message_memberships WHERE source_id=? AND message_id=? AND mailbox=? AND uidvalidity=? AND uid=?`, m.Destination, m.DestinationUIDValidity, m.DestinationUID, t.SourceID, t.ID, t.Mailbox, t.UIDValidity, t.UID)
		if err != nil {
			return fmt.Errorf("attach destination IMAP membership: %w", err)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM imap_message_memberships WHERE source_id=? AND message_id=? AND mailbox=? AND uidvalidity=? AND uid=?`, t.SourceID, t.ID, t.Mailbox, t.UIDValidity, t.UID)
		if err := imapMoveRowResult(result, err, "source IMAP membership changed"); err != nil {
			return err
		}
		if _, err := s.rebuildIMAPMailboxLabelsTx(ctx, tx, t.SourceID, t.ID); err != nil {
			return err
		}
		if err := s.moveIMAPDraftReceiptTx(ctx, tx, m); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `DELETE FROM imap_pending_moves WHERE source_id=? AND message_id=? AND destination_uid=?`, t.SourceID, t.ID, m.DestinationUID)
		return imapMoveRowResult(result, err, "pending IMAP move changed")
	})
}

// IMAPPendingMovesContext returns all durable move evidence for one source.
func (s *Store) IMAPPendingMovesContext(ctx context.Context, sourceID int64) ([]IMAPPendingMove, error) {
	return loadIMAPPendingMoves(ctx, s.db, sourceID, 0)
}

func loadIMAPPendingMoves(ctx context.Context, q contextRowsQuerier, sourceID, messageID int64) ([]IMAPPendingMove, error) {
	query := `SELECT message_id,source_message_id,source_identifier,old_mailbox,old_uidvalidity,old_uid,destination,destination_uidvalidity,destination_uid FROM imap_pending_moves WHERE source_id=?`
	args := []any{sourceID}
	if messageID != 0 {
		query += ` AND message_id=?`
		args = append(args, messageID)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var moves []IMAPPendingMove
	for rows.Next() {
		var m IMAPPendingMove
		var uid sql.NullInt64
		m.Target.SourceID, m.Target.Provider = sourceID, imapArchiveSourceType
		if err := rows.Scan(&m.Target.ID, &m.Target.SourceMessageID, &m.SourceIdentifier, &m.Target.Mailbox, &m.Target.UIDValidity, &m.Target.UID, &m.Destination, &m.DestinationUIDValidity, &uid); err != nil {
			return nil, err
		}
		if uid.Valid {
			if uid.Int64 <= 0 || uid.Int64 > math.MaxUint32 {
				return nil, errors.New("invalid pending destination UID")
			}
			m.DestinationUID = uint32(uid.Int64)
		}
		moves = append(moves, m)
	}
	return moves, rows.Err()
}

func (s *Store) imapCurrentCopies(ctx context.Context, sourceID, messageID int64) ([]EmailTagTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.mailbox,m.uidvalidity,m.uid FROM imap_message_memberships m JOIN imap_folder_state f ON f.source_id=m.source_id AND f.mailbox=m.mailbox AND f.uidvalidity=m.uidvalidity WHERE m.source_id=? AND m.message_id=?`, sourceID, messageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var copies []EmailTagTarget
	for rows.Next() {
		var item EmailTagTarget
		if err := rows.Scan(&item.Mailbox, &item.UIDValidity, &item.UID); err != nil {
			return nil, err
		}
		copies = append(copies, item)
	}
	return copies, rows.Err()
}

// IMAPCurrentCopyContext resolves the current membership without guessing among copies.
func (s *Store) IMAPCurrentCopyContext(ctx context.Context, sourceID int64, key string) (EmailTagTarget, bool, error) {
	var target EmailTagTarget
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT id, EXISTS(SELECT 1 FROM imap_pending_moves p WHERE p.source_id=messages.source_id AND p.message_id=messages.id) FROM messages WHERE source_id=? AND source_message_id=?`, sourceID, key).Scan(&target.ID, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return target, false, errors.New("no current IMAP copy; run msgvault sync-full without folder filters")
	}
	if err != nil {
		return target, false, err
	}
	copies, err := s.imapCurrentCopies(ctx, sourceID, target.ID)
	if err != nil {
		return target, pending, err
	}
	if len(copies) == 0 {
		if pending {
			return target, pending, nil
		}
		return target, pending, errors.New("no current IMAP copy; run msgvault sync-full without folder filters")
	}

	for _, item := range copies {
		if IMAPSourceMessageID(item.Mailbox, item.UID) == key {
			return item, pending, nil
		}
	}
	if len(copies) != 1 {
		return target, pending, errors.New("IMAP current copy is stale or ambiguous")
	}
	return copies[0], pending, nil
}
