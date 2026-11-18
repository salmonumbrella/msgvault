package msmail

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
)

type archiveGraph struct {
	mu                                                                                    sync.Mutex
	parent, parentOnCatalog                                                               string
	posts, reads                                                                          int
	paths                                                                                 map[string]int
	status                                                                                int
	landed, mismatch, lost, cancel, failRead, failCatalog, missingDeleted, missingArchive bool
	cancelFunc                                                                            context.CancelFunc
	server                                                                                *httptest.Server
}

func newArchiveGraph(t *testing.T) *archiveGraph {
	t.Helper()
	f := &archiveGraph{parent: "inbox-id", paths: map[string]int{}, status: http.StatusCreated, landed: true}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths[r.URL.Path]++
		assert.Contains(t, r.Header.Get("Prefer"), `IdType="ImmutableId"`)
		switch {
		case r.URL.Path == "/me/mailFolders":
			if f.parentOnCatalog != "" {
				f.parent = f.parentOnCatalog
			}
			if f.failCatalog {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			list := []map[string]any{{"id": "inbox-id", "displayName": "A/B"}, {"id": "archive-id", "displayName": "Archiv"}, {"id": "sent-id", "displayName": "A/B"}, {"id": "custom-id", "displayName": "Custom"}, {"id": "deleted-id", "displayName": "Corbeille/箱", "childFolderCount": 1}}
			_ = json.MarshalWrite(w, map[string]any{"value": list})
		case r.URL.Path == "/me/mailFolders/deleted-id/childFolders":
			_ = json.MarshalWrite(w, map[string]any{"value": []map[string]any{{"id": "child-id", "displayName": "A/B", "childFolderCount": 1}}})
		case r.URL.Path == "/me/mailFolders/child-id/childFolders":
			_ = json.MarshalWrite(w, map[string]any{"value": []map[string]any{{"id": "grandchild-id", "displayName": "Archiv"}}})
		case strings.HasPrefix(r.URL.Path, "/me/mailFolders/"):
			name := strings.TrimPrefix(r.URL.Path, "/me/mailFolders/")
			if name == "deleteditems" && !f.missingDeleted {
				_ = json.MarshalWrite(w, map[string]string{"id": "deleted-id"})
				return
			}
			if name == "archive" && f.missingArchive {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if name == "archive" || name == "inbox" {
				_ = json.MarshalWrite(w, map[string]string{"id": name + "-id"})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost:
			f.posts++
			assert.True(t, strings.HasSuffix(r.URL.Path, "/move"))
			assert.Contains(t, r.URL.EscapedPath(), "%2F")
			var body map[string]string
			if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Len(t, body, 1)
			if f.landed {
				f.parent = body["destinationId"]
			}
			if f.cancel {
				f.cancelFunc()
			}
			if f.lost {
				conn, _, e := w.(http.Hijacker).Hijack()
				if assert.NoError(t, e) {
					_ = conn.Close()
				}
				return
			}
			w.WriteHeader(f.status)
			if f.status == http.StatusCreated {
				responseID := "immutable/+id"
				if f.mismatch {
					responseID = "other"
				}
				_ = json.MarshalWrite(w, map[string]string{"id": responseID})
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/me/messages/"):
			f.reads++
			if f.failRead && f.posts > 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.MarshalWrite(w, map[string]any{"id": "immutable/+id", "parentFolderId": f.parent, "categories": []string{"Work"}})
		default:
			assert.Fail(t, "unexpected Graph request", "%s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}
func (f *archiveGraph) client() *Client {
	return NewClient(f.server.URL, func(context.Context) (string, error) { return "synthetic", nil }, 1000)
}

const archiveTestID = "immutable/+id"

func TestArchiveExcludedSubtree(t *testing.T) {
	for _, parent := range []string{"deleted-id", "child-id", "grandchild-id"} {
		for _, archived := range []bool{true, false} {
			for _, preview := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%v/%v", parent, archived, preview), func(t *testing.T) {
					f := newArchiveGraph(t)
					f.parent = parent
					_, err := f.client().ArchiveMessage(t.Context(), archiveTestID, archived, preview)
					var failure *emailtags.MessageTagError
					require.ErrorAs(t, err, &failure)
					assert.Equal(t, "stale_identity", failure.Code)
					assert.Zero(t, f.posts)
				})
			}
		}
	}
}
func TestArchiveParentChangesDuringCatalog(t *testing.T) {
	for _, parent := range []string{"deleted-id", "grandchild-id"} {
		t.Run(parent, func(t *testing.T) {
			f := newArchiveGraph(t)
			f.parentOnCatalog = parent
			_, err := f.client().ArchiveMessage(t.Context(), archiveTestID, true, false)
			var failure *emailtags.MessageTagError
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, "stale_identity", failure.Code)
			assert.Zero(t, f.posts)
		})
	}
}
func TestArchiveMessageMovesAndReadsBack(t *testing.T) {
	for _, initial := range []string{"inbox-id", "sent-id", "custom-id"} {
		for _, archived := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%v", initial, archived), func(t *testing.T) {
				f := newArchiveGraph(t)
				f.parent = initial
				c := f.client()
				preview, err := c.ArchiveMessage(t.Context(), archiveTestID, archived, true)
				require.NoError(t, err)
				assert.Equal(t, initial != "inbox-id" || archived, preview.Changed)
				assert.Zero(t, f.posts)
				got, err := c.ArchiveMessage(t.Context(), archiveTestID, archived, false)
				require.NoError(t, err)
				assert.True(t, got.Verified)
				assert.Equal(t, initial != "inbox-id" || archived, got.Changed)
				require.NotNil(t, got.MicrosoftCategories)
				assert.Equal(t, []string{"Work"}, *got.MicrosoftCategories)
				expectedPosts := 1
				if initial == "inbox-id" && !archived {
					expectedPosts = 0
				}
				assert.Equal(t, expectedPosts, f.posts)
				beforeArchive, beforeDeleted := f.paths["/me/mailFolders/archive"], f.paths["/me/mailFolders/deleteditems"]
				got, err = c.ArchiveMessage(t.Context(), archiveTestID, archived, false)
				require.NoError(t, err)
				assert.True(t, got.Verified)
				assert.False(t, got.Changed)
				assert.Equal(t, expectedPosts, f.posts)
				archiveLookups := 1
				if archived {
					archiveLookups++
				}
				assert.Equal(t, archiveLookups, f.paths["/me/mailFolders/archive"]-beforeArchive)
				assert.Equal(t, 2, f.paths["/me/mailFolders/deleteditems"]-beforeDeleted)
			})
		}
	}
}
func TestArchivePreflightFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*archiveGraph)
		code string
	}{{"unlisted parent", func(f *archiveGraph) { f.parent = "hidden" }, "stale_identity"}, {"missing deleted", func(f *archiveGraph) { f.missingDeleted = true }, "folder_not_found"}, {"missing archive", func(f *archiveGraph) { f.missingArchive = true }, "folder_not_found"}, {"catalog", func(f *archiveGraph) { f.failCatalog = true }, "provider_read_failed"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchiveGraph(t)
			tc.set(f)
			got, err := f.client().ArchiveMessage(t.Context(), archiveTestID, true, false)
			var failure *emailtags.MessageTagError
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, tc.code, failure.Code)
			assert.Nil(t, got)
			assert.Zero(t, f.posts)
		})
	}
}
func TestArchiveMoveResponses(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		status                           int
		landed, mismatch, lost, failRead bool
		code                             string
	}{
		{"forbidden", 403, false, false, false, false, "insufficient_scope"}, {"missing", 404, false, false, false, false, "stale_identity"}, {"throttled", 429, false, false, false, false, "provider_write_failed"},
		{"server error accepted", 500, true, false, false, false, ""}, {"server error unchanged", 500, false, false, false, false, "verification_failed"}, {"mismatched response", 201, true, true, false, false, "stale_identity"}, {"readback failure", 201, true, false, false, true, "remote_unknown"}, {"lost reply", 201, true, false, true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchiveGraph(t)
			f.status, f.landed, f.mismatch, f.lost, f.failRead = tc.status, tc.landed, tc.mismatch, tc.lost, tc.failRead
			got, err := f.client().ArchiveMessage(t.Context(), archiveTestID, true, false)
			assert.Equal(t, 1, f.posts)
			if tc.code == "" {
				require.NoError(t, err)
				assert.True(t, got.Verified)
				assert.True(t, got.Changed)
			} else {
				var failure *emailtags.MessageTagError
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, tc.code, failure.Code)
				if tc.status == http.StatusForbidden {
					assert.Contains(t, failure.Message, "Mail.ReadWrite")
					assert.Contains(t, failure.Message, "add-o365 --graph --mail-write")
					assert.Contains(t, failure.Message, "moves")
				}
				assert.False(t, got.Verified)
				assert.False(t, got.Changed)
			}
			if tc.status != 403 && tc.status != 404 && tc.status != 429 {
				assert.Equal(t, 2, f.reads)
			} else {
				assert.Equal(t, 1, f.reads)
			}
		})
	}
}
func TestArchiveCanceledRequestReadback(t *testing.T) {
	f := newArchiveGraph(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.cancel = true
	f.cancelFunc = cancel
	f.lost = true
	got, err := f.client().ArchiveMessage(ctx, archiveTestID, true, false)
	require.NoError(t, err)
	assert.True(t, got.Verified)
	assert.True(t, got.Changed)
	assert.Equal(t, 1, f.posts)
	assert.Equal(t, 2, f.reads)
}
