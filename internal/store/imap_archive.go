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

// IMAPArchiveTargetContext refuses ambiguous copies even when the tag path
// could prefer the original source key.
func (s *Store) IMAPArchiveTargetContext(ctx context.Context, id int64) (EmailTagTarget, error) {
	target, err := s.EmailTagTargetContext(ctx, id, "")
	if err != nil {
		return target, err
	}
	return s.CheckIMAPArchiveTargetContext(ctx, target)
}

// CheckIMAPArchiveTargetContext validates a previously authorized target.
func (s *Store) CheckIMAPArchiveTargetContext(ctx context.Context, target EmailTagTarget) (EmailTagTarget, error) {
	id := target.ID
	var err error
	if target.Provider != imapArchiveSourceType {
		return target, errors.New("source is not IMAP")
	}
	var count int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM imap_message_memberships m JOIN imap_folder_state f ON f.source_id=m.source_id AND f.mailbox=m.mailbox AND f.uidvalidity=m.uidvalidity WHERE m.source_id=? AND m.message_id=?`, target.SourceID, id).Scan(&count)
	if err != nil {
		return target, fmt.Errorf("count IMAP memberships: %w", err)
	}
	if count != 1 {
		return target, emailtags.Failure("stale_identity", "IMAP message must have exactly one current mailbox copy", nil, nil)
	}
	return target, nil
}

func (s *Store) IMAPPendingMoveContext(ctx context.Context, sourceID, messageID int64) (*IMAPPendingMove, error) {
	var m IMAPPendingMove
	var uid sql.NullInt64
	m.Target.SourceID, m.Target.ID, m.Target.Provider = sourceID, messageID, imapArchiveSourceType
	err := s.db.QueryRowContext(ctx, `SELECT source_message_id,source_identifier,old_mailbox,old_uidvalidity,old_uid,destination,destination_uidvalidity,destination_uid FROM imap_pending_moves WHERE source_id=? AND message_id=?`, sourceID, messageID).Scan(&m.Target.SourceMessageID, &m.SourceIdentifier, &m.Target.Mailbox, &m.Target.UIDValidity, &m.Target.UID, &m.Destination, &m.DestinationUIDValidity, &uid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // No row means there is no pending move.
	}
	if err != nil {
		return nil, fmt.Errorf("read pending IMAP move: %w", err)
	}
	if uid.Valid {
		if uid.Int64 <= 0 || uid.Int64 > math.MaxUint32 {
			return nil, errors.New("invalid pending destination UID")
		}
		m.DestinationUID = uint32(uid.Int64)
	}
	return &m, nil
}

func checkIMAPMoveSource(ctx context.Context, tx *loggedTx, m IMAPPendingMove) error {
	var identifier string
	err := tx.QueryRowContext(ctx, `SELECT identifier FROM sources WHERE id=? AND source_type='imap' AND NOT EXISTS(SELECT 1 FROM source_settings ss WHERE ss.source_id=sources.id AND ss.merged_into_source_id IS NOT NULL)`, m.Target.SourceID).Scan(&identifier)
	if err != nil {
		return fmt.Errorf("read IMAP account identity: %w", err)
	}
	if identifier != m.SourceIdentifier {
		return errors.New("IMAP account identity changed")
	}
	return nil
}

// BeginIMAPMoveContext writes intent before dispatch, under the source lock.
func (s *Store) BeginIMAPMoveContext(ctx context.Context, m IMAPPendingMove) error {
	t := m.Target
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{t.SourceID}}, func(tx *loggedTx) error {
		if err := checkIMAPMoveSource(ctx, tx, m); err != nil {
			return err
		}
		var count int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages msg JOIN sources src ON src.id=msg.source_id JOIN imap_message_memberships mm ON mm.source_id=msg.source_id AND mm.message_id=msg.id JOIN imap_folder_state fs ON fs.source_id=mm.source_id AND fs.mailbox=mm.mailbox AND fs.uidvalidity=mm.uidvalidity WHERE msg.id=? AND msg.source_id=? AND msg.source_message_id=? AND msg.deleted_at IS NULL AND msg.deleted_from_source_at IS NULL AND NOT EXISTS(SELECT 1 FROM source_settings ss WHERE ss.source_id=src.id AND ss.merged_into_source_id IS NOT NULL)`, t.ID, t.SourceID, t.SourceMessageID).Scan(&count)
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("IMAP membership changed")
		}
		var exists bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM imap_message_memberships WHERE source_id=? AND message_id=? AND mailbox=? AND uidvalidity=? AND uid=?)`, t.SourceID, t.ID, t.Mailbox, t.UIDValidity, t.UID).Scan(&exists)
		if err != nil {
			return err
		}
		if !exists {
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

// SaveIMAPMoveMappingContext persists COPYUID immediately after the response.
func (s *Store) SaveIMAPMoveMappingContext(ctx context.Context, m IMAPPendingMove) error {
	if m.DestinationUID == 0 {
		return errors.New("invalid destination UID")
	}
	t := m.Target
	result, err := s.db.ExecContext(ctx, `UPDATE imap_pending_moves SET destination_uid=? WHERE source_id=? AND message_id=? AND source_message_id=? AND old_mailbox=? AND old_uidvalidity=? AND old_uid=? AND destination=? AND destination_uidvalidity=? AND destination_uid IS NULL`, m.DestinationUID, t.SourceID, t.ID, t.SourceMessageID, t.Mailbox, t.UIDValidity, t.UID, m.Destination, m.DestinationUIDValidity)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("pending IMAP move changed")
	}
	return nil
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
		if err := checkIMAPMoveSource(ctx, tx, m); err != nil {
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
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("source IMAP membership changed")
		}
		mailboxes, err := s.imapMembershipMailboxes(ctx, tx, t.SourceID, t.ID)
		if err != nil {
			return err
		}
		labels := make([]int64, 0, len(mailboxes))
		for _, box := range mailboxes {
			label, err := ensureIMAPMailboxLabel(ctx, tx, t.SourceID, box)
			if err != nil {
				return err
			}
			labels = append(labels, label)
		}
		if err := s.refreshAccountAttributionIfOutboundChangedTx(ctx, tx, t.ID, func() error { return replaceMessageLabelsTx(boundQuerier{ctx: ctx, q: tx}, t.ID, labels) }); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `DELETE FROM imap_pending_moves WHERE source_id=? AND message_id=? AND destination_uid=?`, t.SourceID, t.ID, m.DestinationUID)
		if err != nil {
			return err
		}
		n, err = result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("pending IMAP move changed")
		}
		return nil
	})
}

// MappedIMAPPendingMovesContext lists only moves whose exact mapping was saved.
func (s *Store) MappedIMAPPendingMovesContext(ctx context.Context, sourceID int64) ([]IMAPPendingMove, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id FROM imap_pending_moves WHERE source_id=? AND destination_uid IS NOT NULL`, sourceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var moves []IMAPPendingMove
	for _, id := range ids {
		move, err := s.IMAPPendingMoveContext(ctx, sourceID, id)
		if err != nil {
			return nil, err
		}
		if move != nil {
			moves = append(moves, *move)
		}
	}
	return moves, nil
}
