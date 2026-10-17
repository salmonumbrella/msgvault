//go:build sqlite_vec

package scheduler

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestEmbedJob_MaybeRunBackstop_YieldedCleanRunDoesNotRecordCompletion(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	runner := &fakeRunner{yieldBackstop: cancel}
	job := &EmbedJob{Worker: runner, BackstopInterval: time.Hour}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	now := time.Now()

	ran, err := job.maybeRunBackstop(ctx, 5, logger, now)
	require.NoError(t, err)
	assert.True(t, ran)
	assert.NotContains(t, logs.String(), "embed backstop complete")

	runner.yieldBackstop = nil
	ran, err = job.maybeRunBackstop(t.Context(), 5, logger, now.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, ran, "a yielded repair retries before the interval expires")
	calls, _ := runner.backstops()
	assert.Equal(t, 2, calls)
}

type backstopYieldPublisher struct {
	vector.DocumentPublisher

	cancel context.CancelCauseFunc
	resets int
}

func (p *backstopYieldPublisher) ResetDocumentReconcileCursor(ctx context.Context, gen vector.GenerationID) error {
	p.resets++
	if p.cancel != nil {
		p.cancel(jobctx.ErrYieldedToWaiter)
		p.cancel = nil
	}
	return p.DocumentPublisher.ResetDocumentReconcileCursor(ctx, gen)
}

func TestContextualBackstopRetriesAfterCancelledCursorReset(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	st, err := store.OpenForTest(mainPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	require.NoError(sqlitevec.RegisterExtension())
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(dir, "vectors.db"), MainPath: mainPath, MainDB: st.DB(), Dimension: 4})
	require.NoError(err)
	t.Cleanup(func() { _ = backend.Close() })
	gen, err := backend.CreateGeneration(t.Context(), "synthetic", 4, "synthetic:4")
	require.NoError(err)
	cursor := "done:0"
	require.NoError(backend.SetDocumentReconcileCursor(t.Context(), gen, cursor))
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	ctx = jobctx.WithProgress(ctx)
	publisher := &backstopYieldPublisher{DocumentPublisher: backend, cancel: cancel}
	worker := embed.NewContextWorker(embed.ContextWorkerDeps{
		Backend: backend, Publisher: publisher, Store: st, Assembler: embed.CompositeAssembler{}, Recorder: st,
		Client: embed.NewVoyageClient(embed.VoyageConfig{Endpoint: "http://127.0.0.1:1", Model: "synthetic", Dimension: 4}),
	})
	job := &EmbedJob{Worker: worker, Backend: backend, SequenceBoundActivation: true, BackstopInterval: time.Hour}
	now := time.Now().UTC()
	ran, err := job.maybeRunBackstop(ctx, gen, slog.Default(), now)
	assert.True(ran)
	require.ErrorIs(err, context.Canceled)
	progress, err := backend.GetDocumentProgress(t.Context(), gen)
	require.NoError(err)
	assert.Equal(cursor, progress.ReconcileCursor)
	ran, err = job.maybeRunBackstop(t.Context(), gen, slog.Default(), now.Add(time.Minute))
	require.NoError(err)
	assert.True(ran, "retry when cancellation prevented the durable reset")
	assert.Equal(2, publisher.resets)
	progress, err = backend.GetDocumentProgress(t.Context(), gen)
	require.NoError(err)
	assert.True(strings.HasPrefix(progress.ReconcileCursor, "done:"))
}
