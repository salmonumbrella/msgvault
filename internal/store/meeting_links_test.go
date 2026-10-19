package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

const linkSeriesRaw = `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`

func TestMeetingLinksUnlinkSavedIdentityWhenRecurrenceCannotBeResolved(t *testing.T) {
	for _, tc := range []struct {
		name       string
		updatedRaw string
		wantErr    error
	}{
		{
			name:       "shortened series",
			updatedRaw: strings.Replace(linkSeriesRaw, "RRULE:FREQ=WEEKLY;COUNT=6", "RRULE:FREQ=WEEKLY;COUNT=1", 1),
			wantErr:    sql.ErrNoRows,
		},
		{
			name:       "excluded occurrence",
			updatedRaw: strings.Replace(linkSeriesRaw, "RRULE:FREQ=WEEKLY;COUNT=6", `RRULE:FREQ=WEEKLY;COUNT=6","EXDATE:20270314T100000Z`, 1),
			wantErr:    sql.ErrNoRows,
		},
		{
			name:       "unsupported recurrence",
			updatedRaw: strings.Replace(linkSeriesRaw, "RRULE:FREQ=WEEKLY;COUNT=6", "RRULE:FREQ=HOURLY", 1),
			wantErr:    calrecur.ErrUnsupportedRecurrence,
		},
		{
			name: "recurrence budget",
			updatedRaw: strings.NewReplacer(
				"2027-03-07T10:00:00Z", "1800-01-01T10:00:00Z",
				"2027-03-07T11:00:00Z", "1800-01-01T11:00:00Z",
				"RRULE:FREQ=WEEKLY;COUNT=6", "RRULE:FREQ=DAILY;COUNT=100000",
			).Replace(linkSeriesRaw),
			wantErr: ErrQueryTooBroad,
		},
	} {
		for _, uidCase := range []struct{ name, uid string }{
			{"with UID", "weekly@example.org"},
			{"without UID", ""},
			{"later UID", ""},
		} {
			t.Run(uidCase.name+"/"+tc.name, func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				_, automatic := calendarFixture(t, st, "automatic", singleLinkRaw("automatic", "automatic@example.org"))
				_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"automatic@example.org"}}`)
				raw := strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", uidCase.uid)
				_, master := calendarFixture(t, st, "master", raw)
				ref := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
				requirements.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), linkedMeetingRefs(t, st, note)[note])
				if uidCase.name == "later UID" {
					requirements.NoError(st.SetMessageMetadata(master, sql.NullString{String: `{"calendar_id":"calendar@example.com","ical_uid":"weekly@example.org"}`, Valid: true}))
				}
				requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(strings.ReplaceAll(tc.updatedRaw, "weekly@example.org", uidCase.uid)), "gcal_json"))
				links, err := st.MeetingLinksContext(t.Context(), note)
				if errors.Is(tc.wantErr, ErrQueryTooBroad) {
					requirements.ErrorIs(err, ErrQueryTooBroad)
				} else {
					requirements.NoError(err)
					requirements.Empty(links, "the saved override still suppresses the automatic match")
				}
				requirements.ErrorIs(st.LinkMeetingContext(t.Context(), note, ref), tc.wantErr)
				requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, ref))
				assertions.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note], "other links remain readable before the recurrence is repaired")
				requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(raw), "gcal_json"))
				assertions.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note], "the saved split continues to suppress the restored occurrence")
				requirements.ErrorIs(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270425T100000Z"}), sql.ErrNoRows)
			})
		}
	}
}

func TestMeetingLinksSplitBeforeOccurrenceRemovalAfterUIDAcquisition(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, automatic := calendarFixture(t, st, "automatic", singleLinkRaw("automatic", "automatic@example.org"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"automatic@example.org"}}`)
	raw := strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", "")
	_, master := calendarFixture(t, st, "master", raw)
	ref := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
	requirements.NoError(st.SetMessageMetadata(master, sql.NullString{String: `{"ical_uid":"weekly@example.org"}`, Valid: true}))
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, ref))
	requirements.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(strings.Replace(raw, "COUNT=6", "COUNT=1", 1)), "gcal_json"))
	assert.Equal(t, fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note], "the superseded manual link must keep allowing unrelated automatic matches")
}

func TestMeetingLinksSplitPersistsUIDAcquisition(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	uid := "acquired@example.org"
	noteRaw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T10:00:00Z"}}`, uid)
	_, splitNote := projectionFixture(t, st, "split-note", "twenty_json", noteRaw)
	_, linkedNote := projectionFixture(t, st, "linked-note", "twenty_json", noteRaw)
	data, event := calendarFixture(t, st, "uidless", singleLinkRaw("uidless", ""), "original-calendar@example.com")
	ref := OccurrenceRef{MessageID: event}
	for _, note := range []int64{splitNote, linkedNote} {
		requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
		requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, ref))
	}
	var originalCreatedAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT created_at FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = '' AND target_message_id = ? AND occurrence_key = ''`, splitNote, event).Scan(&originalCreatedAt))

	requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: `{"calendar_id":"original-calendar@example.com","ical_uid":"acquired@example.org"}`, Valid: true}))
	var copiedDecision string
	var copiedCreatedAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT decision,created_at FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = ''`, splitNote, uid).Scan(&copiedDecision, &copiedCreatedAt))
	assertions.Equal("split", copiedDecision)
	assertions.True(originalCreatedAt.Equal(copiedCreatedAt), "the UID identity keeps the original split timestamp")

	_, copyID := calendarFixture(t, st, "copy", singleLinkRaw("copy", uid), "copy-calendar@example.com")
	assertions.Empty(linkedMeetingRefs(t, st, splitNote)[splitNote], "the split suppresses the new UID copy while the original remains")
	requirements.NoError(st.LinkMeetingContext(t.Context(), linkedNote, OccurrenceRef{MessageID: copyID}))
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, linkedNote)[linkedNote], "the newer UID link wins and resolves to the canonical copy")

	requirements.NoError(st.RemoveSource(data.Message.SourceID))
	assertions.Empty(linkedMeetingRefs(t, st, splitNote)[splitNote], "the split survives source removal")
	assertions.Equal(fmt.Sprintf("evt:%d", copyID), linkedMeetingRefs(t, st, linkedNote)[linkedNote], "the newer link still wins after source removal")
}

