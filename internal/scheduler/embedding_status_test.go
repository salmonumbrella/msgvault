package scheduler

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
)

func TestEmbeddingJobStatus(t *testing.T) {
	t.Run("shows queue before slot acquisition", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			gate := newSerialWorkTracker()
			release, ok := gate.BeginWork()
			require.True(ok)
			s := New(nil).WithWorkTracker(gate)
			defer func() { <-s.Stop().Done() }()
			job := &EmbedJob{}
			require.NoError(s.SetEmbedJob(job, "0 0 * * *", true, 5*time.Minute))
			go s.cron.Entry(s.genericJobs["embed"]).Job.Run()
			synctest.Wait()
			status := s.EmbeddingStatus()
			assert.Equal("queued", status.Job.State)
			assert.NotNil(status.Job.QueuedAt)
			release()
			synctest.Wait()
			status = s.EmbeddingStatus()
			assert.Equal("idle", status.Job.State)
		})
	})
}

func TestPostSyncJobsStartIndependently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		s := New(func(context.Context, string) error { return nil }).WithWorkTracker(newSerialWorkTracker())
		defer func() { <-s.Stop().Done() }()
		passes := 0
		require.NoError(s.setVectorJob("embed", "scheduled embedding", func(ctx context.Context) error {
			passes++
			jobctx.RecordProgress(ctx)
			<-ctx.Done()
			return ctx.Err()
		}, "", true, time.Millisecond))
		documents := 0
		require.NoError(s.SetDocumentVectorJob(func(context.Context) error { documents++; return nil }, "", true, time.Millisecond))
		require.NoError(s.AddAccount("sync@example.com", "0 0 1 1 *"))
		require.NoError(s.TriggerSync("sync@example.com"))
		synctest.Wait()
		time.Sleep(2 * time.Millisecond)
		synctest.Wait()
		assert.Equal(1, documents, "documents get a turn while the message backfill continues")
		assert.GreaterOrEqual(passes, 2)
		assert.False(s.Status()[0].Running)
	})
}
