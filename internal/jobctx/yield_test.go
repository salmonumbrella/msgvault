package jobctx

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrorAfterYieldDistinguishesSchedulerShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	failure := errors.New("provider failed")
	for _, err := range []error{ctx.Err(), errors.Join(failure, ctx.Err())} {
		require.Same(t, err, ErrorAfterYield(ctx, err), "ordinary cancellation keeps the caller's original error")
	}
	shutdown, stop := context.WithCancelCause(t.Context())
	stop(ErrSchedulerStopped)
	for _, err := range []error{shutdown.Err(), context.Cause(shutdown)} {
		require.NoError(t, ErrorAfterYield(shutdown, err))
	}
	require.ErrorIs(t, ErrorAfterYield(shutdown, errors.Join(failure, shutdown.Err())), failure)
	require.NotErrorIs(t, ErrorAfterYield(shutdown, errors.Join(failure, shutdown.Err())), context.Canceled)
	yielded, yield := context.WithCancelCause(t.Context())
	yield(ErrYieldedToWaiter)
	for _, tc := range []struct {
		name    string
		err     error
		want    error
		message string
	}{
		{name: "joined source failure", err: errors.Join(failure, context.Canceled, ErrYieldedToWaiter), want: failure, message: "provider failed"},
		{name: "wrapped source failure", err: fmt.Errorf("source callback: %w", errors.Join(failure, context.Canceled, ErrYieldedToWaiter)), want: failure, message: "source callback: provider failed"},
		{name: "independent deadline", err: context.DeadlineExceeded, want: context.DeadlineExceeded, message: "context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ErrorAfterYield(yielded, tc.err)
			require.ErrorIs(t, got, tc.want)
			require.NotErrorIs(t, got, ErrYieldedToWaiter)
			require.NotErrorIs(t, got, context.Canceled)
			require.EqualError(t, got, tc.message)
		})
	}
	budget, expire := context.WithTimeoutCause(t.Context(), 0, ErrRunBudgetExceeded)
	defer expire()
	require.ErrorIs(t, budget.Err(), context.DeadlineExceeded)
	require.NoError(t, ErrorAfterYield(budget, context.Cause(budget)))
}
