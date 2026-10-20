package query

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
)

func TestRoomEdgesAbsentFromPeopleLists(t *testing.T) {
	require := require.New(t)
	st, err := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchemaContext(t.Context()))
	mock := gcal.NewMockAPI()
	mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner", TimeZone: "UTC"}}
	mock.FullEvents["primary"] = [][]gcal.Event{{{
		ID: "planning", Summary: "Planning",
		Start:     gcal.EventDateTime{DateTime: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)},
		End:       gcal.EventDateTime{DateTime: time.Date(2027, 3, 14, 11, 0, 0, 0, time.UTC)},
		Organizer: gcal.Person{Email: "room@c.resource.calendar.google.com"},
		Attendees: []gcal.Attendee{{Email: "resource@example.org", Resource: true}, {Email: "human@example.org", DisplayName: "Human"}},
	}}}
	_, err = calsync.New(mock, st, calsync.Options{AccountEmail: "owner@example.org"}).Full(t.Context())
	require.NoError(err)
	// Feed the persisted recipient edges into the same Parquet fixtures used by
	// people queries, so a room edge created by the importer reaches the list.
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("owner@example.org", "gcal")
	message := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 1, MessageType: "calendar_event", SentAt: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)})
	rows, err := st.DB().Query(`SELECT p.email_address, coalesce(p.display_name, ''), r.recipient_type FROM message_recipients r JOIN participants p ON p.id = r.participant_id`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	for rows.Next() {
		var email, name, role string
		require.NoError(rows.Scan(&email, &name, &role))
		id := b.AddParticipant(email, "", name)
		if role == "from" {
			b.AddFrom(message, id, name)
		} else {
			b.AddTo(message, id, name)
		}
	}
	require.NoError(rows.Err())
	require.NoError(rows.Close())
	result, err := b.BuildEngine().SearchPeople(t.Context(), PersonSearchRequest{})
	require.NoError(err)
	require.Len(result.Rows, 1)
	assert.Equal(t, "Human", result.Rows[0].DisplayName)
}