func TestMeetingLinksSplitSurvivesUIDAcquisitionAndSourceRemoval(t *testing.T) {
	for _, unlinkAfterRemoval := range []bool{false, true} {
		name := "unlink before occurrence removal"
		if unlinkAfterRemoval {
			name = "unlink after occurrence removal"
		}
		t.Run(name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			noteUID := "weekly@example.org"
			_, note := projectionFixture(t, st, "note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T10:00:00Z"}}`, noteUID))
			copyRaw := strings.Replace(linkSeriesRaw, `"id":"series"`, `"id":"copy"`, 1)
			_, copyID := calendarFixture(t, st, "copy", copyRaw, "copy-calendar@example.com")
			uidlessRaw := strings.ReplaceAll(linkSeriesRaw, noteUID, "")
			masterData, masterID := calendarFixture(t, st, "master", uidlessRaw, "master-calendar@example.com")
			ref := OccurrenceRef{MessageID: masterID, Key: "20270314T100000Z"}
			copyRef := fmt.Sprintf("evt:%d@%s", copyID, ref.Key)
			requirements.Equal(copyRef, linkedMeetingRefs(t, st, note)[note], "the UID copy is initially matched automatically")
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
			requirements.Equal(fmt.Sprintf("evt:%d@%s", masterID, ref.Key), linkedMeetingRefs(t, st, note)[note], "the explicit link replaces the automatic copy")

			requirements.NoError(st.SetMessageMetadata(masterID, sql.NullString{String: `{"calendar_id":"master-calendar@example.com","ical_uid":"weekly@example.org"}`, Valid: true}))
			requirements.NoError(st.UpsertMessageRawWithFormat(masterID, []byte(linkSeriesRaw), "gcal_json"))
			removedOccurrence := strings.Replace(linkSeriesRaw, "RRULE:FREQ=WEEKLY;COUNT=6", "RRULE:FREQ=WEEKLY;COUNT=1", 1)
			if unlinkAfterRemoval {
				requirements.NoError(st.UpsertMessageRawWithFormat(masterID, []byte(removedOccurrence), "gcal_json"))
				requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, ref))
			} else {
				requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, ref))
				requirements.NoError(st.UpsertMessageRawWithFormat(masterID, []byte(removedOccurrence), "gcal_json"))
			}
			var legacyDecision string
			requirements.NoError(st.db.QueryRow(`SELECT decision FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = '' AND target_message_id = ? AND occurrence_key = ?`, note, masterID, ref.Key).Scan(&legacyDecision))
			assertions.Equal("split", legacyDecision, "the old message-based link is superseded")
			assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the split suppresses the copy after the occurrence is removed")

			requirements.NoError(st.RemoveSource(masterData.Message.SourceID))
			assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the UID split survives source removal and continues to suppress the surviving copy")
		})
	}
}

func TestMeetingLinksSplitPersistsAliasWhenSeriesArrives(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	uid := "late-series@example.org"
	standaloneData, standalone := calendarFixture(t, st, "standalone", singleLinkRaw("standalone", uid), "standalone-calendar@example.com")
	_, note := projectionFixture(t, st, "note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T10:00:00Z"}}`, uid))
	requirements.Equal(fmt.Sprintf("evt:%d", standalone), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: standalone}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	var splitCreatedAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT created_at FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = '' AND decision = 'split'`, note, uid).Scan(&splitCreatedAt))

	seriesRaw := strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", uid)
	calendarFixture(t, st, "series", seriesRaw, "series-calendar@example.com")
	assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the new series alias is recognized while the standalone evidence remains")

	var aliasDecision string
	var aliasCreatedAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT decision,created_at FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = ?`, note, uid, "20270314T100000Z").Scan(&aliasDecision, &aliasCreatedAt))
	assertions.Equal("split", aliasDecision)
	assertions.True(splitCreatedAt.Equal(aliasCreatedAt), "the persisted alias keeps the original split timestamp")

	requirements.NoError(st.RemoveSource(standaloneData.Message.SourceID))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the saved split still suppresses the series after its standalone evidence is removed")
}

