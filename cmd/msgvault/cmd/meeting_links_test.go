package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type meetingLinkFakeClient struct {
	version, event, decision, ref string
	id                            int64
	calls                         int
}

func (f *meetingLinkFakeClient) APISchemaVersion(context.Context) (string, error) {
	return f.version, nil
}
func (f *meetingLinkFakeClient) DecideMeetingLink(_ context.Context, id int64, event, decision string) error {
	f.calls++
	f.id, f.event, f.decision = id, event, decision
	return nil
}
func (f *meetingLinkFakeClient) SuggestMeetingLinks(_ context.Context, ref string) ([]store.MeetingSuggestion, error) {
	f.calls++
	f.ref = ref
	return []store.MeetingSuggestion{{Ref: "evt:81@20270314T100000Z", MemberMessageID: 42, OverlapMinutes: 30}}, nil
}

func TestMeetingLinkCommands(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		args                   []string
		version, wantErr, want string
	}{
		{"link", []string{"link", "42", "--event", "evt:81@20270314T100000Z"}, "", "", ""},
		{"unlink", []string{"unlink", "42", "--event", "evt:81@20270314T100000Z"}, "", "", ""},
		{"suggest note", []string{"suggest", "msg:42"}, "", "", "evt:81@20270314T100000Z"},
		{"suggest event", []string{"suggest", "evt:81@20270314T100000Z", "--json"}, "", "", `"member_message_id":42`},
		{"old daemon link", []string{"link", "42", "--event", "evt:81"}, "3.24.0", "upgrade the daemon", ""},
		{"old daemon unlink", []string{"unlink", "42", "--event", "evt:81"}, "3.24.0", "upgrade the daemon", ""},
		{"old daemon suggest", []string{"suggest", "msg:42"}, "3.24.0", "upgrade the daemon", ""},
		{"invalid note", []string{"link", "0", "--event", "evt:81"}, "", "positive message ID", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &meetingLinkFakeClient{version: tc.version}
			if fake.version == "" {
				fake.version = meetingLinksMinAPISchemaVersion
			}
			cleaned := false
			open := func(context.Context) (meetingLinkClient, func(), error) { return fake, func() { cleaned = true }, nil }
			root := newMeetingsCommand(meetingCommandDeps{})
			for _, child := range root.Commands() {
				if child.Name() == "link" || child.Name() == "unlink" || child.Name() == "suggest" {
					root.RemoveCommand(child)
				}
			}
			root.AddCommand(newMeetingLinkDecisionCommand("link", open), newMeetingLinkDecisionCommand("unlink", open), newMeetingSuggestCommand(open))
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)
			err := root.Execute()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Zero(t, fake.calls)
				if tc.version != "" {
					assert.True(t, cleaned)
				}
				return
			}
			require.NoError(t, err)
			assert.True(t, cleaned)
			assert.Equal(t, 1, fake.calls)
			if tc.args[0] == "suggest" {
				assert.Equal(t, tc.args[1], fake.ref)
				assert.Contains(t, out.String(), tc.want)
			} else {
				assert.Equal(t, int64(42), fake.id)
				assert.Equal(t, "evt:81@20270314T100000Z", fake.event)
				assert.Equal(t, tc.args[0], fake.decision)
			}
		})
	}
	// The production parent includes all three commands.
	root := newMeetingsCommand(meetingCommandDeps{})
	for _, name := range []string{"link", "unlink", "suggest"} {
		command, _, err := root.Find([]string{name})
		require.NoError(t, err)
		assert.Equal(t, name, command.Name())
	}
}
