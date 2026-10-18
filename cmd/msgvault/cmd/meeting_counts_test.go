package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestMeetingCountsLinkedCopies(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.OpenForTest(path)
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	human, err := st.EnsureParticipant("human@example.org", "Example Human", "example.org")
	requirements.NoError(err)
	persist := func(kind, identifier, format, raw string) int64 {
		source, err := st.GetOrCreateSource(kind, identifier+"@example.org")
		requirements.NoError(err)
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: identifier, MessageType: kind, SentAt: sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: identifier, ConversationType: "meeting", Title: "Planning"},
			RawMIME:      []byte(raw), RawFormat: format,
		})
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{human}, []string{"Example Human"}))
		return id
	}
	persist("meeting_transcript", "first", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"planning@example.org"}}`)
	second := persist("meeting_transcript", "second", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"planning@example.org"}}`)
	cache := filepath.Join(t.TempDir(), "analytics")
	readCount := func(want int64) {
		engine, err := query.NewDuckDBEngine(cache, "", nil)
		requirements.NoError(err)
		defer func() { requirements.NoError(engine.Close()) }()
		people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{})
		requirements.NoError(err)
		requirements.Len(people.Rows, 1)
		assertions.Equal(want, people.Rows[0].MeetingCount)
		person, err := engine.GetPerson(t.Context(), human, query.Context{}, nil)
		requirements.NoError(err)
		requirements.NotNil(person)
		assertions.Equal(want, person.MeetingCount)
	}
	_, err = buildCache(path, cache, true)
	requirements.NoError(err)
	readCount(2)
	event := persist("calendar_event", "calendar", "gcal_json", `{"id":"event","iCalUID":"planning@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: `{"ical_uid":"planning@example.org"}`, Valid: true}))
	assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
	incremental, err := buildCache(path, cache, false)
	requirements.NoError(err)
	assertions.Equal(int64(1), incremental.StagedCount)
	readCount(1)
	previousRefresh := runDerivedCacheSubprocess
	runDerivedCacheSubprocess = func(_ context.Context, dir string) error {
		_, err := buildCacheDerivedOnly(path, dir)
		return err
	}
	t.Cleanup(func() { runDerivedCacheSubprocess = previousRefresh })
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: &storeAPIAdapter{store: st, analyticsDir: cache}, Logger: slog.New(slog.DiscardHandler),
	})
	decide := func(decision string) {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(fmt.Sprintf(`{"message_id":%d,"event":"evt:%d","decision":%q}`, second, event, decision)))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		requirements.Equal(http.StatusOK, response.Code, response.Body.String())
		assertions.False(cacheNeedsBuild(path, cache).NeedsBuild)
	}
	decide("unlink")
	readCount(2)
	decide("link")
	readCount(1)
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), second, store.OccurrenceRef{MessageID: event}))
	assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
	result, err := buildCache(path, cache, false)
	requirements.NoError(err)
	assertions.True(result.IdentityOnly)
	readCount(2)
	requirements.NoError(st.LinkMeetingContext(t.Context(), second, store.OccurrenceRef{MessageID: event}))
	_, err = buildCache(path, cache, false)
	requirements.NoError(err)
	readCount(1)
	_, err = buildCache(path, cache, true)
	requirements.NoError(err)
	readCount(1)
}

func TestMeetingSyncsStayIncremental(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	root := t.TempDir()
	path, cache := filepath.Join(root, "test.db"), filepath.Join(root, "analytics")
	st, err := store.OpenForTest(path)
	requirements.NoError(err)
	defer func() { requirements.NoError(st.Close()) }()
	requirements.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "baseline@example.org")
	requirements.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "baseline", MessageType: "email"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "baseline", ConversationType: "email"},
	})
	requirements.NoError(err)
	_, err = buildCache(path, cache, true)
	requirements.NoError(err)
	checks := []struct {
		name  string
		write func()
	}{
		{"phone participant", func() {
			id, err := st.EnsureParticipant("new@example.org", "New Person", "example.org")
			requirements.NoError(err)
			requirements.NoError(st.SetParticipantIdentifier(id, "phone", "+15550100001"))
		}},
		{"calendar event", func() {
			source, err := st.GetOrCreateSource("gcal", "calendar@example.org")
			requirements.NoError(err)
			_, err = st.PersistMessage(&store.MessagePersistData{
				Message:      &store.Message{SourceID: source.ID, SourceMessageID: "event", MessageType: "calendar_event"},
				Conversation: &store.ConversationPersistData{SourceConversationID: "event", ConversationType: "calendar"},
				RawMIME:      []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), RawFormat: "gcal_json",
			})
			requirements.NoError(err)
		}},
		{"transcript import", func() {
			source, err := st.GetOrCreateSource("meeting_import", "meeting@example.org")
			requirements.NoError(err)
			_, err = st.PersistMessage(&store.MessagePersistData{
				Message:      &store.Message{SourceID: source.ID, SourceMessageID: "note", MessageType: "meeting_transcript"},
				Conversation: &store.ConversationPersistData{SourceConversationID: "note", ConversationType: "meeting"},
				RawMIME:      []byte(`{"summary_text":"Planning"}`), RawFormat: "meeting_json",
			})
			requirements.NoError(err)
		}},
	}
	for _, tc := range checks {
		assertions.False(cacheNeedsBuild(path, cache).NeedsBuild, tc.name)
		tc.write()
		assertions.False(cacheNeedsBuild(path, cache).FullRebuild, tc.name)
		_, err := buildCache(path, cache, false)
		requirements.NoError(err)
	}
}

func TestMeetingKeysSharePublicationSnapshot(t *testing.T) {
	for _, csv := range []bool{false, true} {
		for _, derived := range []bool{false, true} {
			t.Run(fmt.Sprintf("csv=%t/derived=%t", csv, derived), func(t *testing.T) {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "")
				if csv {
					t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
				}
				require := require.New(t)
				root := t.TempDir()
				path, cache := filepath.Join(root, "archive.db"), filepath.Join(root, "analytics")
				st, err := store.OpenForTest(path)
				require.NoError(err)
				defer func() { require.NoError(st.Close()) }()
				require.NoError(st.InitSchema())
				source, err := st.GetOrCreateSource("twenty", "owner@example.org")
				require.NoError(err)
				human, err := st.EnsureParticipant("person@example.org", "Person", "example.org")
				require.NoError(err)
				persist := func(id, kind, format, raw string) int64 {
					messageID, err := st.PersistMessage(&store.MessagePersistData{
						Message:      &store.Message{SourceID: source.ID, SourceMessageID: id, MessageType: kind, SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
						Conversation: &store.ConversationPersistData{SourceConversationID: id, ConversationType: "meeting"},
						RawFormat:    format, RawMIME: []byte(raw),
					})
					require.NoError(err)
					require.NoError(st.ReplaceMessageRecipients(messageID, "to", []int64{human}, []string{"Person"}))
					return messageID
				}
				event := persist("event", "calendar_event", "gcal_json", `{"id":"event","iCalUID":"shared@example.org","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"}}`)
				require.NoError(st.SetMessageMetadata(event, sql.NullString{String: `{"ical_uid":"shared@example.org"}`, Valid: true}))
				persist("first", "meeting_transcript", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org","startsAt":"2026-08-01T10:00:00Z"}}`)
				second := persist("second", "meeting_transcript", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org","startsAt":"2026-08-01T10:00:00Z"}}`)
				_, err = buildCache(path, cache, true)
				require.NoError(err)
				require.NoError(st.UnlinkMeetingContext(t.Context(), second, store.OccurrenceRef{MessageID: event}))
				hook := func() {
					require.NoError(st.LinkMeetingContext(t.Context(), second, store.OccurrenceRef{MessageID: event}))
				}
				if derived {
					derivedRefreshBeforeSnapshotHook = hook
				} else {
					buildCacheBeforeSnapshotHook = hook
				}
				t.Cleanup(func() { derivedRefreshBeforeSnapshotHook = nil; buildCacheBeforeSnapshotHook = nil })
				_, err = buildCache(path, cache, !derived)
				require.NoError(err)
				engine, err := query.NewDuckDBEngine(cache, "", nil)
				require.NoError(err)
				defer func() { require.NoError(engine.Close()) }()
				people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{})
				require.NoError(err)
				require.Len(people.Rows, 1)
				assert.Equal(t, int64(1), people.Rows[0].MeetingCount)
				assert.False(t, cacheNeedsBuild(path, cache).NeedsBuild)
			})
		}
	}
}

func TestMeetingKeyResolutionSkipsAndUsesCallerContext(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	path, cache := filepath.Join(root, "archive.db"), filepath.Join(root, "analytics")
	st, err := store.OpenForTest(path)
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("twenty", "owner@example.org")
	require.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "note", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "note", ConversationType: "meeting"},
	})
	require.NoError(err)
	_, err = buildCache(path, cache, true)
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := buildCacheLockedAttempt(ctx, path, cache, false, false, acquirePublishLock)
	require.NoError(err)
	assert.True(t, result.Skipped)
	_, err = buildCacheLockedAttempt(ctx, path, cache, true, false, acquirePublishLock)
	require.ErrorIs(err, context.Canceled)
	require.ErrorContains(err, "read meeting count keys")
}
