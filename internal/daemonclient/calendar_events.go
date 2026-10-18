package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) ListCalendarOccurrencesContext(ctx context.Context, from, to time.Time) (store.CalendarOccurrencePage, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListCalendarEventsResp, error) {
		return client.ListCalendarEventsWithResponse(ctx, &generated.ListCalendarEventsRequestOptions{Query: &generated.ListCalendarEventsQuery{From: from, To: to}})
	})
	if err != nil {
		return store.CalendarOccurrencePage{}, err
	}
	if response.JSON200 == nil {
		return store.CalendarOccurrencePage{}, errors.New("calendar events: empty response")
	}
	var page store.CalendarOccurrencePage
	if err := json.Unmarshal(response.Body, &page); err != nil {
		return page, fmt.Errorf("decode calendar events: %w", err)
	}
	return page, nil
}