func TestMeetingLinksSplitPersistsKeyedAliasWhenStandaloneArrives(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	uid := "late-moved-single@example.org"
	seriesRaw := strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", uid)
	seriesData, master := calendarFixture(t, st, "series", seriesRaw, "series-calendar@example.com")
	calendarFixture(t, st, "exception", fmt.Sprintf(`{"id":"moved","iCalUID":%q,"recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, uid), "series-calendar@example.com")
	noteRaw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T12:00:00Z"}}`, uid)
	_, note := projectionFixture(t, st, "note", "twenty_json", noteRaw)
	key := "20270314T100000Z"

	requirements.Equal(fmt.Sprintf("evt:%d@%s", master, key), linkedMeetingRefs(t, st, note)[note], "the Twenty note initially matches the moved occurrence")
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: key}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	var splitCreatedAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT created_at FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = ? AND decision = 'split'`, note, uid, key).Scan(&splitCreatedAt))

	singleRaw := fmt.Sprintf(`{"id":"single","iCalUID":%q,"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, uid)
	calendarFixture(t, st, "single", singleRaw, "single-calendar@example.com")
	assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the split still suppresses the moved standalone alias")
	var aliasDecision string
	var aliasCreatedAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT decision,created_at FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = ''`, note, uid).Scan(&aliasDecision, &aliasCreatedAt))
	assertions.Equal("split", aliasDecision)
	assertions.True(splitCreatedAt.Equal(aliasCreatedAt), "the late standalone alias keeps the original split timestamp")

	requirements.NoError(st.RemoveSource(seriesData.Message.SourceID))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the persisted standalone alias survives series source removal")
}

func TestMeetingLinksSplitSurvivesStandaloneBecomingSeries(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	uid := "single-becomes-series@example.org"
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", uid), "event-calendar@example.com")
	noteRaw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T10:00:00Z"}}`, uid)
	_, note := projectionFixture(t, st, "note", "twenty_json", noteRaw)
	requirements.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	var splitCreatedAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT created_at FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = '' AND decision = 'split'`, note, uid).Scan(&splitCreatedAt))

	seriesRaw := strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", uid)
	requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(seriesRaw), "gcal_json"))
	var aliasCreatedAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT created_at FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = ? AND decision = 'split'`, note, uid, "20270314T100000Z").Scan(&aliasCreatedAt))
	assertions.True(splitCreatedAt.Equal(aliasCreatedAt), "the recurrence alias keeps the standalone split timestamp")
	assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the standalone split still suppresses the matching series occurrence")
}

func TestCalendarProjectionWriteKeepsEvidenceWhenSplitAliasExpansionIsTooBroad(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	uid := "broad-series@example.org"
	_, standalone := calendarFixture(t, st, "standalone", singleLinkRaw("standalone", uid), "standalone-calendar@example.com")
	noteRaw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T10:00:00Z"}}`, uid)
	_, note := projectionFixture(t, st, "note", "twenty_json", noteRaw)
	requirements.Equal(fmt.Sprintf("evt:%d", standalone), linkedMeetingRefs(t, st, note)[note], "the Twenty note initially matches the standalone event")
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: standalone}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])

	seriesRaw := strings.NewReplacer(
		"2027-03-07T10:00:00Z", "1800-01-01T10:00:00Z",
		"2027-03-07T11:00:00Z", "1800-01-01T11:00:00Z",
		"weekly@example.org", uid,
		"RRULE:FREQ=WEEKLY;COUNT=6", "RRULE:FREQ=DAILY;COUNT=100000",
	).Replace(linkSeriesRaw)
	_, series := calendarFixture(t, st, "broad-series", seriesRaw, "series-calendar@example.com")
	rows := calendarRows(t, st, series)
	requirements.Len(rows, 1, "the calendar projection is committed despite unresolved alias expansion")
	assertions.Equal("series", rows[0].Event.Kind)
	var splitCount int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = '' AND decision = 'split'`, note, uid).Scan(&splitCount))
	assertions.Equal(1, splitCount, "the existing owner decision remains stored")
}

func TestMeetingLinksSplitAliasPreservesNewerExplicitLink(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	uid := "late-series-link@example.org"
	standaloneData, standalone := calendarFixture(t, st, "standalone", singleLinkRaw("standalone", uid), "standalone-calendar@example.com")
	_, note := projectionFixture(t, st, "note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T10:00:00Z"}}`, uid))
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: standalone}))
	var splitCreatedAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT created_at FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = '' AND decision = 'split'`, note, uid).Scan(&splitCreatedAt))

	_, orphan := calendarFixture(t, st, "orphan", fmt.Sprintf(`{"id":"series_20270314T100000Z","iCalUID":%q,"recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, uid), "orphan-calendar@example.com")
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
	var linkCreatedAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT created_at FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = ? AND decision = 'link'`, note, uid, "20270314T100000Z").Scan(&linkCreatedAt))
	linkCreatedAt = splitCreatedAt.Add(time.Minute)
	_, err := st.db.Exec(`UPDATE meeting_link_decisions SET created_at = ? WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = ? AND decision = 'link'`, linkCreatedAt, note, uid, "20270314T100000Z")
	requirements.NoError(err)
	assertions.True(linkCreatedAt.After(splitCreatedAt), "the explicit link is newer than the standalone split")

	seriesRaw := strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", uid)
	calendarFixture(t, st, "series", seriesRaw, "series-calendar@example.com")
	var decision string
	var createdAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT decision,created_at FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND target_message_id IS NULL AND occurrence_key = ?`, note, uid, "20270314T100000Z").Scan(&decision, &createdAt))
	assertions.Equal("link", decision)
	assertions.True(linkCreatedAt.Equal(createdAt), "the older split must not replace a newer explicit link")

	requirements.NoError(st.RemoveSource(standaloneData.Message.SourceID))
	assertions.NotEmpty(linkedMeetingRefs(t, st, note)[note], "the newer explicit link remains active after the standalone evidence is removed")
}

