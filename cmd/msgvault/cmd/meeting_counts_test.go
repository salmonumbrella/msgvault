package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
