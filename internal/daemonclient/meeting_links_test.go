package daemonclient_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestMeetingLinksClient(t *testing.T) {
	var decisions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-owner-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			assert.Equal(t, "/api/v1/meetings/links/decisions", r.URL.Path)
			var body struct {
				MessageID int64  `json:"message_id"`
				Event     string `json:"event"`
				Decision  string `json:"decision"`
			}
			if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
				return
			}
			assert.Equal(t, int64(42), body.MessageID)
			assert.Equal(t, "evt:81@20270314T100000Z", body.Event)
			decisions = append(decisions, body.Decision)
			assert.NoError(t, json.MarshalWrite(w, body))
			return
		}
		assert.Equal(t, "/api/v1/meetings/suggestions", r.URL.Path)
		if r.URL.Query().Get("ref") == "msg:404" {
			w.WriteHeader(404)
			_, err := w.Write([]byte(`{"error":"meeting_not_found","message":"Missing"}`))
			assert.NoError(t, err)
			return
		}
		assert.Equal(t, "evt:81@20270314T100000Z", r.URL.Query().Get("ref"))
		_, err := w.Write([]byte(`{"suggestions":[{"ref":"evt:81@20270314T100000Z","member_message_id":42,"overlap_minutes":30,"shared_attendees":1,"title_tokens":2,"reasons":["time_overlap"]}]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	for _, decision := range []string{"link", "unlink"} {
		require.NoError(t, client.DecideMeetingLink(t.Context(), 42, "evt:81@20270314T100000Z", decision))
	}
	assert.Equal(t, []string{"link", "unlink"}, decisions)
	suggestions, err := client.SuggestMeetingLinks(t.Context(), "evt:81@20270314T100000Z")
	require.NoError(t, err)
	require.Len(t, suggestions, 1)
	assert.Equal(t, int64(42), suggestions[0].MemberMessageID)
	assert.Equal(t, 30, suggestions[0].OverlapMinutes)
	assert.Equal(t, 1, suggestions[0].SharedAttendees)
	assert.Equal(t, 2, suggestions[0].TitleTokens)
	assert.Equal(t, []string{"time_overlap"}, suggestions[0].Reasons)
	_, err = client.SuggestMeetingLinks(t.Context(), "msg:404")
	var apiError *daemonclient.APIError
	require.ErrorAs(t, err, &apiError)
	assert.Equal(t, 404, apiError.Status)
}
