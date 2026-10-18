package query

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingCountsAcrossFilters(t *testing.T) {
	b := NewTestDataBuilder(t)
	firstSource := b.AddSourceWithType("first@example.org", "granola")
	secondSource := b.AddSourceWithType("second@example.org", "twenty")
	person := b.AddParticipant("person@example.org", "example.org", "Example Person")
	for i, source := range []int64{firstSource, secondSource, firstSource} {
		id := b.AddMessage(MessageOpt{SourceID: source, ConversationID: int64(100 + i), MessageType: "meeting_transcript", SentAt: time.Date(2026, 8, 1, 10+i, 0, 0, 0, time.UTC)})
		b.AddTo(id, person, "Example Person")
		if i < 2 {
			b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, 'ical:shared@example.org|')", id))
		}
	}
	engine := b.BuildEngine()
	for _, tc := range []struct {
		name    string
		context Context
		want    int64
	}{
		{"all", Context{}, 2},
		{"one source", Context{SourceIDs: []int64{firstSource}}, 2},
		{"both sources", Context{SourceIDs: []int64{firstSource, secondSource}}, 2},
		{"explore both sources", Context{SourceIDs: []int64{firstSource, secondSource}, ParticipantIDs: []int64{person}}, 2},
		{"explore one source", Context{SourceIDs: []int64{secondSource}, ParticipantIDs: []int64{person}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			result, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Explore: ExploreRequest{Context: tc.context}})
			requirements.NoError(err)
			requirements.Len(result.Rows, 1)
			assertions.Equal(tc.want, result.Rows[0].MeetingCount)
			detail, err := engine.GetPerson(t.Context(), person, tc.context, nil)
			requirements.NoError(err)
			requirements.NotNil(detail)
			assertions.Equal(tc.want, detail.MeetingCount)
		})
	}
}
