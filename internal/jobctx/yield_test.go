package jobctx

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrorAfterYieldDistinguishesSchedulerShutdown(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	failure := errors.New("provider failed")
	for _, err := range []error{ctx.Err(), errors.Join(failure, ctx.Err())} {
		require.Same(err, ErrorAfterYield(ctx, err), "ordinary cancellation keeps the caller's original error")
	}
	shutdown, stop := context.WithCancelCause(t.Context())
	stop(ErrSchedulerStopped)
	for _, err := range []error{shutdown.Err(), context.Cause(shutdown)} {
		require.NoError(ErrorAfterYield(shutdown, err))
	}
	require.ErrorIs(ErrorAfterYield(shutdown, errors.Join(failure, shutdown.Err())), failure)
	require.NotErrorIs(ErrorAfterYield(shutdown, errors.Join(failure, shutdown.Err())), context.Canceled)
}
