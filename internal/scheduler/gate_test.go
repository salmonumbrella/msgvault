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

// backfillRunner commits progress on its first pass and then waits until the
// scheduler interrupts it, like a large embedding backfill.
type backfillRunner struct {
	record func()
	passes int
}

func (r *backfillRunner) RunOnce(ctx context.Context, _ vector.GenerationID, _ operations.PassScope) (embed.RunResult, error) {
	r.record()
	r.passes++
	if r.passes > 1 {
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
