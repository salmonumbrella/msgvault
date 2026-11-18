package api

import (
	"context"
	"net"
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
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/imap"
)

type archiveHTTPStore struct {
	mockStore

	calls  int
	change emailtags.ArchiveChange
	fail   error
}

func (s *archiveHTTPStore) ArchiveMessage(ctx context.Context, id int64, change emailtags.ArchiveChange, _ *agentgrant.Grant, acquire func(context.Context) (func(), error)) (*emailtags.ArchiveResult, error) {
	if !change.DryRun {
		release, err := acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	s.calls++
	s.change = change
	return &emailtags.ArchiveResult{MessageID: id, Archived: *change.Archived, DryRun: change.DryRun}, s.fail
}
func TestArchiveMessageRouteValidationAndGate(t *testing.T) {
	assertionsForTest := assert.New(t) //nolint:paralleltest // swaps the gate wait bound
	old := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = old })
	st := &archiveHTTPStore{}
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger(), OperationGate: gate})
	call := func(id, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/messages/"+id+"/archive", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w
	}
	for _, body := range []string{`{}`, `{"archived":null}`, `{"archived":true,"dry_run":null}`, `{"archived":true,"whole_chat":null}`, `{"archived":true,"whole_chat":"true"}`, `{"archived":true,"imap_copy":null}`, `{"archived":true,"imap_copy":{"mailbox":"INBOX","uid":1}}`} {
		response := call("7", body)
		assertionsForTest.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	}
	assertionsForTest.Equal(http.StatusBadRequest, call("0", `{"archived":true}`).Code)
	assertionsForTest.Zero(st.calls)
	selected := call("7", `{"archived":true,"dry_run":true,"imap_copy":{"mailbox":"INBOX","uidvalidity":77,"uid":8}}`)
	assertionsForTest.Equal(http.StatusOK, selected.Code, selected.Body.String())
	assertionsForTest.Equal(&emailtags.IMAPCopy{Mailbox: "INBOX", UIDValidity: 77, UID: 8}, st.change.IMAPCopy)
	st.calls = 0
	delegated := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Logger: testLogger()})
	t.Cleanup(delegated.agentGrants.Close)
	_, secret, _, err := delegated.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "owner@example.test"}}, time.Time{})
	require.NoError(t, err)
	readOnly := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/messages/7/archive", strings.NewReader(`{"archived":true,"dry_run":true}`))
	readOnly.Header.Set(apiprotocol.AgentTokenHeader, secret)
	readOnly.Header.Set("Content-Type", "application/json")
	denied := httptest.NewRecorder()
	delegated.Router().ServeHTTP(denied, readOnly)
	assertionsForTest.Equal(http.StatusForbidden, denied.Code, denied.Body.String())
	assertionsForTest.Zero(st.calls)
	st.fail = emailtags.Failure("archive_unsupported", "synthetic failure", nil, nil)
	response := call("7", `{"archived":false}`)
	assertionsForTest.Equal(http.StatusNotImplemented, response.Code, response.Body.String())
	st.fail = nil
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "sync")
	require.True(t, ok)
	defer release()
	// The route table permits previews past the middleware gate; adapter tests cover provider behavior.
	preview := call("7", `{"archived":true,"dry_run":true}`)
	assertionsForTest.Equal(http.StatusOK, preview.Code, preview.Body.String())
	write := call("7", `{"archived":true}`)
	assertionsForTest.Equal(http.StatusServiceUnavailable, write.Code, write.Body.String())
}

func TestArchiveWriteOnlyFailureDoesNotRevealMailbox(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := addr.Port
	require.NoError(t, listener.Close())
	client := imap.NewClient(&imap.Config{Host: "127.0.0.1", Port: port, Username: "user@example.test"}, "fixture")
	defer func() { _ = client.Close() }()
	_, _, providerErr := client.ArchiveCopy(t.Context(), nil, imap.DraftReceipt{Mailbox: "Private-Example-Folder", UIDValidity: 456, UID: 123}, nil, true, false)
	require.Error(t, providerErr)
	st := &archiveHTTPStore{fail: providerErr}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	_, secret, _, err := srv.agentGrants.Issue("writer", []agentgrant.Permission{agentgrant.PermissionMessageArchive}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "user@example.test"}}, time.Time{})
	require.NoError(t, err)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/messages/7/archive", strings.NewReader(`{"archived":true}`))
	req.Header.Set(apiprotocol.AgentTokenHeader, secret)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "Private-Example-Folder")
	assert.Contains(t, w.Body.String(), `"error":"provider_read_failed"`)
	assert.Contains(t, w.Body.String(), "Cannot access the IMAP account")
}
