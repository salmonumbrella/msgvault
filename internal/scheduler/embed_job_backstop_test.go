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
	db         *sql.DB
	afterStamp func()
}

func (s *e2eWorkStore) ScanForEmbedding(ctx context.Context, target, afterID int64, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM messages
		  WHERE (embed_gen IS NULL OR embed_gen <> ?)
		    AND deleted_at IS NULL AND deleted_from_source_at IS NULL
		    AND id > ?
		  ORDER BY id LIMIT ?`, target, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *e2eWorkStore) SetEmbedGen(ctx context.Context, ids []int64, target int64) error {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, 1+len(ids))
	args = append(args, target)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET embed_gen = ? WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
	return err
}

func (s *e2eWorkStore) SetEmbedGenIfUnchanged(ctx context.Context, items []store.EmbedGenStamp, target int64) (missed []int64, covered int, err error) {
	for _, it := range items {
		res, err := s.db.ExecContext(ctx,
			`UPDATE messages SET embed_gen = ? WHERE id = ? AND last_modified = ?`,
			target, it.ID, it.LastModified)
		if err != nil {
			return missed, covered, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return missed, covered, err
		}
		if n == 0 {
			missed = append(missed, it.ID)
		} else {
			covered++
		}
	}
	if s.afterStamp != nil && covered > 0 {
		s.afterStamp()
	}
	return missed, covered, nil
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

// A repair commits one batch before yielding, resumes the remaining rows,
// and starts its interval after completion.
func TestEmbedJob_Backstop_RecoversSubWatermarkStraggler(t *testing.T) {
	assert := assert.New(
		t,
	)
	require := require.New(t)

	ctx := context.Background()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	require.NoError(
		sqlitevec.RegisterExtension(), "RegisterExtension")

	mainDB, err := sql.Open(sqlitevec.DriverName(), mainPath)
	require.NoError(
		err, "open main")

	t.Cleanup(func() { _ = mainDB.Close() })

	_, err = mainDB.Exec(`
CREATE TABLE messages (
    id INTEGER PRIMARY KEY, subject TEXT,
    deleted_at DATETIME, deleted_from_source_at DATETIME, embed_gen INTEGER,
    last_modified DATETIME DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE message_bodies (
    message_id INTEGER PRIMARY KEY, body_text TEXT, body_html TEXT);
CREATE TABLE applied_migrations (name TEXT PRIMARY KEY, applied_at DATETIME);
CREATE TRIGGER trg_messages_last_modified
AFTER UPDATE ON messages FOR EACH ROW
WHEN OLD.last_modified = NEW.last_modified
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;
CREATE TRIGGER trg_message_bodies_last_modified_upd
AFTER UPDATE ON message_bodies FOR EACH ROW
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
END;
CREATE TRIGGER trg_message_bodies_last_modified_ins
AFTER INSERT ON message_bodies FOR EACH ROW
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
END;`)
	require.NoError(
		err, "schema")

	const n = 4
	for i := 1; i <= n; i++ {
		_, err = mainDB.Exec(`INSERT INTO messages (id, subject) VALUES (?, ?)`, i, fmt.Sprintf("msg %d", i))
		require.NoError(
			err, "insert message")

		_, err = mainDB.Exec(`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`, i, fmt.Sprintf("body %d", i))
		require.NoError(
			err, "insert body")
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

	ws := &e2eWorkStore{db: mainDB}
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

	_, err = mainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id IN (2, 3, 4)`)
	require.NoError(err, "un-stamp stragglers")
	now := time.Now()
	clock := &now
	job := &EmbedJob{
		Worker: worker, Backend: backend, Store: &e2eCoverage{db: mainDB}, Fingerprint: "fake:4",
		BackstopInterval: 24 * time.Hour, Now: func() time.Time { return *clock },
	}

	ctx, cancel := context.WithCancelCause(jobctx.WithProgress(ctx))
	defer cancel(nil)
	ws.afterStamp = func() { cancel(jobctx.ErrYieldedToWaiter) }
	_ = job.Run(ctx)
	ws.afterStamp = nil
	assert.True(jobctx.HasProgress(ctx), "the first repair batch committed before the yield")
	require.Equal(2, countMissingE2E(t, mainDB, int64(gen)), "the yield left two stragglers")

	*clock = now.Add(time.Minute)
	require.NoError(job.Run(t.Context()))
	assert.Equal(0, countMissingE2E(t, mainDB, int64(gen)), "the continuation finishes the interrupted repair")

	_, err = mainDB.Exec(`UPDATE messages SET embed_gen = NULL WHERE id = 2`)
	require.NoError(err)
	*clock = now.Add(2 * time.Minute)
	require.NoError(job.Run(t.Context()))
	assert.Equal(1, countMissingE2E(t, mainDB, int64(gen)), "the finished repair starts the interval")
}

type backstopYieldPublisher struct {
	vector.DocumentPublisher

	phase  string
	cancel context.CancelCauseFunc
	resets int
}

func (p *backstopYieldPublisher) GetDocumentProgress(ctx context.Context, gen vector.GenerationID) (vector.DocumentProgress, error) {
	progress, err := p.DocumentPublisher.GetDocumentProgress(ctx, gen)
	if err == nil && p.phase == "existing cursor" {
		p.phase = ""
		p.cancel(jobctx.ErrYieldedToWaiter)
	}
	return progress, err
}

func (p *backstopYieldPublisher) ResetDocumentReconcileCursor(ctx context.Context, gen vector.GenerationID) error {
	p.resets++
	phase := p.phase
	p.phase = ""
	if phase == "before reset" {
		p.cancel(jobctx.ErrYieldedToWaiter)
	}
	err := p.DocumentPublisher.ResetDocumentReconcileCursor(ctx, gen)
	if err == nil && phase == "after reset" {
		p.cancel(jobctx.ErrYieldedToWaiter)
	}
	return err
}

func TestContextualBackstopYieldRetainsOnlyDurableCursor(t *testing.T) {
	for _, phase := range []string{"before reset", "after reset", "existing cursor"} {
		t.Run(phase, func(t *testing.T) {
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
			if phase == "existing cursor" {
				cursor = "source:0"
			}
			require.NoError(backend.SetDocumentReconcileCursor(t.Context(), gen, cursor))
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			ctx = jobctx.WithProgress(ctx)
			publisher := &backstopYieldPublisher{DocumentPublisher: backend, phase: phase, cancel: cancel}
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
			if phase == "after reset" {
				assert.Empty(progress.ReconcileCursor)
			} else {
				assert.Equal(cursor, progress.ReconcileCursor)
			}
			ran, err = job.maybeRunBackstop(t.Context(), gen, slog.Default(), now.Add(time.Minute))
			require.NoError(err)
			assert.Equal(phase == "before reset", ran, "retry only when cancellation prevented the durable reset")
			if phase == "before reset" {
				assert.Equal(2, publisher.resets)
				progress, err = backend.GetDocumentProgress(t.Context(), gen)
				require.NoError(err)
				assert.True(strings.HasPrefix(progress.ReconcileCursor, "done:"))
			} else {
				_, err = worker.RunOnce(t.Context(), gen, scheduledEmbeddingPassScope(now.Add(time.Minute), gen, "forward"))
				require.NoError(err)
				progress, err = backend.GetDocumentProgress(t.Context(), gen)
				require.NoError(err)
				assert.True(strings.HasPrefix(progress.ReconcileCursor, "done:"), "RunOnce finishes the preserved repair cursor")
			}
		})
	}
}
