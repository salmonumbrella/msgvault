package inline

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseArchiveKeys(t *testing.T) {
	for _, tc := range []struct {
		chat, message string
		valid         bool
	}{
		{"chat:7", "chat:7:message:9", true},
		{"chat:07", "chat:07:message:9", false},
		{"msgvault-archive:7", "chat:7:message:9", false},
		{"chat:7", "chat:8:message:9", false},
		{"chat:7", "chat:7:message:0", false},
		{"chat:7", "chat:7:message:9007199254740992", false},
	} {
		t.Run(tc.chat+"/"+tc.message, func(t *testing.T) {
			chat, message, err := ParseArchiveKeys(tc.chat, tc.message)
			if tc.valid {
				require.NoError(t, err)
				assert.EqualValues(t, 7, chat)
				assert.EqualValues(t, 9, message)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestCLIDialogDecode(t *testing.T) {
	chat := `{"id":7,"peer_id":{"type":{"User":{"user_id":9}}}}`
	peer := `{"type":{"User":{"user_id":9}}}`
	for _, tc := range []struct {
		name, body      string
		valid, archived bool
	}{
		{"omitted", `{"chat":` + chat + `,"dialog":{"peer":` + peer + `,"chat_id":7}}`, true, false},
		{"false", `{"chat":` + chat + `,"dialog":{"peer":` + peer + `,"chat_id":7,"archived":false}}`, true, false},
		{"true", `{"chat":` + chat + `,"dialog":{"peer":` + peer + `,"chat_id":7,"archived":true}}`, true, true},
		{"missing dialog", `{"chat":` + chat + `}`, false, false},
		{"wrong chat", `{"chat":` + chat + `,"dialog":{"peer":` + peer + `,"chat_id":8}}`, false, false},
		{"wrong native user", `{"chat":` + chat + `,"dialog":{"peer":{"type":{"User":{"user_id":7}}}}}`, false, false},
		{"wrong peer type", `{"chat":` + chat + `,"dialog":{"peer":{"type":{"Chat":{"chat_id":7}}}}}`, false, false},
		{"multiple peers", `{"chat":` + chat + `,"dialog":{"peer":{"type":{"Chat":{"chat_id":7},"User":{"user_id":9}}}}}`, false, false},
		{"wrong chat object", `{"chat":{"id":8,"peer_id":{"type":{"User":{"user_id":9}}}},"dialog":{"peer":` + peer + `}}`, false, false},
		{"child", `{"chat":{"id":7,"parent_chat_id":6,"parent_message_id":1,"peer_id":{"type":{"Chat":{"chat_id":7}}}},"dialog":{"peer":{"type":{"Chat":{"chat_id":7}}},"chat_id":7}}`, true, false},
		{"wrong child peer", `{"chat":{"id":7,"parent_chat_id":6,"parent_message_id":1,"peer_id":{"type":{"Chat":{"chat_id":7}}}},"dialog":{"peer":{"type":{"Chat":{"chat_id":6}}}}}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := cliDecodeDialog([]byte(tc.body), 7, 42)
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, tc.archived, state.Archived)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestCLIMessageIdentityIgnoresUnrelatedFields(t *testing.T) {
	raw := []byte(`{"id":8,"chat_id":7,"from_id":42,"date":1,"media":{"media":{"FutureVariant":{}}},"reactions":{"reactions":[{"user_id":0}]}}`)
	require.NoError(t, cliVerifyMessageIdentity(raw, 7, 8))
	require.Error(t, cliVerifyMessageIdentity(raw, 6, 8))
	require.Error(t, cliVerifyMessageIdentity(raw, 7, 9))
}