func TestMeetingLinksCalendarUIDLookupPlan(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "event", singleLinkRaw("event", "shared@example.org"))
	// Metadata is the UID source even if the archived provider payload differs.
	requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"ical_uid":"metadata@example.org"}`, Valid: true}))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"metadata@example.org"}}`)
	var query string
	var args []any
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, q string, values []driver.NamedValue) error {
		if strings.Contains(q, "FROM calendar_events e") && strings.Contains(q, "ical_uid") && len(values) > 0 && values[0].Value == "metadata@example.org" {
			query = q
			args = nil
			for _, v := range values {
				args = append(args, v.Value)
			}
		}
		return nil
	}})
	assertions.Equal(fmt.Sprintf("evt:%d", id), linkedMeetingRefs(t, observed, note)[note])
	requirements.NotEmpty(query)

	explain := "EXPLAIN QUERY PLAN "
	if st.IsPostgreSQL() {
		explain = "EXPLAIN "
	}
	planQuery := st.db.QueryContext
	if st.IsPostgreSQL() {
		// The fixture has one calendar row, so PostgreSQL prefers a sequential
		// scan even though this predicate can use the partial index. Force the
		// planner to expose the indexable plan for this assertion.
		tx, err := st.db.BeginTx(t.Context(), nil)
		requirements.NoError(err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(t.Context(), `SET LOCAL enable_seqscan = off`)
		requirements.NoError(err)
		planQuery = tx.QueryContext
	}
	rows, err := planQuery(t.Context(), explain+query, args...)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	var plan []string
	for rows.Next() {
		var detail string
		if st.IsPostgreSQL() {
			requirements.NoError(rows.Scan(&detail))
		} else {
			var id, parent, unused int
			requirements.NoError(rows.Scan(&id, &parent, &unused, &detail))
		}
		plan = append(plan, detail)
	}
	requirements.NoError(rows.Err())
	assertions.Contains(strings.Join(plan, "\n"), "idx_messages_calendar_ical_uid")
}

func TestMeetingLinksWindowedExceptions(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := strings.ReplaceAll(strings.ReplaceAll(linkSeriesRaw, "2027-03-07", "2026-01-04"), "FREQ=WEEKLY;COUNT=6", "FREQ=DAILY;COUNT=1000")
	_, master := calendarFixture(t, st, "master", raw)
	for i := range 201 {
		at := time.Date(2026, 1, 4, 10, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		calendarFixture(t, st, fmt.Sprintf("old-%d", i), fmt.Sprintf(`{"id":"old-%d","iCalUID":"weekly@example.org","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":%q}}`, i, at.Format(time.RFC3339)))
	}
	_, twenty := projectionFixture(t, st, "twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	assertions.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), linkedMeetingRefs(t, st, twenty)[twenty])

	// An old exception moved into this window retains its original identity.
	calendarFixture(t, st, "old-1", `{"id":"old-1","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2026-01-05T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	raw = strings.Replace(raw, `"RRULE:FREQ=DAILY;COUNT=1000"`, `"RRULE:FREQ=DAILY;COUNT=1000","EXDATE:20270314T100000Z"`, 1)
	requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(raw), "gcal_json"))
	assertions.Equal(fmt.Sprintf("evt:%d@20260105T100000Z", master), linkedMeetingRefs(t, st, twenty)[twenty])
}

func TestMeetingLinksTwentyExactInstant(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:30Z"}}`)
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	requirements.NoError(err)
	requirements.Len(suggestions, 1)
	assertions.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), suggestions[0].Ref)
}

func TestMeetingLinksSplitSurvivesReingest(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	eventData, event := calendarFixture(t, st, "event", singleLinkRaw("event", "same@example.org"))
	noteData, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"event"}}`)
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	for _, tc := range []struct {
		data *MessagePersistData
		id   int64
	}{{noteData, note}, {eventData, event}} {
		id, err := st.PersistMessage(tc.data)
		requirements.NoError(err)
		assertions.Equal(tc.id, id)
	}
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
}

func singleLinkRaw(id, uid string) string {
	return fmt.Sprintf(`{"id":%q,"iCalUID":%q,"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, id, uid)
}
func granolaLinkFixture(t *testing.T, st *Store, event string) int64 {
	t.Helper()
	_, id := projectionFixture(t, st, "note", "granola_json", fmt.Sprintf(`{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":%q,"scheduled_start_time":"2027-03-14T10:00:00Z"}}`, event))
	return id
}

func TestMeetingLinksGranolaOwnCalendar(t *testing.T) {
	for _, tc := range []struct{ name, calendar, owner string }{
		{"exact email", "calendar@example.com", "calendar@example.com"},
		{"padded uppercase email", " Calendar@Example.COM ", " CALENDAR@example.com "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := singleLinkRaw("event", "own@example.org")
			_, other := calendarFixture(t, st, "other", raw, "other@example.com")
			_, note := projectionFixture(t, st, "note", "granola_json", fmt.Sprintf(`{"owner":{"email":%q},"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`, tc.owner))
			assertions.Empty(linkedMeetingRefs(t, st, note)[note])
			suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
			requirements.NoError(err)
			requirements.Len(suggestions, 1)
			assertions.Equal(fmt.Sprintf("evt:%d", other), suggestions[0].Ref)
			_, own := calendarFixture(t, st, "own", raw, tc.calendar)
			assertions.Equal(fmt.Sprintf("evt:%d", own), linkedMeetingRefs(t, st, note)[note])
		})
	}
}

func TestMeetingLinksIdentityHasSeparateFields(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, single := calendarFixture(t, st, "single", singleLinkRaw("single", "x@20270314T100000Z"))
	_, series := calendarFixture(t, st, "series", strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", "x"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"x@20270314T100000Z","startsAt":"2027-03-14T10:00:00Z"}}`)
	assertions.Equal(fmt.Sprintf("evt:%d", single), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: series, Key: "20270314T100000Z"}))
	assertions.Equal(fmt.Sprintf("evt:%d", single), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: single}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
}

