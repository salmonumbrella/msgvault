package identityindex

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonMeetingCountDedupesCopiesGlobalAndSourceFiltered(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	root, db := writeRelationshipBaseFixture(t, false)
	replaceRelationshipParquet(t, db, root, "messages", `
 SELECT id::BIGINT AS id, source_id::BIGINT AS source_id,
        'synthetic-' || id AS source_message_id, 10::BIGINT AS conversation_id,
        'Planning'::VARCHAR AS subject, ''::VARCHAR AS snippet,
        TIMESTAMP '2026-07-20 10:30:00' AS sent_at,
        50::BIGINT AS size_estimate, false AS has_attachments,
        0::INTEGER AS attachment_count, NULL::TIMESTAMP AS deleted_from_source_at,
        1::BIGINT AS sender_id, NULL::BIGINT AS owner_participant_id,
        message_type::VARCHAR AS message_type, false AS is_from_me,
        2026::INTEGER AS year, 7::INTEGER AS month
 FROM (VALUES
    (100,1,'meeting_transcript'),
    (101,2,'meeting_transcript'),
    (102,1,'meeting_transcript'),
    (103,2,'calendar_event'),
    (104,1,'meeting_transcript')
 ) t(id,source_id,message_type)`)
	replaceRelationshipParquet(t, db, root, "sources", `SELECT * FROM (VALUES
 (1::BIGINT,'owner@example.com'::VARCHAR,'granola'::VARCHAR),
 (2::BIGINT,'copy@example.org'::VARCHAR,'twenty'::VARCHAR)
 ) t(id,account_email,source_type)`)
	replaceRelationshipParquet(t, db, root, "message_recipients", `SELECT id::BIGINT AS message_id,
  2::BIGINT AS participant_id,'to'::VARCHAR AS recipient_type
  FROM (VALUES(100),(101),(102),(103),(104)) t(id)`)
	replaceRelationshipParquet(t, db, root, "meeting_keys", `SELECT * FROM (VALUES
 (100::BIGINT,'ical:planning@example.org|'::VARCHAR),
 (101::BIGINT,'ical:planning@example.org|'::VARCHAR),
 (104::BIGINT,'ical:planning@example.org|'::VARCHAR)) t(message_id,meeting_key)`)
	_, err := Build(context.Background(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root})
	requirements.NoError(err)
	var count int64
	requirements.NoError(db.QueryRow(`SELECT meeting_count FROM read_parquet('` + root + `/relationship_people/*.parquet') WHERE canonical_id=2`).Scan(&count))
	assertions.Equal(int64(2), count)
	var first, second int64
	requirements.NoError(db.QueryRow(`SELECT (SELECT item.meeting_count FROM unnest(source_rollups) r(item) WHERE item.source_id=1),
 (SELECT item.meeting_count FROM unnest(source_rollups) r(item) WHERE item.source_id=2)
 FROM read_parquet('`+root+`/relationship_people/*.parquet') WHERE canonical_id=2`).Scan(&first, &second))
	assertions.Equal(int64(2), first)
	assertions.Equal(int64(1), second)
}
