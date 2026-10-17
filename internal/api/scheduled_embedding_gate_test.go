package api

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

func TestScheduledEmbeddingYieldsRealGateToMail(t *testing.T) {
	for _, message := range []bool{false, true} {
		name := "document"
		if message {
			name = "message"
		}
		t.Run(name, func(t *testing.T) {
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
				run := func(ctx context.Context) error {
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
				}
				if message {
					require.NoError(t, s.SetEmbedJob(&scheduler.EmbedJob{Worker: gateEmbeddingRunner{run: run}, Backend: gateEmbeddingBackend{}, BackstopInterval: -1}, "* * * * *", false))
				} else {
					require.NoError(t, s.SetDocumentVectorJob(run, "* * * * *", false))
				}
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
		})
	}
}

// The backend supplies the active generation; the real scheduler and gate own admission.
type gateEmbeddingBackend struct{ vector.Backend }

func (gateEmbeddingBackend) BuildingGeneration(context.Context) (*vector.Generation, error) {
	return nil, nil //nolint:nilnil // No building generation is the backend's normal idle state.
}
func (gateEmbeddingBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	return vector.Generation{ID: 1}, nil
}

type gateEmbeddingRunner struct{ run func(context.Context) error }

func (gateEmbeddingRunner) ReclaimStale(context.Context) (int, error) { return 0, nil }
func (r gateEmbeddingRunner) RunOnce(ctx context.Context, _ vector.GenerationID, _ operations.PassScope) (embed.RunResult, error) {
	return embed.RunResult{}, r.run(ctx)
}
func (gateEmbeddingRunner) RunBackstop(context.Context, vector.GenerationID, operations.PassScope) (embed.RunResult, error) {
	return embed.RunResult{}, nil
}
