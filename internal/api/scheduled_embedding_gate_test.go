package api

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/scheduler"
)

func TestScheduledEmbeddingYieldsRealGateToMail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var order []string
		runs := 0
		release := make(chan struct{})
		s := scheduler.New(func(context.Context, string) error {
			order = append(order, "mail")
			return nil
		}).WithWorkTracker(NewSerialOperationGate())
		defer func() { <-s.Stop().Done() }()
		require.NoError(t, s.AddAccount("sync@example.com", "0 0 1 1 *"))
		require.NoError(t, s.SetDocumentVectorJob(func(ctx context.Context) error {
			runs++
			order = append(order, "embedding")
			if runs == 1 {
				<-ctx.Done()
				return ctx.Err()
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		}, "* * * * *", false))
		s.Start()
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Equal(t, []string{"embedding"}, order)
		require.NoError(t, s.TriggerSync("sync@example.com"))
		synctest.Wait()
		assert.True(t, s.Status()[0].Queued)
		time.Sleep(65 * time.Second)
		synctest.Wait()
		assert.Equal(t, []string{"embedding", "mail", "embedding"}, order)
		close(release)
	})
}
