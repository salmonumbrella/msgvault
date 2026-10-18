package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

type meetingLinksTestStore struct {
	*mockStore

	member        int64
	event         store.OccurrenceRef
	decision, ref string
	err           error
	calls         int
}

func (s *meetingLinksTestStore) LinkMeetingContext(_ context.Context, id int64, ref store.OccurrenceRef) error {
	s.calls++
	s.member, s.event, s.decision = id, ref, "link"
	return s.err
}
func (s *meetingLinksTestStore) UnlinkMeetingContext(_ context.Context, id int64, ref store.OccurrenceRef) error {
	s.calls++
	s.member, s.event, s.decision = id, ref, "unlink"
	return s.err
}
func (s *meetingLinksTestStore) SuggestMeetingLinksContext(_ context.Context, ref string, _ int) ([]store.MeetingSuggestion, error) {
	s.calls++
	s.ref = ref
	return []store.MeetingSuggestion{{Ref: "evt:81@20270314T100000Z", MemberMessageID: 42, OverlapMinutes: 30, SharedAttendees: 1, TitleTokens: 2, Reasons: []string{"time_overlap", "shared_attendees", "title_tokens"}}}, s.err
}
func TestMeetingLinkRoutes(t *testing.T) {
	requirements := require.New(t)

	backend := &meetingLinksTestStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true, RemoteClients: []config.RemoteClientConfig{{ClientID: "reader", APIKey: "synthetic-reader-key"}}}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, secret, _, err := srv.agentGrants.Issue("calendar", []agentgrant.Permission{agentgrant.PermissionCalendarWrite}, []agentgrant.SourceRef{{ID: 1, Type: "gcal", Identifier: "calendar@example.com"}}, time.Time{})
	requirements.NoError(err)
	for _, tc := range []struct {
		name, method, path, body, token string
		status                          int
		err                             error
	}{
		{"link", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81@20270314T100000Z","decision":"link"}`, "", 200, nil},
		{"unlink", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81@20270314T100000Z","decision":"unlink"}`, "", 200, nil},
		{"note suggestions", "GET", "/meetings/suggestions?ref=msg:42", "", "", 200, nil},
		{"event suggestions", "GET", "/meetings/suggestions?ref=evt:81@20270314T100000Z", "", "", 200, nil},
		{"invalid ref", "GET", "/meetings/suggestions?ref=msg:42@20270314", "", "", 400, nil},
		{"invalid decision", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81","decision":"split"}`, "", 400, nil},
		{"invalid key", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81@20270230","decision":"link"}`, "", 400, nil},
		{"missing", "GET", "/meetings/suggestions?ref=msg:42", "", "", 404, sql.ErrNoRows},
		{"broad", "GET", "/meetings/suggestions?ref=msg:42", "", "", 400, store.ErrQueryTooBroad},
		{"delegated read", "GET", "/meetings/suggestions?ref=msg:42", "", secret, 401, nil},
		{"remote read", "GET", "/meetings/suggestions?ref=msg:42", "", "synthetic-reader-key", 403, nil},
		{"delegated write", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81","decision":"link"}`, secret, 401, nil},
		{"remote write", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81","decision":"link"}`, "synthetic-reader-key", 403, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			backend.err = tc.err
			before := backend.calls
			r := httptest.NewRequest(tc.method, "/api/v1"+tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			if tc.token == secret {
				r.Header.Set(apiprotocol.AgentTokenHeader, secret)
			} else if tc.token != "" {
				r.Header.Set("X-Api-Key", tc.token)
			} else {
				r.Header.Set("X-Api-Key", "synthetic-owner-key")
			}
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			requirements.Equal(tc.status, w.Code, w.Body.String())
			if tc.status >= 400 && tc.err == nil {
				assertions.Equal(before, backend.calls)
			}
			if tc.status != 200 {
				return
			}
			if tc.method == http.MethodPost {
				assertions.Equal(int64(42), backend.member)
				assertions.Equal(store.OccurrenceRef{MessageID: 81, Key: "20270314T100000Z"}, backend.event)
				assertions.Equal(tc.name, backend.decision)
				assertions.JSONEq(tc.body, w.Body.String())
			} else {
				assertions.Equal(strings.TrimPrefix(tc.path, "/meetings/suggestions?ref="), backend.ref)
				assertions.JSONEq(`{"suggestions":[{"ref":"evt:81@20270314T100000Z","member_message_id":42,"overlap_minutes":30,"shared_attendees":1,"title_tokens":2,"reasons":["time_overlap","shared_attendees","title_tokens"]}]}`, w.Body.String())
			}
		})
	}
}
