package api

import (
	"context"
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

type calendarEventsTestStore struct {
	*mockStore

	err   error
	calls int
}

func (s *calendarEventsTestStore) ListCalendarOccurrencesContext(_ context.Context, from, to time.Time) (store.CalendarOccurrencePage, error) {
	s.calls++
	return store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{MessageID: 42, Subject: "Planning", Start: from, End: to}}, Unexpandable: []store.CalendarUnexpandable{}}, s.err
}
func TestCalendarEventsRoute(t *testing.T) {
	backend := &calendarEventsTestStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, secret, _, err := srv.agentGrants.Issue("calendar", []agentgrant.Permission{agentgrant.PermissionCalendarWrite}, []agentgrant.SourceRef{{ID: 1, Type: "gcal", Identifier: "calendar@example.com"}}, time.Time{})
	require.NoError(t, err)
	path := "/api/v1/calendar/events?from=2027-03-01T00:00:00Z&to=2027-04-01T00:00:00Z"
	for _, tc := range []struct {
		name, path, token string
		err               error
		status            int
		want              string
	}{
		{"owner", path, "", nil, 200, `"message_id":42`},
		{"delegated", path, secret, nil, 403, ""},
		{"missing", "/api/v1/calendar/events", "", nil, 400, "invalid_argument"},
		{"reversed", strings.Replace(path, "2027-04-01", "2027-02-01", 1), "", nil, 400, "invalid_argument"},
		{"broad", path, "", store.ErrQueryTooBroad, 400, "query_too_broad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend.err = tc.err
			before := backend.calls
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.token != "" {
				r.Header.Set(apiprotocol.AgentTokenHeader, tc.token)
			} else {
				r.Header.Set("X-Api-Key", "synthetic-owner-key")
			}
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), tc.want)
			if tc.status == 403 {
				assert.Equal(t, before, backend.calls)
			}
		})
	}
}
