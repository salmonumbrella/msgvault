package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestLinkBudgetChargesActualRecurrenceCandidates(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	start := time.Date(2027, 3, 7, 10, 0, 0, 0, time.UTC)
	set, err := calrecur.ParseSet(start, false, "", time.UTC, []string{"RRULE:FREQ=WEEKLY"})
	requirements.NoError(err)

	budget := 100
	occurrences, err := expandLinkRecurrence(context.Background(), set, time.Hour, 0, nil, start, start.AddDate(0, 0, 8), &budget)
	requirements.NoError(err)
	requirements.Len(occurrences, 2)
	assertions.Equal(97, budget, "charge DTSTART and both RRULE candidates through the horizon")
}

func TestLinkBudgetCapsLongSeriesAndHonorsCancellation(t *testing.T) {
	requirements := require.New(t)
	start := time.Date(1800, 1, 1, 0, 0, 0, 0, time.UTC)
	set, err := calrecur.ParseSet(start, false, "", time.UTC, []string{"RRULE:FREQ=DAILY"})
	requirements.NoError(err)
	budget := calrecur.RequestReadBudget
	_, err = expandLinkRecurrence(context.Background(), set, time.Hour, 0, nil, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC), &budget)
	requirements.ErrorIs(err, ErrQueryTooBroad)
	requirements.Equal(calrecur.RequestReadBudget-calrecur.SeriesReadBudget, budget)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	budget = calrecur.RequestReadBudget
	_, err = expandLinkRecurrence(ctx, set, time.Hour, 0, nil, start, start.Add(time.Hour), &budget)
	requirements.ErrorIs(err, context.Canceled)
	requirements.Equal(calrecur.RequestReadBudget, budget)
}

func TestLinkBudgetAllowsMovedExceptionOfFinishedMaster(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	start := time.Date(1800, 1, 1, 10, 0, 0, 0, time.UTC)
	set, err := calrecur.ParseSet(start, false, "", time.UTC, []string{"RRULE:FREQ=DAILY;COUNT=6"})
	requirements.NoError(err)
	moved := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	end := moved.Add(time.Hour)
	budget := 50
	occurrences, err := expandLinkRecurrence(context.Background(), set, time.Hour, 0, []calrecur.Exception{{Original: calrecur.Identity{Instant: start}, Start: &moved, End: &end}}, moved.Add(-time.Hour), end.Add(time.Hour), &budget)
	requirements.NoError(err)
	requirements.Len(occurrences, 1)
	assertions.Equal(moved, occurrences[0].Start)
	assertions.Equal(43, budget)
}
