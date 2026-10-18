package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingLinksDedupUndoAndPurge(t *testing.T) {
	for _, targetSide := range []bool{false, true} {
		t.Run(fmt.Sprintf("target=%t", targetSide), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := newRFC822IDBackfillBackendStore(t)
			uid := "dedup@example.org"
			if targetSide {
				uid = ""
			}
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", uid))
			_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
			survivor := note
			var duplicate int64
			if targetSide {
				_, duplicate = calendarFixture(t, st, "duplicate", singleLinkRaw("duplicate", uid))
				survivor = event
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: duplicate}))
				var target int64
				requirements.NoError(st.db.QueryRow(`SELECT target_message_id FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&target))
				requirements.Equal(duplicate, target)
				requirements.Equal(fmt.Sprintf("evt:%d", duplicate), linkedMeetingRefs(t, st, note)[note])
			} else {
				_, duplicate = projectionFixture(t, st, "duplicate", "plaud_json", `{}`)
				requirements.NoError(st.LinkMeetingContext(t.Context(), duplicate, OccurrenceRef{MessageID: event}))
			}
			_, err := st.MergeDuplicates(survivor, []int64{duplicate}, "meetings")
			requirements.NoError(err)
			assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
			count, err := st.UndoDedup("meetings")
			requirements.NoError(err)
			assertions.Equal(int64(1), count)
			if targetSide {
				assertions.Equal(fmt.Sprintf("evt:%d,evt:%d", event, duplicate), linkedMeetingRefs(t, st, note)[note])
			} else {
				keys := linkedMeetingRefs(t, st, note, duplicate)
				assertions.Equal(fmt.Sprintf("evt:%d", event), keys[note])
				assertions.Equal(fmt.Sprintf("evt:%d", event), keys[duplicate])
			}
			_, err = st.MergeDuplicates(survivor, []int64{duplicate}, "purge")
			requirements.NoError(err)
			count, err = st.DeleteDedupedBatchesContext(t.Context(), []string{"purge"})
			requirements.NoError(err)
			assertions.Equal(int64(1), count)
			assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
		})
	}
}

func TestMeetingLinksConsolidationDecisionOrder(t *testing.T) {
	for _, tc := range []struct {
		name              string
		linkDay, splitDay int
		want              string
	}{
		{"newer split", 1, 2, "split"}, {"newer link", 2, 1, "link"}, {"equal split", 1, 1, "split"},
	} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", tc.name, reverse), func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)

				st := newRFC822IDBackfillBackendStore(t)
				_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "copy@example.org"))
				_, a := projectionFixture(t, st, "a", "plaud_json", `{}`)
				_, b := projectionFixture(t, st, "b", "plaud_json", `{}`)
				for _, d := range []struct {
					member int64
					kind   string
					day    int
				}{{a, "link", tc.linkDay}, {b, "split", tc.splitDay}} {
					_, err := st.db.Exec(`INSERT INTO meeting_link_decisions(member_message_id,target_ical_uid,occurrence_key,decision,created_at) VALUES (?,'copy@example.org','',?,?)`, d.member, d.kind, time.Date(2027, 1, d.day, 0, 0, 0, 0, time.UTC))
					requirements.NoError(err)
				}
				if reverse {
					a, b = b, a
				}
				_, err := st.MergeDuplicates(a, []int64{b}, "order")
				requirements.NoError(err)
				var decision string
				requirements.NoError(st.db.QueryRow(`SELECT decision FROM meeting_link_decisions WHERE member_message_id = ?`, a).Scan(&decision))
				assertions.Equal(tc.want, decision)
				want := ""
				if tc.want == "link" {
					want = fmt.Sprintf("evt:%d", event)
				}
				assertions.Equal(want, linkedMeetingRefs(t, st, a)[a])
			})
		}
	}
}

func TestMeetingLinksSourceMessageIDMigration(t *testing.T) {
	for _, targetSide := range []bool{false, true} {
		t.Run(fmt.Sprintf("target=%t", targetSide), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := newRFC822IDBackfillBackendStore(t)
			uid := "migration@example.org"
			if targetSide {
				uid = ""
			}
			eventData, event := calendarFixture(t, st, "event", singleLinkRaw("event", uid))
			noteData, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
			var source, conversation int64
			oldKey, newKey := "legacy-note", "note"
			if targetSide {
				oldData, old := calendarFixture(t, st, "legacy-event", singleLinkRaw("legacy-event", uid))
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: old}))
				var target int64
				requirements.NoError(st.db.QueryRow(`SELECT target_message_id FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&target))
				requirements.Equal(old, target)
				requirements.Equal(fmt.Sprintf("evt:%d", old), linkedMeetingRefs(t, st, note)[note])
				source, conversation = eventData.Message.SourceID, oldData.Message.ConversationID
				oldKey, newKey = "legacy-event", "event"
			} else {
				oldData, old := projectionFixture(t, st, "legacy-note", "plaud_json", `{}`)
				requirements.NoError(st.LinkMeetingContext(t.Context(), old, OccurrenceRef{MessageID: event}))
				source, conversation = noteData.Message.SourceID, oldData.Message.ConversationID
			}
			requirements.NoError(st.MigrateSourceMessageID(source, conversation, oldKey, newKey))
			assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
		})
	}
}

