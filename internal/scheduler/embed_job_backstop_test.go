//go:build sqlite_vec

package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// e2eWorkStore is a minimal embed.WorkStore over the test main DB,
// mirroring store.ScanForEmbedding / store.SetEmbedGen. afterStamp, when set,
// runs after each committed coverage stamp.
type e2eWorkStore struct {
	*store.Store

	afterStamp   func()
	scanAfterIDs []int64
	afterScan    func(int64)
}

func (s *e2eWorkStore) ScanForEmbedding(ctx context.Context, target, afterID int64, limit int) ([]int64, error) {
	s.scanAfterIDs = append(s.scanAfterIDs, afterID)
	ids, err := s.Store.ScanForEmbedding(ctx, target, afterID, limit)
	if s.afterScan != nil {
		s.afterScan(afterID)
	}
	return ids, err
}

func (s *e2eWorkStore) ScanEmbeddingBackstop(ctx context.Context, target, afterID int64, limit int, messageTypes []string, sourceIDs []int64) ([]int64, int64, error) {
	s.scanAfterIDs = append(s.scanAfterIDs, afterID)
	ids, examined, err := s.Store.ScanEmbeddingBackstop(ctx, target, afterID, limit, messageTypes, sourceIDs)
	if s.afterScan != nil {
		s.afterScan(afterID)
	}
	return ids, examined, err
}

func (s *e2eWorkStore) SetEmbedGenIfUnchanged(ctx context.Context, items []store.EmbedGenStamp, target int64) ([]int64, int, error) {
	missed, covered, err := s.Store.SetEmbedGenIfUnchanged(ctx, items, target)
	if s.afterStamp != nil && covered > 0 {
		s.afterStamp()
	}
	return missed, covered, err
}

// e2eCoverage satisfies EmbedCoverage from the live main DB so the
// EmbedJob's activation gate reflects real coverage.
type e2eCoverage struct{ db *sql.DB }

func (c *e2eCoverage) MissingCount(ctx context.Context, activeGen int64) (int64, error) {
	var missing int64
	if err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages
		  WHERE (embed_gen IS NULL OR embed_gen <> ?)
		    AND deleted_at IS NULL AND deleted_from_source_at IS NULL`, activeGen).Scan(&missing); err != nil {
		return 0, err
	}
	return missing, nil
}

// e2eClient returns one deterministic non-zero vector per input.
type e2eClient struct{ dim int }

func (c *e2eClient) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		v := make([]float32, c.dim)
		v[0] = float32(len(inputs[i])%c.dim + 1)
		out[i] = v
	}
	return out, nil
}

func countMissingE2E(t *testing.T, db *sql.DB, gen int64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM messages
		  WHERE (embed_gen IS NULL OR embed_gen <> ?)
		    AND deleted_at IS NULL AND deleted_from_source_at IS NULL`, gen).Scan(&n))

	return n
}

