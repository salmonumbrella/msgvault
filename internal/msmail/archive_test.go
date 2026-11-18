package msmail

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
)

func TestArchiveMessageMovesAndReadsBack(t *testing.T) {
	const id = "immutable/+id"
	parent := "inbox-id"
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.Header.Get("Prefer"), `IdType="ImmutableId"`)
		switch {
		case strings.HasPrefix(r.URL.Path, "/me/mailFolders/"):
			name := strings.TrimPrefix(r.URL.Path, "/me/mailFolders/")
			if name == "inbox" || name == "archive" {
				require.NoError(t, json.MarshalWrite(w, map[string]string{"id": name + "-id"}))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/me/mailFolders":
			require.NoError(t, json.MarshalWrite(w, map[string]any{"value": []map[string]string{{"id": "inbox-id", "displayName": "Boîte"}, {"id": "archive-id", "displayName": "Archiv"}}}))
		case r.URL.Path == "/me/messages/"+id+"/move":
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Contains(t, r.URL.EscapedPath(), "%2F")
			var body map[string]string
			require.NoError(t, json.UnmarshalRead(r.Body, &body))
			assert.Len(t, body, 1)
			parent = body["destinationId"]
			posts++
			require.NoError(t, json.MarshalWrite(w, map[string]string{"id": id}))
		case r.URL.Path == "/me/messages/"+id:
			require.NoError(t, json.MarshalWrite(w, map[string]any{"id": id, "parentFolderId": parent, "categories": []string{"Work"}}))
		default:
			assert.Fail(t, "unexpected Graph request", "%s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic", nil }, 1000)
	preview, err := c.ArchiveMessage(t.Context(), id, true, true)
	require.NoError(t, err)
	assert.True(t, preview.DryRun)
	assert.Equal(t, 0, posts)
	result, err := c.ArchiveMessage(t.Context(), id, true, false)
	require.NoError(t, err)
	assert.True(t, result.Verified)
	assert.Equal(t, "archive-id", result.MicrosoftFolderID)
	require.NotNil(t, result.MicrosoftCategories)
	assert.Equal(t, []string{"Work"}, *result.MicrosoftCategories)
	_, err = c.ArchiveMessage(t.Context(), id, true, false)
	require.NoError(t, err)
	assert.Equal(t, 1, posts)
	_, err = c.ArchiveMessage(t.Context(), id, false, false)
	require.NoError(t, err)
	assert.Equal(t, 2, posts)
}

func TestArchiveMessageUncertainPostReadback(t *testing.T) {
	for _, scenario := range []string{"lost reply", "canceled request", "different readback"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			parent, reads, posts := "inbox-id", 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/me/mailFolders/"):
					name := strings.TrimPrefix(r.URL.Path, "/me/mailFolders/")
					if name == "archive" || name == "inbox" {
						_ = json.MarshalWrite(w, map[string]string{"id": name + "-id"})
						return
					}
					w.WriteHeader(http.StatusNotFound)
				case r.URL.Path == "/me/mailFolders":
					_ = json.MarshalWrite(w, map[string]any{"value": []map[string]string{{"id": "archive-id", "displayName": "Archiv"}}})
				case r.Method == http.MethodPost:
					posts++
					if scenario != "different readback" {
						parent = "archive-id"
					}
					if scenario == "canceled request" {
						cancel()
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if assert.NoError(t, err) {
						_ = conn.Close()
					}
				default:
					reads++
					_ = json.MarshalWrite(w, map[string]string{"id": "id", "parentFolderId": parent})
				}
			}))
			t.Cleanup(srv.Close)
			c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic", nil }, 1000)
			result, err := c.ArchiveMessage(ctx, "id", true, false)
			assert.Equal(t, 1, posts)
			assert.Equal(t, 2, reads)
			if scenario == "different readback" {
				require.Error(t, err)
				assert.Empty(t, result.Before)
				return
			}
			require.NoError(t, err)
			assert.True(t, result.Verified)
			assert.Equal(t, []string{"INBOX"}, result.Before)
			assert.Nil(t, result.MicrosoftCategories)
		})
	}
}

func TestArchiveMessageReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		folderStatus, messageStatus int
		messageID, code             string
	}{
		{"missing archive", http.StatusNotFound, 0, "", "folder_not_found"},
		{"forbidden folder", http.StatusForbidden, 0, "", "insufficient_scope"},
		{"missing message", http.StatusOK, http.StatusNotFound, "", "stale_identity"},
		{"forbidden message", http.StatusOK, http.StatusForbidden, "", "insufficient_scope"},
		{"different identity", http.StatusOK, http.StatusOK, "different", "stale_identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if strings.HasPrefix(r.URL.Path, "/me/mailFolders/") {
					w.WriteHeader(tc.folderStatus)
					if tc.folderStatus == http.StatusOK {
						_, _ = w.Write([]byte(`{"id":"folder"}`))
					}
					return
				}
				w.WriteHeader(tc.messageStatus)
				if tc.messageStatus == http.StatusOK {
					_ = json.MarshalWrite(w, map[string]string{"id": tc.messageID, "parentFolderId": "folder"})
				}
			}))
			t.Cleanup(srv.Close)
			c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic", nil }, 1000)
			_, err := c.ArchiveMessage(t.Context(), "id", true, false)
			var failure *emailtags.MessageTagError
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, tc.code, failure.Code)
			assert.Zero(t, posts)
		})
	}
}
