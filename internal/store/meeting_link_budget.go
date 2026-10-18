package store

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

func spendLinkBudget(ctx context.Context, budget *int, cost int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if budget == nil || *budget < cost {
		return ErrQueryTooBroad
	}
	*budget -= cost
	return nil
}

func linkRecurrenceAllowance(ctx context.Context, budget *int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if budget == nil || *budget < 0 {
		return 0, ErrQueryTooBroad
	}
	return min(calrecur.SeriesReadBudget, *budget), nil
}

func chargeLinkRecurrenceUsage(ctx context.Context, budget *int, allowance, used int) error {
	// Iter reports the candidate that crossed its per-series budget as used too.
	// Charge at most the amount that was authorized before translating that
	// budget error, so the request budget never goes negative.
	return spendLinkBudget(ctx, budget, min(allowance, used))
}

func expandLinkRecurrence(
	ctx context.Context,
	set calrecur.Set,
	duration time.Duration,
	durationDays int,
	exceptions []calrecur.Exception,
	from, to time.Time,
	budget *int,
) ([]calrecur.Occurrence, error) {
	allowance, err := linkRecurrenceAllowance(ctx, budget)
	if err != nil {
		return nil, err
	}
	occurrences, used, err := calrecur.ExpandWithUsage(ctx, set, duration, durationDays, exceptions, from, to, allowance)
	if chargeErr := chargeLinkRecurrenceUsage(ctx, budget, allowance, used); chargeErr != nil {
		return nil, chargeErr
	}
	if err != nil {
		return nil, linkRecurrenceError(err)
	}
	return occurrences, nil
}

func containsLinkRecurrence(
	ctx context.Context,
	set calrecur.Set,
	exceptions []calrecur.Exception,
	original calrecur.Identity,
	budget *int,
) (bool, error) {
	allowance, err := linkRecurrenceAllowance(ctx, budget)
	if err != nil {
		return false, err
	}
	found, used, err := calrecur.ContainsWithUsage(ctx, set, exceptions, original, allowance)
	if chargeErr := chargeLinkRecurrenceUsage(ctx, budget, allowance, used); chargeErr != nil {
		return false, chargeErr
	}
	if err != nil {
		return false, linkRecurrenceError(err)
	}
	return found, nil
}
