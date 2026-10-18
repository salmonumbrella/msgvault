package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

type calendarEventsFakeClient struct {
	err      error
	page     store.CalendarOccurrencePage
	version  string
	from, to time.Time
}

func (f *calendarEventsFakeClient) APISchemaVersion(context.Context) (string, error) {
	return f.version, nil
}
func (f *calendarEventsFakeClient) ListCalendarOccurrencesContext(_ context.Context, from, to time.Time) (store.CalendarOccurrencePage, error) {
	f.from, f.to = from, to
	return f.page, f.err
}
func TestCalendarEventsCommand(t *testing.T) {
	start, err := time.Parse(time.RFC3339, "2027-03-02T17:00:00Z")
	require.NoError(t, err)
	for _, tc := range []struct {
		name                       string
		args                       []string
		page                       store.CalendarOccurrencePage
		version                    string
		want, wantErr, wantWarning string
		err                        error
	}{
		{name: "table", page: store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{MessageID: 378, SeriesMessageID: 377, Subject: "Standup", Status: "confirmed", Start: start, End: start.Add(time.Hour)}, {MessageID: 412, Subject: "Offsite", Status: "confirmed", AllDay: true, StartDate: "2027-03-03", EndDate: "2027-03-04"}}, Unexpandable: []store.CalendarUnexpandable{{MessageID: 500, Reason: "unsupported recurrence"}}}, want: "Standup", wantWarning: "1 event could not be expanded: message 500 (unsupported recurrence)"},
		{name: "empty", want: "No archived calendar events from 2027-03-01 to 2027-04-01."},
		{name: "json", args: []string{"--json"}, page: store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{MessageID: 378, SeriesMessageID: 377, Subject: "Standup", Start: start, End: start.Add(time.Hour)}}, Unexpandable: []store.CalendarUnexpandable{}}, want: `"message_id":378`},
		{name: "deleted", page: store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{MessageID: 378, Subject: "Removed event", Status: "confirmed", DeletedFromSource: true, Start: start, End: start.Add(time.Hour)}}}, want: "deleted"},
		{name: "too broad", err: &daemonclient.APIError{Status: 400, Code: "query_too_broad", Message: "calendar query is too broad; narrow the window"}, wantErr: "calendar query is too broad; narrow the window"},
		{name: "series cap", err: &daemonclient.APIError{Status: 400, Code: "query_too_broad", Message: "calendar query is too broad: more than 2000 recurring series overlap the window; a narrower window will not reduce open-ended series"}, wantErr: "calendar query is too broad: more than 2000 recurring series overlap the window; a narrower window will not reduce open-ended series"},
		{name: "old daemon", version: "3.23.0", wantErr: "calendar events require daemon API schema 3.24.0 or newer"},
		{name: "account", args: []string{"--account", "calendar@example.com"}, wantErr: "--account is only valid"},
		{name: "dry run", args: []string{"--dry-run"}, wantErr: "--dry-run is only valid"},
		{name: "read only", args: []string{"--read-only"}, wantErr: "--read-only is only valid"},
		{name: "send updates", args: []string{"--send-updates", "none"}, wantErr: "--send-updates is only valid"},
		{name: "reversed", args: []string{"--to", "2027-02-01"}, wantErr: "--to must be after --from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			fake := &calendarEventsFakeClient{page: tc.page, version: tc.version, err: tc.err}
			if fake.version == "" {
				fake.version = calendarEventsMinAPISchemaVersion
			}
			root := newCalendarControlCmd(nil)
			for _, child := range root.Commands() {
				if child.Name() == "events" {
					root.RemoveCommand(child)
				}
			}
			root.AddCommand(newCalendarEventsCommand(func(context.Context) (calendarEventsClient, func(), error) { return fake, func() {}, nil }))
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SilenceErrors = true
			root.SilenceUsage = true
			root.SetArgs(append([]string{"events", "--from", "2027-03-01", "--to", "2027-04-01"}, tc.args...))
			err := root.Execute()
			if tc.wantErr != "" {
				requirements.ErrorContains(err, tc.wantErr)
				if tc.name == "too broad" || tc.name == "series cap" {
					requirements.EqualError(err, tc.wantErr)
					requirements.ErrorIs(err, store.ErrQueryTooBroad)
				}
				return
			}
			requirements.NoError(err)
			assertions.Contains(out.String(), tc.want)
			assertions.Contains(stderr.String(), tc.wantWarning)
			assertions.Equal(time.Date(2027, 3, 1, 0, 0, 0, 0, time.Local), fake.from)
			if tc.name == "table" {
				assertions.Contains(out.String(), "2027-03-03")
				assertions.Contains(out.String(), "2027-03-04")
				assertions.Contains(out.String(), "ID")
				assertions.Regexp(`\s378\s+Standup`, out.String())
			}
			if tc.name == "json" {
				var page store.CalendarOccurrencePage
				requirements.NoError(json.Unmarshal(out.Bytes(), &page))
				requirements.Len(page.Occurrences, 1)
				assertions.Equal("Standup", page.Occurrences[0].Subject)
				assertions.Equal(int64(378), page.Occurrences[0].MessageID)
				assertions.Equal(int64(377), page.Occurrences[0].SeriesMessageID)
			}
		})
	}
}
func TestCalendarEventsRFC3339Bounds(t *testing.T) {
	fake := &calendarEventsFakeClient{version: calendarEventsMinAPISchemaVersion}
	cmd := newCalendarEventsCommand(func(context.Context) (calendarEventsClient, func(), error) { return fake, func() {}, nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--from", "2027-03-01T12:00:00-05:00", "--to", "2027-03-02T12:00:00-05:00"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "2027-03-01T17:00:00Z", fake.from.UTC().Format(time.RFC3339))
}

func TestCalendarEventsInvalidBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing from", []string{"--to", "2027-03-03"}, `required flag(s) "from" not set`},
		{"missing to", []string{"--from", "2027-03-02"}, `required flag(s) "to" not set`},
		{"local from", []string{"--from", "2027-03-02T09:00", "--to", "2027-03-03"}, "--from: use YYYY-MM-DD or RFC3339 with an offset"},
		{"local to", []string{"--from", "2027-03-02", "--to", "2027-03-03T17:00"}, "--to: use YYYY-MM-DD or RFC3339 with an offset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newCalendarEventsCommand(func(context.Context) (calendarEventsClient, func(), error) {
				require.FailNow(t, "invalid bounds reached daemon")
				return nil, nil, nil
			})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			require.ErrorContains(t, cmd.Execute(), tc.want)
		})
	}
}
