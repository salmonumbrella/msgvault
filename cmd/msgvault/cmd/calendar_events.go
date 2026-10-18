package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

const calendarEventsMinAPISchemaVersion = "3.24.0"

type calendarEventsClient interface {
	ListCalendarOccurrencesContext(ctx context.Context, from, to time.Time) (store.CalendarOccurrencePage, error)
	APISchemaVersion(ctx context.Context) (string, error)
}
type calendarEventsOpen func(context.Context) (calendarEventsClient, func(), error)

func openCalendarEvents(ctx context.Context) (calendarEventsClient, func(), error) {
	client, _, err := OpenHTTPStore(ctx)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open daemon: %w", err)
	}
	return client, func() { _ = client.Close() }, nil
}
func newCalendarEventsCommand(open calendarEventsOpen) *cobra.Command {
	var from, to string
	var jsonOutput bool
	command := &cobra.Command{Use: "events --from <date> --to <date>", Short: "List archived calendar events", Args: cobra.NoArgs}
	command.Flags().StringVar(&from, "from", "", "window start: YYYY-MM-DD or RFC3339")
	command.Flags().StringVar(&to, "to", "", "exclusive window end: YYYY-MM-DD or RFC3339")
	command.Flags().BoolVar(&jsonOutput, "json", false, "print occurrences and attendees as JSON")
	_ = command.MarkFlagRequired("from")
	_ = command.MarkFlagRequired("to")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		for _, name := range []string{"account", "dry-run", "read-only", "send-updates"} {
			if cmd.Flags().Changed(name) {
				return usageErr(cmd, fmt.Errorf("--%s is only valid for live calendar commands", name))
			}
		}
		bounds := make([]time.Time, 2)
		for i, value := range []string{from, to} {
			var err error
			if len(value) == len(time.DateOnly) {
				bounds[i], err = time.ParseInLocation(time.DateOnly, value, time.Local)
			} else {
				bounds[i], err = time.Parse(time.RFC3339, value)
			}
			if err != nil {
				return usageErr(cmd, fmt.Errorf("--%s: use YYYY-MM-DD or RFC3339 with an offset", []string{"from", "to"}[i]))
			}
		}
		if !bounds[1].After(bounds[0]) {
			return usageErr(cmd, errors.New("--to must be after --from"))
		}
		client, cleanup, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer cleanup()
		version, err := client.APISchemaVersion(cmd.Context())
		if err != nil {
			return err
		}
		if !daemonclient.APISchemaVersionAtLeast(version, calendarEventsMinAPISchemaVersion) {
			return errors.New("calendar events require daemon API schema 3.24.0 or newer")
		}
		page, err := client.ListCalendarOccurrencesContext(cmd.Context(), bounds[0], bounds[1])
		if err != nil {
			var apiError *daemonclient.APIError
			if errors.As(err, &apiError) && apiError.Code == "query_too_broad" {
				return store.ErrQueryTooBroad
			}
			return err
		}
		if jsonOutput {
			data, err := json.Marshal(page)
			if err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
			if err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
			return nil
		}
		if len(page.Occurrences) == 0 {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "No archived calendar events from %s to %s.\n", from, to); err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
		} else {
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if _, err := fmt.Fprintln(writer, "START\tEND\tSTATUS\tID\tTITLE"); err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
			for _, o := range page.Occurrences {
				start, end := o.Start.In(time.Local).Format("2006-01-02 15:04"), o.End.In(time.Local).Format("2006-01-02 15:04")
				if o.AllDay {
					start, end = o.StartDate, o.EndDate
				}
				if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\n", start, end, o.Status, o.MessageID, o.Subject); err != nil {
					return fmt.Errorf("write calendar events: %w", err)
				}
			}
			if err := writer.Flush(); err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
		}
		if len(page.Unexpandable) > 0 {
			noun := "events"
			if len(page.Unexpandable) == 1 {
				noun = "event"
			}
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%d %s could not be expanded: ", len(page.Unexpandable), noun); err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
			for i, event := range page.Unexpandable {
				if i > 0 {
					if _, err := fmt.Fprint(cmd.ErrOrStderr(), ", "); err != nil {
						return fmt.Errorf("write calendar events: %w", err)
					}
				}
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "message %d (%s)", event.MessageID, event.Reason); err != nil {
					return fmt.Errorf("write calendar events: %w", err)
				}
			}
			if _, err := fmt.Fprintln(cmd.ErrOrStderr()); err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
		}
		return nil
	}
	return command
}