func TestMeetingLinksGranolaOccurrences(t *testing.T) {
	for _, tc := range []struct {
		name, raw, exception, key string
		want                      bool
	}{
		{name: "generated", raw: linkSeriesRaw, want: true},
		{name: "exdate", raw: strings.Replace(linkSeriesRaw, `"RRULE:FREQ=WEEKLY;COUNT=6"`, `"RRULE:FREQ=WEEKLY;COUNT=6","EXDATE:20270314T100000Z"`, 1)},
		{name: "cancelled override", raw: strings.Replace(linkSeriesRaw, `"RRULE:FREQ=WEEKLY;COUNT=6"`, `"RRULE:FREQ=WEEKLY;COUNT=6","EXDATE:20270314T100000Z"`, 1), exception: `{"id":"series_20270314T100000Z","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`, want: true},
		{name: "moved", raw: linkSeriesRaw, exception: `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-04-01T12:00:00Z"},"end":{"dateTime":"2027-04-01T13:00:00Z"}}`, want: true},
		{name: "non UTC", raw: `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T05:00:00-05:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-07T06:00:00-05:00"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`},
		{name: "non UTC after DST", raw: `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T06:00:00-05:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-07T07:00:00-05:00"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`, want: true},
		{name: "all day", raw: `{"id":"series","iCalUID":"weekly@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`},
		{name: "all day instance", raw: `{"id":"series","iCalUID":"day@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`, key: "20270314", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "master", tc.raw)
			if tc.exception != "" {
				calendarFixture(t, st, "exception", tc.exception)
			}
			key := tc.key
			if key == "" {
				key = "20270314T100000Z"
			}
			note := granolaLinkFixture(t, st, "series_"+key)
			want := ""
			if tc.want {
				want = fmt.Sprintf("evt:%d@%s", master, key)
			}
			assert.Equal(t, want, linkedMeetingRefs(t, st, note)[note])
		})
	}
}

func TestMeetingLinksTwentyCopiesAndAmbiguity(t *testing.T) {
	for _, kind := range []string{"single copies", "series copies", "different occurrences"} {
		t.Run(kind, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			raw := linkSeriesRaw
			key := "@20270314T100000Z"
			if kind == "single copies" {
				raw = singleLinkRaw("one", "weekly@example.org")
				key = ""
			}
			_, one := calendarFixture(t, st, "one", raw)
			calendarFixture(t, st, "copy", raw, "copy@example.com")
			_, note := projectionFixture(t, st, "twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
			want := fmt.Sprintf("evt:%d%s", one, key)
			if kind == "different occurrences" {
				calendarFixture(t, st, "single", singleLinkRaw("single", "weekly@example.org"))
				want = ""
			}
			assert.Equal(t, want, linkedMeetingRefs(t, st, note)[note])
		})
	}
}

func TestMeetingLinksUserDecisions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	raw := singleLinkRaw("event", "same@example.org")
	_, event := calendarFixture(t, st, "one", raw)
	_, copyID := calendarFixture(t, st, "copy", raw, "copy@example.com")
	_, other := calendarFixture(t, st, "other", singleLinkRaw("other", "other@example.org"))
	note := granolaLinkFixture(t, st, "event")
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: other}))
	assertions.Equal(fmt.Sprintf("evt:%d", other), linkedMeetingRefs(t, st, note)[note])
	requirements.ErrorIs(st.LinkMeetingContext(t.Context(), event, OccurrenceRef{MessageID: other}), sql.ErrNoRows)
	requirements.ErrorIs(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: 999999}), sql.ErrNoRows)
}

func TestMeetingLinksOrphanDecisionAfterMasterArrives(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, automatic := calendarFixture(t, st, "automatic", singleLinkRaw("automatic", "automatic@example.org"))
	_, orphan := calendarFixture(t, st, "orphan", `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"automatic@example.org"}}`)
	requirements.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
	assertions.Equal(fmt.Sprintf("evt:%d", orphan), linkedMeetingRefs(t, st, note)[note])
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	assertions.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	assertions.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note])
}

func TestMeetingLinksActualRecurrenceBudget(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"series","iCalUID":"gaps@example.org","start":{"dateTime":"2027-03-07T02:30:00-08:00","timeZone":"America/Los_Angeles"},"end":{"dateTime":"2027-03-07T03:30:00-08:00"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=260"]}`)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	r := st.meetingReader(ctx)
	budget := calrecur.RequestReadBudget
	ref := OccurrenceRef{MessageID: master, Key: "20320404T093000Z"}
	o, err := resolveMeetingOccurrence(ctx, r, ref, &budget)
	requirements.NoError(err)
	assertions.Equal("20320404T093000Z", o.Ref.Key)
	spent := calrecur.RequestReadBudget - budget
	assertions.Greater(spent, 260)
	budget = spent
	_, err = resolveMeetingOccurrence(ctx, r, ref, &budget)
	requirements.NoError(err)
	assertions.Zero(budget)
	budget = spent - 1
	_, err = resolveMeetingOccurrence(ctx, r, ref, &budget)
	requirements.ErrorIs(err, ErrQueryTooBroad)
}

func TestMeetingLinksProjectionEnvelopeAndRepair(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "valid@example.org"))
	data, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":2,"recording":{},"calendar_event":{"iCalUid":"valid@example.org"}}`)
	requirements.NoError(st.SetMessageMetadata(note, sql.NullString{String: `{"platform":"twenty","calendar_ical_uid":"valid@example.org"}`, Valid: true}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	valid := `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"valid@example.org"}}`
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(valid), "twenty_json"))
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	// Only the shared ID changes; the content remains identical.
	before, hash := readProjection(t, st, note)
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(strings.ReplaceAll(valid, "valid@example.org", "changed@example.org")), "twenty_json"))
	after, newHash := readProjection(t, st, note)
	assertions.Equal(before, after)
	assertions.NotEqual(hash, newHash)
	assertions.Empty(linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(`{broken`), "twenty_json"))
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_key WHERE message_id = ?`, note).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(valid), "twenty_json"))
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	data.Message.MessageType = "email"
	_, err := st.UpsertMessage(data.Message)
	requirements.NoError(err)
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_key WHERE message_id = ?`, note).Scan(&count))
	assertions.Zero(count)
}

