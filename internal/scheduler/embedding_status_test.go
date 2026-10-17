package scheduler

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/vector"
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
			require.NoError(s.SetEmbedJob(job, "0 0 * * *", true))
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
	t.Run("post-sync run acquires its own slot", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			gate := newSerialWorkTracker()
			runner := &slowRunner{gate: make(chan struct{}), release: make(chan struct{})}
			backend := &fakeBackend{active: vector.Generation{ID: 1}}
			s := New(func(context.Context, string) error { return nil }).WithWorkTracker(gate)
			defer func() { <-s.Stop().Done() }()
			require.NoError(s.SetEmbedJob(&EmbedJob{Worker: runner, Backend: backend}, "", true))
			require.NoError(s.AddAccount("synthetic@example.com", "0 0 * * *"))
			require.NoError(s.TriggerSync("synthetic@example.com"))
			<-runner.gate
			synctest.Wait()
			status := s.EmbeddingStatus()
			assert.Equal("running", status.Job.State)
			assert.False(s.Status()[0].Running, "the sync finishes before embedding starts")
			require.NotNil(status.Job.StartedAt)
			close(runner.release)
			synctest.Wait()
			assert.Equal("idle", s.EmbeddingStatus().Job.State)
		})
	})
	t.Run("coalesced triggers preserve the current start", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert := assert.New(t)
			s := New(nil)
			defer func() { <-s.Stop().Done() }()
			firstRelease := make(chan struct{})
			blocking := func(release chan struct{}) func(context.Context) error {
				return func(context.Context) error {
					<-release
					return nil
				}
			}
			require.NoError(t, s.setVectorJob("embed", "scheduled embedding", blocking(firstRelease), "", false))
			_, err := s.StartJob("embed")
			require.NoError(t, err)
			synctest.Wait()
			firstStarted := *s.EmbeddingStatus().Job.StartedAt
			time.Sleep(time.Minute)
			disposition, err := s.StartJob("embed")
			require.NoError(t, err)
			assert.Equal(JobPending, disposition)
			synctest.Wait()
			status := s.EmbeddingStatus()
			assert.Equal("running", status.Job.State)
			assert.Equal(firstStarted, *status.Job.StartedAt, "the run still going started first")
			close(firstRelease)
			synctest.Wait()
			assert.Equal("idle", s.EmbeddingStatus().Job.State)
		})
	})
}

func TestQueueTimesClearOnAdmission(t *testing.T) {
	for _, account := range []bool{true, false} {
		name := "job"
		if account {
			name = "account"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := newSerialWorkTracker()
				releaseGate, ok := gate.BeginWork()
				require.True(t, ok)
				releaseRun := make(chan struct{})
				run := func(ctx context.Context) error { <-releaseRun; return ctx.Err() }
				s := New(func(ctx context.Context, _ string) error { return run(ctx) }).WithWorkTracker(gate)
				defer func() { <-s.Stop().Done() }()
				queuedAt := time.Now()
				if account {
					require.NoError(t, s.AddAccount("queue@example.com", "0 0 1 1 *"))
					require.NoError(t, s.TriggerSync("queue@example.com"))
				} else {
					require.NoError(t, s.AddJob(Job{Name: "queue", Schedule: "0 0 1 1 *", Run: run}))
					_, err := s.StartJob("queue")
					require.NoError(t, err)
				}
				synctest.Wait()
				time.Sleep(20 * time.Second)
				// A repeated trigger while queued keeps the original queue time.
				if account {
					s.onAccountTick("queue@example.com")
					assert.Equal(t, queuedAt, s.Status()[0].QueuedAt)
					assert.False(t, s.Status()[0].Running)
				} else {
					disposition, err := s.StartJob("queue")
					require.NoError(t, err)
					assert.Equal(t, JobCoalesced, disposition)
					assert.Equal(t, queuedAt, s.JobStatus()[0].QueuedAt)
					assert.False(t, s.JobStatus()[0].Running)
				}
				releaseGate()
				synctest.Wait()
				if account {
					assert.Zero(t, s.Status()[0].QueuedAt)
					assert.True(t, s.Status()[0].Running)
				} else {
					assert.Zero(t, s.JobStatus()[0].QueuedAt)
					assert.True(t, s.JobStatus()[0].Running)
				}
				close(releaseRun)
			})
		})
	}
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
		}, "", true))
		documents := 0
		require.NoError(s.SetDocumentVectorJob(func(context.Context) error { documents++; return nil }, "", true))
		require.NoError(s.AddAccount("sync@example.com", "0 0 1 1 *"))
		require.NoError(s.TriggerSync("sync@example.com"))
		synctest.Wait()
		time.Sleep(5*time.Minute + time.Second)
		synctest.Wait()
		assert.Equal(1, documents, "documents get a turn while the message backfill continues")
		assert.GreaterOrEqual(passes, 2)
		assert.False(s.Status()[0].Running)
	})
}

func TestAddJobRejectsEmptySchedule(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := New(nil)
	defer func() { <-s.Stop().Done() }()
	job := Job{Name: "generic", Run: func(context.Context) error { return nil }}
	require.Error(s.AddJob(job))
	assert.False(s.IsJobScheduled(job.Name))
	job.Schedule = "0 0 * * *"
	require.NoError(s.AddJob(job))
	job.Schedule = ""
	require.Error(s.AddJob(job))
	assert.Equal("0 0 * * *", s.JobStatus()[0].Schedule, "invalid replacement preserves the registered schedule")
}