// A repair resumes covered pages and committed batches after yielding.
func TestEmbedJob_Backstop_ResumesAfterCommittedBatchYields(t *testing.T) {
	assert := assert.New(
		t,
	)
	require := require.New(t)

	ctx := context.Background()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	require.NoError(
		sqlitevec.RegisterExtension(), "RegisterExtension")

	st, err := store.OpenForTest(mainPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	mainDB := st.DB()
	source, err := st.GetOrCreateSource("gmail", "user@example.com")
	require.NoError(err)
	conversation, err := st.EnsureConversationWithType(source.ID, "thread", "email_thread", "Subject")
	require.NoError(err)
	const n = 256
	for i := 1; i <= n; i++ {
		_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation,
			SourceMessageID: fmt.Sprintf("message-%d", i), MessageType: "email",
			Subject: sql.NullString{String: fmt.Sprintf("msg %d", i), Valid: true}})
		require.NoError(err)
		_, err = mainDB.Exec(`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`, i, fmt.Sprintf("body %d", i))
		require.NoError(err)
	}

	vecPath := filepath.Join(dir, "vectors.db")
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path: vecPath, MainPath: mainPath, Dimension: 4, MainDB: mainDB,
	})
	require.NoError(
		err, "sqlitevec.Open")

	t.Cleanup(func() { _ = backend.Close() })

	gen, err := backend.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(
		err, "CreateGeneration")

	vecDB, err := sql.Open(sqlitevec.DriverName(), vecPath)
	require.NoError(
		err, "open vectors handle")

	t.Cleanup(func() { _ = vecDB.Close() })

	ws := &e2eWorkStore{Store: st}
	worker := embed.NewWorker(embed.WorkerDeps{
		Backend: backend, VectorsDB: vecDB, MainDB: mainDB,
		Store: ws, Client: &e2eClient{dim: 4}, BatchSize: 1,
		LastModifiedExpr: "CAST(m.last_modified AS TEXT)",
		Recorder:         testutil.NewTestStore(t),
	})

	// Drain the corpus fully via the worker so every message is embedded +
	// stamped and the per-gen watermark advances to the max id.
	_, err = worker.RunOnce(ctx, gen, operations.PassScope{
		Key: "test:scheduler:backstop:seed", Trigger: operations.TriggerScheduled,
		StartedAt: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(
		err, "initial drain")

	require.NoError(
		backend.ActivateGeneration(ctx, gen, false), "activate (coverage complete)")

	require.Equal(0, countMissingE2E(t, mainDB, int64(gen)), "all embedded after drain")

	_, err = mainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id IN (254, 255, 256)`)
	require.NoError(err, "un-stamp stragglers")
	now := time.Now()
	clock := &now
	var logs strings.Builder
	job := &EmbedJob{
		Worker: worker, Backend: backend, Store: &e2eCoverage{db: mainDB}, Fingerprint: "fake:4",
		BackstopInterval: 24 * time.Hour, Now: func() time.Time { return *clock },
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}

	scanCtx, cancelScan := context.WithCancelCause(jobctx.WithProgress(ctx))
	defer cancelScan(nil)
	ws.afterScan = func(afterID int64) {
		if afterID == 0 {
			cancelScan(jobctx.ErrYieldedToWaiter)
		}
	}
	_ = job.Run(scanCtx)
	assert.True(jobctx.HasProgress(scanCtx), "an empty examined page is a resumable checkpoint before any missing IDs are returned")
	require.Equal(3, countMissingE2E(t, mainDB, int64(gen)), "interruption before returned IDs leaves every straggler pending")
	ws.afterScan = nil
	ws.scanAfterIDs = nil
	*clock = now.Add(time.Second)

	ctx, cancel := context.WithCancelCause(jobctx.WithProgress(ctx))
	defer cancel(nil)
	ws.afterStamp = func() { cancel(jobctx.ErrYieldedToWaiter) }
	_ = job.Run(ctx)
	ws.afterStamp = nil
	require.GreaterOrEqual(len(ws.scanAfterIDs), 2)
	assert.Equal(int64(1), ws.scanAfterIDs[1], "the repair resumes after its examined covered page")
	assert.True(jobctx.HasProgress(ctx), "the first repair batch committed before the yield")
	assert.NotContains(logs.String(), "embed backstop complete")
	require.Equal(2, countMissingE2E(t, mainDB, int64(gen)), "the yield left two stragglers")

	*clock = now.Add(time.Minute)
	ws.scanAfterIDs = nil
	require.NoError(job.Run(t.Context()))
	require.GreaterOrEqual(len(ws.scanAfterIDs), 2)
	assert.Equal(int64(254), ws.scanAfterIDs[1], "the repair resumes after its committed batch instead of rescanning the covered prefix")
	assert.Equal(0, countMissingE2E(t, mainDB, int64(gen)), "the continuation finishes the interrupted repair")
}

type backstopYieldPublisher struct {
	vector.DocumentPublisher

	phase  string
	cancel context.CancelCauseFunc
	resets int
}

func (p *backstopYieldPublisher) ResetDocumentReconcileCursor(ctx context.Context, gen vector.GenerationID) error {
	p.resets++
	phase := p.phase
	p.phase = ""
	if phase == "before reset" {
		p.cancel(jobctx.ErrYieldedToWaiter)
	}
	return p.DocumentPublisher.ResetDocumentReconcileCursor(ctx, gen)
}

func TestContextualBackstopYieldRetainsOnlyDurableCursor(t *testing.T) {
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
	publisher := &backstopYieldPublisher{DocumentPublisher: backend, phase: "before reset", cancel: cancel}
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
