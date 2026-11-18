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
	parent, parentOnCatalog, parentOnSupplement                                           string
	ancestors                                                                             map[string]string
	badAncestor, emptyAncestor, denyAncestor, denyCatalog, emptyBody, malformed           bool
	posts, reads                                                                          int
	status                                                                                int
	landed, mismatch, lost, cancel, failRead, failCatalog, missingDeleted, missingArchive bool
	cancelFunc                                                                            context.CancelFunc
	server                                                                                *httptest.Server
}

func newArchiveGraph(t *testing.T) *archiveGraph {
	t.Helper()
	f := &archiveGraph{parent: "inbox-id", status: http.StatusCreated, landed: true}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		assert.Contains(t, r.Header.Get("Prefer"), `IdType="ImmutableId"`)
		switch {
		case r.URL.Path == "/me/mailFolders":
			if f.parentOnCatalog != "" {
				f.parent = f.parentOnCatalog
			}
			if f.denyCatalog {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if f.failCatalog {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			list := []map[string]any{{"id": "inbox-id", "displayName": "Inbox"}, {"id": "archive-id", "displayName": "Archiv"}, {"id": "sent-id", "displayName": "Sent"}, {"id": "custom-id", "displayName": "Custom"}, {"id": "deleted-id", "displayName": "Corbeille/箱", "childFolderCount": 1}}
			_ = json.MarshalWrite(w, map[string]any{"value": list})
		case r.URL.Path == "/me/mailFolders/deleted-id/childFolders":
			_ = json.MarshalWrite(w, map[string]any{"value": []map[string]any{{"id": "child-id", "displayName": "A/B", "childFolderCount": 1}}})
		case r.URL.Path == "/me/mailFolders/child-id/childFolders":
			_ = json.MarshalWrite(w, map[string]any{"value": []map[string]any{{"id": "grandchild-id", "displayName": "Archiv"}}})
		case strings.HasPrefix(r.URL.Path, "/me/mailFolders/"):
			name := strings.TrimPrefix(r.URL.Path, "/me/mailFolders/")
			if name == "msgfolderroot" {
				_ = json.MarshalWrite(w, map[string]string{"id": "root-id"})
				return
			}
			if parent, ok := f.ancestors[name]; ok {
				if f.denyAncestor {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if f.parentOnSupplement != "" {
					f.parent = f.parentOnSupplement
				}
				id := name
				if f.badAncestor {
					id = "different"
				}
				if f.emptyAncestor {
					id = ""
				}
				_ = json.MarshalWrite(w, map[string]string{"id": id, "parentFolderId": parent})
				return
			}
			if name == "deleteditems" && !f.missingDeleted {
				_ = json.MarshalWrite(w, map[string]string{"id": "deleted-id"})
				return
			}
			if name == "archive" && f.missingArchive {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if name == "sentitems" {
				_ = json.MarshalWrite(w, map[string]string{"id": "sent-id"})
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
				if f.emptyBody {
					return
				}
				if f.malformed {
					_, _ = w.Write([]byte("{"))
					return
				}
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

func TestArchiveEligibility(t *testing.T) {
	cases := []struct {
		name, parent                      string
		ancestors                         map[string]string
		catalogChange, supplementalChange string
		allowed                           bool
	}{
		{"hidden root", "hidden-id", map[string]string{"hidden-id": "root-id"}, "", "", true},
		{"hidden nested", "hidden-child", map[string]string{"hidden-child": "hidden-id", "hidden-id": "custom-id"}, "", "", true},
		{"deleted", "deleted-id", nil, "", "", false},
		{"deleted child", "child-id", nil, "", "", false},
		{"deleted grandchild", "grandchild-id", nil, "", "", false},
		{"hidden deleted child", "hidden-id", map[string]string{"hidden-id": "child-id"}, "", "", false},
		{"recoverable", "recovery", map[string]string{"recovery": "non-ipm", "non-ipm": ""}, "", "", false},
		{"retention sibling", "purges", map[string]string{"purges": "non-ipm", "non-ipm": ""}, "", "", false},
		{"unlisted", "hidden", nil, "", "", false},
		{"empty parent", "hidden", map[string]string{"hidden": ""}, "", "", false},
		{"mismatched ancestor", "hidden", map[string]string{"hidden": "root-id"}, "", "", false},
		{"empty ancestor ID", "hidden", map[string]string{"hidden": "root-id"}, "", "", false},
		{"inaccessible ancestor", "hidden", map[string]string{"hidden": "root-id"}, "", "", false},
		{"cycle", "hidden", map[string]string{"hidden": "hidden2", "hidden2": "hidden"}, "", "", false},
		{"too deep", "deep0", nil, "", "", false},
		{"changed during catalog", "inbox-id", nil, "grandchild-id", "", false},
		{"changed during ancestry", "hidden", map[string]string{"hidden": "root-id"}, "", "deleted-id", false},
	}
	for _, tc := range cases {
		for _, archived := range []bool{true, false} {
			for _, preview := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%v/%v", tc.name, archived, preview), func(t *testing.T) {
					f := newArchiveGraph(t)
					f.parent, f.ancestors = tc.parent, tc.ancestors
					f.parentOnCatalog, f.parentOnSupplement = tc.catalogChange, tc.supplementalChange
					f.badAncestor = tc.name == "mismatched ancestor"
					f.emptyAncestor = tc.name == "empty ancestor ID"
					f.denyAncestor = tc.name == "inaccessible ancestor"
					if tc.name == "too deep" {
						f.ancestors = map[string]string{}
						for i := range 33 {
							f.ancestors[fmt.Sprintf("deep%d", i)] = fmt.Sprintf("deep%d", i+1)
						}
					}
					got, err := f.client().ArchiveMessage(t.Context(), archiveTestID, archived, preview)
					if tc.allowed {
						require.NoError(t, err)
						assert.Equal(t, !preview, got.Verified)
						if !preview {
							assert.Equal(t, 1, f.posts)
						} else {
							assert.Zero(t, f.posts)
						}
					} else {
						var failure *emailtags.MessageTagError
						require.ErrorAs(t, err, &failure)
						assert.Equal(t, "stale_identity", failure.Code)
						assert.Zero(t, f.posts)
					}
				})
			}
		}
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
				got, err = c.ArchiveMessage(t.Context(), archiveTestID, archived, false)
				require.NoError(t, err)
				assert.True(t, got.Verified)
				assert.False(t, got.Changed)
				assert.Equal(t, expectedPosts, f.posts)

			})
		}
	}
}
func TestArchivePreflightFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*archiveGraph)
		code string
	}{{"missing deleted", func(f *archiveGraph) { f.missingDeleted = true }, "folder_not_found"}, {"missing archive", func(f *archiveGraph) { f.missingArchive = true }, "folder_not_found"}, {"catalog", func(f *archiveGraph) { f.failCatalog = true }, "provider_read_failed"}, {"catalog forbidden", func(f *archiveGraph) { f.denyCatalog = true }, "insufficient_scope"}} {
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
		name                                                         string
		status                                                       int
		landed, mismatch, lost, failRead, canceled, empty, malformed bool
		code                                                         string
		reads                                                        int
	}{
		{"success", 201, true, false, false, false, false, false, false, "", 2},
		{"forbidden", 403, false, false, false, false, false, false, false, "insufficient_scope", 1},
		{"missing", 404, false, false, false, false, false, false, false, "stale_identity", 1},
		{"throttled", 429, false, false, false, false, false, false, false, "provider_write_failed", 1},
		{"server error accepted", 500, true, false, false, false, false, false, false, "", 2},
		{"server error unchanged", 500, false, false, false, false, false, false, false, "remote_unknown", 2},
		{"success wrong parent", 201, false, false, false, false, false, false, false, "verification_failed", 2},
		{"mismatched response", 201, true, true, false, false, false, false, false, "stale_identity", 2},
		{"empty response", 201, true, false, false, false, false, true, false, "stale_identity", 2},
		{"malformed response", 201, true, false, false, false, false, false, true, "stale_identity", 2},
		{"readback failure", 201, true, false, false, true, false, false, false, "remote_unknown", 2},
		{"lost reply", 201, true, false, true, false, false, false, false, "", 2},
		{"canceled caller", 201, true, false, true, false, true, false, false, "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchiveGraph(t)
			f.status, f.landed, f.mismatch, f.lost, f.failRead = tc.status, tc.landed, tc.mismatch, tc.lost, tc.failRead
			f.emptyBody, f.malformed = tc.empty, tc.malformed
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.cancel, f.cancelFunc = tc.canceled, cancel
			got, err := f.client().ArchiveMessage(ctx, archiveTestID, true, false)
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
			assert.Equal(t, tc.reads, f.reads)
		})
	}
}
