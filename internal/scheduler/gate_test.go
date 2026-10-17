package scheduler_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

// backfillRunner commits progress and waits for interruption during each
// blocking pass, like a large embedding backfill.
type backfillRunner struct {
	record         func()
	passes         int
	blockingPasses int
}

func (r *backfillRunner) RunOnce(ctx context.Context, _ vector.GenerationID, _ operations.PassScope) (embed.RunResult, error) {
	r.record()
	r.passes++
	if r.passes > max(1, r.blockingPasses) {
		return embed.RunResult{}, nil
	}
	jobctx.RecordProgress(ctx)
	<-ctx.Done()
	return embed.RunResult{}, ctx.Err()
}

func (r *backfillRunner) RunBackstop(context.Context, vector.GenerationID, operations.PassScope) (embed.RunResult, error) {
	return embed.RunResult{}, nil
}

func (r *backfillRunner) ReclaimStale(context.Context) (int, error) { return 0, nil }

type activeGenerationBackend struct{ vector.Backend }

func (activeGenerationBackend) BuildingGeneration(context.Context) (*vector.Generation, error) {
	return nil, nil //nolint:nilnil // No building generation is the backend's normal idle result.
}

func (activeGenerationBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	return vector.Generation{ID: 1}, nil
}

func TestEmbeddingYieldsDaemonGateToQueuedSync(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		gate := api.NewSerialOperationGate()
		var holders []string
		record := func() {
			label, _, _ := gate.Holder()
			holders = append(holders, label)
		}
		s := scheduler.New(func(context.Context, string) error { record(); return nil }).WithWorkTracker(gate)
		defer func() { <-s.Stop().Done() }()
		job := &scheduler.EmbedJob{Worker: &backfillRunner{record: record}, Backend: activeGenerationBackend{}, BackstopInterval: -1}
		require.NoError(s.SetEmbedJob(job, "", false))
		require.NoError(s.AddAccount("sync@example.com", "0 0 1 1 *"))

		_, err := s.StartJob("embed")
		require.NoError(err)
		synctest.Wait()
		require.Equal("running", s.EmbeddingStatus().Job.State)
		require.NoError(s.TriggerSync("sync@example.com"))
		synctest.Wait()

		time.Sleep(time.Minute + 10*time.Second)
		synctest.Wait()
		assert.Equal([]string{"scheduled embedding", "scheduled sync of sync@example.com", "scheduled embedding"}, holders,
			"embedding yields the gate to the sync, then continues")
		assert.False(s.Status()[0].LastRun.IsZero(), "the sync completed")
		embedding := s.JobStatus()[0]
		assert.False(embedding.Running)
		assert.False(embedding.Pending)
		assert.Empty(embedding.LastError)
		assert.False(embedding.LastRun.IsZero(), "the continuation completed")
	})
}

// delayedEmbeddingGate holds a fresh embed goroutine before real gate admission.
// This makes the post-sync goroutine race reproducible without replacing the gate.
type delayedEmbeddingGate struct {
	*api.SerialOperationGate

	ready, release chan struct{}
	delayed        bool
}

func (g *delayedEmbeddingGate) BeginLabeledWorkContext(ctx context.Context, label string) (func(), bool) {
	if label == "scheduled embedding" && !g.delayed {
		g.delayed = true
		close(g.ready)
		select {
		case <-g.release:
		case <-ctx.Done():
			return func() {}, false
		}
	}
	return g.SerialOperationGate.BeginLabeledWorkContext(ctx, label)
}

func TestPostSyncEmbeddingAdmission(t *testing.T) {
	for _, alreadyRunning := range []bool{false, true} {
		name := "fresh chain"
		if alreadyRunning {
			name = "existing backfill"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				gate := &delayedEmbeddingGate{SerialOperationGate: api.NewSerialOperationGate(), ready: make(chan struct{}), release: make(chan struct{})}
				var holders []string
				record := func() { label, _, _ := gate.Holder(); holders = append(holders, label) }
				s := scheduler.New(func(context.Context, string) error { record(); return nil }).WithWorkTracker(gate).WithVectorPassBudget(time.Millisecond)
				defer func() { <-s.Stop().Done() }()
				runner := &backfillRunner{record: record}
				if alreadyRunning {
					runner.blockingPasses = 2
				}
				job := &scheduler.EmbedJob{Worker: runner, Backend: activeGenerationBackend{}, BackstopInterval: -1}
				require.NoError(s.SetEmbedJob(job, "", true))
				require.NoError(s.SetDocumentVectorJob(func(context.Context) error { record(); return nil }, "", true))
				require.NoError(s.AddAccount("sync@example.com", "0 0 1 1 *"))
				if alreadyRunning {
					close(gate.release)
					_, err := s.StartJob("embed")
					require.NoError(err)
					synctest.Wait()
				}
				require.NoError(s.TriggerSync("sync@example.com"))
				if !alreadyRunning {
					<-gate.ready
					synctest.Wait()
					assert.Equal([]string{"scheduled sync of sync@example.com"}, holders, "documents wait for message admission")
					close(gate.release)
				}
				synctest.Wait()
				time.Sleep(3 * time.Millisecond)
				synctest.Wait()
				if alreadyRunning {
					require.Len(holders, 5)
					assert.Equal([]string{"scheduled embedding", "scheduled sync of sync@example.com"}, holders[:2])
					assert.ElementsMatch([]string{"scheduled embedding", "scheduled document indexing"}, holders[2:4])
					assert.Equal("scheduled embedding", holders[4], "embedding resumes after documents")
				} else {
					assert.Equal([]string{"scheduled sync of sync@example.com", "scheduled embedding", "scheduled document indexing", "scheduled embedding"}, holders)
				}
			})
		})
	}
}