func TestMeetingLinksUpgradeFromSchema11(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "upgraded@example.org"))
	note := granolaLinkFixture(t, st, "event")
	_, err := st.db.Exec(`DROP TABLE meeting_link_decisions; DROP TABLE meeting_link_key; DROP INDEX idx_messages_calendar_ical_uid`)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE meeting_details SET projection_version = 1; UPDATE archive_metadata SET value = '11' WHERE key = 'schema_version'`)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE applied_migrations SET version = 1 WHERE name = ?`, migrationMeetingProjectionV1)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.InitSchemaContext(t.Context()))
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
}

func TestMeetingLinksIgnoreUnresolvableDecision(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(`{"id":"series","recurrence":["RRULE:FREQ=WEEKLY"]}`), "gcal_json"))
	assert.Empty(t, linkedMeetingRefs(t, st, note)[note])
}

func TestMeetingLinksReadDecisions(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	note := granolaLinkFixture(t, st, "event")
	links, err := st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	assertions.Empty(links)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "linked@example.org"))
	for step := range 3 {
		if step == 1 {
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
		}
		if step == 2 {
			requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
		}
		links, err = st.MeetingLinksContext(t.Context(), note)
		requirements.NoError(err)
		if step == 2 {
			assertions.Empty(links)
			continue
		}
		requirements.Len(links, 1)
		assertions.Equal(fmt.Sprintf("evt:%d", event), links[0].Ref)
		assertions.Equal("Example event", links[0].Subject)
		assertions.Equal("2027-03-14T10:00:00Z", links[0].Start.Format(time.RFC3339))
		assertions.Equal("2027-03-14T11:00:00Z", links[0].End.Format(time.RFC3339))
	}
	_, err = st.MeetingLinksContext(t.Context(), event)
	assertions.ErrorIs(err, sql.ErrNoRows)
}

// Read the same references returned to CLI and API users.
func linkedMeetingRefs(t *testing.T, st *Store, ids ...int64) map[int64]string {
	t.Helper()
	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		links, err := st.MeetingLinksContext(t.Context(), id)
		require.NoError(t, err)
		var refs []string
		for _, link := range links {
			refs = append(refs, link.Ref)
		}
		out[id] = strings.Join(refs, ",")
	}
	return out
}

func TestMeetingLinksPrimaryCalendar(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "event", singleLinkRaw("event", "primary@example.org"))
	requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"calendar_id":"primary","account_email":"Calendar@Example.COM","primary":true,"time_zone":"UTC"}`))
	note := granolaLinkFixture(t, st, "event")
	links, err := st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), links[0].Ref)
}

func TestMeetingLinksTwoOccurrenceAliases(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, exception := calendarFixture(t, st, "exception", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	ref := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
	links, err := st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), links[0].Ref)
	assertions.Equal("2027-03-14T12:00:00Z", links[0].Start.Format(time.RFC3339))

	requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
	links, err = st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), links[0].Ref)
	assertions.Equal("2027-03-14T12:00:00Z", links[0].Start.Format(time.RFC3339))
}

func TestMeetingLinksSplitStandaloneSupersedesSeriesOccurrence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	uid := "shared@example.org"
	_, master := calendarFixture(t, st, "master", strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", uid))
	noteRaw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T10:00:00Z"}}`, uid)
	_, note := projectionFixture(t, st, "note", "twenty_json", noteRaw)
	key := "20270314T100000Z"
	requirements.Equal(fmt.Sprintf("evt:%d@%s", master, key), linkedMeetingRefs(t, st, note)[note], "the Twenty note initially matches the series occurrence")
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: key}))
	requirements.Equal(fmt.Sprintf("evt:%d@%s", master, key), linkedMeetingRefs(t, st, note)[note])
	singleData, single := calendarFixture(t, st, "single", singleLinkRaw("single", uid), "copy-calendar@example.com")

	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: single}))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note], "a standalone split supersedes the same UID occurrence in its series")
	var aliasDecision string
	requirements.NoError(st.db.QueryRow(`SELECT decision FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND COALESCE(target_message_id, 0) = 0 AND occurrence_key = ?`, note, uid, key).Scan(&aliasDecision))
	assertions.Equal("split", aliasDecision, "the series identity is saved alongside the standalone split")
	requirements.NoError(st.RemoveSource(singleData.Message.SourceID))
	assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the split remains after its standalone alias is removed")
}

