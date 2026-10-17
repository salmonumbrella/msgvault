package scheduler

import (
	"context"
	"errors"
	"strconv"
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
			status := s.EmbeddingStatus()
			assert.Equal("running", status.Job.State)
			assert.False(s.Status()[0].Running, "the sync finishes before embedding starts")
			assert.Nil(status.Job.QueuedAt, "queue time clears on admission")
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
			require.NoError(t, s.setCronSlot(&s.embed, blocking(firstRelease), "", false))
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
					require.NoError(t, s.AddJob(Job{Name: "queue", Run: run}))
					_, err := s.StartJob("queue")
					require.NoError(t, err)
				}
				synctest.Wait()
				time.Sleep(20 * time.Second)
				if account {
					assert.Equal(t, queuedAt, s.Status()[0].QueuedAt)
					assert.False(t, s.Status()[0].Running)
				} else {
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

func TestAutomaticEmbeddingBudgetContinuesOnlyCommittedProgress(t *testing.T) {
	for _, progress := range []bool{true, false} {
		t.Run(strconv.FormatBool(progress), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := New(nil)
				defer func() { <-s.Stop().Done() }()
				runs := 0
				callbackErr := errors.New("synthetic publication failure")
				release := make(chan struct{})
				require.NoError(t, s.SetDocumentVectorJob(func(ctx context.Context) error {
					runs++
					if runs == 1 {
						if progress {
							jobctx.RecordProgress(ctx)
						}
						<-ctx.Done()
						return errors.Join(callbackErr, ctx.Err(), context.Cause(ctx))
					}
					<-release
					return nil
				}, "", true))
				_, err := s.StartJob("document-vector")
				require.NoError(t, err)
				time.Sleep(5*time.Minute + time.Second)
				synctest.Wait()
				status := s.JobStatus()[0]
				assert.Contains(t, status.LastError, callbackErr.Error())
				if progress {
					assert.Equal(t, 2, runs)
					assert.True(t, status.Running)
					assert.NotContains(t, status.LastError, jobctx.ErrRunBudgetExceeded.Error())
				} else {
					assert.Equal(t, 1, runs)
					assert.False(t, status.Running)
					assert.Contains(t, status.LastError, "before committing progress")
				}
				close(release)
			})
		})
	}
}

func TestEmbedJobStopsBetweenPhases(t *testing.T) {
	for _, budget := range []bool{false, true} {
		t.Run(strconv.FormatBool(budget), func(t *testing.T) {
			ctx, request := jobctx.WithPreemption(t.Context())
			ctx, cancel := context.WithCancelCause(ctx)
			defer cancel(nil)
			runner := &fakeRunner{onRunOnce: func(vector.GenerationID) {
				if budget {
					cancel(jobctx.ErrRunBudgetExceeded)
				} else {
					request()
				}
			}}
			backend := &fakeBackend{active: vector.Generation{ID: 1}}
			job := &EmbedJob{Worker: runner, Backend: backend}
			job.Run(ctx)
			assert.Equal(t, 1, runner.runCalls)
			assert.Zero(t, runner.backstopCalls, "a stop request prevents starting another embedding phase")
		})
	}
}

func TestPostSyncDocumentWaitsForEmbeddingContinuation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var order []string
		runs := 0
		release := make(chan struct{})
		s := New(func(context.Context, string) error { return nil }).WithWorkTracker(newSerialWorkTracker())
		defer func() { <-s.Stop().Done() }()
		require.NoError(t, s.setCronSlot(&s.embed, func(ctx context.Context) error {
			runs++
			order = append(order, "embed")
			if runs == 1 {
				jobctx.RecordProgress(ctx)
				<-ctx.Done()
				return ctx.Err()
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
			order = append(order, "embed complete")
			return nil
		}, "", true))
		require.NoError(t, s.SetDocumentVectorJob(func(context.Context) error {
			order = append(order, "document")
			return nil
		}, "", true))
		require.NoError(t, s.AddAccount("sync@example.com", "0 0 1 1 *"))
		require.NoError(t, s.TriggerSync("sync@example.com"))
		synctest.Wait()
		time.Sleep(5*time.Minute + time.Second)
		synctest.Wait()
		assert.Equal(t, []string{"embed", "embed"}, order, "document work waits through the embedding continuation")
		close(release)
		synctest.Wait()
		assert.Equal(t, []string{"embed", "embed", "embed complete", "document"}, order)
	})
}
