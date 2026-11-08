package personagenda

import (
	"context"
	"testing"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/taskclient"
)

type scheduleScopeClient struct {
	TaskClient
	parent taskclient.KataTask
	job    taskclient.KataCronJob
	writes int
}

func (c *scheduleScopeClient) GetTask(context.Context, string, string) (taskclient.KataTask, error) {
	return c.parent, nil
}
func (c *scheduleScopeClient) GetCronJob(context.Context, string, string) (taskclient.KataCronJob, error) {
	return c.job, nil
}
func (c *scheduleScopeClient) EditCronJob(context.Context, string, taskclient.KataCronJob, string, taskclient.KataCronSettings) (taskclient.KataCronJob, error) {
	c.writes++
	return c.job, nil
}
func (c *scheduleScopeClient) SetCronJobEnabled(context.Context, string, taskclient.KataCronJob, string, bool) (taskclient.KataCronJob, error) {
	c.writes++
	return c.job, nil
}
func (c *scheduleScopeClient) SetCronJobDeleted(context.Context, string, taskclient.KataCronJob, string, bool) (taskclient.KataCronJob, error) {
	c.writes++
	return c.job, nil
}

func TestScheduleMutationRejectsWrongPersonWithoutWriting(t *testing.T) {
	client := &scheduleScopeClient{parent: taskclient.KataTask{UID: fixtureParentUID, ProjectID: 1, Metadata: map[string]any{PersonMetadataKey: "other"}}, job: taskclient.KataCronJob{UID: fixtureJobUID, ParentUID: fixtureParentUID, ProjectID: 1, DefinitionEventUID: fixtureEventUID}}
	service := Service{Tasks: client, People: fakeIdentityStore{uids: []string{"person"}}, Project: "example"}
	_, err := service.UpdateSchedule(t.Context(), 1, fixtureParentUID, fixtureJobUID, ScheduleUpdateInput{Action: "pause", ExpectedEventUID: fixtureEventUID})
	require.ErrorIs(t, err, taskclient.ErrNotFound)
	assert.Zero(t, client.writes)
}

func TestScheduleInputValidation(t *testing.T) {
	valid := ScheduleInput{TriggerKind: "cron", Cron: "0 9 * * 1", Timezone: "Europe/London", OccurrenceTitle: "Review", Recipient: "synthetic", Catchup: "skip", GraceSeconds: 60}
	for _, tc := range []struct {
		name   string
		modify func(*ScheduleInput)
		want   bool
	}{
		{"valid", func(*ScheduleInput) {}, true},
		{"invalid timezone", func(in *ScheduleInput) { in.Timezone = "invalid/zone" }, false},
		{"machine local timezone", func(in *ScheduleInput) { in.Timezone = "Local" }, false},
		{"missing recipient", func(in *ScheduleInput) { in.Recipient = "" }, false},
		{"invalid cron", func(in *ScheduleInput) { in.Cron = "not a cron" }, false},
		{"skip requires grace", func(in *ScheduleInput) { in.GraceSeconds = 0 }, false},
		{"interval minimum", func(in *ScheduleInput) { in.TriggerKind = "interval"; in.IntervalSeconds = 59 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			tc.modify(&input)
			assert.Equal(t, tc.want, validScheduleInput(input))
		})
	}
}

func TestScheduleManagementEditableProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	result, err := f.service(t, "person", "alias").List(t.Context(), 1)
	require.NoError(err)
	require.Len(result.Items, 1)
	require.Len(result.Items[0].Schedules, 1)
	assert.True(result.Items[0].Schedules[0].Editable, "supported native five-field shapes can be represented by the edit form")
}
