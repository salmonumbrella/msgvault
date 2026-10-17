package jobctx

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrorAfterYieldDistinguishesSchedulerShutdown(t *testing.T) {
	requirements := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	failure := errors.New("provider failed")
	for _, err := range []error{ctx.Err(), errors.Join(failure, ctx.Err())} {
		requirements.Same(err, ErrorAfterYield(ctx, err), "ordinary cancellation keeps the caller's original error")
	}
	shutdown, stop := context.WithCancelCause(t.Context())
	stop(ErrSchedulerStopped)
	for _, err := range []error{shutdown.Err(), context.Cause(shutdown)} {
		requirements.NoError(ErrorAfterYield(shutdown, err))
	}
	requirements.ErrorIs(ErrorAfterYield(shutdown, errors.Join(failure, shutdown.Err())), failure)
	requirements.NotErrorIs(ErrorAfterYield(shutdown, errors.Join(failure, shutdown.Err())), context.Canceled)
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
			requirements := require.New(t)
			got := ErrorAfterYield(yielded, tc.err)
			requirements.ErrorIs(got, tc.want)
			requirements.NotErrorIs(got, ErrYieldedToWaiter)
			requirements.NotErrorIs(got, context.Canceled)
			requirements.EqualError(got, tc.message)
		})
	}
	budget, expire := context.WithTimeoutCause(t.Context(), 0, ErrRunBudgetExceeded)
	defer expire()
	requirements.ErrorIs(budget.Err(), context.DeadlineExceeded)
	requirements.NoError(ErrorAfterYield(budget, context.Cause(budget)))
}
