package scheduler

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
