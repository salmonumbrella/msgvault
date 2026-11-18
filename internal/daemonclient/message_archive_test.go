package daemonclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

func TestArchiveMessageClientClassifiesWriteAndPreviewFailures(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{"status":"ok","api_schema_version":"5.17.0"}`))
			assert.NoError(t, err)
			return
		}
		assertions.Equal(http.MethodPost, r.Method)
		assertions.Equal("/api/v1/messages/7/archive", r.URL.Path)
		var payload map[string]bool
		if !assertions.NoError(json.NewDecoder(r.Body).Decode(&payload)) {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if payload["whole_chat"] {
			assertions.Contains(payload, "archived")
			assertions.False(payload["archived"])
			_, err := w.Write([]byte(`{"message_id":7,"chat_id":"fixture-chat","archived":false,"verified":true}`))
			assertions.NoError(err)
			return
		}
		_, _ = w.Write([]byte(`{`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(client.Close()) })
	archived := true
	_, err = client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &archived})
	var failure *emailtags.MessageTagError
	requirements.ErrorAs(err, &failure)
	assertions.Equal("remote_unknown", failure.Code)
	_, err = client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &archived, DryRun: true})
	requirements.Error(err)
	assertions.NotErrorAs(err, &failure, "preview failures must not claim a provider write happened")
	desired := false
	result, err := client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &desired, WholeChat: true})
	requirements.NoError(err)
	assertions.Equal("fixture-chat", result.ChatID)
}

func TestArchiveMessageClientIMAPCopyAndVersionGate(t *testing.T) {
	for _, tc := range []struct {
		version string
		allowed bool
	}{{"5.16.0", false}, {"5.17.0", true}} {
		t.Run(tc.version, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					_, err := w.Write([]byte(`{"status":"ok","api_schema_version":"` + tc.version + `"}`))
					assert.NoError(t, err)
					return
				}
				posts++
				var got emailtags.ArchiveChange
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&got)) {
					return
				}
				assert.Equal(t, &emailtags.IMAPCopy{Mailbox: "INBOX", UIDValidity: 4294967295, UID: 4294967295}, got.IMAPCopy)
				_, err := w.Write([]byte(`{"message_id":7,"archived":true,"verified":true}`))
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, client.Close()) })
			archived := true
			_, err = client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &archived, IMAPCopy: &emailtags.IMAPCopy{Mailbox: "INBOX", UIDValidity: 4294967295, UID: 4294967295}})
			if tc.allowed {
				require.NoError(t, err)
				assert.Equal(t, 1, posts)
			} else {
				require.Error(t, err)
				assert.Zero(t, posts)
			}
		})
	}
}
