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
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestVisualOperationPassScopeIsRequestOwnedStableAndPrivacyBounded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	privateRequestID := "private-browser-request-with-user-material"
	privateHash := "private-retry-owner-hash"
	var buildScopes, resumeScopes, retryScopes []operations.PassScope
	srv := NewServerWithOptions(ServerOptions{
		Config: config.NewDefaultConfig(), Store: testutil.NewSQLiteTestStore(t), Logger: testLogger(),
	})
	srv.SetVisualOperations(
		func(_ context.Context, scope operations.PassScope) error {
			buildScopes = append(buildScopes, scope)
			return nil
		},
		func(_ context.Context, scope operations.PassScope) error {
			resumeScopes = append(resumeScopes, scope)
			return nil
		},
		func(_ context.Context, scope operations.PassScope, _ int64, _ string) error {
			retryScopes = append(retryScopes, scope)
			return nil
		},
		func(context.Context, bool) (visual.Status, error) { return visual.Status{}, nil }, nil,
	)
	router := srv.Router()
	headers := map[string]string{"X-Request-Id": privateRequestID}

	for range 2 {
		response := doRequest(t, router, http.MethodPost, "/api/v1/multimodal/run", nil, headers)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
	}
	response := doRequest(t, router, http.MethodPost, "/api/v1/multimodal/build",
		[]byte(`{"consent":true}`), headers)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	response = doRequest(t, router, http.MethodPost, "/api/v1/multimodal/retry",
		[]byte(`{"message_id":42,"blob_hash":"`+privateHash+`"}`), headers)
	require.Equal(http.StatusOK, response.Code, response.Body.String())

	require.Len(resumeScopes, 2)
	require.Len(buildScopes, 1)
	require.Len(retryScopes, 1)
	assert.Equal(resumeScopes[0].Key, resumeScopes[1].Key,
		"a retried HTTP request must reproduce its invocation key")
	assert.NotEqual(resumeScopes[0].Key, buildScopes[0].Key)
	assert.NotEqual(resumeScopes[0].Key, retryScopes[0].Key)
	for _, scope := range []operations.PassScope{resumeScopes[0], buildScopes[0], retryScopes[0]} {
		require.NoError(scope.Validate())
		assert.Equal(operations.TriggerManual, scope.Trigger)
		assert.LessOrEqual(len(scope.Key), operations.MaxInvocationKeyBytes)
		assert.NotContains(scope.Key, privateRequestID)
		assert.NotContains(scope.Key, privateHash)
	}
}

func TestVisualOperationPassScopeFailurePreventsExecution(t *testing.T) {
	executed := false
	srv := NewServerWithOptions(ServerOptions{
		Config: config.NewDefaultConfig(), Store: testutil.NewSQLiteTestStore(t), Logger: testLogger(),
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/multimodal/run", nil)
	response := httptest.NewRecorder()
	srv.runVisualOperation(response, request,
		func(context.Context, operations.PassScope) error {
			executed = true
			return nil
		},
		func(context.Context, bool) (visual.Status, error) { return visual.Status{}, nil },
		visualOperationResume, "visual_resume_failed",
	)

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.False(t, executed)
}

func TestVisualOperationHTTPMapsTerminalReplayToFixedFailure(t *testing.T) {
	tests := []struct {
		name     string
		state    operations.State
		code     operations.PublicErrorCode
		wantBody string
	}{
		{name: "failed", state: operations.StateFailed,
			code: operations.PublicErrorInvocationUpstreamFailed, wantBody: "Upstream operation failed."},
		{name: "cancelled", state: operations.StateCancelled,
			code: operations.PublicErrorInvocationCancelled, wantBody: "Operation was cancelled."},
		{name: "timed out", state: operations.StateFailed,
			code: operations.PublicErrorInvocationTimeout, wantBody: "Operation timed out."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			started := time.Date(2026, 8, 30, 16, 0, 0, 0, time.UTC)
			finished := started.Add(time.Second)
			trigger := operations.TriggerManual
			id, err := operations.NewInt64ID(operations.KindVisualEmbedding, 29)
			require.NoError(err)
			counters := operations.InvocationCounters{}
			if test.state == operations.StateFailed && test.code != operations.PublicErrorInvocationTimeout {
				counters = operations.InvocationCounters{Attempted: 1, Failed: 1}
			}
			run := &operations.Run{
				ID: id, Lane: operations.LaneVisualAttachments, State: test.state, Trigger: &trigger,
				StartedAt: started, FinishedAt: &finished,
				Counters: counters.PublicCounters(operations.KindVisualEmbedding),
				Error:    operations.FixedPublicError(test.code),
			}
			replayErr := operations.TerminalReplayOutcome(run)
			require.Error(replayErr)

			srv := NewServerWithOptions(ServerOptions{
				Config: config.NewDefaultConfig(), Store: testutil.NewSQLiteTestStore(t), Logger: testLogger(),
			})
			srv.SetVisualOperations(
				func(context.Context, operations.PassScope) error { return nil },
				func(context.Context, operations.PassScope) error { return replayErr },
				func(context.Context, operations.PassScope, int64, string) error { return nil },
				func(context.Context, bool) (visual.Status, error) { return visual.Status{}, nil }, nil,
			)
			response := doRequest(t, srv.Router(), http.MethodPost, "/api/v1/multimodal/run", nil, nil)

			assert.Equal(http.StatusBadGateway, response.Code)
			assert.Contains(response.Body.String(), test.wantBody)
			assert.NotContains(response.Body.String(), "private provider response")
			assert.NotContains(strings.ToLower(response.Body.String()), "fingerprint")
		})
	}
}

func TestVisualOperationPassScopeIsFreshForEachGeneratedRequestID(t *testing.T) {
	var scopes []operations.PassScope
	srv := NewServerWithOptions(ServerOptions{
		Config: config.NewDefaultConfig(), Store: testutil.NewSQLiteTestStore(t), Logger: testLogger(),
	})
	srv.SetVisualOperations(
		func(context.Context, operations.PassScope) error { return nil },
		func(_ context.Context, scope operations.PassScope) error {
			scopes = append(scopes, scope)
			return nil
		},
		func(context.Context, operations.PassScope, int64, string) error { return nil },
		func(context.Context, bool) (visual.Status, error) { return visual.Status{}, nil }, nil,
	)
	router := srv.Router()

	for range 2 {
		response := doRequest(t, router, http.MethodPost, "/api/v1/multimodal/run", nil, nil)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	}

	require.Len(t, scopes, 2)
	assert.NotEqual(t, scopes[0].Key, scopes[1].Key,
		"each HTTP loop pass receives a new middleware-owned request identity")
}
