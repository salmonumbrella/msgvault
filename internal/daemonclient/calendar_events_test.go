package daemonclient_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

func TestCalendarEventsClient(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	from, err := time.Parse(time.RFC3339, "2027-03-01T17:00:00Z")
	requirements.NoError(err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("/api/v1/calendar/events", r.URL.Path)
		assertions.Equal("2027-03-01T17:00:00Z", r.URL.Query().Get("from"))
		assertions.Equal("synthetic-owner-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		assertions.NoError(json.MarshalWrite(w, store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{MessageID: 42, Start: from, End: from.Add(time.Hour)}}, Unexpandable: []store.CalendarUnexpandable{}}))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(client.Close()) })
	page, err := client.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(int64(42), page.Occurrences[0].MessageID)
}