func TestMeetingLinksSplitAliasesMovedSeriesOccurrenceInBothDirections(t *testing.T) {
	for _, unlinkViaSeries := range []bool{false, true} {
		name := "standalone split supersedes a moved series link"
		if unlinkViaSeries {
			name = "series split supersedes a moved standalone link"
		}
		t.Run(name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			uid := "moved-copy@example.org"
			seriesRaw := strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", uid)
			seriesData, master := calendarFixture(t, st, "master", seriesRaw, "series-calendar@example.com")
			calendarFixture(t, st, "exception", fmt.Sprintf(`{"id":"moved","iCalUID":%q,"recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, uid), "series-calendar@example.com")
			noteRaw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T12:00:00Z"}}`, uid)
			_, note := projectionFixture(t, st, "note", "twenty_json", noteRaw)
			seriesRef := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
			requirements.Equal(fmt.Sprintf("evt:%d@%s", master, seriesRef.Key), linkedMeetingRefs(t, st, note)[note], "the Twenty note initially matches the moved series occurrence")
			singleData, single := calendarFixture(t, st, "single", fmt.Sprintf(`{"id":"single","iCalUID":%q,"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, uid), "copy-calendar@example.com")
			singleRef := OccurrenceRef{MessageID: single}
			linkRef, unlinkRef, aliasKey := seriesRef, singleRef, seriesRef.Key
			removeSource := singleData.Message.SourceID
			if unlinkViaSeries {
				linkRef, unlinkRef, aliasKey = singleRef, seriesRef, ""
				removeSource = seriesData.Message.SourceID
			}
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, linkRef))
			requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, unlinkRef))
			assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the split hides the linked copy at the moved occurrence's effective bounds")
			var aliasDecision string
			requirements.NoError(st.db.QueryRow(`SELECT decision FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND COALESCE(target_message_id, 0) = 0 AND occurrence_key = ?`, note, uid, aliasKey).Scan(&aliasDecision))
			assertions.Equal("split", aliasDecision, "the equivalent identity is persisted")
			requirements.NoError(st.RemoveSource(removeSource))
			assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the split survives source removal of the identity used for matching")
		})
	}
}

func TestMeetingLinksAllDayExceptionUsesInheritedZone(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	uid := "all-day-zone@example.org"
	masterRaw := fmt.Sprintf(`{"id":"series","iCalUID":%q,"start":{"date":"2027-03-01","timeZone":"America/Los_Angeles"},"end":{"date":"2027-03-02"},"recurrence":["RRULE:FREQ=DAILY;COUNT=1"]}`, uid)
	_, master := calendarFixture(t, st, "master", masterRaw, "series-calendar@example.com")
	_, exception := calendarFixture(t, st, "exception", fmt.Sprintf(`{"id":"moved","iCalUID":%q,"recurringEventId":"series","originalStartTime":{"date":"2027-03-01"},"start":{"date":"2027-03-10"},"end":{"date":"2027-03-11"}}`, uid), "series-calendar@example.com")
	_, expectedNote := projectionFixture(t, st, "expected-zone-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-10T08:30:00Z"}}`)
	_, sourceZoneNote := projectionFixture(t, st, "source-zone-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-10T00:30:00Z"}}`)

	from := time.Date(2027, 3, 10, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	listed := page.Occurrences[0]
	assertions.Equal(fmt.Sprintf("evt:%d@20270301", master), listed.Ref)
	assertions.Equal("2027-03-10T08:00:00Z", listed.Start.UTC().Format(time.RFC3339))
	assertions.Equal("2027-03-11T08:00:00Z", listed.End.UTC().Format(time.RFC3339))

	requirements.NoError(st.LinkMeetingContext(t.Context(), expectedNote, OccurrenceRef{MessageID: exception}))
	links, err := st.MeetingLinksContext(t.Context(), expectedNote)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(listed.Ref, links[0].Ref)
	assertions.True(listed.Start.Equal(links[0].Start), "meeting links and calendar listings use the same inherited-zone start")
	assertions.True(listed.End.Equal(links[0].End), "meeting links and calendar listings use the same inherited-zone end")

	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{MessageID: master, Key: "20270301", Kind: "evt"}, 10)
	requirements.NoError(err)
	requirements.Len(suggestions, 1)
	assertions.Equal(expectedNote, suggestions[0].MemberMessageID)
	assertions.Equal(30, suggestions[0].OverlapMinutes)
	reverse, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{MessageID: expectedNote, Kind: "msg"}, 10)
	requirements.NoError(err)
	requirements.Len(reverse, 1)
	assertions.Equal(listed.Ref, reverse[0].Ref)
	assertions.Equal(30, reverse[0].OverlapMinutes)
	wrongZone, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{MessageID: sourceZoneNote, Kind: "msg"}, 10)
	requirements.NoError(err)
	assertions.Empty(wrongZone, "a transcript at the calendar source zone's midnight does not overlap the inherited-zone exception")
}

func TestMeetingLinksOrphanSplitAfterMasterArrives(t *testing.T) {
	for _, uid := range []string{"", "orphan@example.org"} {
		t.Run(uid, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, orphan := calendarFixture(t, st, "orphan", fmt.Sprintf(`{"id":"series_20270314T100000Z","iCalUID":%q,"recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, uid))
			note := granolaLinkFixture(t, st, "series_20270314T100000Z")
			requirements.Equal(fmt.Sprintf("evt:%d", orphan), linkedMeetingRefs(t, st, note)[note])
			requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
			// The saved split predates the master and therefore uses the orphan's identity.
			decisions, err := readMeetingLinkDecisions(t.Context(), st.meetingReader(t.Context()), note)
			requirements.NoError(err)
			requirements.Len(decisions, 1)
			assertions.Equal(uid, decisions[0].Identity.ICalUID)
			assertions.Equal("20270314T100000Z", decisions[0].Identity.OccurrenceKey)
			_, master := calendarFixture(t, st, "master", strings.Replace(linkSeriesRaw, "RRULE:FREQ=WEEKLY;COUNT=6", "RRULE:FREQ=HOURLY", 1))
			_, automatic := projectionFixture(t, st, "automatic", "plaud_json", `{}`)
			requirements.NoError(st.LinkMeetingContext(t.Context(), automatic, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
			requirements.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), linkedMeetingRefs(t, st, automatic)[automatic])
			assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the orphan split suppresses the master's canonical identity without expanding its unsupported recurrence")
		})
	}
}

func TestMeetingLinksOrphanExceptionSplitSupersedesSavedLink(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, orphan := calendarFixture(t, st, "orphan", `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
	requirements.Equal(fmt.Sprintf("evt:%d", orphan), linkedMeetingRefs(t, st, note)[note])

	_, automatic := calendarFixture(t, st, "automatic", singleLinkRaw("automatic", "weekly@example.org"))
	uidlessMaster := strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", "")
	_, master := calendarFixture(t, st, "master", uidlessMaster)
	key := "20270314T100000Z"
	requirements.Equal(fmt.Sprintf("evt:%d@%s", master, key), linkedMeetingRefs(t, st, note)[note], "the UID-less exception resolves through its arriving master")
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: key}))
	assertions.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note], "unlinking the canonical occurrence restores the automatic copy")
	var oldDecision string
	requirements.NoError(st.db.QueryRow(`SELECT decision FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = '' AND target_message_id = ? AND occurrence_key = ?`, note, orphan, key).Scan(&oldDecision))
	assertions.Equal("split", oldDecision, "unlinking must supersede the orphan's saved message-based link")

	// When the master stops recurring, the exception becomes an orphan again.
	// The old message-based link must already have been superseded by the split.
	requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(singleLinkRaw("series", "")), "gcal_json"))
	assertions.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note], "removing the master recurrence must not reactivate the orphan's saved link")
}

func TestMeetingLinksSplitSurvivesSourceRemoval(t *testing.T) {
	for _, recurring := range []bool{false, true} {
		t.Run(fmt.Sprintf("recurring=%t", recurring), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw, key := singleLinkRaw("event", "weekly@example.org"), ""
			if recurring {
				raw, key = linkSeriesRaw, "20270314T100000Z"
			}
			data, a := calendarFixture(t, st, "a", raw, "a@example.com")
			_, b := calendarFixture(t, st, "b", raw, "b@example.com")
			_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
			requirements.NotEmpty(linkedMeetingRefs(t, st, note)[note])
			requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: a, Key: key}))
			requirements.Empty(linkedMeetingRefs(t, st, note)[note])
			requirements.NoError(st.RemoveSource(data.Message.SourceID))
			assertions.Empty(linkedMeetingRefs(t, st, note)[note], "the surviving calendar must retain the split")
			calendarFixture(t, st, "a", raw, "a@example.com")
			assertions.Empty(linkedMeetingRefs(t, st, note)[note], "reimport must retain the split")
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: b, Key: key}))
			assertions.NotEmpty(linkedMeetingRefs(t, st, note)[note], "a new owner decision can undo the split")
		})
	}
}

func TestMeetingLinksManualOverrideSurvivesSourceRemoval(t *testing.T) {
	for _, recurring := range []bool{false, true} {
		t.Run(fmt.Sprintf("recurring=%t", recurring), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, automatic := calendarFixture(t, st, "automatic", singleLinkRaw("automatic", "automatic@example.org"))
			_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"automatic@example.org"}}`)
			raw, key := singleLinkRaw("manual", "manual@example.org"), ""
			if recurring {
				raw, key = linkSeriesRaw, "20270314T100000Z"
			}
			data, manual := calendarFixture(t, st, "manual", raw, "manual@example.com")
			requirements.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note])
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: manual, Key: key}))
			suffix := ""
			if recurring {
				suffix = "@20270314T100000Z"
			}
			want := fmt.Sprintf("evt:%d%s", manual, suffix)
			requirements.Equal(want, linkedMeetingRefs(t, st, note)[note])
			requirements.NoError(st.RemoveSource(data.Message.SourceID))
			assertions.Empty(linkedMeetingRefs(t, st, note)[note], "an absent manual target must still override the available automatic match")

			data, replacement := calendarFixture(t, st, "manual", raw, "manual@example.com")
			want = fmt.Sprintf("evt:%d%s", replacement, suffix)
			assertions.Equal(want, linkedMeetingRefs(t, st, note)[note], "reimport must resolve the saved UID decision")
			requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: replacement, Key: key}))
			assertions.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note], "a later split must allow automatic matching again")
			requirements.NoError(st.RemoveSource(data.Message.SourceID))
			assertions.Equal(fmt.Sprintf("evt:%d", automatic), linkedMeetingRefs(t, st, note)[note], "an absent split target must keep the earlier link superseded")
		})
	}
}