func TestMeetingLinksSourceMerge(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "merge@example.org"))
	data, note := projectionFixture(t, st, "same-note", "plaud_json", `{}`)
	source, err := st.GetOrCreateSource("meeting_import", "copy@example.com")
	requirements.NoError(err)
	sender, err := st.EnsureParticipant("speaker@example.com", "Speaker Example", "")
	requirements.NoError(err)
	data.Message.SenderID.Int64 = sender
	data.Message.SenderID.Valid = true
	data.Message.SentAt.Time = time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	data.Message.SentAt.Valid = true
	_, err = st.UpsertMessage(data.Message)
	requirements.NoError(err)
	data.Conversation.SourceConversationID = "!meeting:example.org"
	_, err = st.db.Exec(`UPDATE conversations SET source_conversation_id = ? WHERE id = ?`, data.Conversation.SourceConversationID, data.Message.ConversationID)
	requirements.NoError(err)
	data.Message.ID = 0
	data.Message.SourceID = source.ID
	data.Message.ConversationID = 0
	duplicate, err := st.PersistMessage(data)
	requirements.NoError(err)
	requirements.NoError(st.LinkMeetingContext(t.Context(), duplicate, OccurrenceRef{MessageID: event}))
	var originalSource int64
	requirements.NoError(st.db.QueryRow(`SELECT source_id FROM messages WHERE id = ?`, note).Scan(&originalSource))
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: originalSource})
	requirements.NoError(err)
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
}

func TestMeetingLinksSubsetCopy(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	requirements.NoError(st.InitSchemaContext(t.Context()))
	data, event := calendarFixture(t, st, "event", singleLinkRaw("event", "subset@example.org"))
	data.Message.SentAt.Time = time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	data.Message.SentAt.Valid = true
	_, err = st.UpsertMessage(data.Message)
	requirements.NoError(err)
	noteData, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	noteData.Message.SentAt.Time = time.Date(2027, 3, 14, 11, 0, 0, 0, time.UTC)
	noteData.Message.SentAt.Valid = true
	_, err = st.UpsertMessage(noteData.Message)
	requirements.NoError(err)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	for _, limit := range []int{1, 2} {
		dir := t.TempDir()
		_, err := CopySubset(path, dir, limit, false)
		requirements.NoError(err)
		dst, err := OpenForTest(filepath.Join(dir, "msgvault.db"))
		requirements.NoError(err)
		var count int
		requirements.NoError(dst.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
		assertions.Equal(limit-1, count)
		if limit == 2 {
			assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, dst, note)[note])
		}
		requirements.NoError(dst.Close())
	}
}

func TestMeetingDecisionDedupJournalsDerivedRefresh(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "journal@example.org"))
	_, survivor := projectionFixture(t, st, "survivor", "plaud_json", `{}`)
	_, duplicate := projectionFixture(t, st, "duplicate", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), duplicate, OccurrenceRef{MessageID: event}))
	_, before, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	_, err = st.MergeDuplicates(survivor, []int64{duplicate}, "journal")
	requirements.NoError(err)
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, survivor)[survivor])
	if !st.IsPostgreSQL() {
		var count int
		requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE seq > ? AND dataset = 'meeting_links' AND message_id = ?`, before, survivor).Scan(&count))
		assertions.Positive(count, "the cache must refresh derived meeting counts after moving the decision")
	}
}

func TestMeetingDecisionSubsetJournalsBeforeProjection(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	path := filepath.Join(t.TempDir(), "source.db")
	src, err := OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(src.Close()) })
	requirements.NoError(src.InitSchema())
	_, event := calendarFixture(t, src, "event", singleLinkRaw("event", "subset@example.org"))
	_, note := projectionFixture(t, src, "note", "plaud_json", `{}`)
	requirements.NoError(src.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	dst, err := OpenForTest(filepath.Join(t.TempDir(), "destination.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(dst.Close()) })
	requirements.NoError(dst.InitSchema())
	conn, err := dst.db.Conn(t.Context())
	requirements.NoError(err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `ATTACH DATABASE ? AS src`, path)
	requirements.NoError(err)
	tx, err := conn.BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer tx.Rollback()
	_, err = copyData(tx, 2, CopySubsetOptions{})
	requirements.NoError(err)
	var count int
	requirements.NoError(tx.QueryRow(`SELECT count(*) FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&count))
	assertions.Equal(1, count)
	requirements.NoError(tx.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset = 'meeting_links' AND message_id = ?`, note).Scan(&count))
	assertions.Positive(count, "copying a decision must request a derived refresh in the copy transaction")
}