func TestMeetingLinksDecisionLifetime(t *testing.T) {
	for _, uid := range []string{"lifetime@example.org", ""} {
		t.Run("uid="+uid, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			data, event := calendarFixture(t, st, "event", singleLinkRaw("event", uid))
			noteData, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			requirements.NoError(st.RemoveSource(data.Message.SourceID))
			links, err := st.MeetingLinksContext(t.Context(), note)
			requirements.NoError(err)
			assertions.Empty(links)
			var count int
			requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&count))
			if uid == "" {
				assertions.Zero(count, "a UID-less target retains its message lifetime")
			} else {
				assertions.Equal(1, count, "a UID target survives loss of every copy")
			}
			_, replacement := calendarFixture(t, st, "event", singleLinkRaw("event", uid))
			links, err = st.MeetingLinksContext(t.Context(), note)
			requirements.NoError(err)
			if uid == "" {
				assertions.Empty(links)
			} else {
				requirements.Len(links, 1)
				assertions.Equal(fmt.Sprintf("evt:%d", replacement), links[0].Ref)
			}
			requirements.NoError(st.RemoveSource(noteData.Message.SourceID))
			requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_link_decisions`).Scan(&count))
			assertions.Zero(count, "decisions follow the member note's lifetime")
		})
	}
}
